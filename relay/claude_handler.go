package relay

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relay/requestplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
)

// CountClaudeTokens sends one native counting request outside the generation,
// billing and channel-health pipelines. New count-only JSON fields pass through.
func CountClaudeTokens(c *gin.Context) {
	steps, record, apiErr := requestplugin.Ingress(c, types.RelayFormatClaude)
	var info *relaycommon.RelayInfo
	defer func() {
		if apiErr != nil && !c.Writer.Written() {
			service.RecordRequestPolicyTermination(c, apiErr)
			switch apiErr.GetErrorCode() {
			case types.ErrorCodeRequestRejected, types.ErrorCodeRequestPluginFailed:
				audit := info
				channelID := 0
				if audit == nil {
					audit = &relaycommon.RelayInfo{}
					audit.SetRequestPluginIngress(steps, record)
				} else {
					channelID = audit.ChannelId
				}
				service.RecordRelayErrorLog(c, channelID, apiErr, audit)
				middleware.AbortWithClaudeRelayError(c, apiErr.StatusCode, apiErr.ToClaudeError(common.UpstreamAddresses(c)...))
			default:
				middleware.AbortWithClaudeError(c, apiErr.StatusCode, apiErr.MaskSensitiveError(common.UpstreamAddresses(c)...))
			}
		}
		requestplugin.Report(c, types.RelayFormatClaude, info, record, apiErr)
	}()
	if apiErr != nil {
		return
	}
	info = relaycommon.GenRelayInfoClaude(c, nil)
	info.RelayMode = relayconstant.RelayModeClaudeCountTokens
	info.SetRequestPluginIngress(steps, record)
	info.InitChannelMeta(c)
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		apiErr = types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest)
		return
	}
	var body io.Reader = common.NewReplayableBodyReader(storage)
	if model_setting.GetGlobalSettings().PassThroughRequestEnabled || info.ChannelSetting.PassThroughBodyEnabled {
		if apiErr = requestplugin.PassThrough(c, info, storage); apiErr != nil {
			return
		}
	} else {
		data, err := storage.Bytes()
		if err != nil {
			apiErr = types.NewError(err, types.ErrorCodeReadRequestBodyFailed)
			return
		}
		// Request-stage policy reads the Claude message. Keep the original JSON for
		// forwarding so fields specific to counting survive model normalization.
		var request dto.ClaudeRequest
		if err = common.Unmarshal(data, &request); err != nil {
			apiErr = types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest)
			return
		}
		info.Request = &request
		if err = helper.ModelMappedHelper(c, info, nil); err != nil {
			apiErr = types.NewError(err, types.ErrorCodeChannelModelMappedError)
			return
		}
		if err = helper.ApplyReasoningModelSuffix(c, info); err != nil {
			apiErr = newConvertRequestFailedError(c, info, err)
			return
		}
		if info.UpstreamModelName != info.OriginModelName {
			doc := ojson.NewDoc(data)
			if err = relaycommon.SetJSONMember(doc, "", "model", info.UpstreamModelName); err != nil {
				apiErr = types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest)
				return
			}
			data = doc.Bytes()
		}
		if data, apiErr = requestplugin.Outbound(c, info, data); apiErr != nil {
			return
		}
		if len(info.ParamOverride) > 0 {
			if data, err = relaycommon.ApplyParamOverrideWithRelayInfo(data, info); err != nil {
				apiErr = newAPIErrorFromParamOverride(err)
				return
			}
		}
		if apiErr = requestplugin.Egress(c, info, data); apiErr != nil {
			return
		}
		body = bytes.NewReader(data)
	}
	resp, err := channel.DoApiRequest(&claude.Adaptor{}, c, info, body)
	if err != nil {
		apiErr = types.NewErrorWithStatusCode(errors.New(i18n.T(c, i18n.MsgRelayUpstreamTokenCountFailed)), types.ErrorCodeDoRequestFailed, http.StatusBadGateway)
		return
	}
	defer service.CloseResponseBodyGracefully(resp)
	addresses := common.UpstreamAddresses(c)
	credentials := []string{info.ApiKey}
	if req := resp.Request; req != nil {
		credentials = append(credentials, req.Header.Values("X-Api-Key")...)
		for _, auth := range req.Header.Values("Authorization") {
			credentials = append(credentials, auth)
			if fields := strings.Fields(auth); len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
				credentials = append(credentials, fields[1])
			}
		}
	}
	// Mask longer credentials first when an override extends the channel key.
	slices.SortFunc(credentials, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	// A compatible upstream may echo credentials outside the message, including
	// its request ID or error type. Sanitize every string returned from errors.
	redact := func(value string) string {
		if value == "" {
			return ""
		}
		for _, credential := range credentials {
			if credential != "" {
				value = strings.ReplaceAll(value, credential, "***")
			}
		}
		return common.MaskSensitiveInfo(value, addresses...)
	}
	requestID := redact(resp.Header.Get("request-id"))
	if requestID != "" {
		c.Header("request-id", requestID)
	}
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		c.Header("Retry-After", redact(retryAfter))
	}
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		c.DataFromReader(resp.StatusCode, resp.ContentLength, gin.MIMEJSON, resp.Body, nil)
		return
	}
	if resp.StatusCode < http.StatusBadRequest {
		apiErr = types.NewErrorWithStatusCode(errors.New(i18n.T(c, i18n.MsgRelayUpstreamTokenCountFailed)), types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway)
		return
	}
	const maxErrorBytes = 64 << 10
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes+1))
	var envelope struct {
		Type      string            `json:"type"`
		Error     types.ClaudeError `json:"error"`
		RequestID string            `json:"request_id,omitempty"`
	}
	if err != nil || len(data) > maxErrorBytes || common.Unmarshal(data, &envelope) != nil || envelope.Type != "error" || envelope.Error.Type == "" {
		apiErr = types.NewErrorWithStatusCode(errors.New(i18n.T(c, i18n.MsgRelayUpstreamTokenCountFailed)), types.ErrorCodeBadResponseBody, resp.StatusCode)
		return
	}
	envelope.Error.Type = redact(envelope.Error.Type)
	envelope.Error.Message = redact(envelope.Error.Message)
	if envelope.RequestID != "" {
		envelope.RequestID = redact(envelope.RequestID)
	} else {
		envelope.RequestID = requestID
	}
	apiErr = types.WithClaudeError(envelope.Error, resp.StatusCode)
	c.JSON(resp.StatusCode, envelope)
}

func ClaudeHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {

	info.InitChannelMeta(c)

	claudeReq, ok := info.Request.(*dto.ClaudeRequest)

	if !ok {
		return types.NewErrorWithStatusCode(fmt.Errorf("invalid request type, expected *dto.ClaudeRequest, got %T", info.Request), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}

	request, err := common.DeepCopy(claudeReq)
	if err != nil {
		return types.NewError(fmt.Errorf("failed to copy request to ClaudeRequest: %w", err), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	err = helper.ModelMappedHelper(c, info, request)
	if err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}
	if err := helper.ApplyReasoningModelSuffix(c, info, request); err != nil {
		return newConvertRequestFailedError(c, info, err)
	}

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)

	if info.ChannelSetting.SystemPrompt != "" {
		if request.System == nil {
			request.SetStringSystem(info.ChannelSetting.SystemPrompt)
		} else if info.ChannelSetting.SystemPromptOverride {
			common.SetContextKey(c, constant.ContextKeySystemPromptOverride, true)
			if request.IsStringSystem() {
				existing := strings.TrimSpace(request.GetStringSystem())
				if existing == "" {
					request.SetStringSystem(info.ChannelSetting.SystemPrompt)
				} else {
					request.SetStringSystem(info.ChannelSetting.SystemPrompt + "\n" + existing)
				}
			} else {
				systemContents := request.ParseSystem()
				newSystem := dto.ClaudeMediaMessage{Type: dto.ContentTypeText}
				newSystem.SetText(info.ChannelSetting.SystemPrompt)
				if len(systemContents) == 0 {
					request.System = []dto.ClaudeMediaMessage{newSystem}
				} else {
					request.System = append([]dto.ClaudeMediaMessage{newSystem}, systemContents...)
				}
			}
		}
	}

	if !model_setting.GetGlobalSettings().PassThroughRequestEnabled &&
		!info.ChannelSetting.PassThroughBodyEnabled &&
		service.ShouldChatCompletionsUseResponsesGlobal(info.ChannelId, info.ChannelType, info.OriginModelName) {
		usage, newApiErr := textRequestViaResponses(c, info, adaptor, request)
		if newApiErr != nil {
			return newApiErr
		}

		service.PostTextConsumeQuota(c, info, usage, nil)
		return nil
	}

	var requestBody io.Reader
	if model_setting.GetGlobalSettings().PassThroughRequestEnabled || info.ChannelSetting.PassThroughBodyEnabled {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		if newAPIError = requestplugin.PassThrough(c, info, storage); newAPIError != nil {
			return newAPIError
		}
		requestBody = common.NewReplayableBodyReader(storage)
	} else {
		convertedRequest, err := adaptor.ConvertClaudeRequest(c, info, request)
		if err != nil {
			return newConvertRequestFailedError(c, info, err)
		}
		relaycommon.AppendRequestConversionFromRequest(info, convertedRequest)
		jsonData, err := common.Marshal(convertedRequest)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}

		// remove disabled fields for Claude API
		jsonData, err = relaycommon.RemoveDisabledFields(jsonData, info.ChannelOtherSettings, info.ChannelSetting.PassThroughBodyEnabled)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}

		if jsonData, newAPIError = requestplugin.Outbound(c, info, jsonData); newAPIError != nil {
			return newAPIError
		}
		// apply param override
		if len(info.ParamOverride) > 0 {
			jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
			if err != nil {
				return newAPIErrorFromParamOverride(err)
			}
		}
		if newAPIError = requestplugin.Egress(c, info, jsonData); newAPIError != nil {
			return newAPIError
		}

		logger.LogDebug(c, "requestBody: %s", jsonData)
		body, closer, err := relaycommon.NewOutboundJSONBody(jsonData)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		defer closer.Close()
		jsonData = nil
		requestBody = body
	}

	statusCodeMappingStr := c.GetString("status_code_mapping")
	var httpResp *http.Response
	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}

	if resp != nil {
		httpResp = resp.(*http.Response)
		info.IsStream = info.IsStream || strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")
		if httpResp.StatusCode != http.StatusOK {
			newAPIError = service.RelayErrorHandler(c.Request.Context(), httpResp, false)
			// reset status code 重置状态码
			service.ResetStatusCode(newAPIError, statusCodeMappingStr)
			return newAPIError
		}
	}

	usage, newAPIError := adaptor.DoResponse(c, httpResp, info)
	if newAPIError != nil {
		// reset status code 重置状态码
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return newAPIError
	}

	service.PostTextConsumeQuota(c, info, usage.(*dto.Usage), nil)
	return nil
}

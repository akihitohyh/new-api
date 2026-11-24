package middleware

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

func abortWithOpenAiMessage(c *gin.Context, statusCode int, message string, code ...types.ErrorCode) {
	if c.FullPath() == constant.ClaudeCountTokensPath {
		AbortWithClaudeError(c, statusCode, message)
		return
	}
	codeStr := ""
	if len(code) > 0 {
		codeStr = string(code[0])
	}
	userId := c.GetInt("id")
	_, preparedPluginRoute := c.Get(pluginruntime.ContextKeyRouteRequest)
	if !preparedPluginRoute || !RespondTaskPluginError(c, &dto.TaskError{
		Code:       codeStr,
		Message:    message,
		StatusCode: statusCode,
	}) {
		c.JSON(statusCode, gin.H{
			"error": gin.H{
				"message": common.MessageWithRequestId(message, c.GetString(common.RequestIdKey)),
				"type":    "new_api_error",
				"code":    codeStr,
			},
		})
	}
	c.Abort()
	logger.LogError(c.Request.Context(), common.LogText("user %d | %s", userId, message))
}

// AbortWithClaudeError writes a gateway-generated error for an Anthropic client.
func AbortWithClaudeError(c *gin.Context, status int, message string) {
	errorType := "api_error"
	switch status {
	case http.StatusBadRequest:
		errorType = "invalid_request_error"
	case http.StatusUnauthorized:
		errorType = "authentication_error"
	case http.StatusForbidden:
		errorType = "permission_error"
	case http.StatusNotFound:
		errorType = "not_found_error"
	case http.StatusRequestEntityTooLarge:
		errorType = "request_too_large"
	case http.StatusTooManyRequests:
		errorType = "rate_limit_error"
	}
	AbortWithClaudeRelayError(c, status, types.ClaudeError{Type: errorType, Message: message})
}

// AbortWithClaudeRelayError preserves the error classification of the relay.
func AbortWithClaudeRelayError(c *gin.Context, status int, relayErr types.ClaudeError) {
	requestID := c.GetString(common.RequestIdKey)
	logger.LogError(c.Request.Context(), common.LogText("user %d | %s", c.GetInt("id"), relayErr.Message))
	relayErr.Message = common.MessageWithRequestId(relayErr.Message, requestID)
	body := gin.H{"type": "error", "error": relayErr}
	if upstreamID := c.Writer.Header().Get("request-id"); upstreamID != "" {
		requestID = upstreamID
	}
	if requestID != "" {
		c.Header("request-id", requestID)
		body["request_id"] = requestID
	}
	c.AbortWithStatusJSON(status, body)
}

// invalidRequestStatus is the status of a request whose body could not be
// read or parsed: 413 when the body exceeds MAX_REQUEST_BODY_MB, else 400.
func invalidRequestStatus(err error) int {
	if common.IsRequestBodyTooLargeError(err) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func abortWithMidjourneyMessage(c *gin.Context, statusCode int, code int, description string) {
	c.JSON(statusCode, gin.H{
		"description": description,
		"type":        "new_api_error",
		"code":        code,
	})
	c.Abort()
	logger.LogError(c.Request.Context(), description)
}

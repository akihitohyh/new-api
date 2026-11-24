package router

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/request_plugin_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListModelsSupportsOpenAIAndGeminiAuthentication(t *testing.T) {
	setupRelayRouterTestDB(t)

	user := model.User{
		Username: "models-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:         user.Id,
		Key:            "modelstestkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}).Error)

	engine := gin.New()
	SetRelayRouter(engine)

	tests := []struct {
		name           string
		path           string
		headerName     string
		expectedObject string
		expectedField  string
	}{
		{
			name:           "OpenAI bearer token",
			path:           "/v1/models",
			headerName:     "Authorization",
			expectedObject: "list",
			expectedField:  "data",
		},
		{
			name:          "Gemini API key header",
			path:          "/v1/models",
			headerName:    "x-goog-api-key",
			expectedField: "models",
		},
		{
			name:          "Gemini API key query",
			path:          "/v1/models?key=modelstestkey",
			expectedField: "models",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.headerName != "" {
				value := "modelstestkey"
				if test.headerName == "Authorization" {
					value = "Bearer " + value
				}
				request.Header.Set(test.headerName, value)
			}

			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code)
			var payload map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			assert.Contains(t, payload, test.expectedField)
			assert.NotContains(t, payload, "error")
			if test.expectedObject != "" {
				assert.Equal(t, test.expectedObject, payload["object"])
			}
		})
	}
}

func TestGeminiRelayHandsLongRunningPredictionToPluginRoute(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, i18n.Init())
	user := model.User{Username: "veo-user", Status: common.UserStatusEnabled, Group: "default", Quota: 100}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{UserId: user.Id, Key: "veotestkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}).Error)

	engine := gin.New()
	SetRelayRouter(engine)
	source, err := plugins.Source("google")
	require.NoError(t, err)
	registry := jsplugin.NewRegistry()
	_, err = registry.RegisterFactory(source, jsplugin.Options{Key: "google"})
	require.NoError(t, err)
	// The plugin route authenticates as in production, then names itself.
	handlers := func(generation *jsplugin.RoutingGeneration, group jsplugin.RouteGroup) []gin.HandlerFunc {
		production := productionPluginRouteHandlers(generation, group)
		return []gin.HandlerFunc{production[0], production[1], production[2], func(c *gin.Context) {
			c.String(http.StatusOK, "plugin %s for user %d", group.Bindings[0].Plugin.Meta.Key, c.GetInt("id"))
		}}
	}
	require.NoError(t, registry.SetGenerationPreparer(newPluginGenerationBuilder(engine.Routes(), nil, handlers).prepare))
	require.Empty(t, registry.RoutingErrors())
	previousRegistry := defaultPluginDispatcher.registry
	defaultPluginDispatcher.registry = registry
	t.Cleanup(func() { defaultPluginDispatcher.registry = previousRegistry })
	engine.NoRoute(defaultPluginDispatcher.dispatch)

	for _, testCase := range []struct {
		method   string
		path     string
		toPlugin bool
	}{
		{method: http.MethodPost, path: "/v1beta/models/veo-3.1-generate-preview:predictLongRunning", toPlugin: true},
		{method: http.MethodGet, path: "/v1beta/models/veo-3.1-generate-preview/operations/task_sample", toPlugin: true},
		{method: http.MethodPost, path: "/v1beta/models/gemini-2.5-flash:generateContent"},
	} {
		t.Run(testCase.path, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(`{}`))
			request.Header.Set("x-goog-api-key", "veotestkey")
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)

			if testCase.toPlugin {
				assert.Equal(t, fmt.Sprintf("plugin google for user %d", user.Id), recorder.Body.String())
				return
			}
			// The relay looks for a Gemini channel serving the model; there is none.
			assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
			assert.Contains(t, recorder.Body.String(), `"code":"model_not_found"`)
		})
	}
}

func setupRelayRouterTestDB(t *testing.T) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	originalIsMasterNode := common.IsMasterNode
	originalRedisEnabled := common.RedisEnabled
	originalSQLitePath := common.SQLitePath
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalSQLDSN, hadSQLDSN := os.LookupEnv("SQL_DSN")

	common.IsMasterNode = false
	common.RedisEnabled = false
	common.SQLitePath = fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, os.Setenv("SQL_DSN", "local"))
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}))

	t.Cleanup(func() {
		if sqlDB, err := model.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		common.IsMasterNode = originalIsMasterNode
		common.RedisEnabled = originalRedisEnabled
		common.SQLitePath = originalSQLitePath
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		if hadSQLDSN {
			require.NoError(t, os.Setenv("SQL_DSN", originalSQLDSN))
		} else {
			require.NoError(t, os.Unsetenv("SQL_DSN"))
		}
	})
}

func TestClaudeCountTokensRelay(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Log{}))
	cacheEnabled, autoDisable, logConsume := common.MemoryCacheEnabled, common.AutomaticDisableChannelEnabled, common.LogConsumeEnabled
	oldRetries := common.RetryTimes
	common.RetryTimes = 3
	t.Cleanup(func() { common.RetryTimes = oldRetries })
	disableCodes := operation_setting.AutomaticDisableStatusCodeRanges
	global := model_setting.GetGlobalSettings()
	globalPassThrough := global.PassThroughRequestEnabled
	t.Cleanup(func() {
		common.MemoryCacheEnabled, common.AutomaticDisableChannelEnabled, common.LogConsumeEnabled = cacheEnabled, autoDisable, logConsume
		operation_setting.AutomaticDisableStatusCodeRanges = disableCodes
		global.PassThroughRequestEnabled = globalPassThrough
	})
	common.MemoryCacheEnabled, common.AutomaticDisableChannelEnabled, common.LogConsumeEnabled = false, true, true
	require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString("429,500"))
	user := model.User{Username: "count-user", Status: common.UserStatusEnabled, Group: "default", Quota: 100}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "counttestkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 75}
	require.NoError(t, model.DB.Create(&token).Error)
	const requestBody = `{ "model":"alias", "messages":[{"role":"user","content":"hello"}], "future_field":{"n":9007199254740993} }`
	const success = `{ "input_tokens":123, "future_field":true }`
	type observed struct {
		path   string
		header http.Header
		body   string
	}
	var mu sync.Mutex
	var calls []observed
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Failf(t, "count_tokens reached OpenAI", "upstream URL: %s", r.URL)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer wrong.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		mu.Lock()
		calls = append(calls, observed{r.URL.RequestURI(), r.Header.Clone(), string(data)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("X-Fixture-Status") {
		case "401":
			key := r.Header.Get("x-api-key")
			auth := r.Header.Get("Authorization")
			_, token, _ := strings.Cut(auth, " ")
			echo := strings.Join([]string{key, auth, token, "native-key"}, " | ")
			w.Header().Set("Request-Id", "req-"+key)
			w.Header().Set("Retry-After", token)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"authentication_error","message":%q},"request_id":%q}`, echo, "req-"+token)
		case "307":
			w.Header().Set("Location", "/private-redirect")
			w.WriteHeader(http.StatusTemporaryRedirect)
			_, _ = io.WriteString(w, "private redirect page")
		case "429":
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"},"request_id":"req1"}`)
		case "502":
			w.Header().Set("request-id", "req-proxy")
			w.WriteHeader(502)
			_, _ = io.WriteString(w, strings.Repeat("x", 65537))
		case "500":
			w.Header().Set("request-id", "req-server")
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"try later native-key at 10.2.3.4","debug":"native-key"},"config":"private"}`)
		default:
			w.Header().Set("request-id", "req-ok")
			_, _ = io.WriteString(w, success)
		}
	}))
	defer upstream.Close()
	native := model.Channel{
		Type: constant.ChannelTypeAnthropic, Name: "native", Key: "native-key", BaseURL: &upstream.URL,
		Group: "default", Models: "alias,claude-opus-4-8-high", Status: common.ChannelStatusEnabled, AutoBan: common.GetPointer(1),
		ModelMapping:   common.GetPointer(`{"alias":"native-model"}`),
		HeaderOverride: common.GetPointer(`{"x-check":"override","x-fixture-status":"{client_header:x-fixture-status}"}`),
		ParamOverride:  common.GetPointer(`{"system":"count system"}`),
	}
	other := model.Channel{
		Type: constant.ChannelTypeOpenAI, Name: "other", Key: "other-key", BaseURL: &wrong.URL,
		Group: "default", Models: "alias,other-only", Status: common.ChannelStatusEnabled, Priority: common.GetPointer(int64(100)),
	}
	for _, ch := range []*model.Channel{&native, &other} {
		require.NoError(t, model.DB.Create(ch).Error)
		require.NoError(t, ch.AddAbilities(nil))
	}
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set(common.RequestIdKey, "req-gateway"); c.Next() })
	SetRelayRouter(engine)
	for _, tc := range []struct {
		name                    string
		status                  int
		response                string
		channelPass, globalPass bool
	}{
		{"mapped request", 200, success, false, false},
		{"normalized model", 200, success, false, false},
		{"rate limited", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"},"request_id":"req1"}`, false, false},
		{"server error", 500, `{"type":"error","error":{"type":"api_error","message":"try later *** at ***.***.***.***"},"request_id":"req-server"}`, false, false},
		{"channel passthrough", 200, success, true, false},
		{"global passthrough", 200, success, false, true},
		{"only OpenAI serves model", 404, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global.PassThroughRequestEnabled = tc.globalPass
			require.NoError(t, model.DB.Model(&native).Update("setting", fmt.Sprintf(`{"pass_through_body_enabled":%t}`, tc.channelPass)).Error)
			payload := requestBody
			if tc.status == 404 {
				payload = strings.Replace(payload, "alias", "other-only", 1)
			}
			if tc.name == "normalized model" {
				payload = strings.Replace(payload, "alias", "claude-opus-4-8-high", 1)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens?beta=true", strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-api-key", token.Key)
			req.Header.Set("anthropic-version", "fixture-version")
			req.Header.Add("anthropic-beta", "first")
			req.Header.Add("anthropic-beta", "second")
			req.Header.Set("x-fixture-status", fmt.Sprint(tc.status))
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			mu.Lock()
			defer mu.Unlock()
			defer func() { calls = nil }()
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == 404 {
				assert.Empty(t, calls)
				var envelope struct {
					Type  string
					Error struct{ Type string }
				}
				require.NoError(t, common.Unmarshal(w.Body.Bytes(), &envelope))
				assert.Equal(t, "error", envelope.Type)
				assert.Equal(t, "not_found_error", envelope.Error.Type)
				return
			}
			require.Len(t, calls, 1)
			call := calls[0]
			assert.Equal(t, "/v1/messages/count_tokens?beta=true", call.path)
			assert.Equal(t, "native-key", call.header.Get("x-api-key"))
			assert.Equal(t, "fixture-version", call.header.Get("anthropic-version"))
			assert.Equal(t, "first,second", call.header.Get("anthropic-beta"))
			assert.Equal(t, "override", call.header.Get("x-check"))
			if tc.channelPass || tc.globalPass {
				assert.Equal(t, requestBody, call.body)
			} else {
				expectedBody := `{"model":"native-model","messages":[{"role":"user","content":"hello"}],"future_field":{"n":9007199254740993},"system":"count system"}`
				if tc.name == "normalized model" {
					expectedBody = strings.Replace(expectedBody, "native-model", "claude-opus-4-8", 1)
				}
				assert.JSONEq(t, expectedBody, call.body)
				assert.Contains(t, call.body, "9007199254740993")
			}
			if tc.status == 200 {
				assert.Equal(t, tc.response, w.Body.String())
				assert.Equal(t, "req-ok", w.Header().Get("request-id"))
			} else {
				assert.JSONEq(t, tc.response, w.Body.String())
			}
			if tc.status == 500 {
				assert.Equal(t, "req-server", w.Header().Get("request-id"))
				assert.NotContains(t, w.Body.String(), "native-key")
				assert.NotContains(t, w.Body.String(), "10.2.3.4")
			}
			if tc.status == 429 {
				assert.Equal(t, "4", w.Header().Get("Retry-After"))
			}
		})
	}
	post := func(path, body, key string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("x-api-key", key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-fixture-status", "200")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}
	takeCalls := func() []observed { mu.Lock(); defer mu.Unlock(); got := calls; calls = nil; return got }
	t.Run("mask outbound credentials", func(t *testing.T) {
		const key = "native-key-override"
		const bearer = "fixture-access-token"
		previous := *native.HeaderOverride
		override, err := common.Marshal(map[string]string{"x-api-key": key, "Authorization": "bEaReR " + bearer, "x-fixture-status": "401"})
		require.NoError(t, err)
		require.NoError(t, model.DB.Model(&native).Update("header_override", string(override)).Error)
		t.Cleanup(func() { require.NoError(t, model.DB.Model(&native).Update("header_override", previous).Error) })
		w := post(constant.ClaudeCountTokensPath, requestBody, token.Key, nil)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		got := takeCalls()
		require.Len(t, got, 1)
		assert.Equal(t, key, got[0].header.Get("x-api-key"))
		assert.Equal(t, "bEaReR "+bearer, got[0].header.Get("Authorization"))
		assert.JSONEq(t, `{"type":"error","error":{"type":"authentication_error","message":"*** | *** | *** | ***"},"request_id":"req-***"}`, w.Body.String())
		assert.Equal(t, "req-***", w.Header().Get("Request-Id"))
		assert.Equal(t, "***", w.Header().Get("Retry-After"))
		for _, secret := range []string{key, bearer, native.Key} {
			assert.NotContains(t, w.Body.String(), secret)
			assert.NotContains(t, fmt.Sprint(w.Header()), secret)
		}
	})
	t.Run("redirect is a localized gateway error", func(t *testing.T) {
		w := post(constant.ClaudeCountTokensPath, requestBody, token.Key, map[string]string{"x-fixture-status": "307", "Accept-Language": "zh-CN"})
		require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "上游令牌计数请求失败")
		assert.NotContains(t, w.Body.String(), "private")
		assert.Empty(t, w.Header().Get("Location"))
		assert.Len(t, takeCalls(), 1)
	})
	t.Run("equivalent route", func(t *testing.T) {
		for _, path := range []string{"/v1/messages/%63ount_tokens", "http://gateway.test/v1/messages/count_tokens?beta=true"} {
			w := post(path, requestBody, token.Key, nil)
			require.Equal(t, 200, w.Code, w.Body.String())
			got := takeCalls()
			require.Len(t, got, 1)
			assert.Equal(t, constant.ClaudeCountTokensPath, strings.SplitN(got[0].path, "?", 2)[0])
		}
	})
	t.Run("oversized error is localized", func(t *testing.T) {
		for lang, want := range map[string]string{"en": "Upstream token counting request failed", "zh-CN": "上游令牌计数请求失败", "zh-TW": "上游權杖計數請求失敗"} {
			w := post(constant.ClaudeCountTokensPath, requestBody, token.Key, map[string]string{"Accept-Language": lang, "x-fixture-status": "502"})
			require.Equal(t, 502, w.Code)
			assert.Contains(t, w.Body.String(), want)
			assert.Less(t, w.Body.Len(), 512)
			assert.Equal(t, "req-proxy", w.Header().Get("request-id"))
			assert.Len(t, takeCalls(), 1)
		}
	})
	t.Run("pinned channels", func(t *testing.T) {
		wrongPin := fmt.Sprintf("%s-%d", token.Key, other.Id)
		w := post(constant.ClaudeCountTokensPath, requestBody, wrongPin, nil)
		require.Equal(t, 403, w.Code)
		require.NoError(t, model.DB.Model(&user).Update("role", common.RoleAdminUser).Error)
		t.Cleanup(func() { require.NoError(t, model.DB.Model(&user).Update("role", common.RoleCommonUser).Error) })
		w = post(constant.ClaudeCountTokensPath, requestBody, wrongPin, nil)
		require.Equal(t, 404, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "not_found_error")
		assert.Equal(t, "req-gateway", w.Header().Get("request-id"))
		assert.Contains(t, w.Body.String(), `"request_id":"req-gateway"`)
		w = post(constant.ClaudeCountTokensPath, requestBody, token.Key+"-99999", nil)
		require.Equal(t, 400, w.Code)
		assert.Empty(t, takeCalls())
	})
	t.Run("request redact", func(t *testing.T) {
		oldErrorLogs := constant.ErrorLogEnabled
		constant.ErrorLogEnabled = true
		t.Cleanup(func() { constant.ErrorLogEnabled = oldErrorLogs })
		source, err := plugins.Source("request-redact")
		require.NoError(t, err)
		previousRegistry := jsplugin.DefaultRegistry
		jsplugin.DefaultRegistry = jsplugin.NewRegistry()
		t.Cleanup(func() { jsplugin.DefaultRegistry = previousRegistry })
		_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "request-redact"})
		require.NoError(t, err)
		saved, err := common.Marshal(request_plugin_setting.GetInstances())
		require.NoError(t, err)
		t.Cleanup(func() {
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{request_plugin_setting.InstancesOption: string(saved)}))
		})
		configure := func(global bool) {
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{request_plugin_setting.InstancesOption: fmt.Sprintf(`[{"id":"count-redact","name":"Redact","plugin":"request-redact","enabled":true,"global":%t,"config":{"categories":["email"]}}]`, global)}))
		}
		configure(true)
		sensitive := strings.Replace(requestBody, "hello", "customer@example.org", 1)
		w := post(constant.ClaudeCountTokensPath, sensitive, token.Key, nil)
		require.Equal(t, 200, w.Code, w.Body.String())
		got := takeCalls()
		require.Len(t, got, 1)
		assert.Contains(t, got[0].body, "[REDACTED]")
		assert.NotContains(t, got[0].body, "customer@example.org")
		require.NoError(t, model.DB.Model(&native).Update("param_override", `{"system":"customer@example.org"}`).Error)
		w = post(constant.ClaudeCountTokensPath, requestBody, token.Key, nil)
		require.Equal(t, 400, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "sensitive_content")
		assert.Contains(t, w.Body.String(), `"type":"new_api_error"`)
		assert.Empty(t, takeCalls())
		configure(false)
		require.NoError(t, model.DB.Model(&native).Update("setting", `{"pass_through_body_enabled":true,"request_plugins":["count-redact"]}`).Error)
		w = post(constant.ClaudeCountTokensPath, sensitive, token.Key, nil)
		require.Equal(t, 400, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "sensitive_content")
		assert.Contains(t, w.Body.String(), `"type":"new_api_error"`)
		assert.Empty(t, takeCalls())
		var policyErrors int64
		require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("type = ? AND channel_id = ?", model.LogTypeError, native.Id).Count(&policyErrors).Error)
		assert.EqualValues(t, 2, policyErrors)
	})
	require.NoError(t, model.DB.First(&native, native.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, native.Status)
	assert.Zero(t, native.UsedQuota)
	require.NoError(t, model.DB.First(&user, user.Id).Error)
	assert.Equal(t, 100, user.Quota)
	assert.Zero(t, user.UsedQuota)
	require.NoError(t, model.DB.First(&token, token.Id).Error)
	assert.Equal(t, 75, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
	var charges int64
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("type = ?", model.LogTypeConsume).Count(&charges).Error)
	assert.Zero(t, charges)
}

// Exercise the two real distributor routes, observing their selected channels
// before generation or billing. Priorities make the ordinary selection deterministic.
func TestClaudeCountTokensLeavesMessageAffinityAlone(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}))
	cache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = cache })
	const modelName = "claude-affinity-test"
	native := model.Channel{Type: constant.ChannelTypeAnthropic, Group: "default", Models: modelName, Status: common.ChannelStatusEnabled, Key: "test", Priority: common.GetPointer(int64(1))}
	other := model.Channel{Type: constant.ChannelTypeOpenAI, Group: "default", Models: modelName, Status: common.ChannelStatusEnabled, Key: "other", Priority: common.GetPointer(int64(2))}
	for _, ch := range []*model.Channel{&native, &other} {
		require.NoError(t, model.DB.Create(ch).Error)
		require.NoError(t, ch.AddAbilities(nil))
	}
	setting := operation_setting.GetChannelAffinitySetting()
	saved := *setting
	const rule = "count-affinity-regression"
	setting.Enabled = true
	setting.KeepOnChannelDisabled = false
	setting.Rules = []operation_setting.ChannelAffinityRule{{Name: rule, ModelRegex: []string{"^claude-"}, PathRegex: []string{"/v1/messages"}, SessionMode: "strict", IncludeRuleName: true, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "gjson", Path: "metadata.user_id"}}}}
	t.Cleanup(func() {
		_, err := service.ClearChannelAffinityCacheByRuleName(rule)
		assert.NoError(t, err)
		*setting = saved
	})
	engine := gin.New()
	engine.Use(middleware.BodyStorageCleanup(), func(c *gin.Context) { common.SetContextKey(c, constant.ContextKeyUsingGroup, "default"); c.Next() }, middleware.Distribute())
	for _, route := range []string{constant.ClaudeCountTokensPath, "/v1/messages"} {
		engine.POST(route, func(c *gin.Context) { c.String(200, "%d", c.GetInt("channel_id")) })
	}
	// First count may not establish a binding. The next count may not clear an
	// OpenAI binding, even when re-selection would now prefer Anthropic.
	steps := []struct {
		path string
		want int
	}{{constant.ClaudeCountTokensPath, native.Id}, {"/v1/messages", other.Id}, {constant.ClaudeCountTokensPath, native.Id}, {"/v1/messages", other.Id}}
	for i, step := range steps {
		if i == 2 {
			require.NoError(t, model.DB.Model(&native).Update("priority", 10).Error)
			require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", native.Id).Update("priority", 10).Error)
		}
		req := httptest.NewRequest(http.MethodPost, step.path, strings.NewReader(`{"model":"claude-affinity-test","messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"count-user"}}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Equal(t, fmt.Sprint(step.want), w.Body.String())
		if i == 0 {
			assert.Zero(t, service.GetChannelAffinityCacheStats().ByRuleName[rule])
		}
	}
}

package router

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
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
	calls := make(chan observed, 2)
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("count_tokens reached OpenAI: %s", r.URL)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer wrong.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		calls <- observed{r.URL.RequestURI(), r.Header.Clone(), string(data)}
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("X-Fixture-Status") {
		case "429":
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"},"request_id":"req1"}`)
		case "500":
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"try later"}}`)
		default:
			_, _ = io.WriteString(w, success)
		}
	}))
	defer upstream.Close()
	native := model.Channel{
		Type: constant.ChannelTypeAnthropic, Name: "native", Key: "native-key", BaseURL: &upstream.URL,
		Group: "default", Models: "alias", Status: common.ChannelStatusEnabled, AutoBan: common.GetPointer(1),
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
	SetRelayRouter(engine)
	for _, tc := range []struct {
		name                    string
		status                  int
		response                string
		channelPass, globalPass bool
	}{
		{"mapped request", 200, success, false, false},
		{"rate limited", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"},"request_id":"req1"}`, false, false},
		{"server error", 500, `{"type":"error","error":{"type":"api_error","message":"try later"}}`, false, false},
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
			req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens?beta=true", strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-api-key", token.Key)
			req.Header.Set("anthropic-version", "fixture-version")
			req.Header.Add("anthropic-beta", "first")
			req.Header.Add("anthropic-beta", "second")
			req.Header.Set("x-fixture-status", fmt.Sprint(tc.status))
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
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
			call := <-calls
			assert.Equal(t, "/v1/messages/count_tokens?beta=true", call.path)
			assert.Equal(t, "native-key", call.header.Get("x-api-key"))
			assert.Equal(t, "fixture-version", call.header.Get("anthropic-version"))
			assert.Equal(t, "first,second", call.header.Get("anthropic-beta"))
			assert.Equal(t, "override", call.header.Get("x-check"))
			if tc.channelPass || tc.globalPass {
				assert.Equal(t, requestBody, call.body)
			} else {
				assert.JSONEq(t, `{"model":"native-model","messages":[{"role":"user","content":"hello"}],"future_field":{"n":9007199254740993},"system":"count system"}`, call.body)
				assert.Contains(t, call.body, "9007199254740993")
			}
			assert.Equal(t, tc.response, w.Body.String())
			if tc.status == 429 {
				assert.Equal(t, "4", w.Header().Get("Retry-After"))
			}
		})
	}
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

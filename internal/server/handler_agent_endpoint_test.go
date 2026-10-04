// agent 独立上游配置的测试。
//
// 测的是三件"配错了会出事"的事：
//   - 密钥绝不下发到前端（这是本特性成立的前提，泄了就等于凭据失守）；
//   - 独立上游【配了一半】时必须退回借用渠道，而不是拿半份配置去打上游；
//   - 密钥留空表示沿用原值，而不是清空。
//
// 为什么单独一个文件而不是并进 handler_agent_test.go：
// 这一组测的是"配置存取 + 优先级判定"，与对话流的鉴权/提示注入是
// 两件不相干的事。放一起会让那个已经很长的文件更难读。
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/config"
	"github.com/LTZY-ACU/ltzy-api/internal/crypto"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/store"
)

// newAgentEndpointTestServer 装配一个带独立上游配置仓储的测试服务。
func newAgentEndpointTestServer(t *testing.T) (*Server, model.AgentEndpointRepository) {
	t.Helper()
	gin.DefaultWriter = io.Discard

	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "agent_endpoint_test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}
	cipher, err := crypto.New(testEncryptionKey)
	if err != nil {
		t.Fatalf("构造加密器失败: %v", err)
	}

	cfg := config.Default()
	cfg.Server.Mode = "test"
	cfg.Server.Listen = "127.0.0.1:0"

	endpoints := store.NewAgentEndpointRepository(st.DB(), cipher)
	srv := New(Deps{
		Config:        cfg,
		Store:         st,
		Channels:      store.NewChannelRepository(st.DB(), cipher),
		ChannelKeys:   store.NewChannelKeyRepository(st.DB(), cipher),
		Users:         store.NewUserRepository(st.DB()),
		Sessions:      store.NewSessionRepository(st.DB()),
		Settings:      store.NewSettingRepository(st.DB(), st.Dialect()),
		AgentEndpoint: endpoints,
	})
	return srv, endpoints
}

// newAgentEndpointAdmin 建立一个管理员用户与一条有效会话，返回会话令牌。
//
// 为什么必须建真实用户：SessionAuth 会载入用户并校验状态，
// 会话上的 user_id 指向不存在的行会得到"账号不存在"，
// 那样请求根本到不了 /admin 处理器，测试会因"接口没生效"而误判。
func newAgentEndpointAdmin(t *testing.T, srv *Server) string {
	t.Helper()
	ctx := context.Background()
	user := &model.User{
		Username: "endpoint-admin", PasswordHash: "test-hash",
		Role: model.UserRoleAdmin, Status: model.UserStatusEnabled,
		Quota: model.QuotaUnlimited,
	}
	if err := srv.deps.Users.Create(ctx, user); err != nil {
		t.Fatalf("创建测试管理员失败: %v", err)
	}
	token := "agent-endpoint-admin-session-" + strconv.FormatUint(user.ID, 10)
	if err := srv.deps.Sessions.Create(ctx, &model.Session{
		UserID:    user.ID,
		TokenHash: crypto.SHA256Hex(token),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("创建测试会话失败: %v", err)
	}
	return token
}

// adminEndpointRequest 以管理员身份调用独立上游配置接口。
func adminEndpointRequest(
	t *testing.T, srv *Server, token, method, path string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// setAgentSettings 预置「运行配置」。
//
// 走 Settings.SetMany 而不是调接口：这里要的是"运行配置处于某个状态"，
// 顺带测一遍配置接口的保存逻辑会与本组测试的目的混在一起。
func setAgentSettings(t *testing.T, srv *Server, values map[string]string) {
	t.Helper()
	if err := srv.deps.Settings.SetMany(context.Background(), values); err != nil {
		t.Fatalf("预置运行配置失败: %v", err)
	}
}

// createEnabledChannel 预置一条启用中的渠道（含自带单密钥）。
//
// 注意只往 channels.api_key 上放密钥、不建 channel_keys：
// 那正是单密钥渠道的真实形态，也是此前 agent 判"无可用密钥"的那个坑。
func createEnabledChannel(t *testing.T, srv *Server, baseURL, apiKey string) {
	t.Helper()
	channel := &model.Channel{
		Name:    "测试渠道",
		Type:    1,
		BaseURL: baseURL,
		APIKey:  apiKey,
		Models:  []string{"channel-model"},
		Group:   model.DefaultGroupName,
		// 权重必须为正：权重为 0 的渠道永远不会被选中，
		// 那样"退回渠道"就退到了一个不存在的目标上，测试会误判。
		Weight: 1,
		// 优先级 0 合法（数值越小越优先），不需要额外设置。
		Priority: 0,
		Status:   model.ChannelStatusEnabled,
	}
	if err := srv.deps.Channels.Create(context.Background(), channel); err != nil {
		t.Fatalf("预置渠道失败: %v", err)
	}
}

// postEndpointAgentChat 以管理员身份发起一次助手对话，返回 SSE 响应。
func postEndpointAgentChat(t *testing.T, srv *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/agent/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAgentEndpoint_SaveAndGet_密钥不回显(t *testing.T) {
	srv, _ := newAgentEndpointTestServer(t)
	token := newAgentEndpointAdmin(t, srv)

	rec := adminEndpointRequest(t, srv, token, http.MethodPut, "/api/admin/agent/endpoint",
		map[string]any{
			"base_url": "https://api.example.com/v1",
			"api_key":  "sk-super-secret-value",
			"model":    "qwen-max",
			"kind":     "openai",
			"enabled":  true,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("保存状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	// 响应体里绝不能出现密钥明文——包括保存响应本身。
	// 只查保存响应是不够的：有的实现会在"保存成功"那次回显，
	// 那种泄法比列表接口回显更隐蔽（站长往往不以为它会被看到）。
	if strings.Contains(rec.Body.String(), "sk-super-secret-value") {
		t.Fatalf("保存响应泄露了密钥明文: %s", rec.Body.String())
	}

	rec = adminEndpointRequest(t, srv, token, http.MethodGet, "/api/admin/agent/endpoint", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-super-secret-value") {
		t.Fatalf("读取响应泄露了密钥明文: %s", rec.Body.String())
	}

	var got agentEndpointResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got.BaseURL != "https://api.example.com/v1" || got.Model != "qwen-max" {
		t.Fatalf("读取到的配置不对: %+v", got)
	}
	// api_key_set 必须为 true，否则前端会显示"还没配密钥"，
	// 站长会以为刚才那次保存失败了。
	if !got.APIKeySet {
		t.Fatalf("api_key_set = false，期望 true（密钥确实存进去了）")
	}
	if !got.Configured {
		t.Fatalf("configured = false，期望 true（地址与密钥都齐了）")
	}
	// kind_options 必须非空：前端下拉框直接用它渲染，
	// 空数组的表现是"只有一个空的下拉框"。
	if len(got.KindOptions) == 0 {
		t.Fatalf("kind_options 为空，前端无法渲染协议类型下拉框")
	}
}

func TestAgentEndpoint_密钥留空沿用原值(t *testing.T) {
	srv, repo := newAgentEndpointTestServer(t)
	ctx := context.Background()

	if err := repo.Save(ctx, &model.AgentEndpoint{
		BaseURL: "https://api.example.com/v1",
		APIKey:  "sk-keep-me",
		Model:   "old-model",
		Enabled: true,
	}); err != nil {
		t.Fatalf("预置配置失败: %v", err)
	}
	token := newAgentEndpointAdmin(t, srv)

	// 只改模型名，api_key 字段完全不传（前端也是这么发的：
	// 密钥框不回显，界面上必然是空的）。
	rec := adminEndpointRequest(t, srv, token, http.MethodPut, "/api/admin/agent/endpoint",
		map[string]any{"model": "new-model"})
	if rec.Code != http.StatusOK {
		t.Fatalf("保存状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}

	stored, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("回读配置失败: %v", err)
	}
	// 这一条是本测试的全部意义：留空必须沿用，而不是清空。
	// 若这里变成空，站长下次助手"莫名其妙改用渠道了"，
	// 而他完全查不出原因（界面上密钥框一直是空的，看不出被清过）。
	if stored.APIKey != "sk-keep-me" {
		t.Fatalf("密钥被改动了：得到 %q，期望沿用 sk-keep-me", stored.APIKey)
	}
	if stored.Model != "new-model" {
		t.Fatalf("模型未更新：得到 %q", stored.Model)
	}
}

func TestAgentEndpoint_只配地址不放行(t *testing.T) {
	srv, _ := newAgentEndpointTestServer(t)
	token := newAgentEndpointAdmin(t, srv)

	// 启用 + 填地址但不填密钥：必须明确拒绝，而不是"存下来等运行时出问题"。
	// 若放行，站长会得到一个"配好了但助手不工作"的状态，
	// 而报错只会在对话时以"上游 401"的形式出现，与配置页完全脱节。
	rec := adminEndpointRequest(t, srv, token, http.MethodPut, "/api/admin/agent/endpoint",
		map[string]any{"base_url": "https://api.example.com/v1", "enabled": true})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "API Key") {
		t.Fatalf("报错未指出缺哪一项: %s", rec.Body.String())
	}
}

func TestAgentEndpoint_未启用时不校验(t *testing.T) {
	srv, _ := newAgentEndpointTestServer(t)
	token := newAgentEndpointAdmin(t, srv)

	// 只填地址、先不打开开关：必须允许保存。
	// 站长正常的操作顺序就是"先把值填进来看一眼，最后再打开开关"，
	// 这里拦下来只会让他以为自己填错了方向。
	rec := adminEndpointRequest(t, srv, token, http.MethodPut, "/api/admin/agent/endpoint",
		map[string]any{"base_url": "https://api.example.com/v1", "enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	var got agentEndpointResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	// 未启用 + 无密钥 → configured 必须是 false，界面据此显示"还没配完"。
	if got.Configured {
		t.Fatalf("configured = true，期望 false（没开开关也没密钥）")
	}
}

func TestResolveEndpointModel(t *testing.T) {
	full := &model.AgentEndpoint{
		BaseURL: "https://api.example.com/v1",
		APIKey:  "sk-x",
		Model:   "endpoint-model",
		Enabled: true,
	}
	// 只配了一半：地址有、密钥无，且开了开关。
	// 这是最需要被挡住的情况——拿它去打上游必然 401。
	halfBaked := &model.AgentEndpoint{
		BaseURL: "https://api.example.com/v1",
		Model:   "endpoint-model",
		Enabled: true,
	}
	noModel := &model.AgentEndpoint{
		BaseURL: "https://api.example.com/v1",
		APIKey:  "sk-x",
		Enabled: true,
	}

	cases := []struct {
		name      string
		endpoint  *model.AgentEndpoint
		role      model.AgentRole
		requested string
		want      string
	}{
		{
			name: "运维请求指定的模型优先于独立上游的模型",
			// 这是站长排障时临时切模型的入口，必须压过配置值。
			endpoint: full, role: model.AgentRoleOps, requested: "ops-override",
			want: "ops-override",
		},
		{
			name:     "运维未指定时用独立上游的模型",
			endpoint: full, role: model.AgentRoleOps,
			want: "endpoint-model",
		},
		{
			name: "客服忽略请求里指定的模型",
			// 允许外部人指定模型 = 让他决定站长付哪个模型的钱。见 resolveAgentModel。
			endpoint: full, role: model.AgentRoleSupport, requested: "expensive-model",
			want: "endpoint-model",
		},
		{
			name: "配了一半时整体退回运行配置",
			// 半份配置一律不算数：与 agent 包里的 resolveUpstream 必须同口径，
			// 否则会出现"用独立上游的密钥去请求渠道的模型名"。
			endpoint: halfBaked, role: model.AgentRoleOps, requested: "",
			want: "",
		},
		{
			name: "配了一半但运维显式指定模型时仍然生效",
			// 显式指定与"上游是谁"无关，不能被配置状态连累。
			endpoint: halfBaked, role: model.AgentRoleOps, requested: "ops-override",
			want: "ops-override",
		},
		{
			name: "独立上游没配模型时退回运行配置",
			// "只换上游不换模型"是一次零成本操作，必须支持。
			endpoint: noModel, role: model.AgentRoleOps, requested: "",
			want: "",
		},
		{
			name:     "未接入仓储（nil）时运维指定仍生效",
			endpoint: nil, role: model.AgentRoleOps, requested: "ops-override",
			// 运维指定的模型与上游是谁无关，必须照常生效。
			want: "ops-override",
		},
		{
			name:     "未接入仓储且未指定模型",
			endpoint: nil, role: model.AgentRoleSupport,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveEndpointModel(tc.endpoint, tc.role, tc.requested)
			if got != tc.want {
				t.Fatalf("resolveEndpointModel = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestAgentEndpoint_独立上游优先于渠道 是整条链路的端到端验证：
// 站点渠道的地址是打不通的假地址，只有独立上游是好的。
// 若不是独立上游优先（或根本没生效），这里必然失败。
func TestAgentEndpoint_独立上游优先于渠道(t *testing.T) {
	srv, repo := newAgentEndpointTestServer(t)
	ctx := context.Background()

	var receivedPath, receivedAuth, receivedModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		receivedModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"choices":[{"message":{"role":"assistant","content":"独立上游答了"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)

	if err := repo.Save(ctx, &model.AgentEndpoint{
		BaseURL: upstream.URL + "/v1",
		APIKey:  "sk-endpoint-only",
		Model:   "endpoint-only-model",
		Enabled: true,
	}); err != nil {
		t.Fatalf("预置配置失败: %v", err)
	}
	// 同时存在一个渠道侧的模型名：独立上游必须胜出——这正是本特性的意义。
	createEnabledChannel(t, srv, "https://channel.invalid/v1", "sk-channel-key")
	setAgentSettings(t, srv, map[string]string{
		model.SettingKeyAgentEnabled:      "true",
		model.SettingKeyAgentDefaultModel: "channel-model",
	})
	token := newAgentEndpointAdmin(t, srv)

	rec := postEndpointAgentChat(t, srv, token, `{"question":"你好","model":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("对话状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "独立上游答了") {
		t.Fatalf("没有拿到独立上游的回答；body=%s", rec.Body.String())
	}
	if receivedPath != "/v1/chat/completions" {
		t.Fatalf("请求路径 = %q，期望 /v1/chat/completions", receivedPath)
	}
	if receivedAuth != "Bearer sk-endpoint-only" {
		t.Fatalf("Authorization = %q，期望独立上游的密钥", receivedAuth)
	}
	// 模型名也必须取独立上游那份，否则就是"用独立上游的地址配渠道的模型名"，
	// 那种错配在上游侧表现为 404，而站长看不出是哪一步错了。
	if receivedModel != "endpoint-only-model" {
		t.Fatalf("模型 = %q，期望 endpoint-only-model", receivedModel)
	}
}

// TestAgentEndpoint_未配齐时退回渠道 是上一条的镜像：
// 配了一半（没密钥）就必须退回渠道，而不是拿半份配置去打独立上游。
func TestAgentEndpoint_未配齐时退回渠道(t *testing.T) {
	srv, repo := newAgentEndpointTestServer(t)
	ctx := context.Background()

	var calledEndpoint bool
	endpointSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledEndpoint = true
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(endpointSrv.Close)

	if err := repo.Save(ctx, &model.AgentEndpoint{
		BaseURL: endpointSrv.URL,
		Model:   "endpoint-model",
		Enabled: true,
		// 故意不填 APIKey。
	}); err != nil {
		t.Fatalf("预置配置失败: %v", err)
	}
	// 渠道地址给一个必然打不通的域名：对话最终会失败，
	// 但失败点必须落在渠道上，这正是"退回渠道"的证据。
	createEnabledChannel(t, srv, "https://channel.invalid/v1", "sk-channel-key")
	setAgentSettings(t, srv, map[string]string{
		model.SettingKeyAgentEnabled:      "true",
		model.SettingKeyAgentDefaultModel: "channel-model",
	})
	token := newAgentEndpointAdmin(t, srv)

	rec := postEndpointAgentChat(t, srv, token, `{"question":"你好","model":""}`)

	// 关键断言是 calledEndpoint 为 false：半份配置绝不能被打出去。
	// 哪怕这次对话最终因为渠道地址不通而失败，只要独立上游没被碰过，
	// 就说明退回逻辑生效了。
	if calledEndpoint {
		t.Fatalf("配置不完整却仍然请求了独立上游，应该退回借用渠道")
	}
	// 报错必须把方向指出来（"渠道地址"），而不是笼统的"请稍后重试"——
	// 观众是站长本人，回一句"稍后重试"等于废掉他的诊断能力。
	//
	// 这里刻意【不】断言具体域名：llm.doRequest 故意不透传底层网络错误
	// （可能含内网地址），只保留"连不上"这个事实。那是刻意的取舍，
	// 测试若反过来要求它泄漏细节，就等于把那条防线判成了错误。
	if !strings.Contains(rec.Body.String(), "渠道") {
		t.Fatalf("错误信息未指明问题在渠道：%s", rec.Body.String())
	}
}

// 加速器后台接口的单元测试。
//
// 测试重点：
//  1. **默认关闭** —— 全新站点打开页面，加速器必须是关的。
//     默认开启等于给所有站长施加一次未经同意的行为变更。
//  2. **局部提交不影响其他项** —— 站长只想点开总开关时，
//     上次调好的参数不能被打回默认。
//  3. **越界值被拒且指明字段** —— 前端要能把红框标在对应输入框上。
//  4. **PUT 后 Relay 真的生效** —— 这是最容易做成"看起来保存了、
//     实际没生效"的地方。
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/config"
	"github.com/LTZY-ACU/ltzy-api/internal/crypto"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/relay"
	"github.com/LTZY-ACU/ltzy-api/internal/store"
)

// newAccelTestServer 装配一个带 Relay 的测试服务。
func newAccelTestServer(t *testing.T) (*Server, *relay.Relay) {
	t.Helper()
	gin.DefaultWriter = io.Discard

	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "accel_test.db"))
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

	settings := store.NewSettingRepository(st.DB(), st.Dialect())
	channels := store.NewChannelRepository(st.DB(), cipher)
	usageLogs := store.NewUsageLogRepository(st.DB(), st.Dialect())
	// relay.Options 不接设置仓储：加速器配置是启动时由
	// applyAcceleratorFromSettings 灌进去的，不是构造参数。
	// 这里给 UsageLogs 是为了让 GET 的效果视图有真实数据可读。
	r := relay.New(channels, relay.Options{UsageLogs: usageLogs})

	srv := New(Deps{
		Config:      cfg,
		Store:       st,
		Settings:    settings,
		Channels:    channels,
		ChannelKeys: store.NewChannelKeyRepository(st.DB(), cipher),
		Users:       store.NewUserRepository(st.DB()),
		Sessions:    store.NewSessionRepository(st.DB()),
		Tokens:      store.NewTokenRepository(st.DB(), cipher),
		UsageLogs:   usageLogs,
		Relay:       r,
	})
	return srv, r
}

// accelAdmin 建立管理员会话。
func accelAdmin(t *testing.T, srv *Server) string {
	t.Helper()
	ctx := context.Background()
	user := &model.User{
		Username: "accel-admin", PasswordHash: "test-hash",
		Role: model.UserRoleAdmin, Status: model.UserStatusEnabled,
	}
	if err := srv.deps.Users.Create(ctx, user); err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}
	token := "accel-session-token"
	// 会话表只存摘要，明文只在签发时返回一次。
	if err := srv.deps.Sessions.Create(ctx, &model.Session{
		TokenHash: crypto.SHA256Hex(token), UserID: user.ID,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	// 加速器不挂二次验证闸门（改的是性能参数不是凭据），
	// 但会话本身必须是"已完成验证"状态，否则会被更前面的通用闸门拦下。
	return mustReauthedSession(t, srv.deps.Sessions, token)
}

// accelRequest 发一个带管理员鉴权的请求。
func accelRequest(t *testing.T, srv *Server, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeAccel 解析响应。
func decodeAccel(t *testing.T, rec *httptest.ResponseRecorder) acceleratorGetResponse {
	t.Helper()
	var out acceleratorGetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	return out
}

// TestAccelerator_默认关闭 守住"加速器是一次行为变更"这条底线。
func TestAccelerator_默认关闭(t *testing.T) {
	srv, _ := newAccelTestServer(t)
	token := accelAdmin(t, srv)

	rec := accelRequest(t, srv, token, http.MethodGet, "/api/admin/accelerator", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	got := decodeAccel(t, rec)
	if got.Setting.Enabled {
		t.Error("加速器默认必须是关闭的：默认开启等于给所有站长施加未经同意的行为变更")
	}
	if got.Setting.LatencyRouting || got.Setting.CachePassthrough {
		t.Error("总开关关闭时子项也不应显示为开启")
	}
	if got.Latency.Enabled || got.Cache.Enabled {
		t.Error("效果视图里的 enabled 应与配置一致")
	}
	// 边界必须下发，否则前端只能硬编码。
	if got.Bounds.LatencyWeightMax != model.LatencyWeightMax {
		t.Errorf("未下发参数边界: %+v", got.Bounds)
	}
}

// TestAccelerator_保存后立即生效 验证热更新真的生效。
//
// 这条最容易做成"看起来保存了、实际没生效"：
// 设置写进了数据库，但 Relay 用的还是旧值。
func TestAccelerator_保存后立即生效(t *testing.T) {
	srv, r := newAccelTestServer(t)
	token := accelAdmin(t, srv)

	rec := accelRequest(t, srv, token, http.MethodPut, "/api/admin/accelerator",
		`{"enabled":true,"latency_routing":true,"latency_weight":80}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}

	// 数据库里存了。
	got := accelRequest(t, srv, token, http.MethodGet, "/api/admin/accelerator", "")
	setting := decodeAccel(t, got).Setting
	if !setting.Enabled || !setting.LatencyRouting || setting.LatencyWeight != 80 {
		t.Fatalf("数据库里的值不对: %+v", setting)
	}

	// Relay 里也生效了 —— 这是关键断言。
	if r.AcceleratorSetting().LatencyWeightRatio() <= 0 {
		t.Errorf("Relay 未热更新，ratio = %v：界面显示已开启但转发行为没变",
			r.AcceleratorSetting().LatencyWeightRatio())
	}

	// 再读一次 GET，确认读到的是数据库里的值。
	again := decodeAccel(t, got).Setting
	if !again.Enabled {
		t.Error("GET 未回读数据库")
	}
}

// TestAccelerator_局部提交不影响其他项 守住"只想点个开关"这个常见操作。
func TestAccelerator_局部提交不影响其他项(t *testing.T) {
	srv, _ := newAccelTestServer(t)
	token := accelAdmin(t, srv)

	// 先精调一组参数。
	first := accelRequest(t, srv, token, http.MethodPut, "/api/admin/accelerator",
		`{"enabled":true,"latency_weight":77,"max_idle_conns_per_host":128,"idle_conn_timeout_ms":120000}`)
	if first.Code != http.StatusOK {
		t.Fatalf("首次 PUT 失败: %s", first.Body.String())
	}

	// 只改总开关。
	second := accelRequest(t, srv, token, http.MethodPut, "/api/admin/accelerator",
		`{"enabled":false}`)
	if second.Code != http.StatusOK {
		t.Fatalf("二次 PUT 失败: %s", second.Body.String())
	}

	got := decodeAccel(t, accelRequest(t, srv, token, http.MethodGet, "/api/admin/accelerator", ""))
	if got.Setting.Enabled {
		t.Error("总开关未关闭")
	}
	if got.Setting.LatencyWeight != 77 {
		t.Errorf("latency_weight = %d，被打回默认了：局部提交不应影响其他项",
			got.Setting.LatencyWeight)
	}
	if got.Setting.MaxIdleConnsPerHost != 128 {
		t.Errorf("max_idle_conns_per_host = %d，被打回默认了", got.Setting.MaxIdleConnsPerHost)
	}
}

// TestAccelerator_越界值被拒且指明字段 验证前端能定位到输入框。
func TestAccelerator_越界值被拒且指明字段(t *testing.T) {
	srv, _ := newAccelTestServer(t)
	token := accelAdmin(t, srv)

	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"延迟权重超上限", `{"enabled":true,"latency_weight":999}`, "latency_weight"},
		{"延迟权重低于下限", `{"enabled":true,"latency_weight":0}`, "latency_weight"},
		{"连接数超上限", `{"max_idle_conns_per_host":100000}`, "max_idle_conns_per_host"},
		{"连接数低于下限", `{"max_idle_conns_per_host":1}`, "max_idle_conns_per_host"},
		{"回收时间超范围", `{"idle_conn_timeout_ms":10}`, "idle_conn_timeout_ms"},
	}
	for _, tc := range cases {
		rec := accelRequest(t, srv, token, http.MethodPut, "/api/admin/accelerator", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: 状态码 = %d，期望 400；body=%s", tc.name, rec.Code, rec.Body.String())
			continue
		}
		var resp struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
				Field   string `json:"field"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Errorf("%s: 响应不是合法 JSON: %v", tc.name, err)
			continue
		}
		if resp.Error.Field != tc.field {
			t.Errorf("%s: field = %q，期望 %q（前端据此把红框标在对应输入框上）",
				tc.name, resp.Error.Field, tc.field)
		}
		if resp.Error.Message == "" {
			t.Errorf("%s: 缺少可读的错误文案", tc.name)
		}
		// code / type 不能缺：前端 toApiError 会按 code 分支处理错误，
		// 缺了虽然有 ?? '' 兜底不会崩，但会静默落到"未知错误"分支。
		if resp.Error.Code != "accelerator_invalid_param" {
			t.Errorf("%s: code = %q，期望 accelerator_invalid_param", tc.name, resp.Error.Code)
		}
		if resp.Error.Type == "" {
			t.Errorf("%s: 缺少 type（与其他错误响应的形状不一致）", tc.name)
		}
	}
}

// TestAccelerator_脏数据被钳位而非拒绝 守住"加速器配错不该让站点起不来"。
func TestAccelerator_脏数据被钳位而非拒绝(t *testing.T) {
	// 直接往设置表写越界值（模拟有人手工改库）。
	values := map[string]string{
		model.SettingKeyAcceleratorEnabled:             "true",
		model.SettingKeyAcceleratorLatencyRouting:      "yes",
		model.SettingKeyAcceleratorLatencyWeight:       "99999",
		model.SettingKeyAcceleratorMaxIdleConnsPerHost: "-5",
		model.SettingKeyAcceleratorIdleConnTimeoutMS:   "abc",
		model.SettingKeyAcceleratorCacheMinTokens:      "-100",
	}
	got := model.LoadAcceleratorSetting(values)

	if got.LatencyWeight != model.LatencyWeightMax {
		t.Errorf("延迟权重 = %d，应被钳到上限 %d", got.LatencyWeight, model.LatencyWeightMax)
	}
	if got.MaxIdleConnsPerHost != model.MaxIdleConnsPerHostMin {
		t.Errorf("连接数 = %d，应被钳到下限 %d", got.MaxIdleConnsPerHost, model.MaxIdleConnsPerHostMin)
	}
	if got.IdleConnTimeoutMS != model.IdleConnTimeoutDefaultMS {
		t.Errorf("无法解析时应回退默认值，实际 %d", got.IdleConnTimeoutMS)
	}
	if got.CacheMinTokens != 0 {
		t.Errorf("负的缓存阈值应钳到 0，实际 %d", got.CacheMinTokens)
	}
	if !got.LatencyRouting {
		t.Error("\"yes\" 应被解析为 true")
	}
}

// TestAccelerator_配置往返不丢字段 守住"保存后读回一致"。
func TestAccelerator_配置往返不丢字段(t *testing.T) {
	original := model.AcceleratorSetting{
		Enabled:             true,
		LatencyRouting:      true,
		LatencyWeight:       66,
		CachePassthrough:    true,
		CacheMinTokens:      2048,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeoutMS:   45_000,
		Expect100Continue:   true,
	}
	values := model.AcceleratorValuesToMap(original)
	back := model.LoadAcceleratorSetting(values)

	if back != original {
		t.Errorf("往返不一致:\n原始 = %+v\n读回 = %+v", original, back)
	}
}

// TestAccelerator_关闭后子项失效但保留原值 守住界面的诚实性。
//
// 站长关了总开关，界面上子项还应显示为他填的值（而不是被清空），
// 否则他无法知道"我之前开过什么"。
func TestAccelerator_关闭后子项失效但保留原值(t *testing.T) {
	orig := model.AcceleratorSetting{
		Enabled: false, LatencyRouting: true, LatencyWeight: 90,
		CachePassthrough: true,
	}
	eff := orig.Effective()
	if eff.LatencyRouting || eff.CachePassthrough {
		t.Error("Effective 未收敛子项")
	}
	if eff.LatencyWeight != 90 {
		t.Error("Effective 不该改数值型子项（界面要如实展示站长填的值）")
	}
	if !orig.LatencyRouting {
		t.Error("Effective 修改了入参")
	}
}

// TestAccelerator_响应字段是蛇形命名 守住前后端契约。
//
// 这条是线上实测抓到的：Go 结构体漏写 json tag 时，接口会返回
// `{"Enabled": false, "LatencyRouting": false, ...}`（大写开头），
// 而前端按 snake_case 取值 —— 于是界面上所有开关都读不到，
// 表现为"配置保存了但页面永远显示默认值"。
//
// 单测查不出来是因为它们直接比较 Go 结构体，不经过序列化；
// 契约错误只在真实的 JSON 往返里才暴露。
func TestAccelerator_响应字段是蛇形命名(t *testing.T) {
	srv, _ := newAccelTestServer(t)
	token := accelAdmin(t, srv)

	rec := accelRequest(t, srv, token, http.MethodGet, "/api/admin/accelerator", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}

	// 直接看原始 JSON 的键，而不是解析后的结构体 ——
	// 解析会按 Go 字段名匹配，正好把契约错误藏起来。
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	setting, ok := raw["setting"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 setting 字段: %s", rec.Body.String())
	}
	for _, key := range []string{
		"enabled", "latency_routing", "latency_weight",
		"cache_passthrough", "cache_min_tokens",
		"max_idle_conns_per_host", "idle_conn_timeout_ms", "expect_100_continue",
	} {
		if _, found := setting[key]; !found {
			t.Errorf("响应里没有 snake_case 的 %q 键：前端按 snake_case 取值会全部读不到。"+
				"实际键名 = %v", key, keysOf(setting))
		}
	}
	// 大写开头的键意味着 json tag 漏了
	for key := range setting {
		if key != "" && key[0] >= 'A' && key[0] <= 'Z' {
			t.Errorf("响应里出现大写开头的键 %q：Go 结构体漏了 json tag", key)
		}
	}
}

// keysOf 返回 map 的键列表（仅用于失败时的报错信息）。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

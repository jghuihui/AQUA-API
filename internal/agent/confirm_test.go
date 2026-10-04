// 二次确认机制的单元测试。
//
// 这个机制的失败模式全是"本该被拦住的写操作执行了"，而且【不会报错】——
// 封禁用户、发布公告这类操作一旦执行成功，界面上看不出任何异常，
// 只有站长事后才发现。因此这里的重点不是"确认流程能跑通"，
// 而是"该拦的都被拦住了"，尤其是这几条：
//
//  1. 没有确认通道（OnConfirmRequired 未挂载）时，ConfirmAlways 工具【必须】拒绝执行；
//  2. 确认之后执行的是【被展示过的那份参数】，不是模型新解析出来的；
//  3. 工具名与被确认的不一致时必须拒绝（防"确认 A 执行 B"）；
//  4. ConfirmAlways 的工具必须有 SummarizeChange —— 没有明细的确认单
//     等于让站长闭着眼点同意。
//
// 这些用例共用一个"记录副作用"的假仓储：任何写操作都会在 writes 里留痕，
// 于是"有没有真的动过数据"可以被断言，而不是靠推测。
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// ── 假仓储 ──────────────────────────────────────────────────────

// recordingSettings 是一个会记录写入的假设置仓储。
//
// 只实现本文件用到的方法；其余方法属于嵌入的 nil 接口，
// 一旦被调用会 panic —— 这比默默返回零值更容易发现"测试没覆盖到"。
type recordingSettings struct {
	model.SettingRepository
	values map[string]string
	writes []string
}

func newRecordingSettings(pairs map[string]string) *recordingSettings {
	v := make(map[string]string, len(pairs))
	for k, val := range pairs {
		v[k] = val
	}
	return &recordingSettings{values: v}
}

func (r *recordingSettings) Get(_ context.Context, key string) (string, error) {
	return r.values[key], nil
}

func (r *recordingSettings) GetAll(_ context.Context) (map[string]string, error) {
	out := make(map[string]string, len(r.values))
	for k, v := range r.values {
		out[k] = v
	}
	return out, nil
}

func (r *recordingSettings) Set(_ context.Context, key, value string) error {
	r.writes = append(r.writes, key+"="+value)
	r.values[key] = value
	return nil
}

// recordingAnnouncements 是会记录创建的假公告仓储。
type recordingAnnouncements struct {
	model.AnnouncementRepository
	created []*model.Announcement
}

func (r *recordingAnnouncements) Create(_ context.Context, item *model.Announcement) error {
	item.ID = uint64(len(r.created) + 1)
	r.created = append(r.created, item)
	return nil
}

// recordingWords 是会记录新增的假敏感词仓储。
type recordingWords struct {
	model.SensitiveWordRepository
	existing map[string]bool
	added    []*model.SensitiveWord
}

func (r *recordingWords) CreateMany(_ context.Context, words []*model.SensitiveWord) (int, error) {
	n := 0
	for _, w := range words {
		if r.existing[w.MatchKey()] {
			continue
		}
		r.existing[w.MatchKey()] = true
		w.ID = uint64(len(r.added) + 1)
		r.added = append(r.added, w)
		n++
	}
	return n, nil
}

// newConfirmTools 构造一个带站点运营仓储的工具集。
func newConfirmTools(settings *recordingSettings) (*AgentTools, *recordingAnnouncements, *recordingWords) {
	anns := &recordingAnnouncements{}
	words := &recordingWords{existing: map[string]bool{}}
	return &AgentTools{
		Settings:       settings,
		Announcements:  anns,
		SensitiveWords: words,
	}, anns, words
}

// callOps 以运维角色调用一个工具。
func callOps(t *testing.T, tools *AgentTools, name string, args any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("参数序列化失败: %v", err)
	}
	return tools.Execute(context.Background(), model.AgentRoleOps, name, raw)
}

// ── 1. 没有确认通道时必须拒绝 ───────────────────────────────────

// TestExecuteConfirmed_没有确认通道时拒绝执行 是整个机制的地基。
//
// 若这条退化（找不到通道就当"不需要确认"），任何没有挂 OnConfirmRequired 的部署
// —— 例如自建二进制升级后忘了接线的场景 —— 会在毫无提示的情况下
// 获得无限制的写权限，而且不会有任何报错。
func TestExecuteConfirmed_没有确认通道时拒绝执行(t *testing.T) {
	settings := newRecordingSettings(map[string]string{"site_name": "旧站点名"})
	tools, _, _ := newConfirmTools(settings)

	_, err := callOps(t, tools, "set_site_setting", map[string]string{
		"key": "site_name", "value": "新站点名",
	})
	if err == nil {
		t.Fatal("未挂确认通道时高危写操作被执行了")
	}
	if !IsConfirmRequired(err) {
		t.Errorf("err = %v，期望 ErrConfirmRequired", err)
	}
	// 关键断言：数据必须没被动过。
	if len(settings.writes) != 0 {
		t.Errorf("被拒绝的操作仍产生了写入 %v", settings.writes)
	}
	if got := settings.values["site_name"]; got != "旧站点名" {
		t.Errorf("site_name = %q，应保持原值", got)
	}
}

// TestExecuteConfirmed_确认后写入被展示过的参数 验证"展示什么就执行什么"。
//
// 场景：站长看到确认单上写着关掉注册，点确认；期间模型在同一句问题下
// 又解析出了另一个键。若执行的是模型新解析的值，就成了"批准 A 执行 B"。
func TestExecuteConfirmed_确认后写入被展示过的参数(t *testing.T) {
	settings := newRecordingSettings(map[string]string{"site_name": "旧站点名"})
	tools, _, _ := newConfirmTools(settings)

	confirmed := &ConfirmedCall{
		ToolName: "set_site_setting",
		Params:   json.RawMessage(`{"key":"site_name","value":"新站点名"}`),
	}
	raw, err := json.Marshal(map[string]string{"key": "site_name", "value": "模型重新解析的值"})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	if _, err := tools.ExecuteConfirmed(
		context.Background(), model.AgentRoleOps, "set_site_setting", raw, confirmed,
	); err != nil {
		t.Fatalf("确认后执行失败: %v", err)
	}
	if len(settings.writes) != 1 {
		t.Fatalf("应恰好写入 1 次，实际 %v", settings.writes)
	}
	if settings.writes[0] != "site_name=新站点名" {
		t.Errorf("写入 = %q，应为被展示过的 site_name=新站点名", settings.writes[0])
	}
}

// TestExecuteConfirmed_工具名不匹配时拒绝 验证"确认 A 执行 B"被拦住。
//
// 这不是假想：前端可能把上一次响应的确认单带到下一次请求
// （站长在两次操作之间点了别的工具）。
func TestExecuteConfirmed_工具名不匹配时拒绝(t *testing.T) {
	settings := newRecordingSettings(nil)
	tools, anns, _ := newConfirmTools(settings)

	confirmed := &ConfirmedCall{
		ToolName: "set_site_setting", // 站长确认的是这个
		Params:   json.RawMessage(`{"key":"site_name","value":"X"}`),
	}
	raw, _ := json.Marshal(map[string]any{"title": "偷偷发的公告", "content": "正文"})
	_, err := tools.ExecuteConfirmed(
		context.Background(), model.AgentRoleOps, "publish_announcement", raw, confirmed,
	)
	if err == nil {
		t.Fatal("工具名不一致时竟然执行了")
	}
	if !strings.Contains(err.Error(), "不一致") {
		t.Errorf("错误信息 = %v，应明确指出确认的是哪个工具、要执行的是哪个", err)
	}
	if len(anns.created) != 0 {
		t.Errorf("被拒绝的操作仍创建了公告：%+v", anns.created)
	}
}

// TestExecuteConfirmed_只读工具忽略确认凭证 验证确认的作用范围不会悄悄扩大。
//
// 前端可能把上次的确认单误带到只读工具的调用上。若把凭证里的参数
// 用到只读工具上，一次确认就管住了两件事。
func TestExecuteConfirmed_只读工具忽略确认凭证(t *testing.T) {
	settings := newRecordingSettings(map[string]string{
		"site_name":       "真站点名",
		"smtp_password":   "smtp-secret",
		"payment_api_key": "pay-secret",
		"jwt_secret":      "jwt-secret",
		"agent_api_key":   "agent-secret",
	})
	tools, _, _ := newConfirmTools(settings)

	// 带一个指向别处的凭证去调只读工具。
	confirmed := &ConfirmedCall{
		ToolName: "set_site_setting",
		Params:   json.RawMessage(`{"key":"site_name","value":"被污染的值"}`),
	}
	out, err := tools.ExecuteConfirmed(
		context.Background(), model.AgentRoleOps, "get_site_settings", json.RawMessage(`{}`), confirmed,
	)
	if err != nil {
		t.Fatalf("只读工具不应因确认凭证而失败: %v", err)
	}
	encoded, _ := json.Marshal(out)
	body := string(encoded)

	if strings.Contains(body, "被污染的值") {
		t.Errorf("只读工具用上了确认凭证里的参数: %s", body)
	}
	if got := settings.values["site_name"]; got != "真站点名" {
		t.Errorf("site_name = %q，被只读调用改掉了", got)
	}
}

// ── 2. 设置类工具的行为 ─────────────────────────────────────────

// TestSetSiteSetting_拒绝白名单外的键 验证改设置是白名单而非黑名单。
//
// "看起来不像敏感字段"不代表安全：关掉注册、清空默认额度都能影响全站。
// 站长没要求的设置项，助手不该有权限动。
func TestSetSiteSetting_拒绝白名单外的键(t *testing.T) {
	settings := newRecordingSettings(map[string]string{"smtp_password": "x"})
	tools, _, _ := newConfirmTools(settings)

	for _, key := range []string{"smtp_password", "jwt_secret", "payment_api_key", "site_logo", ""} {
		confirmed := &ConfirmedCall{
			ToolName: "set_site_setting",
			Params:   json.RawMessage(`{"key":"` + key + `","value":"x"}`),
		}
		if _, err := tools.ExecuteConfirmed(
			context.Background(), model.AgentRoleOps, "set_site_setting",
			json.RawMessage(`{"key":"`+key+`","value":"x"}`), confirmed,
		); err == nil {
			t.Errorf("白名单外的键 %q 被接受了", key)
		}
	}
	if len(settings.writes) != 0 {
		t.Errorf("被拒绝的操作仍产生了写入 %v", settings.writes)
	}
}

// TestSetSiteSetting_布尔值容错 但错值必须拒。
//
// 关闭注册是站长一定会盯着看的字段，一个手滑的 "flase" 若被当成 false 写进去
// （Set 是覆盖写），后果是站点悄悄关了注册而没人发现。
func TestSetSiteSetting_布尔值容错(t *testing.T) {
	accept := map[string]string{
		"true": "true", "TRUE": "true", "1": "true", "yes": "true", "是": "true",
		"false": "false", "0": "false", "no": "false", "否": "false",
	}
	for in, want := range accept {
		settings := newRecordingSettings(nil)
		tools, _, _ := newConfirmTools(settings)
		confirmed := &ConfirmedCall{
			ToolName: "set_site_setting",
			Params:   json.RawMessage(`{"key":"register_enabled","value":"` + in + `"}`),
		}
		if _, err := tools.ExecuteConfirmed(
			context.Background(), model.AgentRoleOps, "set_site_setting",
			json.RawMessage(`{"key":"register_enabled","value":"`+in+`"}`), confirmed,
		); err != nil {
			t.Fatalf("register_enabled=%q 应被接受: %v", in, err)
		}
		if got := settings.values["register_enabled"]; got != want {
			t.Errorf("register_enabled=%q 存成了 %q，应为 %q", in, got, want)
		}
	}

	for _, in := range []string{"flase", "禁用", "off-ish", ""} {
		settings := newRecordingSettings(nil)
		tools, _, _ := newConfirmTools(settings)
		confirmed := &ConfirmedCall{
			ToolName: "set_site_setting",
			Params:   json.RawMessage(`{"key":"register_enabled","value":"` + in + `"}`),
		}
		if _, err := tools.ExecuteConfirmed(
			context.Background(), model.AgentRoleOps, "set_site_setting",
			json.RawMessage(`{"key":"register_enabled","value":"`+in+`"}`), confirmed,
		); err == nil {
			t.Errorf("register_enabled=%q 被当成布尔值接受了 —— 错的开关值比不改更糟", in)
		}
	}
}

// TestGetSiteSettings_不回传凭据类设置 验证敏感设置不会进模型上下文。
//
// 设置表里混着支付密钥、SMTP 密码。助手把整包设置送进上下文，
// 等于把这些凭据完整交给上游模型厂商 —— 而模型的输入是会被留存的。
func TestGetSiteSettings_不回传凭据类设置(t *testing.T) {
	settings := newRecordingSettings(map[string]string{
		"site_name":        "本站",
		"register_enabled": "true",
		"smtp_password":    "smtp-secret",
		"payment_api_key":  "pay-secret",
		"jwt_secret":       "jwt-secret",
		"agent_api_key":    "agent-secret",
		"webhook_secret":   "hook-secret",
		"encrypt_key":      "enc-secret",
	})
	tools, _, _ := newConfirmTools(settings)

	out, err := callOps(t, tools, "get_site_settings", map[string]any{})
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	body, _ := json.Marshal(out)
	for _, secret := range []string{"smtp-secret", "pay-secret", "jwt-secret", "agent-secret", "hook-secret", "enc-secret"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("凭据 %q 出现在返回结果里，会随回答发往上游模型", secret)
		}
	}
	for _, want := range []string{"site_name", "register_enabled"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("正常设置 %q 应当返回，实际 %s", want, body)
		}
	}
}

// TestGetSiteSettings_指定keys时也不回传凭据 验证白名单不能被"点名"绕过。
func TestGetSiteSettings_指定keys时也不回传凭据(t *testing.T) {
	settings := newRecordingSettings(map[string]string{
		"site_name":     "本站",
		"smtp_password": "smtp-secret",
	})
	tools, _, _ := newConfirmTools(settings)

	out, err := callOps(t, tools, "get_site_settings", map[string]any{
		"keys": []string{"site_name", "smtp_password"},
	})
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	body, _ := json.Marshal(out)
	if strings.Contains(string(body), "smtp-secret") {
		t.Errorf("点名要凭据类设置时仍被返回了: %s", body)
	}
	if !strings.Contains(string(body), "site_name") {
		t.Errorf("正常设置应当返回: %s", body)
	}
}

// ── 3. 公告工具 ─────────────────────────────────────────────────

// TestPublishAnnouncement_过期早于发布被拒 验证 model 层的窗口自洽校验生效。
//
// 这种公告落库后一切正常，只是永远不显示 —— 站长会以为"发了但没人看到"，
// 然后反复检查前台、反复怀疑用户。
func TestPublishAnnouncement_过期早于发布被拒(t *testing.T) {
	tools, anns, _ := newConfirmTools(newRecordingSettings(nil))
	confirmed := &ConfirmedCall{
		ToolName: "publish_announcement",
		Params: json.RawMessage(`{"title":"维护通知","content":"今晚维护",` +
			`"publish_at":"2026-10-05 10:00","expire_at":"2026-10-05 09:00"}`),
	}
	_, err := tools.ExecuteConfirmed(
		context.Background(), model.AgentRoleOps, "publish_announcement",
		json.RawMessage(`{"title":"维护通知","content":"今晚维护",`+
			`"publish_at":"2026-10-05 10:00","expire_at":"2026-10-05 09:00"}`),
		confirmed,
	)
	if err == nil {
		t.Fatal("过期早于发布的公告被接受了")
	}
	if len(anns.created) != 0 {
		t.Errorf("被拒绝的公告仍被创建: %+v", anns.created)
	}
}

// TestPublishAnnouncement_非法语气被拒 验证 level 会真的被校验。
func TestPublishAnnouncement_非法语气被拒(t *testing.T) {
	tools, anns, _ := newConfirmTools(newRecordingSettings(nil))
	args := `{"title":"通知","content":"正文","level":"urgent"}`
	_, err := tools.ExecuteConfirmed(
		context.Background(), model.AgentRoleOps, "publish_announcement",
		json.RawMessage(args),
		&ConfirmedCall{ToolName: "publish_announcement", Params: json.RawMessage(args)},
	)
	if err == nil {
		t.Fatal("非法语气被接受了")
	}
	if len(anns.created) != 0 {
		t.Errorf("被拒绝的公告仍被创建: %+v", anns.created)
	}
}

// TestPublishAnnouncement_未确认时不创建 验证公告必须确认。
func TestPublishAnnouncement_未确认时不创建(t *testing.T) {
	tools, anns, _ := newConfirmTools(newRecordingSettings(nil))
	var got *ConfirmationRequest
	tools.Announcements = &guardedAnnouncements{inner: anns}

	// 走引擎路径才能触发确认；这里直接验证 Execute 层拒绝即可。
	_, err := callOps(t, tools, "publish_announcement", map[string]string{
		"title": "通知", "content": "正文",
	})
	if !IsConfirmRequired(err) {
		t.Fatalf("err = %v，期望 ErrConfirmRequired", err)
	}
	if len(anns.created) != 0 {
		t.Errorf("未确认就创建了公告: %+v", anns.created)
	}
	_ = got
}

// guardedAnnouncements 是一个占位包装：让上面那条用例能安全地替换仓储。
type guardedAnnouncements struct {
	model.AnnouncementRepository
	inner *recordingAnnouncements
}

func (g *guardedAnnouncements) Create(ctx context.Context, item *model.Announcement) error {
	return g.inner.Create(ctx, item)
}

// ── 4. 敏感词工具 ───────────────────────────────────────────────

// TestAddSensitiveWords_按匹配键去重 验证 "BadWord" 与 "badword" 只算一个。
//
// 模型很可能在同一个数组里同时给出两种写法，而它们在匹配器眼里是同一个词。
// 不去重的话第二条会返回"已存在"，站长会以为只加了一个。
func TestAddSensitiveWords_按匹配键去重(t *testing.T) {
	tools, _, words := newConfirmTools(newRecordingSettings(nil))
	args := `{"words":[" BadWord ","badword","坏词"]}`
	if _, err := tools.ExecuteConfirmed(
		context.Background(), model.AgentRoleOps, "add_sensitive_words",
		json.RawMessage(args),
		&ConfirmedCall{ToolName: "add_sensitive_words", Params: json.RawMessage(args)},
	); err != nil {
		t.Fatalf("新增敏感词失败: %v", err)
	}
	if len(words.added) != 2 {
		t.Errorf("应新增 2 个（大写与小写视为同一个），实际 %d 个: %+v", len(words.added), words.added)
	}
}

// TestAddSensitiveWords_已存在的词被跳过 验证重复调用不会失败。
//
// 批量导入是站长贴一大段文本的场景，其中常常混有已加过的词；
// 若整体报错，站长只能一个个删掉重来。
func TestAddSensitiveWords_已存在的词被跳过(t *testing.T) {
	tools, _, words := newConfirmTools(newRecordingSettings(nil))
	words.existing["已存在词"] = true

	args := `{"words":["新词","已存在词"]}`
	out, err := tools.ExecuteConfirmed(
		context.Background(), model.AgentRoleOps, "add_sensitive_words",
		json.RawMessage(args),
		&ConfirmedCall{ToolName: "add_sensitive_words", Params: json.RawMessage(args)},
	)
	if err != nil {
		t.Fatalf("含已存在词时应成功（跳过即可）: %v", err)
	}
	if len(words.added) != 1 {
		t.Errorf("应只新增 1 个，实际 %d", len(words.added))
	}
	body, _ := json.Marshal(out)
	if !strings.Contains(string(body), `"added":1`) {
		t.Errorf("返回结果应说明实际新增 1 个: %s", body)
	}
}

// TestAddSensitiveWords_空词条被拒 验证不会把空串写进词表。
func TestAddSensitiveWords_空词条被拒(t *testing.T) {
	tools, _, words := newConfirmTools(newRecordingSettings(nil))
	args := `{"words":["  ","","\t"]}`
	if _, err := tools.ExecuteConfirmed(
		context.Background(), model.AgentRoleOps, "add_sensitive_words",
		json.RawMessage(args),
		&ConfirmedCall{ToolName: "add_sensitive_words", Params: json.RawMessage(args)},
	); err == nil {
		t.Fatal("全是空白词条时被接受了")
	}
	if len(words.added) != 0 {
		t.Errorf("仍写入了词条: %+v", words.added)
	}
}

// ── 5. 确认单本身的规矩 ─────────────────────────────────────────

// TestOps工具_需要确认的都实现了SummarizeChange 验证每个 ConfirmAlways
// 工具都能说清"改什么"。
//
// 缺了 SummarizeChange 的确认单只显示工具名与风险提示，
// 站长点确认时完全不知道会改到谁 —— 那不是确认，是橡皮章。
func TestOps工具_需要确认的都实现了SummarizeChange(t *testing.T) {
	tools, _, _ := newConfirmTools(newRecordingSettings(nil))

	for _, tool := range ToolsForRole(model.AgentRoleOps, tools) {
		if tool.Confirm != ConfirmAlways {
			continue
		}
		if tool.SummarizeChange == nil {
			t.Errorf("工具 %q 标记为需要确认却没有 SummarizeChange —— 站长无从判断影响面", tool.Name)
		}
		if tool.ConfirmTitle == "" {
			t.Errorf("工具 %q 缺少确认单标题", tool.Name)
		}
		if tool.ConfirmRiskNote == "" {
			t.Errorf("工具 %q 缺少风险提示：站长点确认前必须知道最坏情况", tool.Name)
		}
	}
}

// TestOps工具_只读工具不要求确认 验证确认不会滥用到读操作上。
//
// 一次不必要的确认就是一次多余的点击；连续几次之后站长会闭眼点"确认"，
// 于是这个机制整体失效。
func TestOps工具_只读工具不要求确认(t *testing.T) {
	tools, _, _ := newConfirmTools(newRecordingSettings(nil))

	for _, tool := range ToolsForRole(model.AgentRoleOps, tools) {
		if !tool.Mutating {
			if tool.Confirm != ConfirmNone {
				t.Errorf("只读工具 %q 不该要求确认", tool.Name)
			}
			continue
		}
		// 写工具必须至少声明一档策略：留空（零值 ConfirmNone）虽然等价，
		// 但那说明作者没想过这个问题，意图不可见。
		if tool.Confirm != ConfirmNone && tool.Confirm != ConfirmAlways && tool.Confirm != ConfirmNever {
			t.Errorf("工具 %q 的 Confirm 取值非法: %v", tool.Name, tool.Confirm)
		}
	}
}

// TestSummarizeSetSiteSetting_给出改前改后 验证确认单展示的是真实旧值。
//
// 这条是本轮最容易被写错的地方：确认单显示的旧值若来自模型的说法
// 而不是数据库，站长点确认时看到的就不是真实状态。
func TestSummarizeSetSiteSetting_给出改前改后(t *testing.T) {
	settings := newRecordingSettings(map[string]string{
		"register_enabled": "true",
		"site_name":        "旧名",
	})
	tools, _, _ := newConfirmTools(settings)

	items, err := summarizeSetSiteSetting(
		context.Background(), tools,
		json.RawMessage(`{"key":"register_enabled","value":"false"}`),
	)
	if err != nil {
		t.Fatalf("生成确认明细失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应恰好 1 条明细，实际 %d", len(items))
	}
	item := items[0]
	if !strings.Contains(item.Value, "true") {
		t.Errorf("明细未展示当前值 true: %+v", item)
	}
	if !strings.Contains(item.After, "false") {
		t.Errorf("明细未展示将变成 false: %+v", item)
	}
	// 关闭注册这一项必须说人话，不能只回一个裸 false。
	if !strings.Contains(item.After, "已关闭") {
		t.Errorf("关闭注册应给出中文说明而非裸值: %+v", item)
	}
}

// TestSummarizeSetSiteSetting_白名单外的键直接报错 验证连确认单都生成不出来。
//
// 生成确认单的过程要读数据库；参数本身非法时不该走到那一步。
func TestSummarizeSetSiteSetting_白名单外的键直接报错(t *testing.T) {
	tools, _, _ := newConfirmTools(newRecordingSettings(nil))
	if _, err := summarizeSetSiteSetting(
		context.Background(), tools,
		json.RawMessage(`{"key":"smtp_password","value":"x"}`),
	); err == nil {
		t.Fatal("为白名单外的键生成了确认单")
	}
}

// TestPreviewWords_词多时截断并写明总数 验证确认单不会瞒着站长扩大动作。
func TestPreviewWords_词多时截断并写明总数(t *testing.T) {
	got := previewWords([]string{"a", "b", "c"})
	if got != "a、b、c" {
		t.Errorf("少量词应直接列出，实际 %q", got)
	}
	many := make([]string, 40)
	for i := range many {
		many[i] = "词" + string(rune('A'+i%26))
	}
	got = previewWords(many)
	if !strings.Contains(got, "共 40 个") {
		t.Errorf("词多时必须写明总数，实际 %q", got)
	}
	if strings.Count(got, "词") > 14 {
		t.Errorf("截断后仍列得太长: %q", got)
	}
}

// TestAnnouncementVisible_覆盖四种可见性 验证公告的可见性判断。
//
// 只回 enabled 是不够的：一条 enabled=1 但已过期的公告，
// 站长看到会以为它还挂着。
func TestAnnouncementVisible_覆盖四种可见性(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local)
	cases := []struct {
		name string
		a    model.Announcement
		want bool
	}{
		{"启用且无时间限制", model.Announcement{Enabled: true}, true},
		{"草稿", model.Announcement{Enabled: false}, false},
		{"尚未到发布时间", model.Announcement{
			Enabled: true, PublishAt: now.Add(time.Hour),
		}, false},
		{"已过期", model.Announcement{
			Enabled: true, ExpireAt: now.Add(-time.Hour),
		}, false},
		{"窗口内", model.Announcement{
			Enabled: true, PublishAt: now.Add(-time.Hour), ExpireAt: now.Add(time.Hour),
		}, true},
	}
	for _, tc := range cases {
		if got := announcementVisible(&tc.a, now); got != tc.want {
			t.Errorf("%s: visible = %v，应为 %v", tc.name, got, tc.want)
		}
	}
}

// 确认机制在 HTTP/SSE 层的端到端测试。
//
// 前面两层（internal/agent 的单元测试）验的是"引擎会不会拦住"，
// 这一层验的是"拦截之后前端能不能正确弹窗、确认后能不能真的执行"。
//
// 它覆盖三件在单元层看不见、但会直接决定功能好不好用的事：
//  1. confirm 事件能到达前端，且【只发一次】（发两次会弹两个窗）；
//  2. 停下来时【不发 done】——发了前端会把"等你确认"当成最终答案；
//  3. 带上凭证重发后，站点数据真的被改了。
//
// 之所以要用真的假上游而不是 mock 引擎：这里要验的恰恰是
// "模型提出一个写操作 → 引擎发现要确认 → handler 发事件"这条完整链路，
// 任何一环被 mock 掉，断的往往就是那一环。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// scriptedUpstream 是一个按脚本逐次返回内容的假 OpenAI 上游。
//
// 【为什么要按"对话"而不是按"请求"推进脚本】
//
//	一次"模型要求调工具 → 引擎拿结果再问一次"的往返会打上游两次，
//	而下一轮对话又会从头开始。若用全局计数器，第二次对话会接着第一次的
//	位置读脚本，于是"确认后重发"那段根本没测到工具执行 ——
//	而它恰恰是要验的部分。
//	因此这里靠"请求里有没有 tool 消息"判断是不是新对话的第一轮：
//	没有就归零。引擎回填工具结果时必然带上 tool 消息，所以这个判据是可靠的。
type scriptedUpstream struct {
	mu        sync.Mutex
	responses []string
	idx       int
	// received 记录每次请求体，用于断言模型名等。
	received []map[string]any
}

func (s *scriptedUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		s.mu.Lock()
		s.received = append(s.received, body)
		if !bodyHasToolMessage(body) {
			s.idx = 0 // 新对话的第一轮
		}
		cur := s.idx
		s.idx++
		if cur >= len(s.responses) {
			cur = len(s.responses) - 1
		}
		payload := s.responses[cur]
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}
}

// bodyHasToolMessage 判断请求里是否含 role=tool 的消息。
func bodyHasToolMessage(body map[string]any) bool {
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		if role, _ := msg["role"].(string); role == "tool" {
			return true
		}
	}
	return false
}

// toolCallResponse 构造一个"模型要求调用某工具"的响应。
func toolCallResponse(name, args string) string {
	return fmt.Sprintf(
		`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"id":"call_1","type":"function","function":{"name":%q,"arguments":%q}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		name, args)
}

// textResponse 构造一个纯文字回复。
func textResponse(content string) string {
	return fmt.Sprintf(
		`{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1}}`, content)
}

// newConfirmE2EServer 装配一个带假上游与管理员会话的服务。
func newConfirmE2EServer(t *testing.T, script []string) (*Server, *scriptedUpstream, string) {
	t.Helper()
	srv, _ := newAgentEndpointTestServer(t)
	up := &scriptedUpstream{responses: script}
	ts := httptest.NewServer(up.handler())
	t.Cleanup(ts.Close)

	createEnabledChannel(t, srv, ts.URL+"/v1", "sk-test")
	setAgentSettings(t, srv, map[string]string{
		model.SettingKeyAgentEnabled:      "true",
		model.SettingKeyAgentDefaultModel: "channel-model",
	})
	if err := srv.deps.Settings.Set(
		context.Background(), model.SettingKeySiteName, "原始站名",
	); err != nil {
		t.Fatalf("预置站点名失败: %v", err)
	}
	token := newAgentEndpointAdmin(t, srv)
	return srv, up, token
}

// parseSSEEvents 把 SSE 响应体解析成事件列表。
func parseSSEEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("SSE 事件不是合法 JSON: %v（原文 %q）", err, payload)
		}
		out = append(out, ev)
	}
	return out
}

func countEvents(events []map[string]any, typ string) int {
	n := 0
	for _, ev := range events {
		if ev["type"] == typ {
			n++
		}
	}
	return n
}

// TestAgentChat_需要确认的操作下发confirm事件且不发done 是本文件最核心的一条。
//
// 三条断言各自对应一种真实故障：
//   - 没有 confirm 事件 → 前端什么都不弹，助手说"等你确认"然后卡住；
//   - 发了 done → 前端把那句暂停说明当成最终答案，弹窗被盖过去；
//   - confirm 发了两次 → 前端弹两个叠在一起的窗，关掉一个还剩一个。
func TestAgentChat_需要确认的操作下发confirm事件且不发done(t *testing.T) {
	srv, _, token := newConfirmE2EServer(t, []string{
		toolCallResponse("set_site_setting", `{"key":"site_name","value":"被改的名字"}`),
	})

	rec := postEndpointAgentChat(t, srv, token, `{"question":"把站点名改成被改的名字"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}

	events := parseSSEEvents(t, rec.Body.String())

	if n := countEvents(events, "confirm"); n != 1 {
		t.Fatalf("confirm 事件数 = %d，期望恰好 1 次（发两次会弹两个窗）；事件序列=%v", n, eventTypes(events))
	}
	if n := countEvents(events, "done"); n != 0 {
		t.Errorf("等待确认时不该发 done，实际发了 %d 次：done 会让前端把暂停说明当成最终答案", n)
	}
	if n := countEvents(events, "error"); n != 0 {
		t.Errorf("等待确认不是错误，不该发 error 事件（实际 %d 次）", n)
	}

	// 确认单必须带足信息：工具名、标题、明细、风险提示。
	var confirm map[string]any
	for _, ev := range events {
		if ev["type"] == "confirm" {
			confirm, _ = ev["confirm"].(map[string]any)
		}
	}
	if confirm == nil {
		t.Fatal("confirm 事件里没有 confirm 负载")
	}
	if got, _ := confirm["tool_name"].(string); got != "set_site_setting" {
		t.Errorf("tool_name = %q", got)
	}
	if got, _ := confirm["title"].(string); got == "" {
		t.Error("确认单缺少标题")
	}
	if got, _ := confirm["risk_note"].(string); got == "" {
		t.Error("确认单缺少风险提示：站长点确认前必须知道最坏情况")
	}
	summary, _ := confirm["summary"].([]any)
	if len(summary) == 0 {
		t.Error("确认单缺少变更明细，站长无从判断影响面")
	}
	// params 是前端原样回传的那份，缺了它确认就无法执行。
	if _, ok := confirm["params"].(map[string]any); !ok {
		t.Error("确认单缺少 params，前端无法回传凭证")
	}

	// 【最要紧的断言】数据不能被动过。
	got, err := srv.deps.Settings.Get(context.Background(), model.SettingKeySiteName)
	if err != nil {
		t.Fatalf("读取站点名失败: %v", err)
	}
	if got != "原始站名" {
		t.Errorf("站点名 = %q，等待确认期间竟已被改动", got)
	}
}

// TestAgentChat_带凭证重发后执行写操作 验证完整闭环。
func TestAgentChat_带凭证重发后执行写操作(t *testing.T) {
	srv, _, token := newConfirmE2EServer(t, []string{
		toolCallResponse("set_site_setting", `{"key":"site_name","value":"新站名"}`),
		textResponse("站点名已经改成新站名了。"),
	})

	// 第一步：拿确认单。
	rec := postEndpointAgentChat(t, srv, token, `{"question":"把站点名改成新站名"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("第一次状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	events := parseSSEEvents(t, rec.Body.String())
	var confirm map[string]any
	for _, ev := range events {
		if ev["type"] == "confirm" {
			confirm, _ = ev["confirm"].(map[string]any)
		}
	}
	if confirm == nil {
		t.Fatalf("未拿到确认单；事件序列=%v", eventTypes(events))
	}

	// 第二步：把确认单里的 tool_name 与 params 原样带回去。
	payload := map[string]any{
		"question": "把站点名改成新站名",
		"confirm": map[string]any{
			"tool_name": confirm["tool_name"],
			"params":    confirm["params"],
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	rec2 := postEndpointAgentChat(t, srv, token, string(raw))
	if rec2.Code != http.StatusOK {
		t.Fatalf("第二次状态码 = %d；body=%s", rec2.Code, rec2.Body.String())
	}

	events2 := parseSSEEvents(t, rec2.Body.String())
	if n := countEvents(events2, "confirm"); n != 0 {
		t.Errorf("已确认却又要求确认（confirm 事件 %d 次）", n)
	}
	if n := countEvents(events2, "done"); n != 1 {
		t.Errorf("确认后应正常收尾（done 一次），实际 %d 次；序列=%v", n, eventTypes(events2))
	}
	if !strings.Contains(rec2.Body.String(), "新站名") {
		t.Errorf("最终答复里没有提到新站名；body=%s", rec2.Body.String())
	}

	// 数据必须真的改了。
	got, err := srv.deps.Settings.Get(context.Background(), model.SettingKeySiteName)
	if err != nil {
		t.Fatalf("读取站点名失败: %v", err)
	}
	if got != "新站名" {
		t.Errorf("站点名 = %q，确认后仍未执行", got)
	}
}

// TestAgentChat_凭证工具名与本次不符时被拒 验证"确认 A 执行 B"在 HTTP 层也拦得住。
func TestAgentChat_凭证工具名与本次不符时被拒(t *testing.T) {
	srv, _, token := newConfirmE2EServer(t, []string{
		toolCallResponse("set_site_setting", `{"key":"site_name","value":"不该被改"}`),
	})

	body := `{"question":"把站点名改掉","confirm":{` +
		`"tool_name":"publish_announcement",` +
		`"params":{"title":"偷偷发的公告","content":"正文"}}}`
	rec := postEndpointAgentChat(t, srv, token, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}

	events := parseSSEEvents(t, rec.Body.String())
	// 不管是走确认还是走报错，站点名都绝不能被改。
	got, err := srv.deps.Settings.Get(context.Background(), model.SettingKeySiteName)
	if err != nil {
		t.Fatalf("读取站点名失败: %v", err)
	}
	if got != "原始站名" {
		t.Errorf("站点名 = %q，工具名不匹配时竟然执行了", got)
	}
	// 工具必须被记为失败，让模型知道该重试而不是继续。
	var sawFailedTool bool
	for _, ev := range events {
		if ev["type"] == "done" {
			if result, ok := ev["result"].(map[string]any); ok {
				calls, _ := result["tool_calls"].([]any)
				for _, c := range calls {
					call, _ := c.(map[string]any)
					if ok, _ := call["ok"].(bool); !ok {
						sawFailedTool = true
					}
				}
			}
		}
	}
	if !sawFailedTool {
		t.Errorf("工具名不匹配时该工具应被记为失败；事件序列=%v", eventTypes(events))
	}
}

// TestAgentChat_只读工具不弹确认 验证查数据不会打断对话。
func TestAgentChat_只读工具不弹确认(t *testing.T) {
	srv, _, token := newConfirmE2EServer(t, []string{
		toolCallResponse("get_site_settings", `{"keys":["site_name"]}`),
		textResponse("站点名叫原始站名。"),
	})

	rec := postEndpointAgentChat(t, srv, token, `{"question":"本站叫什么"}`)
	events := parseSSEEvents(t, rec.Body.String())

	if n := countEvents(events, "confirm"); n != 0 {
		t.Errorf("只读工具不该弹确认，实际 confirm 事件 %d 次", n)
	}
	if n := countEvents(events, "done"); n != 1 {
		t.Errorf("应正常收尾，实际 done %d 次；序列=%v", n, eventTypes(events))
	}
}

// TestAgentChat_客服带凭证被拒 验证公开客服不能借确认凭证触发写操作。
//
// 客服角色本来就拿不到任何工具，这条是纵深防御：
// 若将来给 support 加了某个带确认的工具，这里是唯一的显式拦截。
// TestAgentChat_确认后告诉模型已获批准 守住一个实测踩到的坑。
//
// 确认后重发的是【同一句问题】，模型看不到任何"站长已经点头了"的线索。
// 修这个 bug 之前，站长点完确认会收到助手一句"要我发起这个改动吗？"——
// 死循环：既没改成，也没得到解释。
//
// 断言方式：检查第二次请求里是否带了"已确认"这句提示。
// 它必须排在 question 之前（buildMessages 把 question 放在最后，
// 追加进 History 会让这句刚发生的事夹在历史中间，读起来像很久以前说的）。
func TestAgentChat_确认后告诉模型已获批准(t *testing.T) {
	srv, up, token := newConfirmE2EServer(t, []string{
		toolCallResponse("set_site_setting", `{"key":"site_name","value":"新站名"}`),
		textResponse("已经改好了。"),
	})

	// 先跑一次拿到确认单。
	rec := postEndpointAgentChat(t, srv, token, `{"question":"把站点名改成新站名"}`)
	var confirm map[string]any
	for _, ev := range parseSSEEvents(t, rec.Body.String()) {
		if ev["type"] == "confirm" {
			confirm, _ = ev["confirm"].(map[string]any)
		}
	}
	if confirm == nil {
		t.Fatal("未拿到确认单")
	}

	// 带上确认单重发。
	payload, _ := json.Marshal(map[string]any{
		"question": "把站点名改成新站名",
		"confirm": map[string]any{
			"tool_name": confirm["tool_name"],
			"params":    confirm["params"],
		},
	})
	postEndpointAgentChat(t, srv, token, string(payload))

	// 找第二次对话的第一轮请求（最后一条不含 tool 消息的）。
	up.mu.Lock()
	defer up.mu.Unlock()
	var last map[string]any
	for _, body := range up.received {
		if !bodyHasToolMessage(body) {
			last = body
		}
	}
	if last == nil {
		t.Fatal("没有捕获到第二次对话的请求")
	}
	msgs, _ := last["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("请求里没有消息")
	}
	final, _ := msgs[len(msgs)-1].(map[string]any)
	text, _ := final["content"].(string)
	if !strings.Contains(text, "已经确认") {
		t.Errorf("最后一条消息 = %q，应包含『站长已确认』的提示，"+
			"否则模型会重新问一遍要不要发起，站长点确认就白点了", text)
	}
	// 站长原话必须仍在（不能被提示语替换掉）。
	if !strings.Contains(text, "把站点名改成新站名") {
		t.Errorf("站长原话丢失了：%q", text)
	}
}

func TestAgentChat_客服带凭证被拒(t *testing.T) {
	srv, keys := newAgentTestServer(t)
	// 必须先启用助手：入口会先判"助手是否启用"，未启用时返回的是
	// 503 agent_not_enabled，那样这条用例就变成了在测启用检查，
	// 根本走不到凭证拒绝那一步。
	setAgentSettings(t, srv, map[string]string{
		model.SettingKeyAgentEnabled:      "true",
		model.SettingKeyAgentDefaultModel: "channel-model",
	})
	supportKey := issueAgentKey(t, keys, model.AgentRoleSupport, "客服")

	body := `{"question":"帮我改站点名","confirm":{"tool_name":"set_site_setting",` +
		`"params":{"key":"site_name","value":"x"}}}`
	rec := postAgentChat(t, srv, "/api/agent/chat", supportKey, json.RawMessage(body))

	if rec.Code != http.StatusForbidden {
		t.Errorf("客服带确认凭证应 403，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// eventTypes 把事件序列压成类型列表，便于失败时打印。
func eventTypes(events []map[string]any) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		if typ, ok := ev["type"].(string); ok {
			out = append(out, typ)
		}
	}
	return out
}

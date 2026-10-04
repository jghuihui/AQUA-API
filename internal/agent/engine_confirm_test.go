// 引擎层的确认流程端到端测试。
//
// 与 confirm_test.go 的分工：那边验"执行入口会不会拦住"，
// 这边验"整条链路跑起来是什么样" —— 收到确认请求后本轮是否真的中断、
// 有没有产生副作用、第二次带凭证重发后是否执行并把真实结果回填给模型。
//
// 最要紧的一条是 TestAsk_待确认时不产生任何副作用：
// 如果这条退化，界面上会看到助手说"已帮你改好"而数据库纹丝不动，
// 站长却以为已经改了 —— 那比直接报错难查得多。
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// newConfirmEngine 构造一个装着站点运营仓储的 stub 引擎。
//
// 返回 settings 让调用方能断言"有没有真的动过数据"。
func newConfirmEngine(t *testing.T, script []*ChatResult) (*Engine, *recordingSettings) {
	t.Helper()
	settings := newRecordingSettings(map[string]string{
		"register_enabled": "true",
		"site_name":        "本站",
	})
	tools, _, _ := newConfirmTools(settings)
	eng, _ := newStubEngine(t, script, tools)
	eng.now = time.Now
	return eng, settings
}

// rewindStub 把 stub 的脚本游标拨回开头，模拟"新的一次 HTTP 请求"。
//
// 为什么要它：stubChat 用同一个 idx 累加，而真实的每次 Ask 都是独立请求、
// 都从第一轮开始。不重置的话，第二次 Ask 会直接读到脚本里第二轮的纯文字答复，
// 于是"确认后执行工具"这段根本没被测到 —— 而这恰恰是要验的部分。
func rewindStub(t *testing.T, eng *Engine) {
	t.Helper()
	stub, ok := eng.llm.(*stubChat)
	if !ok {
		t.Fatalf("引擎的 llm 不是 stubChat（是 %T），本测试的用法需要调整", eng.llm)
	}
	stub.idx = 0
}

// TestAsk_待确认时中断且不产生副作用 是确认机制最核心的一条。
func TestAsk_待确认时中断且不产生副作用(t *testing.T) {
	eng, settings := newConfirmEngine(t, []*ChatResult{
		toolResult("set_site_setting", `{"key":"register_enabled","value":"false"}`),
		textResult("已经把注册关掉了。"), // 不应走到这里
	})

	var gotConfirm *ConfirmationRequest
	result, err := eng.Ask(context.Background(), AskRequest{
		Role:              model.AgentRoleOps,
		Model:             "gpt-4o",
		Question:          "把注册关掉",
		OnConfirmRequired: func(req ConfirmationRequest) { gotConfirm = &req },
	})
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}

	// 确认请求必须送到。
	if gotConfirm == nil {
		t.Fatal("没有收到确认请求")
	}
	if gotConfirm.ToolName != "set_site_setting" {
		t.Errorf("确认单的工具名 = %q", gotConfirm.ToolName)
	}
	if len(gotConfirm.Summary) == 0 {
		t.Error("确认单没有变更明细：站长无从判断影响面")
	}
	if gotConfirm.Title == "" {
		t.Error("确认单缺少标题")
	}

	// 结果里必须带确认单，server 层据此发 SSE confirm 事件。
	if result.PendingConfirmation == nil {
		t.Fatal("AskResult.PendingConfirmation 为空，server 层将无法下发确认事件")
	}

	// 【最要紧的断言】数据不能被动过。
	if len(settings.writes) != 0 {
		t.Errorf("等待确认期间竟产生了写入 %v", settings.writes)
	}
	if settings.values["register_enabled"] != "true" {
		t.Errorf("register_enabled = %q，应仍为 true", settings.values["register_enabled"])
	}

	// 答复不能宣称已完成。
	if strings.Contains(result.Answer, "关掉") && !strings.Contains(result.Answer, "确认") {
		t.Errorf("答复 %q 像是已经执行完了，站长会以为改过了", result.Answer)
	}
	// 本轮就该结束，不能再去问上游（第二轮脚本准备了假答复，抓它）。
	if result.Rounds != 1 {
		t.Errorf("应在第 1 轮中断，实际第 %d 轮", result.Rounds)
	}
}

// TestAsk_确认后重发执行并回填真实结果 验证完整的"问 → 确认 → 做完"闭环。
func TestAsk_确认后重发执行并回填真实结果(t *testing.T) {
	eng, settings := newConfirmEngine(t, []*ChatResult{
		toolResult("set_site_setting", `{"key":"register_enabled","value":"false"}`),
		textResult("注册已经关掉了，新用户将无法注册。"),
	})

	// 第一次：停在确认。
	result, err := eng.Ask(context.Background(), AskRequest{
		Role:              model.AgentRoleOps,
		Model:             "gpt-4o",
		Question:          "把注册关掉",
		OnConfirmRequired: func(ConfirmationRequest) {},
	})
	if err != nil {
		t.Fatalf("第一次 Ask 失败: %v", err)
	}
	if result.PendingConfirmation == nil {
		t.Fatal("第一次未停在确认")
	}
	if len(settings.writes) != 0 {
		t.Fatalf("第一次就产生了写入 %v", settings.writes)
	}

	// 第二次：带上凭证重发同一句问题。
	rewindStub(t, eng)
	confirmed := &ConfirmedCall{
		ToolName: "set_site_setting",
		Params:   json.RawMessage(`{"key":"register_enabled","value":"false"}`),
	}
	result2, err := eng.Ask(context.Background(), AskRequest{
		Role:              model.AgentRoleOps,
		Model:             "gpt-4o",
		Question:          "把注册关掉",
		Confirmed:         confirmed,
		OnConfirmRequired: func(ConfirmationRequest) { t.Error("已确认却又在要求确认") },
	})
	if err != nil {
		t.Fatalf("确认后 Ask 失败: %v", err)
	}

	if result2.PendingConfirmation != nil {
		t.Error("确认后仍在等待确认")
	}
	if len(settings.writes) != 1 || settings.writes[0] != "register_enabled=false" {
		t.Errorf("写入 = %v，应恰好一次 register_enabled=false", settings.writes)
	}
	if !strings.Contains(result2.Answer, "注册已经关掉") {
		t.Errorf("最终答复 = %q", result2.Answer)
	}
	// 工具必须被记为成功，界面上那个"set_site_setting"标签不该是红的。
	if len(result2.ToolCalls) != 1 {
		t.Fatalf("应记录 1 次工具调用，实际 %d", len(result2.ToolCalls))
	}
	if !result2.ToolCalls[0].OK {
		t.Errorf("确认后的工具执行应成功: %s", result2.ToolCalls[0].Error)
	}
}

// TestAsk_未挂确认通道时高危工具不执行 验证 server 层漏接线时的后果。
//
// 这是"部署升级后忘接 OnConfirmRequired"那类问题的守门测试。
// 表现必须是【工具报错】而不是【悄悄改掉数据】。
func TestAsk_未挂确认通道时高危工具不执行(t *testing.T) {
	eng, settings := newConfirmEngine(t, []*ChatResult{
		toolResult("set_site_setting", `{"key":"register_enabled","value":"false"}`),
		textResult("好的，已经关掉了。"),
	})

	result, err := eng.Ask(context.Background(), AskRequest{
		Role:     model.AgentRoleOps,
		Model:    "gpt-4o",
		Question: "把注册关掉",
		// 刻意不挂 OnConfirmRequired
	})
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if len(settings.writes) != 0 {
		t.Fatalf("没有确认通道却执行了高危写操作: %v", settings.writes)
	}
	if result.PendingConfirmation != nil {
		t.Error("没有确认通道时不该产出确认单")
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("应记录 1 次工具调用，实际 %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].OK {
		t.Error("工具被记为成功，但它本该失败")
	}
	if !strings.Contains(result.ToolCalls[0].Error, "确认") {
		t.Errorf("错误信息 = %q，应指出缺确认通道", result.ToolCalls[0].Error)
	}
}

// TestAsk_只读工具不触发确认 验证查数据不会被弹窗打断。
//
// 一次不必要的确认就是一次多余的点击；连续几次之后站长会闭眼点确认。
func TestAsk_只读工具不触发确认(t *testing.T) {
	eng, _ := newConfirmEngine(t, []*ChatResult{
		toolResult("get_site_settings", `{"keys":["site_name"]}`),
		textResult("站点名叫「本站」。"),
	})

	result, err := eng.Ask(context.Background(), AskRequest{
		Role:              model.AgentRoleOps,
		Model:             "gpt-4o",
		Question:          "本站叫什么",
		OnConfirmRequired: func(ConfirmationRequest) { t.Error("只读工具触发了确认") },
	})
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if result.PendingConfirmation != nil {
		t.Error("只读工具不该产出确认单")
	}
	if result.Rounds != 2 {
		t.Errorf("应正常走完 2 轮，实际 %d", result.Rounds)
	}
}

// TestAsk_连续两轮对话的待确认状态不残留 验证引擎是共享实例时的隔离。
//
// Engine 通过 sync.Once 派生后在进程内长期共享；若 pendingConfirm 不在
// 每次 Ask 开头重置，站长上一轮点过确认的操作会让【下一轮无关对话】
// 直接被判定为"待确认"，表现为新问题刚问出口就弹窗。
func TestAsk_连续两轮对话的待确认状态不残留(t *testing.T) {
	eng, _ := newConfirmEngine(t, []*ChatResult{
		toolResult("set_site_setting", `{"key":"register_enabled","value":"false"}`),
		textResult("好的。"),
	})

	// 第一轮：产生确认请求。
	if _, err := eng.Ask(context.Background(), AskRequest{
		Role:              model.AgentRoleOps,
		Model:             "gpt-4o",
		Question:          "把注册关掉",
		OnConfirmRequired: func(ConfirmationRequest) {},
	}); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}

	// 第二轮：一个纯只读问题，不该看到上一轮的确认单。
	result, err := eng.Ask(context.Background(), AskRequest{
		Role:              model.AgentRoleOps,
		Model:             "gpt-4o",
		Question:          "本站叫什么",
		OnConfirmRequired: func(ConfirmationRequest) { t.Error("上一轮的确认状态泄漏到了新一轮") },
	})
	if err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	if result.PendingConfirmation != nil {
		t.Error("新一轮 Ask 复用了上一轮遗留的确认单")
	}
}

// TestAsk_一次只问一个操作 验证同一轮里多个高危工具只弹一次窗。
//
// 两个确认单会互相覆盖，站长只能看到最后一个，
// 于是"确认了一个、另一个悄悄也执行了"这种错觉就有了土壤。
func TestAsk_一次只问一个操作(t *testing.T) {
	eng, settings := newConfirmEngine(t, []*ChatResult{
		toolResult("set_site_setting", `{"key":"register_enabled","value":"false"}`),
	})

	var count int
	_, err := eng.Ask(context.Background(), AskRequest{
		Role:     model.AgentRoleOps,
		Model:    "gpt-4o",
		Question: "把注册关掉",
		OnConfirmRequired: func(ConfirmationRequest) {
			count++
		},
	})
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if count != 1 {
		t.Errorf("一次确认请求应只回调 1 次，实际 %d 次", count)
	}
	if len(settings.writes) != 0 {
		t.Errorf("产生了写入 %v", settings.writes)
	}
}

// TestAsk_确认单展示的是数据库真实旧值 验证 SummarizeChange 读的是库而不是模型说法。
//
// 这条直接对应"确认单上写着当前 100、实际执行按 200 算"这类事故：
// 站长是靠确认单做判断的，它一旦失准，整个机制就是橡皮章。
func TestAsk_确认单展示的是数据库真实旧值(t *testing.T) {
	eng, _ := newConfirmEngine(t, []*ChatResult{
		toolResult("set_site_setting", `{"key":"register_enabled","value":"false"}`),
	})

	var got *ConfirmationRequest
	if _, err := eng.Ask(context.Background(), AskRequest{
		Role:              model.AgentRoleOps,
		Model:             "gpt-4o",
		Question:          "把注册关掉",
		OnConfirmRequired: func(req ConfirmationRequest) { got = &req },
	}); err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if got == nil {
		t.Fatal("未收到确认单")
	}
	// 库里是 true；模型只说了"改成 false"，没有说当前值。
	// 因此确认单上的"当前值"只可能来自数据库。
	var found bool
	for _, item := range got.Summary {
		if strings.Contains(item.Value, "true") && strings.Contains(item.After, "false") {
			found = true
		}
	}
	if !found {
		t.Errorf("确认单未展示「当前 true → 将变 false」: %+v", got.Summary)
	}
}

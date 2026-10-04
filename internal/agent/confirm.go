// AI Agent 的写操作确认机制：把"该不该问站长"从模型手里拿走。
//
// 意图（Why）：
//
//	给 agent 加了删渠道、封用户、改额度这类能力之后，出现了一个新问题：
//	【谁来决定某次操作要不要先问站长】
//
//	如果让模型自己判断（"这个操作挺重要的，我先问一下"），后果是：
//	  - 模型漏判 → 一次误封就发生在站长眼前，且不可撤回；
//	  - 模型过度问 → 每改个权重都要点一次确认，助手变得比后台菜单还难用。
//	更糟的是这两种错误都不稳定：同样的操作在不同的对话里判定不同，
//	而站长无法预判"我问它一句，它会不会就把事做了"。
//
//	所以本文件把风险等级变成**工具的静态属性**：
//	每个写工具在构造时声明 Confirm 策略，Execute 时按策略执行。
//	模型无法影响这个判定 —— 它甚至看不到这个字段。
//
// 三档策略（关键取舍）：
//
//	ConfirmNone   直接执行。用于可逆、影响面小、后果明确的动作
//	              （改权重、启停渠道、停用令牌）。
//	ConfirmAlways 必须弹窗。用于【不可逆或影响用户权益】的动作
//	              （封禁用户、改额度、删除渠道、替换密钥）。
//	              注意"删除"归在这一档而不是靠"可逆"归到第一档：
//	              删掉的东西即便能从备份恢复，恢复过程本身也是事故。
//	ConfirmNever  永不执行（预留给将来的"只读运维 agent"角色）。
//
// 为什么用 SSE 而不是 HTTP 往返：
//
//	一次对话可能连续产生多个工具调用（查列表 → 改状态 → 再查）。
//	若每次高危操作都走一次独立的 HTTP 请求，token 与上下文管理会
//	在多处重复。走同一条事件流，由 server 层把"待确认"事件
//	交给前端，站长点确认后再通过一个专门的接口执行 ——
//	模型的主循环不中断，但真正动手之前必然经过人眼。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// ConfirmPolicy 是一个工具的写操作确认策略。
type ConfirmPolicy int

const (
	// ConfirmNone 直接执行，不问站长。
	//
	// 判据（三条全满足）：① 可逆且代价小（再改回来即可）；
	// ② 影响面局限于单个对象，不会波及其他用户；
	// ③ 站长自己就能预见后果。
	//
	// 举例：改渠道权重、启停渠道、停用一把令牌。
	// 反例（属于 ConfirmAlways）：封禁用户会让人无法登录，
	// 改额度会直接改变 someone's 账务。
	ConfirmNone ConfirmPolicy = iota

	// ConfirmAlways 必须由站长点击确认后才执行。
	//
	// 判据（满足任一）：不可逆、涉及资金/权益、影响多个对象、
	// 或者"站长可能不知道自己做这件事"。
	ConfirmAlways

	// ConfirmNever 永不执行。调用即报错。
	//
	// 存在的意义是给"只读运维 agent"留出位置：那个角色复用同一批工具，
	// 但把策略全部降级为 ConfirmNever，于是它在结构上不可能写任何东西，
	// 而不需要为它单独维护一份工具清单。
	ConfirmNever
)

// String 返回中文策略名，供日志与前端展示。
func (p ConfirmPolicy) String() string {
	switch p {
	case ConfirmNone:
		return "无需确认"
	case ConfirmAlways:
		return "需要确认"
	case ConfirmNever:
		return "禁止执行"
	default:
		return "未知策略"
	}
}

// ConfirmationRequest 是"请站长确认这次操作"的载荷。
//
// 字段刻意做成给【前端直接渲染】的形状（而不是让前端解析自然语言）：
// 站长的判断依据必须是他自己填进去的那些值，而不是模型复述的话。
// 模型可以提议，但不能篡改这些值 —— 它们来自工具的参数与数据库。
type ConfirmationRequest struct {
	// ToolName 是要执行的工具名。
	ToolName string `json:"tool_name"`

	// Title 是一句话说明（给确认弹窗的标题）。
	Title string `json:"title"`

	// Summary 是操作摘要，逐条列出"会改什么"。
	// 不给自然语言描述：模型生成的那句话可能听起来无害，
	// 而站长真正要判断的是下面这些结构化的值。
	Summary []ConfirmationItem `json:"summary"`

	// RiskNote 是一句话风险提示（例："被封禁的用户将无法登录，且不会收到通知"）。
	RiskNote string `json:"risk_note"`

	// Params 是原始参数，回填给执行接口。
	//
	// 【为什么原样回传而不让前端改】
	//	前端只负责"是/否"，参数由前端回传才能保证执行的是【被展示过的那一份】。
	//	若让前端重新组装参数，就可能出现"弹窗显示停用 3 号、执行时传了 5 号"。
	//	前端必须把这份 Params 原样送回，不解析也不修改。
	Params json.RawMessage `json:"params"`
}

// ConfirmationItem 是确认清单里的一行。
type ConfirmationItem struct {
	// Label 是这一项的名字（例："目标渠道"）。
	Label string `json:"label"`
	// Value 是它的当前值。
	Value string `json:"value"`
	// After 是它【将变成】什么。空串表示不变。
	After string `json:"after,omitempty"`
}

// ErrConfirmRequired 表示该工具需要确认但尚未获得确认。
//
// Execute 在 ConfirmAlways 工具未被确认时返回它，
// server 层据此发一条 confirmation 事件而不是 error 事件 ——
// 对模型而言这是"请等一下"，不是"失败了"。
var ErrConfirmRequired = ConfirmRequiredError{}

type ConfirmRequiredError struct{}

func (ConfirmRequiredError) Error() string {
	return "该操作需要站长确认后才能执行"
}

// IsConfirmRequired 判断 err 是否为"待确认"信号。
func IsConfirmRequired(err error) bool {
	_, ok := err.(ConfirmRequiredError)
	return ok
}

// ConfirmedCall 是一次已获站长确认、可以执行的写操作。
//
// 【为什么用独立类型而不是两个方法】
//
//	Execute 的签名是固定的（server 层按 role + name 调用），
//	而"是否已确认"是这个签名之外的信息。用一个显式类型承接它，
//	编译器就会盯着所有需要确认的调用点 —— 少传一个就是编译不过，
//	而不是"运行时发现它默默按未确认处理了"。
type ConfirmedCall struct {
	// ToolName 与工具名一致，防止确认了 A 却执行 B。
	ToolName string
	// Params 是被【展示给站长看过】的那份参数。
	Params json.RawMessage
}

// executeWithConfirmation 按确认策略处理一次写工具调用。
//
// 三个分支的行为：
//   - ConfirmNever  → 立刻报错，不问也不执行（结构性禁止）；
//   - ConfirmAlways 且未确认 → 返回 ErrConfirmRequired，server 层发确认事件；
//   - 其余（已确认 / ConfirmNone）→ 真正执行。
//
// 为什么 ConfirmAlways 在未确认时【不执行但也不算错误】：
// 模型的主循环会把错误信息回填给模型，模型通常会据此改用别的说法重问；
// 而"请等站长确认"这种情况模型无法自行推进，需要前端介入。
// 所以 server 层识别这个信号后发专门事件，模型这一轮就此结束。
func executeWithConfirmation(
	ctx context.Context,
	tool Tool,
	args json.RawMessage,
	confirmed *ConfirmedCall,
) (any, error) {
	switch tool.Confirm {
	case ConfirmNever:
		return nil, fmt.Errorf("工具 %s 被配置为禁止执行（只读模式）", tool.Name)

	case ConfirmAlways:
		// 确认过：必须校验工具名一致，否则"确认 A 执行 B"就成立了。
		// 这不是理论风险 —— 前端把上一次响应的 Params 存下来、
		// 期间站长又触发了另一个工具，是很容易发生的操作序列。
		if confirmed != nil {
			if confirmed.ToolName != tool.Name {
				return nil, fmt.Errorf(
					"确认的工具是 %s，与要执行的 %s 不一致，请重新发起",
					confirmed.ToolName, tool.Name)
			}
			// 用被展示过的那份参数，不是本次新解析的：
			// 站长看到的是前者，执行的必须是前者。
			return tool.Handler(ctx, confirmed.Params)
		}
		return nil, ErrConfirmRequired

	default:
		// ConfirmNone：直接执行本次参数。
		// 即使带了 confirmed 也忽略它 —— 站长确认了某次操作不代表
		// 此后所有同名工具调用都获得了授权，那会让一次点确认变成长期授权。
		return tool.Handler(ctx, args)
	}
}

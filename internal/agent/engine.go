// AI Agent 的对话引擎：把"用户提问"变成"带工具调用的多轮回答"。
//
// 意图（Why）：
//
//	站长问"我的渠道怎么了"，模型需要先查数据、再基于数据回答。
//	这就是工具调用循环：请求 → 模型要调工具 → 执行工具 → 把结果回填 → 再请求，
//	直到模型给出纯文字答复或达到轮次上限。
//
// 为什么要限制轮次（maxToolRounds）：
//
//	模型可能反复请求同一个工具（参数填错、被回填的结果看不懂），
//	没有上限时一次对话能烧掉几十次上游调用——按量计费的站点上这是真金白银。
//	上限到了就返回"我需要更多信息"之类的收尾，而不是继续烧钱。
//
// 流转（Flow）：
//
//	Engine.Ask(ctx, req)
//	  → 组装 messages（系统提示词 + 历史 + 本轮提问）
//	  → loop：
//	      llm.ChatOnce
//	      ├─ 无 tool_calls → 返回文字，结束
//	      └─ 有 tool_calls → 逐个执行（tools.Execute）→ 结果以 role=tool 回填 → 继续
//	  → 达到 maxToolRounds → 收尾
//
// 安全边界：
//
//	工具执行走 agent.ToolsForRole（与下发给模型的工具同一判定点），
//	客服角色拿到的工具列表是空的，循环里一次都不会进。
//	写操作在【执行前】不额外拦：能改数据的只有 ops，
//	而 ops 本来就是站长本人——这里再加一道"要不要弹确认"的问题留给前端，
//	后端只负责把"这是写操作"这个事实如实传给前端（见 ToolCall 的 Mutating）。
//
// 扩展（Extend）：
//
//	加"工具结果缓存"：在执行前按 (tool, args) 查近期相同调用。
//	一次对话里模型重复问同一个渠道是常见的，省一次上游往返有实际收益。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

const (
	// maxToolRounds 是单次对话内"请求 → 工具 → 回填"的最多轮数。
	//
	// 取 8：足够覆盖"查渠道 → 查探针 → 查日志 → 回答"这类三到四步的排障链，
	// 又能在模型陷入循环时止损。每轮至少一次上游请求，
	// 8 轮就是 8 倍于预期的成本，值得设上限。
	maxToolRounds = 8

	// maxToolArgsBytes 是工具参数的长度上限。
	//
	// 模型的 arguments 是 JSON 文本，一个正常的 list_channels 参数只有几十字节。
	// 超过 64KB 基本可以断定是模型跑飞了（重复粘贴全文之类），
	// 而把它原样交给工具层会先撑爆 JSON 解析。
	maxToolArgsBytes = 64 << 10

	// maxToolResultBytes 是单个工具结果回填给模型时的长度上限。
	//
	// 超过 1MB 的结果（异常上游返回巨量文本时）会挤掉模型真正的推理空间，
	// 而且它本来就该被截断——工具层已经对失败原因做了截断，
	// 这里兜住的是"某个工具不小心返回了巨量数据"的情况。
	maxToolResultBytes = 1 << 20
)

// AskRequest 是一次 agent 对话请求。
type AskRequest struct {
	// Role 决定可用工具与提示词底稿。空时按 ops 处理（站长本人用）。
	Role model.AgentRole
	// Model 是要调用的模型名；由站长在后台配置，不来自用户输入。
	Model string
	// SystemPrompt 是该角色的系统提示词（站长可在后台改）。
	SystemPrompt string
	// History 是此前的对话（不含本轮提问），按时间正序。
	History []ChatMessage
	// Question 是本轮用户提问。
	Question string
	// OnDelta 是逐段回调（用于流式输出）。
	//
	// 为 nil 时引擎只在结束时返回完整答案（同步模式）。
	// 设计成回调而不是返回 channel：调用方（HTTP handler）需要控制
	// 何时写响应头、失败时怎么降级，这些都不该由引擎决定。
	OnDelta func(text string)
	// OnToolCall 在执行每个工具前调用（用于向前端展示"正在查什么"）。
	OnToolCall func(name string, mutating bool)

	// Confirmed 是本次对话已获站长确认的写操作。
	//
	// 一次对话只允许一个待确认操作：模型在等确认时无法继续推进，
	// 所以它不可能在同一轮里发起第二个需要确认的工具。
	// 单个槽位足够。
	Confirmed *ConfirmedCall

	// OnConfirmRequired 在工具需要站长确认时调用。
	//
	// 为 nil 时需要确认的工具一律【拒绝执行】并把原因回填给模型 ——
	// 绝不能因为"没人来确认"就默认放行，那会让确认机制形同虚设。
	OnConfirmRequired func(req ConfirmationRequest)
}

// AskResult 是一次对话的结果。
type AskResult struct {
	// Answer 是最终答复（面向用户的文字）。
	Answer string
	// ToolCalls 是本轮实际执行过的工具（供前端展示过程与审计）。
	ToolCalls []ExecutedTool
	// Rounds 是实际用掉的轮数。
	Rounds int
	// Truncated 为真表示撞上了轮次上限，答复可能不完整。
	//
	// 前端应当据此提示"我还需要更多信息"，而不是把截断的答复当成完整答案。
	Truncated bool
	// PromptTokens / CompletionTokens 是累计用量。
	PromptTokens     int
	CompletionTokens int

	// PendingConfirmation 非空表示本轮对话【停在等待站长确认】，
	// 没有任何副作用已经发生。
	//
	// 为什么要单独一个字段而不是塞进 ToolCalls 的错误里：
	// 调用方要据此决定"发弹窗"还是"发报错"，
	// 而这两件事在 SSE 里是不同的消息类型。
	// 塞进错误文本的话，server 层就得靠字符串匹配来分辨 ——
	// 那种判定的表现是"提示词一改，弹窗就再也不出现了"。
	//
	// 【有它就一定没有执行】
	//	这是硬保证：引擎在生成确认单之后立刻返回，
	//	不会在站长点头之前碰任何写路径。
	PendingConfirmation *ConfirmationRequest
}

// ExecutedTool 记录一次工具执行的结果。
type ExecutedTool struct {
	Name string
	// Arguments 是模型给的参数原文（截断后）。
	Arguments string
	// Mutating 标记这是写操作。
	Mutating bool
	// OK 为 false 时 Result 是给模型看的失败原因。
	OK bool
	// Result 是回填给模型的 JSON 结果（截断后）。
	Result string
	// Error 面向站长的人类可读失败原因（前端展示用，不给模型）。
	Error string
}

// chatCaller 抽象"向模型发一次请求"这一个动作。
//
// 为什么抽接口而不是直接依赖 *LLMClient：
//
//	工具循环是本包状态最复杂的部分（消息累积、tool_call_id 回填、轮次上限），
//	而它要验证的东西全在"发出去的消息对不对"，跟上游是谁无关。
//	绑定具体类型就只能起真实 HTTP 服务器来测——慢、不稳，还要先造渠道和密钥，
//	最后测的其实是网络而不是循环逻辑。
type chatCaller interface {
	ChatOnce(ctx context.Context, modelName string, messages []ChatMessage, tools []Tool) (*ChatResult, error)
}

// Engine 是 agent 对话引擎。
type Engine struct {
	llm   chatCaller
	tools *AgentTools
	// now 可注入，便于测试超时刻意触发的情况。
	now func() time.Time

	// pendingConfirm 记录本次对话中【第一个】待确认的操作。
	//
	// 【为什么是 Engine 的字段而不是返回值一路往上传】
	//	executeOne 是深埋在工具循环里的，它无法直接改外层的
	//	AskResult（那是一次 Ask 独有的值，而 executeOne 的签名里没有它）。
	//	而把确认单一路作为返回值传上去，需要把 executeOne 拆成
	//	"执行"与"判定"两步，代码会明显变啰嗦。
	//
	// 【为什么用"第一个"而不是"最后一个"】
	//	一个模型回复里可能带多个 tool_calls。若其中两个都要确认，
	//	我们只能先问第一个 —— 站长确认后重新提问，届时模型会再次
	//	提出它的全部调用，第二轮才会问到第二个。
	//	覆盖式赋值会丢掉先前的确认单，导致站长看到的是第二个操作
	//	而第一个被静默跳过，那比"慢一轮"危险得多。
	//
	//	每次 Ask 开头必须重置（见 Ask 内部），否则上一次对话的
	//	待确认项会泄漏到下一次 —— 那表现为"刚问完就弹了个旧弹窗"。
	pendingConfirm *ConfirmationRequest
}

// NewEngine 构造对话引擎。
//
// llm 为 nil 时留空的接口值而不是塞一个 nil 指针进去：
// 接口里装着类型化 nil 指针时 `e.llm == nil` 判定为假，
// 错误会延后到真正发请求时才以空指针崩溃现场报出来，丢掉"没配置客户端"这个真因。
func NewEngine(llm *LLMClient, tools *AgentTools) *Engine {
	e := &Engine{tools: tools, now: time.Now}
	if llm != nil {
		e.llm = llm
	}
	return e
}

// WithCaller 返回一个"本次对话用指定调用者"的引擎副本。
//
// 【为什么需要它，而不是把上游配置放进 AskRequest】
//
// Engine 是 sync.Once 缓存的（见 server/agent_wiring.go），而站长随时可以
// 在后台改独立上游配置。若把配置读在 NewEngine 里，改完配置就得重启服务才生效——
// "我明明保存了却没生效"是最难排查的一类问题。
//
// 但也不能塞进 AskRequest：那会让每次组装请求都多带一份与对话内容无关的字段，
// 更糟的是它暗示"不同轮次可以换上游"，而实际上整个对话过程必须用同一个上游
// （中途换上游会导致工具调用后模型换了、上下文对不上）。
//
// 所以做成"以某个调用者派生一个临时引擎"：原引擎不变（仍然可并发使用），
// 本次对话用派生出来的那一个。
func (e *Engine) WithCaller(llm *LLMClient) *Engine {
	clone := *e
	clone.llm = llm
	return &clone
}

// Ask 执行一次完整对话（含工具循环）。
func (e *Engine) Ask(ctx context.Context, req AskRequest) (*AskResult, error) {
	// 每次对话开始前清掉上一次遗留的待确认项。
	// 引擎是【共享】实例（server 侧 sync.Once 缓存），不清就会串味。
	e.pendingConfirm = nil

	if e.llm == nil {
		return nil, errors.New("agent: 引擎未配置上游客户端")
	}
	question := strings.TrimSpace(req.Question)
	if question == "" {
		return nil, errors.New("agent: 问题不能为空")
	}

	role := req.Role
	if role == "" {
		role = model.AgentRoleOps
	}

	// 工具列表只在这里取一次，且与执行时用同一个判定点。
	tools := ToolsForRole(role, e.tools)

	messages := buildMessages(req, question)
	result := &AskResult{}

	for round := 1; round <= maxToolRounds; round++ {
		result.Rounds = round

		resp, err := e.llm.ChatOnce(ctx, req.Model, messages, tools)
		if err != nil {
			// 已经跑过至少一轮才失败时，把已完成的部分作为答复返回，
			// 而不是丢弃全部：用户至少能看到"我查到了这些"。
			if len(result.ToolCalls) > 0 && resp == nil {
				result.Answer = fallbackAnswer(result, role)
				result.Truncated = true
				return result, nil
			}
			return nil, err
		}
		result.PromptTokens += resp.PromptTokens
		result.CompletionTokens += resp.CompletionTokens

		if !resp.HasToolCalls() {
			// 纯文字答复 = 循环结束。
			result.Answer = resp.Content
			if result.Answer == "" {
				result.Answer = fallbackAnswer(result, role)
			}
			if req.OnDelta != nil && resp.Content != "" {
				req.OnDelta(resp.Content)
			}
			return result, nil
		}

		// 有工具调用：先记进消息历史，再执行。
		// 顺序不能反——assistant 消息必须紧跟在它之前的 user 消息后，
		// 否则上游会因"tool 消息前面没有对应的 tool_call"而 400。
		assistantMsg := ChatMessage{
			Role:      RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		messages = append(messages, assistantMsg)

		for _, call := range resp.ToolCalls {
			executed := e.executeOne(ctx, role, call, &req)
			result.ToolCalls = append(result.ToolCalls, executed)

			// 【发现待确认：立刻停止本轮】
			//
			// 继续循环等于把"尚未执行"当成"结果"回填给模型，
			// 模型会据此编出"已经把用户封了"这种回答 ——
			// 而实际上什么都没发生。那比停下来问更糟：
			// 站长看到一句成功的汇报，却什么都没改。
			if e.pendingConfirm != nil {
				result.PendingConfirmation = e.pendingConfirm
				result.Answer = "该操作需要你确认后才能执行，" +
					"请在上方确认或取消；确认后我会继续处理其余步骤。"
				return result, nil
			}

			// 每个工具结果都必须回填，且带 tool_call_id ——
			// 缺了它模型无法把结果与请求对上（见 ChatMessage.ToolCallID 注释）。
			messages = append(messages, ChatMessage{
				Role:       RoleTool,
				Content:    executed.Result,
				ToolCallID: call.ID,
				Name:       call.Func.Name,
			})
		}
	}

	// 撞上轮次上限：收尾而不是继续烧钱。
	result.Answer = fallbackAnswer(result, role)
	result.Truncated = true
	return result, nil
}

// buildMessages 组装发给上游的消息序列。
//
// 顺序刻意固定为「系统 → 历史 → 本轮提问」：
// 系统的位置会影响它的权重（越靠前越权威），
// 放在历史之后会让模型把"系统提示词"当成一条普通的对话内容。
//
// 不接收 role：提示词底稿在调用方（HTTP 层）就已按角色取好，
// 这里再收一个角色只会让人以为它参与了什么判断，实际却什么也没做。
func buildMessages(req AskRequest, question string) []ChatMessage {
	messages := make([]ChatMessage, 0, len(req.History)+2)

	if prompt := strings.TrimSpace(req.SystemPrompt); prompt != "" {
		messages = append(messages, ChatMessage{Role: RoleSystem, Content: prompt})
	}
	// 历史里若已有 system 消息，跳过：以本轮 SystemPrompt 为准，
	// 否则两条互相矛盾的 system 提示会让模型行为不可预测。
	for _, msg := range req.History {
		if msg.Role == RoleSystem {
			continue
		}
		messages = append(messages, msg)
	}
	messages = append(messages, ChatMessage{Role: RoleUser, Content: question})
	return messages
}

// executeOne 执行一个工具调用，返回执行结果记录。
//
// 这里是"agent 唯一会碰站内数据的地方"，所以每一步都做校验：
// 参数长度 → 工具是否存在 → 执行 → 结果截断。
func (e *Engine) executeOne(
	ctx context.Context,
	role model.AgentRole,
	call ToolCall,
	ask *AskRequest,
) ExecutedTool {
	rec := ExecutedTool{
		Name:      call.Func.Name,
		Arguments: truncateText(call.Func.Arguments, 500),
	}

	// 参数长度先判：异常参数（如模型把整个对话粘进 arguments）
	// 直接进 JSON 解析会先撑爆内存，而它本来就不该被解析。
	var args json.RawMessage
	if len(call.Func.Arguments) > maxToolArgsBytes {
		rec.OK = false
		rec.Result = toolErrorResult("工具参数过长，已拒绝执行（请用更简短的参数重试）")
		rec.Error = "工具参数过长"
		return rec
	}
	if trimmed := strings.TrimSpace(call.Func.Arguments); trimmed != "" {
		// 参数不是合法 JSON 时也要回填一条可读的原因：
		// 模型看到"参数格式错误"会改格式重试；看到空字符串则可能反复重发同样的错误参数。
		if !json.Valid([]byte(trimmed)) {
			rec.OK = false
			rec.Result = toolErrorResult("工具参数不是合法的 JSON，请检查参数格式后重试")
			rec.Error = "工具参数不是合法 JSON"
			return rec
		}
		args = json.RawMessage(trimmed)
	}

	// 查工具定义（拿 Mutating 标记），执行走 Tools.Execute。
	tool, found := findTool(e.tools, role, call.Func.Name)
	if !found {
		// 区分两种"找不到"：依赖没接进来 vs 模型幻觉出的工具名。
		// 前者是部署问题（该报错让站长看日志），后者是模型问题（该让模型改问）。
		// 混为一谈会让部署问题表现为"模型老是幻觉"，极难定位。
		if e.tools == nil {
			rec.OK = false
			rec.Result = toolErrorResult("本站未配置 agent 工具，无法执行查询")
			rec.Error = "agent 工具未配置"
			return rec
		}
		rec.OK = false
		rec.Result = toolErrorResult(fmt.Sprintf("没有名为 %s 的工具，请只使用已提供的工具", call.Func.Name))
		rec.Error = "未知工具"
		return rec
	}
	rec.Mutating = tool.Mutating
	if ask != nil && ask.OnToolCall != nil {
		ask.OnToolCall(tool.Name, tool.Mutating)
	}

	// 【确认优先于执行】
	//
	// 顺序不能颠倒：先执行再问等于已经改了，"确认"沦为形式。
	// 因此这里在真正调用 Handler 之前先问一次。
	//
	// 为什么用"将要用的参数"而不是模型新解析的参数来生成确认单：
	// 站长看到并点确认的就是那一份，之后执行的也必须是那一份。
	// 中间任何一次重新解析都可能产生不同的值。
	callArgs := args
	if tool.Confirm == ConfirmAlways {
		useArgs, blocked, err := e.resolveConfirmation(ctx, ask, tool, args)
		if err != nil {
			rec.OK = false
			rec.Error = truncateText(err.Error(), 200)
			rec.Result = toolErrorResult(err.Error())
			return rec
		}
		if blocked {
			// 已发出确认请求，等站长点击。本轮到此为止 ——
			// 引擎不能在没有结果的情况下继续循环（它手里没有工具结果可回填）。
			rec.OK = false
			rec.Error = "等待站长确认"
			rec.Result = toolErrorResult("该操作已提交给站长确认，确认后才会执行")
			return rec
		}
		callArgs = useArgs
	}

	// ask 为 nil 只可能来自内部误用（Ask 总会传 &req），
	// 但工具执行是本文件里唯一会改数据的地方，
	// 在那里用一个可能 panic 的解引用把"内部 bug"变成"服务崩溃"不划算。
	var confirmed *ConfirmedCall
	if ask != nil {
		confirmed = ask.Confirmed
	}
	out, err := e.tools.ExecuteConfirmed(ctx, role, tool.Name, callArgs, confirmed)
	if err != nil {
		rec.OK = false
		// 错误文本同时回填给模型：让它能根据"渠道不存在，请先查列表"
		// 这类信息改正下一次调用。
		rec.Error = truncateText(err.Error(), 200)
		rec.Result = toolErrorResult(err.Error())
		return rec
	}

	encoded, mErr := json.Marshal(out)
	if mErr != nil {
		rec.OK = false
		rec.Error = "工具结果序列化失败"
		rec.Result = toolErrorResult("工具结果无法序列化")
		return rec
	}
	rec.OK = true
	rec.Result = truncateText(string(encoded), maxToolResultBytes)
	return rec
}

// resolveConfirmation 决定一次需要确认的写操作是"生成确认单"还是"直接执行"。
//
// 三个返回值：
//   - useArgs：真正要用于执行的参数（已确认时是被展示过的那一份）；
//   - blocked：为 true 表示"已提交确认请求，请等站长"，本轮就此中断；
//   - err：参数本身不合法（如引用了不存在的用户），连确认单都不该生成。
//
// 【为什么要在这里生成确认单，而不是让前端自己拼】
//
//	确认单要展示"现在是什么、会变成什么"，这必须读数据库。
//	前端拿不到仓储，若让它自己拼，它就只能展示模型给的参数 ——
//	而模型的参数正是待验证的东西。拿被验证者的说法当证据，
//	确认单就毫无意义。
//
// 【为什么 blocked 时不把错误回填给模型继续循环】
//
//	模型手里没有"操作已完成"的结果，若继续问下去它只会反复重试同一个调用。
//	正确做法是让本轮结束、前端弹窗；站长点确认后前端用新的一次
//	请求（携带 Confirmed）重跑，那时模型才能拿到真实结果继续回答。
func (e *Engine) resolveConfirmation(
	ctx context.Context,
	req *AskRequest,
	tool Tool,
	args json.RawMessage,
) (useArgs json.RawMessage, blocked bool, err error) {
	// 已确认：核对工具名一致后直接放行。
	//
	// 一致性校验不能省：前端可能把上一次响应的确认单带到这次请求里
	// （期间站长又触发了别的工具），那会导致"确认 A 执行 B"。
	if req.Confirmed != nil {
		if req.Confirmed.ToolName != tool.Name {
			return nil, false, fmt.Errorf(
				"站长确认的是 %s，与本次要执行的 %s 不一致，请重新发起操作",
				req.Confirmed.ToolName, tool.Name)
		}
		return req.Confirmed.Params, false, nil
	}

	// 没确认，且没人接收确认请求 → 拒绝执行。
	//
	// 这一分支是整个机制的地基：绝不能因为"没找到确认通道"
	// 就当成"不需要确认"。那会让 OnConfirmRequired 未挂载的部署
	// （例如自建二进制的旧配置）静默获得无限制的写权限。
	if req.OnConfirmRequired == nil {
		return nil, false, fmt.Errorf(
			"%s 需要站长确认，但本次请求没有确认通道，已拒绝执行", tool.Name)
	}

	confirmReq, err := e.buildConfirmation(ctx, req, tool, args)
	if err != nil {
		return nil, false, err
	}
	// 记到引擎上供主循环检测。
	// 只记第一个：见 Engine.pendingConfirm 的注释（一次只问一个，
	// 否则模型一次请求里两个待确认操作会互相覆盖）。
	if e.pendingConfirm == nil {
		e.pendingConfirm = confirmReq
	}
	req.OnConfirmRequired(*confirmReq)
	return nil, true, nil
}

// buildConfirmation 生成确认单。
//
// 刻意【不】在这里执行任何写操作：确认单生成阶段只读。
// 这是"确认之前绝不能动数据"的实现方式 ——
// 生成确认单与执行确认是两个不同的函数，物理上不可能重叠。
func (e *Engine) buildConfirmation(
	ctx context.Context,
	req *AskRequest,
	tool Tool,
	args json.RawMessage,
) (*ConfirmationRequest, error) {
	confirmReq := &ConfirmationRequest{
		ToolName: tool.Name,
		Title:    tool.ConfirmTitle,
		// Params 原样回传：前端必须原样送回，不能解析也不能改。
		Params: args,
	}
	if confirmReq.Title == "" {
		confirmReq.Title = "确认执行该操作"
	}
	confirmReq.RiskNote = tool.ConfirmRiskNote

	// 让工具自己补充"改什么"的明细。
	//
	// 之所以不把明细硬编码在引擎里：各工具要读的当前值完全不同
	// （用户额度、令牌状态、渠道权重），只有工具自己知道该查哪张表。
	// 引擎若去猜，就得维护一张"工具名 → 该读哪个仓储"的表 ——
	// 那张表一旦与工具不同步，确认单就会显示错的旧值，
	// 而站长恰恰靠它做判断。
	if tool.SummarizeChange == nil {
		// 没有 SummarizeChange 的工具【不该】配 ConfirmAlways：
		// 那样站长只能看到"确认执行某个操作"而不知道到底改什么，
		// 那样的确认单是有害的 —— 它会训练站长盲点"确认"。
		// 因此这里直接拒绝，而不是发一张空确认单。
		return nil, fmt.Errorf(
			"工具 %s 未提供确认信息（SummarizeChange 为 nil），"+
				"无法安全地请求确认，已拒绝执行", tool.Name)
	}
	summary, err := tool.SummarizeChange(ctx, e.tools, args)
	if err != nil {
		return nil, fmt.Errorf("无法生成该操作的确认信息：%w", err)
	}
	confirmReq.Summary = summary
	return confirmReq, nil
}

// findTool 在该角色的白名单里找工具。
//
// 依赖显式传入而不是从包级取：工具定义是由 AgentTools 生成的，
// 拿一个 nil 依赖去查会得到"一个工具都没有"的错误结果，
// 而那正是客服角色的正常形态——用包级 nil 依赖会把两种情况混成一种。
func findTool(deps *AgentTools, role model.AgentRole, name string) (Tool, bool) {
	for _, tool := range ToolsForRole(role, deps) {
		if tool.Name == name {
			return tool, true
		}
	}
	return Tool{}, false
}

// toolErrorResult 把失败原因包成工具结果的标准形状。
//
// 必须是 JSON 对象：上游要求 tool 消息的 content 是合法的 JSON 文本，
// 塞纯文本会让部分上游直接 400。
func toolErrorResult(msg string) string {
	payload, err := json.Marshal(map[string]any{
		"ok":    false,
		"error": truncateText(msg, 300),
	})
	if err != nil {
		return `{"ok":false,"error":"内部错误"}`
	}
	return string(payload)
}

// fallbackAnswer 是在"没能拿到模型答复"时给用户的一句话。
//
// 为什么要给兜底而不是返回错误：用户问的是"我的渠道怎么了"，
// 让他看到一句"我查到了这些信息但没能整理成回答"，
// 比看到一个 HTTP 500 有用得多——至少他知道 agent 跑过、查到过东西。
func fallbackAnswer(result *AskResult, role model.AgentRole) string {
	var sb strings.Builder
	if role == model.AgentRoleSupport {
		sb.WriteString("这个问题我暂时回答不了。")
		if len(result.ToolCalls) > 0 {
			sb.WriteString("（我查到了一些信息，但没能整理成完整的回答，请换个说法再问一次。）")
		} else {
			sb.WriteString("你可以换个说法再问一次，或者联系站点管理员。")
		}
		return sb.String()
	}

	if len(result.ToolCalls) == 0 {
		return "我没能理解这个问题。你可以问得更具体一些，比如「我配了哪些渠道」「最近的调用成功率怎么样」「3 号渠道为什么失败」。"
	}
	sb.WriteString("我查到了以下信息，但工具调用次数已达上限，没能给出完整结论：\n")
	for _, t := range result.ToolCalls {
		status := "成功"
		if !t.OK {
			status = "失败：" + t.Error
		}
		verb := "查询"
		if t.Mutating {
			verb = "修改"
		}
		sb.WriteString(fmt.Sprintf("- %s（%s）：%s\n", t.Name, verb, status))
	}
	sb.WriteString("\n建议把问题拆小一点再问，例如先问单个渠道的状态，再问它的测活记录。")
	return sb.String()
}

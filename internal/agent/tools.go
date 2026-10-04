// AI Agent 的工具集：模型可以调用、但只能在白名单内执行的站内运维动作。
//
// 意图（Why）：
//
//	站长遇到"渠道为什么不转发""这个报错什么意思""价格配错了"时，
//	唯一能查的地方是数据库或 SSH 里的日志。把这些动作做成 agent 工具，
//	他就能用一句话问出来，而不必碰服务器。
//
// 安全边界（本文件最重要的部分）：
//
//  1. 只有 ops 角色拿得到这里的工具。support 角色在 toolsForRole 里
//     拿到空列表——客服面向公网输入，一旦能读站内数据，
//     提示注入就能把渠道配置、用户邮箱、订单号套出来。
//
//  2. **不提供任意 SQL / 任意 HTTP / 任意 shell**。这三个是"万能钥匙"，
//     一旦给出去，"agent 能改代码、能读环境变量、能连到内网"就都是它的推论。
//     工具必须是【站长能预见结果的窄动作】，比如"把 3 号渠道停用"，
//     而不是"执行任意 UPDATE"。
//
//  3. 写操作全部要求显式 ID + 明确状态值，不接受"停用那个出问题的渠道"这类
//     模糊指令——模型会自己去找 ID，而它可能找错。找不到唯一目标时必须报错
//     让模型改问，而不是"挑一个最像的"。
//
//  4. 每个工具的返回都是**脱敏后的结构化数据**，不含密钥明文。
//     渠道密钥只在 relay 内部流转，绝不进工具返回值——模型看到的东西会
//     完整发到上游，也可能被展示在前端。
//
// 为什么工具是【方法】而不是包级变量表：
//
//	处理器需要访问注入的仓储（AgentTools.Channels 等），而包级变量的闭包里
//	拿不到实例。第一版写成包级 var 时这个问题被掩盖了（编译不过才暴露），
//	但更重要的是：静态工具表会诱使后来者以为"工具不依赖任何数据源"，
//	从而写出直接开数据库连接的处理器——那会让 agent 绕开所有仓储约束。
//	现在 toolsForRole 是方法，依赖关系写在类型上，编译器会盯着。
//
// 流转（Flow）：
//
//	模型请求 tool_call → server 层解析 → tools.Execute
//	  → 按 role 取白名单（toolsForRole）
//	  → 校验参数（结构体反序列化 + 显式校验）
//	  → 执行（只访问注入的仓储，不碰文件与网络）
//	  → JSON 序列化回填给模型
//
// 扩展（Extend）：
//
//	加一个工具：在 toolsForRole 里加一个构造调用（见 toolListChannels），
//	处理器写成 AgentTools 的方法。需要新数据源时先在 AgentTools 里加字段，
//	不要在处理器里现开数据库连接。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// Tool 是单个可调用工具的定义。
//
// 为什么 Description 要写给模型看而不是站长看：模型是依据这段描述来决定
// "要不要用这个工具、怎么填参数"的。描述含糊 → 模型乱填参数 → 工具报错。
// 因此描述里必须写清【什么情况下用】【参数含义】【返回什么】。
type Tool struct {
	// Name 是模型看到的工具名，必须符合 OpenAI function name 规则
	// （字母数字下划线，≤64 字符）。改这个名字会让线上已配置的提示词失效。
	Name string
	// Description 面向模型的能力说明。
	Description string
	// Parameters 是参数的 JSON Schema（OpenAI function calling 格式）。
	Parameters json.RawMessage
	// Handler 是实际执行体。args 是按 Parameters 反序列化后的原始字节。
	Handler func(ctx context.Context, args json.RawMessage) (any, error)
	// Mutating 标记这是写操作。
	//
	// 用途有二：① 后台展示时提示站长"这个 agent 能改你的数据"；
	// ② 将来若加"只读运维 agent"角色，可按此跳过全部写操作。
	// 漏标一个写操作的后果是"站长以为客服是只读的，它却改了渠道状态"。
	Mutating bool

	// Confirm 是该工具的写操作确认策略（见 confirm.go）。
	//
	// 【为什么不靠 Mutating 推导】Mutating 只回答"是不是写操作"，
	// 答不了"要不要先问站长"。而后者才是权限扩容后的关键问题：
	// 改权重与封用户都是写操作，前者该直接做、后者必须先确认。
	// 混在一个布尔里就没法表达这层差别。
	//
	// 【为什么不让模型决定】模型判断会不稳定（同样的操作不同对话里
	// 判定不同），而且漏判的代价是站长眼前的误伤。
	// 把它变成工具的静态属性后，模型看不到也改不了这个判定。
	Confirm ConfirmPolicy

	// ConfirmTitle 与 ConfirmRiskNote 是高危工具的展示文案。
	//
	// 写死在代码里而不是让工具调用时传：它们要出现在【站长看的那张确认单】上，
	// 而确认单不能由提议操作的一方来措辞 —— 那等于让被告写判决词。
	ConfirmTitle    string
	ConfirmRiskNote string

	// SummarizeChange 生成确认单上"改什么"的明细（读当前值，与改前改后对比）。
	//
	// 【为什么必须由工具自己提供，而不是引擎统一生成】
	//	引擎要知道"改前是什么"就得去读对应的表，而各工具的目标表完全不同
	//	（用户额度、令牌状态、渠道权重）。让引擎去猜就得维护一张
	//	"工具名 → 该读哪个仓储"的映射表，那张表一旦与工具不同步，
	//	确认单就会显示错误的旧值 —— 而站长恰恰是靠它做判断的。
	//	错显示一次"改前 100 元"，就可能换来一次真实的误操作。
	//
	// 只读方法，绝不写库 —— 这保证了"生成确认单"与"执行确认"物理隔离。
	// 为 nil 时确认单只显示工具名与风险提示（不给明细），
	// 那比给错明细安全，但站长会失去判断依据，
	// 因此每个 ConfirmAlways 的工具都必须实现它。
	// ctx 是调用方的上下文：站长关掉页面时它会被取消，
	// 确认单生成阶段同样要尊重它 —— 否则一次已放弃的确认请求
	// 仍会继续查库，而在移动网络下那可能持续好几秒。
	SummarizeChange func(
		ctx context.Context,
		deps *AgentTools,
		args json.RawMessage,
	) ([]ConfirmationItem, error)
}

// AgentTools 是执行工具所需的依赖集合。
//
// 全部为接口而非具体类型：单元测试可以塞内存实现，
// 也能让 agent 只拿到它真正需要的那几个仓储（减少误用面）。
type AgentTools struct {
	Channels  model.ChannelRepository
	ProbeLogs model.ChannelProbeLogRepository
	UsageLogs model.UsageLogRepository
	Users     model.UserRepository
	Orders    model.PaymentOrderRepository
	Settings  model.SettingRepository

	// Tokens 是令牌仓储。用于令牌管理类工具（列出/启停/改额度）。
	//
	// 刻意与 ChannelKeys 分开：两者都是"凭据"，但令牌能【直接调模型接口】，
	// 泄露后果比渠道密钥更重。把它们放在相邻的两个字段而不是一个
	// "凭据"抽象里，是为了让"谁动了凭据"在代码上一目了然。
	Tokens      model.TokenRepository
	ChannelKeys model.ChannelKeyRepository

	// Announcements 与 SensitiveWords 用于站点运营类工具（发公告、管敏感词）。
	//
	// 与上面两个"凭据"仓储并列而不是收进某个"运营"分组：
	// 公告与敏感词是内容审核面的数据，泄露后果与凭据不同
	// （误伤用户对话 / 提前泄露维护公告），混在一组里会让人不再逐个审视。
	Announcements  model.AnnouncementRepository
	SensitiveWords model.SensitiveWordRepository

	// Now 可注入，便于测试"最近 24 小时"这类相对时间查询。
	// 为 nil 时用 time.Now。
	Now func() time.Time
}

func (t *AgentTools) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// 工具执行错误：给模型看的消息必须说明"怎么改"，而不只是"错了"。
var (
	// ErrNoTools 表示该角色一个工具都拿不到（客服）。
	ErrNoTools = errors.New("agent: 当前角色未启用任何工具")
	// ErrUnknownTool 表示模型请求了白名单外的工具名。
	ErrUnknownTool = errors.New("agent: 未知工具")
)

// ToolsForRole 返回该角色可用的工具定义（供 server 层下发给模型）。
//
// 这是**授权的唯一判定点**。执行路径（Execute）也走它，
// 因此"下发给模型的工具"与"能执行的工具"永远一致——
// 分成两处判的话，改了一处忘了另一处就是越权。
// ToolsForRole 返回该角色可用的工具集。
//
// 这是【整个 agent 授权体系的唯一决策点】。
// 无论请求从哪个入口进来、用什么鉴权方式，
// 最终调用哪个工具集合都由这里的 role 分支决定。
//
// 三条不可动摇的规则：
//
//  1. 未知角色返回 nil 而非兜底到某个角色。
//     拼错的角色若被默认成 ops，等于凭空开出一个带工具的入口。
//
//  2. 每个 case 的工具集必须与它的鉴权方式匹配。
//     ops 要求管理员会话（见 router 的 admin 组）；
//     self_service 要求 ctx 里有登录用户（见 tools_selfservice.go）；
//     support 在公网用密钥，无法确定身份，因此零工具。
//
//  3. 加角色时必须在下面加一个独立 case，
//     并在这里的注释里写清"它拿什么身份进来"。只写工具不给身份来源，
//     是这类系统最常见也最危险的一种含糊。
func ToolsForRole(role model.AgentRole, deps *AgentTools) []Tool {
	if deps == nil {
		return nil
	}
	switch role {
	case model.AgentRoleOps:
		// 身份来源：管理员会话（RequireAdmin）。
		// 工具里包含改状态/改额度/删渠道等写操作，
		// 因此额外受 UAC 提权约束（见 elevation.go）。
		return opsTools(deps)
	case model.AgentRoleSelfService:
		// 身份来源：登录用户的会话，经 WithSelfUser 写入 ctx。
		// 工具集刻意只有"读自己的"与"创建自己的 key"，
		// 且没有任何一个工具接受 user_id 参数 —— 越权在签名上不可表达。
		return selfServiceTools(deps)
	case model.AgentRoleSupport:
		// 身份来源：一把 agent 密钥，没有对应的站内用户。
		// 刻意为空：客服不碰站内数据。
		// 这不是"以后再说"，而是安全边界——加了它就得重新评估提示注入风险。
		return nil
	default:
		return nil
	}
}

// Execute 按名称执行一个工具。
//
// role 决定可用工具集：support 拿到空列表，因此对它而言任何调用都返回
// ErrNoTools——这是"客服碰不到站内数据"的唯一保证点。
func (t *AgentTools) Execute(ctx context.Context, role model.AgentRole, name string, args json.RawMessage) (any, error) {
	return t.ExecuteConfirmed(ctx, role, name, args, nil)
}

// ExecuteConfirmed 是带确认信息的执行入口。
//
// 【为什么不需要确认时仍走同一个函数】
//
//	绝大多数工具是 ConfirmNone，理论上可以保留两个函数各走各的。
//
// 但那样"执行写操作"就有两条路径，将来加确认逻辑时很容易只改一条，
// 于是某个工具悄悄绕过了确认。单一入口从结构上排除这种可能。
//
// confirmed 为 nil 表示"尚未获得站长确认"。此时 ConfirmAlways 的工具
// 会返回 ErrConfirmRequired（见 confirm.go），server 据此发确认事件。
func (t *AgentTools) ExecuteConfirmed(
	ctx context.Context,
	role model.AgentRole,
	name string,
	args json.RawMessage,
	confirmed *ConfirmedCall,
) (any, error) {
	allowed := ToolsForRole(role, t)
	if len(allowed) == 0 {
		return nil, ErrNoTools
	}
	for _, tool := range allowed {
		if tool.Name != name {
			continue
		}
		// args 为空时补空对象：模型对无参工具偶尔会省略 arguments，
		// 直接反序列化到结构体会报 "unexpected end of JSON input"，
		// 而这不是模型的错。
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		if !tool.Mutating {
			// 只读工具不接受确认参数。
			// 传了也要忽略而不是执行：前端可能把上次的确认单误带到这次调用，
			// 而"只读工具的参数"与"确认过的写操作参数"语义完全不同，
			// 混用会让一次确认的作用范围悄悄扩大。
			return tool.Handler(ctx, args)
		}
		return executeWithConfirmation(ctx, tool, args, confirmed)
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownTool, name)
}

// agentToolMaxRows 是列表类工具返回行数的硬上限。
//
// 为什么必须有：模型可能因为某种原因反复调用同一个列表工具，
// 没有上限时它能把整张表拉进自己的上下文，既费 token 又可能把敏感数据带出去。
const agentToolMaxRows = 50

// opsTools 返回运维角色的全部工具（本站唯一的带工具角色）。
//
// 【顺序即站长的排查动线，不要随意打乱】
//
//	先看有什么（渠道 / 令牌 / 用户）→ 再看状态（探针 / 日志 / 统计）
//	→ 最后动手（改状态 / 改额度 / 封禁）。
//	模型是按工具在列表里的顺序倾向去选的，
//	把"查看"排在"修改"前面能降低它一上来就写库的概率。
func opsTools(t *AgentTools) []Tool {
	return []Tool{
		// ── 只读：现状 ──
		t.listChannelsTool(),
		t.listChannelKeysTool(),
		t.listTokensTool(),
		t.listUsersTool(),
		t.listOrdersTool(),
		t.listAnnouncementsTool(),
		t.listSensitiveWordsTool(),
		t.getSiteSettingsTool(),
		// ── 只读：诊断 ──
		t.listChannelProbesTool(),
		t.listUsageLogsTool(),
		t.summarizeUsageTool(),
		t.getUserDetailTool(),
		t.getChannelHealthTool(),
		// ── 写：可逆的小改动（无需确认）──
		// 这一档的共同判据是"错了能立刻改回来，且在改回来的路上没人会受损"：
		// 停用渠道/令牌会被探针立刻发现，权重调错下次调用就纠正。
		t.setChannelStatusTool(),
		t.setChannelRoutingTool(),
		t.setTokenStatusTool(),
		// ── 写：需要站长点头 ──
		// 这一档的共同判据是"受害者不在现场"：
		// 被封的用户、被改额度的令牌、看到公告的全站用户，
		// 都不会在操作发生的这一刻收到通知。
		t.adjustTokenQuotaTool(),
		t.setUserStatusTool(),
		t.adjustUserQuotaTool(),
		t.publishAnnouncementTool(),
		t.addSensitiveWordsTool(),
		t.setSiteSettingTool(),
	}
}

// ── 工具 1：列出渠道 ────────────────────────────────────────────

func (t *AgentTools) listChannelsTool() Tool {
	return Tool{
		Name: "list_channels",
		Description: "列出本站的所有上游渠道及其状态。当用户问「我配了哪些渠道」「渠道有哪些」时使用。" +
			"返回字段：id（渠道编号，改状态时要用）、name（名称）、type（类型）、status（1 启用 / 2 停用 / 3 自动停用）、" +
			"models（该渠道允许的模型列表，空表示全部）、healthy（最近一次测活是否通过）、latency_ms（最近延迟毫秒）。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"only_enabled": {
			"type": "boolean",
			"description": "只返回启用中的渠道。省略则返回全部。"
		}
	}
}`),
		Handler: t.handleListChannels,
	}
}

func (t *AgentTools) handleListChannels(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		OnlyEnabled bool `json:"only_enabled"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if t.Channels == nil {
		return nil, errors.New("本站未接入渠道数据，无法查询")
	}

	q := model.ChannelQuery{Limit: agentToolMaxRows}
	if p.OnlyEnabled {
		status := model.ChannelStatusEnabled
		q.Status = &status
	}
	list, err := t.Channels.List(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("读取渠道失败: %w", err)
	}

	// 只回模型判断"是否可用"所需的字段。
	// 渠道的 APIKey 在此被完全丢弃——模型看到的东西会发往上游。
	type channelBrief struct {
		ID         uint64   `json:"id"`
		Name       string   `json:"name"`
		Type       string   `json:"type"`
		Status     int      `json:"status"`
		StatusText string   `json:"status_text"`
		ModelCount int      `json:"model_count"`
		Models     []string `json:"models"`
		Healthy    bool     `json:"healthy"`
		LatencyMS  int      `json:"latency_ms"`
	}
	out := make([]channelBrief, 0, len(list))
	for _, c := range list {
		brief := channelBrief{
			ID:         c.ID,
			Name:       c.Name,
			Type:       c.TypeKey,
			Status:     int(c.Status),
			StatusText: channelStatusText(c.Status),
			Models:     c.Models,
			ModelCount: len(c.Models),
			LatencyMS:  c.LatencyMS,
		}
		// 健康判定与后台看板一致：测过 + 通过 + 有延迟。
		// 刻意不算"状态启用"——启用但从没测过的渠道是危险的，不是健康的。
		// LastTestAt 也要看：三天前测过一次通过，不代表现在还通。
		// （看板卡片会同时显示测量时间，正是为了让管理员判断这个数字是否还新鲜。）
		brief.Healthy = c.LastTestOK && c.LatencyMS > 0 && !c.LastTestAt.IsZero()
		out = append(out, brief)
	}
	return out, nil
}

// channelStatusText 是渠道状态 → 中文（供模型直接转述给站长）。
func channelStatusText(s model.ChannelStatus) string {
	switch s {
	case model.ChannelStatusEnabled:
		return "启用"
	case model.ChannelStatusDisabled:
		return "停用"
	case model.ChannelStatusAutoDisabled:
		return "自动停用（上游连续失败被系统关闭）"
	default:
		return "未知"
	}
}

// ── 工具 2：启用 / 停用渠道（写）─────────────────────────────────

// 描述里强调"必须先 list_channels 确认 ID"，因为模型很爱自己猜 ID，
// 而猜错的后果是停用了不该停的渠道。
func (t *AgentTools) setChannelStatusTool() Tool {
	return Tool{
		Name: "set_channel_status",
		Description: "启用或停用某个渠道。停用后该渠道不再参与请求路由，调用会转到其他渠道或直接失败。" +
			"【执行前必须先用 list_channels 确认渠道 ID 与名称，不要凭猜测】。" +
			"参数：channel_id（渠道编号，来自 list_channels）、enabled（true 启用 / false 停用）。" +
			"返回：是否真的改了、该渠道的名称与新状态。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"channel_id": {
			"type": "integer",
			"description": "渠道 ID，必须来自 list_channels 的返回值。"
		},
		"enabled": {
			"type": "boolean",
			"description": "true 启用该渠道，false 停用该渠道。"
		}
	},
	"required": ["channel_id", "enabled"]
}`),
		Mutating: true,
		// 启停渠道直接执行：它可逆（再点一次就回来了）、
		// 只影响一个渠道、且站长一眼能看出后果。
		// 这类操作加确认只会把助手用成比后台菜单还麻烦的表单。
		Confirm:      ConfirmNone,
		ConfirmTitle: "切换渠道启用状态",
		Handler:      t.handleSetChannelStatus,
	}
}

func (t *AgentTools) handleSetChannelStatus(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		ChannelID uint64 `json:"channel_id"`
		// 用指针区分"没传"与"传了 false"——这是本文件最要紧的一处细节。
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	// 缺参数时明确报错让模型重问，而不是当 false 处理——
	// "没传 enabled" 被当成"停用"是这类接口最危险的默认值。
	if p.Enabled == nil {
		return nil, errors.New("缺少参数 enabled（true 启用 / false 停用），请明确指定")
	}
	if p.ChannelID == 0 {
		return nil, errors.New("缺少参数 channel_id，请先调用 list_channels 确认渠道编号")
	}
	if t.Channels == nil {
		return nil, errors.New("本站未接入渠道数据，无法执行该操作")
	}

	ch, err := t.Channels.GetByID(ctx, p.ChannelID)
	if err != nil {
		if errors.Is(err, model.ErrChannelNotFound) {
			// 报"不存在"而不是"已停用/已启用"：不泄露其他渠道的状态，
			// 也让模型知道该重新查列表而不是换个 ID 再试。
			return nil, fmt.Errorf("渠道 %d 不存在，请调用 list_channels 确认编号", p.ChannelID)
		}
		return nil, fmt.Errorf("读取渠道失败: %w", err)
	}

	want := model.ChannelStatusDisabled
	if *p.Enabled {
		want = model.ChannelStatusEnabled
	}
	// 已是目标状态时不发 UPDATE：避免无谓写库触发 updated_at 变化，
	// 让"我什么都没改"这件事在审计日志里也说得清。
	if ch.Status == want {
		return map[string]any{
			"changed":     false,
			"channel_id":  ch.ID,
			"channel":     ch.Name,
			"status_text": channelStatusText(ch.Status),
			"note":        "该渠道已处于目标状态，未做修改",
		}, nil
	}

	ch.Status = want
	if err := t.Channels.Update(ctx, ch); err != nil {
		return nil, fmt.Errorf("更新渠道状态失败: %w", err)
	}
	return map[string]any{
		"changed":     true,
		"channel_id":  ch.ID,
		"channel":     ch.Name,
		"status_text": channelStatusText(want),
	}, nil
}

// ── 工具 3：查渠道探针历史 ──────────────────────────────────────

func (t *AgentTools) listChannelProbesTool() Tool {
	return Tool{
		Name: "list_channel_probes",
		Description: "查询某个渠道最近的测活记录（时间、是否通过、延迟毫秒、上游状态码、失败原因）。" +
			"当用户问「这个渠道最近怎么样」「为什么老是失败」时用它。" +
			"参数：channel_id（渠道编号）、limit（返回条数，1-50，默认 10，越大越早）。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"channel_id": {
			"type": "integer",
			"description": "渠道 ID，来自 list_channels。"
		},
		"limit": {
			"type": "integer",
			"description": "返回条数，1-50，默认 10。",
			"minimum": 1,
			"maximum": 50
		}
	},
	"required": ["channel_id"]
}`),
		Handler: t.handleListChannelProbes,
	}
}

func (t *AgentTools) handleListChannelProbes(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		ChannelID uint64 `json:"channel_id"`
		Limit     int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if p.ChannelID == 0 {
		return nil, errors.New("缺少参数 channel_id")
	}
	// 探针历史仓储是可选依赖：没注入时明确说"查不了"，
	// 而不是静默返回空列表让模型以为"最近没失败过"。
	if t.ProbeLogs == nil {
		return nil, errors.New("本站未启用探针历史记录，无法查询历史测活数据")
	}

	logs, err := t.ProbeLogs.List(ctx, model.ChannelProbeLogQuery{
		ChannelID: p.ChannelID,
		Limit:     clampToolLimit(p.Limit, 10, 50),
	})
	if err != nil {
		return nil, fmt.Errorf("读取探针历史失败: %w", err)
	}
	if len(logs) == 0 {
		return map[string]any{
			"channel_id": p.ChannelID,
			"probes":     []any{},
			"note":       "没有查到任何测活记录（可能尚未巡检到该渠道）",
		}, nil
	}

	type probeBrief struct {
		At         string `json:"at"`
		OK         bool   `json:"ok"`
		LatencyMS  int    `json:"latency_ms"`
		StatusCode int    `json:"status_code"`
		Message    string `json:"message,omitempty"`
	}
	out := make([]probeBrief, 0, len(logs))
	for _, l := range logs {
		brief := probeBrief{
			At:         l.At.Format("2006-01-02 15:04:05"),
			OK:         l.OK,
			LatencyMS:  l.LatencyMS,
			StatusCode: l.StatusCode,
		}
		// 失败原因截断：上游常回整页 HTML，
		// 全文回填给模型既占上下文又可能带出内部信息。
		if l.Message != "" {
			brief.Message = truncateText(l.Message, 200)
		}
		out = append(out, brief)
	}
	return map[string]any{"channel_id": p.ChannelID, "probes": out}, nil
}

// ── 工具 4：查调用日志 ──────────────────────────────────────────

func (t *AgentTools) listUsageLogsTool() Tool {
	return Tool{
		Name: "list_usage_logs",
		Description: "查询最近的调用日志（谁、什么模型、哪个渠道、成功还是失败、耗时、token 数、失败原因）。" +
			"当用户问「最近有没有报错」「为什么调用失败」「谁在用」时用它。" +
			"参数：channel_id（只看该渠道，可选）、user_id（只看该用户，可选）、" +
			"failed_only（只看失败的，可选）、hours（看最近多少小时内，可选，默认 24）、" +
			"limit（返回条数，1-50，默认 20）。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"channel_id": { "type": "integer", "description": "只看该渠道的记录。" },
		"user_id":    { "type": "integer", "description": "只看该用户的记录。" },
		"failed_only": { "type": "boolean", "description": "只看失败的调用。" },
		"hours": {
			"type": "integer",
			"description": "看最近多少小时内，1-720，默认 24。",
			"minimum": 1,
			"maximum": 720
		},
		"limit": {
			"type": "integer",
			"description": "返回条数，1-50，默认 20。",
			"minimum": 1,
			"maximum": 50
		}
	}
}`),
		Handler: t.handleListUsageLogs,
	}
}

func (t *AgentTools) handleListUsageLogs(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		ChannelID  uint64 `json:"channel_id"`
		UserID     uint64 `json:"user_id"`
		FailedOnly bool   `json:"failed_only"`
		Hours      int    `json:"hours"`
		Limit      int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if t.UsageLogs == nil {
		return nil, errors.New("本站未接入调用日志，无法查询")
	}

	since := t.now().Add(-time.Duration(clampToolLimit(p.Hours, 24, 720)) * time.Hour)
	q := model.UsageLogQuery{
		Since: &since,
		Limit: clampToolLimit(p.Limit, 20, 50),
	}
	if p.ChannelID > 0 {
		id := p.ChannelID
		q.ChannelID = &id
	}
	if p.UserID > 0 {
		id := p.UserID
		q.UserID = &id
	}
	if p.FailedOnly {
		q.Status = model.LogStatusError
	}

	logs, err := t.UsageLogs.List(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("读取调用日志失败: %w", err)
	}
	if len(logs) == 0 {
		return map[string]any{
			"logs": []any{},
			"note": "该条件下没有调用记录（可能是时间范围内没人调用，或过滤条件太窄）",
		}, nil
	}

	type logBrief struct {
		At         string `json:"at"`
		UserID     uint64 `json:"user_id"`
		ChannelID  uint64 `json:"channel_id"`
		Model      string `json:"model"`
		OK         bool   `json:"ok"`
		StatusCode int    `json:"status_code"`
		LatencyMS  int    `json:"latency_ms"`
		Tokens     int    `json:"tokens"`
		Error      string `json:"error,omitempty"`
	}
	out := make([]logBrief, 0, len(logs))
	for _, l := range logs {
		brief := logBrief{
			At:         l.CreatedAt.Format("2006-01-02 15:04:05"),
			UserID:     l.UserID,
			ChannelID:  l.ChannelID,
			Model:      l.Model,
			OK:         usageLogSucceeded(l),
			StatusCode: l.StatusCode,
			LatencyMS:  l.LatencyMS,
			Tokens:     l.TotalTokens,
		}
		if l.Error != "" {
			brief.Error = truncateText(l.Error, 200)
		}
		out = append(out, brief)
	}
	return map[string]any{"count": len(out), "logs": out}, nil
}

// usageLogSucceeded 判断一条调用日志是否成功。
//
// 口径必须与 UsageLogQuery.Status 的过滤条件【完全一致】
// （见 store/usage_log_repo.go：status_code 落在 200–299 即视为成功）。
// 两者一旦不同，模型会看到"汇总说成功 10 条、逐条列出来只有 8 条"的自相矛盾，
// 而这类矛盾会让它对整个工具层失去信任、进而改用猜测回答站长。
func usageLogSucceeded(l *model.UsageLog) bool {
	return l.StatusCode >= 200 && l.StatusCode < 300
}

// ── 工具 5：用量汇总 ────────────────────────────────────────────
func (t *AgentTools) summarizeUsageTool() Tool {
	return Tool{
		Name: "summarize_usage",
		Description: "统计最近一段时间的调用总量、成功率与 token 消耗。当用户问「今天用了多少」" +
			"「成功率怎么样」「哪个模型用得最多」时用它。与 list_usage_logs 的区别是" +
			"这个给汇总数字，那个给逐条明细。" +
			"参数：hours（统计最近多少小时内，1-720，默认 24）、channel_id（只看该渠道，可选）。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"hours": {
			"type": "integer",
			"description": "统计最近多少小时内，1-720，默认 24。",
			"minimum": 1,
			"maximum": 720
		},
		"channel_id": { "type": "integer", "description": "只看该渠道。" }
	}
}`),
		Handler: t.handleSummarizeUsage,
	}
}

func (t *AgentTools) handleSummarizeUsage(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Hours     int    `json:"hours"`
		ChannelID uint64 `json:"channel_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if t.UsageLogs == nil {
		return nil, errors.New("本站未接入调用日志，无法统计")
	}

	since := t.now().Add(-time.Duration(clampToolLimit(p.Hours, 24, 720)) * time.Hour)
	q := model.UsageLogQuery{Since: &since, Limit: 1}
	if p.ChannelID > 0 {
		id := p.ChannelID
		q.ChannelID = &id
	}

	// 分别统计总量与成功量再算比率，而不是让仓储直接返回比率：
	// 后者需要额外一条 SQL，且"总量为 0 时如何算比率"要在两处定义。
	total, err := t.UsageLogs.Count(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("统计调用总量失败: %w", err)
	}
	qSuccess := q
	qSuccess.Status = model.LogStatusSuccess
	success, err := t.UsageLogs.Count(ctx, qSuccess)
	if err != nil {
		return nil, fmt.Errorf("统计成功调用数失败: %w", err)
	}

	rate := 0.0
	if total > 0 {
		rate = float64(success) / float64(total) * 100
	}
	return map[string]any{
		"hours":        clampToolLimit(p.Hours, 24, 720),
		"total":        total,
		"success":      success,
		"failed":       total - success,
		"success_rate": fmt.Sprintf("%.1f%%", rate),
		"note":         "success_rate 是这���间的整体成功率，不代表单个渠道",
	}, nil
}

// ── 工具 6：查用户 ──────────────────────────────────────────────

func (t *AgentTools) listUsersTool() Tool {
	return Tool{
		Name: "list_users",
		Description: "查询本站用户（用户名、邮箱、角色、状态、额度与已用量）。" +
			"当用户问「有多少用户」「某个用户名在不在」「谁的额度用完了」时用它。" +
			"参数：keyword（按用户名或邮箱模糊搜索，可选）、limit（返回条数，1-50，默认 20）。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"keyword": { "type": "string", "description": "按用户名或邮箱模糊搜索。" },
		"limit": {
			"type": "integer",
			"description": "返回条数，1-50，默认 20。",
			"minimum": 1,
			"maximum": 50
		}
	}
}`),
		Handler: t.handleListUsers,
	}
}

func (t *AgentTools) handleListUsers(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Keyword string `json:"keyword"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if t.Users == nil {
		return nil, errors.New("本站未接入用户数据，无法查询")
	}

	list, err := t.Users.List(ctx, model.UserQuery{
		Keyword: p.Keyword,
		Limit:   clampToolLimit(p.Limit, 20, 50),
	})
	if err != nil {
		return nil, fmt.Errorf("读取用户失败: %w", err)
	}
	if len(list) == 0 {
		return map[string]any{
			"users": []any{},
			"note":  "没有匹配的用户（若带 keyword，检查拼写是否正确）",
		}, nil
	}

	type userBrief struct {
		ID       uint64 `json:"id"`
		Username string `json:"username"`
		Email    string `json:"email,omitempty"`
		Role     string `json:"role"`
		Status   string `json:"status"`
		Quota    string `json:"quota"`
		Used     string `json:"used"`
	}
	out := make([]userBrief, 0, len(list))
	for _, u := range list {
		brief := userBrief{
			ID:       u.ID,
			Username: u.Username,
			Email:    u.Email,
			Role:     userRoleText(u.Role),
			Status:   userStatusText(u.Status),
			// 额度转成人类可读：内部单位是不便于心算的整数，
			// 模型照抄一个 123456 给站长看没有任何意义。
			Quota: quotaText(u.Quota),
			Used:  quotaText(u.UsedQuota),
		}
		out = append(out, brief)
	}
	return map[string]any{"count": len(out), "users": out}, nil
}

func userRoleText(r model.UserRole) string {
	switch r {
	case model.UserRoleAdmin:
		return "管理员"
	case model.UserRoleUser:
		return "普通用户"
	default:
		return "未知"
	}
}

func userStatusText(s model.UserStatus) string {
	switch s {
	case model.UserStatusEnabled:
		return "正常"
	case model.UserStatusDisabled:
		return "已禁用"
	default:
		return "未知"
	}
}

// quotaText 把内部额度单位转成可读文本。
//
// 内部单位换算成人类单位需要知道当前兑换比例，而那是一个会变的设置；
// 与其在这里查一次设置（多一次依赖、还可能查不到），不如直接说明"内部单位"，
// 让模型在需要精确数值时改用 summarize_usage。
func quotaText(q int64) string {
	if q == model.QuotaUnlimited {
		return "不限"
	}
	return fmt.Sprintf("%d（内部单位，1 单位约等于 1 元钱额度）", q)
}

// ── 工具 7：查充值订单 ──────────────────────────────────────────

func (t *AgentTools) listOrdersTool() Tool {
	return Tool{
		Name: "list_orders",
		Description: "查询充值订单（订单号、用户、金额、支付方式、状态、时间）。" +
			"当用户问「有没有人充值」「用户付了钱没到账」「最近有哪些订单」时用它。" +
			"参数：user_id（只看某用户的订单，可选）、status（只看某状态，可选）、limit（1-50，默认 20）。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"user_id": { "type": "integer", "description": "只看该用户的订单。" },
		"status": {
			"type": "string",
			"enum": ["pending", "paid", "closed", "refunded"],
			"description": "按状态过滤：pending 待支付 / paid 已支付 / closed 已关闭 / refunded 已退款。"
		},
		"limit": {
			"type": "integer",
			"description": "返回条数，1-50，默认 20。",
			"minimum": 1,
			"maximum": 50
		}
	}
}`),
		Handler: t.handleListOrders,
	}
}

func (t *AgentTools) handleListOrders(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		UserID uint64 `json:"user_id"`
		Status string `json:"status"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if t.Orders == nil {
		return nil, errors.New("本站未接入充值订单数据，无法查询")
	}

	q := model.PaymentOrderQuery{
		UserID: p.UserID,
		Limit:  clampToolLimit(p.Limit, 20, 50),
	}
	if p.Status != "" {
		st, ok := orderStatusFromText(p.Status)
		if !ok {
			// 枚举值非法时明确列出合法取值，而不是静默忽略该过滤条件
			// ——后者会让模型以为"查了没结果"，实际是"没按它想的过滤"。
			return nil, errors.New("status 只能是 pending / paid / closed / refunded 之一")
		}
		q.Status = &st
	}

	list, err := t.Orders.List(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("读取订单失败: %w", err)
	}
	if len(list) == 0 {
		return map[string]any{
			"orders": []any{},
			"note":   "没有匹配的订单（若带了过滤条件，试着放宽后重查）",
		}, nil
	}

	type orderBrief struct {
		TradeNo  string `json:"trade_no"`
		UserID   uint64 `json:"user_id"`
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
		Method   string `json:"method"`
		Status   string `json:"status"`
		At       string `json:"at"`
	}
	out := make([]orderBrief, 0, len(list))
	for _, o := range list {
		out = append(out, orderBrief{
			TradeNo: o.TradeNo,
			UserID:  o.UserID,
			// 金额是分，转成元展示：模型照抄 "5000" 给站长，
			// 站长会以为是 5000 块。
			Amount:   fmt.Sprintf("%.2f", float64(o.Amount)/100),
			Currency: o.Currency,
			Method:   o.Method,
			Status:   orderStatusText(o.Status),
			At:       o.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return map[string]any{"count": len(out), "orders": out}, nil
}

func orderStatusFromText(s string) (model.PaymentStatus, bool) {
	switch s {
	case "pending":
		return model.PaymentStatusPending, true
	case "paid":
		return model.PaymentStatusPaid, true
	case "closed":
		return model.PaymentStatusClosed, true
	case "refunded":
		return model.PaymentStatusRefunded, true
	default:
		return 0, false
	}
}

func orderStatusText(s model.PaymentStatus) string {
	switch s {
	case model.PaymentStatusPending:
		return "待支付"
	case model.PaymentStatusPaid:
		return "已支付"
	case model.PaymentStatusClosed:
		return "已关闭"
	case model.PaymentStatusRefunded:
		return "已退款"
	default:
		return "未知"
	}
}

// clampToolLimit 把请求条数归一到 [1, max]，缺省用 def。
func clampToolLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

// truncateText 截断长文本并标注省略了多少。
//
// 刻意返回"省略 N 字"而不是静默截断：模型看到被截断会知道信息不全，
// 静默截断会让它以为看到的就是全部，据此下结论。
func truncateText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + fmt.Sprintf("…（已省略 %d 字）", len(r)-max)
}

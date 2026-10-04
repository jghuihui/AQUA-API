// 用户侧客服的自助工具：让登录用户能自己查到"我的余额、我的订单、我的 key"。
//
// 意图（Why）：
//
//	站长要求"客服要能帮他创建 key、查背景资料"。这类需求本身完全合理——
//	用户最常问的三件事就是"我还有多少钱""我的 key 在哪""我充值成功了吗"，
//	把它们做成工具，客服就能自助回答，不必转人工。
//
// 但这件事的安全风险与"给客服加运维工具"完全不同。
//	给运维助手加工具，风险是"提示注入让 AI 动了不该动的数据"；
//	给用户侧客服加工具，风险是【用户越权看到别人的数据】——
//	后者更致命，因为用户是匿名的、数量无限的，且他们正是会主动试探边界的人。
//
// ── 本文件最重要的设计：越权在【签名上】不可表达 ──────────────
//
// 下面每一个工具的处理函数签名都是：
//
//	func(ctx context.Context, args json.RawMessage) (any, error)
//
//	userID 不在参数里，而是从 ctx 取。模型无法影响它，
// 因此"读用户 123 的订单"这个调用在协议层就写不出来。
//
// 为什么不采用更常见的做法（工具参数带 user_id，handler 里校验）：
//
//	那种写法里，越权防线是"handler 里那行 if userID != args.UserID"。
//	它能工作，但代价是：
//	  - 每加一个工具就要记得写那行检查，漏一个就开一个越权洞；
//	  - 检查散落在各处，审计时要逐个读 handler 才能确认是否安全；
//	  - 而 LLM 生成的 handler 代码很可能"看起来对"——
//	    提示词改了、模型换了、参数结构调整了，那行检查都可能悄悄失效。
//
// 把 userID 收进 ctx 之后，越权不是一个"要记得做的检查"，
// 而是一个"写不出来的东西"。默认值不是全部，而是这里。
//
// ── 仍然保留的确认机制 ───────────────────────────────────────────
//
// create_my_token 是写操作（会真的创建一把可调模型的 key）。
// 它的 Confirm 是 ConfirmNone 而非 ConfirmAlways，理由是：
//
//	创建的是【归属于用户自己】的 key，他本来就能在"令牌管理"页自己点新建。
//	客服只是把那条路径换了个入口，不存在"代替别人操作"的可能。
//	因此走"每次都弹确认"反而是骚扰 —— 用户问"怎么建 key"，
//	客服说"我来帮你建"，然后弹窗让他确认自己刚说的话，这很荒唐。
//
//	真正的防线不在确认弹窗上，而在上面那条"userID 从 ctx 取"。
//	弹窗防的是"意图被诱导"，而这里不存在"影响他人权益"的可能，
//	所以不需要它。安全属性要放在【结构性约束】上，不是【交互摩擦】上。
//
// 流转（Flow）：
//
//	门户客服对话 → ToolsForRole(UserRole) 返回 selfServiceTools
//	→ Execute 传入带 userID 的 ctx → handler 从 ctx 取身份
//	→ 仓储查询强制带 OwnerID/UserID 过滤
//
// 扩展（Extend）：
//
//	加"改自己令牌的额度"：注意它会改【账务相关字段】，
//	那时才真的需要 ConfirmAlways —— 因为额度直接关系钱。
//	判据不是"是不是写操作"，而是"是否影响金额或他人"。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// ErrNoSelfSubject 表示上下文里没有登录用户身份。
//
// 出现这个错误意味着【接线错误】：自助工具只挂在 portal 路由上（带 SessionAuth），
// 走到这里却拿不到 userID，说明中间件漏挂了或被摘掉了。
// 此时必须直接失败，不能退化成"查第一个用户的数据"——
// 那正是本文件从头到尾在防的事。
var ErrNoSelfSubject = fmt.Errorf("agent: 请求上下文中没有登录用户身份")

// selfUserIDKey 是 ctx 中携带当前用户 ID 的键。
type selfUserIDKey struct{}

// WithSelfUser 把登录用户 ID 写入上下文。
//
// 由 server 层在处理门户客服请求时调用。
// 之所以做成显式的 With 函数而不是让 agent 包自己去读 gin 上下文：
// agent 是领域层，不该知道 HTTP 框架的存在（那会让它无法单测）。
func WithSelfUser(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, selfUserIDKey{}, userID)
}

// SelfUserID 从上下文取出登录用户 ID。
func SelfUserID(ctx context.Context) (int64, error) {
	v := ctx.Value(selfUserIDKey{})
	if v == nil {
		return 0, ErrNoSelfSubject
	}
	id, ok := v.(int64)
	if !ok || id <= 0 {
		return 0, ErrNoSelfSubject
	}
	return id, nil
}

// selfServiceTools 返回用户侧客服的工具集。
//
// 与 opsTools 严格分离成两个函数（而不是加个 role 判断在里面）：
// 它们的返回类型相同但内容毫无交集，分开后"客服拿到运维工具"这件事
// 需要显式改代码才能发生，而不会因为重构不慎混进去。
func selfServiceTools(t *AgentTools) []Tool {
	return []Tool{
		toolMyAccount(t),
		toolMyOrders(t),
		toolMyTokens(t),
		toolCreateMyToken(t),
		toolMyRecentUsage(t),
		toolPublicAnnouncements(t),
	}
}

// ── 我的账户 ─────────────────────────────────────────────────────

// toolMyAccount 查当前用户的余额与账户状态。
//
// 用户问得最多的问题就是"我还剩多少钱"，让客服能直接答，
// 省掉"去后台余额页自己看"的往返。
func toolMyAccount(t *AgentTools) Tool {
	return Tool{
		Name:        "my_account",
		Description: "查询当前登录用户自己的账户信息：剩余额度、已用额度、账户状态、注册时间。当用户问「我还剩多少」「额度够不够」「账户正常吗」时使用。只返回当前用户自己的数据。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Mutating:    false,
		Confirm:     ConfirmNone,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			uid, err := SelfUserID(ctx)
			if err != nil {
				return nil, err
			}
			if t.Users == nil {
				return nil, fmt.Errorf("账户数据源不可用")
			}
			u, err := t.Users.GetByID(ctx, uint64(uid))
			if err != nil {
				return nil, fmt.Errorf("查询账户信息失败，请稍后重试")
			}
			return map[string]any{
				"用户名":       u.Username,
				"剩余额度":     u.Quota,
				"已用额度":     u.UsedQuota,
				"账户状态":     u.Status.String(),
				"注册时间":     u.CreatedAt.Format("2006-01-02"),
				"可用性说明":   "额度单位与充值页一致；剩余额度为 0 时无法调用模型接口",
				"邮箱":         maskEmail(u.Email),
				"是否已绑定邮箱": u.Email != "",
			}, nil
		},
	}
}

// ── 我的订单 ─────────────────────────────────────────────────────

// toolMyOrders 查当前用户自己的充值订单。
func toolMyOrders(t *AgentTools) Tool {
	return Tool{
		Name:        "my_orders",
		Description: "查询当前登录用户自己的充值订单记录（最近若干条），含金额、状态、创建时间。当用户问「我充值成功了吗」「订单在哪」「为什么没到账」时使用。只返回当前用户自己的订单。",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"limit":{"type":"integer","description":"返回条数，默认 5，最大 20"}
			},
			"additionalProperties":false
		}`),
		Mutating: false,
		Confirm:  ConfirmNone,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			uid, err := SelfUserID(ctx)
			if err != nil {
				return nil, err
			}
			if t.Orders == nil {
				return nil, fmt.Errorf("订单数据源不可用")
			}
			var p struct {
				Limit int `json:"limit"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return nil, fmt.Errorf("参数解析失败：limit 应为整数")
			}
			if p.Limit <= 0 {
				p.Limit = 5
			}
			if p.Limit > 20 {
				p.Limit = 20
			}

			// UserID 是从 ctx 取的，不是模型给的 —— 这是本文件的核心。
			// 注意 List 不返回 total（只有 Count 返回），
			// 所以这里额外查一次 Count。数据量小（单个用户的订单），
			// 两次查询换来"告诉用户一共几笔"，值得。
			orders, err := t.Orders.List(ctx, model.PaymentOrderQuery{
				UserID: uint64(uid),
				Limit:  p.Limit,
			})
			if err != nil {
				return nil, fmt.Errorf("查询订单失败，请稍后重试")
			}
			total, err := t.Orders.Count(ctx, model.PaymentOrderQuery{UserID: uint64(uid)})
			if err != nil {
				// 计数失败不该让整次查询失败 —— 少一个总数不影响回答。
				total = int64(len(orders))
			}

			items := make([]map[string]any, 0, len(orders))
			for _, o := range orders {
				items = append(items, map[string]any{
					"订单号":     o.TradeNo,
					"金额":     o.Amount,
					"状态":     o.Status.String(),
					"创建时间":   o.CreatedAt.Format("2006-01-02 15:04"),
					"支付时间":   formatOptionalTime(o.PaidAt),
				})
			}
			out := map[string]any{"订单总数": total, "订单列表": items}
			if len(items) == 0 {
				out["说明"] = "没有查询到订单记录。如果你刚完成充值，请稍等片刻（通常 1 分钟内到账）后重试"
			}
			return out, nil
		},
	}
}

// ── 我的令牌 ─────────────────────────────────────────────────────

// toolMyTokens 查当前用户自己的 API key 列表。
//
// 返回里刻意【不含 key 明文】：客服对话会被完整发到上游模型，
// 明文经过第三方即视为泄露。用户要拿明文应去"令牌管理"页，
// 那里是站内页面，不经第三方。
func toolMyTokens(t *AgentTools) Tool {
	return Tool{
		Name:        "my_tokens",
		Description: "查询当前登录用户自己的 API 令牌（key）列表，含名称、状态、额度、最后使用时间。当用户问「我的 key 有哪些」「key 怎么用」「为什么调不通」时使用。只返回当前用户自己的令牌，且不返回 key 明文（明文请到站点的令牌管理页面查看）。",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"limit":{"type":"integer","description":"返回条数，默认 10，最大 30"}
			},
			"additionalProperties":false
		}`),
		Mutating: false,
		Confirm:  ConfirmNone,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			uid, err := SelfUserID(ctx)
			if err != nil {
				return nil, err
			}
			if t.Tokens == nil {
				return nil, fmt.Errorf("令牌数据源不可用")
			}
			var p struct {
				Limit int `json:"limit"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return nil, fmt.Errorf("参数解析失败：limit 应为整数")
			}
			if p.Limit <= 0 {
				p.Limit = 10
			}
			if p.Limit > 30 {
				p.Limit = 30
			}

			// OwnerID 来自 ctx。这是数据层的最后一道闸：
			// 即便上面某一层的过滤被改错，查询本身也只会返回自己的令牌。
			owner := uint64(uid)
			tokens, err := t.Tokens.List(ctx, model.TokenQuery{
				OwnerID: &owner,
				Limit:   p.Limit,
			})
			if err != nil {
				return nil, fmt.Errorf("查询令牌失败，请稍后重试")
			}
			total, err := t.Tokens.Count(ctx, model.TokenQuery{OwnerID: &owner})
			if err != nil {
				total = len(tokens)
			}

			items := make([]map[string]any, 0, len(tokens))
			for _, tk := range tokens {
				items = append(items, map[string]any{
					"ID":         tk.ID,
					"名称":       tk.Name,
					"状态":       tk.Status.String(),
					"剩余额度":    tk.RemainQuota,
					"不限额度":    tk.UnlimitedQuota,
					"已用额度":    tk.UsedQuota,
					"最后使用":    formatOptionalTime(tk.LastUsedAt),
					"创建时间":    tk.CreatedAt.Format("2006-01-02"),
				})
			}
			out := map[string]any{
				"令牌总数": total,
				"令牌列表": items,
				"明文说明": "key 明文只在创建时显示一次，请到站点的「令牌管理」页面查看",
			}
			if len(items) == 0 {
				out["说明"] = "你还没有创建任何 key。可以在「令牌管理」页面新建，或让我帮你创建一把"
			}
			return out, nil
		},
	}
}

// ── 创建我的 key ─────────────────────────────────────────────────

// toolCreateMyToken 帮当前用户创建一把属于自己的 API key。
//
// 这是本文件里唯一的写操作。安全设计见文件头"仍然保留的确认机制"。
func toolCreateMyToken(t *AgentTools) Tool {
	return Tool{
		Name: "create_my_token",
		Description: "为当前登录用户创建一把新的 API 令牌（key），只能创建属于自己的。当用户说「帮我建一个 key」「怎么创建密钥」「给我生成一个 key」时使用。会真实创建令牌，key 明文只在返回结果里显示一次。",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"name":{"type":"string","description":"令牌名称，如「我的第一个 key」。留空会自动命名"},
				"quota":{"type":"integer","description":"额度上限，0 表示不单独限制（使用账户剩余额度）"}
			},
			"required":["name"],
			"additionalProperties":false
		}`),
		Mutating: true,
		Confirm:  ConfirmNone, // 见文件头：创建自己的 key 本就是用户自己能做的事
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			uid, err := SelfUserID(ctx)
			if err != nil {
				return nil, err
			}
			if t.Tokens == nil {
				return nil, fmt.Errorf("令牌数据源不可用")
			}
			var p struct {
				Name  string `json:"name"`
				Quota int64  `json:"quota"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return nil, fmt.Errorf("参数解析失败：name 应为字符串，quota 应为整数")
			}

			p.Name = strings.TrimSpace(p.Name)
			if p.Name == "" {
				p.Name = "客服创建的 key"
			}
			// 上限 40 字符：与站内令牌页保持一致，
			// 避免出现"客服建的 key 名字能有一百个字"这种割裂感。
			if len([]rune(p.Name)) > 40 {
				p.Name = string([]rune(p.Name)[:40])
			}
			if p.Quota < 0 {
				return nil, fmt.Errorf("额度不能为负数")
			}

			key, err := model.GenerateTokenKey()
			if err != nil {
				return nil, fmt.Errorf("生成 key 失败，请稍后重试")
			}

			owner := uint64(uid)
			tk := &model.Token{
				Name: p.Name,
				Key:  key,
				// OwnerID 来自 ctx，不是参数。
				// 整个函数体里没有一处允许模型影响归属人。
				OwnerID:   owner,
				Status:    model.TokenStatusEnabled,
				CreatedAt: t.now(),
				UpdatedAt: t.now(),
			}
			// 额度语义：RemainQuota 为剩余额度、UnlimitedQuota 为不限。
			// 用户说"不限额度"（quota 传 0）时走 Unlimited，
			// 而不是留一个 RemainQuota=0 的令牌 —— 那把令牌会立刻用尽。
			if p.Quota > 0 {
				tk.UnlimitedQuota = false
				tk.RemainQuota = p.Quota
			} else {
				tk.UnlimitedQuota = true
			}

			if err := tk.Validate(); err != nil {
				return nil, fmt.Errorf("创建失败：%v", err)
			}
			if err := t.Tokens.Create(ctx, tk); err != nil {
				return nil, fmt.Errorf("创建令牌失败，请稍后重试")
			}

			quotaDesc := "不限额度（受账户剩余额度限制）"
			if p.Quota > 0 {
				quotaDesc = fmt.Sprintf("%d", p.Quota)
			}

			return map[string]any{
				"创建成功":  true,
				"名称":     tk.Name,
				"ID":      tk.ID,
				"额度":     quotaDesc,
				"key明文":  key,
				"重要提示":  "key 明文只显示这一次，关闭后无法再查看。请立即复制保存；泄露后请到令牌管理页面停用或重建。",
				"使用方式":  "把它填进你的客户端配置，请求地址为站点的 API 基址（形如 /v1），模型名填站点已启用的模型名",
			}, nil
		},
	}
}

// ── 我的用量 ─────────────────────────────────────────────────────

// toolMyRecentUsage 查当前用户自己的最近调用记录。
//
// 限 24 小时且最多 20 条：用户问"我昨天调了多少"是常事，
// 但让 AI 去翻全量日志既慢又会把大量数据送进模型上下文。
func toolMyRecentUsage(t *AgentTools) Tool {
	return Tool{
		Name: "my_recent_usage",
		Description: "查询当前登录用户自己最近 24 小时的模型调用记录，含模型名、耗时、是否计费。当用户问「我昨天用了多少」「为什么额度掉得这么快」「有没有调用失败」时使用。只返回当前用户自己的调用记录。",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"limit":{"type":"integer","description":"返回条数，默认 10，最大 20"}
			},
			"additionalProperties":false
		}`),
		Mutating: false,
		Confirm:  ConfirmNone,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			uid, err := SelfUserID(ctx)
			if err != nil {
				return nil, err
			}
			if t.UsageLogs == nil {
				return nil, fmt.Errorf("用量数据源不可用")
			}
			var p struct {
				Limit int `json:"limit"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return nil, fmt.Errorf("参数解析失败：limit 应为整数")
			}
			if p.Limit <= 0 {
				p.Limit = 10
			}
			if p.Limit > 20 {
				p.Limit = 20
			}

			now := t.now()
			since := now.Add(-24 * time.Hour)
			owner := uint64(uid)
			logs, err := t.UsageLogs.List(ctx, model.UsageLogQuery{
				UserID: &owner,
				Since:  &since,
				Until:  &now,
				Limit:  p.Limit,
			})
			if err != nil {
				return nil, fmt.Errorf("查询用量失败，请稍后重试")
			}
			total, err := t.UsageLogs.Count(ctx, model.UsageLogQuery{
				UserID: &owner,
				Since:  &since,
				Until:  &now,
			})
			if err != nil {
				total = len(logs)
			}

			items := make([]map[string]any, 0, len(logs))
			var totalCost int64
			var failures int
			for _, l := range logs {
				if l.Quota > 0 {
					totalCost += l.Quota
				}
				// StatusCode >= 400 才是失败。StatusCode 为 0 的历史数据
				// 不计入失败 —— 那是"没采集到"而不是"成功"，
				// 混进成功数会给出虚假的安全感。
				if l.StatusCode >= 400 {
					failures++
				}
				row := map[string]any{
					"时间":     l.CreatedAt.Format("01-02 15:04"),
					"模型":     l.Model,
					"耗时毫秒":   l.LatencyMS,
					"消耗":     l.Quota,
				}
				if l.StatusCode >= 400 {
					row["是否失败"] = true
					if l.Error != "" {
						row["失败原因"] = l.Error
					}
				}
				items = append(items, row)
			}
			out := map[string]any{
				"时间范围": "最近 24 小时",
				"调用总数": total,
				"消耗合计": totalCost,
				"失败次数": failures,
				"调用明细": items,
			}
			if len(items) == 0 {
				out["说明"] = "最近 24 小时没有调用记录。如果你确认调过接口，请检查是否用错了 key 或请求地址"
			}
			return out, nil
		},
	}
}

// ── 站点公告（公开信息） ──────────────────────────────────────────

// toolPublicAnnouncements 读站点公告与常见问题。
//
// 这是"读背景知识"的那部分：用��不必在公告发布时额外维护一份 FAQ，
// 客服直接读真实公告 —— 于是公告一更新，客服的回答自动跟着更新。
// 避免"客服说 A、公告写 B"这种口径分裂。
func toolPublicAnnouncements(t *AgentTools) Tool {
	return Tool{
		Name:        "site_announcements",
		Description: "读取站点当前生效的公告与维护通知。当用户问「有没有公告」「什么时候维护」「为什么现在调不通」时使用。只返回已发布且未过期的公告。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Mutating:    false,
		Confirm:     ConfirmNone,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			// 公告是公开信息，不需要登录身份 —— 外部持 key 的客服也该能读。
			// 因此这里刻意【不】调 SelfUserID。
			if t.Announcements == nil {
				return nil, fmt.Errorf("公告数据源不可用")
			}
			list, err := t.Announcements.ListActive(ctx, t.now(), 10)
			if err != nil {
				return nil, fmt.Errorf("查询公告失败，请稍后重试")
			}
			items := make([]map[string]any, 0, len(list))
			for _, a := range list {
				items = append(items, map[string]any{
					"标题": a.Title,
					"内容": a.Content,
					"置顶": a.Pinned,
					"发布时间": a.PublishAt.Format("2006-01-02"),
				})
			}
			out := map[string]any{"公告数": len(items), "公告列表": items}
			if len(items) == 0 {
				out["说明"] = "当前没有生效的公告"
			}
			return out, nil
		},
	}
}

// ── 小工具 ───────────────────────────────────────────────────────

// maskEmail 对邮箱做脱敏。
//
// 为什么需要：my_account 若原样返回邮箱，客服对话会把它发到上游模型，
// 等于把用户邮箱交给了第三方。客服回答"你的注册邮箱是 a***@x.com"就够了。
func maskEmail(s string) string {
	if s == "" {
		return ""
	}
	at := strings.Index(s, "@")
	if at <= 0 {
		return "***"
	}
	name, domain := s[:at], s[at+1:]
	// 本地部分只留首字符："verylongname@x.com" → "v***@x.com"
	if len([]rune(name)) <= 1 {
		return "***@" + domain
	}
	return string([]rune(name)[0]) + "***@" + domain
}

// formatOptionalTime 处理可能为零值的时间字段。
//
// 零值时间直接 Format 会输出 "0001-01-01"，模型看到它会当成真实时间转述给用户。
func formatOptionalTime(tm time.Time) string {
	if tm.IsZero() {
		return ""
	}
	return tm.Format("2006-01-02 15:04")
}

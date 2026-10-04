// AI Agent 的用户管理工具。
//
// 意图（Why）：
//
//	站长最常问的两句是"谁在偷偷刷额度"和"帮我把这个封了"。
//	此前助手只能列出用户（list_users），改不了 —— 于是站长查完之后
//	仍然要切到后台页面去动手，那一半的排查动线就断了。
//
// 【本文件最重要的取舍：哪些操作要站长点头】
//
//	封禁用户与改额度都是 ConfirmAlways（弹窗确认），不是直接执行。
//	理由不是"谨慎过度"，而是这两类操作有一个共同特征：
//
//	  **它们的受害者不在现场。**
//
//	改渠道权重出问题时，站长立刻能看到（请求变慢了），并且随时能改回来。
//	而封禁一个用户、或者把额度改小：
//	  - 被封的用户不会收到任何通知，他只知道"突然登不上了"；
//	  - 额度被改小的用户同样无从知晓，只会遇到突然的 403；
//	  - 而助手【可能是在误判的前提下做的】（例如把一个正在批量
//	    调用的合法客户端误认成刷量）。
//
//	换句话说，这类操作一旦做错，损失由不知情的一方承担。
//	让站长在确认单上看到"哪个用户、现在多少、改成多少"，
//	是用一次点击换取"错误可被察觉"——这是必要的。
//
//	对比 list_channels 这类只读工具：读错了站长自己能看出来，
//	所以只读操作一律不需要确认。
//
// 流转（Flow）：
//
//	list_users / get_user_detail（查现状）→ 确认是目标用户
//	  → set_user_status（封禁/解封，需确认）
//	  → adjust_user_quota（改额度，需确认）
//
// 扩展（Extend）：
//
//	要"重置某用户密码"：可以加 ConfirmAlways 工具，
//	但**不要**让工具返回明文密码 —— 让站长用后台的重置流程，
//	助手只负责告诉他"该去哪个页面"。密码是这条链路上唯一一个
//	"经手即泄露"的东西，工具形态本身就是漏洞。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// ── 工具：查看用户详情 ──────────────────────────────────────────

func (t *AgentTools) getUserDetailTool() Tool {
	return Tool{
		Name: "get_user_detail",
		Description: "查看单个用户的完整信息：用户名、邮箱、角色、状态、额度与已用量、令牌数量、" +
			"注册与最后登录时间、最近的调用与订单情况。" +
			"当用户问「用户 X 到底什么情况」「他额度怎么用完的」时使用。" +
			"支持按用户名或邮箱查找（填哪个都行，系统会自动判断）。" +
			"参数：user_id（用户编号）或 username（用户名/邮箱，二选一）。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"user_id": {
			"type": "integer",
			"description": "用户 ID，来自 list_users。"
		},
		"username": {
			"type": "string",
			"description": "用户名或邮箱。与 user_id 二选一即可。"
		}
	}
}`),
		Handler: t.handleGetUserDetail,
	}
}

func (t *AgentTools) handleGetUserDetail(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		UserID   uint64 `json:"user_id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if p.UserID == 0 && p.Username == "" {
		return nil, errors.New("请给出 user_id 或 username 其中之一")
	}
	if t.Users == nil {
		return nil, errors.New("本站未接入用户数据")
	}

	// 三条查找路径：ID → 用户名 → 邮箱。
	// 模型的参数名是 username，用户很可能把邮箱填进来，
	// 因此第三条路径不是"多余"，而是这条工具能用的关键。
	var user *model.User
	var err error
	switch {
	case p.UserID > 0:
		user, err = t.Users.GetByID(ctx, p.UserID)
	case looksLikeEmail(p.Username):
		user, err = t.Users.GetByEmail(ctx, p.Username)
	default:
		user, err = t.Users.GetByUsername(ctx, p.Username)
	}
	if err != nil {
		if errors.Is(err, model.ErrUserNotFound) {
			return nil, fmt.Errorf(
				"没找到用户（id=%d, username=%q）。请用 list_users 确认，"+
					"注意用户名要完全一致", p.UserID, p.Username)
		}
		return nil, fmt.Errorf("读取用户失败: %w", err)
	}

	detail := map[string]any{
		"user_id":    user.ID,
		"username":   user.Username,
		"email":      user.Email,
		"role":       userRoleText(user.Role),
		"status":     userStatusText(user.Status),
		"quota":      quotaText(user.Quota),
		"used_quota": quotaText(user.UsedQuota),
		"created_at": user.CreatedAt.Format("2006-01-02 15:04:05"),
	}
	if !user.LastLoginAt.IsZero() {
		detail["last_login_at"] = user.LastLoginAt.Format("2006-01-02 15:04:05")
	}
	// 登录失败与锁定状态是这个用户是否在被爆破的直接证据，
	// 而它在后台要翻用户详情页才能看到 —— 这里主动带上。
	// 锁定状态比失败计数更值得说：锁定意味着"已经有人在尝试了"，
	// 而计数只说明"有过失败"，可能只是用户自己忘了密码。
	if user.FailedLogins > 0 {
		detail["failed_logins"] = user.FailedLogins
		detail["login_fail_note"] = "存在登录失败记录，若非本人操作建议立即修改密码"
	}
	if !user.LockedUntil.IsZero() && user.IsLocked(time.Now()) {
		detail["locked"] = true
		detail["lock_remain_minutes"] = int(user.LockRemain(time.Now()).Minutes()) + 1
		detail["locked_until"] = user.LockedUntil.Format("2006-01-02 15:04:05")
		detail["lock_note"] = "该账号当前处于登录锁定状态（连续失败会锁定 15 分钟）。" +
			"若本人并未反复登录失败，很可能有第三方正在尝试爆破这个账号。"
	}

	// 令牌概况：只给数量与状态分布，不列具体令牌。
	// 理由是详情页回答的是"这个用户怎么了"，
	// 而"他有哪些令牌"是一个独立问题（用 list_tokens 查更合适）。
	if t.Tokens != nil {
		tokens, err := t.Tokens.List(ctx, model.TokenQuery{
			OwnerID: &user.ID,
			Limit:   agentToolMaxRows,
		})
		if err == nil {
			enabled, disabled := 0, 0
			for _, tok := range tokens {
				if tok == nil {
					continue
				}
				if tok.Status == model.TokenStatusEnabled {
					enabled++
				} else {
					disabled++
				}
			}
			detail["token_count"] = len(tokens)
			detail["token_enabled"] = enabled
			detail["token_disabled"] = disabled
		}
	}

	return detail, nil
}

// ── 工具：封禁 / 解封用户 ───────────────────────────────────────

func (t *AgentTools) setUserStatusTool() Tool {
	return Tool{
		Name: "set_user_status",
		Description: "禁用或重新启用一个用户。禁用后该用户无法登录，其名下所有令牌也会被拒绝调用。" +
			"当用户说「把 X 封了」「为什么不能禁用他」「解封 X」时使用。" +
			"【执行前必须先用 get_user_detail 确认用户身份，把用户名与邮箱一并报给站长核对】。" +
			"此操作会影响该用户的登录与调用，需站长点击确认。" +
			"参数：user_id（用户编号）、enabled（true 启用 / false 禁用）。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"user_id": {
			"type": "integer",
			"description": "用户 ID，来自 get_user_detail 或 list_users。"
		},
		"enabled": {
			"type": "boolean",
			"description": "true 启用该用户，false 禁用该用户。"
		}
	},
	"required": ["user_id", "enabled"]
}`),
		Mutating: true,
		Confirm:  ConfirmAlways,
		// 确认单里必须带用户名与邮箱：只显示"用户 7"无法让站长核对，
		// 而误封的代价正是"封错人"。
		ConfirmTitle: "禁用/启用用户",
		ConfirmRiskNote: "被禁用的用户将立即无法登录，其名下令牌也会一并失效。" +
			"本站不会给对方发送任何通知，被误封者只会看到\"登录失败\"。",
		SummarizeChange: summarizeUserStatus,
		Handler:         t.handleSetUserStatus,
	}
}

// summarizeUserStatus 生成封禁/解封的确认明细。
//
// 【为什么要把标识信息列进确认单】
//
//	站长看到的若是"用户 7"，他无法判断那是不是自己在找的那个人 ——
//	而误封的代价是对方莫名其妙登不上，且本站不会通知他。
//	因此这里给用户名与邮箱两个可交叉核对的字段：
//	只有一种情况下是危险的（同名用户），而那种情况下邮箱能区分开。
func summarizeUserStatus(ctx context.Context, deps *AgentTools, args json.RawMessage) ([]ConfirmationItem, error) {
	var p struct {
		UserID  uint64 `json:"user_id"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if p.UserID == 0 {
		return nil, errors.New("缺少参数 user_id")
	}
	if p.Enabled == nil {
		return nil, errors.New("缺少参数 enabled")
	}
	if deps == nil || deps.Users == nil {
		return nil, errors.New("本站未接入用户数据")
	}

	user, err := deps.Users.GetByID(ctx, p.UserID)
	if err != nil {
		if errors.Is(err, model.ErrUserNotFound) {
			return nil, fmt.Errorf("用户 %d 不存在", p.UserID)
		}
		return nil, fmt.Errorf("读取用户失败: %w", err)
	}
	if !*p.Enabled && user.Role == model.UserRoleAdmin {
		return nil, errors.New("该用户是管理员，不能用助手禁用")
	}

	action := "启用"
	if !*p.Enabled {
		action = "禁用"
	}
	items := []ConfirmationItem{
		{Label: "操作", Value: action},
		{Label: "用户名", Value: user.Username},
		{Label: "用户 ID", Value: fmt.Sprintf("%d", user.ID)},
		{Label: "当前状态", Value: userStatusText(user.Status)},
	}
	if user.Email != "" {
		items = append(items, ConfirmationItem{Label: "邮箱", Value: user.Email})
	}
	items = append(items, ConfirmationItem{
		Label: "影响", Value: userStatusImpact(model.UserStatusDisabled, user),
	})
	return items, nil
}

func (t *AgentTools) handleSetUserStatus(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		UserID  uint64 `json:"user_id"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if p.Enabled == nil {
		return nil, errors.New("缺少参数 enabled（true 启用 / false 禁用），请明确指定")
	}
	if p.UserID == 0 {
		return nil, errors.New("缺少参数 user_id，请先调用 get_user_detail 确认用户")
	}
	if t.Users == nil {
		return nil, errors.New("本站未接入用户数据，无法执行该操作")
	}

	user, err := t.Users.GetByID(ctx, p.UserID)
	if err != nil {
		if errors.Is(err, model.ErrUserNotFound) {
			return nil, fmt.Errorf("用户 %d 不存在，请用 list_users 确认编号", p.UserID)
		}
		return nil, fmt.Errorf("读取用户失败: %w", err)
	}

	// 拒绝禁用管理员：这是最容易造成"全站无人能管"的操作，
	// 而助手判断"谁是管理员"未必比站长准。
	// 后台有"降级最后一个管理员"的保护，这里补上禁用这一条 ——
	// 两者是不同的路径，绕过界面直接改库时只有这条拦得住。
	if !*p.Enabled && user.Role == model.UserRoleAdmin {
		return nil, errors.New(
			"该用户是管理员，不能用这个工具禁用。" +
				"若确实要处理，请到「用户管理」页面操作（那里有额外的保护与提示）")
	}

	want := model.UserStatusDisabled
	if *p.Enabled {
		want = model.UserStatusEnabled
	}
	if user.Status == want {
		return map[string]any{
			"changed": false,
			"user_id": user.ID,
			"user":    user.Username,
			"status":  userStatusText(user.Status),
			"note":    "该用户已处于目标状态，未做修改",
		}, nil
	}

	user.Status = want
	if err := t.Users.Update(ctx, user); err != nil {
		return nil, fmt.Errorf("更新用户状态失败: %w", err)
	}
	return map[string]any{
		"changed": true,
		"user_id": user.ID,
		"user":    user.Username,
		"email":   user.Email,
		"status":  userStatusText(want),
		"impact":  userStatusImpact(want, user),
	}, nil
}

// ── 工具：调整用户额度 ──────────────────────────────────────────

func (t *AgentTools) adjustUserQuotaTool() Tool {
	return Tool{
		Name: "adjust_user_quota",
		Description: "调整用户的总可用额度（充值、扣减、改为不限额度）。" +
			"当用户说「给 X 充值 100」「把 Y 的额度调成 0」「给他改成不限」时使用。" +
			"【重要：quota 填的是调整量，不是新的总额度】——" +
			"要加 100 就填 100，要扣 100 就填 -100，要改总额度先看 get_user_detail 里的 quota 再相减。" +
			"【执行前必须先用 get_user_detail 确认用户与当前额度】。需站长点击确认。" +
			"参数：user_id（用户编号）、delta（调整量）、reason（调整原因，会记入返回结果以便追溯）。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"user_id": {
			"type": "integer",
			"description": "用户 ID，来自 get_user_detail 或 list_users。"
		},
		"delta": {
			"type": "integer",
			"description": "额度调整量：正数为增加，负数为扣减。注意这是【调整量】而非新的总额度 —— 加 100 填 100，不是填 100 之后的总额。本站不提供「改成不限额度」的工具，那需要站长到「用户管理」页面操作。"
		},
		"reason": {
			"type": "string",
			"description": "调整原因。会原样回显给站长，便于确认单上核对。"
		}
	},
	"required": ["user_id", "delta"]
}`),
		Mutating: true,
		Confirm:  ConfirmAlways,
		// 改额度是本文件里风险最高的一个：它直接对应钱，
		// 且用户的额度可能已经被用于抵扣未完成的请求。
		ConfirmTitle: "调整用户额度",
		ConfirmRiskNote: "这会立即改变该用户的可用额度。若方向填反（充值填成扣减），" +
			"用户可能立刻遇到额度不足的报错。",
		SummarizeChange: summarizeAdjustUserQuota,
		Handler:         t.handleAdjustUserQuota,
	}
}

// summarizeAdjustUserQuota 生成额度调整的确认明细。
//
// 【为什么把"调整后"也算出来给站长看，而不是只给调整量】
//
//	站长要判断的是"这个人还剩多少钱"，不是"我加了多少"。
//	只给 delta 的话，他必须在脑子里做一次加法 ——
//	而错误的加法正是这类操作出错的起点。
func summarizeAdjustUserQuota(
	ctx context.Context, deps *AgentTools, args json.RawMessage,
) ([]ConfirmationItem, error) {
	var p struct {
		UserID uint64 `json:"user_id"`
		Delta  *int64 `json:"delta"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if p.UserID == 0 {
		return nil, errors.New("缺少参数 user_id")
	}
	if p.Delta == nil {
		return nil, errors.New("缺少参数 delta")
	}
	if deps == nil || deps.Users == nil {
		return nil, errors.New("本站未接入用户数据")
	}

	user, err := deps.Users.GetByID(ctx, p.UserID)
	if err != nil {
		if errors.Is(err, model.ErrUserNotFound) {
			return nil, fmt.Errorf("用户 %d 不存在", p.UserID)
		}
		return nil, fmt.Errorf("读取用户失败: %w", err)
	}
	// 与 handler 同一道拦截：给"不限额度"用户充值的语义是明确的（无效），
	// 确认单阶段就拦下，好过让站长点完确认才看到失败。
	if user.Quota == model.QuotaUnlimited {
		return nil, errors.New(
			"该用户当前是不限额度，充值不会改变它的状态。" +
				"若要改成有限额度，请到「用户管理」页面操作")
	}
	after := user.Quota + *p.Delta
	if after < 0 {
		return nil, fmt.Errorf("调整后额度会变成负数（当前 %s，调整量 %d）",
			quotaText(user.Quota), *p.Delta)
	}

	items := []ConfirmationItem{
		{Label: "操作", Value: "调整用户额度"},
		{Label: "用户名", Value: user.Username},
		{Label: "用户 ID", Value: fmt.Sprintf("%d", user.ID)},
		{Label: "当前额度", Value: quotaText(user.Quota), After: quotaText(after)},
		{Label: "调整量", Value: fmt.Sprintf("%+d", *p.Delta)},
	}
	if p.Reason != "" {
		items = append(items, ConfirmationItem{Label: "原因", Value: p.Reason})
	}
	return items, nil
}

func (t *AgentTools) handleAdjustUserQuota(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		UserID uint64 `json:"user_id"`
		Delta  *int64 `json:"delta"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if p.Delta == nil {
		return nil, errors.New("缺少参数 delta（正数增加、负数扣减），请明确指定调整量")
	}
	if *p.Delta == 0 {
		// 允许传 0 的话，模型可能用它"试探"一下，而结果是什么都没发生 ——
		// 它会据此向站长汇报"已调整"，但用户额度分毫未动。
		return nil, errors.New("delta 不能为 0（没有任何改变）。要充值请填正数，扣减请填负数")
	}
	if p.UserID == 0 {
		return nil, errors.New("缺少参数 user_id，请先调用 get_user_detail 确认用户")
	}
	if t.Users == nil {
		return nil, errors.New("本站未接入用户数据，无法执行该操作")
	}

	user, err := t.Users.GetByID(ctx, p.UserID)
	if err != nil {
		if errors.Is(err, model.ErrUserNotFound) {
			return nil, fmt.Errorf("用户 %d 不存在，请用 list_users 确认编号", p.UserID)
		}
		return nil, fmt.Errorf("读取用户失败: %w", err)
	}

	before := user.Quota
	// AddQuota 是增量接口，且对"不限额度"用户不做任何修改
	// （见 model.UserRepository.AddQuota 的约定）——
	// 那条约定是对的：给"不限"加数字会把它变成有限额度。
	// 但它意味着【对不限额度用户充值会静默无效】，所以必须先拦住并说明。
	if user.Quota == model.QuotaUnlimited {
		return nil, errors.New(
			"该用户当前是不限额度，按本站规则充值不会改变它的状态（这是有意的设计：不限额度没有上限可加）。" +
				"若确实要改成有限额度，请到「用户管理」页面操作")
	}
	after := user.Quota + *p.Delta
	if after < 0 {
		// 不让额度变成负数：那会让用户的 used > quota，
		// 而"已用超过总额度"在本站不是一个有定义的状态
		// （各处的判断会不一致，最坏情况是无限欠费）。
		return nil, fmt.Errorf(
			"调整后额度会变成负数（当前 %s，调整量 %d）。"+
				"本站的额度不能为负——欠费请用别的方式处理，或先减少该用户的已用量",
			quotaText(user.Quota), *p.Delta)
	}

	if _, err := t.Users.AddQuota(ctx, user.ID, *p.Delta); err != nil {
		return nil, fmt.Errorf("调整用户额度失败: %w", err)
	}

	changes := []ConfirmationItem{{
		Label: "用户额度", Value: quotaText(before), After: quotaText(after),
	}}
	if p.Reason != "" {
		changes = append(changes, ConfirmationItem{Label: "原因", Value: p.Reason})
	}
	return map[string]any{
		"changed": true,
		"user_id": user.ID,
		"user":    user.Username,
		"changes": changes,
	}, nil
}

// ── 辅助 ────────────────────────────────────────────────────────

// userStatusImpact 说明状态变更后会发生什么。
//
// 为什么要把它写进返回值而不是让模型自己组织语言：
// 模型倾向于写成"该用户已被禁用"这种复述，
// 而站长需要知道的是【副作用】——"他的令牌也会一起失效"这一条
// 才是他判断影响面的依据。
func userStatusImpact(status model.UserStatus, user *model.User) string {
	if status == model.UserStatusEnabled {
		return "该用户可以重新登录，其名下令牌恢复可用"
	}
	impact := "该用户无法登录，名下所有令牌的调用也会被拒绝"
	if user.Email != "" {
		impact += "。本站不会发送通知，请自行告知对方"
	}
	return impact
}

// looksLikeEmail 判断字符串是否像邮箱。
//
// 极简判断：含 @ 且不在开头结尾。
// 不用正则是因为邮箱格式规则在 RFC 里有太多历史包袱，
// 而这里只需要区分"要不要试着按邮箱查一次"，不要求严格。
func looksLikeEmail(s string) bool {
	if s == "" {
		return false
	}
	at := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '@' {
			if at >= 0 {
				return false // 两个 @，不是邮箱
			}
			at = i
		}
	}
	return at > 0 && at < len(s)-1
}

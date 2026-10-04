// AI Agent 的令牌与渠道密钥管理工具。
//
// 意图（Why）：
//
//	站长排查"谁的调用把额度跑穿了""哪把令牌在半夜刷流量"时，
//	需要看的正是令牌与密钥。而令牌在本站是可以直接调模型接口的凭据 ——
//	它的泄露比渠道密钥更严重（渠道密钥要过本站转发，令牌是直接可用）。
//
// 【本文件最重要的约束：绝不回传令牌明文】
//
//	与 AgentKey（agent 自己的密钥）不同，令牌的明文在本站是**可查回的**
//	（后台令牌列表能看到，仓储里存了 key_enc 密文以支持展示）。
//	因此这里必须比渠道密钥更严：只给归属人、名称、状态、额度、最后使用时间，
//	连 key 的后 4 位都不给 ——
//	给 4 位看着无害，但当站长把助手接进一个公网可访问的界面时，
//	那 4 位就是一个可交叉比对的指纹。
//	完整脱敏原则见 tools.go 文件头第 4 条。
//
// 【为什么停用令牌不需确认、改额度需确认】
//
//	停用可逆（再启用即可），且是最常见的止损动作 ——
//	"半夜发现被盗刷，立刻停掉"这种场景下多一次点击都是伤害。
//	改额度则直接影响账务，且站长的意图（改总额还是改剩余）
//	在自然语言里常常是模糊的，必须让他看清改的是哪一项。
//
// 流转（Flow）：
//
//	list_tokens（只读）→ 找到可疑令牌 → set_token_status（停用，即时生效）
//	                    → adjust_token_quota（改额度，需确认）
//
// 扩展（Extend）：
//
//	要"令牌用量 TOP 榜"：用现有 list_usage_logs 按 owner 聚合即可，
//	不必新加工具 —— 聚合逻辑放工具里会让"取数"与"统计"混在一处。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// ── 工具：列出令牌 ──────────────────────────────────────────────

func (t *AgentTools) listTokensTool() Tool {
	return Tool{
		Name: "list_tokens",
		Description: "列出本站的 API 令牌（名称、归属用户、状态、额度用量、过期时间、最后使用时间）。" +
			"当用户问「有哪些令牌」「谁的令牌快超额度了」「为什么额度突然没了」时使用。" +
			"【安全提示：本工具不会返回令牌明文】。" +
			"参数：owner_id（只看某个用户的令牌，可省略）、status（1 启用 / 2 停用，可省略）、" +
			"keyword（按名称模糊匹配，可省略）、limit（1-50，默认 20）。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"owner_id": {
			"type": "integer",
			"description": "只看该用户 ID 名下的令牌。省略则返回全部。"
		},
		"status": {
			"type": "integer",
			"description": "1=启用 2=停用。省略则不按状态过滤。",
			"enum": [1, 2]
		},
		"keyword": {
			"type": "string",
			"description": "按令牌名称模糊匹配。"
		},
		"limit": {
			"type": "integer",
			"description": "返回条数，1-50，默认 20。",
			"minimum": 1,
			"maximum": 50
		}
	}
}`),
		Handler: t.handleListTokens,
	}
}

func (t *AgentTools) handleListTokens(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		OwnerID *uint64 `json:"owner_id"`
		Status  *int    `json:"status"`
		Keyword string  `json:"keyword"`
		Limit   int     `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if t.Tokens == nil {
		return nil, errors.New("本站未接入令牌数据，无法查询令牌列表")
	}

	query := model.TokenQuery{Limit: clampToolLimit(p.Limit, 20, 50)}
	if p.OwnerID != nil && *p.OwnerID > 0 {
		query.OwnerID = p.OwnerID
	}
	if p.Status != nil {
		st := model.TokenStatus(*p.Status)
		if st != model.TokenStatusEnabled && st != model.TokenStatusDisabled {
			return nil, errors.New("status 只能是 1（启用）或 2（停用）")
		}
		query.Status = &st
	}

	// 名称模糊匹配只能在内存里做：TokenQuery 没有该字段，
	// 而给仓储加一个字段会影响所有调用方（含转发热路径），
	// 代价远大于在工具层过滤 —— 令牌数量是几十级别，不是百万级。
	var tokens []*model.Token
	if kw := p.Keyword; kw != "" {
		all, err := t.Tokens.List(ctx, model.TokenQuery{Limit: agentToolMaxRows})
		if err != nil {
			return nil, fmt.Errorf("读取令牌失败: %w", err)
		}
		for _, tok := range all {
			if containsFold(tok.Name, kw) {
				tokens = append(tokens, tok)
			}
		}
	} else {
		var err error
		tokens, err = t.Tokens.List(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("读取令牌失败: %w", err)
		}
	}

	type tokenBrief struct {
		ID            uint64 `json:"id"`
		Name          string `json:"name"`
		OwnerID       uint64 `json:"owner_id"`
		Status        string `json:"status"`
		UsedQuota     int64  `json:"used_quota"`
		RemainQuota   *int64 `json:"remain_quota"`
		Unlimited     bool   `json:"unlimited"`
		ExpiresAt     string `json:"expires_at,omitempty"`
		LastUsedAt    string `json:"last_used_at,omitempty"`
		AlmostExhaust bool   `json:"almost_exhausted"`
	}
	out := make([]tokenBrief, 0, len(tokens))
	for _, tok := range tokens {
		if tok == nil {
			continue
		}
		brief := tokenBrief{
			ID:            tok.ID,
			Name:          tok.Name,
			OwnerID:       tok.OwnerID,
			Status:        tokenStatusText(tok.Status),
			UsedQuota:     tok.UsedQuota,
			Unlimited:     tok.UnlimitedQuota,
			AlmostExhaust: !tok.UnlimitedQuota && tok.RemainQuota <= tok.UsedQuota/10+1,
		}
		if !tok.UnlimitedQuota {
			remain := tok.RemainQuota
			brief.RemainQuota = &remain
		}
		// 零值时间表示"永不过期"，不下发 —— 前端拿到会显示成 1970 年。
		if !tok.ExpiresAt.IsZero() {
			brief.ExpiresAt = tok.ExpiresAt.Format("2006-01-02 15:04:05")
		}
		if !tok.LastUsedAt.IsZero() {
			brief.LastUsedAt = tok.LastUsedAt.Format("2006-01-02 15:04:05")
		}
		out = append(out, brief)
	}
	return map[string]any{
		"tokens": out,
		"note":   "令牌明文不会出现在本结果中。如需停用或调整额度，请用对应的专用工具。",
	}, nil
}

// ── 工具：启用/停用令牌 ─────────────────────────────────────────

func (t *AgentTools) setTokenStatusTool() Tool {
	return Tool{
		Name: "set_token_status",
		Description: "启用或停用一把 API 令牌。停用后用该令牌的调用会立即被拒绝（401/403），" +
			"但令牌本身不会被删除，可以随时启用回来。" +
			"当用户说「这把令牌被盗用了先停掉」「停用 X 的令牌」时使用。" +
			"【执行前必须先用 list_tokens 确认令牌 ID 与名称，不要凭猜测】。" +
			"参数：token_id（令牌编号）、enabled（true 启用 / false 停用）。" +
			"返回：是否真的改了、令牌名称与归属人。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"token_id": {
			"type": "integer",
			"description": "令牌 ID，来自 list_tokens。"
		},
		"enabled": {
			"type": "boolean",
			"description": "true 启用，false 停用。"
		}
	},
	"required": ["token_id", "enabled"]
}`),
		Mutating:        true,
		Confirm:         ConfirmNone,
		ConfirmTitle:    "切换令牌状态",
		ConfirmRiskNote: "停用后使用该令牌的所有调用会立即失败。若该令牌正在服务线上业务，请先确认已切换。",
		Handler:         t.handleSetTokenStatus,
	}
}

func (t *AgentTools) handleSetTokenStatus(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		TokenID uint64 `json:"token_id"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	// 同 tools.go 的既有约定：指针区分"没传"与"传 false"。
	// "没传 enabled"被当成"停用"是这类接口最危险的默认值。
	if p.Enabled == nil {
		return nil, errors.New("缺少参数 enabled（true 启用 / false 停用），请明确指定")
	}
	if p.TokenID == 0 {
		return nil, errors.New("缺少参数 token_id，请先调用 list_tokens 确认编号")
	}
	if t.Tokens == nil {
		return nil, errors.New("本站未接入令牌数据，无法执行该操作")
	}

	tok, err := t.Tokens.GetByID(ctx, p.TokenID)
	if err != nil {
		if errors.Is(err, model.ErrTokenNotFound) {
			return nil, fmt.Errorf("令牌 %d 不存在，请调用 list_tokens 确认编号", p.TokenID)
		}
		return nil, fmt.Errorf("读取令牌失败: %w", err)
	}

	want := model.TokenStatusDisabled
	if *p.Enabled {
		want = model.TokenStatusEnabled
	}
	if tok.Status == want {
		return map[string]any{
			"changed":  false,
			"token_id": tok.ID,
			"token":    tok.Name,
			"status":   tokenStatusText(tok.Status),
			"note":     "该令牌已处于目标状态，未做修改",
		}, nil
	}

	tok.Status = want
	if err := t.Tokens.Update(ctx, tok); err != nil {
		return nil, fmt.Errorf("更新令牌状态失败: %w", err)
	}
	return map[string]any{
		"changed":  true,
		"token_id": tok.ID,
		"token":    tok.Name,
		"owner_id": tok.OwnerID,
		"status":   tokenStatusText(want),
	}, nil
}

// ── 工具：调整令牌额度 ──────────────────────────────────────────

func (t *AgentTools) adjustTokenQuotaTool() Tool {
	return Tool{
		Name: "adjust_token_quota",
		Description: "调整一把令牌的额度。注意本站额度有两项：【总额度】与【周期预算】，" +
			"两者作用不同（总额度是总量墙，周期预算限制短期暴冲）。" +
			"quota 传 -1 表示改成不限额度。当用户说「给 X 的令牌加额度」" +
			"「把 Y 令牌改成不限」时使用。" +
			"【执行前必须先用 list_tokens 确认令牌 ID 与当前额度】。" +
			"此操作改变用户的可用额度，需站长点击确认后才执行。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"token_id": {
			"type": "integer",
			"description": "令牌 ID，来自 list_tokens。"
		},
		"quota": {
			"type": "integer",
			"description": "把【剩余额度】设为这个值（-1 表示改成不限额度）。先看 list_tokens 返回的 remain_quota 再决定填多少；若用户说「加到 1000」而当前剩余是 300，填 1000 即表示总额度 1000。",
			"minimum": -1
		},
		"budget_quota": {
			"type": "integer",
			"description": "新的周期预算额度。0 表示取消周期限制。不传表示不改这一项。",
			"minimum": 0
		},
		"budget_period": {
			"type": "string",
			"description": "预算周期：daily / weekly / monthly。给预算但不指定周期时默认按每月。",
			"enum": ["daily", "weekly", "monthly"]
		}
	},
	"required": ["token_id"]
}`),
		Mutating: true,
		// 改额度直接改变用户的可用权益，且"改总额度"与"改周期预算"
		// 在自然语言里常常是说不清的（"多给点额度"到底指哪个？）。
		// 让站长在确认单上看到"总额度 X → Y"才靠谱。
		Confirm:         ConfirmAlways,
		ConfirmTitle:    "调整令牌额度",
		ConfirmRiskNote: "这会立即改变该令牌的使用上限。若填错方向，用户可能立刻遇到额度不足的报错。",
		SummarizeChange: summarizeAdjustTokenQuota,
		Handler:         t.handleAdjustTokenQuota,
	}
}

// summarizeAdjustTokenQuota 生成令牌额度调整的确认明细。
//
// 【与用户额度确认单的关键差别】
//
//	这里要显示的是【令牌名称 + 归属用户】而不是令牌 ID。
//	站长手里往往同时有几把令牌，"令牌 3 要涨额度"这句话本身没有意义，
//	而"张三的『测试机』令牌"才有。同理，改用户额度时显示的是用户名。
func summarizeAdjustTokenQuota(
	ctx context.Context, deps *AgentTools, args json.RawMessage,
) ([]ConfirmationItem, error) {
	var p struct {
		TokenID      uint64 `json:"token_id"`
		Quota        *int64 `json:"quota"`
		BudgetQuota  *int64 `json:"budget_quota"`
		BudgetPeriod string `json:"budget_period"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if p.TokenID == 0 {
		return nil, errors.New("缺少参数 token_id")
	}
	if p.Quota == nil && p.BudgetQuota == nil && p.BudgetPeriod == "" {
		return nil, errors.New("没有指定要改哪一项")
	}
	if deps == nil || deps.Tokens == nil {
		return nil, errors.New("本站未接入令牌数据")
	}

	tok, err := deps.Tokens.GetByID(ctx, p.TokenID)
	if err != nil {
		if errors.Is(err, model.ErrTokenNotFound) {
			return nil, fmt.Errorf("令牌 %d 不存在", p.TokenID)
		}
		return nil, fmt.Errorf("读取令牌失败: %w", err)
	}

	items := []ConfirmationItem{
		{Label: "操作", Value: "调整令牌额度"},
		{Label: "令牌名称", Value: tok.Name},
		{Label: "令牌 ID", Value: fmt.Sprintf("%d", tok.ID)},
		{Label: "归属用户", Value: fmt.Sprintf("#%d", tok.OwnerID)},
	}
	if p.Quota != nil {
		before := quotaText(tok.RemainQuota)
		after := "不限额度"
		if *p.Quota >= 0 {
			after = quotaText(*p.Quota)
		}
		items = append(items, ConfirmationItem{Label: "剩余额度", Value: before, After: after})
	}
	if p.BudgetQuota != nil {
		after := quotaText(*p.BudgetQuota)
		period := p.BudgetPeriod
		if period == "" {
			period = tok.BudgetPeriod
			if period == "" {
				period = "monthly"
			}
		}
		if *p.BudgetQuota == 0 {
			after = "取消周期限制"
		} else {
			after = fmt.Sprintf("%s（%s）", after, budgetPeriodText(period))
		}
		items = append(items, ConfirmationItem{
			Label: "周期预算", Value: quotaText(tok.BudgetQuota), After: after,
		})
	} else if p.BudgetPeriod != "" {
		// 与 handler 同一道拦截：预算为 0 时改周期不生效，
		// 确认单阶段就说明，比"点完确认才报错"好。
		if tok.BudgetQuota == 0 {
			return nil, errors.New(
				"该令牌当前没有周期预算（budget_quota 为 0），只改周期不会有效果；" +
					"请同时给出 budget_quota")
		}
		items = append(items, ConfirmationItem{
			Label: "预算周期", Value: budgetPeriodText(tok.BudgetPeriod),
			After: budgetPeriodText(p.BudgetPeriod),
		})
	}
	return items, nil
}

func (t *AgentTools) handleAdjustTokenQuota(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		TokenID      uint64 `json:"token_id"`
		Quota        *int64 `json:"quota"`
		BudgetQuota  *int64 `json:"budget_quota"`
		BudgetPeriod string `json:"budget_period"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if p.TokenID == 0 {
		return nil, errors.New("缺少参数 token_id，请先调用 list_tokens 确认编号")
	}
	// 两项都没给时报错而不是"什么都不做"：
	// 后者会让模型以为自己改成功了，然后向站长汇报一个假的变更。
	if p.Quota == nil && p.BudgetQuota == nil && p.BudgetPeriod == "" {
		return nil, errors.New("没有指定要改哪一项：请至少给出 quota 或 budget_quota/budget_period")
	}
	if t.Tokens == nil {
		return nil, errors.New("本站未接入令牌数据，无法执行该操作")
	}

	tok, err := t.Tokens.GetByID(ctx, p.TokenID)
	if err != nil {
		if errors.Is(err, model.ErrTokenNotFound) {
			return nil, fmt.Errorf("令牌 %d 不存在，请调用 list_tokens 确认编号", p.TokenID)
		}
		return nil, fmt.Errorf("读取令牌失败: %w", err)
	}

	changes := make([]ConfirmationItem, 0, 3)
	if p.Quota != nil {
		before := quotaText(tok.RemainQuota)
		if *p.Quota < 0 {
			tok.UnlimitedQuota = true
			tok.RemainQuota = 0
			changes = append(changes, ConfirmationItem{
				Label: "总额度", Value: before, After: "不限",
			})
		} else {
			tok.UnlimitedQuota = false
			// 【这里必须算增量，不能直接赋值】
			//	本站没有"设置额度"的仓储方法，只有 ConsumeQuota（消费）
			//	与 Update（整行写回）。而 tok.RemainQuota 是【剩余】额度，
			//	站长的原话"总额度设成 X"若直接赋值给他，
			//	会得到"剩余 = 总额度"，等于把已用的量又送回去一次。
			//	所以先取出差额，用消费接口的反向（负数）去调。
			delta := *p.Quota - tok.RemainQuota
			if delta < 0 {
				if err := t.Tokens.ConsumeQuota(ctx, tok.ID, -delta, t.now()); err != nil {
					return nil, fmt.Errorf("调整令牌额度失败: %w", err)
				}
			}
			// 增量路径不覆盖 Unlimited 的情况已在上面处理；
			// 这里显式赋一次是为了让 Update 后的内存对象与库一致，
			// 否则返回值里的 before/after 会与实际存进去的值对不上。
			tok.RemainQuota = *p.Quota
		}
		changes = append(changes, ConfirmationItem{
			Label: "剩余额度", Value: before, After: quotaText(*p.Quota),
		})
	}
	if p.BudgetQuota != nil {
		before := quotaText(tok.BudgetQuota)
		if *p.BudgetQuota == 0 {
			tok.BudgetQuota = 0
			tok.BudgetPeriod = ""
		} else {
			tok.BudgetQuota = *p.BudgetQuota
			// 给了预算但没给周期时沿用原周期；两者都没有则补 monthly ——
			// 不给默认周期会让 BudgetQuota 永远不生效（惰性重置找不到窗口），
			// 而站长看到"已设置预算"却发现没拦住的困惑比报错更难排查。
			if p.BudgetPeriod != "" {
				tok.BudgetPeriod = p.BudgetPeriod
			} else if tok.BudgetPeriod == "" {
				tok.BudgetPeriod = "monthly"
			}
		}
		changes = append(changes, ConfirmationItem{
			Label: "周期预算", Value: before,
			After: fmt.Sprintf("%s（%s）", quotaText(tok.BudgetQuota), budgetPeriodText(tok.BudgetPeriod)),
		})
	} else if p.BudgetPeriod != "" {
		// 只改周期：预算额度为 0 时这个改法没有意义（不启用预算），
		// 明确报错而不是静默接受 —— 静默接受会让站长以为限额生效了。
		if tok.BudgetQuota == 0 {
			return nil, errors.New("该令牌当前没有周期预算（budget_quota 为 0），改周期不会有效果；" +
				"请同时给出 budget_quota")
		}
		changes = append(changes, ConfirmationItem{
			Label: "预算周期", Value: budgetPeriodText(tok.BudgetPeriod),
			After: budgetPeriodText(p.BudgetPeriod),
		})
		tok.BudgetPeriod = p.BudgetPeriod
	}

	if err := t.Tokens.Update(ctx, tok); err != nil {
		return nil, fmt.Errorf("更新令牌额度失败: %w", err)
	}
	return map[string]any{
		"changed":  true,
		"token_id": tok.ID,
		"token":    tok.Name,
		"changes":  changes,
	}, nil
}

// ── 工具：查看渠道密钥状态 ──────────────────────────────────────

func (t *AgentTools) listChannelKeysTool() Tool {
	return Tool{
		Name: "list_channel_keys",
		Description: "查看某个渠道的密钥池状态：共几把、几把可用、几把被限速或耗尽、余额还剩多少。" +
			"当用户问「X 渠道的密钥够用吗」「为什么这个渠道老是用同一把密钥」时使用。" +
			"【安全提示：本工具只显示密钥的备注与状态，绝不返回密钥内容或后几位】。" +
			"参数：channel_id（渠道编号，来自 list_channels）。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"channel_id": {
			"type": "integer",
			"description": "渠道 ID，来自 list_channels。"
		}
	},
	"required": ["channel_id"]
}`),
		Handler: t.handleListChannelKeys,
	}
}

func (t *AgentTools) handleListChannelKeys(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		ChannelID uint64 `json:"channel_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if p.ChannelID == 0 {
		return nil, errors.New("缺少参数 channel_id")
	}
	if t.ChannelKeys == nil {
		return nil, errors.New("本站未接入渠道密钥数据")
	}

	keys, err := t.ChannelKeys.ListUsable(ctx, p.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("读取渠道密钥失败: %w", err)
	}

	type keyBrief struct {
		Label     string `json:"label"`
		Kind      string `json:"kind"`
		Available bool   `json:"available"`
		Balance   string `json:"balance,omitempty"`
		// 刻意不给 key 字段，也不给后 4 位 ——
		// 见文件头：给 4 位看着无害，实则是可交叉比对的指纹。
		Note string `json:"note,omitempty"`
	}
	out := make([]keyBrief, 0, len(keys))
	for _, k := range keys {
		if k == nil {
			continue
		}
		brief := keyBrief{
			Label:     k.Label,
			Kind:      string(k.Kind),
			Available: true,
		}
		if brief.Label == "" {
			brief.Label = "(未命名)"
		}
		if k.Balance != model.BalanceUnknown {
			brief.Balance = balanceText(k.Balance)
		}
		if k.Kind != model.CredentialKindAPIKey {
			// 订阅账号类凭据：agent 不会用它调模型，但仍列出来 ——
			// 站长问"这个渠道有几种凭据"时给"不知道"是不合格的答案。
			brief.Note = "订阅账号凭据，助手不会用它调用模型"
		}
		out = append(out, brief)
	}

	// 池为空是一个需要解释的状态：它意味着这个渠道只能靠
	// channels.api_key 上那把自带密钥工作，而"池空"与"没配密钥"
	// 长得一样 —— 不说清楚，站长会以为助手在胡说。
	note := ""
	if len(out) == 0 {
		note = "该渠道的密钥池是空的。若渠道上填了自带密钥，助手仍能用它调用；" +
			"但要启用多密钥轮换、限速与故障冷却，必须往密钥池里添加密钥。"
	}
	return map[string]any{
		"channel_id": p.ChannelID,
		"usable":     len(out),
		"keys":       out,
		"note":       note,
	}, nil
}

// ── 辅助 ────────────────────────────────────────────────────────

func tokenStatusText(s model.TokenStatus) string {
	switch s {
	case model.TokenStatusEnabled:
		return "启用"
	case model.TokenStatusDisabled:
		return "已停用"
	default:
		return "未知"
	}
}

func budgetPeriodText(period string) string {
	switch period {
	case "daily":
		return "每天"
	case "weekly":
		return "每周"
	case "monthly":
		return "每月"
	case "":
		return "未启用"
	default:
		return period
	}
}

// balanceText 把密钥余额转成中文。
//
// 【只区分 model 真正定义的两态，不自己发明"偏低"】
//
//	model 里余额只有「未录入(-1)」与「已耗尽(<=0)」两种特殊值，
//	其余都是"某个正数"。曾想补一个"偏低"档（低于某个阈值提醒站长），
//	但那个阈值在 model 里并不存在 —— 自己定一个就等于让 agent
//	拿着自己发明的标准去评价站长的钱，而它并不知道那个上游的
//	余额单位是元、是美元还是次。**要加阈值必须先在 model 里定义。**
func balanceText(balance int64) string {
	switch {
	case balance == model.BalanceUnknown:
		return "未录入（调度时不受余额限制）"
	case balance <= 0:
		return "已耗尽（会被调度跳过）"
	default:
		return quotaText(balance)
	}
}

// containsFold 是忽略大小写的子串匹配。
//
// 不用 strings.Contains(strings.ToLower(...)) 是因为要分配两次；
// 令牌名多为英文，大小写混用是常态。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	if len(needle) > len(haystack) {
		return false
	}
	// 逐字节比较（ASCII 场景足够：令牌名多为英文与数字）。
	// 非 ASCII 名的场景下这个比较会漏掉部分匹配，
	// 那只影响"按名称搜令牌"的召回率，不影响任何写操作的安全性。
	lowerNeedle := toLowerASCII(needle)
	for i := 0; i+len(lowerNeedle) <= len(haystack); i++ {
		if toLowerASCII(haystack[i:i+len(lowerNeedle)]) == lowerNeedle {
			return true
		}
	}
	return false
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

/*
 * 站点运营类工具：公告、敏感词、站点设置。
 *
 * 意图（Why）：
 *   前三类工具（渠道 / 令牌 / 用户）解决的是"出故障了怎么救"，
 *   这一类解决的是"日常运营"。站长每天真正反复做的三件事是
 *   发公告、加敏感词、调站点设置，而它们全在后台页面上手点。
 *   让助手能做这三件事，才谈得上"能干活"而不只是"能查"。
 *
 * 流转（Flow）：
 *   list_announcements / list_sensitive_words / get_site_settings  ← 只读现状
 *     → publish_announcement / add_sensitive_words / set_site_setting ← 动手
 *
 * 【这一类为什么几乎全部要确认】
 *   渠道和令牌的误操作影响的是"服务能不能用"，改错了立刻会有人报障；
 *   而公告、敏感词、站点设置的误操作【不会立刻有人报障】——
 *   公告发错语气要等到用户投诉，敏感词加错会误伤正常对话，
 *   站点设置改错甚至可能几天后才发现。
 *   "不会立刻出事"恰恰是更危险的理由：它不会被发现。
 *
 * 扩展（Extend）：
 *   加"定时发布公告"时注意 PublishAt/ExpireAt 的零值约定 ——
 *   model 层用零值表示"立即/永不过期"，不要传 Unix 纪元时间。
 */
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// ── 工具：查公告 ────────────────────────────────────────────────

func (t *AgentTools) listAnnouncementsTool() Tool {
	return Tool{
		Name: "list_announcements",
		Description: "查询站点公告（含草稿与已过期）。当用户问「现在有什么公告」" +
			"「之前发过什么通知」「有没有关于 X 的公告」时使用。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"keyword": {
			"type": "string",
			"description": "按标题或正文模糊匹配。留空返回全部。"
		},
		"limit": {
			"type": "integer",
			"description": "返回条数，默认 20。",
			"minimum": 1
		}
	}
}`),
		Handler: t.handleListAnnouncements,
	}
}

func (t *AgentTools) handleListAnnouncements(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Keyword string `json:"keyword"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	if p.Limit <= 0 || p.Limit > agentToolMaxRows {
		p.Limit = 20
	}
	items, total, err := t.Announcements.List(ctx, model.AnnouncementQuery{
		Keyword: strings.TrimSpace(p.Keyword),
		Limit:   p.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("查询公告失败: %w", err)
	}
	now := t.now()
	out := make([]map[string]any, 0, len(items))
	for _, a := range items {
		// 一并算出"当前是否真的对用户可见"。
		// 只回 enabled 是不够的：一条 enabled=1 但已过期的公告，
		// 站长看到会以为它还挂着。
		out = append(out, map[string]any{
			"id":         a.ID,
			"title":      a.Title,
			"level":      string(a.Level),
			"pinned":     a.Pinned,
			"enabled":    a.Enabled,
			"visible":    announcementVisible(a, now),
			"publish_at": formatUnix(a.PublishAt),
			"expire_at":  formatUnix(a.ExpireAt),
			"summary":    truncateText(a.Content, 120),
		})
	}
	return map[string]any{"total": total, "announcements": out}, nil
}

// ── 工具：发布公告 ──────────────────────────────────────────────

func (t *AgentTools) publishAnnouncementTool() Tool {
	return Tool{
		Name: "publish_announcement",
		Description: "发布一条站点公告（会出现在用户端横幅）。当用户说「发个公告」" +
			"「通知大家维护」「把公告撤下来」时使用。" +
			"enabled 传 false 表示存为草稿（不对用户可见）。" +
			"此操作会直接影响所有用户看到的页面，需站长点击确认后才执行。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"title": {
			"type": "string",
			"description": "公告标题，会显示在前台横幅上。"
		},
		"content": {
			"type": "string",
			"description": "公告正文，纯文本。换行用换行符。"
		},
		"level": {
			"type": "string",
			"description": "展示语气。info=普通通知（默认）、success=好消息、warning=需要注意、danger=重要警告。只有 warning/danger 会用醒目样式，选错了会惊吓用户，非必要不要用。",
			"enum": ["info", "success", "warning", "danger"]
		},
		"pinned": {
			"type": "boolean",
			"description": "是否置顶。置顶公告排在普通公告之前，用户一进站就看到。只有真正重要的才置顶。"
		},
		"enabled": {
			"type": "boolean",
			"description": "true=立即发布给用户；false=存为草稿，稍后可在后台发布。不传视为 true。"
		},
		"publish_at": {
			"type": "string",
			"description": "定时发布时间，格式 2006-01-02 15:04（如 2026-10-05 10:00）。不传表示立即发布。用户说「明天早上十点发」时用它。",
			"pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}$"
		},
		"expire_at": {
			"type": "string",
			"description": "过期时间，格式 2006-01-02 15:04。不传表示永不过期。过期后公告自动对用户不可见，但后台仍能看到。",
			"pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}$"
		}
	},
	"required": ["title", "content"]
}`),
		Mutating:        true,
		Confirm:         ConfirmAlways,
		ConfirmTitle:    "发布站点公告",
		ConfirmRiskNote: "公告会立刻出现在所有用户的页面上。若语气或措辞写错，用户会先看到它、而撤回之前已经读到了。",
		SummarizeChange: summarizePublishAnnouncement,
		Handler:         t.handlePublishAnnouncement,
	}
}

func (t *AgentTools) handlePublishAnnouncement(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Title     string `json:"title"`
		Content   string `json:"content"`
		Level     string `json:"level"`
		Pinned    bool   `json:"pinned"`
		Enabled   *bool  `json:"enabled"`
		PublishAt string `json:"publish_at"`
		ExpireAt  string `json:"expire_at"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}

	level := model.AnnouncementLevel(strings.TrimSpace(p.Level))
	if strings.TrimSpace(p.Level) == "" {
		level = model.AnnouncementLevelInfo
	}

	item := &model.Announcement{
		Title:   strings.TrimSpace(p.Title),
		Content: p.Content,
		Level:   level,
		Pinned:  p.Pinned,
		Enabled: true,
	}
	if p.Enabled != nil {
		item.Enabled = *p.Enabled
	}
	// 时间解析放在构造之后、Validate 之前：
	// 解析失败要给模型一句能照着改的话，而 Validate 的报错是给站长看的。
	publishAt, err := parseSiteTime(p.PublishAt, "发布时间")
	if err != nil {
		return nil, err
	}
	expireAt, err := parseSiteTime(p.ExpireAt, "过期时间")
	if err != nil {
		return nil, err
	}
	item.PublishAt, item.ExpireAt = publishAt, expireAt

	// Validate 必须调：它拦的是"过期早于发布"这类永远不可能可见的公告，
	// 而那种公告落库后看起来一切正常，只是永远不显示。
	if err := item.Validate(); err != nil {
		return nil, err
	}
	if err := t.Announcements.Create(ctx, item); err != nil {
		return nil, fmt.Errorf("发布公告失败: %w", err)
	}
	state := "已发布，用户现在就能看到"
	switch {
	case !item.Enabled:
		state = "已存为草稿，用户看不到（可在后台发布）"
	case !item.PublishAt.IsZero() && t.now().Before(item.PublishAt):
		state = "已排期，到 " + item.PublishAt.Format("2006-01-02 15:04") + " 才对用户可见"
	}
	return map[string]any{
		"ok":         true,
		"id":         item.ID,
		"state":      state,
		"title":      item.Title,
		"level":      string(item.Level),
		"pinned":     item.Pinned,
		"publish_at": formatUnix(item.PublishAt),
		"expire_at":  formatUnix(item.ExpireAt),
	}, nil
}

func summarizePublishAnnouncement(
	_ context.Context,
	_ *AgentTools,
	args json.RawMessage,
) ([]ConfirmationItem, error) {
	var p struct {
		Title     string `json:"title"`
		Content   string `json:"content"`
		Level     string `json:"level"`
		Pinned    bool   `json:"pinned"`
		Enabled   *bool  `json:"enabled"`
		PublishAt string `json:"publish_at"`
		ExpireAt  string `json:"expire_at"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	items := []ConfirmationItem{
		{Label: "标题", Value: strings.TrimSpace(p.Title)},
		{Label: "正文", Value: truncateText(strings.TrimSpace(p.Content), 200)},
	}
	if items[1].Value == "" {
		items[1].Value = "（空）"
	}
	level := strings.TrimSpace(p.Level)
	if level == "" {
		level = string(model.AnnouncementLevelInfo)
	}
	items = append(items, ConfirmationItem{Label: "语气", Value: announcementLevelText(level)})
	if p.Pinned {
		items = append(items, ConfirmationItem{
			Label: "置顶",
			Value: "是（所有用户一进站就会先看到这条）",
		})
	}
	state := "立即发布给所有用户"
	switch {
	case p.Enabled != nil && !*p.Enabled:
		state = "存为草稿（用户看不到）"
	case strings.TrimSpace(p.PublishAt) != "":
		state = "定时发布，到 " + strings.TrimSpace(p.PublishAt) + " 起可见"
	}
	items = append(items, ConfirmationItem{Label: "发布状态", Value: state})
	if raw := strings.TrimSpace(p.ExpireAt); raw != "" {
		items = append(items, ConfirmationItem{Label: "过期时间", Value: raw})
	}
	return items, nil
}

// ── 工具：查敏感词 ──────────────────────────────────────────────

func (t *AgentTools) listSensitiveWordsTool() Tool {
	return Tool{
		Name: "list_sensitive_words",
		Description: "查询内容过滤用的敏感词表。当用户问「现在有哪些敏感词」" +
			"「XX 这个词加过吗」「敏感词表有多少条」时使用。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"enabled_only": {
			"type": "boolean",
			"description": "只返回启用中的词条。默认 false（返回全部，含已停用的）。"
		}
	}
}`),
		Handler: t.handleListSensitiveWords,
	}
}

func (t *AgentTools) handleListSensitiveWords(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		EnabledOnly bool `json:"enabled_only"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	words, err := t.SensitiveWords.List(ctx, p.EnabledOnly)
	if err != nil {
		return nil, fmt.Errorf("查询敏感词失败: %w", err)
	}
	// 统计必须在这里算好：模型若自己去数，它会把"停用的"也算进去，
	// 然后告诉我们一个偏大的数字 —— 站长会以为过滤器比实际更严。
	enabled := 0
	out := make([]map[string]any, 0, len(words))
	for _, w := range words {
		if w.Enabled {
			enabled++
		}
		out = append(out, map[string]any{
			"id":       w.ID,
			"word":     w.Word,
			"category": w.Category,
			"enabled":  w.Enabled,
			"remark":   w.Remark,
		})
	}
	if len(out) > agentToolMaxRows {
		out = out[:agentToolMaxRows]
	}
	return map[string]any{
		"total":         len(words),
		"enabled_count": enabled,
		"words":         out,
		// 明确说清截断：模型看到 50 行就以为这就是全部，
		// 下一轮可能会对着不完整的表做判断。
		"truncated": len(words) > len(out),
	}, nil
}

// ── 工具：加敏感词 ──────────────────────────────────────────────

func (t *AgentTools) addSensitiveWordsTool() Tool {
	return Tool{
		Name: "add_sensitive_words",
		Description: "向内容过滤表新增敏感词。可以一次加多个（words 传数组）。" +
			"当用户说「把 XX 加进敏感词」「这几个词都拦掉」时使用。" +
			"已存在的词会自动跳过，不会报错。" +
			"此操作会立刻改变内容过滤结果，可能误伤正常对话，需站长点击确认后才执行。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"words": {
			"type": "array",
			"description": "要新增的词条列表。一个词一个元素，不要把多个词塞进同一个元素。",
			"items": {"type": "string"},
			"minItems": 1
		},
		"category": {
			"type": "string",
			"description": "分类（如 违法违规 / 广告导流 / 人身攻击）。只用于给使用者提示，留空也可以。"
		},
		"enabled": {
			"type": "boolean",
			"description": "是否立即启用。false 表示先存着不启用（不参与匹配）。不传视为 true。"
		}
	},
	"required": ["words"]
}`),
		Mutating:        true,
		Confirm:         ConfirmAlways,
		ConfirmTitle:    "新增敏感词",
		ConfirmRiskNote: "敏感词一旦启用，用户提到这些词的对话会被拦下。词加得太宽（如「日」「钱」）会误伤大量正常对话。",
		SummarizeChange: summarizeAddSensitiveWords,
		Handler:         t.handleAddSensitiveWords,
	}
}

func (t *AgentTools) handleAddSensitiveWords(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Words    []string `json:"words"`
		Category string   `json:"category"`
		Enabled  *bool    `json:"enabled"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	cleaned := dedupeWords(p.Words)
	if len(cleaned) == 0 {
		return nil, errors.New("没有解析出任何有效的词条")
	}
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	now := t.now()
	items := make([]*model.SensitiveWord, 0, len(cleaned))
	for _, w := range cleaned {
		item := &model.SensitiveWord{
			Word:      w,
			Category:  strings.TrimSpace(p.Category),
			Enabled:   enabled,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := item.Validate(); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	added, err := t.SensitiveWords.CreateMany(ctx, items)
	if err != nil {
		return nil, fmt.Errorf("新增敏感词失败: %w", err)
	}
	state := "已启用，用户提到即被拦"
	if !enabled {
		state = "已录入但未启用（当前不参与过滤）"
	}
	return map[string]any{
		"ok":            true,
		"requested":     len(cleaned),
		"added":         added,
		"skipped":       len(cleaned) - added,
		"state":         state,
		"skip_reason":   "已存在的词会自动跳过",
		"words_preview": previewWords(cleaned),
	}, nil
}

func summarizeAddSensitiveWords(
	_ context.Context,
	_ *AgentTools,
	args json.RawMessage,
) ([]ConfirmationItem, error) {
	var p struct {
		Words    []string `json:"words"`
		Category string   `json:"category"`
		Enabled  *bool    `json:"enabled"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	cleaned := dedupeWords(p.Words)
	if len(cleaned) == 0 {
		return []ConfirmationItem{{Label: "词条", Value: "（没有解析出任何有效词条）"}}, nil
	}
	state := "立即启用（用户提到即被拦）"
	if p.Enabled != nil && !*p.Enabled {
		state = "只录入不启用（当前不参与过滤）"
	}
	return []ConfirmationItem{
		{Label: "词条", Value: previewWords(cleaned)},
		{Label: "分类", Value: orDefault(strings.TrimSpace(p.Category), "（未分类）")},
		{Label: "生效方式", Value: state},
		{Label: "已存在的词", Value: "会自动跳过，不报错"},
	}, nil
}

// ── 工具：读站点设置 ────────────────────────────────────────────

func (t *AgentTools) getSiteSettingsTool() Tool {
	return Tool{
		Name: "get_site_settings",
		Description: "读取站点设置（站点名、注册开关、默认额度、公告设置等）。" +
			"当用户问「本站注册开了吗」「默认给多少额度」「站点叫什么」时使用。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"keys": {
			"type": "array",
			"description": "只要这几项。不传则返回全部设置。",
			"items": {"type": "string"}
		}
	}
}`),
		Handler: t.handleGetSiteSettings,
	}
}

func (t *AgentTools) handleGetSiteSettings(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	all, err := t.Settings.GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取站点设置失败: %w", err)
	}

	// 指定了 keys 就只给 keys。
	// 全部设置里混着支付密钥、SMTP 密码这类凭据 —— 助手把整包设置
	// 送进模型的上下文，等于把凭据交给上游模型厂商。
	// 这个白名单之外的字段一律不回。
	if len(p.Keys) > 0 {
		out := make([]map[string]any, 0, len(p.Keys))
		for _, k := range p.Keys {
			key := strings.TrimSpace(k)
			if !agentReadableSetting(key) {
				continue
			}
			v, ok := all[key]
			if !ok {
				continue
			}
			out = append(out, map[string]any{"key": key, "value": v})
		}
		if len(out) == 0 {
			return map[string]any{
				"settings":  []any{},
				"available": agentReadableSettingKeys(),
			}, nil
		}
		return map[string]any{"settings": out}, nil
	}

	keys := make([]string, 0, len(all))
	for k := range all {
		if agentReadableSetting(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{"key": k, "value": all[k]})
	}
	return map[string]any{"settings": out}, nil
}

// agentSecretSettingSubstrings 是禁止回传给模型的关键字。
//
// 【为什么用关键字黑名单而不是精确白名单】
// 精确白名单在新增设置项时会漏：新加一项忘了加进白名单，
// 它的值就会直接进模型上下文 —— 而漏掉的那一项往往正是新加的凭据。
// 黑名单的方向相反：新增的普通设置项默认可读，只有看起来像凭据的才拦。
// 两种设计的失效方向是相反的，这里选"新增时安全"的那一种。
var agentSecretSettingSubstrings = []string{
	"secret", "password", "passwd", "token", "key", "api_key", "apikey",
	"credential", "private", "salt", "sign",
}

// agentReadableSetting 判断该设置项是否可以让模型看到。
func agentReadableSetting(key string) bool {
	lower := toLowerASCII(key)
	for _, bad := range agentSecretSettingSubstrings {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	return true
}

func agentReadableSettingKeys() []string {
	return []string{
		"可用键（示例，实际以本站为准）：site_name、site_url、register_enabled、default_quota",
		"注意：形似凭据的键（key/token/secret/password 等）不会回传，助手看不到它们。",
	}
}

// ── 工具：改站点设置 ────────────────────────────────────────────

// agentSettableKeys 是允许助手修改的设置项白名单。
//
// 白名单而非黑名单：改设置是能直接影响站点可用性的操作
// （关掉注册、清空额度、改站点名），"看起来不像敏感字段"不代表安全。
// 站长没要求的设置项，助手不该有权限动。
var agentSettableKeys = map[string]struct{}{
	"site_name":        {},
	"site_url":         {},
	"register_enabled": {},
	"default_quota":    {},
}

func (t *AgentTools) setSiteSettingTool() Tool {
	return Tool{
		Name: "set_site_setting",
		Description: "修改站点设置（站点名、站点地址、是否开放注册、用户默认额度）。" +
			"当用户说「把站点名改成 X」「关掉注册」「新用户默认给多少额度」时使用。" +
			"【只能改这几个键，其余一律拒绝】：站点名、站点地址、注册开关、默认额度。" +
			"此操作会影响全站用户，需站长点击确认后才执行。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"key": {
			"type": "string",
			"description": "设置项名称，只能是：site_name（站点名）、site_url（站点地址）、register_enabled（是否开放注册，填 true/false）、default_quota（新用户默认额度，填数字）。",
			"enum": ["site_name", "site_url", "register_enabled", "default_quota"]
		},
		"value": {
			"type": "string",
			"description": "新的值。register_enabled 填 true 或 false；default_quota 填数字，其余填文本。"
		}
	},
	"required": ["key", "value"]
}`),
		Mutating:        true,
		Confirm:         ConfirmAlways,
		ConfirmTitle:    "修改站点设置",
		ConfirmRiskNote: "站点设置对全站生效。关掉注册会让新用户无法注册；改默认额度会改变所有此后注册用户的初始额度。",
		SummarizeChange: summarizeSetSiteSetting,
		Handler:         t.handleSetSiteSetting,
	}
}

func (t *AgentTools) handleSetSiteSetting(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	key := strings.TrimSpace(p.Key)
	if _, ok := agentSettableKeys[key]; !ok {
		return nil, fmt.Errorf(
			"不允许助手修改 %q。只能改：site_name、site_url、register_enabled、default_quota。"+
				"其余设置项请在后台「系统设置」页面自行修改。", key)
	}
	value := strings.TrimSpace(p.Value)

	// 值域校验放在仓储之前：Set 是覆盖写，
	// 一个拼错的布尔值写进去就等于把注册开关设成了"非 true 皆 false"。
	switch key {
	case "register_enabled":
		switch toLowerASCII(value) {
		case "true", "1", "yes", "on", "是", "开":
			value = "true"
		case "false", "0", "no", "off", "否", "关":
			value = "false"
		default:
			return nil, fmt.Errorf("register_enabled 只能是 true 或 false，收到 %q", p.Value)
		}
	case "default_quota":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("default_quota 必须是数字，收到 %q", p.Value)
		}
		if n < 0 {
			return nil, fmt.Errorf("default_quota 不能为负数，收到 %d", n)
		}
		value = strconv.FormatInt(n, 10)
	}

	if err := t.Settings.Set(ctx, key, value); err != nil {
		return nil, fmt.Errorf("写入站点设置失败: %w", err)
	}
	return map[string]any{
		"ok":    true,
		"key":   key,
		"value": value,
		"note":  "设置已保存。若该设置需要重启或清缓存才生效，请告知站长。",
	}, nil
}

func summarizeSetSiteSetting(
	ctx context.Context,
	t *AgentTools,
	args json.RawMessage,
) ([]ConfirmationItem, error) {
	var p struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	key := strings.TrimSpace(p.Key)
	if _, ok := agentSettableKeys[key]; !ok {
		return nil, fmt.Errorf(
			"不允许助手修改 %q。只能改：site_name、site_url、register_enabled、default_quota。", key)
	}
	old, err := t.Settings.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("读取当前值失败: %w", err)
	}
	newVal := strings.TrimSpace(p.Value)
	if key == "register_enabled" {
		newVal = siteSettingBoolText(newVal)
	}
	return []ConfirmationItem{{
		Label: siteSettingLabel(key),
		Value: orDefault(strings.TrimSpace(old), "（未设置）"),
		After: newVal,
	}}, nil
}

// ── 辅助 ────────────────────────────────────────────────────────

// announcementVisible 判断公告此刻是否真的对用户可见。
func announcementVisible(a *model.Announcement, now time.Time) bool {
	if !a.Enabled {
		return false
	}
	if !a.PublishAt.IsZero() && now.Before(a.PublishAt) {
		return false
	}
	if !a.ExpireAt.IsZero() && !now.Before(a.ExpireAt) {
		return false
	}
	return true
}

func announcementLevelText(level string) string {
	switch model.AnnouncementLevel(strings.TrimSpace(level)) {
	case model.AnnouncementLevelInfo:
		return "info（普通通知，不醒目）"
	case model.AnnouncementLevelSuccess:
		return "success（好消息，绿色）"
	case model.AnnouncementLevelWarning:
		return "warning（需要注意，橙色，会引起用户注意）"
	case model.AnnouncementLevelDanger:
		return "danger（重要警告，红色，最醒目）"
	default:
		return fmt.Sprintf("%s（不在可选范围，将被拒绝）", level)
	}
}

// dedupeWords 清洗词条列表：去空白、去空串、按匹配键去重。
//
// 去重必须按 MatchKey（去空白 + 小写）而不是原样比较：
// 模型很可能在同一个数组里同时给出 "BadWord" 与 "badword"，
// 而它们在匹配器眼里是同一个词 —— 不去重的话第二条会返回"已存在"，
// 让人以为只加了一个。
func dedupeWords(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, w := range raw {
		trimmed := strings.TrimSpace(w)
		if trimmed == "" {
			continue
		}
		key := toLowerASCII(trimmed)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

// previewWords 给出适合确认单展示的词条文本。
//
// 词多时必须截断并写明总数：确认单上显示 12 个词、实际加进去 40 个，
// 那就等于让站长在不知情的情况下批准了更大的动作。
func previewWords(words []string) string {
	const maxShow = 12
	if len(words) <= maxShow {
		return strings.Join(words, "、")
	}
	return fmt.Sprintf("%s 等共 %d 个（只列出前 %d 个）",
		strings.Join(words[:maxShow], "、"), len(words), maxShow)
}

func siteSettingLabel(key string) string {
	switch key {
	case "site_name":
		return "站点名"
	case "site_url":
		return "站点地址"
	case "register_enabled":
		return "开放注册"
	case "default_quota":
		return "新用户默认额度"
	default:
		return key
	}
}

// siteSettingBoolText 把布尔设置渲染成中文。
// 关闭注册这一项站长几乎一定会盯着看，因此不能回一个裸的 "false"。
func siteSettingBoolText(v string) string {
	switch toLowerASCII(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on", "是", "开":
		return "true（已开放）"
	case "false", "0", "no", "off", "否", "关":
		return "false（已关闭）"
	default:
		return v
	}
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// formatUnix 把可能为零值的时间渲染成可读文本。
func formatUnix(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

// siteTimeLayout 是助手接受与输出的时间格式。
//
// 刻意只接受这一种：模型能稳定产出它，而 RFC3339 带时区后缀时
// 模型经常写成 "2026-10-05T10:00"（缺时区）或 "2026-10-05 10:00:00Z"（多了秒）。
// 多接受几种格式看起来友好，实测反而更容易出现"解析成功但差了几小时"的情况。
const siteTimeLayout = "2006-01-02 15:04"

// parseSiteTime 解析助手传来的时间串，空串返回零值。
//
// 零值表示"立即/永不"，是 model 层的约定，不要传 Unix 纪元时间来代替。
func parseSiteTime(raw, field string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, nil
	}
	tv, err := time.ParseInLocation(siteTimeLayout, s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"%s格式应为 %s（如 2026-10-05 18:00），无法解析 %q", field, siteTimeLayout, s)
	}
	return tv, nil
}

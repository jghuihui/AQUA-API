// AI Agent 入口的限流与审计。
//
// 意图（Why）：
//
//	用户最担心的一句话是"黑客只需要读到这个 API 接口就可以远程发货"。
//	路由级鉴权与工具授权已经把"发货"堵住了（见 router.go 的注释），
//	但还有一类风险它挡不住：【拿一把泄漏的客服 key 刷问答，把站长的钱烧光】。
//
//	每次 agent 对话都要真的调用上游模型，是真金白银的支出，
//	而且不消耗站长的转发额度、不留任何"超限"痕迹 ——
//	在账单上它看起来与正常用户的正常调用完全一样。
//	只靠"key 有效"来防，等于把信用卡贴在门把手上。
//
// 三个维度的限流，缺一不可：
//
//	1. 【按 key】—— 攻击者换 IP 无效，一把 key 就是一把钥匙。
//	2. 【按 IP】—— 攻击者换 key 无效，防止批量申请小号 key 绕过第 1 条。
//	3. 【按站点总量】—— 前两条都基于"某一把 key"或"某一个 IP"，
//	   而攻击者可以既换 key 又换 IP。这个上限是最后一道闸：
//	   它保证单站点的总支出有硬上界。
//
// 为什么第 3 条不能省：前两条的阈值都是"单把 key / 单个 IP"，
// 而攻击者控制着 key 的申请（站长给他发一把）与来源 IP（代理池）。
// 真正兜底的是"这个站点今天最多花这么多"。
//
// 流转（Flow）：
//
//	agentPublic 组挂三个限流器（key / IP / 站点）
//	  → 任一超限即 429，错误码区分维度（前端据此提示"换个 key"还是"稍后再试"）
//	  → 通过后记审计日志（谁、哪个角色、有没有带工具、结果如何）
//
// 扩展（Extend）：
//
//	要"每把 key 独立配额"：给 agent_keys 加一列 max_calls_per_day，
//	在 AgentKeyAuth 里查库时一并读出，写进上下文供本文件计数。
//	注意那样会增加一次字段读取，而 key 的鉴权在转发热路径上——
//	是否值得取决于站点是否真的有"一把 key 绑定一个客户"的用法。
package middleware

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// 限流维度常量。
//
// 为什么用字符串而非常量 int：它们会出现在响应体与日志里，
// 用字符串能让排障时一眼看出是哪一维触顶，而不必去翻代码里的数字。
const (
	// LimitScopeKey 按 agent 密钥计数。
	LimitScopeKey = "agent_key"
	// LimitScopeIP 按客户端 IP 计数。
	LimitScopeIP = "agent_ip"
	// LimitScopeSite 按站点总量计数（所有 key 加起来）。
	LimitScopeSite = "agent_site"
	// LimitScopeUser 按登录用户计数（门户客服入口）。
	//
	// 为什么单独一维：门户客服没有 agent key，它按用户会话计数。
	// 与 agent_key 分开是因为"一把 key 被多人共用"是可能的
	// （站长把客服 key 发给一个团队），而"一个账号就是一个人"是更细的粒度。
	LimitScopeUser = "agent_user"
)

// AgentKeyLimitFunc 返回按 agent 密钥计数的键。
//
// 取不到 key 时返回空串：此时应该【照常限流】而不是跳过——
// 取不到 key 意味着路由装配有问题（AgentKeyAuth 没挂），
// 而那本身就不该被放行。让空串落进同一个桶里，
// 所有"装配错误"的请求会共享一个配额并很快被限流掉，
// 从而把问题暴露出来，而不是安静地无限放行。
func AgentKeyLimit(c *gin.Context) string {
	key, ok := AgentKeyFromContext(c)
	if !ok {
		return "no-key"
	}
	// 用 KeyHash 而非明文：键会出现在内存 map 的键位上，
	// 转储内存等于泄漏了全部有效 key。明文只在鉴权那一刻存在。
	return key.KeyHash
}

// AgentIPLimit 返回按客户端 IP 计数的键。
//
// 刻意不与登录限流共用一个桶：登录失败与 agent 对话是完全不同的事，
// 合并计数会让"某人在疯狂试口令"连带把正常用户的客服对话一起限掉。
func AgentIPLimit(c *gin.Context) string {
	return "ip:" + ClientIP(c)
}

// AgentSiteLimit 返回按站点总量计数的键。
//
// 固定单桶：它表达的是"整个站点的 agent 总用量"，
// 与具体调用方无关，因此恒为同一个值。
func AgentSiteLimit(c *gin.Context) string {
	return "site:all"
}

// AgentUserLimit 返回按登录用户计数的键。
//
// 取不到用户时返回空串：门户路由挂在 SessionAuth 之下，
// 走到这里必然有用户。取不到说明装配错了 ——
// 与 AgentKeyLimit 同样让它落进同一个桶并很快触顶，把问题暴露出来，
// 而不是安静地按"每请求一个新桶"无限放行（那等于没有限流）。
func AgentUserLimit(c *gin.Context) string {
	user, ok := CurrentUser(c)
	if !ok {
		return "no-user"
	}
	return "user:" + strconv.FormatUint(user.ID, 10)
}

// WriteRateLimitKeyed 写一个带维度标记的 429。
//
// 为什么单独写而不用限流器内置的那个：内置的错误码是通用的
// （给登录接口用的），而这里前端需要区分是"key 用超了"还是"IP 被限了"——
// 前者该换一把 key，后者只能等。给出可区分的码，
// 前端才能给出正确的下一步指引。
func WriteRateLimitKeyed(c *gin.Context, scope string, retryAfter int) {
	c.Header("Retry-After", strconv.Itoa(retryAfter))
	// 按 scope 生成错误码：agent_key / agent_ip / agent_site。
	// 前端按前缀判断"是不是我的 key 的问题"，
	// 站点总量触顶时它知道这是站长该去后台调限额，而非用户该换 key。
	c.AbortWithStatusJSON(429, gin.H{
		"error": gin.H{
			"message":    rateLimitMessage(scope),
			"type":       "rate_limit_error",
			"code":       "rate_limited_" + scope,
			"scope":      scope,
			"retry_after": retryAfter,
		},
	})
}

func rateLimitMessage(scope string) string {
	switch scope {
	case LimitScopeKey:
		return "这把密钥的调用过于频繁，请稍后再试或更换密钥"
	case LimitScopeIP:
		return "请求过于频繁，请稍后再试"
	case LimitScopeSite:
		return "本站的助手调用已达上限，请稍后再试或联系管理员"
	default:
		return "请求过于频繁，请稍后再试"
	}
}

// AgentAuditLog 记录一次 agent 对话的审计条目。
//
// 为什么记审计而不仅靠访问日志：
//	访问日志记的是"谁调了哪个 URL"，答不出"这把 key 拿到了什么能力"。
//	而后者才是安全问题的核心 —— 一把 support key 在这里是零工具，
//	这个事实将来若被改错，只有审计日志能立刻看出"公网 key 拿到了写工具"。
//
// 刻意不记录问题原文与回答原文：那可能包含用户数据与站内信息，
// 而审计日志的读者范围通常比对话内容应有的范围更广。
func AgentAuditLog(c *gin.Context, role model.AgentRole, questionRunes int, outcome string) {
	entry := map[string]any{
		"event":         "agent_chat",
		"role":          string(role),
		"key_id":        agentKeyID(c),
		"question_runes": questionRunes,
		"outcome":       outcome,
		"ip":            ClientIP(c),
	}
	// 只记 key 的 ID，不记明文，也不记 key_hash ——
	// 前者能定位"是哪一把"，后者在日志里就是一把能用的钥匙。
	slog.Info("agent 对话审计", slogFields(entry)...)
}

func agentKeyID(c *gin.Context) any {
	key, ok := AgentKeyFromContext(c)
	if !ok {
		return nil
	}
	return key.ID
}

func slogFields(m map[string]any) []any {
	out := make([]any, 0, len(m)*2)
	for k, v := range m {
		out = append(out, k, v)
	}
	return out
}

// AgentRateLimitDesc 描述一个限流器的维度，用于装配时记录日志。
func AgentRateLimitDesc(scope string, limit int, windowMinutes int) string {
	return fmt.Sprintf("%s: %d 次/%d 分钟", scope, limit, windowMinutes)
}

// AgentRateLimit 返回一个【按指定维度】限流的中间件。
//
// 为什么不用 RateLimiter.Middleware：那个的错误响应是通用格式
// （固定文案 + too_many_requests），而这里需要让调用方决定
// "触顶的是哪一维"，以便前端给出不同的下一步指引 ——
// key 触顶该换一把 key，站点总量触顶该找管理员，含义完全不同。
//
// 复用同一个 RateLimiter 类型而不另写一个：滑动窗口的清理逻辑、
// 内存清理策略都已在那里验证过，复制一份只会带来"两份实现迟早不一致"。
func AgentRateLimit(l *RateLimiter, keyFunc func(*gin.Context) string, scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if l.Allow(keyFunc(c)) {
			c.Next()
			return
		}
		slog.Warn("agent 调用被限流", "scope", scope, "ip", ClientIP(c))
		// Retry-After 用一个保守的固定值：滑动窗口的最大长度未知，
		// 报一个偏小的值让客户端早点重试是可接受的（最多多试一次），
		// 而报一个偏大的值会让客户端白白等待。
		WriteRateLimitKeyed(c, scope, 60)
	}
}

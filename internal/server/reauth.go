// 本文件实现敏感操作的二次验证（STEP-UP AUTH）：
// 做高危动作前必须重新输一次密码，验证只在当前会话生效一段时间后过期。
//
// 意图（Why）：
//
//	会话被劫持的现实场景是：攻击者拿到了一台已登录设备上的令牌
//	（恶意扩展、共用电脑、流量嗅探），此后他能做用户能做的任何事。
//	而"登录时认证过密码"这件事随时间衰减——一周前的登录不足以证明此刻
//	坐在电脑前的是本人。二次验证把"证明此刻是本人"与"曾经登录过"切开。
//
// 设计取舍：
//   - 【挂在会话而不是用户上】：验证时效属于"这台设备这一次登录"。
//     挂在用户上会让一次输密码放行所有设备，等于没有验证。
//   - 【窗口制而非一次有效】窗口过长会让被劫持会话长期畅通；
//     过短会让管理员连续操作时反复输密码。15 分钟是常见取值。
//   - 【只保护不可撤回/涉及资产的写操作】：全给每一个接口都加上会让后台无法使用，
//     保护面越大、用户越倾向于想办法绕过。
//
// 流转（Flow）：
//
//	POST /api/auth/reauth {password} → 校验口令 → Sessions.UpdateReauth(sessionID, now)
//	 → 之后的敏感操作由 s.requireFreshReauth(c) 判定，过期则 403 auth.reauth_required
//
// 扩展（Extend）：
//
//	想给某个操作加保护：在处理器开头调用 s.requireFreshReauth(c)，
//	返回 true 才继续执行（见 handleGrantTrial / handleCreateBroadcast / handleAdminMarkOrderPaid）。
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/crypto"
	"github.com/LTZY-ACU/ltzy-api/internal/oai"
	"github.com/LTZY-ACU/ltzy-api/internal/server/middleware"
)

// ReauthWindow 是一次二次验证的有效时长。
//
// 取 15 分钟：足够完成"一批连续的后台操作"，
// 又短到"攻击者不可能指望一次不小心看到的密码能用一整天"。
const ReauthWindow = 15 * time.Minute

// ReauthWindowStrict 是【高危操作】的二次验证有效时长（见 requireFreshReauthStrict）。
//
// 取 2 分钟，不是"更短的 15 分钟"而是另一个量级：
// 它的目标不是"防离开工位"（那是 ReauthWindow 的职责），
// 而是"确保这一次提议与我上一次点头之间没有插入别的东西"。
//
// 2 分钟的取舍：
//   - 够读完助手的提议、想清楚要不要做、按一次回车；
//   - 不够"看完 → 去倒杯水 → 回来点确认"这种间隔，
//     而那正是被诱导的高发时间窗；
//   - 站长连续改多个用户额度会被打断多次，这是它的代价 ——
//     但改额度属于碰钱的事，值得这个摩擦。
const ReauthWindowStrict = 2 * time.Minute

// reauthRequest 是二次验证的请求体。
type reauthRequest struct {
	Password string `json:"password"`
}

// handleReauth 处理 POST /api/auth/reauth：重新验证密码并刷新当前会话的验证时刻。
//
// 返回 reauth_until：前端据此在页面上提示"免密窗口还剩多久"，
// 而不是让用户对着一个看不见的时间边界困惑。
func (s *Server) handleReauth(c *gin.Context) {
	user, ok := middleware.CurrentUser(c)
	if !ok {
		writeUserError(c, http.StatusUnauthorized,
			"auth.not_logged_in", oai.TypeAuthentication, oai.CodeMissingAPIKey)
		return
	}

	var req reauthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeUserError(c, http.StatusBadRequest,
			"request.invalid_json", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	// 口令错误时不区分"输错"与"什么都没输"，统一一个提示：
	// 这是登录之后的操作，没必要为它保留枚举语义。
	if !crypto.VerifyPassword(req.Password, user.PasswordHash) {
		writeUserError(c, http.StatusUnauthorized,
			"auth.invalid_credentials", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
		return
	}

	session, ok := middleware.CurrentSession(c)
	if !ok || session.ID == 0 {
		// 理论不可达：本路由挂在 SessionAuth 之后。真出现了说明装配有误，
		// 明确报错好过"静默当成已验证"。
		slog.Error("二次验证：上下文中缺少会话信息（路由装配是否有误？）", "user_id", user.ID)
		s.respondInternalError(c, "无法定位当前会话")
		return
	}

	now := time.Now()
	if err := s.deps.Sessions.UpdateReauth(c.Request.Context(), session.ID, now); err != nil {
		s.respondInternalError(c, "记录二次验证结果失败", err)
		return
	}
	// 同步更新上下文里的会话快照：同一个请求里若紧接着还要判定
	// （例如将来把二次验证做成中间件），不必等下次请求才生效。
	session.ReauthAt = now

	c.JSON(http.StatusOK, gin.H{
		"ok":           true,
		"reauth_until": now.Add(ReauthWindow).Unix(),
	})
}

// requireFreshReauth 判断当前会话是否在二次验证的有效窗口内。
//
// 返回 true 表示放行；false 表示已经写好响应，调用方必须立即 return。
//
// 为什么默认【不放行】找不到会话的情况：宁可让管理员多输一次密码，
// 也不能让"查不到会话"变成一条绕过二次验证的路径。
func (s *Server) requireFreshReauth(c *gin.Context) bool {
	return s.requireReauthWithin(c, ReauthWindow, "auth.reauth_required")
}

// requireFreshReauthStrict 是高危操作的版本：要求验证时刻更近。
//
// 【为什么需要比 ReauthWindow 更严的一道】
//
//	ReauthWindow 是 15 分钟，为的是让管理员能连续处理一批事务而不被打断。
//	但 AI 助手的风险模型不一样：它的操作由【模型提议】，
//	而模型可能因为一段被注入的内容去改额度、封用户、发公告。
//	对这类操作，"15 分钟前我验过一次"是不够的证据 ——
//	问题不在于人有没有离开，而在于那一刻的意图仍然成立吗。
//
//	因此高危操作要求"最近 2 分钟内刚验过"。
//	代价是站长在连续改多个用户额度时会被打断几次；
//	换来的是"上一句 innocuous 的问答"不能顺带授权一次删除。
//
//	为什么不用 UAC 式的独立令牌机制：项目已有一套成熟的二次验证
//	（同一密码弹窗、同一 ReauthAt、7 个使用点），
//	再造第二套会让管理员要记两种习惯与两个窗口时长。
//	在这里加严窗口是唯一不产生第二套认知的做法。
func (s *Server) requireFreshReauthStrict(c *gin.Context) bool {
	return s.requireReauthWithin(c, ReauthWindowStrict, "auth.reauth_required")
}

// requireReauthWithin 是两档窗口的共同实现。
//
// 参数化而不是两个几乎一样的函数：两份实现迟早会在"错误码"或
// "取不到会话时的处理"上分叉，而这类分叉表现为某些操作能绕过、某些不能，
// 极难通过测试发现。
func (s *Server) requireReauthWithin(c *gin.Context, window time.Duration, errCode string) bool {
	session, ok := middleware.CurrentSession(c)
	if !ok {
		slog.Warn("敏感操作被拦下：上下文中没有会话信息", "path", c.Request.URL.Path)
		writeUserError(c, http.StatusForbidden, errCode, oai.TypePermission, "reauth_required")
		return false
	}
	if !session.IsReauthFresh(time.Now(), window) {
		// 记一下窗口长度：排障时"为什么这次又要输密码"最常见的原因是
		// 站长没意识到高危操作的标准更高，而这行日志能直接给出答案。
		slog.Info("二次验证窗口不足，需重新输入密码",
			"path", c.Request.URL.Path, "window", window.String())
		writeUserError(c, http.StatusForbidden, errCode, oai.TypePermission, "reauth_required")
		return false
	}
	return true
}

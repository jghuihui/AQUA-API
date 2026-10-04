// 高危工具清单：哪些工具在 UAC 提权下必须"强制重新输入密码"。
//
// 为什么它属于领域层而不是 server 层：
//
//	"这个操作有多危险"是对工具本身的判断，与 HTTP、鉴权、传输无关。
//	放在 server 层会出现两个问题：
//	  - 引擎在决定"是否要问站长"时需要它（agent 层），而 SSE 回调在 server 层，
//	    两边都要用就会各写一份 —— 清单不同步是迟早的事；
//	  - 将来在别处（比如审计日志、后台"这个 agent 能做什么"的展示页）
//	    再用到它，就又多一个需要同步的点。
//
// 判据（与 ConfirmAlways 的判据不同，两者互补）：
//
//	提权令牌防的是"会话被捡走"，防不住"被诱导执行" ——
//	后者才是真正造成损失的路径（站长自己可能就不小心点了）。
//	而令牌一旦存在就有一段有效期：若诱导站长先解一次权限，
//	后续一串高危操作就会全部落在授权期内。
//
//	因此对"影响大且难以事后挽回"的工具单独加一道。
//	它匹配的是【不可逆 + 涉资金或他人权益】，不是 ConfirmAlways 这个值 ——
//	两者当前恰好重合，但语义不同。将来某个工具加了撤销功能而被降级为
//	ConfirmNone 时，它是否仍需强制重验要看这条判据，而不是看它的 Confirm 字段。
//
// 明确【不】在此列的三个写工具：set_channel_status / set_token_status / set_channel_routing。
//
//	它们同样写数据，但都作用于单个对象且可逆（改回来即可），
//	后果一眼可预知。站长连点五次"停用某渠道"不该被要求输五次密码 ——
//	那只会训练出"手快就回车"的肌肉记忆，形式上在验证，实质上已经失效。
package agent

import "sort"

// highRiskTools 是需要强制重新验证密码的工具集合。
var highRiskTools = map[string]bool{
	"adjust_user_quota":    true, // 改额度 = 直接改钱
	"adjust_token_quota":   true, // 同上，令牌额度
	"set_user_status":      true, // 封禁 = 用户无法登录且不会收到通知
	"set_site_setting":     true, // 站点级设置，可能含安全开关
	"publish_announcement": true, // 对全体用户发声，发出去无法静默撤回
	"add_sensitive_words":  true, // 内容审核策略，影响所有用户对话
}

// IsHighRiskTool 判断某工具是否需要强制重新验证管理员密码。
//
// 未知工具一律返回 false：它还没被授权执行（ToolsForRole 会先拦掉），
// 这时要求它"高危"没有意义。返回 true 反而会让"拼错的名字"变成弹密码框的理由。
func IsHighRiskTool(name string) bool { return highRiskTools[name] }

// HighRiskToolNames 返回高危工具名清单，供后台展示"这些操作会再次要求密码"。
//
// 刻意导出：让"界面告诉站长会弹几次密码"与"代码实际会弹几次"必然一致。
// 清单写死在前端的话，改了这边就会对不上。
func HighRiskToolNames() []string {
	out := make([]string, 0, len(highRiskTools))
	for name := range highRiskTools {
		out = append(out, name)
	}
	// 排序：map 遍历顺序是随机的，而它会被用于界面展示。
	// 不排序会导致每次刷新工具列表顺序都不同，看起来像页面坏了。
	sort.Strings(out)
	return out
}

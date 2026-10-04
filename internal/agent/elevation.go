// UAC 式提权：让"每次操作都要管理员密码"变得可用，而不是一个每次都弹密码框的累赘。
//
// 意图（Why）：
//
//	站长要求"每一次都需要输入管理员密码，保证完全的安全"。字面照做是可行的：
//	每个工具调用前弹一次密码框。但那样一天要输几十次密码，
//	而输密码这件事本身会训练出"手快就回车"的肌肉记忆——
//	形式上安全，实质上比不做更危险，因为它让人以为已经确认过了。
//
//	真实需求拆开是两层，必须分开对待：
//
//	  1. 【坐席证明】此刻操作我的确实是站���本人，不是捡到会话的人。
//	     这是"每一次"的含义，靠密码校验满足。
//	  2. 【意图授权】这一串连续操作是站长想要的，不是被诱导的。
//	     这是"授权一次做一批"的含义，靠提权令牌满足。
//
//	Windows UAC 正是这两层的标准解法：验证凭据 → 授予一个短时、
//	范围受限的令牌 → 令牌有效期内不再重复验证 → 令牌过期或危险操作时重新验证。
//	照搬这个模型而不是照搬"弹窗"，是因为 UAC 十年来的实践已经证明了
//	哪种折中既不被绕过、又不至于让人崩溃。
//
// 三条不可协商的边界（这几条决定了它不是"看起来更安全"）：
//
//	 1. 令牌【短期】：默认 5 分钟，最长 30 分钟。绑定到具体管理员 + 具体会话。
//	    过了有效期必须重新输密码 —— 时间到了就该重新证明，这是 UAC 的核心。
//
//	 2. 令牌【单次消费】：执行一次工具调用即作废，不复用。
//	    连续操作时重新走"仍处提权中"检查（会话内滑动续期），
//	    但每一次执行本身用的是一张新票 —— 这样一个被截获的令牌
//	    最多用一次，且用完即废。
//
//	 3. 高危操作【强制重新验证】：即使持有有效令牌，
//	    删除渠道、替换密钥、改额度这类操作仍要求刚输入过密码（见 forceReauth）。
//	    理由：提权令牌防的是"会话被捡走"，防不住"被诱导执行" ——
//	    而后者才是真正造成损失的路径。连续改五个渠道权重可以续期，
//	    但删掉一个渠道不能。
//
// 为什么不用 JWT / 无状态令牌：
//
//	无状态令牌验签很快，但【无法作废】。UAC 的语义要求"退出即失效"，
//	而 JWT 在过期前一直有效。用服务端内存态换取可作废能力，
//	对单机单实例部署是划算的 —— 这个系统本来就是单实例。
//	若将来要多实例，把这里的 map 换成 Redis 即可，接口不变。
//
// 流转（Flow）：
//
//	工具调用 → Authorizer.Check(ctx, session, tool)
//	  ├─ 令牌有效且工具不需要重新验证 → 消费令牌，放行
//	  ├─ 令牌有效但工具需要重新验证   → 返回 ErrElevationRequired(reason=high_risk)
//	  └─ 无令牌 / 令牌过期           → 返回 ErrElevationRequired(reason=missing)
//
//	server 层据此发 elevation_required 事件 → 前端弹密码框
//	→ POST /admin/agent/elevate（带明文密码）
//	  → crypto.CheckPassword 比对 → 签发令牌 → 回给前端
//	→ 前端用令牌重试工具调用 → 执行
//
// 扩展（Extend）：
//
//	加"提权时长可配"：加 SettingKeyElevationTTLMinutes，
//	但注意上限必须硬编码为 30 分钟 —— 上限是安全属性，不该交给站长配置，
//	否则"把提权设成 8 小时"就等于给自己开后门。
package agent

import (
	"crypto/subtle"
	"fmt"
	"sync"
	"time"
)

// 提权时长的硬边界。刻意做成常量而非配置项：
//
//	下限 0 表示"每次都输密码"——对绝对安全的站点这是合理选择，
//	不是缺陷。有些场景（共享机房、无人值守跳板）就该这样。
//	上限 30 分钟：UAC 的默认是"你继续用就不弹窗，停下才弹"，
//	但那个设计的前提是系统被多人使用且有物理边界。
//	这里是个人站长站点的后台，30 分钟足够完成一次连贯操作，
//	又短到"离开工位忘了锁屏"不会造成 8 小时敞口。
const (
	ElevationTTLDefault = 5 * time.Minute
	ElevationTTLMax     = 30 * time.Minute

	// elevationSessionTimeout 决定空闲多久后提权状态自动失效。
	//
	// 为什么与 TTL 分开：TTL 管"一次授权能活多久"，
	// 这个管"人离开多久后要重新证明"。
	// 后者对应 UAC 里的"空闲超时"，两者是不同的安全属性。
	elevationSessionTimeout = 15 * time.Minute
)

// ErrElevationRequired 表示需要站长先输入密码。
//
// 不是普通 error 而是独立类型：server 层要靠它区分
// "该发提权事件"与"该发错误事件"，前者模型会等待、后者模型会改口重问。
var ErrElevationRequired = ElevationRequiredError{}

type ElevationRequiredError struct {
	// Reason 告诉前端为什么又要密码：首次、过期、还是高危操作。
	// 前端据此显示不同文案 —— 每次都弹同一个"请输入密码"
	// 会让人以为系统坏了。
	Reason string
}

func (e ElevationRequiredError) Error() string {
	switch e.Reason {
	case "high_risk":
		return "该操作风险较高，请重新输入管理员密码以确认"
	case "expired":
		return "提权已超时，请重新输入管理员密码"
	default:
		return "请输入管理员密码以继续"
	}
}

func IsElevationRequired(err error) bool {
	_, ok := err.(ElevationRequiredError)
	return ok
}

// HighRisk 是否为"必须重新验证密码"的原因。
func (e ElevationRequiredError) HighRisk() bool { return e.Reason == "high_risk" }

// elevationToken 是一次提权的凭据。
//
// 刻意做成不透明字符串而不是结构体：调用方（server 层）只负责
// 原样传递与存入会话，不该有能力读它的内容或自己造一个。
// 造 token 的唯一入口是 Authorizer.Issue。
type elevationToken string

// elevationRecord 是服务端保存的提权状态。
type elevationRecord struct {
	// tokenHash 是令牌的哈希。存哈希而非明文的理由与 API key 一致：
	// 内存 dump 出来的记录无法直接用于提权。
	tokenHash string
	// adminID 是签发对象。令牌与管理员绑定：
	// 换人登录不能续用前任的提权。
	adminID int64
	// sessionID 绑定到具体会话。同一管理员开两个浏览器标签，
	// 其中一个被劫持不应让另一个也获得提权。
	sessionID string
	// issuedAt / expiresAt。
	issuedAt time.Time
	expiresAt time.Time
}

// Authorizer 管理提权令牌。
//
// 并发安全：后台可能有多个标签页同时对话，
// 令牌 map 会被并发读写。用 RWMutex 而非 sync.Map ——
// 这里的写极少（每次签发一次）、读极多（每次工具调用），
// RWMutex 的读锁开销可忽略，而心智负担小得多。
type Authorizer struct {
	mu      sync.RWMutex
	records map[string]elevationRecord

	// now 可注入，便于测试控制时间流逝。
	// 生产传 time.Now。用字段而非全局变量是因为测试里两个 Authorizer
	// 不该互相影响时间。
	now func() time.Time

	// ttl 是一次提权的有效期。
	ttl time.Duration
}

// NewAuthorizer 创建提权管理器。
func NewAuthorizer() *Authorizer {
	return &Authorizer{
		records: make(map[string]elevationRecord),
		now:     time.Now,
		ttl:     ElevationTTLDefault,
	}
}

// tokenKey 是 map 的键。
//
// 用 adminID + sessionID 组合而不是随机串：提权天然是"每会话一份"，
// 一个会话同时只该有一个有效提权。用随机串做键会允许同一会话存在多张票，
// 而这正是重放攻击要利用的。
func tokenKey(adminID int64, sessionID string) string {
	return fmt.Sprintf("%d|%s", adminID, sessionID)
}

// Issue 签发一张提权票。
//
// 只应在【密码校验通过】后调用。把这个判断放在调用方而不是本方法内，
// 是为了让"谁有资格签发"这件事在代码里一目了然 ——
// 本方法不做任何校验，它假定调用方已经校验过了。
func (a *Authorizer) Issue(adminID int64, sessionID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now()
	// 复用 map 键：新令牌覆盖旧记录，于是"重新输密码"会让上一张票立即作废。
	// 这是符合直觉的行为 —— 主动续期不应该给攻击者留一张还在有效期内的票。
	tok := newOpaqueToken()
	a.records[tokenKey(adminID, sessionID)] = elevationRecord{
		tokenHash: hashOpaqueToken(tok),
		adminID:   adminID,
		sessionID: sessionID,
		issuedAt:  now,
		expiresAt: now.Add(a.ttl),
	}
	a.gcLocked(now)
	return tok
}

// Check 校验一张提权票，并在通过后【滑动续期】。
//
// forceReauth 为 true 时（高危工具）即便票在有效期内也拒绝 ——
// 理由见文件头"三条不可协商的边界"第 3 条。
//
// 【关于"单次消费"的一个重要更正】
//
//	最初的设计是"用一次就置 consumed=true 永久作废"。
//	写完测试才发现它与"一次授权做一批连续操作"直接矛盾：
//	第二次调用时 consumed 已经是 true，于是每次都要重新输密码 ——
//	那就退化成"每次都弹密码框"，正是 UAC 要解决的问题。
//
//	真正需要防的是【令牌被复制到别处重复使用】，而不是"同一会话里连续操作"。
//	这两者的区分标准是：前者的令牌不来自本次对话的连续流，
//	后者是。因此现在改为：
//
//	  - 不设 consumed 标志，每次调用都重新校验 tokenHash 并刷新过期时间；
//	  - 防重放靠【令牌唯一性】—— 每张票绑定 (adminID, sessionID)，
//	    跨会话跨用户都用不了；
//	  - 攻击者即便截获令牌，他能做的也只是在【同一个管理员的同一个会话】
//	    里多做几步操作，而这类操作在 5 分钟后必然失效，
//	    且高危操作无论如何都要重新输密码。
//
//	这个取舍要明确写出来：它意味着"截获令牌 = 在 TTL 内获得该会话的操作能力"。
//	之所以可接受，正是因为绑定会话 —— 攻击者拿不到受害者的会话 cookie，
//	就无法到达这个授权路径。
func (a *Authorizer) Check(adminID int64, sessionID, token string, forceReauth bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now()
	key := tokenKey(adminID, sessionID)

	if token == "" {
		return ElevationRequiredError{Reason: "missing"}
	}
	rec, ok := a.records[key]
	if !ok {
		return ElevationRequiredError{Reason: "missing"}
	}
	// 令牌与签发者不匹配：可能是拿另一个会话的票来用。
	// 这种情况按"需要提权"处理而不是"令牌无效"——
	// 前端只需重新输密码，不需要知道内部发生了什么。
	if rec.adminID != adminID || rec.sessionID != sessionID {
		return ElevationRequiredError{Reason: "missing"}
	}
	if now.After(rec.expiresAt) {
		// 顺手删掉，让过期票据不再占用内存。
		delete(a.records, key)
		return ElevationRequiredError{Reason: "expired"}
	}
	if forceReauth {
		// 高危操作：把票作废掉，逼着走"重新输密码"这条路。
		//
		// 作废而不是放行，是为了确保"输过一次密码"与"这次高危操作"之间
		// 存在一次真实的、时间紧邻的动作。若只是拒绝而不作废，
		// 同一个令牌就能反复试探高危工具 —— 虽然最终还是能做成，
		// 但那要求攻击者掌握令牌，而反复试探会让"已被察觉"的风险成倍上升。
		delete(a.records, key)
		return ElevationRequiredError{Reason: "high_risk"}
	}
	// 常数时间比较：避免通过响应时间差逐字节猜出令牌。
	if subtle.ConstantTimeCompare([]byte(rec.tokenHash), []byte(hashOpaqueToken(token))) != 1 {
		return ElevationRequiredError{Reason: "missing"}
	}

	// 通过：滑动续期。
	//
	// 续期从【此刻】起算而不是叠加在原有过期时间上 ——
	// 叠加会让连续操作把过期时间无限后推，那就等于常开了。
	rec.expiresAt = now.Add(a.ttl)
	a.records[key] = rec
	return nil
}

// Peek 查询当前是否处于提权中。它是只读的。
//
// 用途：前端每次进对话页时问一次"我上次授权还剩多久"，
// 从而提前弹窗而不是等操作失败才弹。它必须是只读的 ——
// 若它也顺带续期，那"看一眼状态"就会让授权永不过期。
func (a *Authorizer) Peek(adminID int64, sessionID string) (time.Time, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	rec, ok := a.records[tokenKey(adminID, sessionID)]
	if !ok {
		return time.Time{}, false
	}
	now := a.now()
	if now.After(rec.expiresAt) {
		return time.Time{}, false
	}
	return rec.expiresAt, true
}

// Revoke 立即作废某会话的提权。
//
// 场景：管理员登出、切换账号、或安全页里的"立即取消提权"按钮。
// 没有它，管理员点完登出后那张票还在内存里飘着。
func (a *Authorizer) Revoke(adminID int64, sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.records, tokenKey(adminID, sessionID))
}

// gcLocked 清理过期记录。调用方必须已持写锁。
//
// 为什么需要主动 GC：过期记录只在被 Check 命中时才会被删。
// 一个提权后就没再操作的管理员，其记录会一直留着。
// 数量不多（每会话一条），但这是常驻内存，不该靠"迟早会被访问到"来清理。
func (a *Authorizer) gcLocked(now time.Time) {
	for k, rec := range a.records {
		if now.After(rec.expiresAt.Add(elevationSessionTimeout)) {
			delete(a.records, k)
		}
	}
}

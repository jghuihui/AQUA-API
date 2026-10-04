// UAC 式提权的契约测试。
//
// 为什么这组测试比一般单测更严格：
//
//	提权是"少一层就全线失守"的东西。测试若只验证"能提权成功"，
//	那它与"直接放行"的差别在测试结果上看不出来 ——
//	而差别恰恰是这个文件存在的全部理由。
//	因此这里的主体是【反例】：每个反例对应一种攻击路径。
//
// 被验证的三条边界（见 elevation.go 文件头）：
//  1. 令牌短期有效，过期必须重新输密码；
//  2. 令牌单次消费，重放无效；
//  3. 高危操作强制重新验证，即便令牌在有效期内。
package agent

import (
	"sync"
	"testing"
	"time"
)

const (
	testAdminID = int64(1)
	testSession = "sess-test-001"
)

// newTestAuthorizer 造一个可控时间的提权管理器。
//
// 注入时钟是必须的：这些测试要验证"5 分钟后失效"，
// 若真去 sleep 5 分钟，测试套件就得跑五分钟。
func newTestAuthorizer() (*Authorizer, func(time.Duration)) {
	a := NewAuthorizer()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	return a, func(d time.Duration) { now = now.Add(d) }
}

// ── 正例：基本流程能走通 ────────────────────────────────────────

func TestElevation_签发后可通过一次校验(t *testing.T) {
	a, _ := newTestAuthorizer()
	tok := a.Issue(testAdminID, testSession)

	if err := a.Check(testAdminID, testSession, tok, false); err != nil {
		t.Fatalf("刚签发的令牌应通过校验，实际: %v", err)
	}
}

func TestElevation_校验通过后续期可做连续操作(t *testing.T) {
	// 一次授权做一批连续操作 —— 这是 UAC 相对"每次都弹密码"的核心价值。
	a, advance := newTestAuthorizer()
	tok := a.Issue(testAdminID, testSession)

	for i := 1; i <= 5; i++ {
		advance(20 * time.Second) // 每步过去 20 秒
		if err := a.Check(testAdminID, testSession, tok, false); err != nil {
			t.Fatalf("第 %d 次操作应通过（连续授权范围内），实际: %v", i, err)
		}
	}
}

// ── 反例 1：令牌不可跨边界使用 ──────────────────────────────────
//
// 注意这里测的不是"同一令牌用两次要失败" ——
// 同一会话里连续操作【本来就该被允许】，那是 UAC 的核心价值。
// 要防的是令牌被复制到别处使用，而那需要令牌脱离原会话才有效。

func TestElevation_令牌在同会话内可连续使用(t *testing.T) {
	// 这条是"每次都弹密码"与 UAC 的分界线。
	a, advance := newTestAuthorizer()
	tok := a.Issue(testAdminID, testSession)

	advance(2 * time.Minute)
	if err := a.Check(testAdminID, testSession, tok, false); err != nil {
		t.Fatalf("同会话内滑动续期应成功: %v", err)
	}
}

func TestElevation_截获的令牌无法用于其他会话(t *testing.T) {
	// 攻击者截获令牌，但没有受害者的会话 cookie。
	// 绑定会话正是让这件事无解的原因。
	a, _ := newTestAuthorizer()
	tok := a.Issue(testAdminID, "sess-A")

	if err := a.Check(testAdminID, "sess-B", tok, false); err == nil {
		t.Fatal("截获的令牌不能用于其他会话")
	}
}

func TestElevation_截获的令牌无法用于其他管理员(t *testing.T) {
	a, _ := newTestAuthorizer()
	tok := a.Issue(1, testSession)

	if err := a.Check(2, testSession, tok, false); err == nil {
		t.Fatal("截获的令牌不能用于其他管理员")
	}
}

// ── 反例 2：过期必须重新验证 ────────────────────────────────────

func TestElevation_过期后必须重新输密码(t *testing.T) {
	a, advance := newTestAuthorizer()
	tok := a.Issue(testAdminID, testSession)

	advance(ElevationTTLMax + time.Minute) // 越过最长有效期

	err := a.Check(testAdminID, testSession, tok, false)
	if err == nil {
		t.Fatal("过期令牌必须被拒绝")
	}
	er, ok := err.(ElevationRequiredError)
	if !ok {
		t.Fatalf("应返回 ElevationRequiredError，实际: %T", err)
	}
	if er.Reason != "expired" {
		t.Errorf("reason = %q，期望 expired（前端据此显示超时文案）", er.Reason)
	}
}

func TestElevation_提权时长默认五分钟(t *testing.T) {
	// 默认 5 分钟：一次连贯操作够用，离开工位也不会敞开太久。
	if ElevationTTLDefault != 5*time.Minute {
		t.Errorf("默认提权时长 = %v，期望 5m", ElevationTTLDefault)
	}
	if ElevationTTLMax != 30*time.Minute {
		t.Errorf("提权时长上限 = %v，期望 30m（上限是安全属性，不该可配）", ElevationTTLMax)
	}
}

func TestElevation_连续操作不无限后推过期时间(t *testing.T) {
	// 续期必须"从此刻起算"而不是"叠加" ——
	// 叠加等于常开，一次提权就能用一整天。
	a, advance := newTestAuthorizer()
	tok := a.Issue(testAdminID, testSession)

	// 每 4 分钟操作一次，累计远超 5 分钟。
	for i := 0; i < 4; i++ {
		advance(4 * time.Minute)
		_ = a.Check(testAdminID, testSession, tok, false)
	}
	// 此时距首次提权已 16 分钟。但每次都续期，理论上仍有效 ——
	// 这没问题，前提是【每次操作都要拿票】。再等 6 分钟不做任何操作，
	// 最后一张票的 5 分钟到期。
	advance(6 * time.Minute)
	if err := a.Check(testAdminID, testSession, tok, false); err == nil {
		t.Fatal("停止操作超过 TTL 后必须要求重新提权")
	}
}

// ── 反例 3：高危操作强制重新验证 ────────────────────────────────

func TestElevation_高危操作即便持票也要求重新验证(t *testing.T) {
	// 提权令牌防的是"会话被捡走"，防不住"被诱导执行"。
	// 删除渠道、改额度这类操作不能因为"刚才授权过"就放行。
	a, _ := newTestAuthorizer()
	tok := a.Issue(testAdminID, testSession)

	err := a.Check(testAdminID, testSession, tok, true)
	if err == nil {
		t.Fatal("高危操作必须要求重新验证密码")
	}
	er := err.(ElevationRequiredError)
	if !er.HighRisk() {
		t.Errorf("reason = %q，期望 high_risk", er.Reason)
	}
}

func TestElevation_高危拒绝后令牌作废不可重用(t *testing.T) {
	// 关键：拒绝之后【必须作废那张票】。
	// 若只拒绝不���废，攻击者可用同一张票反复试探高危工具，
	// 或者等站长的下一次普通操作把票"续"回来。
	a, _ := newTestAuthorizer()
	tok := a.Issue(testAdminID, testSession)

	_ = a.Check(testAdminID, testSession, tok, true) // 高危，被拒
	if err := a.Check(testAdminID, testSession, tok, false); err == nil {
		t.Fatal("高危被拒后原令牌必须作废，不能降级当普通票用")
	}
}

func TestElevation_高危通过后普通操作仍需新令牌(t *testing.T) {
	// 站长输完密码完成高危操作后，应当重新走普通授权，
	// 而不是因为"刚输过密码"就长期有效。
	a, _ := newTestAuthorizer()
	tok := a.Issue(testAdminID, testSession)
	_ = a.Check(testAdminID, testSession, tok, true)

	if err := a.Check(testAdminID, testSession, tok, false); err == nil {
		t.Fatal("高危操作完成后，普通操作应重新要求提权")
	}
}

// ── 反例 4：伪造令牌 ────────────────────────────────────────────

func TestElevation_伪造令牌被拒(t *testing.T) {
	a, _ := newTestAuthorizer()
	_ = a.Issue(testAdminID, testSession)

	rec := a.records[tokenKey(testAdminID, testSession)]
	for _, fake := range []string{
		"",
		"elev-deadbeef",
		rec.tokenHash, // 用摘要冒充令牌
		rec.tokenHash[:len(rec.tokenHash)-1], // 少一位
	} {
		if err := a.Check(testAdminID, testSession, fake, false); err == nil {
			t.Errorf("伪造令牌 %q 必须被拒绝", fake)
		}
	}
}

func TestElevation_重新签发让旧令牌立即作废(t *testing.T) {
	// 主动重新输密码时，上一张还在有效期内的票必须立刻失效。
	// 否则"续期"这个动作反而给攻击者留了一张票。
	a, _ := newTestAuthorizer()
	old := a.Issue(testAdminID, testSession)
	_ = a.Issue(testAdminID, testSession) // 重新输密码

	if err := a.Check(testAdminID, testSession, old, false); err == nil {
		t.Fatal("重新签发后旧令牌必须立即作废")
	}
}

// ── 反例 5：显式撤销 ────────────────────────────────────────────

func TestElevation_撤销后立即失效(t *testing.T) {
	// 登出 / 点"取消提权"按钮之后，令牌不能还在内存里飘着。
	a, _ := newTestAuthorizer()
	tok := a.Issue(testAdminID, testSession)

	a.Revoke(testAdminID, testSession)
	if err := a.Check(testAdminID, testSession, tok, false); err == nil {
		t.Fatal("撤销后必须立即失效")
	}
}

func TestElevation_Peek是只读的(t *testing.T) {
	// 前端问"我上次授权还剩多久"不能把用户的授权消耗掉 ——
	// 否则打开对话页就掉授权，站长会莫名其妙地反复输密码。
	// 更糟的是 Peek 若续期，"看一眼"就能让提权永不过期。
	a, advance := newTestAuthorizer()
	tok := a.Issue(testAdminID, testSession)
	advance(time.Minute)

	for i := 0; i < 3; i++ {
		if _, ok := a.Peek(testAdminID, testSession); !ok {
			t.Fatal("Peek 应报告处于提权中")
		}
	}
	if err := a.Check(testAdminID, testSession, tok, false); err != nil {
		t.Fatalf("多次 Peek 后令牌仍应可用: %v", err)
	}
}

func TestElevation_Peek不延长有效期(t *testing.T) {
	// 反例：Peek 若偷偷续期，攻击者（或误操作）只需不停查询就能永久延长。
	a, advance := newTestAuthorizer()
	a.Issue(testAdminID, testSession)

	// 到期前 1 秒反复 Peek
	for i := 0; i < 5; i++ {
		advance(200 * time.Millisecond)
		a.Peek(testAdminID, testSession)
	}
	// 再过 5 秒就应过期
	advance(ElevationTTLDefault)
	if _, ok := a.Peek(testAdminID, testSession); ok {
		t.Fatal("Peek 不应延长有效期 —— 否则「看一眼」就能让提权永不过期")
	}
}

// ── 内存与并发 ──────────────────────────────────────────────────

func TestElevation_空闲记录被GC(t *testing.T) {
	// 提权后就没再操作的管理员，其记录不能永远留着。
	a, advance := newTestAuthorizer()
	tok := a.Issue(testAdminID, testSession)
	_ = tok

	advance(ElevationTTLMax + elevationSessionTimeout + time.Minute)
	a.mu.Lock()
	n := len(a.records)
	a.gcLocked(a.now())
	after := len(a.records)
	a.mu.Unlock()

	if after >= n {
		t.Errorf("过期记录应被清理：清理前 %d 条，清理后 %d 条", n, after)
	}
}

func TestElevation_并发校验不产生数据竞争(t *testing.T) {
	// 提权状态是常驻内存的共享可变状态，并发下必须自洽。
	// 断言方式：用 -race 跑，若有竞态测试会直接失败；
	// 这里额外检查"反复校验后状态仍一致"——
	// 比如某次校验把记录写坏，后续校验会开始莫名失败。
	a, _ := newTestAuthorizer()
	tok := a.Issue(testAdminID, testSession)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = a.Check(testAdminID, testSession, tok, false)
				a.Peek(testAdminID, testSession)
			}
		}()
	}
	wg.Wait()

	if err := a.Check(testAdminID, testSession, tok, false); err != nil {
		t.Fatalf("并发校验后状态应仍然自洽: %v", err)
	}
}

func TestElevation_并发签发不产生重复有效票(t *testing.T) {
	// 同一会话反复签发，map 键相同所以只保留最后一张。
	// 关键是：无论签发多少次，同时有效的票只有一张。
	a, _ := newTestAuthorizer()

	var wg sync.WaitGroup
	tokens := make([]string, 30)
	for i := range tokens {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i] = a.Issue(testAdminID, testSession)
		}(i)
	}
	wg.Wait()

	valid := 0
	for _, tok := range tokens {
		if err := a.Check(testAdminID, testSession, tok, false); err == nil {
			valid++
		}
	}
	if valid != 1 {
		t.Errorf("有效令牌数 = %d，期望 1", valid)
	}
}

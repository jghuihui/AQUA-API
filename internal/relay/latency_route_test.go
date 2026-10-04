// 加速器·延迟感知选路的单元测试。
//
// 测试重点按"错了会怎样"排序：
//
//  1. **关闭加速器时行为与改造前一致** —— 这是本文件所有代码的前提。
//     任何"ratio=0 却仍改变了选路结果"的退化，都意味着一次未被声明的
//     行为变更：站长只想让它快点，却不知道流量分配变了。
//  2. **缺延迟数据的渠道不会被判死刑** —— 新加的渠道如果永远拿不到流量，
//     就永远测不到速度，形成死锁。
//  3. **超慢渠道不会被永久出局** —— 一次异常的测活不该废掉一个渠道。
//  4. **延迟不跨优先级** —— 优先级是成本控制手段，不能被速度覆盖。
//  5. **分布而非硬排序** —— 快渠道拿到更多流量，但不是全部。
package relay

import (
	"math"
	"testing"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// ch 快速构造一个测试渠道。
func ch(id uint64, weight, priority, latency int) *model.Channel {
	return &model.Channel{
		ID: id, Name: "c", Weight: weight, Priority: priority,
		Status: model.ChannelStatusEnabled, LatencyMS: latency,
	}
}

// pickN 抽样 n 次并统计各渠道被选中的次数。
func pickN(t *testing.T, channels []*model.Channel, ratio float64, n int) map[uint64]int {
	t.Helper()
	hits := make(map[uint64]int, len(channels))
	for range n {
		c := pickByWeight(channels, ratio)
		if c == nil {
			t.Fatalf("pickByWeight 返回 nil，输入非空（%d 个渠道）", len(channels))
		}
		hits[c.ID]++
	}
	return hits
}

// ratioNear 判断浮点是否在容差内相等。
func ratioNear(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < tol
}

// TestLatencyScore_无数据与极小值都给中性分 是整个文件最重要的一条。
//
// 没测过的渠道（LatencyMS=0）若被判 0 分，它就永远拿不到流量；
// 而拿不到流量就永远不会被选中去测速 —— 死锁。
// 它必须与"延迟刚好等于参考上限"同分，即"不奖不罚"。
func TestLatencyScore_无数据与极小值都给中性分(t *testing.T) {
	cases := []struct {
		name    string
		latency int
	}{
		{"从未测过", 0},
		{"负数（脏数据）", -100},
		{"过小不可信", latencyMinUsableMS - 1},
	}
	for _, tc := range cases {
		got := latencyScore(ch(1, 1, 0, tc.latency))
		if !ratioNear(got, latencyUnknownScore, 1e-9) {
			t.Errorf("%s（LatencyMS=%d）得分 = %v，应为中性分 %v",
				tc.name, tc.latency, got, latencyUnknownScore)
		}
	}
}

// TestLatencyScore_越快分越高且有上界 验证得分的方向与范围。
func TestLatencyScore_越快分越高且有上界(t *testing.T) {
	fast := latencyScore(ch(1, 1, 0, 100))
	slow := latencyScore(ch(2, 1, 0, 1500))
	verySlow := latencyScore(ch(3, 1, 0, 10000))

	if fast <= slow {
		t.Errorf("快渠道得分 %v 应高于慢渠道 %v", fast, slow)
	}
	if slow <= verySlow {
		t.Errorf("慢渠道得分 %v 应高于超慢渠道 %v", slow, verySlow)
	}
	if fast > 1.0 {
		t.Errorf("得分不应超过 1.0，实际 %v", fast)
	}
	if verySlow <= 0 {
		t.Errorf("超慢渠道得分必须为正（不能判死刑），实际 %v", verySlow)
	}
}

// TestPickByWeight_关闭加速器时与加权随机同分布 是本文件的基线契约。
//
// ratio=0 时有效权重必须【恰好】等于渠道权重 —— 不是"接近"，
// 而是同一个整数。差一点就意味着加速器关闭时流量分配已经变了。
func TestPickByWeight_关闭加速器时与加权随机同分布(t *testing.T) {
	channels := []*model.Channel{
		ch(1, 80, 0, 5000), // 权重 80、延迟很差
		ch(2, 20, 0, 10),   // 权重 20、延迟极好
		ch(3, 50, 0, 0),    // 权重 50、无延迟数据
	}

	for _, c := range channels {
		got := pickByWeight([]*model.Channel{c}, 0)
		if got.ID != c.ID {
			t.Errorf("单渠道时 pickByWeight 返回了 %d，应为 %d", got.ID, c.ID)
		}
	}

	// 三个渠道权重互不相同，且延迟与权重【反向】（权重大的最慢），
	// 这样只要延迟渗入了权重，分布就一定会偏。
	const n = 60000
	hits := pickN(t, channels, 0, n)
	for _, c := range channels {
		want := float64(c.Weight) / 150 * n // 80+20+50=150
		got := float64(hits[c.ID])
		// 容差取 3%，二项分布 6 万样本的标准差约 0.4%，3% 已经很宽松。
		if !ratioNear(got, want, want*0.03) {
			t.Errorf("渠道 %d 命中 %v，期望约 %.0f（纯权重下应为 %.1f%%）",
				c.ID, hits[c.ID], want, want*100/n)
		}
	}
}

// TestPickByWeight_开启后快渠道获得更多流量 但不独占。
func TestPickByWeight_开启后快渠道获得更多流量但不独占(t *testing.T) {
	channels := []*model.Channel{
		ch(1, 50, 0, 2000), // 慢
		ch(2, 50, 0, 100),  // 快
	}

	const n = 60000
	off := pickN(t, channels, 0, n)
	on := pickN(t, channels, 1.0, n)

	// 关闭时两者各半（权重相同）。
	if !ratioNear(float64(off[1]), float64(off[2]), float64(n)*0.03) {
		t.Errorf("关闭加速器时应各半，实际 %v", off)
	}
	// 开启后快渠道应明显更多。
	if on[2] <= on[1] {
		t.Errorf("开启后快渠道(100ms)命中 %d，慢渠道(2000ms)命中 %d，快渠道应显著更多",
			on[2], on[1])
	}
	// 但不能独占 —— 慢渠道仍需分担，否则测速与容灾都失去意义。
	if on[1] == 0 {
		t.Error("慢渠道被完全排除：一次异常测活不该永久废掉一个渠道")
	}
}

// TestPickByWeight_无数据渠道不会被饿死 守住"新渠道能拿到流量"这条死锁防线。
func TestPickByWeight_无数据渠道不会被饿死(t *testing.T) {
	channels := []*model.Channel{
		ch(1, 50, 0, 50),   // 快
		ch(2, 50, 0, 0),    // 刚加的，没测过
		ch(3, 50, 0, 3000), // 慢
	}

	const n = 60000
	hits := pickN(t, channels, 1.0, n)
	if hits[2] == 0 {
		t.Errorf("无延迟数据的渠道命中 0 次：新渠道永远拿不到流量 → 永远测不到 → 死锁。命中分布 %v", hits)
	}
}

// TestPickByWeight_相同延迟时退化为权重比例 验证延迟不改变"并列"的处理。
//
// 这条曾经失败过（抽样得 79:20 而非 75:25），根因是延迟得分被乘上了
// 渠道自身权重，导致权重大的一方被双重加权。修正后比例必须精确还原。
func TestPickByWeight_相同延迟时退化为权重比例(t *testing.T) {
	channels := []*model.Channel{
		ch(1, 75, 0, 500),
		ch(2, 25, 0, 500),
	}
	const n = 60000
	hits := pickN(t, channels, 1.0, n)
	want1 := float64(n) * 0.75
	if !ratioNear(float64(hits[1]), want1, want1*0.03) {
		t.Errorf("相同延迟时应按权重 75:25 分配（baseMax 量纲下 eff 比应等于权重比），实际 %v", hits)
	}
}

// TestLatencyScore_延迟翻倍得分必降 守住曲线的单调性。
//
// 这条曾失败过：500ms 以上曾是平坦的常数 0.05，
// 于是 600ms 与 10 秒的渠道同分 —— 而后者慢 16 倍。
// 加速器最该发挥作用的恰恰是"很慢"这个区间，曲线在那里平坦等于功能失效。
func TestLatencyScore_延迟翻倍得分必降(t *testing.T) {
	samples := []int{100, 250, 499, 500, 1000, 2000, 4000, 10000, 30000, 49000, 60000}
	prev := math.Inf(1)
	for _, ms := range samples {
		got := latencyScore(ch(1, 1, 0, ms))
		if got > prev {
			t.Errorf("延迟 %dms 得分 %v，高于更小延迟的 %v —— 曲线必须单调不增",
				ms, got, prev)
		}
		if got <= 0 {
			t.Errorf("延迟 %dms 得分为 %v，必须为正（不得判死刑）", ms, got)
		}
		prev = got
	}
}

// TestLatencyScore_无数据得分低于实测最快渠道 守住"未知不等于最快"。
func TestLatencyScore_无数据得分低于实测最快渠道(t *testing.T) {
	unknown := latencyScore(ch(1, 1, 0, 0))
	fastest := latencyScore(ch(2, 1, 0, latencyMinUsableMS))
	if unknown >= fastest {
		t.Errorf("无数据得分 %v 不应高于实测最快(%dms)的 %v —— 否则新渠道会立刻吃满流量",
			unknown, latencyMinUsableMS, fastest)
	}
	if unknown != latencyUnknownScore {
		t.Errorf("无数据得分 = %v，应为 %v", unknown, latencyUnknownScore)
	}
	// 可信下界（5ms）是真实测量值，应拿满分而非中性分。
	if fastest < 1.0 {
		t.Errorf("实测 %dms 的得分 = %v，应接近满分", latencyMinUsableMS, fastest)
	}
}

// TestPickCandidateForTest_不跨优先级 验证延迟不能覆盖站长的优先级意图。
//
// 优先级同时是成本控制手段："先用便宜的、再用贵的"。
// 若延迟能跨层，就等于让加速器替站长做采购决策。
func TestPickCandidateForTest_不跨优先级(t *testing.T) {
	channels := []*model.Channel{
		ch(1, 1, 10, 3000), // 高优先级（列表在前）、很慢
		ch(2, 100, 5, 10),  // 低优先级、很快、权重大 100 倍
	}
	// candidates 已按优先级降序排列（这是 listCandidates 的约定）。
	if channels[0].Priority <= channels[1].Priority {
		t.Fatalf("测试数据构造错误：应按优先级降序")
	}
	for range 200 {
		got := pickCandidateForTest(channels, 1.0)
		if got.ID != 1 {
			t.Fatalf("选到了低优先级渠道 %d：延迟越过了优先级边界", got.ID)
		}
	}
}

// TestPickCandidateForTest_空输入返回nil 守住调用方"候选耗尽"的判断依据。
func TestPickCandidateForTest_空输入返回nil(t *testing.T) {
	if got := pickCandidateForTest(nil, 1.0); got != nil {
		t.Errorf("空输入应返回 nil，实际 %+v", got)
	}
	if got := pickCandidateForTest([]*model.Channel{}, 0); got != nil {
		t.Errorf("空切片应返回 nil，实际 %+v", got)
	}
}

// TestPickByWeight_单个渠道恒定选中 验证最常见的边界情况。
func TestPickByWeight_单个渠道恒定选中(t *testing.T) {
	only := ch(1, 1, 0, 0)
	for _, ratio := range []float64{0, 0.5, 1.0} {
		for range 10 {
			if got := pickByWeight([]*model.Channel{only}, ratio); got.ID != 1 {
				t.Errorf("ratio=%v 时返回了 %d", ratio, got.ID)
			}
		}
	}
}

// TestEffectiveWeight_不会把渠道压成零 验证下限保护。
//
// 权重被压到 0 意味着该渠道永不被选中，且不产生任何错误 ——
// 那是"悄无声息地废掉一个渠道"。必须保证有效权重 >= 1。
func TestEffectiveWeight_不会把渠道压成零(t *testing.T) {
	cases := []*model.Channel{
		ch(1, 1, 0, 100000), // 权重 1、极慢
		ch(2, 1, 0, 0),      // 权重 1、无数据
		ch(3, 0, 0, 500),    // 权重 0（脏数据，Validate 本应拦住）
		ch(4, 100, 0, 10),
	}
	for _, c := range cases {
		for _, ratio := range []float64{0.5, 1.0} {
			if got := effectiveWeight(c, ratio); got < 1 {
				t.Errorf("渠道 %d 在 ratio=%v 时有效权重 = %d，必须 >= 1",
					c.ID, ratio, got)
			}
		}
	}
}

// TestRankChannelsByLatency_无数据排最后 验证面板不会误导站长。
func TestRankChannelsByLatency_无数据排最后(t *testing.T) {
	in := []*model.Channel{
		ch(3, 1, 0, 0),    // 无数据
		ch(1, 1, 0, 800),  // 中
		ch(2, 1, 0, 100),  // 快
		ch(4, 1, 0, 5000), // 慢
	}
	got := rankChannelsByLatency(in)
	want := []uint64{2, 1, 4, 3}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("第 %d 位 = %d，期望 %d（顺序应按延迟升序、无数据垫底）", i, got[i].ID, id)
		}
	}
	// 同延迟时按 ID 升序，保证多次查询顺序稳定（否则页面不停跳动）。
	same := []*model.Channel{ch(5, 1, 0, 300), ch(2, 1, 0, 300), ch(9, 1, 0, 300)}
	gotSame := rankChannelsByLatency(same)
	for i := 1; i < len(gotSame); i++ {
		if gotSame[i-1].ID > gotSame[i].ID {
			t.Errorf("同延迟时未按 ID 升序：%v", []uint64{gotSame[0].ID, gotSame[1].ID, gotSame[2].ID})
			break
		}
	}
	// 不得修改入参切片顺序（调用方还持有它）。
	if in[0].ID != 3 {
		t.Error("rankChannelsByLatency 原地修改了入参切片")
	}
}

// TestAcceleratorRatio_总开关关闭时子项失效 验证 Effective 的收敛语义。
//
// 站长关了总开关却看到子项仍是勾选状态，会以为"没生效是不是有 bug"。
// 而如果 Load 直接把子项也置 false，界面就与站长填的值对不上。
// 因此 Load 保留原值、Effective 负责收敛 —— 这里守的是后者。
func TestAcceleratorRatio_总开关关闭时子项失效(t *testing.T) {
	s := model.AcceleratorSetting{
		Enabled:          false,
		LatencyRouting:   true,
		LatencyWeight:    100,
		CachePassthrough: true,
	}
	eff := s.Effective()
	if eff.LatencyRouting {
		t.Error("总开关关闭时延迟选路仍生效")
	}
	if eff.CachePassthrough {
		t.Error("总开关关闭时缓存透传仍生效")
	}
	if ratio := s.LatencyWeightRatio(); ratio != 0 {
		t.Errorf("总开关关闭时 ratio = %v，应为 0（纯权重，与改造前一致）", ratio)
	}
	// 原始结构体的子项值必须保持不变（供界面如实展示）。
	if !s.LatencyRouting {
		t.Error("Effective 修改了原结构体：界面会显示与站长填写不一致的状态")
	}
}

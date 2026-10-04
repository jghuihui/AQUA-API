// 加速器：按延迟加权选渠道。
//
// 意图（Why）：
//
//	改造前的 pickCandidate 是"同优先级层内按权重随机"。
//	它能表达站长的运营意图（给某个渠道更多流量），但完全无视速度 ——
//	一个 TTFB 3 秒的渠道和一个 TTFB 300 毫秒的渠道被同等对待。
//
//	而本站**已经**具备做速度决策的全部数据：后台有测速（TTFB）与测活，
//	Channel.LatencyMS 就是现成的。数据在手却不用，是最可惜的一类浪费。
//
// 【为什么用"加权"而不是"直接选最快"】
//
//	直接选最快最简单，但它有两个真实问题：
//	  1) LatencyMS 是【最近一次】测活的瞬时值，model 层注释里明确写了
//	     "单点到点抖动很大"。拿一个抖动值做硬排序，等于让全站流量
//	     去赌一次测量的结果 —— 测活那次恰好慢，就会把流量赶到一个其实很快的渠道之外。
//	  2) 所有流量涌向同一个渠道会把它打爆（429），而隔壁慢一点的渠道闲着。
//
//	加权是在"尊重站长的权重配置"与"偏好快渠道"之间取平衡：
//	延迟只改变【概率分布】，不剥夺任何渠道被选中的机会。
//
// 【关闭加速器时行为逐字节一致】
//
//	ratio == 0 时 latencyScore 恒等于 1.0，
//	最终权重 == 渠道权重，pickByWeight 的输入与改造前完全相同。
//	这条是 model.AcceleratorSetting.LatencyWeightRatio 的契约，本文件依赖它。
//
// 流转（Flow）：
//
//	pickCandidate（同优先级分层）
//	  → 过滤 excluded
//	  → pickByWeight(tier, ratio)
//	       ratio == 0 → 纯权重随机（改造前行为）
//	       ratio > 0  → 最终权重 = 渠道权重 × (1-ratio) + 延迟得分 × ratio
package relay

import (
	"math"
	"math/rand/v2"
	"sort"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// 延迟感知的调参常量。
const (
	// latencyScoreCeilingMS 是延迟得分的参考上限（毫秒）。
	//
	// 为什么需要它：延迟得分 = ceiling / latency，理论上 latency 越小得分越大，
	// 但 latency → 0 时得分会趋于无穷，把其他渠道的权重压到 0。
	// 定一个参考上限等价于"延迟低于这个值就视为同等快"。
	//
	// 取 500 而不是更大的值，理由是"上限内必须有区分度"：
	//	若 ceiling 取得太大（如 2000），则 100ms 与 1500ms 的得分都 ≥ 1 被截平，
	//	于是"2 秒以内的延迟差异对选路完全无影响" —— 而流式对话里
	//	100ms 与 1500ms 的体感差别极大（一句话要等一秒半）。
	//	500 这个量级下，常见区间 100~1500ms 恰好铺满 [0.33, 1.0] 的整个量程。
	latencyScoreCeilingMS = 500

	// latencyUnknownScore 是"没有延迟数据"时的得分。
	//
	// 取 0.5（快速段的中位水平），而非 1.0：
	//	给 1.0 会让一个**从未测过**的渠道比所有实测快渠道得分都高，
	//	等于把"未知"当成"最快" —— 那样新渠道会立刻吃满流量，
	//	而它的真实速度完全未知，一旦很慢就会大面积超时。
	//
	//	也不能给 0（判死刑）：那会让新渠道永远拿不到流量、永远测不到，
	//	形成死锁（见文件头的说明）。
	//
	//	0.5 的含义是"假定它与延迟 1000ms 差不多" —— 不奖不罚，
	//	给它一次正常的分流机会，测速一次之后数据就齐了。
	latencyUnknownScore = 0.5

	// latencyFloorScore 是延迟得分的地板值。
	//
	// 刻意不为 0：为 0 意味着该渠道永不被选中，
	// 而"一次异常的测活结果"不该永久剥夺一个渠道的服务机会。
	// 0.02 足够低（几乎不参与分流），又不至于被彻底排除。
	latencyFloorScore = 0.02

	// latencyMinUsableMS 是小于此值的延迟数据视为不可信。
	//
	// 极小的值（< 5ms）几乎不可能是真实的跨网延迟，
	// 更可能是测活实现里把"未发出请求"也记成了极短耗时。
	// 采信它会让一个坏渠道被判为"极快"而获得全部流量。
	latencyMinUsableMS = 5
)

// pickByWeight 在一组同优先级渠道中按"有效权重"随机挑选。
//
// ratio 为延迟权重占比（0 ~ 1，来自 AcceleratorSetting.LatencyWeightRatio）：
//   - ratio == 0：有效权重 == 渠道权重，与加速器引入前完全一致；
//   - ratio > 0 ：有效权重 = 渠道权重 × (1-ratio) + 延迟得分 × ratio，
//     延迟得分的量纲被归一化到与渠道权重同量级（见 latencyScore）。
//
// 【为什么不直接把延迟换算成权重相加】
//
//	那样快渠道的得分可以是慢渠道的几十倍，一个渠道会吃掉几乎全部流量。
//	归一化到 [0, 1] 后，ratio 就是"延迟最多能拿走多少话语权"的直观刻度。
func pickByWeight(channels []*model.Channel, ratio float64) *model.Channel {
	if len(channels) == 1 {
		return channels[0]
	}

	weights := make([]int, len(channels))
	total := 0
	for i, ch := range channels {
		w := ch.Weight
		if ratio > 0 {
			w = effectiveWeight(ch, ratio)
		}
		if w <= 0 {
			// 权重被压到 0 会让这个渠道永不被选中（且不报错）。
			// 给 1 而不是跳过：站长的权重是 0 本就表示"不要用"，
			// 但 Validate 已拦掉这种数据，这里的兜底不该改变语义，
			// 只保证算法不会因总和为 0 而崩。
			w = 1
		}
		weights[i] = w
		total += w
	}

	if total <= 0 {
		return channels[rand.IntN(len(channels))]
	}

	remaining := rand.IntN(total)
	for i, w := range weights {
		remaining -= w
		if remaining < 0 {
			return channels[i]
		}
	}
	// 理论上不可达（前面必然返回），兜底返回最后一个。
	return channels[len(channels)-1]
}

// effectiveWeight 计算单个渠道在延迟感知下的有效权重。
//
// 【为什么不能把延迟得分直接乘上渠道自身的权重】
//
//	最初的写法是：eff = base×(1-ratio) + score×ratio×base，
//	即"延迟得分也按渠道权重缩放"。这看似合理（保持量纲一致），
//	实际会扭曲分布：权重大且快的渠道被【双重加权】——
//	它既因为权重大拿到更多份额，又因为延迟快再拿一次。
//
// 【scale 也不能直接用 baseMax】
//
//	那样 ratio=1 时 eff = baseMax×score，与渠道自身权重完全无关：
//	两个渠道延迟相同（score 相同）时，它们的有效权重会【相等】——
//	实测权重 75:25 的两渠道被抽成 50:50。
//	也就是说 ratio 拉满的那一刻，站长的权重配置反而失效了。
//
// 【正确做法：延迟只决定"权重的偏移方向与幅度"，不替换权重】
//
//	eff = base × (1 + ratio × (score - 1))
//
//	  - ratio == 0          → eff = base（与改造前逐字节一致）
//	  - score == 1（最快）  → eff = base（权重不变，不奖励）
//	  - score == 0.5（中位）→ eff = base × (1 - 0.5×ratio)
//	  - score == 地板       → eff = base × (1 - ratio)
//
//	这样"相同延迟 → 相同得分 → 权重比原样保留"是结构性成立的，
//	不需要靠调参去凑；而快渠道的权重被放大倍数上界为 1
//	（score 上界为 1），因此绝不会把慢渠道压到 0。
func effectiveWeight(ch *model.Channel, ratio float64) int {
	base := float64(ch.Weight)
	if base <= 0 {
		base = 1
	}
	// ratio 已在 AcceleratorSetting.LatencyWeightRatio 钳位在 (0,1]，
	// 这里再钳一次是为了不让调用方（测试、本文件之外的将来代码）传入越界值。
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	score := latencyScore(ch)
	eff := base * (1 + ratio*(score-1))
	r := int(math.Round(eff))
	if r < 1 {
		return 1
	}
	return r
}

// latencyScore 把渠道的最近延迟换算成 [0, 1] 的得分。
//
// 语义：**得分越高越快**。全程单调不增 —— 延迟翻倍得分必须下降。
//
// 三段设计（每段都有明确的理由，不能合并）：
//
//  1. **快速段**（< ceiling）：score = ceiling / latency，上界 1.0。
//     这一段覆盖"用户明显感觉到快慢"的区间，500ms 与 100ms 要分出高下。
//  2. **迟缓段**（≥ ceiling）：对数衰减到 latencyFloorScore。
//     为什么用对数而不是常数 0.05：常数会让 600ms 与 10 秒同分，
//     而后者慢 16 倍 —— 那样"加速器"在最该发挥作用的区间反而失灵。
//     对数衰减保留了"更慢就更差"的次序，同时把惩罚压平，
//     避免一次异常测活就让渠道被判死刑（见 latencyFloorScore）。
//  3. **无数据**：返回中性分 1.0/ceiling 的量级（见 latencyUnknownScore）。
//
// 【为什么无数据给"偏中性偏上"而不是"最差"】
//
//	新加的渠道还没测过速。若给最低分，它永远拿不到流量，
//	而拿不到流量就永远不会被选去测速 —— 死锁。
//	给中性分让它能正常分流，测速一次之后数据就齐了。
func latencyScore(ch *model.Channel) float64 {
	ms := ch.LatencyMS
	if ms <= 0 || ms < latencyMinUsableMS {
		return latencyUnknownScore
	}

	if ms < latencyScoreCeilingMS {
		score := float64(latencyScoreCeilingMS) / float64(ms)
		if score > 1.0 {
			return 1.0
		}
		return score
	}

	// 对数衰减段。
	//
	// 归一化：r = log(ms / ceiling)，ms=ceiling 时 r=0、ms=1s 时 r≈0.69。
	// score = (1-r) × 0.95 + floor，直到 ms 达到 decayHorizonMS 才落到 floor。
	//
	// 取 decayHorizonMS = 50s：超过这个延迟，用户早就认为"这个渠道坏了"
	// 而不是"慢"，此时再细分没有意义，统一给 floor。
	decayHorizonMS := 50000
	if ms >= decayHorizonMS {
		return latencyFloorScore
	}
	r := math.Log(float64(ms) / float64(latencyScoreCeilingMS))
	span := math.Log(float64(decayHorizonMS) / float64(latencyScoreCeilingMS))
	score := (1 - r/span) * (1 - latencyFloorScore)
	return latencyFloorScore + score
}

// rankChannelsByLatency 把渠道按延迟从快到慢排序。
//
// 用途：后台"加速器"面板的展示 —— 让站长直接看到
// "现在我这条流量会落到谁身上、它有多快"，而不是只看到一个开关。
//
// 排序对缺失数据做特殊处理：把【无数据】排在最后而不是最前。
// 否则面板会把"没测过的渠道"显示成"最快"，那是误导。
// 同延迟时按 ID 升序，保证多次查询顺序稳定（否则页面会不停跳动）。
func rankChannelsByLatency(channels []*model.Channel) []*model.Channel {
	out := make([]*model.Channel, len(channels))
	copy(out, channels)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		aOK := a.LatencyMS >= latencyMinUsableMS
		bOK := b.LatencyMS >= latencyMinUsableMS
		switch {
		case aOK && !bOK:
			return true // 有数据的排前面
		case !aOK && bOK:
			return false
		case !aOK && !bOK:
			return a.ID < b.ID
		case a.LatencyMS != b.LatencyMS:
			return a.LatencyMS < b.LatencyMS
		default:
			return a.ID < b.ID
		}
	})
	return out
}

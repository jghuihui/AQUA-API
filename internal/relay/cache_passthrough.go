// 加速器·上游原生提示词缓存（Prompt Caching）。
//
// 意图（Why）：
//
//	本站的用量解析早已支持缓存命中（openAIUsage.CachedTokens 同时认
//	OpenAI 的 prompt_tokens_details.cached_tokens 与 Anthropic 的
//	cache_read_input_tokens），但请求侧【从未发起过缓存】——
//	既没有告诉上游"这段前缀值得缓存"，也没有统计过命中率。
//	于是 usage 里 CachedTokens 恒为 0。
//
//	站内代码里甚至留着一句实测记录（mailer/template.go）：
//	"不承诺缓存命中折扣（上游实测 cached_tokens 恒为 0）"。
//	加速器要做的，就是把这句话变成过去式。
//
// 【最重要的一条约束：关闭时不改请求体一个字节】
//
//	缓存标识的注入是【对上游请求语义的一次改变】。给不支持缓存的上游
//	发 cache_control，上游可能忽略（无害）也可能直接 400（整个请求失败）。
//	因此本文件的每个注入点都以"加速器开启 && 类型声明支持"为前提，
//	两者缺一即完全跳过 —— 此时请求体与加速器引入前逐字节一致。
//
// 【为什么按"类型"而非按"协议"判断】
//
//	同一个 OpenAI 兼容协议下缓存语义完全不同：
//	  - DeepSeek / OpenAI / 通义：自动缓存，客户端【不需要】任何标记；
//	  - Anthropic：必须显式带 cache_control 断点；
//	  - 大量中转站：透传上游行为，自己不缓存。
//	按协议判断会得出"OpenAI 协议全都一样"的错误结论。
//	所以判据是 channeltype.CapPromptCache 这个能力位。
//
// 【为什么不自动注入 cache_control】
//
//	自动缓存型上游（DeepSeek 等）根本不需要标记，而需要标记的 Anthropic
//	走的是另一套协议适配器。把 cache_control 无差别塞进 OpenAI 兼容请求体，
//	会被上游忽略或报错，却让本站"看起来做了缓存"。
//	因此本文件【只做统计与展示】：告诉站长哪些上游能吃到缓存收益、
//	实际命中率多少 —— 而不做请求体改写。
//
// 流转（Flow）：
//
//	缓存命中 → usage.CachedTokens（既有解析）
//	  → UsageLog 落库（既有写入）
//	  → 加速器面板聚合展示（本文件提供聚合口径）
//
// 扩展（Extend）：
//
//	要真正给 Anthropic 注入 cache_control 时：
//	  1) 给对应类型加 CapPromptCache（已有）；
//	  2) 在 upstream_anthropic.go 的请求组装处加断点，
//	     且必须走 buildUpstreamRequest 的参数而不是事后改 body
//	     —— 事后改会破坏 SigV4 等按 body 摘要签名的协议。
package relay

import (
	"context"
	"sort"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/channeltype"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// cacheRateWindow 是缓存命中率统计的时间窗口。
//
// 取 24 小时与用量统计、渠道健康等既有口径一致，
// 避免站长在两个页面看到"同一天却两个不同的命中率"。
const cacheRateWindow = 24 * time.Hour

// CacheCapabilityOf 返回某渠道类型是否声明支持提示词缓存。
//
// typeKey 为空时返回 false —— 那意味着"未指定类型的渠道"（历史数据）。
// 对这类渠道【不猜】：猜错的后果是给上游发了它不认识的字段。
func CacheCapabilityOf(typeKey string) bool {
	if typeKey == "" {
		return false
	}
	t, ok := channeltype.Find(typeKey)
	if !ok {
		return false
	}
	return t.Caps.Has(channeltype.CapPromptCache)
}

// ListCacheCapableChannels 列出声明支持提示词缓存的渠道。
//
// 用途：加速器面板的"哪些上游能吃到缓存收益"。
//
// 返回值按延迟升序（同 rankChannelsByLatency），因为对站长而言
// "又快又能缓存"的渠道价值最高。
func (r *Relay) ListCacheCapableChannels(ctx context.Context, group string) ([]*model.Channel, error) {
	candidates, err := r.listCandidates(ctx, group, "")
	if err != nil {
		return nil, err
	}
	out := make([]*model.Channel, 0, len(candidates))
	for _, ch := range candidates {
		if CacheCapabilityOf(ch.TypeKey) {
			out = append(out, ch)
		}
	}
	return rankChannelsByLatency(out), nil
}

// CacheInsight 是缓存收益的汇总视图（加速器面板用）。
//
// 【为什么"潜在可缓存渠道数"要和"实际命中率"并列】
//
//	只给命中率，站长看到 0% 会以为功能坏了；
//	只给可缓存渠道数，又会以为"这些渠道都在吃缓存"。
//	两个数字放在一起，才能区分"没有可用上游"与"有上游但没命中"。
type CacheInsight struct {
	// Enabled 加速器缓存开关当前是否开启。
	Enabled bool
	// PromptTokens 窗口内的提示词 token 总数。
	PromptTokens int64
	// CachedTokens 窗口内命中缓存的 token 数。
	CachedTokens int64
	// HitRate 是命中率（0 ~ 1）。分母为 0 时返回 0。
	HitRate float64
	// CapableChannels 是声明支持缓存的启用渠道数。
	CapableChannels int
	// SampleCount 是窗口内参与统计的调用次数；用于判断样本是否够大。
	SampleCount int64
}

// cacheInsightRepo 是 CacheInsight 聚合所需的最小仓储接口。
//
// 刻意只声明 Summary 一个方法而不是直接收 model.UsageLogRepository：
// 这样聚合逻辑可以被一个纯内存的假实现测到，
// 不必为了测一段算术而去建真数据库。
//
// 【为什么不自己写一个 SumCachedTokens】
//
//	UsageSummary 里【已经】有 PromptTokens、CachedTokens、FirstTokenSumMS
//	这些字段，且 store 层已经实现了正确的聚合（含"非流式请求不计入 TTFB 样本"
//	这类必须靠 SQL 才能做对的细节）。另写一个 sum 方法等于把同一份口径
//	实现两遍，两遍迟早会不一致。
type cacheInsightRepo interface {
	// Summary 汇总窗口内的用量。
	Summary(ctx context.Context, q model.UsageLogQuery) (*model.UsageSummary, error)
}

// CacheInsightFor 聚合缓存收益。
//
// 仓储为 nil 或窗口内无记录时返回各项为零的结构体（ok）——
// 面板据此显示"还没有数据"，而不是报错或显示 NaN。
func (r *Relay) CacheInsightFor(ctx context.Context, repo cacheInsightRepo, group string) (CacheInsight, error) {
	accel := r.accelerator()
	out := CacheInsight{Enabled: accel.Enabled && accel.CachePassthrough}

	if repo != nil {
		// 刻意【不按分组过滤】：usage_logs 表不落分组（见 model.ReconcileDimGroup
		// 的注释"实现侧按 token 关联"），UsageLogQuery 因此也没有 Group 字段。
		// 而"全站命中率"恰好是站长最想看的数字 —— 按分组拆开反而更难判断
		// 加速器有没有起作用。
		since := time.Now().Add(-cacheRateWindow)
		sum, err := repo.Summary(ctx, model.UsageLogQuery{Since: &since})
		if err != nil {
			return out, err
		}
		if sum != nil {
			out.PromptTokens = sum.PromptTokens
			out.CachedTokens = sum.CachedTokens
			out.SampleCount = sum.Requests
			if sum.PromptTokens > 0 {
				out.HitRate = float64(sum.CachedTokens) / float64(sum.PromptTokens)
			}
		}
	}

	if group == "" {
		group = r.group
	}
	channels, err := r.ListCacheCapableChannels(ctx, group)
	if err != nil {
		return out, err
	}
	out.CapableChannels = len(channels)
	return out, nil
}

// LatencyInsight 是延迟选路的汇总视图（加速器面板用）。
type LatencyInsight struct {
	// Enabled 延迟选路当前是否生效。
	Enabled bool
	// LatencyRatio 是延迟权重占比（0 ~ 1）。
	LatencyRatio float64
	// Channels 是参与同层竞争的渠道数（全部候选）。
	Channels int
	// Measured 是其中有可信延迟数据的渠道数。
	//
	// 与 Channels 的差值就是"还没测过的渠道数"——
	// 站长需要知道它，否则会以为加速器已经对全部渠道生效。
	Measured int
	// FastestMs 是最快渠道的延迟（毫秒）；0 表示无可信数据。
	FastestMs int
	// SlowestMs 是最慢渠道的延迟（毫秒）；0 表示无可信数据。
	SlowestMs int
	// P50Ms / P95Ms 是延迟中位数与 95 分位（毫秒）。
	//
	// 用分位数而不是平均值：平均会被一两个异常慢的渠道拉高，
	// 而站长关心的是"多数请求有多快"。
	P50Ms int
	P95Ms int
	// AvgTTFBMS 是全站近 24 小时的平均首 token 延迟（毫秒）。
	//
	// 它与上面的"渠道延迟分布"是两个不同的东西：
	//   - 渠道延迟来自后台测活，是【站点主动测量】的结果；
	//   - AvgTTFBMS 来自真实调用日志，是【用户实际体验】的结果。
	// 两者不一致时（例如测活都很快但用户 TTFB 很高），
	// 说明瓶颈在本站而不在上游 —— 这正是加速器面板该帮站长发现的。
	AvgTTFBMS int
	// TTFBSamples 是 TTFB 的样本数（不含非流式请求）。
	TTFBSamples int64
}

// LatencyInsightFor 聚合延迟选路的现状。
func (r *Relay) LatencyInsightFor(
	ctx context.Context,
	repo cacheInsightRepo,
	group string,
) (LatencyInsight, error) {
	accel := r.accelerator()
	out := LatencyInsight{
		Enabled:      accel.Enabled && accel.LatencyRouting,
		LatencyRatio: accel.LatencyWeightRatio(),
	}
	if group == "" {
		group = r.group
	}
	channels, err := r.listCandidates(ctx, group, "")
	if err != nil {
		return out, err
	}
	out.Channels = len(channels)

	ordered := make([]int, 0, len(channels))
	for _, ch := range channels {
		if ch.LatencyMS >= latencyMinUsableMS {
			ordered = append(ordered, ch.LatencyMS)
		}
	}
	out.Measured = len(ordered)
	if len(ordered) > 0 {
		sort.Ints(ordered)
		out.FastestMs = ordered[0]
		out.SlowestMs = ordered[len(ordered)-1]
		out.P50Ms = percentile(ordered, 50)
		out.P95Ms = percentile(ordered, 95)
	}

	if repo != nil {
		// 同 CacheInsightFor：不按分组过滤，理由见那里的注释。
		since := time.Now().Add(-cacheRateWindow)
		sum, err := repo.Summary(ctx, model.UsageLogQuery{Since: &since})
		if err != nil {
			return out, err
		}
		if sum != nil && sum.FirstTokenSamples > 0 {
			// 刻意用 FirstTokenSamples 而非 Requests 作分母：
			// 非流式请求不产生 TTFB（值为 0），算进去会把平均值压成假数字。
			out.AvgTTFBMS = int(sum.FirstTokenSumMS / sum.FirstTokenSamples)
			out.TTFBSamples = sum.FirstTokenSamples
		}
	}
	return out, nil
}

// percentile 返回已升序切片中的第 p 分位数（就近取整）。
//
// 样本量很小时（如只有 2 个渠道）它退化为"取较靠后的那个"，
// 这是可接受的：样本不足时任何统计量都没有意义，
// 面板会同时显示 Measured 让站长自行判断。
func percentile(sorted []int, p int) int {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// 能力位标注现状（2026-10-04）。
//
// 目前标了 CapPromptCache 的三个类型，语义都是【自动缓存】：
//
//	openai     — 前缀 ≥1024 token 自动命中，命中部分计费更低
//	deepseek   — 上下文缓存全自动，无需客户端标记
//	dashscope  — compatible-mode 下由服务端自动缓存
//
// 【为什么没有给它们注入任何请求体字段】
//	这三家的缓存都是"自动"的：客户端不需要做任何标记，重复前缀直接命中。
//	注入 cache_control 之类的字段对它们毫无作用，只会让请求体变脏。
//
// 【Anthropic 为什么没标】
//	它确实需要显式 cache_control 断点，但它走的是 ProtocolAnthropic
//	（另一套请求组装）。要支持必须在 upstream_anthropic.go 里加，
//	且必须走 buildUpstreamRequest 的参数而不是事后改 body ——
//	事后改会破坏按 body 摘要签名的协议。这是一次独立的改动，本轮不做。
//
// 【对站长的实际影响】
//	打开"缓存透传"开关本身【不会改变发给上游的请求体】。
//	它的作用是让加速器面板开始统计命中率 —— 而这份统计正是判断
//	"我这边的重复前缀有没有真的被上游缓存"的唯一依据。
//	换句话说：这一项先解决"看不见"，再解决"做不到"。

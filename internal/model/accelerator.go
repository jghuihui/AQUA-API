// 加速器配置：让站长用一组开关控制"转发有多快"，而不是去翻代码。
//
// 意图（Why）：
//
//	本站原有的速度相关能力是【散落且互不联动】的：
//	测速能测出 TTFB、选路却完全不看它；连接池参数写死在代码里、
//	想调只能改代码重新构建；上游原生缓存（prompt caching）能省下
//	大段重复前缀的钱与时间，但请求里根本没带缓存标识。
//
//	把这些收进一个配置对象后，站长的诉求"让 API 更快"才有落点，
//	而且每一项都能单独关掉 —— 关掉后行为必须与改造前完全一致。
//
// 【最重要的一条设计约束：关闭即等同改造前】
//
//	加速器的任何一项都不得改变请求的语义。任何一项关闭时，
//	发出的请求体、选中的渠道、计费结果都必须与改造前逐字节一致。
//	否则"打开加速"这个动作本身就成了一次行为变更 ——
//	而站长无法预期"我只是想让它快点，结果模型选错了"。
//	这条约束是本文件所有取舍的出发点。
//
// 流转（Flow）：
//
//	设置表（布尔/整数键）
//	  → AcceleratorSetting.Load（容错解析 + 钳位）
//	  → relay 用它决定选路策略、缓存透传、连接池参数
//
// 扩展（Extend）：
//
//	加一项加速能力时：先在此加字段与 SettingKey，再在 relay 里读它。
//	必须同时回答两个问题——"关闭时是否真的等价于改造前"，
//	以及"打开后效果如何被观测到"（没有观测手段的能力等于没有）。
package model

import (
	"strconv"
	"strings"
)

// 加速器相关的设置键。
//
// 与其他设置键集中定义在 setting.go 不同，这里单独成块：
// 它们服务于同一个子系统（转发性能），放在一起更容易看出
// "改这一组键会同时影响哪几处行为"，也更容易在新增时发现遗漏。
const (
	// SettingKeyAcceleratorEnabled 加速器总开关（"true" / "false"）。
	//
	// 关掉时下面所有子项一并失效 —— 站长需要一个"全部回到原样"的
	// 单一动作，而不只是逐项去关。
	SettingKeyAcceleratorEnabled = "accelerator_enabled"

	// SettingKeyAcceleratorLatencyRouting 是否按延迟加权选渠道（"true" / "false"）。
	SettingKeyAcceleratorLatencyRouting = "accelerator_latency_routing"
	// SettingKeyAcceleratorLatencyWeight 延迟在选路权重中的占比（0 ~ 100）。
	//
	// 0 表示纯按渠道权重（等价于关闭）；100 表示完全按延迟排序，权重只用于并列时。
	// 中间值按线性插值：权重占比越高，快渠道拿到的流量越多。
	SettingKeyAcceleratorLatencyWeight = "accelerator_latency_weight"

	// SettingKeyAcceleratorCachePassthrough 是否透传上游原生缓存标识（"true" / "false"）。
	//
	// 只对声明了 CacheSupport 的渠道类型生效；其余类型请求体不变。
	SettingKeyAcceleratorCachePassthrough = "accelerator_cache_passthrough"
	// SettingKeyAcceleratorCacheMinTokens 触发上游缓存所需的最小提示词 token 数。
	//
	// 低于这个长度的请求即使带上缓存标识也不会命中，
	// 带了只是白白多传一段字段。默认 1024（与主流上游的最小可缓存长度一致）。
	SettingKeyAcceleratorCacheMinTokens = "accelerator_cache_min_tokens"

	// SettingKeyAcceleratorMaxIdleConnsPerHost 每个上游主机的空闲连接上限。
	//
	// 偏小会让高并发下频繁重建 TCP + TLS 握手（实测每次几十到几百毫秒），
	// 这是"接口慢"最容易被误判成"上游慢"的原因。
	SettingKeyAcceleratorMaxIdleConnsPerHost = "accelerator_max_idle_conns_per_host"
	// SettingKeyAcceleratorIdleConnTimeoutMS 空闲连接回收时间（毫秒）。
	//
	// 太短会频繁丢连接（等于关掉了复用），太长会占着上游连接额度。
	SettingKeyAcceleratorIdleConnTimeoutMS = "accelerator_idle_conn_timeout_ms"
	// SettingKeyAcceleratorExpect100Continue 是否对上游请求启用 Expect: 100-continue。
	//
	// 作用是"先问上游要不要收正文"，避免把大请求体发给一个即将返回
	// 429/403 的上游。默认关闭 —— 它会给每个请求多加一个 RTT，
	// 对小请求是净损失。
	SettingKeyAcceleratorExpect100Continue = "accelerator_expect_100_continue"
)

// 加速器参数边界。
//
// 全部有硬边界且在 Load 时钳位：这些值直接进 http.Transport 与选路算法，
// 一个手滑的 100000 会让进程对上万个上游连接，那是能打垮上游也能打垮自己的量级。
const (
	// LatencyWeightMax 是延迟权重的上限（百分比）。
	LatencyWeightMax = 100
	// LatencyWeightMin 是延迟权重的下限。
	//
	// 不给 0：0% 会让延迟完全不影响选路，等于关掉了这项加速，
	// 与其让站长调成 0 还以为"已经关了但好像没区别"，不如直接拒绝。
	LatencyWeightMin = 1
	// LatencyWeightDefault 是默认延迟权重（50）。
	//
	// 取 50 而不是 100：延迟数据是"最近一次测活"的瞬时值，
	// 注释里写明过单点抖动很大。权重压过人工配置的流量分配（权重）
	// 会让运维的调权失效，因此默认让延迟只能"倾斜"而不能"独断"。
	LatencyWeightDefault = 50

	// MaxIdleConnsPerHostMax 是每主机空闲连接上限的硬上限。
	MaxIdleConnsPerHostMax = 512
	// MaxIdleConnsPerHostDefault 是默认每主机空闲连接上限。
	//
	// 比改造前的 20 提到 64：20 在一个网关转发多用户的场景下很容易打满，
	// 打满后每个新请求都要重新握手。而真正的并发瓶颈在服务端，
	// 空闲连接只是"可复用"的备用池，到不了 512 这种量级。
	MaxIdleConnsPerHostDefault = 64
	// MaxIdleConnsPerHostMin 是下限，低于 2 等于关掉了复用。
	MaxIdleConnsPerHostMin = 2

	// IdleConnTimeoutMinMS 是空闲连接回收时间的下限（毫秒）。
	IdleConnTimeoutMinMS = 5_000
	// IdleConnTimeoutMaxMS 是上限（毫秒）。
	IdleConnTimeoutMaxMS = 600_000
	// IdleConnTimeoutDefaultMS 是默认 90 秒（与改造前一致，不引入行为变化）。
	IdleConnTimeoutDefaultMS = 90_000

	// CacheMinTokensDefault 是触发上游缓存的最小提示词长度。
	CacheMinTokensDefault = 1024
	// CacheMinTokensMax 是上限，防止误填成"所有请求都带缓存标识"。
	CacheMinTokensMax = 1_000_000
)

// AcceleratorSetting 是加速器的全部可调项。
//
// 所有字段都是"最终生效值"：Load 已完成钳位与默认值填充，
// 下游不必再做范围判断。转发层拿到它时可以直接用。
type AcceleratorSetting struct {
	// Enabled 是总开关。为 false 时 LatencyRouting 与 CachePassthrough
	// 一律按 false 处理（见 Effective）。
	Enabled bool `json:"enabled"`

	// LatencyRouting 是否按延迟加权选渠道。
	LatencyRouting bool `json:"latency_routing"`
	// LatencyWeight 是延迟占比（0 ~ 100，见 LatencyWeightMin/Max）。
	LatencyWeight int `json:"latency_weight"`

	// CachePassthrough 是否透传上游原生缓存标识。
	CachePassthrough bool `json:"cache_passthrough"`
	// CacheMinTokens 是触发缓存所需的最小提示词 token 数。
	CacheMinTokens int `json:"cache_min_tokens"`

	// MaxIdleConnsPerHost 是每上游主机的空闲连接上限。
	MaxIdleConnsPerHost int `json:"max_idle_conns_per_host"`
	// IdleConnTimeoutMS 是空闲连接回收毫秒数。
	IdleConnTimeoutMS int `json:"idle_conn_timeout_ms"`
	// Expect100Continue 是否启用 Expect: 100-continue。
	Expect100Continue bool `json:"expect_100_continue"`
}

// DefaultAcceleratorSetting 返回"关闭状态"下的配置。
//
// 关键：总开关默认为 **false**。
//
// 加速器是一次行为变更（哪怕方向是对的），默认开启等于让所有站长
// 在不知情的情况下被施加了新行为。要让加速生效必须显式打开。
func DefaultAcceleratorSetting() AcceleratorSetting {
	return AcceleratorSetting{
		Enabled:             false,
		LatencyRouting:      false,
		LatencyWeight:       LatencyWeightDefault,
		CachePassthrough:    false,
		CacheMinTokens:      CacheMinTokensDefault,
		MaxIdleConnsPerHost: MaxIdleConnsPerHostDefault,
		IdleConnTimeoutMS:   IdleConnTimeoutDefaultMS,
		Expect100Continue:   false,
	}
}

// Effective 返回真正生效的配置：总开关关掉时，子项一律按关闭处理。
//
// 【为什么不直接让 Load 把子项也关掉】
//
//	那样会导致"总开关关了、但后台面板上子项还是打开的"，
//	站长改总开关后看到的界面与实际行为对不上，排查时极难判断。
//	把"总开关 → 子项"的收敛放在 Effective 里，Load 保留站长填的原值，
//	界面就能如实显示"你勾了这些，但总开关是关的"。
func (a AcceleratorSetting) Effective() AcceleratorSetting {
	if a.Enabled {
		return a
	}
	out := a
	out.LatencyRouting = false
	out.CachePassthrough = false
	out.Expect100Continue = false
	return out
}

// LatencyWeightRatio 返回延迟权重占最终权重的比例（0 ~ 1）。
//
// 用于线性插值：最终权重 = 渠道权重 × (1-r) + 延迟得分 × r。
// 返回值已考虑总开关 —— 关闭时为 0，即完全退化为纯渠道权重。
func (a AcceleratorSetting) LatencyWeightRatio() float64 {
	if !a.Enabled || !a.LatencyRouting {
		return 0
	}
	w := a.LatencyWeight
	if w < LatencyWeightMin {
		w = LatencyWeightMin
	}
	if w > LatencyWeightMax {
		w = LatencyWeightMax
	}
	return float64(w) / float64(LatencyWeightMax)
}

// Validate 校验加速器设置的输入值。
//
// 与"Load 时钳位"并存但用途不同：
//   - Load 钳位是"读到脏数据时别崩"（比如有人手改了 settings 表）；
//   - Validate 是"站长提交时告诉他填错了"（HTTP 400）。
//
// 只有 Validate 会拒绝，Load 永远不返回错误 —— 加速器配错不该让站点起不来。
func (a AcceleratorSetting) Validate() error {
	if a.LatencyWeight < LatencyWeightMin || a.LatencyWeight > LatencyWeightMax {
		return &AcceleratorError{
			Field:   "latency_weight",
			Message: "延迟权重需在 " + strconv.Itoa(LatencyWeightMin) + " ~ " + strconv.Itoa(LatencyWeightMax) + " 之间",
		}
	}
	if a.CacheMinTokens < 0 || a.CacheMinTokens > CacheMinTokensMax {
		return &AcceleratorError{
			Field:   "cache_min_tokens",
			Message: "缓存触发长度需在 0 ~ " + strconv.Itoa(CacheMinTokensMax) + " 之间",
		}
	}
	if a.MaxIdleConnsPerHost < MaxIdleConnsPerHostMin || a.MaxIdleConnsPerHost > MaxIdleConnsPerHostMax {
		return &AcceleratorError{
			Field: "max_idle_conns_per_host",
			Message: "每主机空闲连接数需在 " + strconv.Itoa(MaxIdleConnsPerHostMin) +
				" ~ " + strconv.Itoa(MaxIdleConnsPerHostMax) + " 之间",
		}
	}
	if a.IdleConnTimeoutMS < IdleConnTimeoutMinMS || a.IdleConnTimeoutMS > IdleConnTimeoutMaxMS {
		return &AcceleratorError{
			Field: "idle_conn_timeout_ms",
			Message: "空闲连接回收时间需在 " + strconv.Itoa(IdleConnTimeoutMinMS) +
				" ~ " + strconv.Itoa(IdleConnTimeoutMaxMS) + " 毫秒之间",
		}
	}
	return nil
}

// AcceleratorError 是加速器设置的字段级错误。
//
// 做成带 Field 的类型而不是纯文本：后台表单要能据此把红框标在对应输入框上，
// 而"某个参数填错了"这种信息不该退化成一段让人自己找的文案。
type AcceleratorError struct {
	Field   string
	Message string
}

func (e *AcceleratorError) Error() string { return e.Message }

// LoadAcceleratorSetting 从设置表读出加速器配置。
//
// 刻意做成"永不返回错误"：读不到、格式错、超范围，一律退回默认值。
// 加速器是可选增强功能，它配错绝不该让整个站点打不开 ——
// 那会变成一个"关掉加速反而更安全"的荒唐局面。
//
// values 为空 map 时返回默认值，便于调用方无条件使用。
func LoadAcceleratorSetting(values map[string]string) AcceleratorSetting {
	out := DefaultAcceleratorSetting()
	if values == nil {
		return out
	}

	out.Enabled = parseBoolSetting(values, SettingKeyAcceleratorEnabled, false)
	out.LatencyRouting = parseBoolSetting(values, SettingKeyAcceleratorLatencyRouting, false)
	out.LatencyWeight = clampInt(parseIntSetting(values, SettingKeyAcceleratorLatencyWeight,
		LatencyWeightDefault), LatencyWeightMin, LatencyWeightMax)
	out.CachePassthrough = parseBoolSetting(values, SettingKeyAcceleratorCachePassthrough, false)
	out.CacheMinTokens = clampInt(parseIntSetting(values, SettingKeyAcceleratorCacheMinTokens,
		CacheMinTokensDefault), 0, CacheMinTokensMax)
	out.MaxIdleConnsPerHost = clampInt(parseIntSetting(values, SettingKeyAcceleratorMaxIdleConnsPerHost,
		MaxIdleConnsPerHostDefault), MaxIdleConnsPerHostMin, MaxIdleConnsPerHostMax)
	out.IdleConnTimeoutMS = clampInt(parseIntSetting(values, SettingKeyAcceleratorIdleConnTimeoutMS,
		IdleConnTimeoutDefaultMS), IdleConnTimeoutMinMS, IdleConnTimeoutMaxMS)
	out.Expect100Continue = parseBoolSetting(values, SettingKeyAcceleratorExpect100Continue, false)
	return out
}

// AcceleratorValuesToMap 把加速器配置写成设置表的值映射。
//
// 与其他设置页的批量保存走同一个 SetMany，因此：
// 布尔一律写成 "true"/"false"（不是 "1"/"0"），
// 整数不带单位后缀 —— 保持 parseIntSetting 能原样读回来。
func AcceleratorValuesToMap(a AcceleratorSetting) map[string]string {
	return map[string]string{
		SettingKeyAcceleratorEnabled:             boolSettingString(a.Enabled),
		SettingKeyAcceleratorLatencyRouting:      boolSettingString(a.LatencyRouting),
		SettingKeyAcceleratorLatencyWeight:       strconv.Itoa(a.LatencyWeight),
		SettingKeyAcceleratorCachePassthrough:    boolSettingString(a.CachePassthrough),
		SettingKeyAcceleratorCacheMinTokens:      strconv.Itoa(a.CacheMinTokens),
		SettingKeyAcceleratorMaxIdleConnsPerHost: strconv.Itoa(a.MaxIdleConnsPerHost),
		SettingKeyAcceleratorIdleConnTimeoutMS:   strconv.Itoa(a.IdleConnTimeoutMS),
		SettingKeyAcceleratorExpect100Continue:   boolSettingString(a.Expect100Continue),
	}
}

// parseBoolSetting 解析布尔设置。
//
// 容忍多种写法：后台表单、env 覆盖、手工改库都可能给出 "1"/"yes"/"on"。
// 但只有明确的假值才返回 false —— 无法识别时返回默认值而不是 false，
// 因为"读不出"不等于"关闭"。
func parseBoolSetting(values map[string]string, key string, def bool) bool {
	raw, ok := values[key]
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on", "是", "开":
		return true
	case "0", "false", "no", "off", "否", "关":
		return false
	default:
		return def
	}
}

func boolSettingString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func parseIntSetting(values map[string]string, key string, def int) int {
	raw, ok := values[key]
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return n
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

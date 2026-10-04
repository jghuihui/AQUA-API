/**
 * 加速器接口（配置读写 + 效果视图）。
 *
 * 意图（Why）：
 *   站长说"让 API 更快"，但"快"由三件互不相干的事决定：
 *     1) 选路是否挑快的 —— 此前 pickCandidate 是纯权重随机，
 *        Channel.LatencyMS 明明在手却从不参与决策；
 *     2) 连接是否被复用 —— 连接池参数写死在代码里，站长改不了；
 *     3) 上游缓存有没有命中 —— 站内代码里那句
 *        「不承诺缓存命中折扣（上游实测 cached_tokens 恒为 0）」
 *        就是这块缺失的证据。
 *   三件事此前都没有开关也没有反馈，站长只能凭感觉猜。
 *   本文件把它们收成一个面板的配置与效果数据出口。
 *
 * 【为什么 GET 必须同时返回效果数据】
 *   一个只有开关没有数字的面板，站长无法判断"开了之后有没有变快"，
 *   也无法判断"5 万毫秒的 P95 是上游慢还是本站慢"。
 *   配置可以照着抄，效果只能看。
 *
 * 【为什么 PUT 的字段全是可选的】
 *   前端传【只有改动的那几项】，而不是回传整份表单。
 *   理由：站长很可能只想点开总开关，若前端把整份表单回传，
 *   那些他上次精心调过的参数会因为一次无关的点击被打回默认。
 *   这里用 Partial 表达同一件事 —— 类型层面就把"整份覆盖"变成需要刻意为之。
 *
 * 流转（Flow）：
 *   AcceleratorPage → fetchAccelerator / saveAccelerator
 *                   → /api/admin/accelerator
 *
 * 扩展（Extend）：
 *   后端新增参数时，本文件补类型；页面只依赖这里的类型，不依赖字段拼装。
 *   参数边界由后端下发（见 AcceleratorBounds），前端不硬编码 ——
 *   两边各写一份边界，站长会遇到"改任何次数都保存不成功"。
 */
import { api } from './client'

/** 加速器配置（与后端 model.AcceleratorSetting 一一对应）。 */
export interface AcceleratorSetting {
  /** 总开关。默认 false：默认开启等于给所有站长施加未经同意的行为变更 */
  enabled: boolean

  /** 延迟感知选路：让实测更快的渠道被选中的概率更高 */
  latency_routing: boolean
  /**
   * 延迟在选路中的权重（1~100）。
   *
   * 语义：不是"选最快的那个"，而是"按延迟调整概率分布"，
   * 任何一个渠道都不会因此失去被选中的机会。
   * 调高会让流量明显向快渠道倾斜，但也会削弱站长手工配的权重。
   */
  latency_weight: number

  /** 上游缓存透传：把缓存能力位与命中情况暴露出来 */
  cache_passthrough: boolean
  /** 低于该 token 数的请求不计入缓存统计（太短的上游通常不缓存） */
  cache_min_tokens: number

  /** 每个上游主机保留的空闲连接数上限（连接复用） */
  max_idle_conns_per_host: number
  /** 空闲连接回收时间（毫秒） */
  idle_conn_timeout_ms: number
  /** 是否发送 Expect: 100-continue（仅在启用时发送，否则每个请求多等一次却什么都不省） */
  expect_100_continue: boolean
}

/** 延迟选路的效果视图。 */
export interface AcceleratorLatencyInsight {
  /** 是否真的在按延迟选路（总开关 + 子项都开且权重 > 0） */
  enabled: boolean
  /** 实际生效的延迟权重（0 = 未参与选路） */
  latency_ratio: number
  /** 参与统计的渠道数 */
  channels: number
  /** 其中有延迟实测值的渠道数；与 channels 的差额是"从未测过" */
  measured: number
  fastest_ms: number
  slowest_ms: number
  p50_ms: number
  p95_ms: number
  /**
   * 真实调用的首字节耗时均值（来自 usage_logs，反映用户实际体验）。
   *
   * 刻意与上面的"渠道测活延迟"分开放：两个数字差得远，
   * 说明瓶颈在本站而不在上游 —— 这时候去调上游权重是白费力气。
   */
  avg_ttfb_ms: number
  /** TTFB 样本数；为 0 时上面的均值无意义，页面应显示「暂无数据」 */
  ttfb_samples: number
}

/** 上游缓存的效果视图。 */
export interface AcceleratorCacheInsight {
  enabled: boolean
  /** 命中率 0~1；无样本时为 0（绝不显示成 0% 之外的任何数） */
  hit_rate: number
  prompt_tokens: number
  cached_tokens: number
  /** 当前有多少渠道被标注为支持自动缓存 */
  capable_channels: number
  /** 缓存统计的样本数（近期有调用的渠道数） */
  sample_count: number
}

/** 参数边界，由后端下发（前端不硬编码）。 */
export interface AcceleratorBounds {
  latency_weight_min: number
  latency_weight_max: number
  idle_per_host_min: number
  idle_per_host_max: number
  idle_timeout_min_ms: number
  idle_timeout_max_ms: number
  cache_min_tokens_max: number
}

/** GET /api/admin/accelerator 的响应体。 */
export interface AcceleratorOverview {
  setting: AcceleratorSetting
  latency: AcceleratorLatencyInsight
  cache: AcceleratorCacheInsight
  bounds: AcceleratorBounds
}

/** PUT 的请求体：只传要改的项。 */
export type AcceleratorPatch = Partial<AcceleratorSetting>

/** PUT 的响应体：后端回读数据库后返回，保证界面显示的就是真实生效值。 */
export interface AcceleratorSaveResult {
  setting: AcceleratorSetting
}

/** GET /api/admin/accelerator：读取加速器配置与当前效果。 */
export function fetchAccelerator(): Promise<AcceleratorOverview> {
  return api.get<AcceleratorOverview>('/admin/accelerator')
}

/**
 * PUT /api/admin/accelerator：保存配置并热更新转发引擎（无需重启）。
 *
 * 越界值会抛 ApiError，其 `.field` 指出是哪个参数 ——
 * 页面据此把红框标在对应输入框上，而不是弹一段笼统提示。
 *
 * @param patch 只包含要修改的字段；不传的项保持数据库里的原值
 */
export function saveAccelerator(patch: AcceleratorPatch): Promise<AcceleratorSaveResult> {
  return api.put<AcceleratorSaveResult>('/admin/accelerator', patch)
}

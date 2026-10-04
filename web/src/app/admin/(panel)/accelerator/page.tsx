/** 管理后台：加速器面板（/admin/accelerator）。
 *
 * 意图（Why）：
 *   站长能做的事此前只有"改完重启，看看有没有变快"—— 而且多半看不出变化。
 *   加速器把三件互不相干的事（选路 / 连接复用 / 上游缓存）收进一个页面，
 *   并且【在开关旁边就给出效果数字】，而不是让人猜。
 *
 * 页面结构刻意做成"结论在上、参数在下"：
 *   顶部四个数字回答"现在快不快、瓶颈在哪"，
 *   下面的参数区回答"要改成什么"。
 *   反过来排（参数在上、数字在下）会让人在还没看到效果时就先去动参数。
 *
 * 三处关键的诚实性处理（Why 这几条比样式重要）：
 *   1) 【测活延迟】与【真实首字节耗时】分开展示，不合成一个数字。
 *      两者差距大说明瓶颈在本站（转发/排队），而不在上游——
 *      这时候去调上游权重是白费力气。合成一个数会把这个线索抹掉。
 *   2) 【无数据】永远显示"暂无数据"，绝不显示 0。
 *      0 会被读成"延迟是 0 毫秒"或"命中率是 0%"，
 *      而真实含义是"这个部署没有可用样本"。
 *   3) 【没开加速器】时参数区仍可编辑但整体降透明度，
 *      而不是 disabled —— 站长需要能"先配好再开"，
 *      灰掉输入框会让人以为必须先开总开关才能碰这些参数（而总开关就在上面一行）。
 *
 * 流转（Flow）：
 *   本页 → fetchAccelerator（配置 + 效果 + 参数边界）
 *        → 改任一项 → saveAccelerator（只提交改动的那几项）→ 回读结果覆盖本地
 *
 * 扩展（Extend）：
 *   后端加参数时，本页只依赖 @/api/accelerator 的类型。
 *   参数边界由后端下发（bounds），本页不硬编码上下限。
 */
'use client'

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import {
  fetchAccelerator,
  saveAccelerator,
  type AcceleratorBounds,
  type AcceleratorOverview,
  type AcceleratorSetting,
} from '@/api/accelerator'
import { ApiError } from '@/api/client'
import { Button } from '@/components/ui/Button'
import { Badge, Card, PageHeader, Skeleton, StatCard } from '@/components/ui/Display'
import { Field, Input, Switch } from '@/components/ui/Form'
import { useToast } from '@/lib/toast/toast-context'
import { formatNumber, formatPercent } from '@/utils/format'

/** 空白表单：加载完成前的占位，避免渲染出 undefined。 */
const EMPTY_SETTING: AcceleratorSetting = {
  enabled: false,
  latency_routing: false,
  latency_weight: 50,
  cache_passthrough: false,
  cache_min_tokens: 0,
  max_idle_conns_per_host: 64,
  idle_conn_timeout_ms: 90_000,
  expect_100_continue: false,
}

/** 字段名 → 中文标签。用于把后端返回的 field 定位到具体输入框。 */
const FIELD_LABELS: Record<keyof AcceleratorSetting, string> = {
  enabled: '加速器总开关',
  latency_routing: '延迟感知选路',
  latency_weight: '延迟权重',
  cache_passthrough: '上游缓存透传',
  cache_min_tokens: '缓存统计阈值',
  max_idle_conns_per_host: '每主机空闲连接数',
  idle_conn_timeout_ms: '空闲连接回收时间',
  expect_100_continue: 'Expect: 100-continue',
}

export default function AdminAcceleratorPage() {
  const { toastError, toastSuccess } = useToast()

  const [overview, setOverview] = useState<AcceleratorOverview | null>(null)
  const [form, setForm] = useState<AcceleratorSetting>(EMPTY_SETTING)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  /** 字段级错误：key 是后端指出的字段名，值是要显示的红字。 */
  const [fieldError, setFieldError] = useState<{ field: string; message: string } | null>(null)

  // 用来丢弃"上一次请求返回的过期结果"：
  // 站长可能在连点两个开关，两次 PUT 的返回顺序不保证与发出顺序一致。
  const saveSeq = useRef(0)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const data = await fetchAccelerator()
      setOverview(data)
      setForm(data.setting)
    } catch (err) {
      toastError(err instanceof Error ? err.message : '加速器配置加载失败')
    } finally {
      setLoading(false)
    }
  }, [toastError])

  useEffect(() => {
    void load()
  }, [load])

  /** 提交单个字段的改动。
   *
   * 刻意只发【这一个字段】而不是整份表单：
   * 后端对未提供的字段保持原值，若这里回传整份，
   * 一个"只想点开缓存开关"的操作会把并发改动的其他项打回旧值。
   */
  const patch = useCallback(
    async (changes: Partial<AcceleratorSetting>, successHint: string) => {
      const seq = ++saveSeq.current
      setSaving(true)
      setFieldError(null)
      // 先本地更新：开关类操作不做即时反馈的话，手感上像是没生效。
      setForm((prev) => ({ ...prev, ...changes }))
      try {
        const result = await saveAccelerator(changes)
        if (seq !== saveSeq.current) return // 已有更新的请求在飞，丢弃这次结果
        // 用服务端回读的值覆盖本地：界面显示的必须是真实生效值，
        // 而不是我们以为存进去的值（例如有人手工改过数据库）。
        setForm(result.setting)
        setOverview((prev) => (prev ? { ...prev, setting: result.setting } : prev))
        toastSuccess(successHint)
      } catch (err) {
        if (seq !== saveSeq.current) return
        // 回滚：界面上不能停在一个"看起来已保存、实际没保存"的状态。
        void load()
        if (err instanceof ApiError && err.field) {
          setFieldError({ field: err.field, message: err.message })
        }
        toastError(err instanceof Error ? err.message : '保存失败')
      } finally {
        if (seq === saveSeq.current) setSaving(false)
      }
    },
    [load, toastError, toastSuccess],
  )

  const bounds: AcceleratorBounds = useMemo(
    () =>
      overview?.bounds ?? {
        latency_weight_min: 1,
        latency_weight_max: 100,
        idle_per_host_min: 2,
        idle_per_host_max: 512,
        idle_timeout_min_ms: 5_000,
        idle_timeout_max_ms: 600_000,
        cache_min_tokens_max: 1_000_000,
      },
    [overview],
  )

  const latency = overview?.latency
  const cache = overview?.cache

  /**
   * 是否要提示"瓶颈在本站"。
   *
   * 判据：用户实际等到的首字节远高于渠道实测 P95。
   * 差一个数量级就说明多出来的时间不是花在网络上，
   * 而是在本站的排队 / 重试 / 并发限制上。
   *
   * 门槛取"P95 + 2 秒"而不是"P95 的两倍"：
   * 上游本身慢的时候两倍也拉不开差距（会被一起放大），
   * 而"多等 2 秒"是绝对量，更贴近"这明显不对劲"的直觉。
   */
  const ttfbSamples = latency?.ttfb_samples ?? 0
  const latencyBottleneck =
    (latency?.measured ?? 0) > 0 &&
    ttfbSamples > 0 &&
    latency!.avg_ttfb_ms > (latency!.p95_ms + 2000) * 2

  /** 字段红字：只在该字段正在报错时显示，避免常驻的"填错了"提示。 */
  const errorOf = (key: keyof AcceleratorSetting) =>
    fieldError?.field === key ? fieldError.message : undefined

  /** 数字输入的统一处理：空串不算 0。
   *
   * 为什么：把空串直接 parse 成 0，会让"我先清空再填"这个正常动作
   * 立刻触发一次越界校验（0 低于所有下限），焦点一离开就弹红字。
   * 留空态让后端在真正提交时校验。
   */
  const numChange = (key: keyof AcceleratorSetting, raw: string, apply: (n: number) => void) => {
    const trimmed = raw.trim()
    if (trimmed === '') return
    const n = Number(trimmed)
    if (!Number.isFinite(n)) return
    apply(n)
  }

  if (loading && !overview) {
    return (
      <div className="space-y-6">
        <div className="h-7 w-40" />
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-20" />
          ))}
        </div>
        <Skeleton className="h-64" />
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="加速器"
        desc="让转发更快的三件事：按延迟挑渠道、复用连接、用上上游缓存。改动即时生效，不需要重启。"
        actions={
          <Button variant="secondary" size="sm" onClick={() => void load()} loading={loading}>
            刷新
          </Button>
        }
      />

      {/* ── 效果：先回答"现在快不快、瓶颈在哪" ── */}
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard
          label="实测最快 / 最慢渠道"
          value={
            latency && latency.measured > 0
              ? `${formatNumber(latency.fastest_ms)} / ${formatNumber(latency.slowest_ms)} ms`
              : '暂无数据'
          }
          hint={
            latency
              ? `${latency.measured}/${latency.channels} 个渠道有实测值`
              : undefined
          }
        />
        <StatCard
          label="渠道延迟 P50 / P95"
          value={
            latency && latency.measured > 0
              ? `${formatNumber(latency.p50_ms)} / ${formatNumber(latency.p95_ms)} ms`
              : '暂无数据'
          }
          hint={latency?.enabled ? '选路正在按延迟调整概率' : '延迟选路未启用'}
        />
        <StatCard
          label="用户实测首字节"
          value={
            latency && latency.ttfb_samples > 0 ? `${formatNumber(latency.avg_ttfb_ms)} ms` : '暂无数据'
          }
          hint={
            latency && latency.ttfb_samples > 0
              ? `${formatNumber(latency.ttfb_samples)} 次真实调用`
              : '还没有可统计的流式调用'
          }
        />
        <StatCard
          label="上游缓存命中率"
          value={cache && cache.sample_count > 0 ? formatPercent(cache.hit_rate) : '暂无数据'}
          hint={
            cache && cache.sample_count > 0
              ? `${formatNumber(cache.cached_tokens)} / ${formatNumber(cache.prompt_tokens)} tokens`
              : `${cache?.capable_channels ?? 0} 个渠道支持自动缓存`
          }
        />
      </div>

      {/* 瓶颈定位提示：只在两个口径都有数据且差距明显时出现。
          这是本页最有价值的一条判断——差得远说明问题在本站，
          去调上游权重不会有任何改善。 */}
      {latencyBottleneck && latency ? (
        <Card padding="md" className="border-warn/30 bg-warn/5">
          <p className="text-[13px] text-ink-2">
            <span className="font-semibold text-warn">瓶颈可能不在上游。</span>
            渠道实测 P95 是 {formatNumber(latency.p95_ms)} 毫秒，而用户实际等到的首字节是{' '}
            {formatNumber(latency.avg_ttfb_ms)} 毫秒——两者差了一个数量级。
            这种差距通常出在本站的排队、重试或并发限制上，
            继续调上游权重不会让用户更快；建议同时看「调用日志」里的排队与重试。
          </p>
        </Card>
      ) : null}

      {/* ── 总开关 ── */}
      <Card>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <h2 className="text-[15px] font-semibold text-ink">加速器总开关</h2>
              {form.enabled ? <Badge tone="ok">运行中</Badge> : <Badge tone="off">已关闭</Badge>}
            </div>
            <p className="mt-1 max-w-2xl text-[13px] text-ink-3">
              关闭后下面三项都不会参与转发决策。关闭不会丢失已保存的参数，
              重新打开即恢复。
            </p>
          </div>
          <Switch
            checked={form.enabled}
            disabled={saving}
            label={FIELD_LABELS.enabled}
            onChange={(v) => void patch({ enabled: v }, v ? '加速器已开启' : '加速器已关闭')}
          />
        </div>
      </Card>

      {/* 参数区：总开关关闭时降透明度但【不禁用】——
          站长需要能"先配好再开"，灰掉输入框会让人以为必须先开总开关。 */}
      <div className={`space-y-4 ${form.enabled ? '' : 'opacity-60'}`}>
        <Card>
          <SectionTitle
            title="一、延迟感知选路"
            desc="让实测更快的渠道被选中的概率更高。它调整的是概率分布，不是硬排序——任何一个渠道都不会因此失去被选中的机会，避免把所有流量打爆在同一个上游上。"
          />

          <ToggleRow
            label={FIELD_LABELS.latency_routing}
            desc="关闭后退回纯权重随机，与启用加速器之前的行为完全一致。"
            checked={form.latency_routing}
            disabled={saving}
            onChange={(v) => void patch({ latency_routing: v }, v ? '延迟选路已开启' : '延迟选路已关闭')}
          />

          <div className="mt-4 grid gap-4 sm:grid-cols-2">
            <Field
              label={FIELD_LABELS.latency_weight}
              htmlFor="accel-latency-weight"
              error={errorOf('latency_weight')}
              help={`范围 ${bounds.latency_weight_min} ~ ${bounds.latency_weight_max}。默认 50：延迟数据是瞬时值，权重压过人工配置会让站长在渠道页调好的权重失效。`}
            >
              <Input
                id="accel-latency-weight"
                type="number"
                min={bounds.latency_weight_min}
                max={bounds.latency_weight_max}
                value={form.latency_weight}
                onChange={(e) =>
                  numChange('latency_weight', e.target.value, (n) =>
                    setForm((prev) => ({ ...prev, latency_weight: n })),
                  )
                }
                onBlur={() =>
                  void patch(
                    { latency_weight: form.latency_weight },
                    `延迟权重已设为 ${form.latency_weight}`,
                  )
                }
              />
            </Field>

            <div className="flex items-end">
              <p className="text-[12px] text-ink-3">
                当前生效权重：
                <span className="ml-1 font-medium text-ink-2">
                  {latency && latency.enabled
                    ? `${Math.round(latency.latency_ratio * 100)}%`
                    : '不参与选路'}
                </span>
                {latency && latency.measured === 0 ? (
                  <span className="ml-2 text-warn">还没有任何渠道的实测延迟</span>
                ) : null}
              </p>
            </div>
          </div>
        </Card>

        <Card>
          <SectionTitle
            title="二、连接与传输层"
            desc="上游慢不一定是上游的问题：连接没被复用时，每个请求都要重新握手，这段时间全花在网络上而不是计算上。改完即时生效（会丢弃一次空闲连接，只影响换配置的那一刻）。"
          />

          <div className="grid gap-4 sm:grid-cols-2">
            <Field
              label={FIELD_LABELS.max_idle_conns_per_host}
              htmlFor="accel-idle-per-host"
              error={errorOf('max_idle_conns_per_host')}
              help={`范围 ${bounds.idle_per_host_min} ~ ${bounds.idle_per_host_max}。默认 64（改造前是 20）。数值过小会在并发高峰反复建连，过大则长期占着上游连接。`}
            >
              <Input
                id="accel-idle-per-host"
                type="number"
                min={bounds.idle_per_host_min}
                max={bounds.idle_per_host_max}
                value={form.max_idle_conns_per_host}
                onChange={(e) =>
                  numChange('max_idle_conns_per_host', e.target.value, (n) =>
                    setForm((prev) => ({ ...prev, max_idle_conns_per_host: n })),
                  )
                }
                onBlur={() =>
                  void patch(
                    { max_idle_conns_per_host: form.max_idle_conns_per_host },
                    `每主机空闲连接数已设为 ${form.max_idle_conns_per_host}`,
                  )
                }
              />
            </Field>

            <Field
              label={`${FIELD_LABELS.idle_conn_timeout_ms}（毫秒）`}
              htmlFor="accel-idle-timeout"
              error={errorOf('idle_conn_timeout_ms')}
              help={`范围 ${formatNumber(bounds.idle_timeout_min_ms)} ~ ${formatNumber(bounds.idle_timeout_max_ms)} 毫秒。太短会让连接刚建好就被回收（等于没复用），太长会占住上游的连接配额。`}
            >
              <Input
                id="accel-idle-timeout"
                type="number"
                min={bounds.idle_timeout_min_ms}
                max={bounds.idle_timeout_max_ms}
                step={1000}
                value={form.idle_conn_timeout_ms}
                onChange={(e) =>
                  numChange('idle_conn_timeout_ms', e.target.value, (n) =>
                    setForm((prev) => ({ ...prev, idle_conn_timeout_ms: n })),
                  )
                }
                onBlur={() =>
                  void patch(
                    { idle_conn_timeout_ms: form.idle_conn_timeout_ms },
                    `空闲连接回收时间已设为 ${form.idle_conn_timeout_ms} 毫秒`,
                  )
                }
              />
            </Field>
          </div>

          <ToggleRow
            className="mt-4"
            label={FIELD_LABELS.expect_100_continue}
            desc="上传类大请求先探一次再发正文，能省掉被上游直接拒绝的带宽。默认关闭：它会让每个请求多等一个来回，短请求上得不偿失。"
            checked={form.expect_100_continue}
            disabled={saving}
            onChange={(v) => void patch({ expect_100_continue: v }, v ? '已启用 100-continue' : '已关闭 100-continue')}
          />
        </Card>

        <Card>
          <SectionTitle
            title="三、上游缓存"
            desc="同一个长提示词反复发时，上游命中缓存的部分不计费也不算算力。目前只对确定会自动缓存的上游类型（OpenAI / DeepSeek / 通义）打开统计；需要上游显式标记缓存的协议暂不注入，避免发出上游不认的参数。"
          />

          <ToggleRow
            label={FIELD_LABELS.cache_passthrough}
            desc="关闭后不采集缓存统计，命中率一栏会显示「暂无数据」。"
            checked={form.cache_passthrough}
            disabled={saving}
            onChange={(v) => void patch({ cache_passthrough: v }, v ? '缓存统计已开启' : '缓存统计已关闭')}
          />

          <div className="mt-4 grid gap-4 sm:grid-cols-2">
            <Field
              label={`${FIELD_LABELS.cache_min_tokens}（tokens）`}
              htmlFor="accel-cache-min-tokens"
              error={errorOf('cache_min_tokens')}
              help="低于这个长度的请求不计入统计。太短的上游通常不缓存，统计进来只会把命中率稀释成一个没有意义的低数字。"
            >
              <Input
                id="accel-cache-min-tokens"
                type="number"
                min={0}
                max={bounds.cache_min_tokens_max}
                step={256}
                value={form.cache_min_tokens}
                onChange={(e) =>
                  numChange('cache_min_tokens', e.target.value, (n) =>
                    setForm((prev) => ({ ...prev, cache_min_tokens: n })),
                  )
                }
                onBlur={() =>
                  void patch(
                    { cache_min_tokens: form.cache_min_tokens },
                    `缓存统计阈值已设为 ${form.cache_min_tokens}`,
                  )
                }
              />
            </Field>

            <div className="flex items-end">
              <p className="text-[12px] text-ink-3">
                {cache && cache.sample_count > 0 ? (
                  <>
                    近 24 小时 {formatNumber(cache.sample_count)} 个渠道有缓存数据，
                    其中 {formatNumber(cache.capable_channels)} 个渠道被标注为支持自动缓存。
                  </>
                ) : (
                  <>近 24 小时还没有可用于统计的调用。缓存命中率需要真实调用积累，配置完不会立刻出现数字。</>
                )}
              </p>
            </div>
          </div>
        </Card>
      </div>

      {saving ? (
        <p className="text-[12px] text-ink-3" role="status">
          正在保存…
        </p>
      ) : null}
    </div>
  )
}

/** SectionTitle：分节标题 + 一段解释这个分节为什么存在。 */
function SectionTitle({ title, desc }: { title: string; desc: string }) {
  return (
    <div className="mb-4">
      <h2 className="text-[15px] font-semibold text-ink">{title}</h2>
      <p className="mt-1 max-w-3xl text-[13px] text-ink-3">{desc}</p>
    </div>
  )
}

/** ToggleRow：开关 + 标签 + 说明，横向排布。 */
function ToggleRow({
  label,
  desc,
  checked,
  disabled,
  onChange,
  className = '',
}: {
  label: string
  desc: string
  checked: boolean
  disabled?: boolean
  onChange: (v: boolean) => void
  className?: string
}) {
  return (
    <div className={`flex items-start justify-between gap-4 ${className}`}>
      <div className="min-w-0">
        <div className="text-[13px] font-medium text-ink-2">{label}</div>
        <p className="mt-0.5 text-[12px] text-ink-3">{desc}</p>
      </div>
      <Switch checked={checked} onChange={onChange} disabled={disabled} label={label} />
    </div>
  )
}

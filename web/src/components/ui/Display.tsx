/** 展示类基础组件：Card / Badge / StatCard / Skeleton / EmptyState / CodeBlock / Tabs / PageHeader。
 *
 * 意图（Why）：
 *   后台与门户的「信息展示」高度复用这些基础件；集中定义保证视觉重复性与一致性，
 *   避免每个页面各自拼样式类。
 *
 * Fluent 化要点：
 *   - 容器圆角 8px（比 4px 的控件更圆），层次靠「两层软影」--shadow-flat 表达，
 *     而不是靠加重描边；
 *   - 悬停反馈一律用半透明叠加层（bg-layer / bg-layer-2）而不是实色底：
 *     实色底在暗色主题下会与卡片撞色甚至反向（悬停后"更黑"）；
 *   - Tabs 从「分段胶囊」改为「下划线指示条」—— 分段胶囊是 iOS/web 语汇，
 *     Fluent 的分区切换（Pivot）是贴底边的 2px 指示条。
 */
'use client'

import { useState, type ReactNode } from 'react'

import { useI18n } from '@/i18n'

/* ── Card ─────────────────────────────────────────────── */

interface CardProps {
  children: ReactNode
  className?: string
  /** 内部纵向间距（默认 p-5，紧凑场景用 p-4） */
  padding?: 'md' | 'lg' | 'none'
  /**
   * 可点击 / 可选中的卡片：悬停时描边高亮并加深投影。
   *
   * 为什么默认关闭：静态展示卡上加悬停反馈等于给出一个「这里能点」的承诺，
   * 而用户点了没反应。只有真的绑了 onClick / href 的卡才该传 true。
   */
  interactive?: boolean
}

export function Card({ children, className, padding = 'md', interactive }: CardProps) {
  const pad = padding === 'lg' ? 'p-6' : padding === 'none' ? 'p-0' : 'p-5'
  return (
    <div
      className={`rounded-lg border border-line bg-card shadow-flat ${pad} ${
        interactive
          ? 'cursor-pointer transition-[border-color,box-shadow] duration-150 ease-fluent hover:border-line-2 hover:shadow-pop'
          : ''
      } ${className ?? ''}`}
    >
      {children}
    </div>
  )
}

/* ── Badge ─────────────────────────────────────────────── */

type BadgeTone = 'ok' | 'warn' | 'err' | 'info' | 'off' | 'brand'

export function Badge({ tone = 'off', children }: { tone?: BadgeTone; children: ReactNode }) {
  const toneClass: Record<BadgeTone, string> = {
    ok: 'bg-ok/10 text-ok border-ok/25',
    warn: 'bg-warn/10 text-warn border-warn/25',
    err: 'bg-err/10 text-err border-err/25',
    info: 'bg-info/10 text-info border-info/25',
    off: 'bg-layer-2 text-ink-2 border-line-2',
    brand: 'bg-brand/10 text-brand border-brand/25',
  }
  return (
    <span className={`inline-flex items-center gap-1 rounded-sm border px-1.5 py-0.5 text-xs ${toneClass[tone]}`}>
      {children}
    </span>
  )
}

/* ── StatCard：仪表盘/概览的汇总卡 ──────────────────────── */

interface StatCardProps {
  label: string
  value: string
  /** 辅助说明（如单位/对比） */
  hint?: string
  /** 卡片内的小徽标或趋势 */
  extra?: ReactNode
}

export function StatCard({ label, value, hint, extra }: StatCardProps) {
  // 超长数字（如管理员配额 10,000,000,000,000）在 22px 下会撑破卡片，
  // 按字符数降一档字号并允许折行，保证任何量级都不溢出。
  const compact = value.length > 10
  return (
    <div className="rounded-lg border border-line bg-card p-5 shadow-flat">
      <div className="text-[13px] text-ink-3">{label}</div>
      <div
        className={`mt-1.5 font-semibold tracking-tight text-ink tabular-nums ${
          compact ? 'break-all text-[17px] leading-snug' : 'text-[22px]'
        }`}
      >
        {value}
      </div>
      {hint && <div className="mt-1 text-xs text-ink-3">{hint}</div>}
      {extra && <div className="mt-2">{extra}</div>}
    </div>
  )
}

/* ── Skeleton：数据加载占位 ─────────────────────────────── */

export function Skeleton({ className = '' }: { className?: string }) {
  return <div className={`animate-pulse rounded-sm bg-layer-2 ${className}`} />
}

export function SkeletonRows({ rows = 3 }: { rows?: number }) {
  return (
    <div className="space-y-3">
      {Array.from({ length: rows }).map((_, i) => (
        <Skeleton key={i} className="h-10 w-full" />
      ))}
    </div>
  )
}

/* ── EmptyState：空数据占位 ────────────────────────────── */

interface EmptyStateProps {
  title: string
  description?: string
  action?: ReactNode
}

export function EmptyState({ title, description, action }: EmptyStateProps) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 py-14 text-center">
      <div className="text-sm font-medium text-ink-2">{title}</div>
      {description && <div className="max-w-md text-[13px] text-ink-3">{description}</div>}
      {action && <div className="mt-2">{action}</div>}
    </div>
  )
}

/* ── CodeBlock：只读代码展示（含一键复制） ──────────────── */

import { copyText } from '@/utils/clipboard'

interface CodeBlockProps {
  code: string
  language?: string
  title?: string
}

export function CodeBlock({ code, language, title }: CodeBlockProps) {
  const { t } = useI18n()
  const [copied, setCopied] = useState(false)
  async function handleCopy() {
    const ok = await copyText(code)
    if (ok) {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    }
  }
  return (
    // 代码块用专用 code-bg/code-fg 令牌：两套主题下都保持深底浅字，
    // 不复用 ink/surface（它们在暗色下会互换，导致代码块反相成浅底深字）。
    <div className="overflow-hidden rounded-lg border border-line bg-code-bg text-code-fg shadow-flat">
      <div className="flex items-center justify-between border-b border-white/10 px-4 py-2 text-xs">
        <span className="text-white/60">{title || language || 'code'}</span>
        <button type="button" onClick={handleCopy} className="flex items-center gap-1 text-white/60 hover:text-white">
          {copied ? t('components.copy.copied') : t('components.copy.copy')}
        </button>
      </div>
      <pre className="code-block overflow-x-auto p-4 text-white/90">{code}</pre>
    </div>
  )
}

/* ── Tabs：下划线式分区切换（用于广场分组 / 设置分区） ────── */

interface TabItem<T extends string> {
  value: T
  label: string
  count?: number
}

interface TabsProps<T extends string> {
  items: TabItem<T>[]
  value: T
  onChange: (value: T) => void
}

export function Tabs<T extends string>({ items, value, onChange }: TabsProps<T>) {
  return (
    <div className="flex flex-wrap items-stretch gap-1 border-b border-line" role="tablist">
      {items.map((item) => {
        const active = item.value === value
        return (
          <button
            key={item.value}
            type="button"
            role="tab"
            aria-selected={active}
            onClick={() => onChange(item.value)}
            className={`fluent-focus relative -mb-px flex items-center gap-1.5 px-3 pt-2 pb-2.5 text-[13px]
              transition-colors duration-150 ease-fluent
              ${active ? 'font-medium text-ink' : 'text-ink-3 hover:bg-layer hover:text-ink-2'}`}
          >
            {item.label}
            {item.count !== undefined && (
              <span
                className={`rounded-sm px-1.5 text-xs tabular-nums ${
                  active ? 'bg-brand/12 text-brand' : 'bg-layer-2 text-ink-3'
                }`}
              >
                {item.count}
              </span>
            )}
            {/* 指示条压在容器底边上，形成「这一项在轨道上」的读感 */}
            <span
              aria-hidden="true"
              className={`absolute inset-x-2 bottom-0 h-0.5 rounded-full transition-opacity duration-150 ${
                active ? 'bg-brand opacity-100' : 'opacity-0'
              }`}
            />
          </button>
        )
      })}
    </div>
  )
}

/* ── PageHeader：页面标题区 ────────────────────────────── */

/**
 * 页面头部：标题 + 副标题 + 右侧操作区。
 *
 * 意图（Why）：
 *   此前 27 个后台页面各自手写头部，量出 3 种外层布局
 *   （items-start / items-center / 裸 div），还有 loading 态与正常态
 *   各写一遍标题（settings 页切换时头部会跳变）。
 *   更麻烦的是「预留了操作区却没放东西」—— orders 页的
 *   `justify-between` 右侧是空的，看上去像加载失败。
 *
 * 流转（Flow）：
 *   <PageHeader title="…" desc="…" actions={<Button …/>} />
 *
 * 为什么 actions 是显式的具名槽位而不是 children：
 *   具名槽位让「这个页面到底有没有主行动按钮」变成编译期可见的事。
 *   传空 actions 时右侧不渲染任何占位 —— 宁可什么都没有，
 *   也不要留一个空的对齐槽（它比没有更让人以为这里本该有东西）。
 */
interface PageHeaderProps {
  title: string
  /** 副标题：一句话说清这一页能做什么、不能做什么 */
  desc?: ReactNode
  /** 右侧操作区；不传则整块不渲染 */
  actions?: ReactNode
}

export function PageHeader({ title, desc, actions }: PageHeaderProps) {
  return (
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div className="min-w-0">
        {/* semibold 而不是 bold：Fluent 的标题字重是 Semibold，
            用 bold 会让标题比系统原生"更吵"，在密集后台里尤其明显。 */}
        <h1 className="text-xl font-semibold text-ink">{title}</h1>
        {desc ? <p className="mt-0.5 text-[13px] text-ink-3">{desc}</p> : null}
      </div>
      {actions ? <div className="flex flex-wrap items-center gap-2">{actions}</div> : null}
    </div>
  )
}

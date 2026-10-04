/** 数据表格：DataTable（列定义驱动）与 Pagination。
 *
 * 意图（Why）：
 *   后台几乎每页都是「筛选 + 表格 + 分页」范式；把表格收敛成列定义数组，
 *   页面只需声明列与渲染函数，保证列对齐（数字右对齐、文本左对齐）全站一致。
 *
 * Fluent 化要点：
 *   - 行悬停用半透明叠加层 bg-layer，而不是 bg-surface/60：
 *     后者在暗色主题下会把行刷成【比卡片更深的灰】，看起来像在表格上挖了洞；
 *   - 表头文字用 ink-2（次级文字）而不是 ink-3：列头是全表最重要的定位信息，
 *     做到"浅到几乎看不见"只是显得干净，代价是每看一次表都要凑近读一次。
 */
'use client'

import type { ReactNode } from 'react'

import { AppIcon } from '@/components/AppIcon'
import { useI18n } from '@/i18n'
import { Button } from './Button'
import { EmptyState, SkeletonRows } from './Display'

export interface Column<T> {
  /** 列标题 */
  title: string
  /** 取值或自定义渲染 */
  render: (row: T) => ReactNode
  /** 对齐：默认左对齐；数字/状态建议传 right */
  align?: 'left' | 'right' | 'center'
  /** 列宽（tailwind 类，如 w-32）；默认等分 */
  width?: string
  /** 是否隐藏（响应式列） */
  className?: string
}

interface DataTableProps<T> {
  columns: Column<T>[]
  rows: T[] | null
  /** 行唯一键 */
  rowKey: (row: T) => string | number
  loading?: boolean
  emptyTitle?: string
  emptyDescription?: string
  /**
   * 空态里的行动按钮（如"还没有渠道"→ 直接给个"去添加渠道"）。
   *
   * 为什么必须支持：空态是站长第一次走到这一页的时刻，
   * 他此刻最需要的不是一句描述，而是一个能立刻解决问题的入口。
   * 只给纯文字"请先到渠道管理"等于要他在 20+ 项导航里自己找那一项。
   */
  emptyAction?: ReactNode
  onRowClick?: (row: T) => void
  /**
   * 表格上方的标题区（如"无映射渠道"这类分节标题）。
   *
   * 为什么用槽位而不是让调用方在外面套一层 Card：
   * 本组件自带 `rounded-lg border border-line bg-card` 容器，
   * 外面再套一个 Card 会出现两层圆角矩形（内白外灰的"相框"效果）。
   * 此前全站有 18 处这么写，而正确写法（裸放）只出现 1 处——
   * 同一份代码库里并存两种相反做法时，少数派才是对的。
   * 槽位把"表格 + 标题 + 分页"收成一层容器，调用方无需判断该套不该套。
   */
  header?: ReactNode
  /**
   * 表格下方的辅助区，通常放 `<Pagination />`。
   *
   * 同样是为了收进同一层容器：分页条与表格共享一个边框，
   * 而不是一个悬在表格下方的独立方块。
   */
  footer?: ReactNode
  /**
   * 去掉自身的外框（圆角 + 描边 + 背景）。
   *
   * 用于「表格被包在一个已有边框的容器里」的场景，例如带标题栏的 Card。
   * 此前这类场景只能用负 margin 硬掰，脆弱且在容器 padding 一变就错位；
   * 更早的做法是外面再套一层 Card，于是出现双层描边。
   */
  bare?: boolean
}

export function DataTable<T>({
  columns,
  rows,
  rowKey,
  loading,
  emptyTitle,
  emptyDescription,
  emptyAction,
  onRowClick,
  header,
  footer,
  bare,
}: DataTableProps<T>) {
  const { t } = useI18n()
  const loadingState = loading || rows === null

  return (
    <div
      className={
        bare
          ? // bare 模式下外框交给调用方的容器，本组件只负责表格本身。
            // 表头仍保留底色与 border-b —— 那是表头自身的底色与分隔线，
            // 去掉会让表头与第一行糊在一起。
            ''
          : 'overflow-hidden rounded-lg border border-line bg-card shadow-flat'
      }
    >
      {header ? <div className="border-b border-line px-4 py-3">{header}</div> : null}
      <div className="overflow-x-auto">
        <table className="w-full border-collapse text-sm">
          <thead>
            <tr className="border-b border-line bg-layer text-[13px] text-ink-2">
              {columns.map((col, index) => (
                <th
                  key={index}
                  className={`px-4 py-2.5 font-medium ${
                    col.align === 'right' ? 'text-right' : col.align === 'center' ? 'text-center' : 'text-left'
                  } ${col.width ?? ''}`}
                >
                  {col.title}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {loadingState ? (
              <tr>
                <td colSpan={columns.length} className="px-4 py-4">
                  <SkeletonRows rows={3} />
                </td>
              </tr>
            ) : rows && rows.length > 0 ? (
              rows.map((row) => (
                <tr
                  key={rowKey(row)}
                  onClick={onRowClick ? () => onRowClick(row) : undefined}
                  className={`border-b border-line/70 transition-colors duration-150 ease-fluent last:border-0 hover:bg-layer ${
                    onRowClick ? 'cursor-pointer' : ''
                  }`}
                >
                  {columns.map((col, index) => (
                    <td
                      key={index}
                      className={`px-4 py-3 text-ink-2 ${
                        col.align === 'right' ? 'text-right' : col.align === 'center' ? 'text-center' : 'text-left'
                      } ${col.className ?? ''}`}
                    >
                      {col.render(row)}
                    </td>
                  ))}
                </tr>
              ))
            ) : (
              <tr>
                <td colSpan={columns.length}>
                  <EmptyState
                    title={emptyTitle ?? t('components.dataState.empty')}
                    description={emptyDescription}
                    action={emptyAction}
                  />
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
      {/* 分页条紧贴表格下沿：加 border-t 而不是留白，
          否则它看起来像一个与表格无关的独立方块。
          顶部不加 padding —— Pagination 自带 pt-3，两处都加会显得过分松散。 */}
      {footer ? <div className="border-t border-line px-4 pb-2">{footer}</div> : null}
    </div>
  )
}

/* ── Pagination ─────────────────────────────────────────── */

interface PaginationProps {
  page: number
  pageSize: number
  total: number
  onChange: (page: number) => void
}

export function Pagination({ page, pageSize, total, onChange }: PaginationProps) {
  const { t } = useI18n()
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const pages: number[] = []
  const start = Math.max(1, page - 2)
  const end = Math.min(totalPages, page + 2)
  for (let i = start; i <= end; i++) pages.push(i)

  return (
    <div className="flex items-center justify-between gap-3 pt-3">
      {/* 刻意用词条里的 range + page 两句拼接，而不是把整句塞进一个键：
            后两种语言的语序（总数在前还是页码在前）不同，强行按中文语序拼会别扭。 */}
      <div className="text-xs text-ink-3">
        {t('components.pagination.range', {
          start: total === 0 ? 0 : (page - 1) * pageSize + 1,
          end: Math.min(page * pageSize, total),
          total,
        })}
        {' · '}
        {t('components.pagination.page', { page, pages: totalPages })}
      </div>
      <div className="flex items-center gap-1">
        <Button
          variant="ghost"
          size="sm"
          disabled={page <= 1}
          onClick={() => onChange(page - 1)}
          aria-label={t('components.pagination.prev')}
        >
          <AppIcon name="chevron-left" size={16} />
        </Button>
        {pages.map((p) => (
          <button
            key={p}
            type="button"
            onClick={() => onChange(p)}
            className={`fluent-focus min-w-7 rounded-sm px-1.5 py-1 text-[13px] tabular-nums transition duration-150 ease-fluent ${
              p === page ? 'bg-brand font-medium text-on-brand' : 'text-ink-3 hover:bg-layer hover:text-ink'
            }`}
          >
            {p}
          </button>
        ))}
        <Button
          variant="ghost"
          size="sm"
          disabled={page >= totalPages}
          onClick={() => onChange(page + 1)}
          aria-label={t('components.pagination.next')}
        >
          <AppIcon name="chevron-right" size={16} />
        </Button>
      </div>
    </div>
  )
}

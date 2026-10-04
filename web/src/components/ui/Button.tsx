/** Button：全站统一按钮（React 版）。
 *
 * 视觉语言（Fluent Design / Windows 11）：
 *   - primary：实心强调色 + 极淡白描边（Fluent 的强调按钮带一圈 1px 高光边，
 *     让按钮有一点"厚度"，而不是一块平色矩形）；
 *   - secondary：Fluent 的「标准按钮」= 半透明叠加底 + 中性描边；
 *   - ghost：无底无框，只在悬停时给一层叠加（导航内用）；
 *   - danger：实心红。文字色用 --color-on-err 而不是 text-white——
 *     暗色下红被提亮成浅粉，白字在上面只有 1.6:1。
 *
 * 所有按钮统一圆角（4px）/字号/高度，保证扫视时「长得一样的按钮是同一种优先级」。
 * 控件圆角刻意比容器（8px）更方：这是 Windows 11 最容易辨认的一处特征，
 * 控件做到 10px 以上会立刻变回"网页风"。
 */
'use client'

import { forwardRef, type ButtonHTMLAttributes } from 'react'

type Variant = 'primary' | 'secondary' | 'ghost' | 'danger' | 'invert'
type Size = 'sm' | 'md' | 'lg'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant
  size?: Size
  loading?: boolean
}

const variantClass: Record<Variant, string> = {
  primary: 'bg-brand text-on-brand border border-white/10 hover:bg-brand/90 active:bg-brand/80',
  // 半透明叠加底而不是实色底：实色底（如 bg-surface）在暗色主题下会与卡片撞色，
  // 甚至出现"悬停后按钮比卡片更黑"的反向反馈。
  secondary: 'bg-layer text-ink border border-line-2 hover:bg-layer-2 hover:border-ink-3/40',
  ghost: 'text-ink-2 border border-transparent hover:text-ink hover:bg-layer',
  danger: 'bg-err text-on-err border border-transparent hover:bg-err/90 active:bg-err/80',
  // 深色 hero 区里的反色 CTA（白底品牌字）：那种底上品牌色按钮会糊成一片。
  invert: 'bg-white text-brand border border-transparent shadow-flat hover:bg-white/90',
}

const sizeClass: Record<Size, string> = {
  sm: 'h-8 px-3 text-[13px] gap-1.5',
  md: 'h-9.5 px-4 text-sm gap-2',
  lg: 'h-11 px-6 text-[15px] gap-2',
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = 'secondary', size = 'md', loading, className, children, disabled, ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      disabled={disabled || loading}
      className={`fluent-focus inline-flex select-none items-center justify-center rounded-sm font-medium
        transition duration-150 ease-fluent active:scale-[0.98]
        disabled:pointer-events-none disabled:opacity-40
        ${variantClass[variant]} ${sizeClass[size]} ${className ?? ''}`}
      {...rest}
    >
      {loading && (
        <span className="h-4 w-4 animate-spin rounded-full border-2 border-current border-t-transparent" />
      )}
      {children}
    </button>
  )
})

/** 语言切换器（React 版，等价旧 LocaleSwitcher.vue）。
 *
 * 意图（Why）：
 *   落地页顶栏与页脚提供语言切换；用「母语自称」展示（不随界面语言变化），
 *   用户在任何界面语言下都能认出自己的语言。
 *
 * 视觉：与 ThemeToggle 同一套菜单语言（4px 控件圆角 / 8px 浮层圆角、
 *   半透明叠加悬停、选中项左侧强调条）—— 两个下拉菜单挨着摆在顶栏上，
 *   样式分叉会立刻被看出来。
 */
'use client'

import { useEffect, useRef, useState } from 'react'

import { AppIcon } from '@/components/AppIcon'
import { SUPPORTED_LOCALES, useI18n } from '@/i18n'

export function LocaleSwitcher({ compact = false }: { compact?: boolean }) {
  const { locale, setLocale } = useI18n()
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

  // 点击外部关闭。ThemeToggle 一直有这段，本组件此前漏了 ——
  // 表现为菜单点开之后只能再点一次按钮才能关掉，而旁边的主题菜单点空白处就能关。
  useEffect(() => {
    if (!open) return
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDoc)
    return () => document.removeEventListener('mousedown', onDoc)
  }, [open])

  const current = SUPPORTED_LOCALES.find((item) => item.code === locale) ?? SUPPORTED_LOCALES[0]

  function select(code: string) {
    setLocale(code)
    setOpen(false)
  }

  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="fluent-focus flex items-center gap-1 rounded-sm px-2 py-1.5 text-[13px] text-ink-2 transition duration-150 ease-fluent hover:bg-layer hover:text-ink"
        aria-label="切换语言"
        aria-expanded={open}
      >
        <AppIcon name="globe" size={15} />
        <span>{compact ? '' : current.name}</span>
        <AppIcon name="chevron-down" size={13} />
      </button>
      {open && (
        <div className="fluent-dialog-in absolute right-0 z-30 mt-1 w-36 overflow-hidden rounded-lg border border-line bg-card py-1 shadow-pop">
          {SUPPORTED_LOCALES.map((item) => (
            <button
              key={item.code}
              type="button"
              onClick={() => select(item.code)}
              className={`relative flex w-full items-center justify-between px-3 py-1.5 text-left text-[13px] transition-colors duration-150 ease-fluent hover:bg-layer ${
                item.code === locale ? 'font-medium text-ink' : 'text-ink-2'
              }`}
            >
              <span
                aria-hidden="true"
                className={`absolute left-0 top-1/2 h-4 w-[3px] -translate-y-1/2 rounded-full bg-brand transition-opacity ${
                  item.code === locale ? 'opacity-100' : 'opacity-0'
                }`}
              />
              {item.name}
              {item.code === locale && <AppIcon name="check" size={14} className="text-brand" />}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

/** AppShell：功能层外壳（侧边导航 + 顶栏 + 内容区），用户门户与管理后台共用。
 *
 * 意图（Why）：
 *   门户 / 后台的布局骨架相同：左侧导航（分组）、顶部用户态与登出、右侧内容区。
 *   差异只有导航项与鉴权要求，因此收敛为一个组件，两个布局传入各自的 NavGroup。
 *
 * 【为什么侧栏必须有快速搜索】
 *   后台导航有 20+ 项，在 w-56（224px）侧栏里必然滚动 ——
 *   "合规与运维"组的末几项（系统设置、语料共建）落在首屏之外。
 *   滚动本身不是问题，问题是【命名体系不统一】：
 *   搜"渠道"会同时命中渠道管理 / 渠道健康 / 模型映射 / 模型分组四项，
 *   搜"价格"会命中计价规则 / 财务对账 / 系统设置的支付配置三处。
 *   关键词无法预测命中哪些项，这正是搜索能解决、而分组解决不了的。
 *   分组该合并的仍要合并（本组件不替代重排，只做兜底）。
 *
 * 流转（Flow）：
 *   console/layout.tsx 或 admin/layout.tsx → <AppShell groups={...}> → 子路由内容
 */
'use client'

import Link from 'next/link'
import { usePathname, useRouter } from 'next/navigation'
import { useEffect, useMemo, useRef, useState } from 'react'

import { BrandLogo } from '@/components/BrandMark'
import { AppIcon, type IconName } from '@/components/AppIcon'
import { LocaleSwitcher } from '@/components/site/LocaleSwitcher'
import { ThemeToggle } from '@/components/site/ThemeToggle'
import { useI18n } from '@/i18n'
import { useAuth } from '@/lib/auth/auth-context'

export interface ShellNavItem {
  /** 导航文案；labelKey 存在时以 t(labelKey) 为准 */
  label: string
  /**
   * 词条键（如 'components.nav.admin.channels'）。
   *
   * 为什么不让本组件按 href 反查词条：那样每新增一个导航项就要改两个文件
   * （layout 与本组件），漏改时的表现是「导航项存在但点进去没有对应文案」——
   * 排查成本远高于调用方多写一个字符串。
   *
   * label 刻意保留为必填：它同时是 SSG 阶段的首屏内容与词条缺失时的兜底，
   * 两者都依赖一个非空的中文字面量。
   */
  labelKey?: string
  href: string
  icon: IconName
  /** 精确匹配时高亮（默认前缀匹配） */
  exact?: boolean
  /**
   * 搜索别名（可选）。
   *
   * 为什么需要：站长脑子里的词与导航上的词常常不一样 ——
   * 他想找"退款"会点进「充值订单」，想找"黑名单"会找「敏感词」，
   * 想找"成本"会去「财务对账」。只匹配 label 的话这些都搜不到，
   * 而搜索框一旦搜不到东西，站长就再也不会用它了。
   */
  keywords?: string[]
}

export interface ShellNavGroup {
  /** 分组标题；titleKey 存在时以 t(titleKey) 为准 */
  title?: string
  /** 分组标题词条键；缺失时回落到 title（理由同 ShellNavItem.labelKey） */
  titleKey?: string
  items: ShellNavItem[]
}

interface AppShellProps {
  groups: ShellNavGroup[]
  /** 外壳品牌名 */
  brand: string
  children: React.ReactNode
}

/** 一条扁平化后的导航项：搜索结果用它（需要记住来自哪个组）。 */
interface FlatNavItem {
  item: ShellNavItem
  groupTitle: string
}

export function AppShell({ groups, brand, children }: AppShellProps) {
  const pathname = usePathname()
  const router = useRouter()
  const { user, displayName, signOut, isAdmin } = useAuth()
  const { t } = useI18n()
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [query, setQuery] = useState('')
  const searchRef = useRef<HTMLInputElement>(null)

  /**
   * 取导航项显示文案。
   *
   * 词条缺失时回落到 label（而不是显示裸键名）：导航是全站每页都在的位置，
   * 键名裸露在侧边栏比中文更糟——用户看到 'components.nav.admin.channels'
   * 完全无从判断那是什么，而中文虽然语言不对，至少还看得懂。
   */
  const navLabel = (item: ShellNavItem) => resolveLabel(item.label, item.labelKey)

  /** 分组标题取词条，缺失时回落到中文（不显示裸键名）。 */
  const groupTitle = (group: ShellNavGroup) =>
    group.title ? resolveLabel(group.title, group.titleKey) : ''

  /**
   * 词条命中则用词条，否则回落到字面量。
   *
   * 不回落到「键名本身」是刻意的：导航与分组标题是全站每页都在的位置，
   * 键名裸露在侧边栏里用户完全无从判断那是什么，而中文虽然语言不对，
   * 至少还看得懂——前者是让用户困惑，后者只是让用户别扭。
   */
  const resolveLabel = (fallback: string, key?: string) => {
    if (!key) return fallback
    const text = t(key)
    return text === key ? fallback : text
  }

  const isActive = (item: ShellNavItem) => {
    if (item.exact) return pathname === item.href
    return pathname === item.href || pathname.startsWith(item.href + '/')
  }

  // 扁平化一次，供搜索过滤。
  //
  // 刻意在渲染侧算而不是让 layout 预先传一份扁平列表：
  // 两份数据一旦不同步，搜索结果与侧栏高亮就会指向不同的项，
  // 而这种不一致只在"点了搜索结果但侧栏没高亮"时才暴露。
  const flatItems = useMemo<FlatNavItem[]>(
    () => groups.flatMap((g) => g.items.map((item) => ({ item, groupTitle: groupTitle(g) }))),
    // groupTitle 依赖 t()，而 t 随语言变化
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [groups, t],
  )

  const searchResults = useMemo<FlatNavItem[]>(() => {
    const q = query.trim().toLowerCase()
    if (!q) return []
    return flatItems.filter(({ item }) => {
      const haystack = [item.label, item.href, ...(item.keywords ?? [])]
        .join(' ')
        .toLowerCase()
      return haystack.includes(q)
    })
  }, [flatItems, query])

  /** Cmd/Ctrl+K 唤起搜索；Esc 关闭。 */
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        searchRef.current?.focus()
        searchRef.current?.select()
      } else if (e.key === 'Escape' && document.activeElement === searchRef.current) {
        setQuery('')
        searchRef.current?.blur()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  /** 跳转到搜索结果并收起搜索。 */
  const gotoResult = (href: string) => {
    setQuery('')
    searchRef.current?.blur()
    setSidebarOpen(false)
    router.push(href)
  }

  async function handleSignOut() {
    await signOut()
    router.replace(isAdmin ? '/admin/login' : '/login')
  }

  return (
    <div className="flex min-h-screen">
      {/* 桌面侧边栏 */}
      <aside className="fixed inset-y-0 left-0 z-20 hidden w-56 flex-col border-r border-line bg-card lg:flex">
        <Link href="/" className="flex h-14 items-center gap-2 border-b border-line px-4">
          <BrandLogo name={brand} />
        </Link>

        {/* 搜索：20+ 项导航在窄侧栏里必然滚动，搜索是唯一能一步到位的手段 */}
        <div className="border-b border-line px-3 py-2.5">
          <div className="relative">
            <AppIcon
              name="search"
              size={14}
              className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-3"
            />
            <input
              ref={searchRef}
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="搜索功能…"
              aria-label="搜索功能"
              className="h-8 w-full rounded-md border border-line-2 bg-surface pl-8 pr-10 text-[13px] text-ink
                         placeholder:text-ink-3 focus:border-brand focus:outline-none"
            />
            {query ? (
              <button
                type="button"
                onClick={() => {
                  setQuery('')
                  searchRef.current?.focus()
                }}
                aria-label="清空搜索"
                className="absolute right-2 top-1/2 -translate-y-1/2 text-ink-3 hover:text-ink"
              >
                <AppIcon name="close" size={13} />
              </button>
            ) : (
              /* 快捷键提示：让人知道可以按 Cmd/Ctrl+K，
                 否则这个搜索框看起来只是个普通输入框 */
              <kbd className="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 rounded border border-line px-1 text-[10px] text-ink-3">
                ⌘K
              </kbd>
            )}
          </div>

          {/* 搜索结果：替换下方导航（而不是浮层），
              这样点完之后视线不需要再回到侧栏。 */}
          {query.trim() ? (
            <div className="mt-2 max-h-72 overflow-y-auto">
              {searchResults.length === 0 ? (
                <p className="px-2 py-3 text-[12px] text-ink-3">
                  没有匹配「{query.trim()}」的功能。
                  {searchResults.length === 0 && flatItems.length > 0 ? '试试更短的关键词。' : ''}
                </p>
              ) : (
                <div className="space-y-0.5">
                  {searchResults.map(({ item, groupTitle: gt }) => (
                    <button
                      key={item.href}
                      type="button"
                      onClick={() => gotoResult(item.href)}
                      className={`flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-left text-[13px] transition ${
                        isActive(item) ? 'bg-brand/8 font-medium text-brand' : 'text-ink-2 hover:bg-ink/5 hover:text-ink'
                      }`}
                    >
                      <AppIcon
                        name={item.icon}
                        size={16}
                        className={isActive(item) ? 'text-brand' : 'text-ink-3'}
                      />
                      <span className="min-w-0 flex-1 truncate">{navLabel(item)}</span>
                      <span className="shrink-0 text-[11px] text-ink-3">{gt}</span>
                    </button>
                  ))}
                </div>
              )}
            </div>
          ) : null}
        </div>

        <nav className="flex-1 space-y-5 overflow-y-auto px-3 py-4">
          {groups.map((group, gi) => (
            <div key={gi}>
              {group.title && (
                <div className="px-2 pb-1.5 text-xs font-medium tracking-wider text-ink-3">
                  {groupTitle(group)}
                </div>
              )}
              <div className="space-y-0.5">
                {group.items.map((item) => {
                  const active = isActive(item)
                  return (
                    <Link
                      key={item.href}
                      href={item.href}
                      className={`flex items-center gap-2.5 rounded-md px-2.5 py-2 text-[13px] transition ${
                        active ? 'bg-brand/8 font-medium text-brand' : 'text-ink-2 hover:bg-ink/5 hover:text-ink'
                      }`}
                    >
                      <AppIcon name={item.icon} size={17} className={active ? 'text-brand' : 'text-ink-3'} />
                      {navLabel(item)}
                    </Link>
                  )
                })}
              </div>
            </div>
          ))}
        </nav>
      </aside>

      {/* 移动端侧边栏抽屉 */}
      {sidebarOpen && (
        <div className="fixed inset-0 z-40 lg:hidden">
          <div className="fixed inset-0 bg-ink/40" onClick={() => setSidebarOpen(false)} />
          <aside className="fixed inset-y-0 left-0 z-50 flex w-64 flex-col border-r border-line bg-card">
            <div className="flex h-14 items-center justify-between border-b border-line px-4">
              <span className="font-semibold text-ink">{brand}</span>
              <button
                type="button"
                onClick={() => setSidebarOpen(false)}
                aria-label={t('components.drawer.close')}
              >
                <AppIcon name="close" size={18} className="text-ink-3" />
              </button>
            </div>
            <nav className="flex-1 space-y-5 overflow-y-auto px-3 py-4">
              {groups.map((group, gi) => (
                <div key={gi}>
                  {group.title && (
                    <div className="px-2 pb-1.5 text-xs font-medium text-ink-3">{groupTitle(group)}</div>
                  )}
                  {group.items.map((item) => (
                    <Link
                      key={item.href}
                      href={item.href}
                      onClick={() => setSidebarOpen(false)}
                      className={`flex items-center gap-2.5 rounded-md px-2.5 py-2 text-[13px] ${
                        isActive(item) ? 'bg-brand/8 font-medium text-brand' : 'text-ink-2 hover:bg-ink/5'
                      }`}
                    >
                      <AppIcon name={item.icon} size={17} />
                      {navLabel(item)}
                    </Link>
                  ))}
                </div>
              ))}
            </nav>
          </aside>
        </div>
      )}

      {/* 内容区 */}
      <div className="flex min-w-0 flex-1 flex-col lg:pl-56">
        <header className="sticky top-0 z-10 flex h-14 items-center justify-between border-b border-line bg-surface/90 px-4 backdrop-blur lg:px-6">
          <button
            type="button"
            className="rounded p-1.5 text-ink-3 hover:bg-ink/5 lg:hidden"
            onClick={() => setSidebarOpen(true)}
            aria-label={t('components.shell.expandNav')}
          >
            <AppIcon name="menu" size={20} />
          </button>
          <div className="hidden text-[13px] text-ink-3 lg:block">{brand}</div>
          <div className="flex items-center gap-2">
            <ThemeToggle compact />
            <LocaleSwitcher compact />
            <div className="flex items-center gap-2 rounded-md border border-line bg-card px-2.5 py-1.5 text-[13px]">
              <span className="max-w-28 truncate text-ink-2">{displayName}</span>
              {user && user.role === 10 && (
                <span className="rounded bg-brand/10 px-1 text-xs text-brand">
                  {t('components.shell.roleAdmin')}
                </span>
              )}
            </div>
            <button
              type="button"
              onClick={handleSignOut}
              className="flex items-center gap-1 rounded-md px-2 py-1.5 text-[13px] text-ink-3 transition hover:bg-ink/5 hover:text-err"
            >
              <AppIcon name="logout" size={15} />
              {t('components.shell.signOut')}
            </button>
          </div>
        </header>
        <main className="flex-1 px-4 py-6 lg:px-6">{children}</main>
      </div>
    </div>
  )
}
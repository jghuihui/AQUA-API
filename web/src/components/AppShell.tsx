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
 * 【视觉：Fluent NavigationView】
 *   - 选中项是「左侧 3px 强调条 + 一层浅叠加」，不是整块染成强调色：
 *     侧栏每页都在，整块染色会让导航比内容区还抢眼，而导航的职责是让路。
 *   - 可用宽度不足时可收起成 60px 图标条（状态持久化）：
 *     后台用户常在 1366px 的笔记本上工作，224px 侧栏 + 内容区在宽表格页会挤压。
 *   - 顶栏用亚克力材质（半透明 + 背景模糊），滚动时内容从它下面透出来。
 *
 * 流转（Flow）：
 *   console/layout.tsx 或 admin/layout.tsx → <AppShell groups={...}> → 子路由内容
 */
'use client'

import Link from 'next/link'
import { usePathname, useRouter } from 'next/navigation'
import { useEffect, useMemo, useRef, useState } from 'react'

import { BrandLogo, BrandMark } from '@/components/BrandMark'
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

/**
 * 侧栏收起状态的持久化键。
 *
 * 为什么必须持久化：收起的目的是「这一台机器上内容区不够宽」，
 * 那是设备属性而不是临时心情。换个页面就弹回去，用户第一次收起之后
 * 就再也不会用它了。
 */
const COLLAPSE_KEY = 'ltzy.shell.collapsed'

function readCollapsed(): boolean {
  try {
    return window.localStorage.getItem(COLLAPSE_KEY) === '1'
  } catch {
    // 隐私模式 / 存储被禁时 localStorage 会直接抛异常。
    // 导航栏展不展开始终是可用的，不值得为了记住一个偏好中断渲染。
    return false
  }
}

/**
 * 单个导航项：桌面展开态 / 桌面收起态 / 移动抽屉三处共用同一份实现。
 *
 * 为什么抽出来：此前这三处各写了一遍 active/hover 的类名三元表达式。
 * 「选中态」是导航里唯一有语义的状态，三份副本只要有一处漏改，
 * 表现就是「在抽屉里点过的项回到桌面侧栏不高亮」—— 只在特定宽度下复现。
 */
function NavLink({
  item,
  label,
  active,
  collapsed,
  onNavigate,
}: {
  item: ShellNavItem
  label: string
  active: boolean
  collapsed: boolean
  onNavigate?: () => void
}) {
  return (
    <Link
      href={item.href}
      onClick={onNavigate}
      // 收起态没有文字，title 是唯一的可发现性来源（否则只剩一排无字图标）
      title={collapsed ? label : undefined}
      aria-current={active ? 'page' : undefined}
      className={`group relative flex items-center rounded-sm text-[13px] transition-colors duration-150 ease-fluent ${
        collapsed ? 'h-10 w-full justify-center' : 'gap-2.5 px-2.5 py-2'
      } ${active ? 'bg-layer-2 font-medium text-ink' : 'text-ink-2 hover:bg-layer hover:text-ink'}`}
    >
      <span
        aria-hidden="true"
        className={`absolute left-0 top-1/2 h-4 w-[3px] -translate-y-1/2 rounded-full bg-brand transition-opacity duration-150 ${
          active ? 'opacity-100' : 'opacity-0'
        }`}
      />
      <AppIcon
        name={item.icon}
        size={17}
        className={active ? 'text-brand' : 'text-ink-3 group-hover:text-ink-2'}
      />
      {!collapsed && <span className="min-w-0 flex-1 truncate">{label}</span>}
    </Link>
  )
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
   * 收起态两段式启动：`collapsed` 初始为 false，挂载后再从 localStorage 读入。
   *
   * 为什么不直接把初值写成 readCollapsed()：那会在服务端渲出 false、
   * 客户端首帧渲出 true，触发 hydration 不一致。而只用 useEffect 又会让
   * 「展开的侧栏」先画一帧再动画收起（每次刷新都演一遍）。
   * 因此额外用一个 shellReady 标志：挂载完成前不加 transition，收起是瞬时的。
   */
  const [collapsed, setCollapsed] = useState(false)
  const [shellReady, setShellReady] = useState(false)
  const focusSearchAfterExpand = useRef(false)

  useEffect(() => {
    setCollapsed(readCollapsed())
    setShellReady(true)
  }, [])

  useEffect(() => {
    // shellReady 之前不写：读入的 effect 排在前面但状态要到下一次提交才生效，
    // 这一轮会先按默认值 false 写一次，把用户真实偏好冲掉一瞬。
    if (!shellReady) return
    try {
      window.localStorage.setItem(COLLAPSE_KEY, collapsed ? '1' : '0')
    } catch {
      /* 存不下就退化为「本次会话有效」，不影响功能 */
    }
  }, [collapsed, shellReady])

  /** 从收起态点搜索图标展开后，把焦点交给搜索框（否则用户还要再点一次）。 */
  useEffect(() => {
    if (!collapsed && focusSearchAfterExpand.current) {
      focusSearchAfterExpand.current = false
      searchRef.current?.focus()
    }
  }, [collapsed])

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

  /**
   * 选中判定。
   *
   * 【为什么必须先把尾斜杠抹平 —— 这是实际修掉的一个缺陷】
   *   本站 next.config.ts 开了 trailingSlash（为了 go:embed 能直接伺服
   *   /console/tokens/index.html 这样的真实文件），于是 usePathname() 在
   *   后台首页返回的是 "/admin/"，而导航里写的是 "/admin"。
   *   仪表盘那一项标了 exact:true（精确匹配），`"/admin/" === "/admin"` 恒为假
   *   —— 结果就是【站在仪表盘上时，侧栏里没有任何一项是亮的】。
   *   更麻烦的是它只在"带尾斜杠访问"时复现：直接刷新 /admin 就正常，
   *   点侧栏跳过来就不正常，排查时极容易被当成随机现象。
   */
  const isActive = (item: ShellNavItem) => {
    const current = pathname.replace(/\/+$/, '') || '/'
    const target = item.href.replace(/\/+$/, '') || '/'
    if (item.exact) return current === target
    return current === target || current.startsWith(target + '/')
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
        // 收起态下搜索框不在 DOM 里，先把侧栏展开再聚焦（靠上面的 effect 落焦）。
        setCollapsed((prev) => {
          if (prev) focusSearchAfterExpand.current = true
          return false
        })
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

  const navPad = collapsed ? 'px-2' : 'px-3'
  const asideWidth = collapsed ? 'w-[60px]' : 'w-56'
  // 挂载完成前不加 width/pl 过渡：否则首帧会演一遍"侧栏自己收起"的动画。
  const widthTransition = shellReady ? 'transition-[width,padding-left] duration-200 ease-fluent' : ''

  return (
    <div className="flex min-h-screen">
      {/* 桌面侧边栏 */}
      <aside
        id="app-shell-nav"
        className={`fixed inset-y-0 left-0 z-20 hidden flex-col border-r border-line bg-card lg:flex ${asideWidth} ${widthTransition}`}
      >
        <div
          className={`flex h-14 shrink-0 items-center border-b border-line ${
            collapsed ? 'justify-center' : 'px-4'
          }`}
        >
          <Link href="/" className="flex items-center gap-2" title={brand}>
            {collapsed ? <BrandMark size={22} /> : <BrandLogo name={brand} />}
          </Link>
        </div>

        {/* 搜索：20+ 项导航在窄侧栏里必然滚动，搜索是唯一能一步到位的手段 */}
        {collapsed ? (
          <div className="border-b border-line px-2 py-2.5">
            <button
              type="button"
              onClick={() => {
                focusSearchAfterExpand.current = true
                setCollapsed(false)
              }}
              title="搜索功能"
              aria-label="搜索功能"
              className="fluent-focus flex h-9 w-full items-center justify-center rounded-sm text-ink-3 transition duration-150 ease-fluent hover:bg-layer hover:text-ink"
            >
              <AppIcon name="search" size={16} />
            </button>
          </div>
        ) : (
          <div className="border-b border-line px-3 py-2.5">
            <div className="relative">
              <AppIcon
                name="search"
                size={14}
                className="pointer-events-none absolute left-2.5 top-1/2 z-10 -translate-y-1/2 text-ink-3"
              />
              <input
                ref={searchRef}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder="搜索功能…"
                aria-label="搜索功能"
                // fluent-input 提供底边强调线的聚焦形态（见 globals.css）
                className="fluent-input h-8 w-full rounded-sm border border-line-2 bg-surface pl-8 pr-10 text-[13px] text-ink
                           placeholder:text-ink-3"
              />
              {query ? (
                <button
                  type="button"
                  onClick={() => {
                    setQuery('')
                    searchRef.current?.focus()
                  }}
                  aria-label="清空搜索"
                  className="fluent-focus absolute right-2 top-1/2 -translate-y-1/2 rounded-sm text-ink-3 hover:text-ink"
                >
                  <AppIcon name="close" size={13} />
                </button>
              ) : (
                /* 快捷键提示：让人知道可以按 Cmd/Ctrl+K，
                   否则这个搜索框看起来只是个普通输入框 */
                <kbd className="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 rounded-sm border border-line px-1 text-[10px] text-ink-3">
                  ⌘K
                </kbd>
              )}
            </div>

            {/* 搜索结果：替换下方导航（而不是浮层），
                这样点完之后视线不需要再回到侧栏。 */}
            {query.trim() ? (
              <div className="fluent-scroll mt-2 max-h-72 overflow-y-auto">
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
                        className={`fluent-focus flex w-full items-center gap-2.5 rounded-sm px-2.5 py-2 text-left text-[13px] transition duration-150 ease-fluent ${
                          isActive(item) ? 'bg-layer-2 font-medium text-ink' : 'text-ink-2 hover:bg-layer hover:text-ink'
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
        )}

        <nav className={`fluent-scroll flex-1 space-y-5 overflow-y-auto py-4 ${navPad}`}>
          {groups.map((group, gi) => (
            <div key={gi}>
              {/* 收起态放不下分组标题（60px 里塞 4 个汉字必然折行），
                  但分组信息不能直接消失 —— 用一条分隔线保住"这里换了一组"。 */}
              {group.title &&
                (collapsed ? (
                  gi > 0 ? (
                    <div className="mx-1 mb-2 border-t border-line" aria-hidden="true" />
                  ) : null
                ) : (
                  <div className="px-2 pb-1.5 text-xs font-medium tracking-wider text-ink-3">
                    {groupTitle(group)}
                  </div>
                ))}
              <div className="space-y-0.5">
                {group.items.map((item) => (
                  <NavLink
                    key={item.href}
                    item={item}
                    label={navLabel(item)}
                    active={isActive(item)}
                    collapsed={collapsed}
                  />
                ))}
              </div>
            </div>
          ))}
        </nav>
      </aside>

      {/* 移动端侧边栏抽屉 */}
      {sidebarOpen && (
        <div className="fixed inset-0 z-40 lg:hidden">
          {/* 遮罩恒用黑色：bg-ink 在暗色主题下是白色，会变成"给页面盖一层白纱"。
              Fluent 的系统烟幕（Smoke）在两套主题下都是黑色。 */}
          <div className="fluent-fade-in fixed inset-0 bg-black/40" onClick={() => setSidebarOpen(false)} />
          <aside className="fluent-slide-in fixed inset-y-0 left-0 z-50 flex w-64 flex-col border-r border-line bg-card">
            <div className="flex h-14 items-center justify-between border-b border-line px-4">
              <span className="font-semibold text-ink">{brand}</span>
              <button
                type="button"
                onClick={() => setSidebarOpen(false)}
                aria-label={t('components.drawer.close')}
                className="fluent-focus rounded-sm p-1 text-ink-3 transition duration-150 ease-fluent hover:bg-layer hover:text-ink"
              >
                <AppIcon name="close" size={18} />
              </button>
            </div>
            <nav className="fluent-scroll flex-1 space-y-5 overflow-y-auto px-3 py-4">
              {groups.map((group, gi) => (
                <div key={gi}>
                  {group.title && (
                    <div className="px-2 pb-1.5 text-xs font-medium tracking-wider text-ink-3">
                      {groupTitle(group)}
                    </div>
                  )}
                  <div className="space-y-0.5">
                    {group.items.map((item) => (
                      <NavLink
                        key={item.href}
                        item={item}
                        label={navLabel(item)}
                        active={isActive(item)}
                        collapsed={false}
                        onNavigate={() => setSidebarOpen(false)}
                      />
                    ))}
                  </div>
                </div>
              ))}
            </nav>
          </aside>
        </div>
      )}

      {/* 内容区 */}
      <div className={`flex min-w-0 flex-1 flex-col ${collapsed ? 'lg:pl-[60px]' : 'lg:pl-56'} ${widthTransition}`}>
        {/* 顶栏：亚克力材质（半透明 + 背景模糊），滚动时内容从它下面透出来。
            窄屏退化会由 prefers-reduced-transparency 媒体查询处理。 */}
        <header className="fluent-acrylic sticky top-0 z-10 flex h-14 items-center justify-between gap-2 border-b border-line px-4 lg:px-6">
          <div className="flex min-w-0 items-center gap-1.5">
            {/* 窄屏：打开抽屉 */}
            <button
              type="button"
              className="fluent-focus rounded-sm p-1.5 text-ink-3 transition duration-150 ease-fluent hover:bg-layer hover:text-ink lg:hidden"
              onClick={() => setSidebarOpen(true)}
              aria-label={t('components.shell.expandNav')}
            >
              <AppIcon name="menu" size={20} />
            </button>
            {/* 桌面：收起 / 展开侧栏 */}
            <button
              type="button"
              onClick={() => setCollapsed((v) => !v)}
              aria-label={t('components.shell.collapseNav')}
              aria-expanded={!collapsed}
              aria-controls="app-shell-nav"
              title={t('components.shell.collapseNav')}
              className="fluent-focus hidden rounded-sm p-1.5 text-ink-3 transition duration-150 ease-fluent hover:bg-layer hover:text-ink lg:inline-flex"
            >
              <AppIcon name="panel-left" size={18} />
            </button>
            <div className="hidden truncate text-[13px] text-ink-3 lg:block">{brand}</div>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <ThemeToggle compact />
            <LocaleSwitcher compact />
            <div className="flex items-center gap-2 rounded-sm border border-line bg-card px-2.5 py-1.5 text-[13px]">
              <span className="max-w-28 truncate text-ink-2">{displayName}</span>
              {user && user.role === 10 && (
                <span className="rounded-sm bg-brand/10 px-1 text-xs text-brand">
                  {t('components.shell.roleAdmin')}
                </span>
              )}
            </div>
            <button
              type="button"
              onClick={handleSignOut}
              className="fluent-focus flex items-center gap-1 rounded-sm px-2 py-1.5 text-[13px] text-ink-3 transition duration-150 ease-fluent hover:bg-layer hover:text-err"
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

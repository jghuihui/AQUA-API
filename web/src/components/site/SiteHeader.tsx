/** SiteHeader：公共站顶栏——技术站式「品牌 + 版本标签 + 分区导航」。
 *
 * 意图（Why）：
 *   用户否掉了个人博客风，要的是「技术站」。技术站顶栏的信息密度更高：
 *   - 左侧：品牌标 + 一个等宽的运行标签（版本 / 协议兼容），像工具站的状态条；
 *   - 中部：分区导航锚点（能力 / 接入 / 模型 / FAQ），等宽字体，扫视快；
 *   - 右侧：主题、语言与账号入口。
 *   仍保持「不悬浮」——顶部是普通文档流的一部分，滚动时随页面离开。
 *
 * 流转（Flow）：
 *   各公共页 → <SiteHeader /> → 导航锚点回到首页分区，模型走独立路由。
 */
'use client'

import Link from 'next/link'

import { AppIcon } from '@/components/AppIcon'
import { BrandLogo } from '@/components/BrandMark'
import { useAuth } from '@/lib/auth/auth-context'
import { useSite } from '@/lib/site/site-context'

import { LocaleSwitcher } from './LocaleSwitcher'
import { ThemeToggle } from './ThemeToggle'

/** 顶栏分区导航：锚点回到首页小节，模型量级足够大故单列路由 */
const NAV = [
  { href: '/#features', label: '能力' },
  { href: '/#quickstart', label: '接入' },
  { href: '/models', label: '模型与价格' },
  { href: '/#faq', label: '常见问题' },
]

export function SiteHeader({ transparent: _transparent = false }: { transparent?: boolean }) {
  const { ready, isLoggedIn, displayName } = useAuth()
  const { status, siteName } = useSite()

  return (
    <header className="border-b border-line bg-card">
      <div className="mx-auto flex h-14 max-w-6xl items-center gap-4 px-4 sm:px-6">
        <Link href="/" className="shrink-0" aria-label="返回首页">
          <BrandLogo name={siteName} />
        </Link>

        {/* 运行标签：等宽 + 状态点，给技术站一个「在线」的信号 */}
        <span className="hidden shrink-0 items-center gap-1.5 rounded-sm border border-line bg-surface px-2 py-0.5 font-mono text-[11px] text-ink-3 lg:inline-flex">
          <span className="h-1.5 w-1.5 rounded-full bg-ok" />
          v{status?.version || '2'} · OpenAI 兼容
        </span>

        <nav className="ml-auto hidden items-center gap-0.5 md:flex">
          {NAV.map((item) => (
            <Link
              key={item.href}
              href={item.href}
              className="fluent-focus rounded-sm px-2.5 py-1.5 font-mono text-[13px] text-ink-2 transition duration-150 ease-fluent hover:bg-layer hover:text-ink"
            >
              {item.label}
            </Link>
          ))}
        </nav>

        <div className="ml-auto flex items-center gap-1.5 md:ml-3">
          <ThemeToggle compact />
          <LocaleSwitcher compact />
          {/* 必须等 ready 之后再决定渲染哪一支。
              本站是静态导出：SSG 时读不到令牌（它在 localStorage 里），
              服务端必然渲出「登录 / 注册」，而客户端首帧渲出「账号 / 进入控制台」——
              两边不一致，React 会把整棵顶栏丢弃重建，用户看到的是
              右边按钮"闪一下再变成另一个"。
              占位块给了同样的高度（h-8），所以替换时顶栏不会跳高。 */}
          {!ready ? (
            <span className="inline-block h-8 w-28 rounded-sm bg-layer" aria-hidden="true" />
          ) : isLoggedIn ? (
            <Link
              href="/console"
              className="fluent-focus flex items-center gap-1 rounded-sm border border-line-2 px-2.5 py-1.5 font-mono text-[13px] text-ink-2 transition duration-150 ease-fluent hover:border-brand hover:bg-layer hover:text-brand"
            >
              {displayName}
              <AppIcon name="chevron-right" size={14} />
            </Link>
          ) : (
            <>
              <Link
                href="/login"
                className="fluent-focus rounded-sm px-2.5 py-1.5 font-mono text-[13px] text-ink-2 transition duration-150 ease-fluent hover:bg-layer hover:text-ink"
              >
                登录
              </Link>
              <Link
                href="/register"
                className="fluent-focus rounded-sm border border-white/10 bg-brand px-3 py-1.5 font-mono text-[13px] font-medium text-on-brand transition duration-150 ease-fluent hover:bg-brand/90"
              >
                注册
              </Link>
            </>
          )}
        </div>
      </div>
    </header>
  )
}

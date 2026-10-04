/** 用户门户布局（/console/*）：鉴权守卫 + 门户导航。
 *
 * 意图（Why）：
 *   所有门户页面都需要登录。此处用 AuthProvider 的 ready/isLoggedIn 做客户端守卫，
 *   未登录跳转 /login?redirect=当前路径；已登录套 AppShell。
 */
'use client'

import { usePathname, useRouter } from 'next/navigation'
import { useEffect } from 'react'

import { AppShell, type ShellNavGroup } from '@/components/AppShell'
import { ComplianceGate } from '@/components/site/ComplianceGate'
import { useAuth } from '@/lib/auth/auth-context'

const GROUPS: ShellNavGroup[] = [
  {
    title: '总览',
    titleKey: 'components.nav.console.overview',
    items: [
      { label: '概览', href: '/console', labelKey: 'components.nav.console.overview', icon: 'home', exact: true },
      { label: '模型广场', href: '/console/models', labelKey: 'components.nav.console.models', icon: 'grid',
        keywords: ['价格', '模型列表', '可用模型'] },
    ],
  },
  {
    title: '接入',
    items: [
      { label: '访问令牌', href: '/console/tokens', labelKey: 'components.nav.console.tokens', icon: 'key',
        keywords: ['apikey', '密钥', 'key'] },
      { label: '接入示例', href: '/#quickstart', labelKey: 'components.nav.console.docs', icon: 'book',
        keywords: ['文档', '怎么用', 'quickstart', 'curl'] },
      { label: '游乐场', href: '/console/playground', labelKey: 'components.nav.console.playground', icon: 'play',
        keywords: ['调试', '试一试', '测试'] },
    ],
  },
  {
    title: '我的',
    items: [
      // 智能客服放在"我的"组首位：它是登录用户最可能主动找的东西，
      // 而不是必须排在最后的功能之一。
      { label: '智能客服', href: '/console/support', labelKey: 'components.nav.console.support', icon: 'chat',
        keywords: ['客服', '问一句', 'ai'] },
      { label: '调用日志', href: '/console/logs', labelKey: 'components.nav.console.logs', icon: 'list',
        keywords: ['请求', '消费', '记录'] },
      { label: '生成任务', href: '/console/tasks', labelKey: 'components.nav.console.tasks', icon: 'image',
        keywords: ['图片', '视频', '音乐'] },
      { label: '财务记录', href: '/console/finance', labelKey: 'components.nav.console.finance', icon: 'wallet',
        keywords: ['账单', '消费明细', '花了多少'] },
      { label: '账户充值', href: '/console/recharge', labelKey: 'components.nav.console.recharge', icon: 'cart',
        keywords: ['充值', '付款', '余额', '购买'] },
      { label: '邀请奖励', href: '/console/referral', labelKey: 'components.nav.console.referral', icon: 'users',
        keywords: ['邀请', '返利', '推广'] },
      { label: '第三方账号', href: '/console/accounts', labelKey: 'components.nav.console.externalAccounts', icon: 'external',
        keywords: ['绑定', '微信', 'github', '登录方式'] },
    ],
  },
]

export default function ConsoleLayout({ children }: { children: React.ReactNode }) {
  const { ready, isLoggedIn } = useAuth()
  const pathname = usePathname()
  const router = useRouter()

  useEffect(() => {
    if (ready && !isLoggedIn) {
      router.replace(`/login?redirect=${encodeURIComponent(pathname)}`)
    }
  }, [ready, isLoggedIn, pathname, router])

  if (!ready || !isLoggedIn) {
    return <div className="flex min-h-screen items-center justify-center text-[13px] text-ink-3">正在进入门户…</div>
  }

  return (
    <AppShell groups={GROUPS} brand="用户门户">
      {/* 首次进入控制台弹一次合规确认（自包含：内部判断路由与已确认状态） */}
      <ComplianceGate />
      {children}
    </AppShell>
  )
}
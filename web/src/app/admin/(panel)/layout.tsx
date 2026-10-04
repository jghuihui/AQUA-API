/** 管理后台布局（/admin/*，route group (panel)）：鉴权守卫（管理员）+ 后台导航。
 *
 * 意图（Why）：
 *   后台全部页面需要「登录 + 管理员」双重权限。守卫在布局层统一完成，
 *   页面组件不必各自判断。
 *
 * 【导航分组的重排依据】
 *   此前是四组（总览 / 资源 / 业务 / 合规与运维），量下来有三处实质问题：
 *
 *   1) **"异步任务"归错域**。它是用户业务产物（图像/视频/音乐生成），
 *      却和"渠道""计价规则"同在资源组，图标还是 image——
 *      视觉上暗示这是平台资源。站长想找用户生成的东西会去"业务"组找。
 *   2) **"对用户发声"的两件事被拆开**。站点公告（前台横幅）与
 *      群发邮件（群发邮箱）都是对外沟通，却分处两组。
 *   3) **"内容运营"被拆成两半**。语料共建（4 个 tab：清单/样本/统计/授权）
 *      是内容运营，却和内容安全分属两组；而它自己在页内还要提示
 *      "请先在「语料清单」添加模型"，说明内部本就是一条动线。
 *
 *   现在改成五组，判据是"站长要做某件事时，这些功能会不会被一起想起"：
 *   概览（先看）→ 渠道与模型（供给侧）→ 用户与交易（需求侧）→
 *   内容与运营（对用户说话）→ 系统与排障（出事之后）。
 *
 *   另：AI 助手从"运维组首位"移到"概览"组。
 *   此前把它放运维组首位是想让它成为"第一入口"，但 20+ 项导航里
 *   排第 20 位就等于不可达 —— 用组内排序实现"高频"是弱手段。
 *
 * 流转（Flow）：
 *   /admin/login 放在 (panel) 之外独立渲染，不经过本守卫外壳，
 *   避免「要登录后台才能看到登录页」死循环；其余 /admin/* 全部套本布局。
 */
'use client'

import { usePathname, useRouter } from 'next/navigation'
import { useEffect } from 'react'

import { AppShell, type ShellNavGroup } from '@/components/AppShell'
import { useAuth } from '@/lib/auth/auth-context'
import { useToast } from '@/lib/toast/toast-context'

/**
 * 后台导航。
 *
 * keywords 是搜索别名：站长脑子里的词与导航上的词常常不一样
 * （想找"退款"会点「充值订单」，想找"黑名单"会找「敏感词」）。
 * 侧栏搜索框靠它命中，见 AppShell 的说明。
 */
const GROUPS: ShellNavGroup[] = [
  {
    title: '概览',
    titleKey: 'components.nav.admin.overview',
    items: [
      { label: '仪表盘', href: '/admin', labelKey: 'components.nav.admin.dashboard', icon: 'home', exact: true },
      { label: 'AI 助手', href: '/admin/agent', labelKey: 'components.nav.admin.agent', icon: 'sparkles',
        keywords: ['对话', '运维助手', '客服', 'chat', '助手'] },
      { label: '运维监控', href: '/admin/maintenance', labelKey: 'components.nav.admin.maintenanceMonitor', icon: 'trend',
        keywords: ['健康', '备份', '数据库', '版本', '磁盘'] },
    ],
  },
  {
    title: '渠道与模型',
    titleKey: 'components.nav.admin.resources',
    items: [
      { label: '渠道管理', href: '/admin/channels', labelKey: 'components.nav.admin.channels', icon: 'server',
        keywords: ['上游', '供应商', '密钥', '映射', '成本'] },
      { label: '渠道健康', href: '/admin/channel-health', labelKey: 'components.nav.admin.channelHealth', icon: 'chart',
        keywords: ['延迟', '可用性', '成功率', '巡检', '探针'] },
      { label: '加速器', href: '/admin/accelerator', labelKey: 'components.nav.admin.accelerator', icon: 'bolt',
        keywords: ['提速', '缓存', '连接池', '选路', '性能', 'cache'] },
      { label: '模型测速', href: '/admin/speedtest', icon: 'gauge',
        keywords: ['首字延迟', 'benchmark', '测速'] },
      { label: '模型管理', href: '/admin/models', labelKey: 'components.nav.admin.models', icon: 'grid',
        keywords: ['实体', '广场', '展示名', '厂商'] },
      { label: '模型映射', href: '/admin/model-mappings', labelKey: 'components.nav.admin.modelMappings', icon: 'swap',
        keywords: ['model mapping', '模型名对照', '透传'] },
      { label: '模型分组', href: '/admin/groups', labelKey: 'components.nav.admin.groups', icon: 'tag',
        keywords: ['倍率', '分组', 'tier'] },
      { label: '计价规则', href: '/admin/prices', labelKey: 'components.nav.admin.prices', icon: 'quota',
        keywords: ['价格', '定价', '售价', '免费'] },
      { label: '异步任务', href: '/admin/tasks', labelKey: 'components.nav.admin.tasks', icon: 'image',
        keywords: ['生成', '图像', '视频', '音乐'] },
    ],
  },
  {
    title: '用户与交易',
    items: [
      { label: '用户管理', href: '/admin/users', labelKey: 'components.nav.admin.users', icon: 'users',
        keywords: ['额度', '封禁', '试用', '角色'] },
      { label: '令牌管理', href: '/admin/tokens', labelKey: 'components.nav.admin.tokens', icon: 'key',
        keywords: ['apikey', '密钥', '凭据'] },
      { label: '充值订单', href: '/admin/orders', labelKey: 'components.nav.admin.orders', icon: 'cart',
        keywords: ['退款', '入账', '交易', '对账'] },
      { label: '兑换码', href: '/admin/redeem-codes', labelKey: 'components.nav.admin.redeemCodes', icon: 'ticket',
        keywords: ['优惠码', '批量生成', '作废'] },
      { label: '财务对账', href: '/admin/finance', icon: 'wallet',
        keywords: ['成本', '毛利', '收入', '盈亏'] },
      { label: '订阅账号', href: '/admin/oauth', labelKey: 'components.nav.admin.oauth', icon: 'globe',
        keywords: ['oauth', '提供方', 'chatgpt', 'claude'] },
    ],
  },
  {
    title: '内容与运营',
    items: [
      { label: '敏感词', href: '/admin/sensitive-words', labelKey: 'components.nav.admin.sensitiveWords', icon: 'filter',
        keywords: ['黑名单', '过滤', '内容安全', '违规词'] },
      { label: '语料共建', href: '/admin/corpus', icon: 'layers',
        keywords: ['语料', '样本', '采集', '授权', '共建'] },
      { label: '站点公告', href: '/admin/announcements', labelKey: 'components.nav.admin.announcements', icon: 'info',
        keywords: ['横幅', '通知', '前台'] },
      { label: '群发邮件', href: '/admin/broadcast', icon: 'mail',
        keywords: ['邮件', '群发', '推送', 'smtp'] },
    ],
  },
  {
    title: '系统与排障',
    titleKey: 'components.nav.admin.operations',
    items: [
      { label: '调用日志', href: '/admin/logs', labelKey: 'components.nav.admin.logs', icon: 'list',
        keywords: ['请求', '错误', '排查', 'trace'] },
      { label: '操作审计', href: '/admin/audit-logs', labelKey: 'components.nav.admin.audit', icon: 'shield',
        keywords: ['留痕', '谁改的', '操作记录'] },
      { label: '告警通道', href: '/admin/alert-channels', labelKey: 'components.nav.admin.alertChannels', icon: 'alert',
        keywords: ['通知', 'webhook', '钉钉', '企微', '邮件告警'] },
      { label: '系统设置', href: '/admin/settings', labelKey: 'components.nav.admin.settings', icon: 'sliders',
        keywords: ['支付', 'seo', 'smtp', '注册', '站点名', '测速开关'] },
    ],
  },
]

export default function AdminLayout({ children }: { children: React.ReactNode }) {
  const { ready, isLoggedIn, isAdmin } = useAuth()
  const pathname = usePathname()
  const router = useRouter()
  const { toastError } = useToast()

  useEffect(() => {
    if (!ready) return
    // /admin/login 在 (panel) 之外独立渲染，不受本守卫约束（结构上已隔离）
    if (!isLoggedIn) {
      router.replace(`/admin/login?redirect=${encodeURIComponent(pathname)}`)
    } else if (!isAdmin) {
      toastError('没有权限访问管理后台')
      router.replace('/console')
    }
  }, [ready, isLoggedIn, isAdmin, pathname, router, toastError])

  if (!ready || !isLoggedIn || !isAdmin) {
    return <div className="flex min-h-screen items-center justify-center text-[13px] text-ink-3">正在进入管理后台…</div>
  }

  return (
    <AppShell groups={GROUPS} brand="管理后台">
      {children}
    </AppShell>
  )
}

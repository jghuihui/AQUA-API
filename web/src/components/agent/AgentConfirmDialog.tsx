/**
 * 助手写操作的确认弹窗。
 *
 * 意图（Why）：
 *   助手在执行"封禁用户""改额度"这类会影响真实用户的操作前，
 *   必须先让站长看清楚**具体会改什么**再点确认。
 *   这不是普通的二次确认对话框 —— 它要回答的是三个问题：
 *
 *     改的是谁？→ 展示用户名/令牌名/渠道名，而不只是一个 ID
 *     改什么？　→ 逐条列出"现在是什么 → 会变成什么"
 *     有什么后果？→ 一句风险提示（谁会受影响、能不能恢复）
 *
 *   常规 ConfirmDialog 只回答"你确定吗"，那在 AI 助手上是危险的：
 *   站长点确定时其实并不知道自己批准了什么，只能相信助手。
 *
 * 【为什么不能在这里改写任何值】
 *   params 由服务端下发并在弹窗上原样展示。确认时必须【原样送回】：
 *   一旦允许前端修改，就会出现"弹窗写着封禁 3 号、实际执行了 5 号"，
 *   而站长正是因为看到了"3 号"才点的确认。
 *
 * 流转（Flow）：
 *   SSE confirm 事件 → 页面存下 { request, 原始问题, 历史 }
 *   → 用户点确认 → 同一个问题 + confirm 凭证重发 → 服务端执行
 *   → 用户点取消 → 不发任何请求，仅在对话里记一句"已取消"
 */
'use client'

import type { AgentConfirmRequest } from '@/api/agent'
import { Button } from '@/components/ui/Button'
import { Modal } from '@/components/ui/Modal'

export function AgentConfirmDialog({
  request,
  onConfirm,
  onCancel,
  busy = false,
}: {
  request: AgentConfirmRequest | null
  onConfirm: () => void
  onCancel: () => void
  busy?: boolean
}) {
  if (!request) return null
  return (
    <Modal open onClose={busy ? () => {} : onCancel} title={request.title}>
      <div className="space-y-4">
        {/*
          风险提示放在最上面而不是底部。
          站长在扫一眼就点"确认"的动作里，最先看到的是第一行 ——
          把后果藏在末尾等于把它藏起来。
        */}
        {request.risk_note && (
          <p className="rounded-md border border-warn/30 bg-warn/10 px-3 py-2 text-[12px] leading-relaxed text-warn">
            {request.risk_note}
          </p>
        )}

        {/*
          变更清单用表格而不是键值对列表：
          "当前值"与"将变成"并排才能被一眼比对，
          而堆成一列"标签：值"时人脑要自己记住上一个值再对照下一个。
        */}
        <table className="w-full text-[13px]">
          <tbody>
            {request.summary.map((item, i) => (
              <tr key={`${item.label}-${i}`} className="border-b border-line last:border-0">
                <td className="w-28 py-2 pr-3 align-top text-ink-3">{item.label}</td>
                <td className="py-2 align-top text-ink-2">
                  <span className="break-all">{item.value}</span>
                </td>
                {item.after ? (
                  <>
                    <td className="w-6 py-2 text-center align-top text-ink-3">→</td>
                    <td className="py-2 align-top font-medium text-ink">
                      <span className="break-all">{item.after}</span>
                    </td>
                  </>
                ) : (
                  <td colSpan={2} />
                )}
              </tr>
            ))}
          </tbody>
        </table>

        <p className="text-[12px] text-ink-3">
          这项操作将由助手代为执行（工具：
          <code className="mx-1 rounded bg-surface-2 px-1 py-0.5 text-[11px]">
            {request.tool_name}
          </code>
          ）。取消不会有任何改动。
        </p>

        <div className="flex justify-end gap-2">
          {/*
            取消放在左边、确认放右边，且确认用 danger 样式：
            凡是走到这个弹窗的操作都是高危的（封禁、改额度），
            统一用 primary 会让"确认"看起来像日常操作。
          */}
          <Button variant="secondary" onClick={onCancel} disabled={busy}>
            取消
          </Button>
          <Button variant="danger" onClick={onConfirm} loading={busy}>
            确认执行
          </Button>
        </div>
      </div>
    </Modal>
  )
}

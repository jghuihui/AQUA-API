/** 弹窗与对话框：Modal（通用弹层）与 ConfirmDialog（确认框）。
 *
 * 意图（Why）：
 *   后台大量「新建/编辑/删除确认」交互集中于此。统一遮罩、滚动锁定、
 *   无关键操作丢失；焦点管理保持基本可访问性（Esc 关闭、返回值确认）。
 *
 * Fluent 化要点：
 *   - 遮罩在两套主题下都必须是【黑色】烟幕（Fluent Smoke 恒为黑）；
 *     用 bg-ink 这种主题感知色会变成"暗色模式下盖一层白纱"，越遮越亮；
 *   - 进场是「1.06 → 1 + 淡入」而不是「0.95 → 1 放大」：
 *     前者读起来像是"压在纸面上落下来"，后者像是"从中心长出来"；
 *   - 容器圆角 8px + 两层软影 shadow-pop，不用加重描边表达层次。
 */
'use client'

import { Fragment, useEffect, type ReactNode } from 'react'

import { AppIcon } from '@/components/AppIcon'
import { useI18n } from '@/i18n'
import { Button } from './Button'

interface ModalProps {
  open: boolean
  onClose: () => void
  title: string
  /** 弹层宽度（默认 560px，宽表单用 720px，窄提示用 420px） */
  width?: number
  children: ReactNode
  footer?: ReactNode
}

export function Modal({ open, onClose, title, width = 560, children, footer }: ModalProps) {
  const { t } = useI18n()
  // Esc 关闭 + 打开时锁定背景滚动
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    const prevOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.removeEventListener('keydown', onKey)
      document.body.style.overflow = prevOverflow
    }
  }, [open, onClose])

  if (!open) return null

  return (
    <div className="fixed inset-0 z-40 flex items-start justify-center overflow-y-auto p-4 pt-[8vh]">
      {/* 遮罩：点击关闭。恒用黑色 —— 暗色主题下 bg-ink 是白色，会反向变亮。 */}
      <div
        className="fluent-fade-in fixed inset-0 bg-black/40 backdrop-blur-sm"
        onClick={onClose}
        aria-hidden="true"
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        style={{ width }}
        className="fluent-dialog-in relative flex max-w-full flex-col rounded-lg border border-line bg-card shadow-pop"
      >
        <div className="flex items-center justify-between border-b border-line px-5 py-3.5">
          <h3 className="text-base font-semibold text-ink">{title}</h3>
          <button
            type="button"
            onClick={onClose}
            className="fluent-focus rounded-sm p-1 text-ink-3 transition duration-150 ease-fluent hover:bg-layer hover:text-ink"
            aria-label={t('components.modal.close')}
          >
            <AppIcon name="close" size={18} />
          </button>
        </div>
        <div className="flex-1 overflow-y-auto px-5 py-4">{children}</div>
        {footer && <div className="flex justify-end gap-2 border-t border-line px-5 py-3.5">{footer}</div>}
      </div>
    </div>
  )
}

interface ConfirmDialogProps {
  open: boolean
  title: string
  message?: string
  confirmText?: string
  cancelText?: string
  danger?: boolean
  loading?: boolean
  onConfirm: () => void
  onCancel: () => void
}

export function ConfirmDialog({
  open,
  title,
  message,
  confirmText,
  cancelText,
  danger,
  loading,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  const { t } = useI18n()
  return (
    <Modal open={open} onClose={onCancel} title={title} width={420}>
      <div className="text-sm text-ink-2">{message || t('common.confirm.defaultMessage')}</div>
      <div className="mt-5 flex justify-end gap-2">
        <Button variant="secondary" onClick={onCancel}>
          {cancelText ?? t('common.action.cancel')}
        </Button>
        <Button variant={danger ? 'danger' : 'primary'} loading={loading} onClick={onConfirm}>
          {confirmText ?? t('common.action.confirm')}
        </Button>
      </div>
    </Modal>
  )
}

/* ── CopyButton：一键复制（通用） ───────────────────────── */

import { useState } from 'react'
import { copyText } from '@/utils/clipboard'
import { useToast } from '@/lib/toast/toast-context'

interface CopyButtonProps {
  text: string
  label?: string
  className?: string
}

export function CopyButton({ text, label, className }: CopyButtonProps) {
  const [copied, setCopied] = useState(false)
  const { toastError } = useToast()
  const { t } = useI18n()
  async function handleCopy() {
    const ok = await copyText(text)
    if (ok) {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } else {
      // 失败必须有明确反馈：否则用户看到按钮纹丝不动，会以为"无法复制"，
      // 而真实的根因（剪贴板被占用/权限被拒）完全被静默吞掉。
      toastError(t('components.copy.failed'))
    }
  }
  return (
    <button
      type="button"
      onClick={handleCopy}
      className={`fluent-focus inline-flex items-center gap-1 rounded-sm text-[13px] text-ink-3 transition duration-150 ease-fluent hover:text-brand ${className ?? ''}`}
    >
      <AppIcon name={copied ? 'check' : 'copy'} size={14} />
      {/* 复制成功后回落到 label：原写法是 {copied ? '已复制' : label}，
          而 label 允许缺省——一旦调用方不传 label，复制完成后按钮会变成
          一个只剩图标的空按钮（宽度塌陷、看不出是干什么的）。 */}
      {copied ? t('components.copy.copied') : (label ?? t('components.copy.copy'))}
    </button>
  )
}

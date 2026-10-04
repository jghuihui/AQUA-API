/** 表单控件：Input / Textarea / Select / Switch / Field。
 *
 * 意图（Why）：
 *   统一所有表单控件的视觉（对齐、边框、焦点态、禁用态），
 *   并把「标签 + 控件 + 帮助文案」组合成 Field，避免每个表单页面重复排版。
 *
 * Fluent 化要点：
 *   - 焦点态不是"整圈发光"，而是底边 2px 强调线（见 globals.css 的 .fluent-input）——
 *     表单里控件成列堆叠，整圈发光会让整列同时亮起来，底边线只在当前行给信号；
 *   - 圆角 4px（控件比容器更方）；
 *   - Switch 用 Fluent 的比例：40×20 轨道 + 12px 圆点，关闭态是"浅叠加底 + 描边"，
 *     打开态是实心强调色 —— 与 iOS 那种"纯色轨道 + 大圆点"区分开。
 */
'use client'

import { forwardRef, type InputHTMLAttributes, type ReactNode, type SelectHTMLAttributes, type TextareaHTMLAttributes } from 'react'

const controlBase =
  'fluent-input h-9.5 w-full rounded-sm border border-line-2 bg-card px-3 text-sm text-ink ' +
  'placeholder:text-ink-3 disabled:bg-layer disabled:text-ink-3 transition duration-150 ease-fluent'

/* ── Input / Textarea / Select ─────────────────────────── */

export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement>>(function Input(
  { className, ...rest },
  ref,
) {
  return <input ref={ref} className={`${controlBase} ${className ?? ''}`} {...rest} />
})

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaHTMLAttributes<HTMLTextAreaElement>>(
  function Textarea({ className, ...rest }, ref) {
    return <textarea ref={ref} className={`${controlBase} h-auto min-h-24 py-2 ${className ?? ''}`} {...rest} />
  },
)

export const Select = forwardRef<HTMLSelectElement, SelectHTMLAttributes<HTMLSelectElement>>(function Select(
  { className, children, ...rest },
  ref,
) {
  return (
    <select ref={ref} className={`fluent-select ${controlBase} appearance-none pr-8 ${className ?? ''}`} {...rest}>
      {children}
    </select>
  )
})

/* ── Switch：开关 ───────────────────────────────────────── */

interface SwitchProps {
  checked: boolean
  onChange: (checked: boolean) => void
  disabled?: boolean
  label?: string
}

export function Switch({ checked, onChange, disabled, label }: SwitchProps) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      // 用 ring-inset 画关闭态的描边而不是 border：border 会占用盒模型，
      // 导致"有边框的关闭态"和"无边框的打开态"轨道宽度差 2px，
      // 切换时圆点会轻微跳一下。
      className={`relative inline-flex h-5 w-10 shrink-0 items-center rounded-full ring-1 ring-inset
        transition-colors duration-150 ease-fluent disabled:opacity-40
        focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-brand
        ${checked ? 'bg-brand ring-transparent' : 'bg-layer-2 ring-line-2'}`}
    >
      {/* 位移量是算出来的：40(轨道) - 12(圆点) - 4(右侧留白) = 24px。
          写死 translate-x-5 会让圆点贴到轨道最右沿，看起来"顶出去了"。 */}
      <span
        className={`inline-block h-3 w-3 transform rounded-full transition-transform duration-150 ease-fluent ${
          checked ? 'translate-x-6 bg-on-brand' : 'translate-x-1 bg-ink-2'
        }`}
      />
    </button>
  )
}

/* ── Field：标签 + 控件 + 帮助 ──────────────────────────── */

interface FieldProps {
  label: string
  htmlFor?: string
  required?: boolean
  help?: string
  error?: string
  children: ReactNode
}

export function Field({ label, htmlFor, required, help, error, children }: FieldProps) {
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={htmlFor} className="text-[13px] font-medium text-ink-2">
        {label}
        {required && <span className="ml-0.5 text-err">*</span>}
      </label>
      {children}
      {error ? (
        <div className="text-xs text-err">{error}</div>
      ) : help ? (
        <div className="text-xs text-ink-3">{help}</div>
      ) : null}
    </div>
  )
}

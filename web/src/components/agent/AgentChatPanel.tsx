/** AgentChatPanel：流式对话面板（后台运维助手与门户在线客服共用）。
 *
 * 意图（Why）：
 *   两个入口的交互其实完全一样——打字机、工具过程提示、错误重试、清空会话。
 *   差别只在"用哪把钥匙说话"和"能不能显示工具"。把这份实现抄两份，
 *   下次改交互（比如加停止按钮）就必须记得改两个地方，漏一个就出现
 *   "后台能停、客服不能停"这种没人能解释的不一致。因此抽成本组件。
 *
 * 流转（Flow）：
 *   <AgentChatPanel ask={…} canShowTools showModelPicker />
 *     → ask()（后台传 chatWithOps，门户传 chatWithSupport）
 *     → 服务端 SSE → delta/tool/done/error → 本组件的状态机
 *
 * 扩展（Extend）：
 *   新增能力时只改本文件，不要在两个页面各写一份：
 *   - 要显示 token 消耗：读 done 事件里的 result（已在 onDone 回调中透出）
 *   - 要加"停止生成"：给 chatCall 传 AbortSignal 的回传句柄
 *
 * 【为什么不内置鉴权】
 *   门户客服用会话、公开客服用密钥、后台运维用管理员会话，
 *   三种凭据的获取方式完全不同。把它们塞进一个组件只会得到
 *   一堆布尔开关，而布尔开关迟早会被配错（最坏情况：把站长的
 *   会话令牌当成客服密钥发出去）。因此鉴权留在调用方，本组件只管渲染。
 *
 * 【为什么写操作的二次确认也放在本组件里】
 *   确认之后要把【同一句问题】带着凭证重发，而"同一句问题"和"接在第几条气泡上"
 *   都是本组件内部才知道的东西。放到页面层就得把 messages 状态整个交出去，
 *   或者让页面再实现一遍消息列表——两处消息列表迟早会不一致。
 *   客服角色没有工具，永远收不到 confirm 事件，因此这段逻辑对客服是死代码。
 *
 * 【为什么不内置 AgentConfirmDialog 之外的任何"确认方式"】
 *   危险操作的确认必须是一个模态弹窗，不能是行内的一行字或聊天气泡里的按钮：
 *   后者在长对话里会被滚出视野，而站长点确认时必须看得见全部影响面。
 */
'use client'

import { useCallback, useEffect, useRef, useState } from 'react'

import { AppIcon } from '@/components/AppIcon'
import { Button } from '@/components/ui/Button'
import type { AgentChatTurn, AgentConfirmRequest, AgentToolCall } from '@/api/agent'

import { AgentConfirmDialog } from './AgentConfirmDialog'

/** 面板里的一条消息 */
interface ChatMessage {
  id: string
  role: 'user' | 'assistant'
  content: string
  /** 助手消息正在流式输出（用于显示光标与禁用输入） */
  streaming?: boolean
  /** 本轮执行过的工具 */
  tools?: AgentToolCall[]
  /** 出错时的可读原因 */
  error?: string
}

/**
 * ask 发起一次对话。
 *
 * 由调用方注入而非在这里 import 具体的 chat 函数：三种入口的凭据不同，
 * 让调用方自己决定"拿什么去说话"，本组件只负责把事件画出来。
 *
 * 【input.confirm 的存在理由】
 *   确认不是"另发一条消息"，而是【把同一句问题重发一次并附上凭证】。
 *   若允许前端改成"我直接调这个接口执行"，就绕开了引擎里
 *   "工具名必须与被展示的一致"的那道校验。
 */
export type AskFn = (
  input: {
    question: string
    history: AgentChatTurn[]
    /**
     * 已获站长确认的操作凭证。
     *
     * 刻意只要求 tool_name 与 params，不接受整份 AgentConfirmRequest：
     * title / summary / risk_note 是给人看的展示字段，让它们出现在
     * 请求体里只会诱导调用方"照着改一改再发"。
     */
    confirm?: { tool_name: string; params: Record<string, unknown> }
  },
  handlers: {
    onDelta: (text: string) => void
    onTool: (name: string, mutating: boolean) => void
    /** 助手请求确认一个高危写操作；本组件据此弹窗，而不是当成失败 */
    onConfirm?: (confirm: AgentConfirmRequest) => void
    onDone: (result: { answer: string; tool_calls: AgentToolCall[]; prompt_tokens: number; completion_tokens: number }) => void
    onError: (message: string) => void
  },
  signal: AbortSignal,
) => Promise<void>

interface AgentChatPanelProps {
  ask: AskFn
  /** 是否展示工具过程（客服角色没有工具，此处传 false 可省掉一整块空白） */
  showTools?: boolean
  /** 顶部提示语；两处文案差别很大，由调用方给 */
  placeholder?: string
  /** 空态的引导标题与说明 */
  emptyTitle: string
  emptyDescription: string
  /** 输入框下方的额外说明（如"提问会消耗上游额度"） */
  footerHint?: string
  className?: string
}

/** 建议问题：只给四个，覆盖"最可能被问到的四件事"，而不是堆一屏。 */
const SUGGESTIONS = [
  '这个站怎么用，要从哪开始？',
  '支持哪些模型，怎么挑？',
  '我的余额和调用记录在哪看？',
  '遇到问题怎么联系人工？',
]

export function AgentChatPanel({
  ask,
  showTools = false,
  placeholder = '说点什么…（Enter 发送，Shift+Enter 换行）',
  emptyTitle,
  emptyDescription,
  footerHint,
  className,
}: AgentChatPanelProps) {
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [input, setInput] = useState('')
  const [busy, setBusy] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const bottomRef = useRef<HTMLDivElement | null>(null)
  // 输入框要一直可用：流式期间只是不能【发送】，禁用它会让用户
  // 起草下一句时失去输入框，表现为"打字打到一半焦点丢了"。
  const inputRef = useRef<HTMLTextAreaElement | null>(null)

  /*
   * 待确认的写操作。
   *
   * 为什么要把"原始问题 + 历史"一起存下来：确认后必须把【同一句问题】
   * 重发一次（服务端在那一次里才会真正执行）。如果只记住问题不记住历史，
   * 重发时模型看到的是一段缺了上文的对话，很可能改成追问而不是执行。
   *
   * 为什么用 ref 存原问题而不是从 messages 里反推：
   * messages 会被用户的新消息改变，而那份上下文必须与当初弹窗时一模一样。
   */
  const [pendingConfirm, setPendingConfirm] = useState<{
    request: AgentConfirmRequest
    question: string
    history: AgentChatTurn[]
  } | null>(null)
  // 确认后重发期间也要锁住发送：否则站长连点两次"确认"会并发两次写操作。
  const [confirmBusy, setConfirmBusy] = useState(false)

  // 新消息或流式增量都滚到底部。用 messages.length + 最后一条内容做依赖，
  // 比塞一个计数器更直接——依赖的就是真正影响布局的东西。
  const lastContent = messages.length ? messages[messages.length - 1].content : ''
  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth', block: 'end' })
  }, [messages.length, lastContent])

  // 组件卸载时中断在途请求：否则用户关掉页面后，
  // 上游还在替他烧钱跑到超时（服务端有 15 分钟总时限兜底，
  // 但没有理由让一个已经没人看的请求继续跑）。
  useEffect(() => () => abortRef.current?.abort(), [])

  /*
   * 跑一轮对话。
   *
   * 拆出这一层是因为"发新问题"和"确认后重发同一句"要共用几乎全部逻辑，
   * 差别只有三处：要不要再插一条用户气泡、要不要带上凭证、气泡接着用哪一条。
   * 这三处参数化之后，两条路径不可能各自漂移。
   */
  const runTurn = useCallback(
    async (opts: {
      text: string
      history: AgentChatTurn[]
      /** 带它就表示这是"确认后重发"，而不是新问题 */
      confirm?: AgentConfirmRequest
      /** 沿用已有气泡（确认重发时用），不新建 */
      botId?: string
    }) => {
      const { text, history, confirm, botId: reuseId } = opts
      const botId = reuseId ?? `a-${Date.now()}`

      if (!reuseId) {
        const userMsg: ChatMessage = { id: `u-${Date.now()}`, role: 'user', content: text }
        const botMsg: ChatMessage = { id: botId, role: 'assistant', content: '', streaming: true }
        setMessages((prev) => [...prev, userMsg, botMsg])
      } else {
        // 确认重发：把上一轮那句"等你确认"清掉，改为继续流式输出。
        // 留着它的话，站长会看到助手先说"等你确认"、紧接着又自己说"已处理"，
        // 像是两个不同的动作。
        setMessages((prev) =>
          prev.map((m) =>
            m.id === botId
              ? { ...m, streaming: true, content: '', error: undefined, tools: [] }
              : m,
          ),
        )
      }

      setBusy(true)

      const controller = new AbortController()
      abortRef.current = controller

      await ask(
        confirm
          ? {
              question: text,
              history,
              // 只送 tool_name 与 params：服务端确认单里的 title/summary 是
              // 给人看的，回传它们没有任何意义，还会让"展示什么"与"执行什么"
              // 看起来像是两套数据。
              confirm: { tool_name: confirm.tool_name, params: confirm.params },
            }
          : { question: text, history },
        {
          onDelta: (chunk) => {
            setMessages((prev) =>
              prev.map((m) => (m.id === botId ? { ...m, content: m.content + chunk } : m)),
            )
          },
          onTool: (name, mutating) => {
            setMessages((prev) =>
              prev.map((m) =>
                m.id === botId
                  ? { ...m, tools: [...(m.tools ?? []), { name, mutating, ok: true }] }
                  : m,
              ),
            )
          },
          onConfirm: (request) => {
            /*
             * 收到确认请求时服务端不会再发 done：它手里没有工具结果可以回填给模型，
             * 本轮就此中断。这里必须把气泡停住并留一句人话，
             * 否则界面会一直转下去，站长既看不到发生了什么也不知道该做什么。
             */
            setMessages((prev) =>
              prev.map((m) =>
                m.id === botId
                  ? { ...m, streaming: false, content: `要执行「${request.title}」，等你确认。` }
                  : m,
              ),
            )
            setPendingConfirm({ request, question: text, history })
          },
          onDone: (result) => {
            setMessages((prev) =>
              prev.map((m) =>
                m.id === botId
                  ? { ...m, streaming: false, content: result.answer || m.content, tools: result.tool_calls }
                  : m,
              ),
            )
          },
          onError: (message) => {
            setMessages((prev) =>
              prev.map((m) => (m.id === botId ? { ...m, streaming: false, error: message } : m)),
            )
          },
        },
        controller.signal,
      )
        .catch((err: unknown) => {
          // fetch 本身失败（断网、CORS、服务进程重启）时事件一个都收不到，
          // 错误只在这里冒出来。不兜住的话界面会永远停在"正在思考"。
          const message = err instanceof Error ? err.message : '连接中断，请重试'
          setMessages((prev) =>
            prev.map((m) => (m.id === botId ? { ...m, streaming: false, error: message } : m)),
          )
        })
        .finally(() => {
          abortRef.current = null
          setBusy(false)
          setConfirmBusy(false)
          inputRef.current?.focus()
        })
    },
    [ask],
  )

  const send = useCallback(
    async (question: string) => {
      const text = question.trim()
      // confirmBusy 也要挡：确认重发期间若允许再发一条，两轮会并发写同一批数据。
      // pendingConfirm 也要挡：那表示还有一个没处理完的确认单，
      // 此刻发新问题会让"确认的是哪一句"变得无法追溯。
      if (!text || busy || confirmBusy || pendingConfirm) return

      const history: AgentChatTurn[] = messages
        .filter((m) => !m.error && m.content.trim())
        .map((m) => ({ role: m.role, content: m.content }))

      setInput('')
      await runTurn({ text, history })
    },
    [busy, confirmBusy, messages, pendingConfirm, runTurn],
  )

  /*
   * 站长点了"确认执行"。
   *
   * 这里【不发任何新消息】：服务端需要的是"同一句问题 + 凭证"，
   * 新发一句"继续"会让模型重新判断一遍要不要做这件事——而我们要的恰恰是
   * 把它已经决定好的那件事做完。
   */
  const handleConfirmAccept = useCallback(async () => {
    if (!pendingConfirm || confirmBusy) return
    const { request, question, history } = pendingConfirm
    setConfirmBusy(true)
    setPendingConfirm(null)
    await runTurn({ text: question, history, confirm: request })
  }, [confirmBusy, pendingConfirm, runTurn])

  /** 站长点了"取消"：不发请求，只把气泡改成一句实话。 */
  const handleConfirmCancel = useCallback(() => {
    setPendingConfirm(null)
    setMessages((prev) =>
      prev.map((m, i) =>
        i === prev.length - 1 && m.role === 'assistant'
          ? { ...m, content: `${m.content}\n（已取消，没有任何改动）` }
          : m,
      ),
    )
  }, [])

  function handleStop() {
    abortRef.current?.abort()
    abortRef.current = null
    setBusy(false)
    // 停止时若正等着确认，那份确认单一并作废：
    // 站长可能已经转身去别处改了数据，回来再点确认就成了一次意外写入。
    setPendingConfirm(null)
    setConfirmBusy(false)
    setMessages((prev) =>
      prev.map((m) => (m.streaming ? { ...m, streaming: false } : m)),
    )
  }

  function handleClear() {
    abortRef.current?.abort()
    abortRef.current = null
    setBusy(false)
    setPendingConfirm(null)
    setConfirmBusy(false)
    setMessages([])
    setInput('')
  }

  function handleKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    // Enter 发送、Shift+Enter 换行：对话里换行的频率远低于发送。
    // 但 IME 组字期间的 Enter（中文候选确认）必须放行——
    // 否则中文用户每选一次词就发出一条半截消息。
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault()
      // 有未处理的确认单时忽略：确认必须由弹窗上的按钮明确表态，
      // 不能被一次回车顺带触发 —— 那不叫二次确认。
      if (pendingConfirm) return
      void send(input)
    }
  }

  return (
    <div className={`flex h-full min-h-0 flex-col ${className ?? ''}`}>
      {/* 消息区 */}
      <div className="min-h-0 flex-1 space-y-4 overflow-y-auto px-1 py-2">
        {messages.length === 0 ? (
          <div className="flex h-full flex-col items-center justify-center px-4 text-center">
            <div className="mb-3 flex h-12 w-12 items-center justify-center rounded-full bg-brand/10 text-brand">
              <AppIcon name="chat" className="h-6 w-6" />
            </div>
            <div className="text-[15px] font-semibold text-ink">{emptyTitle}</div>
            <p className="mt-1.5 max-w-md text-[13px] leading-relaxed text-ink-3">
              {emptyDescription}
            </p>
            <div className="mt-5 flex flex-wrap justify-center gap-2">
              {SUGGESTIONS.map((s) => (
                <button
                  key={s}
                  type="button"
                  disabled={busy}
                  onClick={() => void send(s)}
                  className="rounded-full border border-line-2 bg-card px-3 py-1.5 text-[13px] text-ink-2 transition hover:border-brand/40 hover:text-brand disabled:opacity-50"
                >
                  {s}
                </button>
              ))}
            </div>
          </div>
        ) : (
          <>
            {messages.map((m) => (
              <MessageBubble key={m.id} message={m} showTools={showTools} />
            ))}
            <div ref={bottomRef} />
          </>
        )}
      </div>

      {/* 输入区 */}
      <div className="mt-3 flex-none border-t border-line pt-3">
        <div className="flex items-end gap-2">
          <textarea
            ref={inputRef}
            value={input}
            rows={2}
            disabled={busy}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={handleKeyDown}
            placeholder={busy ? '助手正在回答…' : placeholder}
            className="max-h-40 min-h-[4.5rem] flex-1 resize-none rounded-md border border-line-2 bg-card px-3 py-2 text-sm text-ink placeholder:text-ink-3 focus:border-brand focus:ring-2 focus:ring-brand/15 focus:outline-none disabled:bg-surface disabled:text-ink-3"
          />
          {busy ? (
            <Button variant="secondary" onClick={handleStop} className="shrink-0">
              停止
            </Button>
          ) : (
            <Button
              variant="primary"
              // 确认单还没处理完时禁用发送：弹窗是遮罩，站长点不到这个按钮，
              // 但键盘 Enter 能触发 handleKeyDown 里的 send。
              disabled={!input.trim() || pendingConfirm !== null}
              onClick={() => void send(input)}
              className="shrink-0"
            >
              发送
            </Button>
          )}
          {messages.length > 0 && (
            <Button variant="ghost" onClick={handleClear} className="shrink-0">
              清空
            </Button>
          )}
        </div>
        {footerHint && <p className="mt-2 text-[12px] text-ink-3">{footerHint}</p>}

        {/*
          等确认期间在输入区上方留一条提示。
          不加的话站长只会看到输入框突然不能用了，不知道是被弹窗挡着
          还是助手卡住了——这两种情况在他眼里长得一模一样。
        */}
        {pendingConfirm && (
          <p className="mt-2 rounded-md border border-warn/30 bg-warn/10 px-3 py-2 text-[12px] text-warn">
            助手准备执行一个会影响线上数据的操作，请在上方弹窗里确认或取消。
          </p>
        )}
      </div>

      <AgentConfirmDialog
        request={pendingConfirm?.request ?? null}
        busy={confirmBusy}
        onConfirm={() => void handleConfirmAccept()}
        onCancel={handleConfirmCancel}
      />
    </div>
  )
}

/* ── 单条消息 ─────────────────────────────────────────────── */

function MessageBubble({ message, showTools }: { message: ChatMessage; showTools: boolean }) {
  const isUser = message.role === 'user'
  return (
    <div className={`flex gap-2.5 ${isUser ? 'flex-row-reverse' : ''}`}>
      <div
        className={`mt-0.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-full text-[11px] font-semibold ${
          isUser ? 'bg-ink/10 text-ink-2' : 'bg-brand/10 text-brand'
        }`}
      >
        {isUser ? '我' : <AppIcon name="sparkles" className="h-4 w-4" />}
      </div>
      <div className={`min-w-0 max-w-[80%] ${isUser ? 'items-end' : ''} flex flex-col`}>
        <div
          className={`rounded-lg px-3.5 py-2 text-[13px] leading-relaxed ${
            isUser
              ? 'bg-brand text-on-brand'
              : 'border border-line bg-card text-ink whitespace-pre-wrap break-words'
          }`}
        >
          {message.content}
          {message.streaming && (
            <span className="ml-0.5 inline-block h-3.5 w-[2px] translate-y-0.5 animate-pulse bg-brand" />
          )}
        </div>

        {message.error && (
          <div className="mt-1.5 rounded-md border border-err/25 bg-err/5 px-3 py-2 text-[12px] text-err">
            {message.error}
          </div>
        )}

        {showTools && (message.tools?.length ?? 0) > 0 && (
          <div className="mt-1.5 flex flex-wrap gap-1.5">
            {message.tools?.map((t, i) => (
              <span
                key={`${t.name}-${i}`}
                className={`inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[11px] ${
                  t.ok === false
                    ? 'border-err/25 bg-err/5 text-err'
                    : t.mutating
                      ? 'border-warn/30 bg-warn/10 text-warn'
                      : 'border-line-2 bg-surface text-ink-3'
                }`}
              >
                <AppIcon name={t.mutating ? 'alert' : 'check'} className="h-3 w-3" />
                {t.name}
                {t.mutating && '（会改动配置）'}
                {t.ok === false && (t.error ? `：${t.error}` : ' 失败')}
              </span>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}

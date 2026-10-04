// AI Agent 在服务层的装配。
//
// 意图（Why）：
//
//	agent 包是纯领域层：它知道自己要查渠道、要调模型，但不该知道
//	"这些仓储从哪来""HTTP 客户端从哪来"。本文件是那层接线，
//	职责只有一件：把 Deps 里已有的依赖组装成 agent.Engine。
//
// 流转（Flow）：
//
//	server.New  → buildAgentEngine()（一次性）→ s.agent
//	请求到达     → agentEngine() 取已建好的实例（不加锁）
//
// 扩展（Extend）：
//
//	要给 agent 加能力：先在 agent 包加工具（internal/agent/tools.go），
//	再在本文件把对应仓储接进 AgentTools——
//	两处缺一不可，只有前者会得到"工具报未配置"，只有后者是死代码。
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/agent"
	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// agentUpstreamClient 是 agent 调用上游模型用的 HTTP 客户端。
//
// 与网关转发用的客户端分开的原因：agent 的超时（120 秒）远短于
// 转发的 300 秒，且它不需要连接池级别的并发能力
// （agent 调用频率极低，一个用户一天问不了几次）。
// 复用转发客户端会让 agent 卡在转发那个更长的超时上，
// 表现为"问一句要等五分钟才报错"。
var agentUpstreamClient = &http.Client{
	Timeout: 120 * time.Second,
}

// errAgentNotConfigured 表示 agent 依赖未接入。
//
// 单独的错误类型而不是复用 agent 包的错误：调用方要据此返回 503
// （"功能未启用"），而不是 500（"服务坏了"）——
// 前者提示站长去配置，后者会让人以为程序有 bug。
var errAgentNotConfigured = errors.New("agent: 未接入依赖，功能未启用")

// buildAgentEngine 构造 agent 引擎；依赖不足时返回错误。
//
// 一次性构造而非每次请求新建：Engine 持有工具层（无状态），
// 每次新建等于每次重建一套工具依赖——高频调用下是纯粹的浪费。
//
// ctx 来自惰性构建路径（agentEngine），那里没有请求上下文，
// 传 context.Background() 是安全的：读一次配置失败也只是退回渠道。
func (s *Server) buildAgentEngine(ctx context.Context) (*agent.Engine, error) {
	if s.deps.Channels == nil || s.deps.ChannelKeys == nil {
		// 没有渠道就选不出上游，没有密钥池就发不出请求。
		// 这两项是模型转发的核心依赖，缺失说明部署不完整。
		return nil, errAgentNotConfigured
	}
	endpoint, err := s.loadAgentEndpoint(ctx)
	if err != nil {
		slog.Warn("读取 agent 独立上游配置失败，本次改用站点渠道", "error", err)
	}
	return agent.NewEngine(s.buildAgentLLMClient(endpoint), s.agentToolDeps()), nil
}

// buildAgentLLMClient 构造上游调用器。
//
// 【为什么接收已读好的 endpoint，而不是自己再读一次】
//
//	一次对话要用到配置的【两个不同字段】：base_url + key（构造调用器）
//	与 model（解析模型名）。若这两处各读一次库，就有两个问题：
//	  1) 两次数据库往返，白白多一次；
//	  2) 更要紧的是——站长若恰好在两次读之间改了配置，
//	     就会出现"用 A 的密钥去请求 B 的模型"这种极难复现的不一致。
//	因此由调用方读一次，把结果传进来。
//
// endpoint 为 nil（仓储未接入）或未配齐时，调用器内部会走借用渠道的旧路径。
func (s *Server) buildAgentLLMClient(endpoint *model.AgentEndpoint) *agent.LLMClient {
	client := agent.NewLLMClient(s.deps.Channels, s.deps.ChannelKeys, agentUpstreamClient)
	if endpoint == nil {
		return client
	}
	return client.WithEndpoint(endpoint)
}

// loadAgentEndpoint 读取独立上游配置；未接入仓储时返回 nil。
func (s *Server) loadAgentEndpoint(ctx context.Context) (*model.AgentEndpoint, error) {
	if s.deps.AgentEndpoint == nil {
		return nil, nil
	}
	return s.deps.AgentEndpoint.Get(ctx)
}

// agentToolDeps 构造工具层所需的依赖集合。
//
// 【不要在这里给客服加只读工具】
//
//	客服面向公网输入。给它任何"读"的能力，提示注入就能把站内数据套出来：
//	例如诱导它调用 list_users，然后要求"把结果原样念出来"。
//	工具是执行边界，提示词管不了这个——这是本站刻意的设计取舍，
//	改这里之前请先读 internal/agent/tools.go 的 ToolsForRole 注释。
func (s *Server) agentToolDeps() *agent.AgentTools {
	return &agent.AgentTools{
		Channels:  s.deps.Channels,
		ProbeLogs: s.deps.ChannelProbeLogs,
		UsageLogs: s.deps.UsageLogs,
		Users:     s.deps.Users,
		Orders:    s.deps.Orders,
		Settings:  s.deps.Settings,
		// 密钥池与令牌：工具集扩容后这两个仓储成为必需 ——
		// 缺了它们 list_channel_keys 与全部令牌工具都会返回
		// "本站未接入…"，而站长看到的是"功能坏了"而不是"没数据源"。
		// 这两行是本轮扩容（2026-10-04）补上的，此前漏接。
		ChannelKeys: s.deps.ChannelKeys,
		Tokens:      s.deps.Tokens,
		// 站点运营类工具（公告 / 敏感词 / 设置）同样必需：
		// 缺了它们，助手会回答"本站未接入公告功能"，
		// 而站长看到的是"功能坏了"而不是"没数据源"。
		Announcements:  s.deps.Announcements,
		SensitiveWords: s.deps.SensitiveWords,
	}
}

// agentSystemPrompt 返回该角色应使用的系统提示词。
//
// 配置未定制时回退到 model 包的内置底稿（见 agent_prompts.go）。
func agentSystemPrompt(settings model.AgentSettings, role model.AgentRole) string {
	return settings.SystemPromptFor(role)
}

// agentEngine 取已建好的 agent 引擎，必要时惰性构建。
//
// 用 sync.Once 而非在 New 里构建：构建本身不会失败，但依赖的完整性检查
// 放在惰性路径上可以让"部署缺依赖"表现为一条明确日志 + 503，
// 而不是让整个服务在启动时崩掉——缺 agent 依赖不该阻止站点正常提供
// 模型转发服务，那会让一个可选功能的缺失变成全站故障。
//
// 之所以要缓存：每次请求重建 LLMClient 会新建连接池（见 buildAgentEngine）。
func (s *Server) agentEngine() (*agent.Engine, error) {
	s.agentOnce.Do(func() {
		s.agent, s.agentErr = s.buildAgentEngine(context.Background())
	})
	return s.agent, s.agentErr
}

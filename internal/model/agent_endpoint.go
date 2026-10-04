// agent 的独立上游配置（base_url + api_key + 模型）。
//
// 意图（Why）：
//
//	agent 此前只能借用站点渠道调模型，于是有三个绕不开的问题：
//	  1) 与业务流量抢配额。渠道被限流时先挂的是助手，而助手一轮对话要发
//	     多次请求，最容易被限流打断；
//	  2) 站长想给问答任务单独挑一个便宜/快的模型，但改渠道会影响所有业务；
//	  3) 站长手上的某个第三方上游可能不支持工具调用（tool_calls），
//	     而运维 agent 的价值恰恰在工具调用上 —— 表现为"能聊天但一查数据就失败"。
//
//	给 agent 一套独立的 base_url + api_key ，上述三个问题一起消失，
//	且不必为此新建一个正式渠道（那意味着它会出现在模型广场、
//	参与路由、参与健康巡检，还要额外交代模型清单）。
//
// 安全性（必须清楚，这条是设计前提）：
//
//	API Key 与 SMTP 口令同一处置——加密落库（AES-GCM，密钥由 AQUA_APP_KEY 派生）、
//	接口永不回传、界面只能重填不能查看。
//	详见 internal/store/migrations/sqlite/0056_agent_endpoint.sql 的注释。
//
// 流转（Flow）：
//
//	后台保存 → AgentEndpointRepository.Save（api_key 加密后 upsert）
//	→ buildAgentEngine 时读出 → agent.NewLLMClientWithEndpoint
//	→ 每次对话优先走独立上游；未启用则回退借用站点渠道
//
// 扩展（Extend）：
//
//	要支持"每角色独立上游"（客服用一个、运维用另一个）：
//	在此结构加两个实例并按角色取用，不要改成 map —— 那会让
//	"哪一角色用哪一套"这个判定从数据层挪到调用处，迟早漏判。
package model

import (
	"context"
	"errors"
	"strings"
)

// agent_endpoint 的设置类别标记（本表不入 KV settings，故单独说明）。
//
// 为什么不用 SettingRepository：
//   见 0056 迁移注释 —— base_url 与 api_key 必须成对存在才算配置完整，
//   拆成 KV 后"算不算配好了"这个判定会散落多处。

// AgentEndpointKind 是独立上游支持的协议类型。
type AgentEndpointKind string

const (
	// AgentEndpointOpenAI 是 OpenAI 兼容协议（本站绝大多数第三方上游）。
	AgentEndpointOpenAI AgentEndpointKind = "openai"
	// AgentEndpointAnthropic 是 Anthropic 原生协议（/v1/messages）。
	AgentEndpointAnthropic AgentEndpointKind = "anthropic"
)

// Valid 报告该协议类型是否受支持。
func (k AgentEndpointKind) Valid() bool {
	switch k {
	case AgentEndpointOpenAI, AgentEndpointAnthropic:
		return true
	default:
		return false
	}
}

// OrOpenAI 返回该协议类型，空值（未配置/未识别）时回退为 OpenAI 兼容。
//
// 为什么默认 OpenAI 而不是报错：第三方上游里绝大多数是 OpenAI 兼容协议，
// 而"必须先知道有这个协议才能选它"对小白是额外门槛。
// 真选错了（比如选了 anthropic 却把地址填成 OpenAI 的），
// 失败会体现在实际请求上，那时的报错（404/405）已经足够指明问题。
func (k AgentEndpointKind) OrOpenAI() AgentEndpointKind {
	if k.Valid() {
		return k
	}
	return AgentEndpointOpenAI
}

// Label 返回中文显示名（供界面下拉框直接使用）。
func (k AgentEndpointKind) Label() string {
	switch k {
	case AgentEndpointAnthropic:
		return "Anthropic 原生（/v1/messages）"
	default:
		return "OpenAI 兼容（/v1/chat/completions）"
	}
}

// AgentEndpoint 是 agent 专用的上游配置。
type AgentEndpoint struct {
	// BaseURL 是上游地址，形如 https://api.example.com/v1（带不带 /v1 都支持，
	// 拼接时去重——见 agent 包里的 joinUpstreamURL）。
	BaseURL string

	// APIKey 是解密后的明文密钥。仅存在于进程内存，
	// 绝不入日志、绝不下发给前端（接口只给 APIKeySet）。
	APIKey string

	// APIKeySet 表示库里已配置密钥（明文为空的唯一合法原因就是"从未配置或被清空"）。
	// 界面据此显示"已配置，留空表示不修改"。
	APIKeySet bool

	// Model 是默认模型名。留空表示沿用「运行配置」里的模型——
	// 这样"只换上游不换模型"是一次零成本操作。
	Model string

	// Kind 是协议类型。空值按 OpenAI 兼容处理（绝大多数情况）。
	Kind AgentEndpointKind

	// Enabled 表示是否启用独立上游。
	// 关闭 = 退回"借用站点渠道"，这是首次部署的默认状态。
	Enabled bool
}

// Configured 报告这份配置是否【完整可用】。
//
// 为什么要有这个判定而不是各处直接看 BaseURL != ""：
//
//	base_url 与 api_key 必须成对存在。只判断 URL 的话，
//	"配了地址没配密钥"会被当成配好了，然后每次对话都拿着空密钥去请求上游，
//	表现为上游 401 —— 而站长看到的是"助手用不了"，完全看不出少配了密钥。
//	让判定只有一处，界面才能据此明确显示"还没配完"。
func (e *AgentEndpoint) Configured() bool {
	if e == nil {
		return false
	}
	return e.Enabled && strings.TrimSpace(e.BaseURL) != "" && strings.TrimSpace(e.APIKey) != ""
}

// Validate 校验配置是否自洽，返回给站长看的具体原因。
//
// 刻意允许"只填一半"并给出明确提示而不是直接拒绝保存：
// 站长经常先填地址、复制密钥的那一刻再回来补。这里拦下来只会让他
// 以为自己填错了方向，而实际上他只是还没填完。
// 真正不可用的是 Configured() 那一侧，界面会显式标出来。
func (e *AgentEndpoint) Validate() error {
	if e == nil {
		return nil
	}
	if !e.Enabled {
		// 未启用时下面几项都只是"预填"，不校验。
		// 否则会出现"先填地址再打开开关"这种正常顺序被拦下的情况。
		return nil
	}
	// 【顺序：先看地址与密钥，再看协议类型】
	//
	// 反过来（先判 kind）会出一条很糟的错：站长只填了地址没填密钥，
	// 而请求里也没带 kind（空值是合法的、按 OpenAI 处理），
	// 于是 kind 校验先命中，报"协议类型不受支持"——
	// 而他根本没碰过那个下拉框。这条信息不指向任何他做过的操作。
	// 报"还缺 API Key"才有可执行性。
	//
	// 因此这里的 kind 判定只拦【非空且不受支持】的值，
	// 空值交给 OrOpenAI 处理（与仓储读取时的归一保持一致）。
	if raw := AgentEndpointKind(e.Kind); strings.TrimSpace(string(raw)) != "" && !raw.Valid() {
		return ErrAgentEndpointKind
	}
	base := strings.TrimSpace(e.BaseURL)
	key := strings.TrimSpace(e.APIKey)
	if base == "" && key == "" {
		return ErrAgentEndpointEmpty
	}
	// 一半的情况给出可执行提示（告诉用户还差哪一项），而不是笼统的"配置无效"
	if base == "" {
		return ErrAgentEndpointMissingBaseURL
	}
	// 注意这里不看 APIKeySet：编辑页保存时密钥输入框必然是空的（接口不回显），
	// 那种"空"是"沿用原值"而不是"没配"。真正该拦的是"从来就没配过"。
	if key == "" && !e.APIKeySet {
		return ErrAgentEndpointMissingAPIKey
	}
	return nil
}

// agent endpoint 配置的语义化错误。
//
// 分开三个"缺一半"的错误而不是笼统一个"配置无效"：
// 站长看到"还缺接口地址"就知道该去补哪一栏，不用自己去猜。
var (
	ErrAgentEndpointKind           = errors.New("model: 协议类型不受支持")
	ErrAgentEndpointEmpty          = errors.New("启用独立上游需要同时填写接口地址与 API Key")
	ErrAgentEndpointMissingBaseURL = errors.New("已填写 API Key，但还缺接口地址")
	ErrAgentEndpointMissingAPIKey  = errors.New("已填写接口地址，但还缺 API Key")
)

// AgentEndpointRepository 读写独立上游配置。
type AgentEndpointRepository interface {
	// Get 读取配置；从未保存过时返回 (&AgentEndpoint{}, nil) 而非 nil——
	// 调用方只想读，不想处理"这是不是第一次配置"这件事。
	Get(ctx context.Context) (*AgentEndpoint, error)

	// Save 保存（单行 upsert）。实现负责加密 API Key。
	Save(ctx context.Context, endpoint *AgentEndpoint) error
}

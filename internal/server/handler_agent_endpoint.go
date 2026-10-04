// AI Agent 独立上游配置的后台接口（base_url + api_key + 模型）。
//
// 意图（Why）：
//
//	让站长在后台直接填一整套与站点渠道无关的上游配置，
//	不必为了给助手换个模型而去「渠道管理」里新建一个正式渠道
//	（那会把它暴露给模型广场、路由与健康巡检，还要额外交代模型清单）。
//
// 【本文件最重要的约束：API Key 永不回传】
//
//	GET 只返回 api_key_set（是否已配置），PUT 时留空表示沿用原值。
//	这是 internal/store/migrations/sqlite/0023_smtp_settings 的同一套做法，
//	理由也一样：后台能随时取回明文，等于"数据库被读走"直接等于凭据失守。
//	因此这里既不返回明文，也不返回密文。
//
// 流转（Flow）：
//
//	GET /api/admin/agent/endpoint → 读配置（解密仅供服务端内部使用，不下发）
//	PUT /api/admin/agent/endpoint → 校验 → Save（密钥留空则沿用旧密文）
//	下一次对话 → buildAgentLLMClient(endpoint) 优先走独立上游
//
// 扩展（Extend）：
//
//	要"测试连通性"：加 POST /api/admin/agent/endpoint/test，
//	用一个最小请求去打上游并回传状态码。注意别在这里直接把上游响应体
//	原样回传——它可能含内部地址或账号信息。
package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/oai"
)

// agentEndpointResponse 是独立上游配置的响应。
//
// 刻意不含 APIKey 字段：不是遗漏，是安全要求（见文件头）。
// 前端要显示"已配置"，用 APIKeySet 就够了。
type agentEndpointResponse struct {
	// BaseURL 是上游地址，回显以便站长继续编辑。
	BaseURL string `json:"base_url"`
	// APIKeySet 表示库里已配置密钥；前端据此显示"留空表示不修改"。
	APIKeySet bool `json:"api_key_set"`
	// Model 是独立上游的默认模型名，留空表示沿用「运行配置」。
	Model string `json:"model"`
	// Kind 是协议类型（openai / anthropic）。
	Kind string `json:"kind"`
	// KindOptions 给前端渲染下拉框用，避免前端硬编码协议列表。
	//
	// 为什么不写死在前端：协议类型是后端的概念（agent 包按它选请求格式），
	// 前端硬编码一份列表，后端将来加一种协议时会漏掉，
	// 而表现是"下拉框里选不了刚支持的那种"。
	KindOptions []agentEndpointKindOption `json:"kind_options"`
	// Enabled 表示是否启用独立上游（关掉 = 借用站点渠道）。
	Enabled bool `json:"enabled"`
	// Configured 表示地址与密钥是否【成对齐全】。
	//
	// 前端据此显示"还没配完，还差 API Key"这类提示。
	// 单独下发是因为前端算不出来：它只知道"密钥没回显"，
	// 无法区分"从未配置"与"已配置但本次留空未修改"。
	Configured bool `json:"configured"`
}

// agentEndpointKindOption 是协议类型下拉框的一项。
type agentEndpointKindOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// agentEndpointRequest 是保存请求。
//
// 全部用指针：必须能区分"没传这一项（沿用）"与"传了空串（清空）"。
// 尤其 APIKey——它的"空"是"不修改"而不是"清除"，
// 但如果某天要支持"清除密钥"，就需要一个显式的清空开关而不是靠空串。
type agentEndpointRequest struct {
	BaseURL *string `json:"base_url"`
	APIKey  *string `json:"api_key"`
	Model   *string `json:"model"`
	Kind    *string `json:"kind"`
	Enabled *bool   `json:"enabled"`
}

// handleGetAgentEndpoint 处理 GET /api/admin/agent/endpoint。
func (s *Server) handleGetAgentEndpoint(c *gin.Context) {
	if s.deps.AgentEndpoint == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable, "独立上游配置功能未启用",
			oai.TypeServer, "agent_endpoint_unavailable")
		return
	}
	endpoint, err := s.deps.AgentEndpoint.Get(c.Request.Context())
	if err != nil {
		// 这里的错误里有"密钥解密失败"这一类，而它对站长极其重要：
		// 意味着他配的密钥因为 AQUA_APP_KEY 变更而读不出来了，
		// 需要重新填。不能糊成一句"读取失败"。
		s.respondInternalError(c, "读取 agent 独立上游配置失败", err)
		return
	}
	c.JSON(http.StatusOK, buildAgentEndpointResponse(endpoint))
}

// handleUpdateAgentEndpoint 处理 PUT /api/admin/agent/endpoint。
//
// 【保存时的字段合并规则】
//
//	指针为 nil = 沿用原值；非 nil（哪怕是空串）= 覆盖。
//	唯一例外是 APIKey：空串表示"不修改"，因为界面上它必然是空的
//	（见文件头），把那个空当成"清除"会抹掉站长的密钥。
func (s *Server) handleUpdateAgentEndpoint(c *gin.Context) {
	if s.deps.AgentEndpoint == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable, "独立上游配置功能未启用",
			oai.TypeServer, "agent_endpoint_unavailable")
		return
	}

	/*
	 * 改 base_url 就是改【所有对话往哪发】——包括用户的订单号、余额、
	 * 以及站长在助手框里打过的任何内容。指向一个第三方地址等于把这些
	 * 交出去，且不像发密钥那样有"一次性明文"之类的显眼痕迹。
	 * 所以它与发密钥同属高危，用同一档 2 分钟窗口。
	 */
	if !s.requireFreshReauthStrict(c) {
		return
	}

	var req agentEndpointRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	current, err := s.deps.AgentEndpoint.Get(c.Request.Context())
	if err != nil {
		s.respondInternalError(c, "读取 agent 独立上游配置失败", err)
		return
	}

	next := *current
	// 注意 next.APIKey 不从 current 复制明文——下面直接用请求里的值。
	// 若在这里把 current.APIKey 带过去，Save 就会把旧密钥重新加密一遍，
	// 产生一个与库中不同的新密文（AEAD 带随机 nonce），
	// 每次保存都让"这行动过没有"变得没法判断。
	if req.BaseURL != nil {
		next.BaseURL = strings.TrimSpace(*req.BaseURL)
	}
	if req.Model != nil {
		next.Model = strings.TrimSpace(*req.Model)
	}
	if req.Kind != nil {
		next.Kind = model.AgentEndpointKind(strings.TrimSpace(*req.Kind))
	}
	if req.Enabled != nil {
		next.Enabled = *req.Enabled
	}
	next.APIKey = ""
	if req.APIKey != nil {
		next.APIKey = strings.TrimSpace(*req.APIKey)
	}

	if err := next.Validate(); err != nil {
		// 校验失败一律 400，并把 domain 层的具体原因原样带给站长：
		// 这些文案本身就是可执行提示（"还缺接口地址"），
		// 换成笼统的"配置无效"等于把判断责任推回给用户。
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "agent_endpoint_invalid")
		return
	}

	if err := s.deps.AgentEndpoint.Save(c.Request.Context(), &next); err != nil {
		s.respondInternalError(c, "保存 agent 独立上游配置失败", err)
		return
	}

	// 保存后回读再返回：让前端渲染的是【真实落库的结果】而不是它提交的乐观值。
	// 典型差异是"沿用了旧密钥"——前端提交时空着密钥框，
	// 回读才能告诉它 configured 仍然是 true。
	saved, err := s.deps.AgentEndpoint.Get(c.Request.Context())
	if err != nil {
		s.respondInternalError(c, "保存后读取 agent 独立上游配置失败", err)
		return
	}
	c.JSON(http.StatusOK, buildAgentEndpointResponse(saved))
}

// buildAgentEndpointResponse 组装响应。
//
// endpoint 为 nil 时给一份"全空且未启用"的默认配置：
// 仓储契约保证 Get 不返回 nil（见 agent_endpoint_repo.Get），
// 但这里仍做判空——一个 nil 解引用会让整个后台页面白屏，
// 代价远大于多写一个分支。
func buildAgentEndpointResponse(endpoint *model.AgentEndpoint) agentEndpointResponse {
	if endpoint == nil {
		endpoint = &model.AgentEndpoint{}
	}
	kind := endpoint.Kind.OrOpenAI()
	return agentEndpointResponse{
		BaseURL:    endpoint.BaseURL,
		APIKeySet:  endpoint.APIKeySet,
		Model:      endpoint.Model,
		Kind:       string(kind),
		Enabled:    endpoint.Enabled,
		Configured: endpoint.Configured(),
		KindOptions: []agentEndpointKindOption{
			{Value: string(model.AgentEndpointOpenAI), Label: model.AgentEndpointOpenAI.Label()},
			{Value: string(model.AgentEndpointAnthropic), Label: model.AgentEndpointAnthropic.Label()},
		},
	}
}

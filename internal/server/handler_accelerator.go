// 加速器的后台接口（配置读写 + 效果视图）。
//
// 意图（Why）：
//
//	站长的诉求是"让 API 更快"，而"快"由三件互不相干的事决定：
//	选路是否挑快的、连接是否被复用、上游缓存有没有命中。
//	这三件事此前散落在代码里（写死的连接池参数、从不被读的 LatencyMS），
//	既没有开关也没有任何反馈 —— 站长改了也看不出效果，
//	于是"加速"只能是站长凭感觉猜。
//
//	本文件把它们收成一个面板：
//	  - GET  返回当前配置【以及效果数据】（不是只返回开关）
//	  - PUT  保存并热更新（不需要重启服务）
//
// 【为什么 GET 必须返回效果数据，而不只是配置】
//
//	一个只有开关没有数字的面板，站长无法判断"开了之后有没有变快"，
//	也无法判断"5 万毫秒的 P95 是上游慢还是本站慢"。
//	配置可以照着抄，效果只能看。
//
// 【为什么关闭加速器不需要重启】
//
//	Relay.SetAccelerator 同时换配置与 HTTP Transport，
//	而 Transport 的连接池参数只在建连时读取 —— 所以换实例才能生效。
//	代价是丢弃一次空闲连接，影响仅限换配置的那一刻。
//
// 流转（Flow）：
//
//	GET  → 读设置表 → LoadAcceleratorSetting（钳位）→ 附加 relay 的聚合视图
//	PUT  → 校验 → 写设置表 → relay.SetAccelerator（热更新）
package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/oai"
)

// acceleratorResponse 是加速器面板的完整响应。
type acceleratorResponse struct {
	Setting model.AcceleratorSetting `json:"setting"`

	// 效果视图：延迟选路。
	Latency struct {
		Enabled      bool    `json:"enabled"`
		LatencyRatio float64 `json:"latency_ratio"`
		Channels     int     `json:"channels"`
		Measured     int     `json:"measured"`
		FastestMs    int     `json:"fastest_ms"`
		SlowestMs    int     `json:"slowest_ms"`
		P50Ms        int     `json:"p50_ms"`
		P95Ms        int     `json:"p95_ms"`
		// AvgTTFBMS 与上面的"渠道测活延迟"来源不同：
		// 它来自真实调用日志，反映用户实际体验。
		AvgTTFBMS   int   `json:"avg_ttfb_ms"`
		TTFBSamples int64 `json:"ttfb_samples"`
	} `json:"latency"`

	// 效果视图：上游缓存。
	Cache struct {
		Enabled         bool    `json:"enabled"`
		HitRate         float64 `json:"hit_rate"`
		PromptTokens    int64   `json:"prompt_tokens"`
		CachedTokens    int64   `json:"cached_tokens"`
		CapableChannels int     `json:"capable_channels"`
		SampleCount     int64   `json:"sample_count"`
	} `json:"cache"`
}

// acceleratorBounds 是参数边界，供前端渲染滑块与提示文案。
//
// 由服务端下发而不是让前端硬编码：边界变了（加了校验）而前端没跟上，
// 就会出现"前端允许填、后端一律拒绝"的死循环 ——
// 站长改任何次数都保存不成功，且错误信息还指向别的字段。
type acceleratorBounds struct {
	LatencyWeightMin  int `json:"latency_weight_min"`
	LatencyWeightMax  int `json:"latency_weight_max"`
	IdlePerHostMin    int `json:"idle_per_host_min"`
	IdlePerHostMax    int `json:"idle_per_host_max"`
	IdleTimeoutMinMS  int `json:"idle_timeout_min_ms"`
	IdleTimeoutMaxMS  int `json:"idle_timeout_max_ms"`
	CacheMinTokensMax int `json:"cache_min_tokens_max"`
}

// acceleratorGetResponse 是 GET 的响应（配置 + 效果 + 边界）。
type acceleratorGetResponse struct {
	acceleratorResponse
	Bounds acceleratorBounds `json:"bounds"`
}

// acceleratorRequest 是 PUT 的请求体。
//
// 所有字段都是可选的（指针）：部分提交时只改动指到的项。
// 不做"整份覆盖"是因为站长很可能只想把总开关点开、
// 其他项沿用数据库里已有的值。
type acceleratorRequest struct {
	Enabled             *bool `json:"enabled"`
	LatencyRouting      *bool `json:"latency_routing"`
	LatencyWeight       *int  `json:"latency_weight"`
	CachePassthrough    *bool `json:"cache_passthrough"`
	CacheMinTokens      *int  `json:"cache_min_tokens"`
	MaxIdleConnsPerHost *int  `json:"max_idle_conns_per_host"`
	IdleConnTimeoutMS   *int  `json:"idle_conn_timeout_ms"`
	Expect100Continue   *bool `json:"expect_100_continue"`
}

// handleGetAccelerator 返回加速器配置与当前效果。
func (s *Server) handleGetAccelerator(c *gin.Context) {
	ctx := c.Request.Context()
	values, err := s.deps.Settings.GetAll(ctx)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusInternalServerError,
			"读取加速器设置失败", oai.TypeServer, oai.CodeInternal)
		return
	}
	setting := model.LoadAcceleratorSetting(values)

	resp := acceleratorGetResponse{acceleratorResponse: acceleratorResponse{Setting: setting}}
	resp.Bounds = acceleratorBounds{
		LatencyWeightMin:  model.LatencyWeightMin,
		LatencyWeightMax:  model.LatencyWeightMax,
		IdlePerHostMin:    model.MaxIdleConnsPerHostMin,
		IdlePerHostMax:    model.MaxIdleConnsPerHostMax,
		IdleTimeoutMinMS:  model.IdleConnTimeoutMinMS,
		IdleTimeoutMaxMS:  model.IdleConnTimeoutMaxMS,
		CacheMinTokensMax: model.CacheMinTokensMax,
	}

	// 效果视图是【附加信息】：拿不到就让它们为零值返回，
	// 而不是让整个页面报错。
	//
	// 理由：站长要看的是开关与参数，延迟分布只是参考。
	// 为了一个参考数据让整页 500，会让他以为"加速器坏了"。
	if s.deps.Relay != nil {
		if li, err := s.deps.Relay.LatencyInsightFor(ctx, s.deps.UsageLogs, ""); err == nil {
			resp.Latency.Enabled = li.Enabled
			resp.Latency.LatencyRatio = li.LatencyRatio
			resp.Latency.Channels = li.Channels
			resp.Latency.Measured = li.Measured
			resp.Latency.FastestMs = li.FastestMs
			resp.Latency.SlowestMs = li.SlowestMs
			resp.Latency.P50Ms = li.P50Ms
			resp.Latency.P95Ms = li.P95Ms
			resp.Latency.AvgTTFBMS = li.AvgTTFBMS
			resp.Latency.TTFBSamples = li.TTFBSamples
		}
		if ci, err := s.deps.Relay.CacheInsightFor(ctx, s.deps.UsageLogs, ""); err == nil {
			resp.Cache.Enabled = ci.Enabled
			resp.Cache.HitRate = ci.HitRate
			resp.Cache.PromptTokens = ci.PromptTokens
			resp.Cache.CachedTokens = ci.CachedTokens
			resp.Cache.CapableChannels = ci.CapableChannels
			resp.Cache.SampleCount = ci.SampleCount
		}
	}

	c.JSON(http.StatusOK, resp)
}

// handleUpdateAccelerator 保存加速器设置并热更新转发引擎。
func (s *Server) handleUpdateAccelerator(c *gin.Context) {
	var req acceleratorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"请求格式不正确", oai.TypeInvalidRequest, "accelerator_invalid_param")
		return
	}

	ctx := c.Request.Context()
	values, err := s.deps.Settings.GetAll(ctx)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusInternalServerError,
			"读取加速器设置失败", oai.TypeServer, oai.CodeInternal)
		return
	}
	current := model.LoadAcceleratorSetting(values)

	// 局部覆盖：只改指到的字段。
	//
	// 【为什么不整份覆盖】
	//	站长很可能只想点开总开关，而数据库里的其他值是他上次精心调过的。
	//	整份覆盖会把那些值打回默认，且界面上看不出发生过什么。
	applyPartial(&current, req)

	if err := current.Validate(); err != nil {
		var ae *model.AcceleratorError
		if errors.As(err, &ae) {
			// 字段级错误：让前端能把红框标在对应输入框上，
			// 而不是给一段"某个参数填错了"的笼统提示。
			// code / type 必须与 oai.WriteError 写出的形状保持一致 ——
			// 前端会按 code 分支处理错误，只给 message+field 会让它走到"未知错误"分支。
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"message": ae.Message,
					"type":    oai.TypeInvalidRequest,
					"code":    "accelerator_invalid_param",
					"field":   ae.Field,
				},
			})
			return
		}
		oai.WriteError(c.Writer, http.StatusBadRequest,
			err.Error(), oai.TypeInvalidRequest, "accelerator_invalid_param")
		return
	}

	if err := s.deps.Settings.SetMany(ctx, model.AcceleratorValuesToMap(current)); err != nil {
		oai.WriteError(c.Writer, http.StatusInternalServerError,
			"保存加速器设置失败", oai.TypeServer, oai.CodeInternal)
		return
	}

	// 热更新：换配置 + 换 HTTP Transport。
	//
	// 刻意【重新读一遍再下发】：SetAccelerator 内部会钳位，
	// 而这里下发的是刚从数据库读出的值 —— 两者顺序反了会让界面显示
	// 与实际生效值不一致（例如有人手工改过数据库）。
	stored, err := s.deps.Settings.GetAll(ctx)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusInternalServerError,
			"读取加速器设置失败", oai.TypeServer, oai.CodeInternal)
		return
	}
	if s.deps.Relay != nil {
		s.deps.Relay.SetAccelerator(model.LoadAcceleratorSetting(stored))
	}

	// 回读后返回：与 agent endpoint 同一个理由 ——
	// 界面显示的必须是数据库里真实存在的值，而不是我们以为存进去的值。
	final := model.LoadAcceleratorSetting(stored)
	c.JSON(http.StatusOK, gin.H{"setting": final})
}

// applyPartial 把请求里指到的字段覆盖到当前配置上。
func applyPartial(cur *model.AcceleratorSetting, req acceleratorRequest) {
	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}
	if req.LatencyRouting != nil {
		cur.LatencyRouting = *req.LatencyRouting
	}
	if req.LatencyWeight != nil {
		cur.LatencyWeight = *req.LatencyWeight
	}
	if req.CachePassthrough != nil {
		cur.CachePassthrough = *req.CachePassthrough
	}
	if req.CacheMinTokens != nil {
		cur.CacheMinTokens = *req.CacheMinTokens
	}
	if req.MaxIdleConnsPerHost != nil {
		cur.MaxIdleConnsPerHost = *req.MaxIdleConnsPerHost
	}
	if req.IdleConnTimeoutMS != nil {
		cur.IdleConnTimeoutMS = *req.IdleConnTimeoutMS
	}
	if req.Expect100Continue != nil {
		cur.Expect100Continue = *req.Expect100Continue
	}
}

// applyAcceleratorFromSettings 在启动时把设置灌进 Relay。
//
// 必须在服务可用【之前】调用：否则加速器配置要到站长手动改一次设置
// 才会生效，而界面显示的却是"已开启"。
func applyAcceleratorFromSettings(ctx context.Context, s *Server) {
	if s == nil || s.deps.Relay == nil || s.deps.Settings == nil {
		return
	}
	values, err := s.deps.Settings.GetAll(ctx)
	if err != nil {
		// 读不到不是致命错误：加速器配错不该让站点起不来。
		return
	}
	s.deps.Relay.SetAccelerator(model.LoadAcceleratorSetting(values))
}

// AI Agent 的渠道调整与健康诊断工具。
//
// 意图（Why）：
//
//	"某个渠道最近老失败""要不要把它临时降权""为什么全都打到同一个上游" ——
//	这三类问题是站长最高频的运维判断，而它们的答案都需要【跨表关联】：
//	渠道配置 + 探针历史 + 调用日志三边对起来才看得出来。
//	只给"列出渠道"的话，站长能拿到数据却得自己在那三张表之间比对，
//	而这正是他打开助手想省掉的部分。
//
// 【为什么 get_channel_health 是聚合而不是让模型自己调三个工具】
//
//	让模型依次调 list_channels → list_channel_probes → summarize_usage
//	再自己综合，是可行的，但有两个实际问题：
//	  1) 三次往返，且模型在综合时可能漏掉某一条 ——
//	     而"漏掉一个失败渠道"恰好是这个工具存在的意义；
//	  2) 交叉关联（"探针失败的那几个渠道，在调用日志里占比多少"）
//	     模型做不稳。计算放在代码里，它就是确定的。
//
//	【调整权重的方向性陷阱】
//	本站的权重语义是"越大越优先"（数值大者先被选中），
//	而站长的口语是"给它降权"= 想让它少被选中。
//	模型极易在这里搞反，把 5 改成 50 恰好做了相反的事。
//	因此工具描述里明确写了方向，且返回值同时给出前后值 ——
//	站长从确认单上一眼就能看出方向对不对。
//
// 流转（Flow）：
//
//	get_channel_health（诊断）→ 发现某渠道异常
//	  → set_channel_routing（改权重/优先级，无需确认）
//	  → 或 set_channel_status（直接停用，无需确认）
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// ── 工具：渠道健康总览 ──────────────────────────────────────────

func (t *AgentTools) getChannelHealthTool() Tool {
	return Tool{
		Name: "get_channel_health",
		Description: "一次性拿到全部渠道的健康总览：启用状态、权重、优先级、最近测活结果与延迟、" +
			"近 24 小时的调用成功率与失败次数，并按【建议处理】排序。" +
			"当用户问「现在渠道整体怎么样」「哪个渠道该处理」「要不要先停掉哪个」时用它，" +
			"比逐个调list_channels 更快也更容易得出结论。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"only_unhealthy": {
			"type": "boolean",
			"description": "为 true 时只返回有问题的渠道（停用的除外）。默认返回全部。"
		}
	}
}`),
		Handler: t.handleGetChannelHealth,
	}
}

func (t *AgentTools) handleGetChannelHealth(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		OnlyUnhealthy bool `json:"only_unhealthy"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if t.Channels == nil {
		return nil, errors.New("本站未接入渠道数据")
	}

	enabled := model.ChannelStatusEnabled
	channels, err := t.Channels.List(ctx, model.ChannelQuery{Status: &enabled, Limit: 100})
	if err != nil {
		return nil, fmt.Errorf("读取渠道失败: %w", err)
	}
	if len(channels) == 0 {
		return map[string]any{
			"channels": []any{},
			"note":     "本站当前没有启用中的渠道，所有模型调用都会直接失败。请到「渠道管理」添加或启用渠道。",
		}, nil
	}

	// 近 24 小时的调用统计：按渠道聚合。
	// 探针仓储是可选依赖，缺失时诊断维度少一维 ——
	// 但不能因此整体失败：那会让"探针没配"的站点完全拿不到这个工具。
	usageByChannel := map[uint64]*usageAgg{}
	if t.UsageLogs != nil {
		since := t.now().Add(-24 * time.Hour)
		logs, err := t.UsageLogs.List(ctx, model.UsageLogQuery{Since: &since, Limit: agentToolMaxRows * 10})
		if err == nil {
			for _, l := range logs {
				if l == nil {
					continue
				}
				agg, ok := usageByChannel[l.ChannelID]
				if !ok {
					agg = &usageAgg{}
					usageByChannel[l.ChannelID] = agg
				}
				agg.total++
				if usageLogSucceeded(l) {
					agg.ok++
				} else {
					agg.failed++
					if lastError(l.Error) != "" {
						agg.lastErr = truncateText(lastError(l.Error), 120)
					}
				}
			}
		}
	}

	type healthRow struct {
		ChannelID    uint64 `json:"channel_id"`
		Channel      string `json:"channel"`
		Weight       int    `json:"weight"`
		Priority     int    `json:"priority"`
		Models       int    `json:"model_count"`
		Calls24h     int    `json:"calls_24h"`
		Failed24h    int    `json:"failed_24h"`
		SuccessRate  string `json:"success_rate_24h"`
		LastProbeAt  string `json:"last_probe_at,omitempty"`
		ProbeOK      *bool  `json:"last_probe_ok"`
		ProbeLatency int    `json:"last_probe_latency_ms,omitempty"`
		Level        string `json:"level"`
		Advice       string `json:"advice"`
	}

	rows := make([]healthRow, 0, len(channels))
	for _, ch := range channels {
		if ch == nil {
			continue
		}
		row := healthRow{
			ChannelID: ch.ID,
			Channel:   ch.Name,
			Weight:    ch.Weight,
			Priority:  ch.Priority,
			Models:    len(ch.Models),
		}
		if agg, ok := usageByChannel[ch.ID]; ok {
			row.Calls24h = agg.total
			row.Failed24h = agg.failed
			row.SuccessRate = fmt.Sprintf("%.0f%%", float64(agg.ok)/float64(agg.total)*100)
		} else {
			// 没有调用记录与"成功率 0%"是两回事，必须区分：
			// 前者说明"没流量"，后者说明"全挂了"。混为一谈会让站长
			// 去修一个其实没人用的渠道。
			row.SuccessRate = "无调用"
		}

		// 探针：只取最近一条，不做趋势判断。
		// 原因见文件头：趋势分析交给调用日志，这里要的是"最新是否通过"。
		if t.ProbeLogs != nil {
			if logs, err := t.ProbeLogs.List(ctx, model.ChannelProbeLogQuery{
				ChannelID: ch.ID, Limit: 1,
			}); err == nil && len(logs) > 0 && logs[0] != nil {
				last := logs[0]
				ok := last.OK
				row.ProbeOK = &ok
				row.LastProbeAt = last.At.Format("2006-01-02 15:04:05")
				row.ProbeLatency = last.LatencyMS
			}
		}

		// 判定等级：探针（直接反映上游可达）优先于调用日志
		//（可能因自身参数问题而失败）。
		row.Level, row.Advice = judgeChannel(row.ProbeOK, row.Calls24h, row.Failed24h)
		rows = append(rows, row)
	}

	// 排序：问题严重的在前。
	// 站长打开这个工具就是为了找问题，把答案放在最后一屏等于没做。
	sort.SliceStable(rows, func(i, j int) bool {
		return levelRank(rows[i].Level) > levelRank(rows[j].Level)
	})

	if p.OnlyUnhealthy {
		filtered := make([]healthRow, 0, len(rows))
		for _, r := range rows {
			if r.Level != "正常" {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}

	summary := map[string]int{}
	for _, r := range rows {
		summary[r.Level]++
	}
	return map[string]any{
		"channels":    rows,
		"level_count": summary,
		"order_note":  "按严重程度排序，第一个最需要处理。level 为「异常」或「注意」时应先处理。",
	}, nil
}

// ── 工具：调整渠道路由参数 ──────────────────────────────────────

func (t *AgentTools) setChannelRoutingTool() Tool {
	return Tool{
		Name: "set_channel_routing",
		Description: "调整渠道在请求路由中的权重与优先级。" +
			"【方向务必搞清：本站权重【数值越大越优先】，优先级同样是数值小者优先】——" +
			"所以\"给这个渠道降权\"是把权重【调小】，\"升权\"是调大。" +
			"当用户说「把 X 权重调成 5」「X 渠道太忙了先少分点流量给它」时使用。" +
			"【执行前必须先用 list_channels 或 get_channel_health 确认当前值，" +
			"并把改动前后的值一起报给站长核对】。" +
			"参数：channel_id、weight（权重，1-100，不传则不改）、priority（优先级，0-100，不传则不改）。",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"channel_id": {
			"type": "integer",
			"description": "渠道 ID，来自 list_channels。"
		},
		"weight": {
			"type": "integer",
			"description": "新的权重，1-100。数值越大越优先被选中。用户说\"降权\"时要【调小】这个值。",
			"minimum": 1,
			"maximum": 100
		},
		"priority": {
			"type": "integer",
			"description": "新的优先级，0-100。数值越小越优先被选中。",
			"minimum": 0,
			"maximum": 100
		}
	},
	"required": ["channel_id"]
}`),
		Mutating: true,
		// 改权重是本站最"可逆"的写操作：改回去就行，
		// 且影响的是流量分配而非数据。
		// 但它是站长在"降权"与"升权"之间最容易说反的操作 ——
		// 工具描述里把方向写死，并在返回值里同时给出前后值供核对。
		Confirm:         ConfirmNone,
		ConfirmTitle:    "调整渠道路由参数",
		ConfirmRiskNote: "权重决定这个渠道分到多少请求。数值越大越优先，\"降权\"是调小而非调大。",
		Handler:         t.handleSetChannelRouting,
	}
}

func (t *AgentTools) handleSetChannelRouting(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		ChannelID uint64 `json:"channel_id"`
		Weight    *int   `json:"weight"`
		Priority  *int   `json:"priority"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误: %w", err)
	}
	if p.ChannelID == 0 {
		return nil, errors.New("缺少参数 channel_id，请先调用 list_channels 确认编号")
	}
	if p.Weight == nil && p.Priority == nil {
		return nil, errors.New("没有指定要改哪一项：请至少给出 weight 或 priority")
	}
	// 范围校验在模型层与仓储层各有一份，但这里【必须】自己查：
	// 越界的权重会让该渠道永远不被选中（或永远霸占全部流量），
	// 而这两个仓储都不一定拦得住。
	if p.Weight != nil && (*p.Weight < 1 || *p.Weight > 100) {
		return nil, fmt.Errorf("weight 必须在 1-100 之间，收到 %d", *p.Weight)
	}
	if p.Priority != nil && (*p.Priority < 0 || *p.Priority > 100) {
		return nil, fmt.Errorf("priority 必须在 0-100 之间，收到 %d", *p.Priority)
	}
	if t.Channels == nil {
		return nil, errors.New("本站未接入渠道数据，无法执行该操作")
	}

	ch, err := t.Channels.GetByID(ctx, p.ChannelID)
	if err != nil {
		if errors.Is(err, model.ErrChannelNotFound) {
			return nil, fmt.Errorf("渠道 %d 不存在，请调用 list_channels 确认编号", p.ChannelID)
		}
		return nil, fmt.Errorf("读取渠道失败: %w", err)
	}

	changes := make([]ConfirmationItem, 0, 2)
	if p.Weight != nil {
		if *p.Weight != ch.Weight {
			changes = append(changes, ConfirmationItem{
				Label: "权重（越大越优先）",
				Value: fmt.Sprintf("%d", ch.Weight),
				After: fmt.Sprintf("%d", *p.Weight),
			})
		}
		ch.Weight = *p.Weight
	}
	if p.Priority != nil {
		if *p.Priority != ch.Priority {
			changes = append(changes, ConfirmationItem{
				Label: "优先级（越小越优先）",
				Value: fmt.Sprintf("%d", ch.Priority),
				After: fmt.Sprintf("%d", *p.Priority),
			})
		}
		ch.Priority = *p.Priority
	}

	if len(changes) == 0 {
		return map[string]any{
			"changed": false,
			"channel": ch.Name,
			"note":    "目标值与当前值相同，未做修改",
			"current": map[string]any{"weight": ch.Weight, "priority": ch.Priority},
		}, nil
	}

	if err := t.Channels.Update(ctx, ch); err != nil {
		return nil, fmt.Errorf("更新渠道路由参数失败: %w", err)
	}
	return map[string]any{
		"changed":    true,
		"channel_id": ch.ID,
		"channel":    ch.Name,
		"changes":    changes,
		"current":    map[string]any{"weight": ch.Weight, "priority": ch.Priority},
	}, nil
}

// ── 辅助 ────────────────────────────────────────────────────────

// usageAgg 是近 24 小时的调用聚合。
type usageAgg struct {
	total   int
	ok      int
	failed  int
	lastErr string
}

// judgeChannel 判定渠道健康等级并给出建议。
//
// 【为什么探针优先于调用日志】
//
//	探针是本站主动发起的最小请求，失败基本等于"上游不可达"；
//	调用日志里的失败则可能源于用户的参数错误（模型名写错、
//	超上下文长度）。把两者混为一谈会让助手建议站长去修一个
//	其实没坏的渠道 —— 那比不回答更糟。
func judgeChannel(probeOK *bool, calls24h, failed24h int) (level, advice string) {
	switch {
	case probeOK != nil && !*probeOK:
		// 探针都过不了，调用日志就不用看了：上游此刻就是不认这个渠道。
		return "异常", "最近一次测活未通过。建议先停用该渠道（set_channel_status），" +
			"避免请求落到它上面后失败；确认原因后再启用。"
	case calls24h == 0:
		return "无流量", "近 24 小时没有调用。可能是配置正常但没人用，" +
			"也可能是被更高优先级的渠道抢走了全部流量。"
	case failed24h == 0:
		return "正常", "近 24 小时调用全部成功，无需处理。"
	case float64(failed24h)/float64(calls24h) >= 0.5:
		return "异常", fmt.Sprintf(
			"近 24 小时 %d 次调用里失败了 %d 次（成功率偏低）。"+
				"建议先查探针历史定位是上游问题还是参数问题，再考虑停用或降权。",
			calls24h, failed24h)
	case float64(failed24h)/float64(calls24h) >= 0.1:
		return "注意", fmt.Sprintf(
			"近 24 小时 %d 次调用里失败了 %d 次。属于可接受范围但值得留意，"+
				"若持续上升可考虑降权。", calls24h, failed24h)
	default:
		return "正常", ""
	}
}

// levelRank 给健康等级排序用，数值越大越严重。
func levelRank(level string) int {
	switch level {
	case "异常":
		return 4
	case "注意":
		return 3
	case "无流量":
		return 2
	default:
		return 1
	}
}

// lastError 从错误文本里取第一行。
//
// 调用日志的 Error 字段常带多行堆栈，而模型只需要"最近一次失败的原因"
// 这一行。全量回填会让它把上下文浪费在重复信息上。
func lastError(errText string) string {
	for i := 0; i < len(errText); i++ {
		if errText[i] == '\n' {
			return errText[:i]
		}
	}
	return errText
}

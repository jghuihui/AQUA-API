/**
 * 图表通用配色与样式常量（主题感知）。
 *
 * 意图（Why）：
 *   仪表盘与门户共 4 处图表，若各自定义颜色会出现「同一指标在不同页颜色不同」的问题；
 *   集中定义保证「请求=蓝、Token=紫、额度=绿」在全站一致，降低读图成本。
 *
 * 主题适配（重要）：
 *   站点提供昼 / 夜 / 深蓝三套主题，ECharts 的 canvas 不认 CSS 变量，
 *   因此这里导出「按主题取样式」的函数：调用方传 isDark，得到该主题下可读的配色。
 *   昼：坐标轴深灰、tooltip 白底深字；夜：坐标轴浅灰、tooltip 深底浅字。
 *
 * 流转（Flow）：
 *   页面 useTheme() → chartStyles(isDark) → 展开进 EChartsOption → EChart 渲染
 *
 * 扩展（Extend）：
 *   新增指标配色请在 palette 中追加（**只能往后加，不能插队**——
 *   页面按下标取色 cs.palette[0..4]，插队会让所有图表的颜色语义整体错位），
 *   并同步页面里的显式取色。
 *
 * 为什么整条色序在本次换肤里被替换：
 *   原色序（青 #0891b2 / 靛 #6366f1）属于 Tailwind 默认色板，
 *   与新的 Fluent 强调色 #0078d4 放在同一个页面里会形成「两套蓝」，
 *   看上去像两个不同产品的图表拼在一起。现改为 Fluent 数据可视化色阶。
 */

/** 图表主色序列（顺序即默认取色顺序：蓝 → 紫 → 绿 → 橙 → 洋红 → 青） */
export const CHART_PALETTE = ['#0078d4', '#8764b8', '#0f7b0f', '#9d5d00', '#c239b3', '#00b7c3']

/** 夜间主题下的主色序列：整体提亮一档，保证暗底可读 */
export const CHART_PALETTE_DARK = ['#60cdff', '#b4a0e5', '#6ccb5f', '#fce100', '#e879d8', '#4fd6e3']

export interface ChartStyles {
  palette: string[]
  axisLabel: { color: string; fontSize: number }
  axisLine: { lineStyle: { color: string } }
  splitLine: { lineStyle: { color: string; type: 'dashed' } }
  tooltip: Record<string, unknown>
}

/** 按主题返回一整套图表样式；调用方只需把结果展开进 option */
export function chartStyles(isDark: boolean): ChartStyles {
  if (isDark) {
    return {
      palette: CHART_PALETTE_DARK,
      axisLabel: { color: '#919191', fontSize: 11 },
      axisLine: { lineStyle: { color: 'rgba(255,255,255,0.14)' } },
      splitLine: { lineStyle: { color: 'rgba(255,255,255,0.08)', type: 'dashed' } },
      tooltip: {
        // 用中性深灰而不是卡片色：深蓝主题下卡片是海军蓝，tooltip 跟着变蓝会
        // 与图表里的蓝序列撞色。tooltip 是浮在所有东西之上的临时层，中性色最稳。
        backgroundColor: 'rgba(43,43,43,0.97)',
        borderColor: 'rgba(58,58,58,1)',
        borderWidth: 1,
        padding: [8, 12],
        textStyle: { color: '#ffffff', fontSize: 12 },
        // 圆角与投影对齐 Fluent 浮层（8px 容器圆角 + 两层软影），
        // 不再用 10px/单层大扩散——那一眼就能看出是"网页图表库默认皮肤"。
        extraCssText: 'border-radius:8px;box-shadow:0 8px 16px rgba(0,0,0,.42),0 0 2px rgba(0,0,0,.4);',
      },
    }
  }
  return {
    palette: CHART_PALETTE,
    axisLabel: { color: '#5c5c5c', fontSize: 11 },
    axisLine: { lineStyle: { color: 'rgba(0,0,0,0.14)' } },
    splitLine: { lineStyle: { color: 'rgba(0,0,0,0.07)', type: 'dashed' } },
    tooltip: {
      backgroundColor: 'rgba(255,255,255,0.98)',
      borderColor: 'rgba(229,229,229,1)',
      borderWidth: 1,
      padding: [8, 12],
      textStyle: { color: '#1a1a1a', fontSize: 12 },
      extraCssText: 'border-radius:8px;box-shadow:0 8px 16px rgba(0,0,0,.14),0 0 2px rgba(0,0,0,.12);',
    },
  }
}

/** 面积图渐变（用于折线下方的填充，让趋势更易读） */
export function areaGradient(color: string): Record<string, unknown> {
  return {
    type: 'linear',
    x: 0,
    y: 0,
    x2: 0,
    y2: 1,
    colorStops: [
      { offset: 0, color: `${color}40` },
      { offset: 1, color: `${color}00` },
    ],
  }
}

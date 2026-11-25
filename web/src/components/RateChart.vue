<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useThemeVars } from 'naive-ui'
import * as echarts from 'echarts/core'
import { LineChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import type { RatePoint } from '@/types/api'
import { useThemeStore } from '@/stores/theme'
import { formatClock, formatRate } from '@/utils/format'

echarts.use([LineChart, GridComponent, TooltipComponent, LegendComponent, CanvasRenderer])

const props = defineProps<{
  rates: RatePoint[]
}>()

const themeVars = useThemeVars()
const themeStore = useThemeStore()
const containerRef = ref<HTMLDivElement | null>(null)

let chart: echarts.ECharts | null = null
let ro: ResizeObserver | null = null

function renderChart() {
  if (!chart) return
  const points = props.rates || []
  const labels = points.map((p) => formatClock(p.ts))
  const inData = points.map((p) => p.in_rate)
  const outData = points.map((p) => p.out_rate)
  const inColor = themeStore.isDark ? '#60A5FA' : '#18181B'
  const outColor = themeStore.isDark ? '#4ADE80' : '#2563EB'

  chart.setOption(
    {
      animationDuration: 200,
      animationEasing: 'cubicOut',
      grid: {
        top: 32,
        right: 12,
        bottom: 24,
        left: 58,
      },
      legend: {
        top: 0,
        right: 4,
        icon: 'roundRect',
        itemWidth: 10,
        itemHeight: 3,
        textStyle: {
          color: themeVars.value.textColor2,
          fontSize: 12,
        },
      },
      tooltip: {
        trigger: 'axis',
        backgroundColor: themeVars.value.popoverColor,
        borderColor: themeVars.value.borderColor,
        textStyle: {
          color: themeVars.value.textColor1,
          fontSize: 12,
        },
        formatter(params: unknown) {
          const list = Array.isArray(params) ? params : []
          if (list.length === 0) return ''
          const axisLabel = (list[0] as { axisValueLabel?: string }).axisValueLabel || ''
          const lines = [`<div style="font-weight:600;margin-bottom:4px">${axisLabel}</div>`]
          for (const item of list as Array<{ seriesName: string; value: number; color: string }>) {
            lines.push(
              `<div style="display:flex;justify-content:space-between;gap:12px"><span><span style="display:inline-block;width:8px;height:8px;border-radius:50%;background:${item.color};margin-right:6px"></span>${item.seriesName}</span><span style="font-variant-numeric:tabular-nums;font-weight:500">${formatRate(item.value)}</span></div>`
            )
          }
          return lines.join('')
        },
      },
      xAxis: {
        type: 'category',
        boundaryGap: false,
        data: labels,
        axisLine: { lineStyle: { color: themeVars.value.dividerColor } },
        axisTick: { show: false },
        axisLabel: {
          color: themeVars.value.textColor3,
          fontSize: 11,
          hideOverlap: true,
        },
      },
      yAxis: {
        type: 'value',
        min: 0,
        max: (v: { max: number }) => (v.max < 1024 ? 1024 : Math.ceil(v.max * 1.15)),
        splitNumber: 4,
        axisLine: { show: false },
        axisTick: { show: false },
        splitLine: { lineStyle: { color: themeVars.value.dividerColor, type: 'dashed' } },
        axisLabel: {
          color: themeVars.value.textColor3,
          fontSize: 11,
          formatter: (v: number) => formatRate(v),
        },
      },
      series: [
        {
          name: '入站速率',
          type: 'line',
          smooth: 0.25,
          showSymbol: false,
          lineStyle: { width: 2, color: inColor },
          itemStyle: { color: inColor },
          areaStyle: {
            color: inColor,
            opacity: themeStore.isDark ? 0.14 : 0.08,
          },
          data: inData,
        },
        {
          name: '出站速率',
          type: 'line',
          smooth: 0.25,
          showSymbol: false,
          lineStyle: { width: 2, color: outColor },
          itemStyle: { color: outColor },
          areaStyle: {
            color: outColor,
            opacity: themeStore.isDark ? 0.1 : 0.07,
          },
          data: outData,
        },
      ],
    },
    true
  )
}

function onWindowResize() {
  chart?.resize()
}

onMounted(() => {
  if (!containerRef.value) return
  chart = echarts.init(containerRef.value)
  renderChart()
  if (typeof ResizeObserver !== 'undefined') {
    ro = new ResizeObserver(() => {
      chart?.resize()
    })
    ro.observe(containerRef.value)
  } else {
    window.addEventListener('resize', onWindowResize, { passive: true })
  }
})

onBeforeUnmount(() => {
  ro?.disconnect()
  window.removeEventListener('resize', onWindowResize)
  chart?.dispose()
  chart = null
})

watch([() => props.rates, () => themeStore.isDark, () => themeStore.primaryKey], () => {
  renderChart()
})
</script>

<template>
  <section class="nfr-card">
    <div class="chart-head">
      <div>
        <h2 class="section-title">近 60 秒实时速率趋势</h2>
        <p class="section-sub">
          {{ rates.length > 0 ? `已采集 ${rates.length} 秒全局入站与出站速率` : '等待首个 1 秒采样点…' }}
        </p>
      </div>
    </div>
    <div ref="containerRef" class="chart-canvas" role="img" aria-label="近 60 秒实时上下行速率折线图" />
  </section>
</template>

<style scoped>
.chart-head {
  margin-bottom: 8px;
}

.section-title {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  color: var(--nfr-text);
}

.section-sub {
  margin: 2px 0 0;
  font-size: 12px;
  color: var(--nfr-text-secondary);
}

.chart-canvas {
  width: 100%;
  height: 220px;
}

@media (max-width: 767px) {
  .chart-canvas {
    height: 196px;
  }
}
</style>

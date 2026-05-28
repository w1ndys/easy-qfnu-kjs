<script setup>
import { ref, computed, onMounted, onUnmounted, watch, nextTick, shallowRef } from 'vue'
import { useRouter } from 'vue-router'
import { ArrowLeft, Refresh, DataBoard } from '@element-plus/icons-vue'
import * as echarts from 'echarts/core'
import { BarChart, LineChart, PieChart } from 'echarts/charts'
import {
  TitleComponent,
  TooltipComponent,
  GridComponent,
  LegendComponent,
} from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import { getDashboard } from '@/api'

echarts.use([
  BarChart, LineChart, PieChart,
  TitleComponent, TooltipComponent, GridComponent, LegendComponent,
  CanvasRenderer,
])

const router = useRouter()

const COLORS = {
  primary: '#884F22',
  primaryLight: '#A67C52',
  caramel: '#C4956A',
  cream: '#F5E6D3',
  success: '#10B981',
  info: '#0EA5E9',
  warning: '#F59E0B',
  rose: '#F43F5E',
  violet: '#8B5CF6',
  slate: '#64748B',
}

const CHART_PALETTE = [
  COLORS.primary, COLORS.success, COLORS.info,
  COLORS.caramel, COLORS.warning, COLORS.violet,
  COLORS.rose, COLORS.slate, COLORS.primaryLight, '#6366F1',
]

const timeRange = ref('today')
const customDays = ref(14)
const loading = ref(true)
const data = ref(null)
const error = ref(null)

const timeRangeOptions = [
  { value: 'today', label: '今天' },
  { value: 'week', label: '最近7天' },
  { value: 'month', label: '最近30天' },
  { value: 'custom', label: '自定义' },
]

const timeRangeLabel = computed(() =>
  timeRange.value === 'custom'
    ? `最近${customDays.value}天`
    : timeRangeOptions.find((o) => o.value === timeRange.value)?.label || ''
)

const trendChartRef = ref(null)
const keywordChartRef = ref(null)
const nodeChartRef = ref(null)
const resultChartRef = ref(null)
const hourlyChartRef = ref(null)

const trendChart = shallowRef(null)
const keywordChart = shallowRef(null)
const nodeChart = shallowRef(null)
const resultChart = shallowRef(null)
const hourlyChart = shallowRef(null)

async function fetchData() {
  loading.value = true
  error.value = null
  try {
    data.value = await getDashboard(timeRange.value, customDays.value)
  } catch (e) {
    error.value = e?.response?.data?.error || '获取数据失败'
  } finally {
    loading.value = false
  }
}

function baseTooltip() {
  return {
    backgroundColor: 'rgba(255,255,255,0.95)',
    borderColor: COLORS.cream,
    borderWidth: 1,
    textStyle: { color: COLORS.primary, fontSize: 13 },
    extraCssText: 'box-shadow: 0 4px 20px rgba(136,79,34,0.12); border-radius: 12px;',
  }
}

function ensureChart(instanceRef, domRef) {
  if (!domRef.value) {
    if (instanceRef.value) { instanceRef.value.dispose(); instanceRef.value = null }
    return null
  }
  if (instanceRef.value && instanceRef.value.getDom() !== domRef.value) {
    instanceRef.value.dispose(); instanceRef.value = null
  }
  if (!instanceRef.value) { instanceRef.value = echarts.init(domRef.value) }
  return instanceRef.value
}

function renderTrendChart() {
  if (!data.value?.trend) return
  const chart = ensureChart(trendChart, trendChartRef)
  if (!chart) return
  const trend = data.value.trend
  chart.setOption({
    tooltip: { ...baseTooltip(), trigger: 'axis' },
    grid: { left: 50, right: 20, top: 20, bottom: 30 },
    xAxis: {
      type: 'category', data: trend.map((t) => t.label),
      axisLabel: { color: COLORS.slate, fontSize: 11, rotate: timeRange.value === 'month' || timeRange.value === 'custom' ? 45 : 0 },
      axisLine: { lineStyle: { color: COLORS.cream } }, axisTick: { show: false },
    },
    yAxis: { type: 'value', minInterval: 1, axisLabel: { color: COLORS.slate, fontSize: 11 }, splitLine: { lineStyle: { color: COLORS.cream, type: 'dashed' } } },
    series: [{
      type: 'line', data: trend.map((t) => t.count), smooth: true, symbol: 'circle', symbolSize: 6,
      lineStyle: { color: COLORS.primary, width: 3 },
      itemStyle: { color: COLORS.primary, borderColor: '#fff', borderWidth: 2 },
      areaStyle: { color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [{ offset: 0, color: 'rgba(136,79,34,0.25)' }, { offset: 1, color: 'rgba(136,79,34,0.02)' }]) },
    }],
  }, true)
}

function renderKeywordChart() {
  if (!data.value?.top_keywords) return
  const chart = ensureChart(keywordChart, keywordChartRef)
  if (!chart) return
  const kw = data.value.top_keywords.slice().reverse()
  chart.setOption({
    tooltip: { ...baseTooltip(), trigger: 'axis', axisPointer: { type: 'shadow' } },
    grid: { left: 100, right: 30, top: 10, bottom: 20 },
    xAxis: { type: 'value', minInterval: 1, axisLabel: { color: COLORS.slate, fontSize: 11 }, splitLine: { lineStyle: { color: COLORS.cream, type: 'dashed' } } },
    yAxis: { type: 'category', data: kw.map((k) => k.keyword), axisLabel: { color: COLORS.primary, fontSize: 12, fontWeight: 600, width: 80, overflow: 'truncate' }, axisLine: { show: false }, axisTick: { show: false } },
    series: [{ type: 'bar', data: kw.map((k, i) => ({ value: k.count, itemStyle: { color: new echarts.graphic.LinearGradient(0, 0, 1, 0, [{ offset: 0, color: CHART_PALETTE[i % CHART_PALETTE.length] }, { offset: 1, color: CHART_PALETTE[i % CHART_PALETTE.length] + '88' }]), borderRadius: [0, 8, 8, 0] } })), barWidth: '60%', label: { show: true, position: 'right', color: COLORS.slate, fontSize: 12, fontWeight: 600 } }],
  }, true)
}

function renderNodeChart() {
  if (!data.value?.node_dist) return
  const chart = ensureChart(nodeChart, nodeChartRef)
  if (!chart) return
  const nd = data.value.node_dist
  const nodeLabels = { '01-02': '1-2节', '03-04': '3-4节', '05-06': '5-6节', '07-08': '7-8节', '09-10': '9-10节', '09-11': '9-11节', '01-04': '1-4节', '05-08': '5-8节', '01-11': '全天' }
  chart.setOption({
    tooltip: { ...baseTooltip(), trigger: 'item', formatter: '{b}: {c} ({d}%)' },
    series: [{ type: 'pie', radius: ['40%', '70%'], center: ['50%', '50%'], avoidLabelOverlap: true, itemStyle: { borderRadius: 8, borderColor: '#fff', borderWidth: 2 }, label: { show: true, fontSize: 12, color: COLORS.slate, formatter: '{b}\n{d}%' }, emphasis: { label: { fontSize: 14, fontWeight: 'bold' }, itemStyle: { shadowBlur: 10, shadowOffsetX: 0, shadowColor: 'rgba(136,79,34,0.2)' } }, data: nd.map((n, i) => ({ name: nodeLabels[n.node] || n.node, value: n.count, itemStyle: { color: CHART_PALETTE[i % CHART_PALETTE.length] } })) }],
  }, true)
}

function renderResultChart() {
  if (!data.value?.result_stats) return
  const chart = ensureChart(resultChart, resultChartRef)
  if (!chart) return
  const dist = data.value.result_stats.distribution || []
  chart.setOption({
    tooltip: { ...baseTooltip(), trigger: 'axis', axisPointer: { type: 'shadow' } },
    grid: { left: 50, right: 20, top: 20, bottom: 30 },
    xAxis: { type: 'category', data: dist.map((d) => d.range), axisLabel: { color: COLORS.slate, fontSize: 11 }, axisLine: { lineStyle: { color: COLORS.cream } }, axisTick: { show: false } },
    yAxis: { type: 'value', minInterval: 1, axisLabel: { color: COLORS.slate, fontSize: 11 }, splitLine: { lineStyle: { color: COLORS.cream, type: 'dashed' } } },
    series: [{ type: 'bar', data: dist.map((d, i) => ({ value: d.count, itemStyle: { color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [{ offset: 0, color: CHART_PALETTE[i % CHART_PALETTE.length] }, { offset: 1, color: CHART_PALETTE[i % CHART_PALETTE.length] + '66' }]), borderRadius: [8, 8, 0, 0] } })), barWidth: '50%', label: { show: true, position: 'top', color: COLORS.slate, fontSize: 11, fontWeight: 600 } }],
  }, true)
}

function renderHourlyChart() {
  if (!data.value?.hourly_dist) return
  const chart = ensureChart(hourlyChart, hourlyChartRef)
  if (!chart) return
  const hd = data.value.hourly_dist
  const maxCount = Math.max(...hd.map((h) => h.count), 1)
  chart.setOption({
    tooltip: { ...baseTooltip(), trigger: 'axis', axisPointer: { type: 'shadow' }, formatter: (params) => { const p = params[0]; return `${p.name}:00 - ${p.name}:59<br/>查询次数: <b>${p.value}</b>` } },
    grid: { left: 50, right: 20, top: 20, bottom: 30 },
    xAxis: { type: 'category', data: hd.map((h) => String(h.hour).padStart(2, '0')), axisLabel: { color: COLORS.slate, fontSize: 10 }, axisLine: { lineStyle: { color: COLORS.cream } }, axisTick: { show: false } },
    yAxis: { type: 'value', minInterval: 1, axisLabel: { color: COLORS.slate, fontSize: 11 }, splitLine: { lineStyle: { color: COLORS.cream, type: 'dashed' } } },
    series: [{ type: 'bar', data: hd.map((h) => ({ value: h.count, itemStyle: { color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [{ offset: 0, color: h.count >= maxCount * 0.8 ? COLORS.rose : h.count >= maxCount * 0.5 ? COLORS.warning : COLORS.success }, { offset: 1, color: (h.count >= maxCount * 0.8 ? COLORS.rose : h.count >= maxCount * 0.5 ? COLORS.warning : COLORS.success) + '44' }]), borderRadius: [6, 6, 0, 0] } })), barWidth: '60%' }],
  }, true)
}

function renderAllCharts() { nextTick(() => { renderTrendChart(); renderKeywordChart(); renderNodeChart(); renderResultChart(); renderHourlyChart() }) }
function resizeAllCharts() { trendChart.value?.resize(); keywordChart.value?.resize(); nodeChart.value?.resize(); resultChart.value?.resize(); hourlyChart.value?.resize() }
function disposeAllCharts() { trendChart.value?.dispose(); keywordChart.value?.dispose(); nodeChart.value?.dispose(); resultChart.value?.dispose(); hourlyChart.value?.dispose() }

watch(timeRange, async () => { await fetchData(); renderAllCharts() })
watch(customDays, async () => { if (timeRange.value !== 'custom') return; await fetchData(); renderAllCharts() })
watch(data, () => { if (data.value) renderAllCharts() })

onMounted(async () => { await fetchData(); renderAllCharts(); window.addEventListener('resize', resizeAllCharts) })
onUnmounted(() => { window.removeEventListener('resize', resizeAllCharts); disposeAllCharts() })

const overview = computed(() => data.value?.overview || {})
const peakHour = computed(() => { if (!data.value?.hourly_dist) return '--'; const max = data.value.hourly_dist.reduce((a, b) => (b.count > a.count ? b : a), { hour: 0, count: 0 }); if (max.count === 0) return '--'; return `${String(max.hour).padStart(2, '0')}:00` })
const resultRate = computed(() => { const rs = data.value?.result_stats; if (!rs || (rs.zero_count + rs.non_zero_count) === 0) return '--'; return ((rs.non_zero_count / (rs.zero_count + rs.non_zero_count)) * 100).toFixed(1) + '%' })

const overviewCards = computed(() => [
  { label: '总查询次数', value: overview.value.total_count || 0, color: 'var(--color-brand-500)', bg: 'var(--color-brand-100)' },
  { label: '独立用户(UV)', value: overview.value.unique_visitors || 0, color: 'var(--color-info-fg)', bg: 'var(--color-info-bg)' },
  { label: '独立 IP', value: overview.value.unique_ips || 0, color: 'var(--color-error-fg)', bg: 'var(--color-error-bg)' },
  { label: '搜索词数', value: overview.value.unique_keywords || 0, color: 'var(--color-info-fg)', bg: 'var(--color-info-bg)' },
  { label: '有结果率', value: resultRate.value, color: 'var(--color-success-fg)', bg: 'var(--color-success-bg)' },
  { label: '高峰时段', value: peakHour.value, color: 'var(--color-warning-fg)', bg: 'var(--color-warning-bg)' },
])

function goBack() { router.back() }
</script>

<template>
  <div class="dashboard-page">
    <header class="dashboard-topbar">
      <div class="topbar-inner">
        <div class="topbar-left">
          <el-button :icon="ArrowLeft" link @click="goBack">返回</el-button>
          <div class="topbar-title">
            <div class="title-icon">
              <el-icon :size="20"><DataBoard /></el-icon>
            </div>
            <div>
              <h1>数据大屏</h1>
              <p>{{ timeRangeLabel }}查询统计</p>
            </div>
          </div>
        </div>
        <el-button :icon="Refresh" :loading="loading" plain @click="fetchData">刷新</el-button>
      </div>
    </header>

    <main class="dashboard-main">
      <el-card class="filter-card" shadow="never">
        <div class="filter-row">
          <el-radio-group v-model="timeRange">
            <el-radio-button
              v-for="opt in timeRangeOptions"
              :key="opt.value"
              :value="opt.value"
            >
              {{ opt.label }}
            </el-radio-button>
          </el-radio-group>
          <div class="custom-days">
            <span>最近</span>
            <el-input-number
              v-model="customDays"
              :min="1"
              :max="365"
              :disabled="timeRange !== 'custom'"
              size="default"
              @focus="timeRange = 'custom'"
            />
            <span>天</span>
          </div>
        </div>
      </el-card>

      <el-empty v-if="error" :description="error" class="state-card">
        <el-button type="primary" @click="fetchData">重试</el-button>
      </el-empty>

      <template v-else>
        <el-card v-loading="loading" class="overview-card" shadow="never">
          <template #header>
            <div class="card-head">
              <span class="card-title">数据总览</span>
              <span class="card-subtitle">{{ timeRangeLabel }}</span>
            </div>
          </template>
          <div class="overview-grid">
            <div
              v-for="item in overviewCards"
              :key="item.label"
              class="overview-item"
              :style="{ background: item.bg }"
            >
              <div class="overview-value" :style="{ color: item.color }">{{ item.value }}</div>
              <div class="overview-label">{{ item.label }}</div>
            </div>
          </div>

          <el-row :gutter="12" class="sub-stats">
            <el-col :span="8">
              <div class="sub-stat-item">
                <div class="sub-stat-value brand">{{ overview.today_count || 0 }}</div>
                <div class="sub-stat-label">今日</div>
              </div>
            </el-col>
            <el-col :span="8">
              <div class="sub-stat-item">
                <div class="sub-stat-value success">{{ overview.week_count || 0 }}</div>
                <div class="sub-stat-label">本周</div>
              </div>
            </el-col>
            <el-col :span="8">
              <div class="sub-stat-item">
                <div class="sub-stat-value info">{{ overview.month_count || 0 }}</div>
                <div class="sub-stat-label">本月</div>
              </div>
            </el-col>
          </el-row>
        </el-card>

        <el-card class="chart-card" shadow="never">
          <template #header>
            <span class="card-title">查询次数趋势</span>
          </template>
          <div ref="trendChartRef" class="chart-container" style="height: 280px;"></div>
        </el-card>

        <el-card class="chart-card" shadow="never">
          <template #header>
            <span class="card-title">搜索词排行榜</span>
          </template>
          <div v-if="data && data.top_keywords && data.top_keywords.length > 0" ref="keywordChartRef" class="chart-container" style="height: 300px;"></div>
          <el-empty v-else description="暂无搜索数据" :image-size="80" />
        </el-card>

        <el-row :gutter="16" class="chart-row">
          <el-col :xs="24" :md="12">
            <el-card class="chart-card" shadow="never">
              <template #header>
                <span class="card-title">节次分布</span>
              </template>
              <div v-if="data && data.node_dist && data.node_dist.length > 0" ref="nodeChartRef" class="chart-container" style="height: 260px;"></div>
              <el-empty v-else description="暂无节次数据" :image-size="80" />
            </el-card>
          </el-col>
          <el-col :xs="24" :md="12">
            <el-card class="chart-card" shadow="never">
              <template #header>
                <span class="card-title">结果数量分布</span>
              </template>
              <div v-if="data && data.result_stats">
                <div ref="resultChartRef" class="chart-container" style="height: 260px;"></div>
                <el-row :gutter="8" class="result-summary">
                  <el-col :span="8">
                    <div class="summary-value brand">{{ data.result_stats.avg_count?.toFixed(1) || '0' }}</div>
                    <div class="summary-label">平均结果数</div>
                  </el-col>
                  <el-col :span="8">
                    <div class="summary-value success">{{ data.result_stats.max_count || 0 }}</div>
                    <div class="summary-label">最多结果</div>
                  </el-col>
                  <el-col :span="8">
                    <div class="summary-value info">{{ data.result_stats.non_zero_count || 0 }}</div>
                    <div class="summary-label">有效查询</div>
                  </el-col>
                </el-row>
              </div>
              <el-empty v-else description="暂无结果数据" :image-size="80" />
            </el-card>
          </el-col>
        </el-row>

        <el-card class="chart-card" shadow="never">
          <template #header>
            <div class="card-head">
              <span class="card-title">高峰时段分析</span>
              <span class="card-subtitle">24小时查询分布</span>
            </div>
          </template>
          <div ref="hourlyChartRef" class="chart-container" style="height: 260px;"></div>
          <div v-if="data && data.hourly_dist" class="peak-legend">
            <span class="peak-item"><span class="peak-dot" style="background: #10B981;"></span>低峰</span>
            <span class="peak-item"><span class="peak-dot" style="background: #F59E0B;"></span>中峰</span>
            <span class="peak-item"><span class="peak-dot" style="background: #F43F5E;"></span>高峰</span>
          </div>
        </el-card>
      </template>

      <p class="dashboard-footer">Powered by <span class="brand-text">曲奇味卷卷</span></p>
    </main>
  </div>
</template>

<style scoped>
.dashboard-page {
  min-height: 100vh;
  background: var(--color-surface-page);
}

.dashboard-topbar {
  background: var(--color-surface-card);
  border-bottom: 1px solid var(--color-border-subtle);
  position: sticky;
  top: 0;
  z-index: 10;
  backdrop-filter: blur(8px);
}

.topbar-inner {
  max-width: 1280px;
  margin: 0 auto;
  padding: 12px 24px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}

.topbar-left {
  display: flex;
  align-items: center;
  gap: 16px;
}

.topbar-title {
  display: flex;
  align-items: center;
  gap: 10px;
}

.title-icon {
  width: 36px;
  height: 36px;
  border-radius: 10px;
  background: var(--color-brand-100);
  color: var(--color-brand-500);
  display: flex;
  align-items: center;
  justify-content: center;
}

.topbar-title h1 {
  margin: 0;
  font-size: 16px;
  font-weight: 700;
  color: var(--color-text-primary);
}

.topbar-title p {
  margin: 0;
  font-size: 12px;
  color: var(--color-text-tertiary);
}

.dashboard-main {
  max-width: 1280px;
  margin: 0 auto;
  padding: 20px 24px 40px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.filter-card,
.overview-card,
.chart-card {
  border: 1px solid var(--color-border-subtle);
  border-radius: 14px;
}

.filter-card :deep(.el-card__body) {
  padding: 14px 18px;
}

.filter-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.custom-days {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  color: var(--color-text-secondary);
}

.state-card {
  padding: 48px 0;
}

.card-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
}

.card-title {
  font-size: 15px;
  font-weight: 700;
  color: var(--color-text-primary);
}

.card-subtitle {
  font-size: 12px;
  font-weight: 500;
  color: var(--color-text-tertiary);
}

.overview-grid {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: 10px;
}

@media (min-width: 480px) {
  .overview-grid {
    grid-template-columns: repeat(3, 1fr);
  }
}

@media (min-width: 1024px) {
  .overview-grid {
    grid-template-columns: repeat(6, 1fr);
  }
}

.overview-item {
  border-radius: 12px;
  padding: 14px 8px;
  text-align: center;
}

.overview-value {
  font-size: 22px;
  font-weight: 700;
}

.overview-label {
  font-size: 11px;
  color: var(--color-text-tertiary);
  margin-top: 4px;
}

.sub-stats {
  margin-top: 12px;
}

.sub-stat-item {
  border-radius: 12px;
  border: 1px solid var(--color-border-subtle);
  background: var(--color-surface-card);
  padding: 10px;
  text-align: center;
}

.sub-stat-value {
  font-size: 18px;
  font-weight: 700;
}

.sub-stat-value.brand { color: var(--color-brand-500); }
.sub-stat-value.success { color: var(--color-success-fg); }
.sub-stat-value.info { color: var(--color-info-fg); }

.sub-stat-label {
  font-size: 11px;
  color: var(--color-text-tertiary);
  margin-top: 2px;
}

.chart-container {
  width: 100%;
}

.chart-row {
  margin: 0 !important;
}

.chart-row .el-col {
  margin-bottom: 16px;
}

.result-summary {
  margin-top: 12px;
  text-align: center;
}

.summary-value {
  font-size: 14px;
  font-weight: 700;
}

.summary-value.brand { color: var(--color-brand-500); }
.summary-value.success { color: var(--color-success-fg); }
.summary-value.info { color: var(--color-info-fg); }

.summary-label {
  font-size: 11px;
  color: var(--color-text-tertiary);
}

.peak-legend {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 16px;
  margin-top: 12px;
  font-size: 12px;
  color: var(--color-text-tertiary);
}

.peak-item {
  display: flex;
  align-items: center;
  gap: 4px;
}

.peak-dot {
  width: 12px;
  height: 12px;
  border-radius: 3px;
}

.dashboard-footer {
  text-align: center;
  font-size: 12px;
  color: var(--color-text-tertiary);
  margin: 8px 0 0;
}

.brand-text {
  color: var(--color-brand-500);
  font-weight: 600;
}

@media (max-width: 640px) {
  .topbar-inner,
  .dashboard-main {
    padding-left: 16px;
    padding-right: 16px;
  }

  .filter-row {
    flex-direction: column;
    align-items: stretch;
  }

  .custom-days {
    justify-content: center;
  }
}
</style>


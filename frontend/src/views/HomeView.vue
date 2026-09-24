<script setup lang="ts">
import { computed } from 'vue'
import AppFooter from '@/components/AppFooter.vue'
import EmptyState from '@/components/EmptyState.vue'
import ErrorState from '@/components/ErrorState.vue'
import LoadingSpinner from '@/components/LoadingSpinner.vue'
import StatusNotice from '@/components/StatusNotice.vue'
import { useBootstrap } from '@/composables/useBootstrap'
import { formatBeijing, formatDateZh, weekdayLabel } from '@/utils/format'
import { parsePublishedWeeks } from '@/utils/manifest'

const { manifest, context, loading, error, load } = useBootstrap()

const inTeaching = computed(() => context.value?.in_teaching_calendar === true)
const term = computed(() => context.value?.term ?? manifest.value?.term ?? '')
const today = computed(() => formatDateZh(context.value?.date) || formatBeijing(manifest.value?.generated_at))
const weekText = computed(() => {
  const week = context.value?.week
  if (!inTeaching.value || week == null || week <= 0) return '—'
  return `第 ${week} 周`
})
const weekday = computed(() => weekdayLabel(context.value?.weekday) || weekdayLabel(context.value?.day))
const publishedWeekCount = computed(() => parsePublishedWeeks(manifest.value?.weeks).length)
const generatedText = computed(() => formatBeijing(manifest.value?.generated_at))

const features = [
  {
    to: '/empty-classroom',
    title: '空教室查询',
    description: '选择教学楼分组与节次范围，快速找到空闲教室。',
    label: '找空教室',
    icon: 'search',
    color: '#156B52',
    bg: '#EAF8F3',
  },
  {
    to: '/full-day-status',
    title: '教室全天状态',
    description: '查看所选教室当天 01–12 小节的占用节奏，适合对比排查。',
    label: '状态矩阵',
    icon: 'bar-chart-o',
    color: 'var(--color-brand-500)',
    bg: 'var(--color-brand-100)',
  },
]
</script>

<template>
  <div class="page-container">
    <div class="page-content">
      <!-- 品牌承接 -->
      <div class="app-card hero-section">
        <div class="hero-top">
          <span class="hero-brand">曲阜师范大学</span>
          <van-tag round type="primary" class="hero-tag">校园查询工具</van-tag>
        </div>
        <h1 class="hero-title">教室查询</h1>
        <p class="hero-desc">
          从教学周快照中查询空教室与全天占用状态。数据每日更新、按楼分组，移动端优先。
        </p>
      </div>

      <!-- context 摘要 -->
      <div class="app-card section-gap context-card">
        <template v-if="loading && !manifest">
          <LoadingSpinner text="正在获取当前教学周信息…" />
        </template>
        <template v-else-if="error && !manifest">
          <div class="context-error">
            <ErrorState
              title="无法加载数据"
              :message="error.message"
              show-retry
              @retry="load"
            />
          </div>
        </template>
        <template v-else>
          <div class="context-head">
            <span class="context-title">当前上下文</span>
            <span v-if="inTeaching" class="context-state context-state--teaching">教学周内</span>
            <span v-else class="context-state context-state--holiday">不在教学周内</span>
          </div>
          <div class="context-grid">
            <div class="context-item">
              <div class="context-key">学期</div>
              <div class="context-value">{{ term || '—' }}</div>
            </div>
            <div class="context-item">
              <div class="context-key">教学周</div>
              <div class="context-value">{{ weekText }}</div>
            </div>
            <div class="context-item">
              <div class="context-key">日期</div>
              <div class="context-value">{{ today }}</div>
            </div>
            <div class="context-item">
              <div class="context-key">星期</div>
              <div class="context-value">{{ weekday || '—' }}</div>
            </div>
          </div>

          <div class="context-foot">
            <span v-if="publishedWeekCount > 0">已收录 {{ publishedWeekCount }} 个教学周</span>
            <span v-if="generatedText"> · 数据更新 {{ generatedText }}</span>
          </div>
        </template>
      </div>

      <StatusNotice
        v-if="!loading && !error && manifest && !inTeaching"
        type="warning"
        icon="info-o"
        class="section-gap"
      >
        当前不在教学周内。已发布的周次仍可在查询页手动选择使用。
      </StatusNotice>

      <!-- 功能入口 -->
      <div class="feature-grid">
        <router-link
          v-for="card in features"
          :key="card.to"
          :to="card.to"
          class="app-card feature-card"
        >
          <div class="feature-header">
            <div class="feature-icon" :style="{ background: card.bg, color: card.color }">
              <van-icon :name="card.icon" size="22" />
            </div>
            <van-tag plain round size="small">{{ card.label }}</van-tag>
          </div>
          <h2 class="feature-title">{{ card.title }}</h2>
          <p class="feature-desc">{{ card.description }}</p>
          <div class="feature-footer">
            <span>立即进入</span>
            <van-icon name="arrow" size="14" />
          </div>
        </router-link>
      </div>

      <div v-if="!loading && !error && !manifest" class="section-gap">
        <EmptyState text="暂无可查询数据" description="数据尚未发布，请稍后再来。" />
      </div>

      <AppFooter />
    </div>
  </div>
</template>

<style scoped>
.hero-section {
  padding: 22px 20px;
  margin-bottom: 16px;
}

.hero-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 12px;
}

.hero-brand {
  font-size: 13px;
  font-weight: 600;
  letter-spacing: 0.08em;
  color: var(--color-text-tertiary);
}

.hero-title {
  font-size: 28px;
  font-weight: 700;
  color: var(--color-text-primary);
  margin: 0 0 8px;
  letter-spacing: -0.02em;
}

.hero-desc {
  font-size: 14px;
  color: var(--color-text-secondary);
  line-height: 1.7;
  margin: 0;
}

.context-card {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.context-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.context-title {
  font-size: 15px;
  font-weight: 700;
  color: var(--color-text-primary);
}

.context-state {
  font-size: 12px;
  font-weight: 600;
  padding: 3px 10px;
  border-radius: 999px;
}

.context-state--teaching {
  color: var(--color-success-fg);
  background: var(--color-success-bg);
}

.context-state--holiday {
  color: var(--color-warning-fg);
  background: var(--color-warning-bg);
}

.context-grid {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: 10px;
}

.context-item {
  background: var(--color-surface-section);
  border-radius: 10px;
  padding: 10px 12px;
}

.context-key {
  font-size: 12px;
  color: var(--color-text-tertiary);
  margin-bottom: 4px;
}

.context-value {
  font-size: 15px;
  font-weight: 700;
  color: var(--color-text-primary);
}

.context-foot {
  font-size: 12px;
  color: var(--color-text-tertiary);
}

.feature-grid {
  display: grid;
  grid-template-columns: 1fr;
  gap: 12px;
  margin-bottom: 16px;
}

@media (min-width: 768px) {
  .feature-grid {
    grid-template-columns: repeat(2, 1fr);
  }
}

.feature-card {
  display: flex;
  flex-direction: column;
  text-decoration: none;
  transition: box-shadow 0.2s, transform 0.2s;
}

.feature-card:active {
  transform: scale(0.99);
}

.feature-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  margin-bottom: 12px;
}

.feature-icon {
  width: 44px;
  height: 44px;
  border-radius: 12px;
  display: flex;
  align-items: center;
  justify-content: center;
}

.feature-title {
  font-size: 17px;
  font-weight: 700;
  color: var(--color-text-primary);
  margin: 0 0 6px;
}

.feature-desc {
  font-size: 13px;
  color: var(--color-text-tertiary);
  line-height: 1.6;
  margin: 0;
  flex: 1;
}

.feature-footer {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 16px;
  padding-top: 12px;
  border-top: 1px solid var(--color-border-subtle);
  font-size: 14px;
  font-weight: 600;
  color: var(--color-brand-500);
}
</style>

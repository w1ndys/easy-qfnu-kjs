<script setup>
import { ref, onMounted } from 'vue'
import { getStats } from '@/api'

const stats = ref(null)
const loading = ref(true)

onMounted(async () => {
  try {
    stats.value = await getStats()
  } catch {
    // silent fail
  } finally {
    loading.value = false
  }
})
</script>

<template>
  <div v-if="!loading && stats" class="app-card">
    <div class="stats-title">查询统计</div>

    <van-row gutter="12">
      <van-col span="8">
        <div class="stat-item stat-info">
          <div class="stat-value">{{ stats.today_count }}</div>
          <div class="stat-label">今日查询</div>
          <div v-if="stats.today_top" class="stat-top" :title="stats.today_top">
            {{ stats.today_top }}
          </div>
        </div>
      </van-col>
      <van-col span="8">
        <div class="stat-item stat-success">
          <div class="stat-value">{{ stats.week_count }}</div>
          <div class="stat-label">本周查询</div>
          <div v-if="stats.week_top" class="stat-top" :title="stats.week_top">
            {{ stats.week_top }}
          </div>
        </div>
      </van-col>
      <van-col span="8">
        <div class="stat-item stat-brand">
          <div class="stat-value">{{ stats.month_count }}</div>
          <div class="stat-label">本月查询</div>
          <div v-if="stats.month_top" class="stat-top" :title="stats.month_top">
            {{ stats.month_top }}
          </div>
        </div>
      </van-col>
    </van-row>
  </div>
</template>

<style scoped>
.stats-title {
  font-size: 16px;
  font-weight: 600;
  color: var(--color-text-primary);
  margin-bottom: 16px;
}

.stat-item {
  border-radius: 12px;
  padding: 16px 8px;
  text-align: center;
  border: 1px solid;
}

.stat-info {
  background: var(--color-info-bg);
  border-color: #B7CBFF;
}

.stat-success {
  background: var(--color-success-bg);
  border-color: #A7DEC7;
}

.stat-brand {
  background: var(--color-brand-100);
  border-color: var(--color-brand-200);
}

.stat-value {
  font-size: 24px;
  font-weight: 700;
  line-height: 1.2;
}

.stat-info .stat-value { color: var(--color-info-fg); }
.stat-success .stat-value { color: var(--color-success-fg); }
.stat-brand .stat-value { color: var(--color-brand-500); }

.stat-label {
  font-size: 12px;
  color: var(--color-text-tertiary);
  margin-top: 4px;
}

.stat-top {
  font-size: 11px;
  font-weight: 500;
  margin-top: 6px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.stat-info .stat-top { color: var(--color-info-fg); }
.stat-success .stat-top { color: var(--color-success-fg); }
.stat-brand .stat-top { color: var(--color-brand-500); }
</style>

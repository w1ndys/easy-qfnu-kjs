<script setup>
import AnnouncementCard from '@/components/AnnouncementCard.vue'
import QRCodeCard from '@/components/QRCodeCard.vue'
import AppFooter from '@/components/AppFooter.vue'
import StatsCard from '@/components/StatsCard.vue'

const featureCards = [
  {
    to: '/empty-classroom',
    title: '空教室查询',
    description: '输入教学楼、日期和节次，快速找到可用教室。',
    label: '高频查询',
    icon: 'search',
    color: '#156B52',
    bg: '#EAF8F3',
  },
  {
    to: '/full-day-status',
    title: '教室全天状态',
    description: '按节次查看教学楼全天占用情况，适合对比和排查。',
    label: '状态矩阵',
    icon: 'bar-chart-o',
    color: 'var(--color-brand-500)',
    bg: 'var(--color-brand-100)',
  },
  {
    to: '/dashboard',
    title: '数据大屏',
    description: '查看查询趋势、热门关键词和结果分布。',
    label: '运营分析',
    icon: 'chart-trending-o',
    color: '#1D4ED8',
    bg: '#ECF3FF',
  },
]
</script>

<template>
  <div class="page-container">
    <div class="page-content">
      <!-- Hero Section -->
      <div class="app-card hero-section">
        <van-tag type="primary" round class="hero-tag">QFNU Campus Tool</van-tag>
        <h1 class="hero-title">教室查询系统</h1>
        <p class="hero-desc">
          面向曲阜师范大学学生的轻量查询工具，把空教室、全天状态和查询热度组织在一个清晰入口里。
        </p>
        <div class="quick-path">
          <van-row gutter="8">
            <van-col span="8">
              <div class="path-step">选楼</div>
            </van-col>
            <van-col span="8">
              <div class="path-step">选日期</div>
            </van-col>
            <van-col span="8">
              <div class="path-step">看结果</div>
            </van-col>
          </van-row>
        </div>
      </div>

      <!-- Announcements -->
      <div class="section-gap">
        <AnnouncementCard />
      </div>

      <!-- Stats -->
      <div class="section-gap">
        <StatsCard />
      </div>

      <!-- Feature Cards -->
      <div class="feature-grid">
        <router-link
          v-for="card in featureCards"
          :key="card.to"
          :to="card.to"
          class="feature-card app-card"
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

      <!-- QRCode -->
      <div class="section-gap">
        <QRCodeCard />
      </div>

      <AppFooter />
    </div>
  </div>
</template>

<style scoped>
.hero-section {
  padding: 24px 20px;
}

.hero-tag {
  margin-bottom: 12px;
}

.hero-title {
  font-size: 28px;
  font-weight: 700;
  color: var(--color-text-primary);
  margin: 0 0 8px;
  letter-spacing: -0.02em;
}

.hero-desc {
  font-size: 15px;
  color: var(--color-text-secondary);
  line-height: 1.6;
  margin: 0 0 20px;
}

.quick-path {
  background: #FAF8F6;
  border-radius: 12px;
  padding: 12px;
}

.path-step {
  background: var(--color-surface-card);
  border-radius: 10px;
  padding: 10px 4px;
  text-align: center;
  font-size: 12px;
  font-weight: 600;
  color: var(--color-brand-500);
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.06);
}

.feature-grid {
  display: grid;
  grid-template-columns: 1fr;
  gap: 12px;
  margin-bottom: 16px;
}

@media (min-width: 768px) {
  .feature-grid {
    grid-template-columns: repeat(3, 1fr);
  }
}

.feature-card {
  display: flex;
  flex-direction: column;
  text-decoration: none;
  transition: box-shadow 0.2s, transform 0.2s;
}

.feature-card:active {
  transform: scale(0.98);
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
  font-size: 16px;
  font-weight: 600;
  color: var(--color-text-primary);
  margin: 0 0 6px;
}

.feature-desc {
  font-size: 13px;
  color: var(--color-text-tertiary);
  line-height: 1.5;
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

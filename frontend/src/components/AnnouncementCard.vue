<script setup>
import { ref, watch, onMounted } from 'vue'
import { useAnnouncements } from '@/composables/useAnnouncements'

const { allAnnouncements, unreadCount, hasUnread, isRead, markAllAsRead, fetchAnnouncements } =
  useAnnouncements()

const expanded = ref(false)

onMounted(async () => {
  await fetchAnnouncements()
  expanded.value = hasUnread.value
})

function handleToggle() {
  if (expanded.value && hasUnread.value) {
    markAllAsRead()
  }
  expanded.value = !expanded.value
}

watch(hasUnread, (val) => {
  if (!val) {
    expanded.value = false
  }
})
</script>

<template>
  <div v-if="allAnnouncements.length > 0" class="announcement-card">
    <!-- 折叠状态 -->
    <van-cell-group v-if="!expanded" inset @click="handleToggle">
      <van-cell :border="false" is-link clickable>
        <template #title>
          <div class="collapsed-title">
            <van-icon name="volume-o" :color="hasUnread ? '#9A5A00' : 'var(--color-brand-500)'" size="18" />
            <span class="collapsed-text">
              {{ hasUnread ? `${unreadCount} 条新公告` : '系统公告' }}
            </span>
            <van-badge v-if="hasUnread" :content="unreadCount" />
          </div>
        </template>
      </van-cell>
    </van-cell-group>

    <!-- 展开状态 -->
    <div v-else class="app-card">
      <div class="expanded-header">
        <div class="header-left">
          <van-icon name="volume-o" :color="hasUnread ? '#9A5A00' : 'var(--color-brand-500)'" size="20" />
          <span class="header-title">系统公告</span>
          <van-tag v-if="hasUnread" type="warning" round>{{ unreadCount }} 条未读</van-tag>
        </div>
        <van-icon name="arrow-up" size="16" color="var(--color-text-tertiary)" @click="handleToggle" />
      </div>

      <div class="announcement-list">
        <div
          v-for="item in allAnnouncements"
          :key="item.id"
          class="announcement-item"
          :class="{ important: item.important, read: isRead(item.id) }"
        >
          <div class="item-header">
            <span class="item-title" :class="{ read: isRead(item.id) }">{{ item.title }}</span>
            <span class="item-date">{{ item.date }}</span>
          </div>
          <p class="item-content" :class="{ read: isRead(item.id) }">{{ item.content }}</p>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.announcement-card :deep(.van-cell-group--inset) {
  margin: 0;
  border-radius: var(--van-radius-lg);
  border: 1px solid var(--color-border-subtle);
}

.collapsed-title {
  display: flex;
  align-items: center;
  gap: 8px;
}

.collapsed-text {
  font-size: 14px;
  font-weight: 700;
  color: var(--color-text-primary);
}

.expanded-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 16px;
}

.header-left {
  display: flex;
  align-items: center;
  gap: 8px;
}

.header-title {
  font-size: 16px;
  font-weight: 700;
  color: var(--color-text-primary);
}

.announcement-list {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.announcement-item {
  border-radius: 12px;
  padding: 12px 16px;
  border: 1px solid var(--color-border-subtle);
  background: #FAF8F6;
}

.announcement-item.important {
  border-color: #F3CF8D;
  background: var(--color-warning-bg);
}

.item-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 8px;
}

.item-title {
  font-size: 14px;
  font-weight: 700;
  color: var(--color-text-primary);
}

.item-title.read {
  color: var(--color-text-tertiary);
}

.item-date {
  font-size: 12px;
  color: var(--color-text-tertiary);
  flex-shrink: 0;
}

.item-content {
  font-size: 13px;
  color: var(--color-text-secondary);
  line-height: 1.6;
  margin-top: 6px;
}

.item-content.read {
  color: var(--color-text-tertiary);
}
</style>

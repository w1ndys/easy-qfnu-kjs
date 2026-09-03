<script setup lang="ts">
defineProps<{
  /** 错误标题 */
  title?: string
  /** 补充说明（通常是错误 detail） */
  message?: string
  /** 重试按钮文案 */
  retryText?: string
  showRetry?: boolean
}>()

const emit = defineEmits<{
  (e: 'retry'): void
}>()
</script>

<template>
  <div class="error-state app-card">
    <van-icon name="warning-o" size="40" color="var(--color-error-fg)" class="error-icon" />
    <div class="error-title">{{ title || '出错了' }}</div>
    <div v-if="message" class="error-message">{{ message }}</div>
    <van-button
      v-if="showRetry"
      round
      type="primary"
      size="small"
      plain
      icon="replay"
      class="error-retry"
      @click="emit('retry')"
    >
      {{ retryText || '重试' }}
    </van-button>
  </div>
</template>

<style scoped>
.error-state {
  display: flex;
  flex-direction: column;
  align-items: center;
  text-align: center;
  padding: 32px 20px;
  border-color: var(--color-error-border);
  background: var(--color-surface-card);
}

.error-icon {
  margin-bottom: 12px;
}

.error-title {
  font-size: 16px;
  font-weight: 700;
  color: var(--color-error-fg);
}

.error-message {
  margin-top: 6px;
  font-size: 13px;
  color: var(--color-text-secondary);
  line-height: 1.6;
  max-width: 420px;
}

.error-retry {
  margin-top: 16px;
}
</style>

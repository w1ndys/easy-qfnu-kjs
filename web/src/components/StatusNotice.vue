<script setup lang="ts">
withDefaults(
  defineProps<{
    /** warning | error | info | success */
    type?: 'warning' | 'error' | 'info' | 'success'
    message?: string
    icon?: string
  }>(),
  { type: 'info', message: '', icon: '' },
)
</script>

<template>
  <van-notice-bar
    class="status-notice"
    :class="`status-notice--${type}`"
    :icon="icon"
    :wrapable="true"
    :scrollable="false"
  >
    <template #default>
      <slot>{{ message }}</slot>
    </template>
  </van-notice-bar>
</template>

<style scoped>
.status-notice {
  border-radius: 12px;
  padding: 12px 16px;
  font-size: 13px;
  font-weight: 500;
  line-height: 1.6;
}

.status-notice--info {
  color: var(--color-info-fg);
  background: var(--color-info-bg);
}

.status-notice--success {
  color: var(--color-success-fg);
  background: var(--color-success-bg);
}

.status-notice--warning {
  color: var(--color-warning-fg);
  background: var(--color-warning-bg);
}

.status-notice--error {
  color: var(--color-error-fg);
  background: var(--color-error-bg);
}

:deep(.van-notice-bar__content) {
  color: inherit;
}
</style>

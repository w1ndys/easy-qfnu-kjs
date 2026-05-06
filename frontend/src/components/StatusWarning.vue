<script setup>
const props = defineProps({
  type: { type: String, default: 'warning' },
  title: { type: String, default: '' },
  message: { type: String, default: '' },
})

const typeMap = {
  warning: 'warning',
  error: 'danger',
  info: 'primary',
  success: 'success',
}
</script>

<template>
  <van-notice-bar
    v-if="props.type === 'warning'"
    left-icon="info-o"
    :text="props.message"
    color="var(--color-warning-fg)"
    background="var(--color-warning-bg)"
    wrapable
    :scrollable="false"
  />
  <van-notice-bar
    v-else-if="props.type === 'error'"
    left-icon="warning-o"
    :text="props.message"
    color="var(--color-error-fg)"
    background="var(--color-error-bg)"
    wrapable
    :scrollable="false"
  />
  <van-notice-bar
    v-else
    left-icon="info-o"
    :text="props.message"
    color="var(--color-info-fg)"
    background="var(--color-info-bg)"
    wrapable
    :scrollable="false"
  />
</template>

<style scoped>
:deep(.van-notice-bar) {
  border-radius: 12px;
  padding: 12px 16px;
  font-size: 13px;
  font-weight: 500;
}
</style>

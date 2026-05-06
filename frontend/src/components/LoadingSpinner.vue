<script setup>
const props = defineProps({
  text: { type: String, default: '加载中...' },
})
</script>

<template>
  <div class="loading-wrapper">
    <van-loading type="spinner" color="var(--color-brand-500)" :size="32">
      {{ props.text }}
    </van-loading>
  </div>
</template>

<style scoped>
.loading-wrapper {
  display: flex;
  justify-content: center;
  align-items: center;
  padding: 40px 16px;
}

:deep(.van-loading__text) {
  color: var(--color-text-tertiary);
  font-size: 14px;
}
</style>

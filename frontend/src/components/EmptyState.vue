<script setup lang="ts">
withDefaults(
  defineProps<{
    text?: string
    description?: string
    icon?: string
  }>(),
  { text: '暂无数据', description: '', icon: 'search' },
)
</script>

<template>
  <van-empty :image="icon" class="empty-state">
    <template #description>
      <div class="empty-title">{{ text }}</div>
      <div v-if="description" class="empty-desc">{{ description }}</div>
    </template>
  </van-empty>
</template>

<style scoped>
.empty-state {
  padding: 40px 0 24px;
}

.empty-title {
  font-size: 14px;
  color: var(--color-text-secondary);
  line-height: 1.6;
}

.empty-desc {
  margin-top: 4px;
  font-size: 13px;
  color: var(--color-text-tertiary);
  line-height: 1.6;
}
</style>

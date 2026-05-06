<script setup>
const props = defineProps({
  text: { type: String, default: '暂无数据' },
  description: { type: String, default: '' },
})
</script>

<template>
  <van-empty :description="props.text">
    <template v-if="props.description" #description>
      <p>{{ props.description }}</p>
    </template>
  </van-empty>
</template>

<style scoped>
:deep(.van-empty__description) {
  color: var(--color-text-tertiary);
  font-size: 14px;
}
</style>

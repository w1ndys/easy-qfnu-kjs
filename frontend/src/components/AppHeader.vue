<script setup lang="ts">
import { useRouter } from 'vue-router'

defineProps<{
  title: string
  showBack?: boolean
}>()

const router = useRouter()
</script>

<template>
  <van-nav-bar
    :title="title"
    :left-arrow="showBack"
    fixed
    placeholder
    safe-area-inset-top
    @click-left="router.back()"
  />
</template>

<style scoped>
:deep(.van-nav-bar) {
  background: rgba(255, 253, 251, 0.94);
  backdrop-filter: blur(12px);
  -webkit-backdrop-filter: blur(12px);
  border-bottom: 1px solid var(--color-border-subtle);
}

:deep(.van-nav-bar__title) {
  font-weight: 700;
  color: var(--color-text-primary);
}

:deep(.van-nav-bar__arrow) {
  color: var(--color-brand-500);
}
</style>

<script setup>
import { useRouter } from 'vue-router'

const props = defineProps({
  title: { type: String, default: '' },
  showBack: { type: Boolean, default: false },
})

const router = useRouter()

function goBack() {
  router.back()
}
</script>

<template>
  <van-nav-bar
    :title="props.title"
    :left-arrow="props.showBack"
    fixed
    placeholder
    safe-area-inset-top
    @click-left="goBack"
  />
</template>

<style scoped>
:deep(.van-nav-bar) {
  background: rgba(255, 253, 251, 0.92);
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

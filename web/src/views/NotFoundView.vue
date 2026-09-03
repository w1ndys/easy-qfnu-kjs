<script setup lang="ts">
import AppFooter from '@/components/AppFooter.vue'
</script>

<template>
  <div class="page-container">
    <div class="page-content not-found">
      <van-empty image="error" description="页面不存在或已被移除">
        <van-button round type="primary" size="small" icon="wap-home-o" @click="$router.push('/')">
          返回首页
        </van-button>
      </van-empty>
    </div>
    <AppFooter />
  </div>
</template>

<style scoped>
.not-found {
  display: flex;
  justify-content: center;
  padding-top: 12vh;
}

:deep(.van-empty__description) {
  font-size: 14px;
  color: var(--color-text-tertiary);
}
</style>

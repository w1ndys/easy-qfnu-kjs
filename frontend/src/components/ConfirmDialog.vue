<script setup>
const props = defineProps({
  open: { type: Boolean, default: false },
  title: { type: String, default: '提示' },
  message: { type: String, default: '' },
  confirmText: { type: String, default: '确定' },
  cancelText: { type: String, default: '取消' },
  showCancel: { type: Boolean, default: true },
})

const emit = defineEmits(['confirm', 'cancel'])
</script>

<template>
  <van-dialog
    :show="props.open"
    :title="props.title"
    :message="props.message"
    :show-cancel-button="props.showCancel"
    :confirm-button-text="props.confirmText"
    :cancel-button-text="props.cancelText"
    @confirm="emit('confirm')"
    @cancel="emit('cancel')"
    @close="emit('cancel')"
  />
</template>

<style scoped>
:deep(.van-dialog) {
  border-radius: 16px;
}

:deep(.van-dialog__header) {
  font-weight: 700;
  color: var(--color-text-primary);
}

:deep(.van-dialog__message) {
  color: var(--color-text-secondary);
  line-height: 1.6;
}

:deep(.van-dialog__confirm) {
  color: var(--color-brand-500);
  font-weight: 600;
}
</style>

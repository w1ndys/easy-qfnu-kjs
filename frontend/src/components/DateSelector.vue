<script setup>
import { watch } from 'vue'
import { useDateSelection } from '@/composables/useDateSelection'

const props = defineProps({
  modelValue: {
    type: Number,
    default: 0,
  },
})

const emit = defineEmits(['update:modelValue'])

const {
  quickDateLabels,
  useCustomDate,
  customOffset,
  dateOffset,
  customDatePreview,
  setOffset,
  setQuickDate,
  toggleCustomDate,
  updateCustomOffset,
} = useDateSelection(props.modelValue)

watch(
  () => props.modelValue,
  (nextValue) => {
    if (nextValue !== dateOffset.value) {
      setOffset(nextValue)
    }
  },
)

watch(dateOffset, (nextValue) => {
  emit('update:modelValue', nextValue)
})

function handleCustomOffsetInput() {
  updateCustomOffset(customOffset.value)
}

function getQuickDateIndex() {
  if (useCustomDate.value) return 3
  return dateOffset.value <= 2 ? dateOffset.value : -1
}

function onTabChange(index) {
  if (index === 3) {
    toggleCustomDate()
  } else {
    setQuickDate(index)
  }
}
</script>

<template>
  <div class="date-selector">
    <div class="date-label">日期</div>

    <div class="date-tabs">
      <div
        v-for="(label, idx) in quickDateLabels"
        :key="idx"
        class="date-tab"
        :class="{ active: dateOffset === idx && !useCustomDate }"
        @click="setQuickDate(idx)"
      >
        {{ label }}
      </div>
      <div
        class="date-tab"
        :class="{ active: useCustomDate }"
        @click="toggleCustomDate"
      >
        自定义
      </div>
    </div>

    <div v-if="useCustomDate" class="custom-date">
      <van-field
        v-model.number="customOffset"
        type="digit"
        placeholder="输入天数"
        :border="false"
        @update:model-value="handleCustomOffsetInput"
      >
        <template #button>
          <span class="custom-suffix">天后</span>
        </template>
      </van-field>
      <div v-if="customDatePreview" class="date-preview">{{ customDatePreview }}</div>
    </div>
  </div>
</template>

<style scoped>
.date-selector {
  margin-bottom: 4px;
}

.date-label {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text-secondary);
  margin-bottom: 8px;
}

.date-tabs {
  display: flex;
  background: var(--color-surface-section);
  border-radius: 12px;
  padding: 4px;
  gap: 4px;
}

.date-tab {
  flex: 1;
  text-align: center;
  padding: 10px 8px;
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text-tertiary);
  border-radius: 8px;
  cursor: pointer;
  transition: all 0.2s;
}

.date-tab:hover {
  color: var(--color-text-primary);
}

.date-tab.active {
  background: var(--color-surface-card);
  color: var(--color-brand-500);
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.08);
}

.custom-date {
  margin-top: 12px;
}

.custom-date :deep(.van-field) {
  border-radius: 10px;
  border: 1px solid var(--color-border-subtle);
  background: var(--color-surface-card);
}

.custom-suffix {
  font-size: 14px;
  color: var(--color-text-tertiary);
  font-weight: 500;
}

.date-preview {
  margin-top: 6px;
  padding-left: 4px;
  font-size: 12px;
  color: var(--color-text-tertiary);
}
</style>

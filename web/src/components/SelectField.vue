<script setup lang="ts">
import { computed, ref } from 'vue'

export interface SelectOption {
  value: string | number
  label: string
  /** 次级说明文字（如房间数） */
  sub?: string
}

const props = withDefaults(
  defineProps<{
    modelValue: string | number | null
    options: SelectOption[]
    /** 表单上方标签 */
    label?: string
    placeholder?: string
    /** 弹出层标题 */
    title?: string
    searchable?: boolean
    clearable?: boolean
    disabled?: boolean
    required?: boolean
    emptyText?: string
    searchPlaceholder?: string
  }>(),
  {
    label: '',
    placeholder: '请选择',
    title: '',
    searchable: false,
    clearable: false,
    disabled: false,
    required: false,
    emptyText: '没有可选项',
    searchPlaceholder: '搜索',
  },
)

const emit = defineEmits<{
  (e: 'update:modelValue', value: string | number | null): void
}>()

const show = ref(false)
const keyword = ref('')

const current = computed(() => props.options.find((opt) => opt.value === props.modelValue) ?? null)
const popupTitle = computed(() => props.title || props.label || '请选择')

const filtered = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  if (!kw) return props.options
  return props.options.filter(
    (opt) =>
      opt.label.toLowerCase().includes(kw) || (opt.sub ?? '').toLowerCase().includes(kw),
  )
})

function open() {
  if (props.disabled) return
  keyword.value = ''
  show.value = true
}

function select(option: SelectOption) {
  emit('update:modelValue', option.value)
  show.value = false
}

function clearValue() {
  emit('update:modelValue', null)
}
</script>

<template>
  <div class="select-field">
    <div v-if="label" class="form-label">
      <span>{{ label }}</span>
      <span v-if="required" class="required-mark">*</span>
    </div>
    <div
      class="select-box"
      :class="{ 'select-box--disabled': disabled, 'select-box--placeholder': !current }"
      role="button"
      tabindex="0"
      @click="open"
      @keydown.enter.prevent="open"
      @keydown.space.prevent="open"
    >
      <span v-if="current" class="select-value">
        {{ current.label }}
        <span v-if="current.sub" class="select-sub">{{ current.sub }}</span>
      </span>
      <span v-else class="select-placeholder">{{ placeholder }}</span>
      <span class="select-side">
        <van-icon
          v-if="clearable && current && !disabled"
          name="clear"
          class="select-clear"
          @click.stop="clearValue"
        />
        <van-icon name="arrow-down" class="select-caret" />
      </span>
    </div>

    <van-popup v-model:show="show" position="bottom" round safe-area-inset-bottom>
      <div class="picker-popup">
        <div class="picker-header">
          <span class="picker-title">{{ popupTitle }}</span>
          <van-icon name="cross" class="picker-close" @click="show = false" />
        </div>
        <van-search
          v-if="searchable"
          v-model="keyword"
          :placeholder="searchPlaceholder"
          shape="round"
          class="picker-search"
        />
        <div class="picker-options" :class="{ 'picker-options--search': searchable }">
          <div v-if="filtered.length === 0" class="picker-empty">{{ emptyText }}</div>
          <div
            v-for="option in filtered"
            :key="String(option.value)"
            class="picker-option"
            :class="{ 'picker-option--active': option.value === modelValue }"
            @click="select(option)"
          >
            <span class="picker-option-label">{{ option.label }}</span>
            <span v-if="option.sub" class="picker-option-sub">{{ option.sub }}</span>
            <van-icon v-if="option.value === modelValue" name="success" class="picker-option-check" />
          </div>
        </div>
      </div>
    </van-popup>
  </div>
</template>

<style scoped>
.select-box {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  min-height: 44px;
  padding: 10px 12px;
  border-radius: 10px;
  border: 1px solid var(--color-border-subtle);
  background: var(--color-surface-card);
  cursor: pointer;
  font-size: 15px;
  transition: border-color 0.2s;
}

.select-box:active {
  border-color: var(--color-brand-200);
}

.select-box--disabled {
  opacity: 0.55;
  cursor: not-allowed;
  background: var(--color-surface-section);
}

.select-value {
  font-weight: 600;
  color: var(--color-text-primary);
  display: inline-flex;
  align-items: baseline;
  gap: 6px;
  flex-wrap: wrap;
}

.select-sub {
  font-size: 12px;
  font-weight: 400;
  color: var(--color-text-tertiary);
}

.select-placeholder {
  color: var(--color-text-tertiary);
  opacity: 0.9;
}

.select-side {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  flex-shrink: 0;
}

.select-clear {
  color: var(--color-text-tertiary);
  font-size: 14px;
}

.select-caret {
  color: var(--color-text-tertiary);
  font-size: 14px;
}

.required-mark {
  color: var(--color-error-fg);
}

.picker-popup {
  display: flex;
  flex-direction: column;
  max-height: 70vh;
  background: var(--color-surface-card);
  border-radius: 16px 16px 0 0;
}

.picker-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 14px 16px 6px;
}

.picker-title {
  font-size: 16px;
  font-weight: 700;
  color: var(--color-text-primary);
}

.picker-close {
  font-size: 18px;
  color: var(--color-text-tertiary);
  padding: 6px;
  margin: -6px;
}

.picker-search {
  padding: 6px 12px 4px;
  flex-shrink: 0;
}

.picker-options {
  overflow-y: auto;
  -webkit-overflow-scrolling: touch;
  padding: 8px 12px calc(12px + env(safe-area-inset-bottom));
}

.picker-options--search {
  max-height: 44vh;
}

.picker-option {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 12px 8px;
  border-radius: 10px;
  cursor: pointer;
  color: var(--color-text-primary);
  border-bottom: 1px solid var(--color-border-subtle);
}

.picker-option:last-child {
  border-bottom: none;
}

.picker-option:active {
  background: var(--color-brand-100);
}

.picker-option--active .picker-option-label {
  color: var(--color-brand-500);
  font-weight: 700;
}

.picker-option-label {
  font-size: 15px;
  flex: 0 0 auto;
}

.picker-option-sub {
  font-size: 12px;
  color: var(--color-text-tertiary);
  flex: 1;
}

.picker-option-check {
  color: var(--color-brand-500);
  font-size: 15px;
}

.picker-empty {
  text-align: center;
  padding: 28px 0;
  color: var(--color-text-tertiary);
  font-size: 13px;
}
</style>

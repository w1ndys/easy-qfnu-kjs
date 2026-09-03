<script setup lang="ts">
import { computed } from 'vue'
import SelectField, { type SelectOption } from './SelectField.vue'
import type { ManifestGroup } from '@/api/types'

const props = withDefaults(
  defineProps<{
    /** manifest 是否加载完成（表单可用） */
    loaded: boolean
    groups: ManifestGroup[]
    weekOptions: SelectOption[]
    dayOptions: SelectOption[]
    /** 节次选项（两位 01..12），空教室页使用 */
    nodeOptions?: SelectOption[]
    submitLoading?: boolean
    submitText?: string
    /** 是否渲染“起始/终止节次”范围 */
    showNodeRange?: boolean
    /** “今天”按钮是否可用（在教学周内） */
    todayEnabled?: boolean
    todayText?: string
  }>(),
  {
    nodeOptions: () => [],
    submitLoading: false,
    submitText: '查询',
    showNodeRange: true,
    todayEnabled: false,
    todayText: '今天',
  },
)

const emit = defineEmits<{
  (e: 'submit'): void
  (e: 'today'): void
}>()

const groupId = defineModel<string | null>('groupId', { default: null })
const keyword = defineModel<string>('keyword', { default: '' })
const week = defineModel<number | null>('week', { default: null })
const day = defineModel<number | null>('day', { default: null })
const startNode = defineModel<string>('startNode', { default: '01' })
const endNode = defineModel<string>('endNode', { default: '12' })

const groupOptions = computed<SelectOption[]>(() =>
  props.groups.map((group) => ({
    value: group.id,
    label: group.name,
    sub: group.room_count > 0 ? `${group.room_count} 间` : '',
  })),
)

const canSubmit = computed(
  () => props.loaded && !props.submitLoading && !!groupId.value && groupOptions.value.length > 0,
)

function onKeydownEnter(event: KeyboardEvent) {
  // 输入法组词回车不应触发查询
  if (event.isComposing) return
  if (!canSubmit.value) return
  emit('submit')
}
</script>

<template>
  <div class="app-card form-card" @keydown.enter="onKeydownEnter">
    <!-- 分组（必填，可搜索；值 = group_id，不提交任意文本） -->
    <SelectField
      v-model="groupId"
      label="教学楼分组"
      :options="groupOptions"
      title="选择教学楼分组"
      placeholder="请选择分组（可搜索楼名）"
      searchable
      required
      clearable
      :disabled="!loaded || groupOptions.length === 0"
      :empty-text="groupOptions.length === 0 ? '暂无可用分组' : '没有匹配的分组'"
      search-placeholder="输入楼名搜索，如“文史楼”"
    />

    <!-- 关键词（可选，房间名子串，<=32 字符） -->
    <div class="field-block">
      <div class="form-label">
        <span>房间关键词</span>
        <span class="label-optional">可选</span>
      </div>
      <van-field
        v-model="keyword"
        class="keyword-field"
        placeholder="在分组内过滤教室名，例如 101"
        left-icon="search"
        clearable
        maxlength="32"
        :border="false"
        :disabled="!loaded"
        enterkeyhint="search"
      >
        <template #extra>
          <span v-if="keyword.length > 0" class="keyword-count">{{ keyword.length }}/32</span>
        </template>
      </van-field>
    </div>

    <!-- 周次 / 星期 + “今天” -->
    <div class="field-block">
      <div class="field-grid">
        <SelectField
          v-model="week"
          label="教学周"
          :options="weekOptions"
          title="选择教学周"
          placeholder="请选择教学周"
          clearable
          :disabled="!loaded || weekOptions.length === 0"
          :empty-text="weekOptions.length === 0 ? '暂无已发布的教学周' : '没有可选项'"
        />
        <SelectField
          v-model="day"
          label="星期"
          :options="dayOptions"
          title="选择星期"
          placeholder="请选择星期"
          clearable
          :disabled="!loaded"
        />
      </div>
      <div class="today-row">
        <van-button
          size="small"
          round
          plain
          type="primary"
          icon="clock-o"
          :disabled="!todayEnabled"
          @click="emit('today')"
        >
          {{ todayText }}
        </van-button>
        <span v-if="!todayEnabled" class="today-hint">当前不在教学周内，可手动选择已发布周次</span>
      </div>
    </div>

    <!-- 节次范围（01..12，start <= end） -->
    <div v-if="showNodeRange" class="field-block">
      <div class="field-grid">
        <SelectField
          v-model="startNode"
          label="起始节次"
          :options="nodeOptions"
          title="选择起始节次"
          placeholder="01"
          :disabled="!loaded"
        />
        <SelectField
          v-model="endNode"
          label="终止节次"
          :options="nodeOptions"
          title="选择终止节次"
          placeholder="12"
          :disabled="!loaded"
        />
      </div>
      <div class="range-note">{{ startNode }} — {{ endNode }} 节</div>
    </div>

    <van-button
      type="primary"
      block
      round
      size="large"
      class="submit-btn"
      :loading="submitLoading"
      :disabled="!canSubmit"
      loading-text="查询中…"
      @click="emit('submit')"
    >
      {{ submitText }}
    </van-button>
  </div>
</template>

<style scoped>
.form-card {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.field-block {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.label-optional {
  font-size: 12px;
  font-weight: 400;
  color: var(--color-text-tertiary);
}

.keyword-field {
  border: 1px solid var(--color-border-subtle);
  border-radius: 10px;
  background: var(--color-surface-card);
}

.keyword-count {
  font-size: 12px;
  color: var(--color-text-tertiary);
}

.field-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 12px;
}

.today-row {
  display: flex;
  align-items: center;
  gap: 10px;
  min-height: 24px;
}

.today-hint {
  font-size: 12px;
  color: var(--color-text-tertiary);
}

.range-note {
  font-size: 12px;
  color: var(--color-text-tertiary);
  text-align: right;
  margin-top: -2px;
}

.submit-btn {
  margin-top: 4px;
}
</style>

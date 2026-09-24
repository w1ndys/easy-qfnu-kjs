<script setup lang="ts">
import { computed } from 'vue'
import { statusVisual } from '@/constants/statusVisuals'

const props = withDefaults(
  defineProps<{
    /** 状态 id（数字或字符串数字） */
    statusId: number | string | null | undefined
    /** 悬停提示（状态名），为空则不提示 */
    title?: string
  }>(),
  { title: '' },
)

const visual = computed(() => statusVisual(props.statusId))

const style = computed(() => ({
  color: visual.value.fg,
  background: visual.value.bg,
  borderColor: visual.value.border,
}))
</script>

<template>
  <span class="status-badge" :style="style" :title="title">{{ visual.glyph }}</span>
</template>

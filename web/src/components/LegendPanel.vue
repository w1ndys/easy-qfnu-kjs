<script setup lang="ts">
import { computed } from 'vue'
import StatusBadge from './StatusBadge.vue'
import type { StatusMetaItem } from '@/constants/statusVisuals'
import { buildStatusMeta } from '@/constants/statusVisuals'
import type { ManifestStatus } from '@/api/types'

const props = defineProps<{
  statuses: Record<string, ManifestStatus> | undefined
  /** 额外说明行文案 */
  note?: string
}>()

const meta = computed<StatusMetaItem[]>(() => buildStatusMeta(props.statuses))
const availableCount = computed(() => meta.value.filter((item) => item.available).length)
</script>

<template>
  <div class="app-card legend-card">
    <div class="legend-head">
      <span class="legend-title">状态图例</span>
      <van-tag v-if="availableCount > 0" plain round type="success" size="small">
        {{ availableCount }} 种可用状态
      </van-tag>
    </div>
    <div v-if="meta.length === 0" class="legend-empty">暂无状态定义</div>
    <div v-else class="legend-grid">
      <div v-for="item in meta" :key="item.key" class="legend-item">
        <StatusBadge :status-id="item.id" :title="item.name" />
        <span class="legend-name">{{ item.name }}</span>
        <van-tag
          plain
          round
          size="small"
          :type="item.available ? 'success' : 'default'"
          class="legend-flag"
        >
          {{ item.available ? '可用' : '占用' }}
        </van-tag>
      </div>
    </div>
    <div v-if="note" class="legend-note">{{ note }}</div>
  </div>
</template>

<style scoped>
.legend-card {
  padding: 14px 16px;
}

.legend-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 12px;
}

.legend-title {
  font-size: 15px;
  font-weight: 700;
  color: var(--color-text-primary);
}

.legend-grid {
  display: flex;
  flex-wrap: wrap;
  gap: 8px 14px;
}

.legend-item {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 13px;
  color: var(--color-text-secondary);
}

.legend-flag {
  font-size: 10px;
}

.legend-note {
  margin-top: 10px;
  font-size: 12px;
  color: var(--color-text-tertiary);
  line-height: 1.6;
}

.legend-empty {
  font-size: 13px;
  color: var(--color-text-tertiary);
  text-align: center;
  padding: 8px 0;
}
</style>

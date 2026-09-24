<script setup lang="ts">
import { computed } from 'vue'
import StatusBadge from './StatusBadge.vue'
import type { FullDayRoom } from '@/api/types'
import type { ManifestNode } from '@/api/types'
import type { StatusMetaItem } from '@/constants/statusVisuals'
import { statusName } from '@/constants/statusVisuals'
import { buildRoomLabels } from '@/utils/format'

const props = defineProps<{
  /** 节次定义（manifest.nodes 已按 code 升序） */
  nodes: ManifestNode[]
  rooms: FullDayRoom[]
  statusMeta: StatusMetaItem[]
}>()

const collapsed = defineModel<boolean>('collapsed', { default: false })

interface BlockColumn {
  source_block: string
  minCode: string
  maxCode: string
  label: string
}

const expandedColumns = computed(() => props.nodes)

const collapsedColumns = computed<BlockColumn[]>(() => {
  const order: string[] = []
  const map = new Map<string, ManifestNode[]>()
  for (const node of props.nodes) {
    if (!map.has(node.source_block)) {
      map.set(node.source_block, [])
      order.push(node.source_block)
    }
    map.get(node.source_block)!.push(node)
  }
  return order.map((block) => {
    const nodes = map.get(block)!
    const minCode = nodes[0].code
    const maxCode = nodes[nodes.length - 1].code
    return {
      source_block: block,
      minCode,
      maxCode,
      label: `${Number(minCode)}-${Number(maxCode)}节`,
    }
  })
})

const roomLabels = computed(() => buildRoomLabels(props.rooms))

function nodeLabel(code: string): string {
  return `第${Number(code)}节`
}

function statusNameOf(id: number | string | undefined): string {
  if (id == null) return ''
  return statusName(props.statusMeta, id)
}
</script>

<template>
  <div class="fd-card">
    <div class="fd-head">
      <span class="fd-title">全天状态</span>
      <span class="fd-sub">
        {{ collapsed ? '按大节折叠（5 个）' : `逐小节展示（${expandedColumns.length} 节）` }}
      </span>
      <van-button
        size="small"
        round
        plain
        type="primary"
        class="fd-toggle"
        @click="collapsed = !collapsed"
      >
        {{ collapsed ? '展开为逐节' : '按大节折叠' }}
      </van-button>
    </div>

    <div class="status-table-container">
      <table class="status-table fd-table">
        <thead>
          <tr>
            <th>教室</th>
            <template v-if="!collapsed">
              <th
                v-for="node in expandedColumns"
                :key="node.code"
                :title="`${nodeLabel(node.code)} · 来源大节 ${node.source_block}`"
              >
                {{ node.code }}
              </th>
            </template>
            <template v-else>
              <th v-for="block in collapsedColumns" :key="block.source_block" :title="`${block.label}（${block.source_block}）`">
                {{ block.label }}
              </th>
            </template>
          </tr>
        </thead>
        <tbody>
          <tr v-for="room in rooms" :key="room.id">
            <td>
              <span class="room-name">{{ roomLabels.get(room.id)?.name ?? room.name }}</span>
              <span v-if="roomLabels.get(room.id)?.shortId" class="room-short-id">
                {{ roomLabels.get(room.id)?.shortId }}
              </span>
            </td>
            <template v-if="!collapsed">
              <td v-for="node in expandedColumns" :key="`${room.id}-${node.code}`">
                <span v-if="room.statuses?.[node.code] != null">
                  <StatusBadge
                    :status-id="room.statuses![node.code]"
                    :title="`${nodeLabel(node.code)} ${statusNameOf(room.statuses![node.code])}`"
                  />
                </span>
                <span v-else class="cell-empty">–</span>
              </td>
            </template>
            <template v-else>
              <td v-for="block in collapsedColumns" :key="`${room.id}-${block.source_block}`">
                <span v-if="room.statuses?.[block.minCode] != null">
                  <StatusBadge
                    :status-id="room.statuses![block.minCode]"
                    :title="`${block.label} ${statusNameOf(room.statuses![block.minCode])}`"
                  />
                </span>
                <span v-else class="cell-empty">–</span>
              </td>
            </template>
          </tr>
        </tbody>
      </table>
    </div>

    <div class="fd-foot">
      共 {{ rooms.length }} 间教室
      <span v-if="collapsed" class="fd-foot-note">同一大节内小节状态一致，折叠后取该节状态</span>
    </div>
  </div>
</template>

<style scoped>
.fd-card {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.fd-head {
  display: flex;
  align-items: center;
  gap: 10px;
}

.fd-title {
  font-size: 16px;
  font-weight: 700;
  color: var(--color-text-primary);
}

.fd-sub {
  font-size: 12px;
  color: var(--color-text-tertiary);
  flex: 1;
}

.fd-toggle {
  flex-shrink: 0;
}

.fd-table {
  font-size: 13px;
}

.room-name {
  font-weight: 600;
  word-break: break-all;
}

.room-short-id {
  margin-left: 6px;
  font-size: 11px;
  font-weight: 400;
  color: var(--color-text-tertiary);
}

.cell-empty {
  color: var(--color-border-strong);
  font-size: 12px;
}

.fd-foot {
  font-size: 12px;
  color: var(--color-text-tertiary);
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
}

.fd-foot-note {
  opacity: 0.8;
}
</style>

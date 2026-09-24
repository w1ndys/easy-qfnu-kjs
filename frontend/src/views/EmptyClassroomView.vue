<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import AppFooter from '@/components/AppFooter.vue'
import AppHeader from '@/components/AppHeader.vue'
import EmptyState from '@/components/EmptyState.vue'
import ErrorState from '@/components/ErrorState.vue'
import LoadingSpinner from '@/components/LoadingSpinner.vue'
import QueryFilters from '@/components/QueryFilters.vue'
import StatusNotice from '@/components/StatusNotice.vue'
import { ApiError, PROBLEM_CODES, toApiError } from '@/api/client'
import { fetchEmptyClassrooms } from '@/api/endpoints'
import type { EmptyClassroomsResponse } from '@/api/types'
import { useBootstrap } from '@/composables/useBootstrap'
import { buildRoomLabels, formatBeijing, weekdayLabel } from '@/utils/format'
import { parsePublishedWeeks, sortedNodes } from '@/utils/manifest'

// ---------- 数据引导（manifest + context） ----------
const {
  manifest,
  context,
  loading: bootLoading,
  error: bootError,
  load: loadBootstrap,
} = useBootstrap()

// ---------- 表单状态（Q123：不默认选择分组；Q81：context 自动填充周/星期） ----------
const groupId = ref<string | null>(null)
const keyword = ref('')
const week = ref<number | null>(null)
const day = ref<number | null>(null)
const startNode = ref('01')
const endNode = ref('12')

const searching = ref(false)
const searched = ref(false)
const result = ref<EmptyClassroomsResponse | null>(null)
const queryError = ref<ApiError | null>(null)

const inTeaching = computed(() => context.value?.in_teaching_calendar === true)
const teachingWeek = computed(() => {
  const w = context.value?.week
  return w != null && w > 0 ? w : null
})
const teachingDay = computed(() => context.value?.weekday ?? context.value?.day ?? null)

const groupName = computed(
  () => manifest.value?.groups.find((g) => g.id === groupId.value)?.name ?? '',
)

const weekOptions = computed(() =>
  parsePublishedWeeks(manifest.value?.weeks).map((row) => ({
    value: row.week,
    label: `第 ${row.week} 周`,
  })),
)
const dayOptions = computed(() =>
  Array.from({ length: 7 }, (_, i) => ({ value: i + 1, label: weekdayLabel(i + 1) })),
)
const nodeOptions = computed(() => {
  if (!manifest.value) return []
  return sortedNodes(manifest.value).map((node) => ({
    value: node.code,
    label: node.code,
    sub: `第 ${Number(node.code)} 节`,
  }))
})

// ---------- 节次范围约束：01..12 且 start <= end（Q143/Q144） ----------
watch(startNode, (start) => {
  if (endNode.value < start) endNode.value = start
})
watch(endNode, (end) => {
  if (startNode.value > end) startNode.value = end
})

// ---------- context 就绪后自动填充当前周/星期（仅首次，Q81） ----------
watch(
  [inTeaching, teachingWeek, teachingDay],
  ([teaching, wk, dy]) => {
    if (!teaching || wk == null || dy == null) return
    if (!searched.value) {
      week.value = wk
      day.value = dy
    }
  },
  { immediate: true },
)

/** “今天”按钮：一键恢复当前周次与星期（Q81） */
function goToday() {
  if (!inTeaching.value) return
  const wk = teachingWeek.value
  const dy = teachingDay.value
  if (wk != null) week.value = wk
  if (dy != null) day.value = dy
}

// ---------- 查询 ----------
async function runQuery() {
  if (!groupId.value || searching.value) return

  // 非教学周不允许省略周次/星期（本地友好校验，避免打到 API 的 not_in_teaching）
  if (!inTeaching.value && (week.value == null || day.value == null)) {
    queryError.value = new ApiError({
      kind: 'http',
      httpStatus: 400,
      code: PROBLEM_CODES.NOT_IN_TEACHING,
      message: '当前不在教学周内，请先手动选择已发布的教学周与星期再查询。',
    })
    searched.value = true
    result.value = null
    return
  }

  searching.value = true
  queryError.value = null
  try {
    result.value = await fetchEmptyClassrooms({
      group_id: groupId.value,
      keyword: keyword.value,
      week: week.value,
      day: day.value,
      start_node: startNode.value,
      end_node: endNode.value,
    })
    searched.value = true
  } catch (err) {
    queryError.value = toApiError(err)
    result.value = null
    searched.value = true
  } finally {
    searching.value = false
  }
}

function retryQuery() {
  void runQuery()
}

// ---------- 结果渲染辅助 ----------
const staleFlag = computed(() => result.value?.snapshot?.stale === true)
const generatedAt = computed(() => result.value?.snapshot?.generated_at ?? '')
const rooms = computed(() => result.value?.rooms ?? [])
const roomLabels = computed(() => buildRoomLabels(rooms.value))

const isNotFoundish = computed(
  () =>
    queryError.value?.code === PROBLEM_CODES.NOT_COLLECTED ||
    queryError.value?.code === PROBLEM_CODES.NOT_FOUND,
)
const isServerish = computed(
  () =>
    queryError.value != null &&
    (queryError.value.kind === 'network' ||
      queryError.value.kind === 'timeout' ||
      queryError.value.httpStatus == null ||
      queryError.value.httpStatus >= 500),
)

function quickPickGroup(id: string) {
  if (groupId.value === id) return
  groupId.value = id
}
</script>

<template>
  <div class="page-container">
    <AppHeader title="空教室查询" show-back />

    <div class="page-content">
      <!-- 非教学周提示 -->
      <StatusNotice
        v-if="!bootLoading && manifest && !inTeaching"
        type="warning"
        class="notice-gap"
      >
        当前不在教学周内，不会自动填入周次；可手动选择已发布的教学周与星期后查询。
      </StatusNotice>

      <!-- 引导加载 / 失败 -->
      <LoadingSpinner v-if="bootLoading" text="正在加载查询数据…" />
      <div v-else-if="bootError" class="notice-gap">
        <ErrorState
          title="无法加载查询数据"
          :message="bootError.message"
          show-retry
          @retry="loadBootstrap"
        />
      </div>

      <template v-else>
        <!-- 查询表单 -->
        <QueryFilters
          v-model:group-id="groupId"
          v-model:keyword="keyword"
          v-model:week="week"
          v-model:day="day"
          v-model:start-node="startNode"
          v-model:end-node="endNode"
          :loaded="!!manifest"
          :groups="manifest?.groups ?? []"
          :week-options="weekOptions"
          :day-options="dayOptions"
          :node-options="nodeOptions"
          :submit-loading="searching"
          :today-enabled="inTeaching"
          submit-text="查询空闲教室"
          @submit="runQuery"
          @today="goToday"
        />

        <!-- 数据过期提示条（含生成时间；数据仍展示） -->
        <StatusNotice
          v-if="searched && result && staleFlag"
          type="warning"
          icon="clock-o"
          class="notice-gap"
        >
          该教学周数据已过期（{{
            generatedAt ? `生成于 ${formatBeijing(generatedAt)}` : '最近一次采集后未再更新'
          }}），以下为最近一次成功采集的结果，请留意准确性。
        </StatusNotice>

        <!-- 查询失败 -->
        <template v-if="searched && queryError">
          <!-- 未收录 / 404：含可用分组提示 -->
          <div v-if="isNotFoundish" class="notice-gap not-collected app-card">
            <div class="nc-title">
              <van-icon name="warning-o" color="var(--color-warning-fg)" />
              <span>{{ queryError.problem?.title || '查询范围暂无收录数据' }}</span>
            </div>
            <div class="nc-desc">{{ queryError.message }}</div>
            <div v-if="manifest && manifest.groups.length > 0" class="nc-groups">
              <span class="nc-groups-label">可尝试以下已收录分组：</span>
              <van-tag
                v-for="group in manifest.groups"
                :key="group.id"
                plain
                round
                type="primary"
                class="nc-group-tag"
                @click="quickPickGroup(group.id)"
              >
                {{ group.name }}（{{ group.room_count }}）
              </van-tag>
            </div>
          </div>

          <!-- 无快照：503 -->
          <div v-else-if="queryError.code === PROBLEM_CODES.NO_SNAPSHOT" class="notice-gap">
            <ErrorState
              title="暂无可查询的数据"
              :message="queryError.message"
              show-retry
              retry-text="重新查询"
              @retry="retryQuery"
            />
          </div>

          <!-- 网络 / 超时 / 5xx：可重试 -->
          <div v-else-if="isServerish" class="notice-gap">
            <ErrorState
              title="查询失败，请稍后重试"
              :message="queryError.message"
              show-retry
              retry-text="重新查询"
              @retry="retryQuery"
            />
          </div>

          <!-- 参数类错误（400 等）：提示性展示 -->
          <div v-else class="notice-gap">
            <StatusNotice type="error" icon="warning-o">
              {{ queryError.message }}
            </StatusNotice>
          </div>
        </template>

        <!-- 查询结果：空态（有效查询但没有空教室 = 正常空集合） -->
        <template v-else-if="searched && result && rooms.length === 0">
          <div class="section-gap" />
          <EmptyState
            text="该时段没有空闲教室"
            :description="`${groupName || groupId} · ${
              week != null ? `第 ${week} 周` : '当前周'
            } ${weekdayLabel(day)} · ${startNode}—${endNode} 节`"
          />
        </template>

        <!-- 查询结果：教室列表 -->
        <template v-else-if="searched && result && rooms.length > 0">
          <div class="result-head app-card section-gap">
            <div class="result-meta">
              <span class="result-group">{{ groupName || groupId }}</span>
              <van-tag plain round type="primary" size="small">
                共 {{ rooms.length }} 间空闲
              </van-tag>
            </div>
            <div class="result-sub">
              {{ week != null ? `第 ${week} 周` : '当前周' }}
              {{ day != null ? weekdayLabel(day) : '' }}
              · {{ startNode }}—{{ endNode }} 节全空闲
            </div>
          </div>

          <div class="room-list">
            <div v-for="room in rooms" :key="room.id" class="app-card room-item">
              <span class="room-icon">
                <van-icon name="wap-home-o" size="18" />
              </span>
              <span class="room-name">{{ roomLabels.get(room.id)?.name ?? room.name }}</span>
              <span v-if="roomLabels.get(room.id)?.shortId" class="room-short">
                #{{ roomLabels.get(room.id)?.shortId }}
              </span>
            </div>
          </div>
        </template>

        <!-- 尚未查询 -->
        <EmptyState
          v-if="!searched"
          text="选择分组与时间后查询空闲教室"
          description="分组必选；周次与星期默认使用当前教学周，也可手动调整。"
        />
      </template>

      <AppFooter class="footer-gap" />
    </div>
  </div>
</template>

<style scoped>
.notice-gap {
  margin-bottom: 12px;
}

.not-collected {
  display: flex;
  flex-direction: column;
  gap: 8px;
  border-color: var(--color-warning-border);
}

.nc-title {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 15px;
  font-weight: 700;
  color: var(--color-warning-fg);
}

.nc-desc {
  font-size: 13px;
  color: var(--color-text-secondary);
  line-height: 1.6;
}

.nc-groups {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  padding-top: 4px;
}

.nc-groups-label {
  font-size: 12px;
  color: var(--color-text-tertiary);
}

.nc-group-tag {
  cursor: pointer;
}

.result-head {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.result-meta {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}

.result-group {
  font-size: 16px;
  font-weight: 700;
  color: var(--color-text-primary);
}

.result-sub {
  font-size: 12px;
  color: var(--color-text-tertiary);
}

.room-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.room-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px 14px;
}

.room-item:active {
  background: var(--color-brand-100);
}

.room-icon {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 34px;
  height: 34px;
  border-radius: 10px;
  background: var(--color-brand-100);
  color: var(--color-brand-500);
  flex-shrink: 0;
}

.room-name {
  font-size: 15px;
  font-weight: 600;
  color: var(--color-text-primary);
  word-break: break-all;
}

.room-short {
  font-size: 12px;
  color: var(--color-text-tertiary);
  margin-left: auto;
  flex-shrink: 0;
}

.section-gap {
  height: 0;
}

.footer-gap {
  margin-top: 16px;
}
</style>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import AppFooter from '@/components/AppFooter.vue'
import AppHeader from '@/components/AppHeader.vue'
import EmptyState from '@/components/EmptyState.vue'
import ErrorState from '@/components/ErrorState.vue'
import FullDayTable from '@/components/FullDayTable.vue'
import LegendPanel from '@/components/LegendPanel.vue'
import LoadingSpinner from '@/components/LoadingSpinner.vue'
import QueryFilters from '@/components/QueryFilters.vue'
import StatusNotice from '@/components/StatusNotice.vue'
import { ApiError, PROBLEM_CODES, toApiError } from '@/api/client'
import { fetchFullDayStatus } from '@/api/endpoints'
import type { FullDayStatusResponse } from '@/api/types'
import { useBootstrap } from '@/composables/useBootstrap'
import { formatBeijing, weekdayLabel } from '@/utils/format'
import { parsePublishedWeeks, sortedNodes } from '@/utils/manifest'
import { buildStatusMeta } from '@/constants/statusVisuals'

// ---------- 数据引导（manifest + context） ----------
const {
  manifest,
  context,
  loading: bootLoading,
  error: bootError,
  load: loadBootstrap,
} = useBootstrap()

// ---------- 表单状态 ----------
const groupId = ref<string | null>(null)
const keyword = ref('')
const week = ref<number | null>(null)
const day = ref<number | null>(null)

const searching = ref(false)
const searched = ref(false)
const result = ref<FullDayStatusResponse | null>(null)
const queryError = ref<ApiError | null>(null)

/** 折叠大节开关：默认逐节（12 小节）展示，可切换按 5 个 source_block 折叠（Q85/Q150） */
const collapsed = ref(false)

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

const statusMeta = computed(() => buildStatusMeta(manifest.value?.statuses))
const tableNodes = computed(() => (manifest.value ? sortedNodes(manifest.value) : []))
const rooms = computed(() => result.value?.rooms ?? [])
const staleFlag = computed(() => result.value?.snapshot?.stale === true)
const generatedAt = computed(() => result.value?.snapshot?.generated_at ?? '')

// ---------- context 就绪后自动填充当前周/星期 ----------
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
    result.value = await fetchFullDayStatus({
      group_id: groupId.value,
      keyword: keyword.value,
      week: week.value,
      day: day.value,
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
    <AppHeader title="教室全天状态" show-back />

    <div class="page-content">
      <StatusNotice
        v-if="!bootLoading && manifest && !inTeaching"
        type="warning"
        class="notice-gap"
      >
        当前不在教学周内，不会自动填入周次；可手动选择已发布的教学周与星期后查询。
      </StatusNotice>

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
        <QueryFilters
          v-model:group-id="groupId"
          v-model:keyword="keyword"
          v-model:week="week"
          v-model:day="day"
          :loaded="!!manifest"
          :groups="manifest?.groups ?? []"
          :week-options="weekOptions"
          :day-options="dayOptions"
          :submit-loading="searching"
          :today-enabled="inTeaching"
          :show-node-range="false"
          submit-text="查看全天状态"
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

          <div v-else-if="queryError.code === PROBLEM_CODES.NO_SNAPSHOT" class="notice-gap">
            <ErrorState
              title="暂无可查询的数据"
              :message="queryError.message"
              show-retry
              retry-text="重新查询"
              @retry="retryQuery"
            />
          </div>

          <div v-else-if="isServerish" class="notice-gap">
            <ErrorState
              title="查询失败，请稍后重试"
              :message="queryError.message"
              show-retry
              retry-text="重新查询"
              @retry="retryQuery"
            />
          </div>

          <div v-else class="notice-gap">
            <StatusNotice type="error" icon="warning-o">
              {{ queryError.message }}
            </StatusNotice>
          </div>
        </template>

        <!-- 查询结果 -->
        <template v-else-if="searched && result">
          <div class="result-head app-card section-gap">
            <div class="result-meta">
              <span class="result-group">{{ groupName || groupId }}</span>
              <van-tag plain round type="primary" size="small">
                共 {{ rooms.length }} 间教室
              </van-tag>
            </div>
            <div class="result-sub">
              {{ week != null ? `第 ${week} 周` : '当前周' }}
              {{ day != null ? weekdayLabel(day) : '' }}
              <template v-if="keyword.trim()"> · 关键词「{{ keyword.trim() }}」</template>
            </div>
          </div>

          <!-- 图例（manifest 状态字典驱动 + 前端色表） -->
          <div class="section-gap">
            <LegendPanel
              :statuses="manifest?.statuses"
              note="图例由 manifest 状态字典生成：绿系“空闲 / 完全空闲”为可用状态，其余视为占用；颜色与图标仅在前端映射。"
            />
          </div>

          <EmptyState
            v-if="rooms.length === 0"
            text="该分组下没有匹配的教室"
            description="可尝试清空房间关键词，或选择其他已收录分组。"
          />
          <div v-else class="app-card section-gap table-card">
            <FullDayTable
              v-model:collapsed="collapsed"
              :nodes="tableNodes"
              :rooms="rooms"
              :status-meta="statusMeta"
            />
          </div>
        </template>

        <!-- 尚未查询 -->
        <EmptyState
          v-if="!searched"
          text="选择分组与时间后查看全天状态"
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

.table-card {
  overflow: hidden;
}

.footer-gap {
  margin-top: 16px;
}
</style>

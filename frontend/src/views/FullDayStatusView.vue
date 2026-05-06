<script setup>
import { reactive, ref } from 'vue'
import { getErrorMessage, queryFullDayStatus } from '@/api'
import { useSystemStatus } from '@/composables/useSystemStatus'
import { useSearchHistory } from '@/composables/useSearchHistory'
import { useTopBuildings } from '@/composables/useTopBuildings'
import { useBuildingAliasReminder } from '@/composables/useBuildingAliasReminder'
import { useAlertDialog } from '@/composables/useAlertDialog'
import AppFooter from '@/components/AppFooter.vue'
import AppHeader from '@/components/AppHeader.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import DateSelector from '@/components/DateSelector.vue'
import EmptyState from '@/components/EmptyState.vue'
import LoadingSpinner from '@/components/LoadingSpinner.vue'
import QRCodeCard from '@/components/QRCodeCard.vue'
import StatusWarning from '@/components/StatusWarning.vue'

const { statusLoading, inTeachingCalendar, hasPermission } = useSystemStatus()
const { history, addToHistory, clearHistory } = useSearchHistory()
const { topQueries } = useTopBuildings()
const {
  dialogOpen: aliasDialogOpen,
  normalizeBuildingName,
  confirmReminder,
  cancelReminder,
} = useBuildingAliasReminder()
const { alertState, showAlert, closeAlert } = useAlertDialog()

function selectTopQuery(query) {
  form.building = query.building
  form.offset = query.date_offset
  showHistory.value = false
  search()
}

const loading = ref(false)
const hasSearched = ref(false)
const resultData = ref(null)
const showHistory = ref(false)
const inputFocused = ref(false)

const form = reactive({
  building: '',
  offset: 0,
})

const legendItems = [
  { id: 1, code: '课', name: '正常上课', color: '#B42318', bg: '#FDEEEE', border: '#F5B3AE' },
  { id: 2, code: '借', name: '借用', color: '#9A5A00', bg: '#FFF6E8', border: '#F3CF8D' },
  { id: 3, code: '锁', name: '锁定', color: '#4C433D', bg: '#F2EEEA', border: '#D1C7BE' },
  { id: 4, code: '考', name: '考试', color: '#1D4ED8', bg: '#ECF3FF', border: '#B7CBFF' },
  { id: 5, code: '空', name: '空闲', color: '#156B52', bg: '#EAF8F3', border: '#A7DEC7' },
  { id: 6, code: '固', name: '固定调课', color: '#1D4ED8', bg: '#ECF3FF', border: '#B7CBFF' },
  { id: 7, code: '临', name: '临时调课', color: '#5F3517', bg: '#F3E5D8', border: '#E7CFBA' },
  { id: 8, code: '全', name: '完全空闲', color: '#156B52', bg: '#EAF8F3', border: '#A7DEC7' },
  { id: 9, code: '混', name: '跨模式', color: '#4C433D', bg: '#FAF8F6', border: '#E5DED7' },
]

function getStatusItem(statusId) {
  return legendItems.find((item) => item.id === statusId) || { code: '-', name: '未知', color: '#8A7C70', bg: '#FFFFFF', border: '#E5DED7' }
}

function onInputFocus() {
  inputFocused.value = true
  showHistory.value = true
}

function onInputBlur() {
  inputFocused.value = false
  setTimeout(() => {
    showHistory.value = false
  }, 200)
}

function onInputChange() {
  showHistory.value = false
}

function applySearchItem(item) {
  form.building = item.building || item
  if (item.offset !== undefined) form.offset = item.offset
  if (item.date_offset !== undefined) form.offset = item.date_offset
  showHistory.value = false
}

function selectHistoryItem(item) {
  applySearchItem(item)
}

async function search() {
  const building = await normalizeBuildingName(form.building)

  if (!building) {
    return
  }

  form.building = building

  loading.value = true
  hasSearched.value = false
  resultData.value = null
  showHistory.value = false

  try {
    const data = await queryFullDayStatus({
      building,
      date_offset: form.offset,
    })

    resultData.value = data
    hasSearched.value = true
    addToHistory({
      building,
      offset: form.offset,
    })
  } catch (error) {
    console.error(error)
    showAlert(getErrorMessage(error, '查询失败'), {
      title: '查询失败',
    })
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <AppHeader title="教室全天状态" showBack />

    <div class="page-content full-day-content">
      <StatusWarning
        v-if="!hasPermission && !statusLoading"
        type="error"
        title="权限不足"
        message="当前账号无权限访问教务系统查询接口，请检查账号状态。"
      />

      <StatusWarning
        v-if="!inTeachingCalendar && !statusLoading"
        type="warning"
        title="提示"
        message="当前日期不在教学周历内，查询结果可能不准确。"
      />

      <LoadingSpinner v-if="statusLoading" text="正在检查系统状态..." />

      <!-- 搜索表单 -->
      <div v-else class="app-card form-card">
        <div class="form-section">
          <div class="form-label">教学楼</div>
          <van-field
            v-model="form.building"
            placeholder="例如：老文史楼"
            left-icon="wap-home-o"
            clearable
            :border="false"
            @focus="onInputFocus"
            @blur="onInputBlur"
            @update:model-value="onInputChange"
          />

          <!-- 搜索历史 -->
          <div v-if="inputFocused && showHistory && history.length > 0" class="history-dropdown">
            <div class="history-header">
              <span>搜索历史</span>
              <span class="history-clear" @click="clearHistory">清除</span>
            </div>
            <van-cell
              v-for="(item, index) in history"
              :key="index"
              :title="item.label || item.building || item"
              icon="clock-o"
              clickable
              @mousedown.prevent="selectHistoryItem(item)"
            />
          </div>
        </div>

        <!-- 热搜 -->
        <div v-if="topQueries.length > 0" class="hot-queries">
          <span class="hot-label">热搜</span>
          <van-tag
            v-for="(query, idx) in topQueries"
            :key="idx"
            plain
            round
            type="primary"
            class="hot-tag"
            @click="selectTopQuery(query)"
          >
            {{ query.label }}
          </van-tag>
        </div>

        <DateSelector v-model="form.offset" />

        <van-button
          type="primary"
          block
          round
          size="large"
          :loading="loading"
          :disabled="!form.building.trim()"
          loading-text="查询中..."
          @click="search"
        >
          查询全天状态
        </van-button>
      </div>

      <!-- 图例 -->
      <div v-if="hasSearched" class="app-card legend-card">
        <div class="legend-title">状态图例</div>
        <div class="legend-grid">
          <div v-for="item in legendItems" :key="item.id" class="legend-item">
            <span
              class="status-badge"
              :style="{ background: item.bg, color: item.color, borderColor: item.border }"
            >
              {{ item.code }}
            </span>
            <span class="legend-name">{{ item.name }}</span>
          </div>
        </div>
      </div>

      <!-- 结果表格 -->
      <div v-if="hasSearched && resultData" class="app-card table-card">
        <div class="table-info">
          {{ resultData.building }} -- {{ resultData.date }} -- {{ resultData.current_term }} 学期 -- 第{{ resultData.week }}周 -- 星期{{ resultData.day_of_week }}
        </div>

        <div class="status-table-container">
          <table class="status-table">
            <thead>
              <tr>
                <th>教室</th>
                <th v-for="node in resultData.node_list" :key="node.node_index">
                  {{ node.node_name }}
                </th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="room in resultData.classrooms" :key="room.room_name">
                <td>{{ room.room_name }}</td>
                <td v-for="(status, idx) in room.status" :key="`${room.room_name}-${idx}`">
                  <span
                    class="status-badge"
                    :style="{
                      background: getStatusItem(status.status_id).bg,
                      color: getStatusItem(status.status_id).color,
                      borderColor: getStatusItem(status.status_id).border,
                    }"
                    :title="getStatusItem(status.status_id).name"
                  >
                    {{ getStatusItem(status.status_id).code }}
                  </span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <EmptyState v-if="hasSearched && !loading && !resultData" text="暂无数据" />

      <QRCodeCard />
    </div>

    <AppFooter />

    <ConfirmDialog
      :open="aliasDialogOpen"
      title="教学楼名称提醒"
      :message="'你是否要搜索\u201C综合教学楼\u201D？注意老校区综合楼全称是\u201C综合教学楼\u201D哦！~'"
      confirm-text="改为综合教学楼"
      cancel-text="继续搜索综合楼"
      @confirm="confirmReminder"
      @cancel="cancelReminder"
    />

    <ConfirmDialog
      :open="alertState.open"
      :title="alertState.title"
      :message="alertState.message"
      :confirm-text="alertState.buttonText"
      :show-cancel="false"
      @confirm="closeAlert"
      @cancel="closeAlert"
    />
  </div>
</template>

<style scoped>
.full-day-content {
  max-width: 1280px;
}

.form-card {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.form-section {
  position: relative;
}

.form-label {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text-secondary);
  margin-bottom: 8px;
}

.form-section :deep(.van-field) {
  border-radius: 10px;
  border: 1px solid var(--color-border-subtle);
  background: var(--color-surface-card);
}

.history-dropdown {
  position: absolute;
  top: 100%;
  left: 0;
  right: 0;
  z-index: 40;
  margin-top: 4px;
  background: var(--color-surface-card);
  border-radius: 12px;
  border: 1px solid var(--color-border-subtle);
  box-shadow: 0 6px 20px rgba(31, 27, 24, 0.08);
  overflow: hidden;
}

.history-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 10px 16px;
  border-bottom: 1px solid var(--color-border-subtle);
  font-size: 12px;
  color: var(--color-text-tertiary);
}

.history-clear {
  cursor: pointer;
  color: var(--color-brand-500);
}

.hot-queries {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}

.hot-label {
  font-size: 12px;
  color: var(--color-text-tertiary);
  font-weight: 500;
}

.hot-tag {
  cursor: pointer;
}

.legend-card {
  margin-top: 16px;
}

.legend-title {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text-primary);
  margin-bottom: 12px;
}

.legend-grid {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: 8px;
}

@media (min-width: 480px) {
  .legend-grid {
    grid-template-columns: repeat(3, 1fr);
  }
}

@media (min-width: 768px) {
  .legend-grid {
    grid-template-columns: repeat(5, 1fr);
  }
}

.legend-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 10px;
  border-radius: 8px;
  border: 1px solid var(--color-border-subtle);
  background: var(--color-surface-card);
}

.legend-name {
  font-size: 12px;
  color: var(--color-text-secondary);
}

.table-card {
  margin-top: 16px;
  padding: 0;
  overflow: hidden;
}

.table-info {
  padding: 12px 16px;
  font-size: 13px;
  color: var(--color-text-secondary);
  border-bottom: 1px solid var(--color-border-subtle);
}
</style>

<script setup>
import { computed, reactive, ref } from 'vue'
import { getErrorMessage, queryClassrooms } from '@/api'
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

const { statusLoading, inTeachingCalendar, hasPermission, currentWeek, currentTerm } = useSystemStatus()
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
  if (query.start_node) form.start = query.start_node
  if (query.end_node) form.end = query.end_node
  showHistory.value = false
  search()
}

const loading = ref(false)
const hasSearched = ref(false)
const results = ref([])
const resultInfo = ref(null)
const displayLimit = ref(100)
const showHistory = ref(false)
const inputFocused = ref(false)
const showStartPicker = ref(false)
const showEndPicker = ref(false)

const form = reactive({
  building: '',
  offset: 0,
  start: '01',
  end: '11',
})

const nodeOptions = Array.from({ length: 11 }, (_, index) => ({
  text: String(index + 1).padStart(2, '0'),
  value: String(index + 1).padStart(2, '0'),
}))

const displayedResults = computed(() => results.value.slice(0, displayLimit.value))

const showHistoryList = computed(() => inputFocused.value && showHistory.value && history.value.length > 0)

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
  if (item.start) form.start = item.start
  if (item.end) form.end = item.end
  if (item.start_node) form.start = item.start_node
  if (item.end_node) form.end = item.end_node
  showHistory.value = false
}

function selectHistoryItem(item) {
  applySearchItem(item)
}

async function search() {
  const building = await normalizeBuildingName(form.building)

  if (!building) {
    showAlert('请输入教学楼', {
      title: '搜索条件不完整',
    })
    return
  }

  form.building = building

  loading.value = true
  displayLimit.value = 100
  hasSearched.value = false
  results.value = []
  resultInfo.value = null
  showHistory.value = false

  try {
    const data = await queryClassrooms({
      building,
      start_node: form.start,
      end_node: form.end,
      date_offset: form.offset,
    })

    results.value = data.classrooms || []
    resultInfo.value = {
      date: data.date,
      week: data.week,
      day: data.day_of_week,
    }
    hasSearched.value = true
    addToHistory({
      building,
      offset: form.offset,
      start: form.start,
      end: form.end,
    })
  } catch (error) {
    console.error(error)
    showAlert(getErrorMessage(error, '查询出错，请重试'), {
      title: '查询失败',
    })
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <AppHeader title="空教室查询" showBack />

    <div class="page-content">
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

      <div v-else class="app-card form-card">
        <!-- 教学周信息 -->
        <van-notice-bar
          v-if="inTeachingCalendar"
          left-icon="passed"
          :text="`当前：${currentTerm} 第${currentWeek}周`"
          color="var(--color-success-fg)"
          background="var(--color-success-bg)"
          :scrollable="false"
        />

        <!-- 教学楼输入 -->
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

          <!-- 搜索历史下拉 -->
          <div v-if="showHistoryList" class="history-dropdown">
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

        <!-- 日期选择 -->
        <DateSelector v-model="form.offset" />

        <!-- 节次选择 -->
        <van-row gutter="12" class="node-row">
          <van-col span="12">
            <div class="form-label">起始节次</div>
            <van-field
              v-model="form.start"
              is-link
              readonly
              :border="false"
              @click="showStartPicker = true"
            />
            <van-popup v-model:show="showStartPicker" position="bottom" round>
              <van-picker
                :columns="nodeOptions"
                @confirm="({ selectedValues }) => { form.start = selectedValues[0]; showStartPicker = false }"
                @cancel="showStartPicker = false"
              />
            </van-popup>
          </van-col>
          <van-col span="12">
            <div class="form-label">终止节次</div>
            <van-field
              v-model="form.end"
              is-link
              readonly
              :border="false"
              @click="showEndPicker = true"
            />
            <van-popup v-model:show="showEndPicker" position="bottom" round>
              <van-picker
                :columns="nodeOptions"
                @confirm="({ selectedValues }) => { form.end = selectedValues[0]; showEndPicker = false }"
                @cancel="showEndPicker = false"
              />
            </van-popup>
          </van-col>
        </van-row>

        <!-- 查询按钮 -->
        <van-button
          type="primary"
          block
          round
          size="large"
          :loading="loading"
          loading-text="查询中..."
          @click="search"
        >
          查询空闲教室
        </van-button>
      </div>

      <!-- 结果信息 -->
      <div v-if="resultInfo" class="result-info app-card">
        <span>{{ resultInfo.date }} (第{{ resultInfo.week }}周 星期{{ resultInfo.day }})</span>
        <van-tag type="primary" round>共 {{ results.length }} 间</van-tag>
      </div>

      <!-- 结果网格 -->
      <div v-if="results.length > 0" class="results-section">
        <div class="results-grid">
          <div
            v-for="(room, index) in displayedResults"
            :key="`${room}-${index}`"
            class="room-item"
          >
            {{ room }}
          </div>
        </div>

        <div v-if="results.length > displayLimit" class="load-more">
          <van-button plain round type="primary" size="small" @click="displayLimit += 100">
            加载更多 ({{ displayedResults.length }} / {{ results.length }})
          </van-button>
        </div>
      </div>

      <EmptyState v-if="hasSearched && results.length === 0 && !loading" text="该时间段暂无空闲教室" />

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
.form-card {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.form-card :deep(.van-notice-bar) {
  border-radius: 12px;
  margin-bottom: 4px;
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

.form-section :deep(.van-field),
.node-row :deep(.van-field) {
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

.node-row {
  margin-bottom: 4px;
}

.result-info {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 16px;
  font-size: 13px;
  color: var(--color-text-secondary);
  margin-top: 16px;
}

.results-section {
  margin-top: 16px;
}

.results-grid {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: 8px;
}

@media (min-width: 480px) {
  .results-grid {
    grid-template-columns: repeat(4, 1fr);
  }
}

@media (min-width: 768px) {
  .results-grid {
    grid-template-columns: repeat(5, 1fr);
  }
}

.room-item {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 44px;
  padding: 8px;
  border-radius: 10px;
  border: 1px solid var(--color-border-subtle);
  background: var(--color-surface-card);
  font-size: 14px;
  font-weight: 700;
  color: var(--color-brand-500);
  text-align: center;
  transition: all 0.2s;
}

.room-item:active {
  background: var(--color-brand-100);
  border-color: var(--color-brand-200);
}

.load-more {
  text-align: center;
  margin-top: 16px;
}
</style>

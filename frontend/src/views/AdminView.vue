<script setup>
import { ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { showConfirmDialog, showToast } from 'vant'
import {
  adminGetAnnouncements,
  adminCreateAnnouncement,
  adminUpdateAnnouncement,
  adminDeleteAnnouncement,
  getErrorMessage,
} from '@/api'

const router = useRouter()
const announcements = ref([])
const loading = ref(false)
const error = ref('')
const showForm = ref(false)
const editingId = ref(null)
const form = ref({ title: '', content: '', important: false })
const saving = ref(false)

async function fetchList() {
  loading.value = true
  error.value = ''
  try {
    const resp = await adminGetAnnouncements()
    announcements.value = resp.announcements || []
  } catch (e) {
    error.value = getErrorMessage(e, '获取公告列表失败')
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editingId.value = null
  form.value = { title: '', content: '', important: false }
  showForm.value = true
}

function openEdit(item) {
  editingId.value = item.id
  form.value = { title: item.title, content: item.content, important: item.important }
  showForm.value = true
}

function cancelForm() {
  showForm.value = false
  editingId.value = null
  form.value = { title: '', content: '', important: false }
}

async function handleSave() {
  saving.value = true
  error.value = ''
  try {
    if (editingId.value) {
      await adminUpdateAnnouncement(editingId.value, form.value)
    } else {
      await adminCreateAnnouncement(form.value)
    }
    showToast({ message: '保存成功', type: 'success' })
    cancelForm()
    await fetchList()
  } catch (e) {
    error.value = getErrorMessage(e, '保存失败')
  } finally {
    saving.value = false
  }
}

async function handleDelete(id) {
  try {
    await showConfirmDialog({ title: '确认删除', message: '确定要删除这条公告吗？' })
    error.value = ''
    await adminDeleteAnnouncement(id)
    showToast({ message: '删除成功', type: 'success' })
    await fetchList()
  } catch (e) {
    if (e !== 'cancel') {
      error.value = getErrorMessage(e, '删除失败')
    }
  }
}

function handleLogout() {
  localStorage.removeItem('admin_token')
  router.push('/admin/login')
}

onMounted(fetchList)
</script>

<template>
  <div class="page-container">
    <div class="page-content admin-content">
      <!-- 顶部栏 -->
      <div class="admin-header">
        <div class="header-left">
          <van-icon name="volume-o" size="24" color="var(--color-brand-500)" />
          <h1 class="admin-title">公告管理</h1>
        </div>
        <div class="header-right">
          <router-link to="/" class="back-link">返回首页</router-link>
          <van-button plain size="small" type="danger" @click="handleLogout">退出登录</van-button>
        </div>
      </div>

      <!-- 错误提示 -->
      <van-notice-bar
        v-if="error"
        left-icon="warning-o"
        :text="error"
        color="var(--color-error-fg)"
        background="var(--color-error-bg)"
        :scrollable="false"
        closeable
        class="error-bar"
        @close="error = ''"
      />

      <!-- 新增按钮 -->
      <div class="action-bar">
        <van-button type="primary" round icon="plus" @click="openCreate">新增公告</van-button>
      </div>

      <!-- 编辑表单弹出层 -->
      <van-popup v-model:show="showForm" position="bottom" round :style="{ maxHeight: '80%' }">
        <div class="form-popup">
          <div class="form-title">{{ editingId ? '编辑公告' : '新增公告' }}</div>
          <van-form @submit="handleSave">
            <van-cell-group inset>
              <van-field
                v-model="form.title"
                label="标题"
                placeholder="请输入公告标题"
                :rules="[{ required: true, message: '请输入标题' }]"
              />
              <van-field
                v-model="form.content"
                label="内容"
                type="textarea"
                rows="4"
                placeholder="请输入公告内容"
                :rules="[{ required: true, message: '请输入内容' }]"
              />
              <van-cell title="标记为重要公告">
                <template #right-icon>
                  <van-switch v-model="form.important" size="20" />
                </template>
              </van-cell>
            </van-cell-group>

            <div class="form-actions">
              <van-button type="primary" block round :loading="saving" loading-text="保存中..." native-type="submit">
                保存
              </van-button>
              <van-button block round @click="cancelForm">取消</van-button>
            </div>
          </van-form>
        </div>
      </van-popup>

      <!-- 公告列表 -->
      <div v-if="loading" class="loading-wrapper">
        <van-loading type="spinner" color="var(--color-brand-500)">加载中...</van-loading>
      </div>

      <van-empty v-else-if="announcements.length === 0" description="暂无公告" />

      <div v-else class="announcement-list">
        <van-swipe-cell v-for="item in announcements" :key="item.id">
          <div class="admin-announcement-item" :class="{ important: item.important }">
            <div class="item-main">
              <div class="item-title-row">
                <span class="item-title">{{ item.title }}</span>
                <van-tag v-if="item.important" type="warning" round size="small">重要</van-tag>
              </div>
              <p class="item-content">{{ item.content }}</p>
              <p class="item-date">{{ item.created_at }}</p>
            </div>
            <div class="item-actions">
              <van-button plain size="mini" type="primary" @click="openEdit(item)">编辑</van-button>
              <van-button plain size="mini" type="danger" @click="handleDelete(item.id)">删除</van-button>
            </div>
          </div>
          <template #right>
            <van-button square type="danger" text="删除" class="swipe-btn" @click="handleDelete(item.id)" />
          </template>
        </van-swipe-cell>
      </div>
    </div>
  </div>
</template>

<style scoped>
.admin-content {
  max-width: 800px;
}

.admin-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 20px;
}

.header-left {
  display: flex;
  align-items: center;
  gap: 10px;
}

.admin-title {
  font-size: 20px;
  font-weight: 700;
  color: var(--color-text-primary);
  margin: 0;
}

.header-right {
  display: flex;
  align-items: center;
  gap: 12px;
}

.back-link {
  font-size: 13px;
  color: var(--color-text-tertiary);
  text-decoration: none;
}

.error-bar {
  margin-bottom: 16px;
  border-radius: 10px;
}

.action-bar {
  margin-bottom: 16px;
}

.form-popup {
  padding: 20px 16px;
}

.form-title {
  font-size: 18px;
  font-weight: 700;
  color: var(--color-text-primary);
  margin-bottom: 16px;
  text-align: center;
}

.form-actions {
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.loading-wrapper {
  display: flex;
  justify-content: center;
  padding: 48px 0;
}

.announcement-list {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.admin-announcement-item {
  background: var(--color-surface-card);
  border-radius: 12px;
  border: 1px solid var(--color-border-subtle);
  padding: 14px 16px;
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
}

.admin-announcement-item.important {
  border-color: #F3CF8D;
  background: var(--color-warning-bg);
}

.item-main {
  flex: 1;
  min-width: 0;
}

.item-title-row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.item-title {
  font-size: 14px;
  font-weight: 700;
  color: var(--color-text-primary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.item-content {
  font-size: 13px;
  color: var(--color-text-secondary);
  margin: 4px 0;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

.item-date {
  font-size: 11px;
  color: var(--color-text-tertiary);
  margin: 4px 0 0;
}

.item-actions {
  display: flex;
  gap: 6px;
  flex-shrink: 0;
}

.swipe-btn {
  height: 100%;
}

:deep(.van-cell-group--inset) {
  margin: 0;
}
</style>

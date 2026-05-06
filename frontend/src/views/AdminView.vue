<script setup>
import { ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { showConfirmDialog, showToast } from 'vant'
import {
  adminGetAnnouncements,
  adminCreateAnnouncement,
  adminUpdateAnnouncement,
  adminDeleteAnnouncement,
  adminGetAPIConfig,
  adminUpdateAPIConfig,
  adminGetAIModels,
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
const configLoading = ref(false)
const configSaving = ref(false)
const modelLoading = ref(false)
const modelOptions = ref([])
const configForm = ref({
  ai_base_url: '',
  ai_key: '',
  ai_model: '',
  open_api_enabled: false,
  open_api_key: '',
})

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

async function fetchConfig() {
  configLoading.value = true
  try {
    configForm.value = await adminGetAPIConfig()
  } catch (e) {
    error.value = getErrorMessage(e, '获取开放接口配置失败')
  } finally {
    configLoading.value = false
  }
}

async function saveConfig() {
  configSaving.value = true
  error.value = ''
  try {
    configForm.value = await adminUpdateAPIConfig(configForm.value)
    showToast({ message: '配置已保存', type: 'success' })
  } catch (e) {
    error.value = getErrorMessage(e, '保存开放接口配置失败')
  } finally {
    configSaving.value = false
  }
}

async function fetchModels() {
  modelLoading.value = true
  error.value = ''
  try {
    const resp = await adminGetAIModels()
    modelOptions.value = (resp.models || []).map((model) => ({ text: model, value: model }))
    if (!configForm.value.ai_model && modelOptions.value.length > 0) {
      configForm.value.ai_model = modelOptions.value[0].value
    }
    showToast({ message: '模型列表已更新', type: 'success' })
  } catch (e) {
    error.value = getErrorMessage(e, '获取模型列表失败')
  } finally {
    modelLoading.value = false
  }
}

function generateOpenAPIKey() {
  const bytes = new Uint8Array(24)
  crypto.getRandomValues(bytes)
  configForm.value.open_api_key = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('')
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

onMounted(() => {
  fetchList()
  fetchConfig()
})
</script>

<template>
  <div class="page-container">
    <div class="page-content admin-content">
      <!-- 顶部栏 -->
      <div class="admin-header">
        <div class="header-left">
          <van-icon name="setting-o" size="24" color="var(--color-brand-500)" />
          <h1 class="admin-title">后台管理</h1>
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

      <div class="admin-section app-card">
        <div class="section-header">
          <div>
            <h2>大模型 API 配置</h2>
            <p>支持 OpenAI 兼容接口，用于自然语言解析查询条件。</p>
          </div>
          <van-button size="small" plain type="primary" :loading="modelLoading" @click="fetchModels">获取模型</van-button>
        </div>
        <van-form @submit="saveConfig">
          <van-cell-group inset>
            <van-field v-model="configForm.ai_base_url" label="BaseURL" placeholder="https://api.openai.com 或兼容地址" />
            <van-field v-model="configForm.ai_key" label="Key" type="password" placeholder="请输入 API Key" />
            <van-field
              v-if="modelOptions.length === 0"
              v-model="configForm.ai_model"
              label="Model"
              placeholder="例如 gpt-4o-mini"
            />
            <van-field v-else label="Model">
              <template #input>
                <van-dropdown-menu class="model-menu">
                  <van-dropdown-item v-model="configForm.ai_model" :options="modelOptions" />
                </van-dropdown-menu>
              </template>
            </van-field>
          </van-cell-group>

          <div class="section-header open-api-title">
            <div>
              <h2>开放接口控制面板</h2>
              <p>外部调用使用授权 Key，不受前台高频限制影响。</p>
            </div>
          </div>
          <van-cell-group inset>
            <van-cell title="启用开放接口">
              <template #right-icon>
                <van-switch v-model="configForm.open_api_enabled" size="20" />
              </template>
            </van-cell>
            <van-field v-model="configForm.open_api_key" label="授权 Key" placeholder="点击随机生成或手动输入">
              <template #button>
                <van-button size="small" type="primary" plain @click="generateOpenAPIKey">随机生成</van-button>
              </template>
            </van-field>
          </van-cell-group>

          <div class="api-docs">
            <p>直接查询：POST /api/v1/open/query</p>
            <p>AI 查询：POST /api/v1/open/ai-query</p>
            <p>请求头：X-API-Key: {{ configForm.open_api_key || 'your-key' }}</p>
          </div>

          <div class="form-actions compact-actions">
            <van-button type="primary" block round :loading="configSaving || configLoading" native-type="submit">保存配置</van-button>
          </div>
        </van-form>
      </div>

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

.admin-section {
  margin-bottom: 18px;
  padding: 16px 0;
}

.section-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  padding: 0 16px 12px;
}

.section-header h2 {
  margin: 0 0 4px;
  font-size: 16px;
  color: var(--color-text-primary);
}

.section-header p {
  margin: 0;
  font-size: 12px;
  line-height: 1.5;
  color: var(--color-text-tertiary);
}

.open-api-title {
  padding-top: 16px;
}

.model-menu {
  width: 100%;
}

.model-menu :deep(.van-dropdown-menu__bar) {
  height: 28px;
  box-shadow: none;
}

.api-docs {
  margin: 12px 16px 0;
  padding: 10px 12px;
  border-radius: 10px;
  background: var(--color-surface-muted);
  color: var(--color-text-secondary);
  font-size: 12px;
  line-height: 1.6;
  overflow-wrap: anywhere;
}

.api-docs p {
  margin: 0;
}

.compact-actions {
  padding-bottom: 0;
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

@media (max-width: 640px) {
  .admin-header,
  .section-header {
    flex-direction: column;
  }

  .header-right {
    width: 100%;
    justify-content: space-between;
  }
}
</style>

<script setup>
import { computed, ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  Setting,
  Bell,
  Plus,
  Edit,
  Delete,
  RefreshRight,
  CopyDocument,
  Key,
  MagicStick,
  Connection,
  Document,
  ArrowLeft,
  SwitchButton,
} from '@element-plus/icons-vue'
import {
  adminGetAnnouncements,
  adminCreateAnnouncement,
  adminUpdateAnnouncement,
  adminDeleteAnnouncement,
  adminGetAPIConfig,
  adminUpdateAPIConfig,
  adminResetAIPrompt,
  adminGetAIModels,
  getErrorMessage,
} from '@/api'

const router = useRouter()
const announcements = ref([])
const activeTab = ref('api')
const loading = ref(false)
const error = ref('')
const showForm = ref(false)
const editingId = ref(null)
const form = ref({ title: '', content: '', important: false })
const formRef = ref(null)
const saving = ref(false)
const configLoading = ref(false)
const configSaving = ref(false)
const modelLoading = ref(false)
const modelOptions = ref([])
const configForm = ref({
  ai_base_url: '',
  ai_key: '',
  ai_model: '',
  ai_prompt: '',
  default_ai_prompt: '',
  ai_prompt_overridden: false,
  open_api_enabled: false,
  open_api_key: '',
})

const formRules = {
  title: [{ required: true, message: '请输入公告标题', trigger: 'blur' }],
  content: [{ required: true, message: '请输入公告内容', trigger: 'blur' }],
}

const pythonExample = computed(() => {
  const apiKey = configForm.value.open_api_key || 'your-open-api-key'
  return `import requests

BASE_URL = "https://your-domain.example.com"
API_KEY = "${apiKey}"

headers = {
    "X-API-Key": API_KEY,
    "Content-Type": "application/json",
}

# 直接查询空教室
query_payload = {
    "building": "老文史楼",
    "date_offset": 0,
    "start_node": "01",
    "end_node": "02",
}

query_resp = requests.post(
    f"{BASE_URL}/api/v1/open/query",
    json=query_payload,
    headers=headers,
    timeout=30,
)
query_resp.raise_for_status()
print(query_resp.json())

# 使用自然语言查询
ai_payload = {"text": "今天老文史楼第一二节有哪些空教室"}

ai_resp = requests.post(
    f"{BASE_URL}/api/v1/open/ai-query",
    json=ai_payload,
    headers=headers,
    timeout=30,
)
ai_resp.raise_for_status()
print(ai_resp.json())`
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
    ElMessage.success('配置已保存')
  } catch (e) {
    error.value = getErrorMessage(e, '保存开放接口配置失败')
  } finally {
    configSaving.value = false
  }
}

async function resetAIPrompt() {
  try {
    await ElMessageBox.confirm(
      '确定恢复系统内置的 AI 解析提示词吗？',
      '恢复默认提示词',
      { confirmButtonText: '确定恢复', cancelButtonText: '取消', type: 'warning' }
    )
  } catch {
    return
  }

  configSaving.value = true
  error.value = ''
  try {
    configForm.value = await adminResetAIPrompt()
    ElMessage.success('已恢复默认提示词')
  } catch (e) {
    error.value = getErrorMessage(e, '恢复默认提示词失败')
  } finally {
    configSaving.value = false
  }
}

function fillDefaultAIPrompt() {
  configForm.value.ai_prompt = configForm.value.default_ai_prompt || ''
}

async function fetchModels() {
  modelLoading.value = true
  error.value = ''
  try {
    const resp = await adminGetAIModels()
    modelOptions.value = (resp.models || []).map((model) => ({ label: model, value: model }))
    if (!configForm.value.ai_model && modelOptions.value.length > 0) {
      configForm.value.ai_model = modelOptions.value[0].value
    }
    ElMessage.success('模型列表已更新')
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

async function copyPythonExample() {
  try {
    await navigator.clipboard.writeText(pythonExample.value)
    ElMessage.success('Python 示例已复制')
  } catch {
    ElMessage.error('复制失败，请手动选择代码复制')
  }
}

async function copyApiKey() {
  if (!configForm.value.open_api_key) return
  try {
    await navigator.clipboard.writeText(configForm.value.open_api_key)
    ElMessage.success('授权 Key 已复制')
  } catch {
    ElMessage.error('复制失败')
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
  if (!formRef.value) return
  try {
    await formRef.value.validate()
  } catch {
    return
  }

  saving.value = true
  error.value = ''
  try {
    if (editingId.value) {
      await adminUpdateAnnouncement(editingId.value, form.value)
    } else {
      await adminCreateAnnouncement(form.value)
    }
    ElMessage.success('保存成功')
    cancelForm()
    await fetchList()
  } catch (e) {
    error.value = getErrorMessage(e, '保存失败')
  } finally {
    saving.value = false
  }
}

async function handleDelete(item) {
  try {
    await ElMessageBox.confirm(
      `确定要删除公告「${item.title}」吗？`,
      '确认删除',
      { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' }
    )
  } catch {
    return
  }

  error.value = ''
  try {
    await adminDeleteAnnouncement(item.id)
    ElMessage.success('删除成功')
    await fetchList()
  } catch (e) {
    error.value = getErrorMessage(e, '删除失败')
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
  <div class="admin-page">
    <header class="admin-topbar">
      <div class="admin-topbar-inner">
        <div class="topbar-left">
          <div class="topbar-logo">
            <el-icon :size="22"><Setting /></el-icon>
          </div>
          <div>
            <h1 class="admin-title">后台管理</h1>
            <p class="admin-subtitle">系统配置 · 公告维护</p>
          </div>
        </div>
        <div class="topbar-right">
          <el-button :icon="ArrowLeft" plain @click="router.push('/')">返回首页</el-button>
          <el-button :icon="SwitchButton" type="danger" plain @click="handleLogout">退出登录</el-button>
        </div>
      </div>
    </header>

    <main class="admin-main">
      <el-alert
        v-if="error"
        :title="error"
        type="error"
        show-icon
        closable
        class="admin-error"
        @close="error = ''"
      />

      <el-tabs v-model="activeTab" class="admin-tabs" type="border-card">
        <el-tab-pane name="api">
          <template #label>
            <span class="tab-label"><el-icon><Connection /></el-icon>接口配置</span>
          </template>

          <el-form
            label-position="top"
            class="config-form"
            v-loading="configLoading"
            @submit.prevent="saveConfig"
          >
            <div class="config-grid">
              <el-card class="config-card" shadow="never">
                <template #header>
                  <div class="card-head">
                    <div>
                      <h3>模型连接</h3>
                      <p class="card-desc">OpenAI 兼容接口地址、Key 与模型</p>
                    </div>
                    <el-button
                      type="primary"
                      plain
                      :icon="RefreshRight"
                      :loading="modelLoading"
                      size="small"
                      @click="fetchModels"
                    >
                      获取模型
                    </el-button>
                  </div>
                </template>

                <el-form-item label="BaseURL">
                  <el-input
                    v-model="configForm.ai_base_url"
                    placeholder="https://api.openai.com 或兼容地址"
                    clearable
                  />
                </el-form-item>
                <el-form-item label="API Key">
                  <el-input
                    v-model="configForm.ai_key"
                    type="password"
                    placeholder="请输入 API Key"
                    show-password
                    clearable
                  />
                </el-form-item>
                <el-form-item label="Model">
                  <el-select
                    v-if="modelOptions.length > 0"
                    v-model="configForm.ai_model"
                    placeholder="请选择模型"
                    filterable
                    allow-create
                    default-first-option
                    style="width: 100%"
                  >
                    <el-option
                      v-for="opt in modelOptions"
                      :key="opt.value"
                      :label="opt.label"
                      :value="opt.value"
                    />
                  </el-select>
                  <el-input
                    v-else
                    v-model="configForm.ai_model"
                    placeholder="例如 gpt-4o-mini"
                    clearable
                  />
                </el-form-item>
              </el-card>

              <el-card class="config-card prompt-card" shadow="never">
                <template #header>
                  <div class="card-head">
                    <div>
                      <h3>AI 解析提示词</h3>
                      <p class="card-desc">系统内置默认提示词，保存自定义内容后会覆盖默认值</p>
                    </div>
                    <el-tag
                      :type="configForm.ai_prompt_overridden ? 'warning' : 'success'"
                      effect="light"
                      round
                    >
                      {{ configForm.ai_prompt_overridden ? '自定义覆盖' : '系统默认' }}
                    </el-tag>
                  </div>
                </template>

                <el-form-item label="提示词内容">
                  <el-input
                    v-model="configForm.ai_prompt"
                    type="textarea"
                    :rows="12"
                    placeholder="请输入 AI 解析提示词"
                    resize="vertical"
                  />
                </el-form-item>

                <div class="prompt-actions">
                  <el-button :icon="MagicStick" plain @click="fillDefaultAIPrompt">填入默认</el-button>
                  <el-button
                    type="primary"
                    plain
                    :icon="RefreshRight"
                    :loading="configSaving"
                    @click="resetAIPrompt"
                  >
                    恢复默认
                  </el-button>
                </div>
              </el-card>

              <el-card class="config-card" shadow="never">
                <template #header>
                  <div class="card-head">
                    <div>
                      <h3>开放接口控制面板</h3>
                      <p class="card-desc">外部调用使用授权 Key，不受前台高频限制影响</p>
                    </div>
                    <el-switch
                      v-model="configForm.open_api_enabled"
                      active-text="启用"
                      inactive-text="停用"
                      inline-prompt
                    />
                  </div>
                </template>

                <el-form-item label="授权 Key">
                  <el-input
                    v-model="configForm.open_api_key"
                    placeholder="点击随机生成或手动输入"
                    clearable
                  >
                    <template #prepend>
                      <el-icon><Key /></el-icon>
                    </template>
                    <template #append>
                      <el-button :icon="CopyDocument" @click="copyApiKey" />
                      <el-button type="primary" @click="generateOpenAPIKey">随机生成</el-button>
                    </template>
                  </el-input>
                </el-form-item>

                <el-descriptions :column="1" border size="small" class="api-docs">
                  <el-descriptions-item label="直接查询">
                    <code>POST /api/v1/open/query</code>
                  </el-descriptions-item>
                  <el-descriptions-item label="AI 查询">
                    <code>POST /api/v1/open/ai-query</code>
                  </el-descriptions-item>
                  <el-descriptions-item label="请求头">
                    <code>X-API-Key: {{ configForm.open_api_key || 'your-key' }}</code>
                  </el-descriptions-item>
                </el-descriptions>
              </el-card>

              <el-card class="config-card doc-example" shadow="never">
                <template #header>
                  <div class="card-head">
                    <div>
                      <h3><el-icon><Document /></el-icon> Python 调用示例</h3>
                      <p class="card-desc">替换域名与参数后即可直接调用开放接口</p>
                    </div>
                    <el-button
                      type="primary"
                      plain
                      :icon="CopyDocument"
                      size="small"
                      @click="copyPythonExample"
                    >
                      复制代码
                    </el-button>
                  </div>
                </template>
                <pre class="code-block"><code>{{ pythonExample }}</code></pre>
              </el-card>
            </div>

            <div class="form-footer">
              <el-button
                type="primary"
                size="large"
                :loading="configSaving"
                @click="saveConfig"
              >
                保存配置
              </el-button>
            </div>
          </el-form>
        </el-tab-pane>

        <el-tab-pane name="announcements">
          <template #label>
            <span class="tab-label"><el-icon><Bell /></el-icon>公告管理</span>
          </template>

          <div class="section-toolbar">
            <div>
              <h3 class="section-title">公告列表</h3>
              <p class="section-desc">新增、编辑或删除展示在首页的系统公告</p>
            </div>
            <el-button type="primary" :icon="Plus" @click="openCreate">新增公告</el-button>
          </div>

          <el-table
            v-loading="loading"
            :data="announcements"
            stripe
            border
            empty-text="暂无公告"
            class="announcement-table"
          >
            <el-table-column prop="title" label="标题" min-width="180">
              <template #default="{ row }">
                <div class="title-cell">
                  <span>{{ row.title }}</span>
                  <el-tag v-if="row.important" type="warning" size="small" round>重要</el-tag>
                </div>
              </template>
            </el-table-column>
            <el-table-column prop="content" label="内容" min-width="320" show-overflow-tooltip />
            <el-table-column prop="created_at" label="创建时间" width="180" />
            <el-table-column label="操作" width="160" fixed="right" align="center">
              <template #default="{ row }">
                <el-button :icon="Edit" link type="primary" @click="openEdit(row)">编辑</el-button>
                <el-button :icon="Delete" link type="danger" @click="handleDelete(row)">删除</el-button>
              </template>
            </el-table-column>
          </el-table>
        </el-tab-pane>
      </el-tabs>
    </main>

    <el-dialog
      v-model="showForm"
      :title="editingId ? '编辑公告' : '新增公告'"
      width="520px"
      :close-on-click-modal="false"
      destroy-on-close
      @close="cancelForm"
    >
      <el-form
        ref="formRef"
        :model="form"
        :rules="formRules"
        label-position="top"
      >
        <el-form-item label="标题" prop="title">
          <el-input v-model="form.title" placeholder="请输入公告标题" maxlength="80" show-word-limit />
        </el-form-item>
        <el-form-item label="内容" prop="content">
          <el-input
            v-model="form.content"
            type="textarea"
            :rows="5"
            placeholder="请输入公告内容"
            maxlength="500"
            show-word-limit
            resize="vertical"
          />
        </el-form-item>
        <el-form-item label="标记为重要公告">
          <el-switch v-model="form.important" active-text="重要" inactive-text="普通" inline-prompt />
        </el-form-item>
      </el-form>

      <template #footer>
        <el-button @click="cancelForm">取消</el-button>
        <el-button type="primary" :loading="saving" @click="handleSave">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.admin-page {
  min-height: 100vh;
  background: var(--color-surface-page);
}

.admin-topbar {
  background: var(--color-surface-card);
  border-bottom: 1px solid var(--color-border-subtle);
  position: sticky;
  top: 0;
  z-index: 10;
}

.admin-topbar-inner {
  max-width: 1440px;
  margin: 0 auto;
  padding: 14px 24px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}

.topbar-left {
  display: flex;
  align-items: center;
  gap: 14px;
}

.topbar-logo {
  width: 40px;
  height: 40px;
  border-radius: 10px;
  background: var(--color-brand-100);
  color: var(--color-brand-500);
  display: flex;
  align-items: center;
  justify-content: center;
}

.admin-title {
  font-size: 18px;
  font-weight: 700;
  color: var(--color-text-primary);
  margin: 0;
  line-height: 1.2;
}

.admin-subtitle {
  font-size: 12px;
  color: var(--color-text-tertiary);
  margin: 2px 0 0;
}

.topbar-right {
  display: flex;
  align-items: center;
  gap: 8px;
}

.admin-main {
  max-width: 1440px;
  margin: 0 auto;
  padding: 20px 24px 40px;
}

.admin-error {
  margin-bottom: 16px;
}

.admin-tabs :deep(.el-tabs__item) {
  font-size: 14px;
  font-weight: 600;
}

.tab-label {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.config-form {
  padding: 4px 0 0;
}

.config-grid {
  display: grid;
  grid-template-columns: minmax(320px, 0.85fr) minmax(420px, 1.15fr);
  gap: 16px;
  align-items: stretch;
}

.config-card {
  border: 1px solid var(--color-border-subtle);
  border-radius: 12px;
  height: 100%;
}

.config-card :deep(.el-card__header) {
  padding: 14px 18px;
  background: var(--color-surface-section);
  border-bottom: 1px solid var(--color-border-subtle);
}

.config-card :deep(.el-card__body) {
  padding: 18px;
}

.card-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
}

.card-head h3 {
  margin: 0 0 4px;
  font-size: 15px;
  font-weight: 700;
  color: var(--color-text-primary);
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.card-desc {
  margin: 0;
  font-size: 12px;
  line-height: 1.5;
  color: var(--color-text-tertiary);
}

.prompt-card {
  grid-row: span 2;
}

.prompt-actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 4px;
}

.api-docs {
  margin-top: 8px;
}

.api-docs :deep(.el-descriptions__label) {
  width: 96px;
  background: var(--color-surface-section);
  color: var(--color-text-secondary);
  font-weight: 600;
}

.api-docs code {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-size: 12px;
  color: var(--color-text-primary);
  word-break: break-all;
}

.doc-example :deep(.el-card__header) {
  background: linear-gradient(135deg, rgba(136, 79, 34, 0.18), rgba(136, 79, 34, 0.04));
}

.doc-example :deep(.el-card__body) {
  padding: 0;
}

.code-block {
  margin: 0;
  padding: 16px 18px;
  overflow: auto;
  background: #15110E;
  color: #FDEBD3;
  font-size: 12px;
  line-height: 1.7;
  tab-size: 4;
  max-height: 380px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
}

.form-footer {
  display: flex;
  justify-content: flex-end;
  margin-top: 18px;
  padding-top: 16px;
  border-top: 1px solid var(--color-border-subtle);
}

.section-toolbar {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 16px;
}

.section-title {
  margin: 0 0 4px;
  font-size: 16px;
  font-weight: 700;
  color: var(--color-text-primary);
}

.section-desc {
  margin: 0;
  font-size: 12px;
  color: var(--color-text-tertiary);
}

.announcement-table {
  border-radius: 10px;
  overflow: hidden;
}

.title-cell {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

@media (max-width: 960px) {
  .config-grid {
    grid-template-columns: 1fr;
  }

  .prompt-card {
    grid-row: auto;
  }
}

@media (max-width: 640px) {
  .admin-topbar-inner,
  .admin-main {
    padding-left: 16px;
    padding-right: 16px;
  }

  .admin-topbar-inner {
    flex-direction: column;
    align-items: flex-start;
    gap: 12px;
  }

  .topbar-right {
    width: 100%;
    justify-content: flex-end;
  }

  .section-toolbar {
    flex-direction: column;
    align-items: stretch;
  }
}
</style>



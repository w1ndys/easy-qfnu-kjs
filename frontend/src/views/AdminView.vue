<script setup>
import { ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
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
  form.value = {
    title: item.title,
    content: item.content,
    important: item.important,
  }
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
    cancelForm()
    await fetchList()
  } catch (e) {
    error.value = getErrorMessage(e, '保存失败')
  } finally {
    saving.value = false
  }
}

async function handleDelete(id) {
  if (!confirm('确定要删除这条公告吗？')) return
  error.value = ''
  try {
    await adminDeleteAnnouncement(id)
    await fetchList()
  } catch (e) {
    error.value = getErrorMessage(e, '删除失败')
  }
}

function handleLogout() {
  localStorage.removeItem('admin_token')
  router.push('/admin/login')
}

onMounted(fetchList)
</script>

<template>
  <div class="page-shell min-h-screen antialiased">
    <div class="mx-auto max-w-4xl px-4 py-6 sm:px-6 lg:px-8">
      <!-- 顶部栏 -->
      <div class="mb-6 flex items-center justify-between">
        <div class="flex items-center space-x-3">
          <div class="flex h-10 w-10 items-center justify-center rounded-2xl bg-primary text-white">
            <svg class="h-5 w-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2"
                d="M11 5.882V19.24a1.76 1.76 0 01-3.417.592l-2.147-6.15M18 13a3 3 0 100-6M5.436 13.683A4.001 4.001 0 017 6h1.832c4.1 0 7.625-1.234 9.168-3v14c-1.543-1.766-5.067-3-9.168-3H7a3.988 3.988 0 01-1.564-.317z" />
            </svg>
          </div>
          <h1 class="text-xl font-bold text-clay-foreground">公告管理</h1>
        </div>
        <div class="flex items-center space-x-3">
          <router-link to="/" class="text-sm text-clay-muted hover:text-primary">返回首页</router-link>
          <button @click="handleLogout"
            class="rounded-xl border border-subtle bg-white px-3 py-1.5 text-sm font-semibold text-clay-muted transition hover:text-red-500">
            退出登录
          </button>
        </div>
      </div>

      <!-- 错误提示 -->
      <div v-if="error" class="mb-4 rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-600">
        {{ error }}
      </div>

      <!-- 新增按钮 -->
      <div class="mb-4">
        <button @click="openCreate"
          class="rounded-xl bg-primary px-4 py-2 text-sm font-semibold text-white transition hover:bg-primary/90">
          新增公告
        </button>
      </div>

      <!-- 编辑表单 -->
      <div v-if="showForm" class="clay-card mb-6 p-6">
        <h2 class="mb-4 text-base font-bold text-clay-foreground">
          {{ editingId ? '编辑公告' : '新增公告' }}
        </h2>
        <form @submit.prevent="handleSave" class="space-y-4">
          <div>
            <label class="mb-1 block text-sm font-semibold text-clay-foreground">标题</label>
            <input v-model="form.title" type="text"
              class="w-full rounded-xl border border-subtle bg-white px-4 py-2.5 text-sm text-clay-foreground outline-none transition focus:border-primary focus:ring-2 focus:ring-primary-200"
              placeholder="请输入公告标题" />
          </div>
          <div>
            <label class="mb-1 block text-sm font-semibold text-clay-foreground">内容</label>
            <textarea v-model="form.content" rows="4"
              class="w-full rounded-xl border border-subtle bg-white px-4 py-2.5 text-sm text-clay-foreground outline-none transition focus:border-primary focus:ring-2 focus:ring-primary-200"
              placeholder="请输入公告内容"></textarea>
          </div>
          <div class="flex items-center space-x-2">
            <input v-model="form.important" type="checkbox" id="important"
              class="h-4 w-4 rounded border-subtle text-primary focus:ring-primary-200" />
            <label for="important" class="text-sm text-clay-foreground">标记为重要公告</label>
          </div>
          <div class="flex space-x-3">
            <button type="submit" :disabled="saving"
              class="rounded-xl bg-primary px-4 py-2 text-sm font-semibold text-white transition hover:bg-primary/90 disabled:opacity-50">
              {{ saving ? '保存中...' : '保存' }}
            </button>
            <button type="button" @click="cancelForm"
              class="rounded-xl border border-subtle bg-white px-4 py-2 text-sm font-semibold text-clay-muted transition hover:bg-gray-50">
              取消
            </button>
          </div>
        </form>
      </div>

      <!-- 公告列表 -->
      <div v-if="loading" class="py-12 text-center text-sm text-clay-muted">加载中...</div>
      <div v-else-if="announcements.length === 0" class="py-12 text-center text-sm text-clay-muted">暂无公告</div>
      <div v-else class="space-y-3">
        <div v-for="item in announcements" :key="item.id"
          class="clay-card p-4 transition-all"
          :class="item.important ? 'border-[#F3CF8D] bg-[#FFF6E8]' : ''">
          <div class="flex items-start justify-between gap-3">
            <div class="flex-1 min-w-0">
              <div class="flex items-center gap-2">
                <h3 class="text-sm font-bold text-clay-foreground truncate">{{ item.title }}</h3>
                <span v-if="item.important"
                  class="flex-shrink-0 rounded-full bg-[#9A5A00] px-2 py-0.5 text-xs font-bold text-white">
                  重要
                </span>
              </div>
              <p class="mt-1 text-sm text-clay-muted line-clamp-2">{{ item.content }}</p>
              <p class="mt-1 text-xs text-clay-muted/60">{{ item.created_at }}</p>
            </div>
            <div class="flex flex-shrink-0 items-center space-x-2">
              <button @click="openEdit(item)"
                class="rounded-lg border border-subtle bg-white px-2.5 py-1 text-xs font-semibold text-primary transition hover:bg-primary-50">
                编辑
              </button>
              <button @click="handleDelete(item.id)"
                class="rounded-lg border border-red-200 bg-white px-2.5 py-1 text-xs font-semibold text-red-500 transition hover:bg-red-50">
                删除
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

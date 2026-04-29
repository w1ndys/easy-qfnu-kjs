<script setup>
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { adminLogin, getErrorMessage } from '@/api'

const router = useRouter()
const username = ref('')
const password = ref('')
const error = ref('')
const loading = ref(false)

async function handleLogin() {
  error.value = ''
  if (!username.value || !password.value) {
    error.value = '请输入用户名和密码'
    return
  }

  loading.value = true
  try {
    const resp = await adminLogin(username.value, password.value)
    localStorage.setItem('admin_token', resp.token)
    router.push('/admin')
  } catch (e) {
    error.value = getErrorMessage(e, '登录失败，请检查用户名和密码')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="page-shell flex min-h-screen items-center justify-center px-4 antialiased">
    <div class="w-full max-w-sm">
      <div class="clay-card p-6 sm:p-8">
        <div class="mb-6 text-center">
          <div class="mx-auto mb-3 flex h-12 w-12 items-center justify-center rounded-2xl bg-primary text-white">
            <svg class="h-6 w-6" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2"
                d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z" />
            </svg>
          </div>
          <h1 class="text-xl font-bold text-clay-foreground">管理后台</h1>
          <p class="mt-1 text-sm text-clay-muted">请登录以管理系统公告</p>
        </div>

        <form @submit.prevent="handleLogin" class="space-y-4">
          <div>
            <label class="mb-1 block text-sm font-semibold text-clay-foreground">用户名</label>
            <input
              v-model="username"
              type="text"
              autocomplete="username"
              class="w-full rounded-xl border border-subtle bg-white px-4 py-2.5 text-sm text-clay-foreground outline-none transition focus:border-primary focus:ring-2 focus:ring-primary-200"
              placeholder="请输入用户名"
            />
          </div>
          <div>
            <label class="mb-1 block text-sm font-semibold text-clay-foreground">密码</label>
            <input
              v-model="password"
              type="password"
              autocomplete="current-password"
              class="w-full rounded-xl border border-subtle bg-white px-4 py-2.5 text-sm text-clay-foreground outline-none transition focus:border-primary focus:ring-2 focus:ring-primary-200"
              placeholder="请输入密码"
            />
          </div>

          <div v-if="error" class="rounded-xl border border-red-200 bg-red-50 px-4 py-2.5 text-sm text-red-600">
            {{ error }}
          </div>

          <button
            type="submit"
            :disabled="loading"
            class="w-full rounded-xl bg-primary px-4 py-2.5 text-sm font-semibold text-white transition hover:bg-primary/90 disabled:opacity-50"
          >
            {{ loading ? '登录中...' : '登录' }}
          </button>
        </form>

        <div class="mt-4 text-center">
          <router-link to="/" class="text-sm text-clay-muted hover:text-primary">
            返回首页
          </router-link>
        </div>
      </div>
    </div>
  </div>
</template>

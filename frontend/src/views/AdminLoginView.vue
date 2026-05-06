<script setup>
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { adminLogin, getErrorMessage } from '@/api'
import { showToast } from 'vant'

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
    showToast({ message: '登录成功', type: 'success' })
    router.push('/admin')
  } catch (e) {
    error.value = getErrorMessage(e, '登录失败，请检查用户名和密码')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="login-page">
    <div class="login-wrapper">
      <div class="app-card login-card">
        <div class="login-header">
          <van-icon name="shield-o" size="36" color="var(--color-brand-500)" />
          <h1 class="login-title">管理后台</h1>
          <p class="login-subtitle">请登录以管理系统公告</p>
        </div>

        <van-form @submit="handleLogin">
          <van-cell-group inset>
            <van-field
              v-model="username"
              name="username"
              label="用户名"
              placeholder="请输入用户名"
              autocomplete="username"
              :rules="[{ required: true, message: '请输入用户名' }]"
            />
            <van-field
              v-model="password"
              type="password"
              name="password"
              label="密码"
              placeholder="请输入密码"
              autocomplete="current-password"
              :rules="[{ required: true, message: '请输入密码' }]"
            />
          </van-cell-group>

          <div v-if="error" class="error-msg">
            <van-notice-bar left-icon="warning-o" :text="error" color="var(--color-error-fg)" background="var(--color-error-bg)" :scrollable="false" />
          </div>

          <div class="login-actions">
            <van-button
              type="primary"
              block
              round
              size="large"
              :loading="loading"
              loading-text="登录中..."
              native-type="submit"
            >
              登录
            </van-button>
          </div>
        </van-form>

        <div class="login-footer">
          <router-link to="/" class="back-link">返回首页</router-link>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.login-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 16px;
  background: var(--color-surface-page);
}

.login-wrapper {
  width: 100%;
  max-width: 400px;
}

.login-card {
  padding: 32px 24px;
}

.login-header {
  text-align: center;
  margin-bottom: 24px;
}

.login-title {
  font-size: 20px;
  font-weight: 700;
  color: var(--color-text-primary);
  margin: 12px 0 4px;
}

.login-subtitle {
  font-size: 14px;
  color: var(--color-text-tertiary);
  margin: 0;
}

.error-msg {
  margin: 12px 16px 0;
}

.error-msg :deep(.van-notice-bar) {
  border-radius: 8px;
}

.login-actions {
  padding: 20px 16px 0;
}

.login-footer {
  text-align: center;
  margin-top: 16px;
}

.back-link {
  font-size: 14px;
  color: var(--color-text-tertiary);
  text-decoration: none;
}

.back-link:active {
  color: var(--color-brand-500);
}

:deep(.van-cell-group--inset) {
  margin: 0;
}
</style>

<script setup>
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { Lock, User } from '@element-plus/icons-vue'
import { adminLogin, getErrorMessage } from '@/api'

const router = useRouter()
const formRef = ref(null)
const formData = ref({ username: '', password: '' })
const error = ref('')
const loading = ref(false)

const rules = {
  username: [{ required: true, message: '请输入用户名', trigger: 'blur' }],
  password: [{ required: true, message: '请输入密码', trigger: 'blur' }],
}

async function handleLogin() {
  if (!formRef.value) return
  try {
    await formRef.value.validate()
  } catch {
    return
  }

  error.value = ''
  loading.value = true
  try {
    const resp = await adminLogin(formData.value.username, formData.value.password)
    localStorage.setItem('admin_token', resp.token)
    ElMessage.success('登录成功')
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
      <el-card class="login-card" shadow="hover">
        <div class="login-header">
          <div class="login-logo">
            <el-icon :size="32" color="#884F22"><Lock /></el-icon>
          </div>
          <h1 class="login-title">管理后台</h1>
          <p class="login-subtitle">请登录以管理系统配置与公告</p>
        </div>

        <el-form
          ref="formRef"
          :model="formData"
          :rules="rules"
          label-position="top"
          size="large"
          @submit.prevent="handleLogin"
        >
          <el-form-item label="用户名" prop="username">
            <el-input
              v-model="formData.username"
              placeholder="请输入用户名"
              autocomplete="username"
              :prefix-icon="User"
              clearable
            />
          </el-form-item>

          <el-form-item label="密码" prop="password">
            <el-input
              v-model="formData.password"
              type="password"
              placeholder="请输入密码"
              autocomplete="current-password"
              :prefix-icon="Lock"
              show-password
              @keyup.enter="handleLogin"
            />
          </el-form-item>

          <el-alert
            v-if="error"
            :title="error"
            type="error"
            show-icon
            :closable="false"
            class="login-error"
          />

          <el-button
            type="primary"
            :loading="loading"
            class="login-submit"
            size="large"
            @click="handleLogin"
          >
            {{ loading ? '登录中...' : '登录' }}
          </el-button>
        </el-form>

        <div class="login-footer">
          <router-link to="/" class="back-link">返回首页</router-link>
        </div>
      </el-card>
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
  background: linear-gradient(135deg, #F8F5F2 0%, #F0E5D8 100%);
}

.login-wrapper {
  width: 100%;
  max-width: 420px;
}

.login-card {
  border-radius: 16px;
  border: 1px solid var(--color-border-subtle);
}

.login-card :deep(.el-card__body) {
  padding: 36px 28px 28px;
}

.login-header {
  text-align: center;
  margin-bottom: 28px;
}

.login-logo {
  width: 56px;
  height: 56px;
  border-radius: 16px;
  background: var(--color-brand-100);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  margin-bottom: 14px;
}

.login-title {
  font-size: 22px;
  font-weight: 700;
  color: var(--color-text-primary);
  margin: 0 0 6px;
}

.login-subtitle {
  font-size: 13px;
  color: var(--color-text-tertiary);
  margin: 0;
}

.login-error {
  margin-bottom: 16px;
}

.login-submit {
  width: 100%;
}

.login-footer {
  text-align: center;
  margin-top: 18px;
}

.back-link {
  font-size: 13px;
  color: var(--color-text-tertiary);
  text-decoration: none;
}

.back-link:hover {
  color: var(--color-brand-500);
}
</style>

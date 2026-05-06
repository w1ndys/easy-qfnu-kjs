import axios from 'axios'

const api = axios.create({
  baseURL: '',
  timeout: 30000,
})

api.interceptors.response.use(
  (response) => response,
  (error) => Promise.reject(error),
)

export async function getStatus() {
  const { data } = await api.get('/api/v1/status')
  return data
}

export async function queryClassrooms(params) {
  const { data } = await api.post('/api/v1/query', params)
  return data
}

export async function queryClassroomsByAI(text) {
  const { data } = await api.post('/api/v1/ai-query', { text })
  return data
}

export async function queryFullDayStatus(params) {
  const { data } = await api.post('/api/v1/query-full-day', params)
  return data
}

export async function getStats() {
  const { data } = await api.get('/api/v1/stats')
  return data
}

export async function getTopBuildings() {
  const { data } = await api.get('/api/v1/top-buildings')
  return data
}

export async function getDashboard(range = 'today', days) {
  const params = { range }
  if (range === 'custom') params.days = days
  // 传递客户端时区偏移（分钟），getTimezoneOffset 返回的是 UTC - local，需要取反
  params.tz_offset = -new Date().getTimezoneOffset()
  const { data } = await api.get('/api/v1/dashboard', { params })
  return data
}

// ---- 前台公告接口 ----

export async function getAnnouncements() {
  const { data } = await api.get('/api/v1/announcements')
  return data
}

// ---- 管理后台接口 ----

const adminApi = axios.create({
  baseURL: '',
  timeout: 30000,
})

// 管理后台请求拦截器：自动附加 JWT token
adminApi.interceptors.request.use((config) => {
  const token = localStorage.getItem('admin_token')
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

adminApi.interceptors.response.use(
  (response) => response,
  (error) => {
    // 401 时清除 token 并跳转登录
    if (error.response?.status === 401) {
      localStorage.removeItem('admin_token')
      if (window.location.pathname.startsWith('/admin') && !window.location.pathname.includes('/login')) {
        window.location.href = '/admin/login'
      }
    }
    return Promise.reject(error)
  },
)

// 登录接口使用普通 api 实例（无需 token），与 adminApi（需 token）区分
export async function adminLogin(username, password) {
  const { data } = await api.post('/api/v1/admin/login', { username, password })
  return data
}

export async function adminGetAnnouncements() {
  const { data } = await adminApi.get('/api/v1/admin/announcements')
  return data
}

export async function adminCreateAnnouncement(payload) {
  const { data } = await adminApi.post('/api/v1/admin/announcements', payload)
  return data
}

export async function adminUpdateAnnouncement(id, payload) {
  const { data } = await adminApi.put(`/api/v1/admin/announcements/${id}`, payload)
  return data
}

export async function adminDeleteAnnouncement(id) {
  const { data } = await adminApi.delete(`/api/v1/admin/announcements/${id}`)
  return data
}

export async function adminGetAPIConfig() {
  const { data } = await adminApi.get('/api/v1/admin/api-config')
  return data
}

export async function adminUpdateAPIConfig(payload) {
  const { data } = await adminApi.put('/api/v1/admin/api-config', payload)
  return data
}

export async function adminGetAIModels() {
  const { data } = await adminApi.get('/api/v1/admin/ai-models')
  return data
}

export function getErrorMessage(error, fallback = '请求失败，请稍后重试') {
  return error?.response?.data?.error || fallback
}

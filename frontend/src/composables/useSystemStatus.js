import { onMounted, ref } from 'vue'
import { getStatus } from '@/api'

export function useSystemStatus(autoCheck = true) {
  const statusLoading = ref(true)
  const inTeachingCalendar = ref(true)
  const hasPermission = ref(true)
  const currentWeek = ref(0)
  const currentTerm = ref('')
  const upstreamHealthy = ref(true)
  const upstreamMessage = ref('')

  async function checkStatus() {
    statusLoading.value = true
    try {
      const data = await getStatus()
      inTeachingCalendar.value = !!data.in_teaching_calendar
      currentWeek.value = data.current_week || 0
      currentTerm.value = data.current_term || ''
      hasPermission.value = data.has_permission !== false
      const upstream = data.upstream || {}
      upstreamHealthy.value = upstream.healthy !== false
      upstreamMessage.value = upstream.message || ''
    } catch (error) {
      console.error('Failed to check status:', error)
      inTeachingCalendar.value = false
      hasPermission.value = true
      upstreamHealthy.value = false
      upstreamMessage.value = '无法连接服务，请稍后重试'
    } finally {
      statusLoading.value = false
    }
  }

  if (autoCheck) {
    onMounted(checkStatus)
  }

  return {
    statusLoading,
    inTeachingCalendar,
    hasPermission,
    currentWeek,
    currentTerm,
    upstreamHealthy,
    upstreamMessage,
    checkStatus,
  }
}

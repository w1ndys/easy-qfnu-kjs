import { ref, computed } from 'vue'
import { getAnnouncements } from '@/api'

const STORAGE_KEY = 'read_announcements'

/**
 * 从 localStorage 读取已读公告 id 列表
 */
function loadReadIds() {
  try {
    const stored = localStorage.getItem(STORAGE_KEY)
    if (stored) {
      return new Set(JSON.parse(stored))
    }
  } catch {
    // localStorage 不可用时静默失败
  }
  return new Set()
}

/**
 * 将已读公告 id 列表写入 localStorage
 */
function saveReadIds(readIds) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify([...readIds]))
  } catch {
    // localStorage 不可用时静默失败
  }
}

export function useAnnouncements() {
  const readIds = ref(loadReadIds())
  const announcements = ref([])
  const loading = ref(false)

  /** 从后端 API 加载公告 */
  async function fetchAnnouncements() {
    loading.value = true
    try {
      const resp = await getAnnouncements()
      announcements.value = resp.announcements || []
    } catch {
      // API 不可用时静默失败，保持空列表
      announcements.value = []
    } finally {
      loading.value = false
    }
  }

  /** 所有公告列表 */
  const allAnnouncements = computed(() => announcements.value)

  /** 未读公告数量 */
  const unreadCount = computed(
    () => announcements.value.filter((a) => !readIds.value.has(a.id)).length,
  )

  /** 是否有未读公告 */
  const hasUnread = computed(() => unreadCount.value > 0)

  /** 判断单条公告是否已读 */
  function isRead(id) {
    return readIds.value.has(id)
  }

  /** 标记所有公告为已读 */
  function markAllAsRead() {
    const ids = new Set(readIds.value)
    announcements.value.forEach((a) => ids.add(a.id))
    readIds.value = ids
    saveReadIds(ids)
  }

  /** 清理缓存中已不存在的旧公告 id，保持 localStorage 干净 */
  function cleanupStaleIds() {
    const currentIds = new Set(announcements.value.map((a) => a.id))
    const cleaned = new Set([...readIds.value].filter((id) => currentIds.has(id)))
    if (cleaned.size !== readIds.value.size) {
      readIds.value = cleaned
      saveReadIds(cleaned)
    }
  }

  // 暴露 fetchAnnouncements 让调用方在组件 onMounted 中自行调用
  return {
    allAnnouncements,
    unreadCount,
    hasUnread,
    isRead,
    markAllAsRead,
    loading,
    fetchAnnouncements,
  }
}

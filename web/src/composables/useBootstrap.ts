import { onMounted, shallowRef, type ShallowRef } from 'vue'
import { fetchContext, fetchManifest } from '@/api/endpoints'
import { ApiError, toApiError } from '@/api/client'
import type { ContextInfo, Manifest } from '@/api/types'

export interface Bootstrap {
  manifest: ShallowRef<Manifest | null>
  context: ShallowRef<ContextInfo | null>
  loading: ShallowRef<boolean>
  error: ShallowRef<ApiError | null>
  /** 并发加载 manifest + context；成功后返回 true。可重复调用（重试）。 */
  load: () => Promise<boolean>
}

/** 页面级数据引导：manifest（分组/周次/节次/状态字典）+ context（当前周/星期）。 */
export function useBootstrap(auto = true): Bootstrap {
  const manifest = shallowRef<Manifest | null>(null)
  const context = shallowRef<ContextInfo | null>(null)
  const loading = shallowRef(false)
  const error = shallowRef<ApiError | null>(null)

  async function load(): Promise<boolean> {
    loading.value = true
    error.value = null
    try {
      const [manifestData, contextData] = await Promise.all([fetchManifest(), fetchContext()])
      manifest.value = manifestData
      context.value = contextData
      return true
    } catch (err) {
      manifest.value = null
      context.value = null
      error.value = toApiError(err)
      return false
    } finally {
      loading.value = false
    }
  }

  if (auto) onMounted(() => void load())
  return { manifest, context, loading, error, load }
}

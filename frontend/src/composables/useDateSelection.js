import { computed, ref } from 'vue'
import { buildDatePreview, clamp } from '@/utils/date'

const MIN_OFFSET = 0
const MAX_OFFSET = 180

export function useDateSelection(initialOffset = 0) {
  const quickDateLabels = ['今天', '明天', '后天']
  const useCustomDate = ref(false)
  const customOffset = ref(3)
  const dateOffset = ref(0)

  function setOffset(nextOffset) {
    const safeOffset = clamp(Number(nextOffset) || 0, MIN_OFFSET, MAX_OFFSET)
    if (safeOffset <= 2) {
      useCustomDate.value = false
      dateOffset.value = safeOffset
      return
    }

    useCustomDate.value = true
    customOffset.value = safeOffset
    dateOffset.value = safeOffset
  }

  function setQuickDate(offset) {
    useCustomDate.value = false
    dateOffset.value = clamp(offset, MIN_OFFSET, 2)
  }

  function toggleCustomDate() {
    useCustomDate.value = !useCustomDate.value
    if (useCustomDate.value) {
      dateOffset.value = customOffset.value
      return
    }
    if (dateOffset.value > 2) {
      dateOffset.value = 0
    }
  }

  function updateCustomOffset(value = customOffset.value) {
    const numValue = Number(value)
    if (isNaN(numValue)) {
      customOffset.value = MIN_OFFSET
      dateOffset.value = MIN_OFFSET
      return
    }
    customOffset.value = clamp(numValue, MIN_OFFSET, MAX_OFFSET)
    dateOffset.value = customOffset.value
  }

  const customDatePreview = computed(() => {
    if (!useCustomDate.value) {
      return ''
    }
    return buildDatePreview(customOffset.value)
  })

  setOffset(initialOffset)

  return {
    quickDateLabels,
    useCustomDate,
    customOffset,
    dateOffset,
    customDatePreview,
    setOffset,
    setQuickDate,
    toggleCustomDate,
    updateCustomOffset,
  }
}

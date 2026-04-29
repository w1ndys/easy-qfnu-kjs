<script setup>
import { ref, watch, onMounted } from 'vue'
import { useAnnouncements } from '@/composables/useAnnouncements'

const { allAnnouncements, unreadCount, hasUnread, isRead, markAllAsRead, fetchAnnouncements } =
  useAnnouncements()

onMounted(() => fetchAnnouncements())

// 有未读公告时默认展开，全部已读时默认折叠
const expanded = ref(hasUnread.value)

function handleToggle() {
  if (expanded.value && hasUnread.value) {
    markAllAsRead()
  }
  expanded.value = !expanded.value
}

// 当所有公告变为已读后，自动折叠
watch(hasUnread, (val) => {
  if (!val) {
    expanded.value = false
  }
})
</script>

<template>
  <!-- 无公告时不渲染 -->
  <div v-if="allAnnouncements.length > 0">
    <!-- 折叠状态：仅显示摘要条 -->
    <div
      v-if="!expanded"
      class="clay-card cursor-pointer transition-all duration-300"
      :class="hasUnread ? 'p-4 sm:p-5' : 'p-3 sm:p-4'"
      @click="handleToggle"
    >
      <div class="relative z-10 flex items-center justify-between">
        <div class="flex items-center space-x-3">
          <!-- 公告图标 -->
          <div :class="['flex h-8 w-8 flex-shrink-0 items-center justify-center rounded-xl text-white', hasUnread ? 'bg-[#9A5A00]' : 'bg-primary']">
            <svg
              class="w-4 h-4 text-white"
              fill="none"
              stroke="currentColor"
              viewBox="0 0 24 24"
            >
              <path
                stroke-linecap="round"
                stroke-linejoin="round"
                stroke-width="2"
                d="M11 5.882V19.24a1.76 1.76 0 01-3.417.592l-2.147-6.15M18 13a3 3 0 100-6M5.436 13.683A4.001 4.001 0 017 6h1.832c4.1 0 7.625-1.234 9.168-3v14c-1.543-1.766-5.067-3-9.168-3H7a3.988 3.988 0 01-1.564-.317z"
              />
            </svg>
          </div>
          <span class="text-sm font-bold text-clay-foreground">
            {{ hasUnread ? `${unreadCount} 条新公告` : '系统公告' }}
          </span>
        </div>
        <!-- 展开箭头 -->
        <div class="flex h-6 w-6 flex-shrink-0 items-center justify-center rounded-full border border-subtle bg-white">
          <svg
            class="w-3 h-3 text-clay-muted"
            fill="none"
            stroke="currentColor"
            viewBox="0 0 24 24"
          >
            <path
              stroke-linecap="round"
              stroke-linejoin="round"
              stroke-width="2.5"
              d="M19 9l-7 7-7-7"
            />
          </svg>
        </div>
      </div>
    </div>

    <!-- 展开状态：完整公告列表 -->
    <div v-else class="clay-card p-6 sm:p-8">
      <div class="relative z-10">
        <!-- 标题栏 -->
        <div class="flex items-center justify-between mb-5">
          <div class="flex items-center space-x-3">
            <div :class="['flex h-10 w-10 flex-shrink-0 items-center justify-center rounded-2xl text-white', hasUnread ? 'bg-[#9A5A00]' : 'bg-primary']">
              <svg
                class="w-5 h-5 text-white"
                fill="none"
                stroke="currentColor"
                viewBox="0 0 24 24"
              >
                <path
                  stroke-linecap="round"
                  stroke-linejoin="round"
                  stroke-width="2"
                  d="M11 5.882V19.24a1.76 1.76 0 01-3.417.592l-2.147-6.15M18 13a3 3 0 100-6M5.436 13.683A4.001 4.001 0 017 6h1.832c4.1 0 7.625-1.234 9.168-3v14c-1.543-1.766-5.067-3-9.168-3H7a3.988 3.988 0 01-1.564-.317z"
                />
              </svg>
            </div>
            <h3
              class="text-base font-bold text-clay-foreground"
            >
              系统公告
              <span
                v-if="hasUnread"
                class="ml-2 inline-flex items-center justify-center rounded-full bg-[#9A5A00] px-2 py-0.5 text-xs font-bold text-white"
              >
                {{ unreadCount }} 条未读
              </span>
            </h3>
          </div>
          <!-- 收起按钮 -->
          <button
            class="flex h-8 w-8 flex-shrink-0 items-center justify-center rounded-full border border-subtle bg-white transition hover:bg-primary-50"
            title="收起公告"
            @click="handleToggle"
          >
            <svg
              class="w-4 h-4 text-clay-muted"
              fill="none"
              stroke="currentColor"
              viewBox="0 0 24 24"
            >
              <path
                stroke-linecap="round"
                stroke-linejoin="round"
                stroke-width="2.5"
                d="M5 15l7-7 7 7"
              />
            </svg>
          </button>
        </div>

        <!-- 公告列表 -->
        <div class="space-y-3">
          <div
            v-for="item in allAnnouncements"
            :key="item.id"
            class="rounded-2xl p-4 transition-all duration-300"
            :class="item.important ? 'border border-[#F3CF8D] bg-[#FFF6E8]' : 'border border-subtle bg-[#FAF8F6]'"
          >
            <div class="flex items-start justify-between gap-2">
              <h4
                class="text-sm font-bold"
                :class="
                  isRead(item.id)
                    ? 'text-clay-muted'
                    : 'text-clay-foreground'
                "
              >
                {{ item.title }}
              </h4>
              <span
                class="text-xs text-clay-muted flex-shrink-0 font-medium mt-0.5"
              >
                {{ item.date }}
              </span>
            </div>
            <p
              class="text-sm mt-2 leading-relaxed font-medium"
              :class="
                isRead(item.id) ? 'text-clay-muted/70' : 'text-clay-muted'
              "
            >
              {{ item.content }}
            </p>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

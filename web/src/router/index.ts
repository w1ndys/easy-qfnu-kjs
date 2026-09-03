import { createRouter, createWebHistory } from 'vue-router'

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  scrollBehavior: () => ({ top: 0 }),
  routes: [
    {
      path: '/',
      name: 'home',
      component: () => import('@/views/HomeView.vue'),
      meta: { title: '教室查询' },
    },
    {
      path: '/empty-classroom',
      name: 'empty-classroom',
      component: () => import('@/views/EmptyClassroomView.vue'),
      meta: { title: '空教室查询' },
    },
    {
      path: '/full-day-status',
      name: 'full-day-status',
      component: () => import('@/views/FullDayStatusView.vue'),
      meta: { title: '教室全天状态' },
    },
    {
      path: '/:pathMatch(.*)*',
      name: 'not-found',
      component: () => import('@/views/NotFoundView.vue'),
      meta: { title: '页面不存在' },
    },
  ],
})

router.afterEach((to) => {
  const title = typeof to.meta.title === 'string' ? to.meta.title : ''
  document.title = title ? `${title} · 曲阜师范大学教室查询` : '曲阜师范大学 · 教室查询'
})

export default router

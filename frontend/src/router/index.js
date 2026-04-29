import { createRouter, createWebHistory } from 'vue-router'

const HomeView = () => import('@/views/HomeView.vue')
const EmptyClassroomView = () => import('@/views/EmptyClassroomView.vue')
const FullDayStatusView = () => import('@/views/FullDayStatusView.vue')
const DashboardView = () => import('@/views/DashboardView.vue')
const AdminLoginView = () => import('@/views/AdminLoginView.vue')
const AdminView = () => import('@/views/AdminView.vue')

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'home', component: HomeView },
    { path: '/empty-classroom', name: 'empty-classroom', component: EmptyClassroomView },
    { path: '/full-day-status', name: 'full-day-status', component: FullDayStatusView },
    { path: '/dashboard', name: 'dashboard', component: DashboardView },
    { path: '/admin/login', name: 'admin-login', component: AdminLoginView },
    {
      path: '/admin',
      name: 'admin',
      component: AdminView,
      meta: { requiresAuth: true },
    },
  ],
})

// 路由守卫：管理后台页面需要登录
router.beforeEach((to) => {
  if (to.meta.requiresAuth) {
    const token = localStorage.getItem('admin_token')
    if (!token) {
      return { name: 'admin-login' }
    }
  }
})

export default router

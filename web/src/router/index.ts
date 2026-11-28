import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { setUnauthorizedHandler } from '@/api/client'
import LoginView from '@/views/LoginView.vue'

const router = createRouter({
  history: createWebHistory('/nfr/'),
  routes: [
    {
      path: '/login',
      name: 'login',
      component: LoginView,
      meta: { public: true, title: '登录' },
    },
    {
      path: '/',
      component: () => import('@/layouts/AppLayout.vue'),
      children: [
        { path: '', name: 'dashboard', component: () => import('@/views/DashboardView.vue'), meta: { title: '仪表盘' } },
        { path: 'create', name: 'create', component: () => import('@/views/CreateTunnelView.vue'), meta: { title: '创建隧道' } },
        { path: 'tunnels', name: 'tunnels', component: () => import('@/views/TunnelsView.vue'), meta: { title: '隧道管理' } },
        { path: 'profile', name: 'profile', component: () => import('@/views/ProfileView.vue'), meta: { title: '用户资料' } },
        {
          path: 'settings',
          name: 'settings',
          component: () => import('@/views/SettingsView.vue'),
          meta: { title: '系统设置', requiresAdmin: true },
        },
        {
          path: 'users/:id',
          name: 'user-detail',
          component: () => import('@/views/UserDetailView.vue'),
          meta: { title: '用户详情', requiresAdmin: true },
        },
      ],
    },
    {
      path: '/:pathMatch(.*)*',
      redirect: '/',
    },
  ],
})

setUnauthorizedHandler(() => {
  const auth = useAuthStore()
  auth.clearSession()
  if (router.currentRoute.value.name !== 'login') {
    void router.push({ name: 'login' })
  }
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  const ok = await auth.ensureChecked()
  if (to.meta.public) {
    if (ok && to.name === 'login') {
      return { name: 'dashboard' }
    }
    return true
  }
  if (!ok) {
    return { name: 'login' }
  }
  if (to.meta.requiresAdmin && !auth.isAdmin) {
    return { name: 'dashboard' }
  }
  return true
})

router.onError((err) => {
  const msg = err instanceof Error ? err.message : String(err)
  if (/dynamically imported module|Loading chunk|Failed to fetch/i.test(msg)) {
    window.location.reload()
  }
})

export default router

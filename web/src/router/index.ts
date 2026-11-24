import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'

const routes: RouteRecordRaw[] = [
  {
    path: '/',
    name: 'home',
    component: () => import('@/views/HomeView.vue'),
    meta: { title: '同绘 CoDraw · 打开链接，一起把想法画出来' },
  },
  {
    path: '/r/:code',
    name: 'room',
    component: () => import('@/views/RoomView.vue'),
    props: true,
  },
  {
    path: '/perf',
    name: 'perf',
    component: () => import('@/views/PerfView.vue'),
    meta: { title: '画布性能自测 · 同绘 CoDraw' },
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFoundView.vue'),
    meta: { title: '页面不存在 · 同绘 CoDraw' },
  },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior: () => ({ top: 0 }),
})

const BASE_TITLE = '同绘 CoDraw'

router.afterEach((to) => {
  const title = typeof to.meta.title === 'string' ? to.meta.title : ''
  document.title = title || BASE_TITLE
})

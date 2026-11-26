<script setup lang="ts">
import { computed, h, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NIcon,
  NLayout,
  NLayoutContent,
  NLayoutHeader,
  NLayoutSider,
  NMenu,
  useMessage,
  type MenuOption,
} from 'naive-ui'
import {
  AddCircleOutline,
  LogOutOutline,
  MoonOutline,
  SettingsOutline,
  SpeedometerOutline,
  SunnyOutline,
  SwapHorizontalOutline,
} from '@vicons/ionicons5'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { useThemeStore } from '@/stores/theme'
import { useUpdateStore } from '@/stores/update'
import UpdateModal from '@/components/UpdateModal.vue'

const route = useRoute()
const router = useRouter()
const message = useMessage()
const authStore = useAuthStore()
const dashStore = useDashboardStore()
const themeStore = useThemeStore()
const updateStore = useUpdateStore()

const collapsed = ref(false)
const winWidth = ref(typeof window !== 'undefined' ? window.innerWidth : 1280)
const isDesktop = computed(() => winWidth.value >= 1024)

function onResize() {
  winWidth.value = window.innerWidth
}

onMounted(() => {
  window.addEventListener('resize', onResize, { passive: true })
  dashStore.startRealtime()
  void updateStore.fetchStatus(true)
})

onBeforeUnmount(() => {
  window.removeEventListener('resize', onResize)
  dashStore.stopRealtime()
})

function renderIcon(comp: unknown) {
  return () => h(NIcon, null, { default: () => h(comp as never) })
}

const menuOptions = computed<MenuOption[]>(() => [
  { label: '仪表盘', key: 'dashboard', icon: renderIcon(SpeedometerOutline) },
  { label: '创建隧道', key: 'create', icon: renderIcon(AddCircleOutline) },
  { label: '隧道管理', key: 'tunnels', icon: renderIcon(SwapHorizontalOutline) },
  {
    label: () =>
      h('span', { class: 'menu-label-wrap' }, [
        h('span', null, '系统设置'),
        updateStore.hasUpdate ? h('span', { class: 'nav-update-dot', title: '发现新版本' }) : null,
      ]),
    key: 'settings',
    icon: renderIcon(SettingsOutline),
  },
])

const bottomNavItems = [
  { key: 'dashboard', label: '仪表盘', icon: SpeedometerOutline },
  { key: 'create', label: '创建隧道', icon: AddCircleOutline },
  { key: 'tunnels', label: '隧道管理', icon: SwapHorizontalOutline },
  { key: 'settings', label: '系统设置', icon: SettingsOutline },
]

const activeKey = computed(() => String(route.name || 'dashboard'))
const pageTitle = computed(() => String(route.meta.title || '仪表盘'))

function handleSelectNav(key: string) {
  if (route.name !== key) {
    void router.push({ name: key })
  }
}

async function handleLogout() {
  dashStore.stopRealtime()
  await authStore.logout()
  message.success('已退出登录')
  await router.replace({ name: 'login' })
}
</script>

<template>
  <NLayout :has-sider="isDesktop" class="app-shell">
    <NLayoutSider
      v-if="isDesktop"
      bordered
      collapse-mode="width"
      :collapsed-width="64"
      :width="216"
      :collapsed="collapsed"
      show-trigger="arrow-circle"
      class="app-sider"
      @collapse="collapsed = true"
      @expand="collapsed = false"
    >
      <div class="sider-brand" :class="{ collapsed }">
        <div class="brand-mark">
          <img src="/logo.png" alt="NFR" class="brand-logo-img" />
        </div>
        <div v-if="!collapsed" class="brand-text">
          <div class="brand-name">NFR 控制台</div>
          <div class="brand-sub nfr-mono">{{ dashStore.info?.name || '本机节点' }}</div>
        </div>
      </div>
      <nav aria-label="主导航">
        <NMenu
          :value="activeKey"
          :collapsed="collapsed"
          :collapsed-width="64"
          :collapsed-icon-size="20"
          :options="menuOptions"
          @update:value="handleSelectNav"
        />
      </nav>
    </NLayoutSider>

    <NLayout class="app-main-wrap">
      <NLayoutHeader bordered class="app-header" :class="{ mobile: !isDesktop }">
        <header class="header-inner">
          <div class="header-left">
            <span v-if="!isDesktop" class="brand-mark small">
              <img src="/logo.png" alt="NFR" class="brand-logo-img" />
            </span>
            <Transition name="nfr-title" mode="out-in">
              <span :key="pageTitle" class="header-title">{{ pageTitle }}</span>
            </Transition>
          </div>

          <div class="header-right">
            <NButton
              quaternary
              circle
              class="icon-btn"
              :aria-label="themeStore.isDark ? '切换为浅色模式' : '切换为深色模式'"
              @click="themeStore.toggleDark()"
            >
              <template #icon>
                <Transition name="nfr-icon-swap" mode="out-in">
                  <NIcon :key="themeStore.isDark ? 'sun' : 'moon'" :size="18">
                    <SunnyOutline v-if="themeStore.isDark" />
                    <MoonOutline v-else />
                  </NIcon>
                </Transition>
              </template>
            </NButton>

            <NButton
              v-if="isDesktop"
              quaternary
              size="small"
              aria-label="退出登录"
              @click="handleLogout"
            >
              <template #icon>
                <NIcon><LogOutOutline /></NIcon>
              </template>
              退出
            </NButton>
          </div>
        </header>
      </NLayoutHeader>

      <NLayoutContent class="app-content" :class="{ 'has-bottom-nav': !isDesktop }">
        <main class="content-container">
          <router-view v-slot="{ Component, route: currentRoute }">
            <Transition name="nfr-page" mode="out-in">
              <component :is="Component" :key="currentRoute.path" />
            </Transition>
          </router-view>
        </main>
      </NLayoutContent>
    </NLayout>

    <nav v-if="!isDesktop" class="mobile-bottom-nav" aria-label="底部导航">
      <button
        v-for="item in bottomNavItems"
        :key="item.key"
        type="button"
        class="bottom-tab"
        :class="{ active: activeKey === item.key }"
        :aria-current="activeKey === item.key ? 'page' : undefined"
        @click="handleSelectNav(item.key)"
      >
        <span class="bottom-tab-icon">
          <NIcon :size="20">
            <component :is="item.icon" />
          </NIcon>
          <span
            v-if="item.key === 'settings' && updateStore.hasUpdate"
            class="bottom-update-dot"
            aria-label="发现新版本"
          />
        </span>
        <span class="bottom-tab-label">{{ item.label }}</span>
      </button>
    </nav>

    <UpdateModal />
  </NLayout>
</template>

<style scoped>
.app-shell {
  min-height: 100vh;
  background: var(--nfr-canvas);
}

.app-sider {
  position: sticky;
  top: 0;
  height: 100vh;
}

.sider-brand {
  height: 56px;
  padding: 0 16px;
  display: flex;
  align-items: center;
  gap: 10px;
  border-bottom: 1px solid var(--nfr-border);
}

.sider-brand.collapsed {
  justify-content: center;
  padding: 0;
}

.brand-mark {
  width: 32px;
  height: 32px;
  border-radius: 8px;
  background: #ffffff;
  border: 1px solid var(--nfr-border);
  padding: 3px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  overflow: hidden;
}

.brand-mark.small {
  width: 28px;
  height: 28px;
  padding: 2px;
  border-radius: 7px;
}

.brand-logo-img {
  width: 100%;
  height: 100%;
  object-fit: contain;
  display: block;
}

.brand-text {
  overflow: hidden;
}

.brand-name {
  font-size: 14px;
  font-weight: 600;
  line-height: 1.25;
  color: var(--nfr-text);
  white-space: nowrap;
}

.brand-sub {
  font-size: 11px;
  color: var(--nfr-text-secondary);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.app-main-wrap {
  min-height: 100vh;
  background: var(--nfr-canvas);
}

.app-header {
  position: sticky;
  top: 0;
  z-index: 20;
  height: 56px;
  padding: 0 24px;
  background: var(--nfr-surface);
}

.app-header.mobile {
  height: 52px;
  padding: 0 14px;
}

.header-inner {
  height: 100%;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.header-left {
  display: flex;
  align-items: center;
  gap: 10px;
  overflow: hidden;
}

.header-title {
  font-size: 16px;
  font-weight: 600;
  color: var(--nfr-text);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.header-right {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
}

.icon-btn {
  width: 36px;
  height: 36px;
}

.app-content {
  background: var(--nfr-canvas);
}

.content-container {
  max-width: 1120px;
  margin: 0 auto;
  padding: 24px;
  width: 100%;
}

.app-content.has-bottom-nav .content-container {
  padding: 16px 14px calc(80px + env(safe-area-inset-bottom));
}

.mobile-bottom-nav {
  position: fixed;
  left: 0;
  right: 0;
  bottom: 0;
  z-index: 30;
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  background: var(--nfr-surface);
  border-top: 1px solid var(--nfr-border);
  padding: 6px 8px calc(6px + env(safe-area-inset-bottom));
}

.bottom-tab {
  min-height: 46px;
  border: none;
  background: transparent;
  color: var(--nfr-text-secondary);
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 3px;
  padding: 4px 2px;
  cursor: pointer;
  border-radius: 8px;
  font-family: inherit;
  -webkit-tap-highlight-color: transparent;
  transition:
    color var(--nfr-dur-fast) var(--nfr-ease-out),
    background-color var(--nfr-dur-base) var(--nfr-ease-out),
    transform 220ms var(--nfr-ease-spring);
}

.bottom-tab:active {
  transform: scale(0.92);
  transition-duration: 90ms;
}

.bottom-tab.active {
  color: var(--nfr-primary);
  background: var(--nfr-primary-soft);
  font-weight: 600;
}

.bottom-tab-icon {
  position: relative;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  line-height: 1;
  transition: transform 260ms var(--nfr-ease-spring);
}

.menu-label-wrap {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.nav-update-dot,
.bottom-update-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--nfr-online-text);
  display: inline-block;
}

.bottom-update-dot {
  position: absolute;
  top: -2px;
  right: -5px;
}

.bottom-tab.active .bottom-tab-icon {
  transform: translateY(-1px) scale(1.06);
}

.bottom-tab-label {
  font-size: 11px;
  line-height: 1.1;
  white-space: nowrap;
}
</style>

<script setup lang="ts">
import { computed, h, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NButton,
  NDropdown,
  NEmpty,
  NIcon,
  NInput,
  NSelect,
  NSkeleton,
  useDialog,
  useMessage,
  type DropdownOption,
} from 'naive-ui'
import {
  AddOutline,
  BuildOutline,
  CreateOutline,
  DocumentTextOutline,
  InformationCircleOutline,
  PowerOutline,
  SearchOutline,
  TrashOutline,
} from '@vicons/ionicons5'
import { apiRequest } from '@/api/client'
import { useDashboardStore } from '@/stores/dashboard'
import type { KickResponse, Tunnel } from '@/types/api'
import TunnelConfigModal from '@/components/TunnelConfigModal.vue'
import TunnelDetailModal from '@/components/TunnelDetailModal.vue'
import TunnelEditDrawer from '@/components/TunnelEditDrawer.vue'

const router = useRouter()
const message = useMessage()
const dialog = useDialog()
const dashStore = useDashboardStore()

const searchText = ref('')
const protocolFilter = ref('all')

const activeTunnelId = ref<string | null>(null)
const activeTunnelFallback = ref<Tunnel | null>(null)
const activeTunnel = computed<Tunnel | null>(() => {
  if (!activeTunnelId.value) return null
  return dashStore.tunnels.find((t) => t.id === activeTunnelId.value) ?? activeTunnelFallback.value
})
const configModalOpen = ref(false)
const detailModalOpen = ref(false)
const editDrawerOpen = ref(false)

const protocolOptions = [
  { label: '全部协议', value: 'all' },
  { label: 'TCP', value: 'tcp' },
  { label: 'HTTP', value: 'http' },
  { label: 'UDP', value: 'udp' },
]

const hostNodeLabel = computed(() => {
  const name = dashStore.info?.name || '本机节点'
  const host = dashStore.settings?.public_host || dashStore.settings?.domain || ''
  return host && host !== name ? `${name} · ${host}` : name
})

const filteredTunnels = computed(() => {
  const q = searchText.value.trim().toLowerCase()
  return dashStore.tunnels.filter((t) => {
    if (protocolFilter.value !== 'all' && t.type !== protocolFilter.value) {
      return false
    }
    if (!q) return true
    const haystack = [
      t.id,
      t.name,
      t.type,
      t.local,
      t.remote_port ? String(t.remote_port) : '',
      ...(t.hosts || []),
    ]
      .join(' ')
      .toLowerCase()
    return haystack.includes(q)
  })
})

function statusMeta(t: Tunnel): { label: string; cls: 'online' | 'offline' | 'disabled' } {
  if (!t.enabled) return { label: '已禁用', cls: 'disabled' }
  if (t.online) return { label: '在线', cls: 'online' }
  return { label: '离线', cls: 'offline' }
}

function remoteEndpointDisplay(t: Tunnel): string {
  if (t.remote_port > 0) return String(t.remote_port)
  if (t.hosts && t.hosts.length > 0) return t.hosts[0]
  return '—'
}

function renderIcon(comp: unknown, color?: string) {
  return () => h(NIcon, { size: 16, color }, { default: () => h(comp as never) })
}

function buildMenuOptions(t: Tunnel): DropdownOption[] {
  return [
    { label: '查看详情', key: 'detail', icon: renderIcon(InformationCircleOutline) },
    { label: '生成启动配置', key: 'config', icon: renderIcon(DocumentTextOutline) },
    { type: 'divider', key: 'd1' },
    { label: '编辑', key: 'edit', icon: renderIcon(CreateOutline) },
    {
      label: '强制下线',
      key: 'kick',
      disabled: !t.online,
      icon: renderIcon(PowerOutline),
    },
    { type: 'divider', key: 'd2' },
    {
      label: t.enabled ? '禁用' : '启用',
      key: 'toggle',
      icon: renderIcon(PowerOutline),
    },
    {
      label: '删除',
      key: 'delete',
      icon: renderIcon(TrashOutline, '#ef4444'),
      props: { style: { color: '#ef4444' } },
    },
  ]
}

async function handleMenuSelect(key: string, t: Tunnel) {
  activeTunnelId.value = t.id
  activeTunnelFallback.value = t
  switch (key) {
    case 'detail':
      detailModalOpen.value = true
      break
    case 'config':
      configModalOpen.value = true
      break
    case 'edit':
      editDrawerOpen.value = true
      break
    case 'kick':
      await handleKick(t)
      break
    case 'toggle':
      await handleToggle(t)
      break
    case 'delete':
      handleDelete(t)
      break
  }
}

async function handleKick(t: Tunnel) {
  try {
    const res = await apiRequest<KickResponse>(`/tunnels/${encodeURIComponent(t.id)}/kick`, {
      method: 'POST',
    })
    message.success(res.message || '已发送强制下线指令')
    void dashStore.fetchDashboard(true)
  } catch (err) {
    message.error(err instanceof Error ? err.message : '强制下线失败')
  }
}

async function handleToggle(t: Tunnel) {
  const next = !t.enabled
  try {
    await apiRequest(`/tunnels/${encodeURIComponent(t.id)}/toggle`, {
      method: 'POST',
      body: { enabled: next },
    })
    message.success(next ? `已启用「${t.name}」` : `已禁用「${t.name}」`)
    void dashStore.fetchDashboard(true)
  } catch (err) {
    message.error(err instanceof Error ? err.message : '切换状态失败')
  }
}

function handleDelete(t: Tunnel) {
  dialog.warning({
    title: '删除隧道',
    content: `确认永久删除隧道「${t.name}」？若客户端在线将被同步下线，此操作不可恢复。`,
    positiveText: '确认删除',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await apiRequest(`/tunnels/${encodeURIComponent(t.id)}`, { method: 'DELETE' })
        message.success(`已删除「${t.name}」`)
        void dashStore.fetchDashboard(true)
      } catch (err) {
        message.error(err instanceof Error ? err.message : '删除失败')
      }
    },
  })
}
function handleTunnelSaved(updated?: Tunnel) {
  if (updated) {
    dashStore.upsertTunnel(updated)
    if (activeTunnelId.value === updated.id) {
      activeTunnelFallback.value = updated
    }
  }
  void dashStore.fetchDashboard(true)
}
</script>

<template>
  <div class="tunnels-page">
    <!-- Minimalist Single-Row Toolbar -->
    <div class="toolbar">
      <NInput
        v-model:value="searchText"
        placeholder="搜索隧道名称、ID、端口或域名…"
        clearable
        class="toolbar-search"
      >
        <template #prefix>
          <NIcon><SearchOutline /></NIcon>
        </template>
      </NInput>

      <div class="toolbar-right">
        <NSelect
          v-model:value="protocolFilter"
          :options="protocolOptions"
          aria-label="按协议筛选"
          class="toolbar-select"
        />
        <NButton
          type="primary"
          class="toolbar-create"
          @click="router.push({ name: 'create' })"
        >
          <template #icon><NIcon><AddOutline /></NIcon></template>
          创建隧道
        </NButton>
      </div>
    </div>

    <!-- Loading Skeleton -->
    <div v-if="dashStore.loading && !dashStore.initialized" class="card-grid">
      <div v-for="i in 3" :key="i" class="nfr-card tunnel-card">
        <NSkeleton text :repeat="5" />
      </div>
    </div>

    <!-- Empty State -->
    <div v-else-if="dashStore.tunnels.length === 0" class="nfr-card empty-card">
      <NEmpty description="还没有创建任何隧道">
        <template #extra>
          <NButton type="primary" class="nfr-touch-btn" @click="router.push({ name: 'create' })">
            立即创建隧道
          </NButton>
        </template>
      </NEmpty>
    </div>

    <!-- Filtered Empty -->
    <div v-else-if="filteredTunnels.length === 0" class="nfr-card empty-card">
      <NEmpty description="没有符合条件的隧道">
        <template #extra>
          <NButton secondary @click="searchText = ''; protocolFilter = 'all'">重置筛选</NButton>
        </template>
      </NEmpty>
    </div>

    <!-- Reference-Matched Tunnel Card Grid -->
    <TransitionGroup v-else name="nfr-card-list" tag="div" class="card-grid">
      <article v-for="t in filteredTunnels" :key="t.id" class="nfr-card tunnel-card">
        <div class="card-head">
          <h3 class="card-name" :title="t.name">{{ t.name }}</h3>
          <span class="status-pill" :class="statusMeta(t).cls">
            {{ statusMeta(t).label }}
          </span>
        </div>

        <div class="card-kv">
          <div class="kv-row">
            <span class="kv-label">ID:</span>
            <span class="id-pill nfr-mono"># {{ t.id }}</span>
          </div>
          <div class="kv-row">
            <span class="kv-label">协议:</span>
            <span class="kv-val nfr-mono">{{ t.type.toUpperCase() }}</span>
          </div>
          <div class="kv-row">
            <span class="kv-label">{{ t.remote_port > 0 ? '远程端口:' : '绑定域名:' }}</span>
            <span class="kv-val nfr-mono nfr-break">{{ remoteEndpointDisplay(t) }}</span>
          </div>
          <div class="kv-row">
            <span class="kv-label">节点:</span>
            <span class="kv-val kv-ellipsis" :title="hostNodeLabel">{{ hostNodeLabel }}</span>
          </div>
        </div>

        <div class="card-foot">
          <NDropdown
            trigger="click"
            placement="bottom-end"
            :options="buildMenuOptions(t)"
            @select="(k) => handleMenuSelect(String(k), t)"
          >
            <NButton secondary size="small" class="more-btn">
              <template #icon><NIcon :size="15"><BuildOutline /></NIcon></template>
              更多
            </NButton>
          </NDropdown>
        </div>
      </article>
    </TransitionGroup>

    <TunnelConfigModal v-model:show="configModalOpen" :tunnel="activeTunnel" />
    <TunnelDetailModal v-model:show="detailModalOpen" :tunnel="activeTunnel" />
    <TunnelEditDrawer
      v-model:show="editDrawerOpen"
      :tunnel="activeTunnel"
      @saved="handleTunnelSaved"
    />
  </div>
</template>

<style scoped>
.tunnels-page {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.toolbar-search {
  flex: 1 1 220px;
}

.toolbar-right {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-shrink: 0;
}

.toolbar-select {
  width: 120px;
}

.toolbar-create {
  white-space: nowrap;
}

.empty-card {
  padding: 48px 16px;
}

.card-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(290px, 1fr));
  gap: 14px;
}

.tunnel-card {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 18px 20px;
}

@media (hover: hover) and (pointer: fine) {
  .tunnel-card:hover {
    transform: translateY(-2px);
    box-shadow: var(--nfr-shadow-hover);
    border-color: color-mix(in srgb, var(--nfr-primary) 28%, var(--nfr-border));
  }
}

.card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.card-name {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  color: var(--nfr-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.status-pill {
  flex-shrink: 0;
  font-size: 12px;
  font-weight: 500;
  padding: 3px 10px;
  border-radius: 6px;
  line-height: 1.35;
}

.status-pill.online {
  background: var(--nfr-online-bg);
  color: var(--nfr-online-text);
}

.status-pill.offline {
  background: var(--nfr-offline-bg);
  color: var(--nfr-offline-text);
}

.status-pill.disabled {
  background: var(--nfr-disabled-bg);
  color: var(--nfr-disabled-text);
}

.card-kv {
  display: flex;
  flex-direction: column;
  gap: 9px;
}

.kv-row {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13.5px;
  line-height: 1.4;
}

.kv-label {
  width: 72px;
  flex-shrink: 0;
  color: var(--nfr-text-secondary);
}

.kv-val {
  color: var(--nfr-text);
  flex: 1;
}

.kv-ellipsis {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.id-pill {
  display: inline-block;
  padding: 1px 9px;
  border-radius: 6px;
  background: var(--nfr-id-bg);
  color: var(--nfr-id-text);
  font-size: 12.5px;
  font-weight: 500;
}

.card-foot {
  display: flex;
  justify-content: flex-end;
  padding-top: 4px;
}

.more-btn {
  min-height: 34px;
  padding: 0 14px;
}

@media (max-width: 640px) {
  .toolbar-right {
    width: 100%;
  }

  .toolbar-select {
    flex: 1;
  }

  .card-grid {
    grid-template-columns: 1fr;
  }

  .tunnel-card {
    padding: 16px;
  }

  .more-btn {
    min-height: 38px;
  }
}
</style>

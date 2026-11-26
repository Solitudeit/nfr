<script setup lang="ts">
import { computed, nextTick, reactive, ref, watch } from 'vue'
import {
  NButton,
  NDrawer,
  NDrawerContent,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NSpace,
  NSwitch,
  useMessage,
} from 'naive-ui'
import { apiRequest } from '@/api/client'
import type { AllocateResponse, Tunnel } from '@/types/api'

const props = defineProps<{
  show: boolean
  tunnel: Tunnel | null
}>()

const emit = defineEmits<{
  'update:show': [val: boolean]
  saved: [tunnel: Tunnel]
}>()

const message = useMessage()
const saving = ref(false)
const allocating = ref(false)
const leaseID = ref('')
const leasedPort = ref<number | null>(null)

const form = reactive({
  name: '',
  type: 'tcp',
  local_host: '127.0.0.1',
  local_port: 8080 as number | null,
  remote_port: 0 as number | null,
  hosts: '',
  bandwidth_kb: 0,
  quota_mb: 0,
  enabled: true,
})

const drawerWidth = computed(() => {
  if (typeof window === 'undefined') return 420
  return window.innerWidth < 640 ? '100%' : 420
})

function splitLocalAddr(raw: string): { host: string; port: number } {
  const s = (raw || '').trim()
  const v6 = s.match(/^\[([^\]]+)\]:(\d+)$/)
  if (v6) {
    const p = Number(v6[2])
    return { host: v6[1], port: Number.isFinite(p) && p > 0 ? p : 8080 }
  }
  const idx = s.lastIndexOf(':')
  if (idx > 0) {
    const p = Number(s.slice(idx + 1))
    return { host: s.slice(0, idx), port: Number.isFinite(p) && p > 0 ? p : 8080 }
  }
  return { host: s, port: 8080 }
}

function joinLocalAddr(hostRaw: string, port: number): string {
  const host = hostRaw.trim().replace(/^\[|\]$/g, '')
  return host.includes(':') ? `[${host}]:${port}` : `${host}:${port}`
}

watch(
  () => [props.show, props.tunnel?.id] as const,
  ([open]) => {
    const t = props.tunnel
    if (!open || !t) return
    form.name = t.name
    form.type = t.type
    const parsed = splitLocalAddr(t.local)
    form.local_host = parsed.host
    form.local_port = parsed.port
    form.remote_port = t.remote_port || null
    form.hosts = (t.hosts || []).join(', ')
    form.bandwidth_kb = t.bandwidth_kb || 0
    form.quota_mb = t.quota_mb || 0
    form.enabled = t.enabled
    leaseID.value = ''
    leasedPort.value = null
  },
  { immediate: true }
)

watch(
  () => form.remote_port,
  (val) => {
    if (leaseID.value && val !== leasedPort.value) {
      leaseID.value = ''
      leasedPort.value = null
    }
  }
)

async function handleAllocate() {
  if (!props.tunnel) return
  allocating.value = true
  try {
    const res = await apiRequest<AllocateResponse>('/ports/allocate', {
      method: 'POST',
      body: {
        tunnel_id: props.tunnel.id,
        network: form.type === 'udp' ? 'udp' : 'tcp',
      },
    })
    leasedPort.value = res.port
    leaseID.value = res.lease_id
    form.remote_port = res.port
    message.success(`已分配空闲端口 ${res.port}`)
  } catch (err) {
    message.error(err instanceof Error ? err.message : '获取空闲端口失败')
  } finally {
    allocating.value = false
  }
}

async function handleSave() {
  if (!props.tunnel) return
  if (document.activeElement instanceof HTMLElement) {
    document.activeElement.blur()
  }
  await nextTick()

  if (!form.name.trim()) {
    message.warning('请填写隧道名称')
    return
  }
  if (!form.local_host.trim() || !form.local_port || form.local_port < 1 || form.local_port > 65535) {
    message.warning('请填写完整的本地地址与端口（1–65535）')
    return
  }
  saving.value = true
  try {
    const hosts = form.hosts
      .split(/[\s,]+/)
      .map((s) => s.trim())
      .filter(Boolean)

    const remotePort = form.remote_port && form.remote_port > 0 ? form.remote_port : 0
    const res = await apiRequest<{ tunnel: Tunnel }>(
      `/tunnels/${encodeURIComponent(props.tunnel.id)}`,
      {
        method: 'PATCH',
        body: {
          name: form.name.trim(),
          node_id: props.tunnel.node_id || '',
          type: form.type,
          local: joinLocalAddr(form.local_host, form.local_port),
          remote_port: remotePort,
          clear_remote_port: remotePort === 0,
          hosts: form.type === 'http' ? hosts : [],
          bandwidth_kb: form.bandwidth_kb || 0,
          quota_mb: form.quota_mb || 0,
          enabled: form.enabled,
          lease_id: leaseID.value && remotePort === leasedPort.value ? leaseID.value : undefined,
        },
      }
    )
    message.success('隧道配置已更新')
    if (res?.tunnel) {
      emit('saved', res.tunnel)
    }
    emit('update:show', false)
  } catch (err) {
    message.error(err instanceof Error ? err.message : '保存失败')
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <NDrawer
    :show="show"
    :width="drawerWidth"
    placement="right"
    @update:show="emit('update:show', $event)"
  >
    <NDrawerContent title="编辑隧道" closable>
      <NForm label-placement="top" class="edit-form">
        <NFormItem label="隧道名称 *">
          <NInput v-model:value="form.name" placeholder="如 web、ssh" />
        </NFormItem>

        <div class="pair-row">
          <NFormItem label="本地地址 *">
            <NInput v-model:value="form.local_host" placeholder="127.0.0.1" />
          </NFormItem>
          <NFormItem label="本地端口 *">
            <NInputNumber
              v-model:value="form.local_port"
              :min="1"
              :max="65535"
              :update-value-on-input="true"
              class="full-w"
            />
          </NFormItem>
        </div>

        <NFormItem v-if="form.type === 'http'" label="绑定域名（多个用逗号分隔）">
          <NInput v-model:value="form.hosts" placeholder="可选，如 app.example.com" />
        </NFormItem>

        <NFormItem label="远程端口">
          <div class="port-row">
            <NInputNumber
              v-model:value="form.remote_port"
              :min="0"
              :max="65535"
              :update-value-on-input="true"
              placeholder="自动分配"
              class="port-input"
            />
            <NButton secondary :loading="allocating" @click="handleAllocate">获取空闲端口</NButton>
          </div>
        </NFormItem>

        <div class="pair-row">
          <NFormItem label="带宽限速 (KB/s)">
            <NInputNumber
              v-model:value="form.bandwidth_kb"
              :min="0"
              :update-value-on-input="true"
              class="full-w"
            />
          </NFormItem>
          <NFormItem label="月配额 (MB)">
            <NInputNumber
              v-model:value="form.quota_mb"
              :min="0"
              :update-value-on-input="true"
              class="full-w"
            />
          </NFormItem>
        </div>

        <NFormItem label="启用隧道">
          <NSwitch v-model:value="form.enabled" />
        </NFormItem>
      </NForm>

      <template #footer>
        <NSpace justify="end">
          <NButton @click="emit('update:show', false)">取消</NButton>
          <NButton type="primary" :loading="saving" @click="handleSave">保存修改</NButton>
        </NSpace>
      </template>
    </NDrawerContent>
  </NDrawer>
</template>

<style scoped>
.pair-row {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px;
}

.full-w {
  width: 100%;
}

.port-row {
  display: flex;
  gap: 8px;
  width: 100%;
  flex-wrap: wrap;
}

.port-input {
  flex: 1 1 140px;
}

@media (max-width: 420px) {
  .pair-row {
    grid-template-columns: 1fr;
  }
}
</style>

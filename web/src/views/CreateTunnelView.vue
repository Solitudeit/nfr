<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  NAlert,
  NButton,
  NCollapse,
  NCollapseItem,
  NForm,
  NFormItem,
  NIcon,
  NInput,
  NInputNumber,
  NRadioButton,
  NRadioGroup,
  NResult,
  NSpace,
  NTag,
  useMessage,
} from 'naive-ui'
import {
  CheckmarkCircleOutline,
  DocumentTextOutline,
  RefreshOutline,
  WarningOutline,
} from '@vicons/ionicons5'
import { apiRequest } from '@/api/client'
import { useDashboardStore } from '@/stores/dashboard'
import type { AllocateResponse, Tunnel } from '@/types/api'
import { tunnelPublicAddress } from '@/utils/format'
import TunnelConfigModal from '@/components/TunnelConfigModal.vue'

const router = useRouter()
const message = useMessage()
const dashStore = useDashboardStore()

const form = reactive({
  name: '',
  local_host: '',
  local_port: null as number | null,
  type: 'tcp' as 'tcp' | 'udp' | 'http',
  remote_port: null as number | null,
  primary_host: '',
  bandwidth_kb: null as number | null,
  quota_mb: null as number | null,
})

const errors = reactive<Record<string, string>>({})
const leaseID = ref('')
const leasedPort = ref<number | null>(null)
const allocating = ref(false)
const submitting = ref(false)

const createdTunnel = ref<Tunnel | null>(null)
const configModalOpen = ref(false)

const serverDomain = computed(() => (dashStore.settings?.domain || '').trim())
const httpUsesDomain = computed(
  () => form.type === 'http' && Boolean(serverDomain.value || form.primary_host.trim())
)

watch(
  () => form.type,
  (next, prev) => {
    if ((next === 'udp') !== (prev === 'udp')) {
      leaseID.value = ''
      leasedPort.value = null
    }
    if (next === 'http' && serverDomain.value && !form.remote_port) {
      form.remote_port = null
      leaseID.value = ''
      leasedPort.value = null
    }
  }
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

async function handleAllocatePort() {
  allocating.value = true
  try {
    const res = await apiRequest<AllocateResponse>('/ports/allocate', {
      method: 'POST',
      body: { network: form.type === 'udp' ? 'udp' : 'tcp' },
    })
    leasedPort.value = res.port
    leaseID.value = res.lease_id
    form.remote_port = res.port
    delete errors.remote_port
    message.success(`已分配空闲端口 ${res.port}`)
  } catch (err) {
    message.error(err instanceof Error ? err.message : '获取空闲端口失败')
  } finally {
    allocating.value = false
  }
}

function validate(): boolean {
  for (const k of Object.keys(errors)) delete errors[k]

  if (!form.name.trim()) {
    errors.name = '请填写隧道名称（如 web、ssh）'
  }
  if (!form.local_host.trim()) {
    errors.local_host = '请填写本地地址（如 127.0.0.1）'
  }
  if (!form.local_port || form.local_port < 1 || form.local_port > 65535) {
    errors.local_port = '请填写 1–65535 之间的本地端口'
  }
  if (form.type !== 'http' && (!form.remote_port || form.remote_port < 1 || form.remote_port > 65535)) {
    errors.remote_port = '请填写远程端口，或点击「获取空闲端口」自动分配'
  }
  if (form.type === 'http' && !httpUsesDomain.value && (!form.remote_port || form.remote_port < 1)) {
    errors.remote_port = '未配置域名时请填写远程端口，或点击「获取空闲端口」'
  }

  const firstKey = Object.keys(errors)[0]
  if (firstKey) {
    const el = document.querySelector(`[data-field="${firstKey}"]`)
    el?.scrollIntoView({ behavior: 'smooth', block: 'center' })
    return false
  }
  return true
}

async function handleSubmit() {
  if (!validate()) return
  submitting.value = true
  try {
    const hosts: string[] = []
    if (form.type === 'http' && form.primary_host.trim()) {
      hosts.push(form.primary_host.trim())
    }

    const res = await apiRequest<{ tunnel: Tunnel }>('/tunnels', {
      method: 'POST',
      body: {
        name: form.name.trim(),
        node_id: '',
        type: form.type,
        local: `${form.local_host.trim()}:${form.local_port}`,
        remote_port: form.remote_port && form.remote_port > 0 ? form.remote_port : 0,
        hosts,
        bandwidth_kb: form.bandwidth_kb || 0,
        quota_mb: form.quota_mb || 0,
        enabled: true,
        lease_id: leaseID.value && form.remote_port === leasedPort.value ? leaseID.value : undefined,
      },
    })

    createdTunnel.value = res.tunnel
    leaseID.value = ''
    leasedPort.value = null
    message.success(`隧道「${res.tunnel.name}」创建完成`)
    void dashStore.fetchDashboard(true)
  } catch (err) {
    message.error(err instanceof Error ? err.message : '创建隧道失败')
  } finally {
    submitting.value = false
  }
}

function resetForAnother() {
  createdTunnel.value = null
  form.name = ''
  form.local_host = ''
  form.local_port = null
  form.remote_port = null
  form.primary_host = ''
  form.bandwidth_kb = null
  form.quota_mb = null
  leaseID.value = ''
  leasedPort.value = null
}
</script>

<template>
  <div class="create-wrap">
    <Transition name="nfr-page" mode="out-in">
      <!-- Step 4: Clean Result Card -->
      <section v-if="createdTunnel" key="result" class="nfr-card result-card">
      <NResult
        status="success"
        title="隧道创建完成"
        :description="`将启动配置填入客户端并运行后，外部即可访问「${createdTunnel.name}」`"
      >
        <template #icon>
          <NIcon :size="44" color="var(--nfr-online-text)"><CheckmarkCircleOutline /></NIcon>
        </template>

        <div class="result-addr-box">
          <div class="result-addr nfr-mono nfr-break">{{ tunnelPublicAddress(createdTunnel) }}</div>
          <div class="result-local nfr-mono">本地服务：{{ createdTunnel.local }}</div>
        </div>

        <template #footer>
          <NSpace justify="center" :wrap="true" :size="10">
            <NButton type="primary" class="nfr-touch-btn" @click="configModalOpen = true">
              <template #icon><NIcon><DocumentTextOutline /></NIcon></template>
              生成启动配置
            </NButton>
            <NButton secondary class="nfr-touch-btn" @click="router.push({ name: 'tunnels' })">
              前往隧道管理
            </NButton>
            <NButton quaternary class="nfr-touch-btn" @click="resetForAnother">
              <template #icon><NIcon><RefreshOutline /></NIcon></template>
              继续创建
            </NButton>
          </NSpace>
        </template>
      </NResult>
    </section>

    <!-- Steps 1–3: Clean Single-Column Form -->
    <section v-else key="form" class="nfr-card form-card">
      <div class="form-head">
        <h2 class="nfr-page-title">创建隧道</h2>
        <p class="nfr-page-subtitle">带 * 为必填，创建后在隧道管理中生成启动配置给客户端运行</p>
      </div>

      <NForm label-placement="top" @submit.prevent="handleSubmit">
        <NFormItem
          label="隧道名称 *"
          :validation-status="errors.name ? 'error' : undefined"
          :feedback="errors.name"
          data-field="name"
        >
          <NInput
            v-model:value="form.name"
            placeholder="便于区分用途（如 web、ssh）"
            clearable
          />
        </NFormItem>

        <div class="local-grid">
          <NFormItem
            label="本地地址 *"
            :validation-status="errors.local_host ? 'error' : undefined"
            :feedback="errors.local_host"
            data-field="local_host"
          >
            <NInput v-model:value="form.local_host" placeholder="如 127.0.0.1" clearable />
          </NFormItem>

          <NFormItem
            label="本地端口 *"
            :validation-status="errors.local_port ? 'error' : undefined"
            :feedback="errors.local_port"
            data-field="local_port"
          >
            <NInputNumber
              v-model:value="form.local_port"
              :min="1"
              :max="65535"
              placeholder="如 8080"
              class="full-w"
            />
          </NFormItem>
        </div>

        <NFormItem label="协议类型 *" data-field="type">
          <NRadioGroup v-model:value="form.type" size="medium" class="proto-group">
            <NRadioButton value="tcp">TCP</NRadioButton>
            <NRadioButton value="http">HTTP</NRadioButton>
            <NRadioButton value="udp">UDP</NRadioButton>
          </NRadioGroup>
        </NFormItem>

        <NFormItem v-if="form.type === 'http'" label="绑定域名（选填）">
          <NInput
            v-model:value="form.primary_host"
            :placeholder="serverDomain ? `留空自动分配 *.${serverDomain}` : '如 app.example.com（也可留空走端口）'"
            clearable
          />
        </NFormItem>

        <NFormItem
          :label="form.type === 'http' && httpUsesDomain ? '远程端口（选填）' : '远程端口 *'"
          :validation-status="errors.remote_port ? 'error' : undefined"
          :feedback="errors.remote_port"
          data-field="remote_port"
        >
          <div class="remote-port-row">
            <NInputNumber
              v-model:value="form.remote_port"
              :min="1"
              :max="65535"
              placeholder="如 20001"
              class="port-stepper"
            />
            <NButton
              secondary
              class="nfr-touch-btn alloc-btn"
              :loading="allocating"
              @click="handleAllocatePort"
            >
              获取空闲端口
            </NButton>
          </div>
        </NFormItem>

        <NCollapse class="adv-collapse">
          <NCollapseItem title="高级配置（可选）" name="adv">
            <NAlert type="warning" :bordered="false" class="adv-warn">
              <template #icon><NIcon><WarningOutline /></NIcon></template>
              该区域仅推荐技术用户修改，配置错误会导致隧道无法启动。
            </NAlert>

            <NFormItem label="传输协议">
              <div class="transport-box">
                <NTag :bordered="false" size="small">TCP（TLS 1.3 + yamux，当前生效）</NTag>
                <span class="transport-note">QUIC 传输为后续规划功能，当前暂不提供切换</span>
              </div>
            </NFormItem>

            <div class="local-grid">
              <NFormItem label="带宽限速（KB/s，留空不限）">
                <NInputNumber v-model:value="form.bandwidth_kb" :min="0" placeholder="留空不限速" class="full-w" />
              </NFormItem>
              <NFormItem label="月流量配额（MB，留空不限）">
                <NInputNumber v-model:value="form.quota_mb" :min="0" placeholder="留空不限额" class="full-w" />
              </NFormItem>
            </div>
          </NCollapseItem>
        </NCollapse>

        <NButton
          type="primary"
          size="large"
          block
          attr-type="submit"
          class="nfr-touch-btn submit-btn"
          :loading="submitting"
        >
          创建隧道
        </NButton>
      </NForm>
      </section>
    </Transition>

    <TunnelConfigModal v-model:show="configModalOpen" :tunnel="createdTunnel" />
  </div>
</template>

<style scoped>
.create-wrap {
  max-width: 560px;
  margin: 0 auto;
  width: 100%;
}

.form-head {
  margin-bottom: 18px;
}

.local-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}

.full-w {
  width: 100%;
}

.proto-group {
  display: flex;
  width: 100%;
}

.proto-group :deep(.n-radio-button) {
  flex: 1;
  text-align: center;
}

.remote-port-row {
  display: flex;
  gap: 8px;
  width: 100%;
  align-items: center;
}

.port-stepper {
  flex: 1;
}

.alloc-btn {
  flex-shrink: 0;
  white-space: nowrap;
}

.adv-collapse {
  margin: 6px 0 18px;
  padding: 10px 12px;
  border-radius: 8px;
  border: 1px solid var(--nfr-border);
  background: var(--nfr-subtle);
}

.adv-warn {
  margin-bottom: 12px;
}

.transport-box {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.transport-note {
  font-size: 12px;
  color: var(--nfr-text-secondary);
}

.submit-btn {
  font-weight: 600;
}

.result-card {
  padding: 28px 20px;
}

.result-addr-box {
  padding: 14px;
  border-radius: 10px;
  border: 1px solid var(--nfr-border);
  background: var(--nfr-subtle);
  text-align: center;
}

.result-addr {
  font-size: 18px;
  font-weight: 600;
  color: var(--nfr-text);
}

.result-local {
  margin-top: 4px;
  font-size: 12.5px;
  color: var(--nfr-text-secondary);
}

@media (max-width: 480px) {
  .local-grid {
    grid-template-columns: 1fr;
    gap: 0;
  }

  .remote-port-row {
    flex-direction: column;
    align-items: stretch;
  }

  .alloc-btn {
    width: 100%;
  }
}
</style>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { NButton, NIcon, NModal, useMessage } from 'naive-ui'
import { CheckmarkOutline, CopyOutline, OpenOutline, PulseOutline } from '@vicons/ionicons5'
import { useDashboardStore } from '@/stores/dashboard'
import type { Tunnel } from '@/types/api'
import {
  copyToClipboard,
  formatBytes,
  formatRate,
  tunnelHttpHref,
  tunnelPublicAddress,
} from '@/utils/format'
import ProbeBadge from '@/components/ProbeBadge.vue'

const props = defineProps<{
  show: boolean
  tunnel: Tunnel | null
}>()

const emit = defineEmits<{
  'update:show': [val: boolean]
}>()

const message = useMessage()
const dashStore = useDashboardStore()

const liveTunnel = computed(() => {
  if (!props.tunnel) return null
  return dashStore.tunnels.find((t) => t.id === props.tunnel?.id) ?? props.tunnel
})

const statusInfo = computed(() => {
  const t = liveTunnel.value
  if (!t) return { label: '离线', cls: 'offline' }
  if (!t.enabled) return { label: '已禁用', cls: 'disabled' }
  if (t.online) return { label: '在线', cls: 'online' }
  return { label: '离线', cls: 'offline' }
})

const publicAddr = computed(() => (liveTunnel.value ? tunnelPublicAddress(liveTunnel.value) : ''))
const httpHref = computed(() => (liveTunnel.value ? tunnelHttpHref(liveTunnel.value) : null))
const probeRes = computed(() => (liveTunnel.value ? dashStore.probeResults[liveTunnel.value.id] : undefined))
const isProbing = computed(() => Boolean(liveTunnel.value && dashStore.probingIds[liveTunnel.value.id]))

const copiedAddr = ref(false)
let copiedTimer: number | undefined

onBeforeUnmount(() => {
  window.clearTimeout(copiedTimer)
})

async function handleCopyAddr() {
  if (!publicAddr.value) return
  if (await copyToClipboard(publicAddr.value)) {
    copiedAddr.value = true
    window.clearTimeout(copiedTimer)
    copiedTimer = window.setTimeout(() => {
      copiedAddr.value = false
    }, 1600)
    message.success(`已复制 ${publicAddr.value}`)
  } else {
    message.warning('自动复制受限，请长按地址手动复制')
  }
}

async function handleProbe() {
  if (!liveTunnel.value) return
  try {
    await dashStore.probeTunnel(liveTunnel.value.id)
  } catch (err) {
    message.error(err instanceof Error ? err.message : '测通失败')
  }
}
</script>

<template>
  <NModal
    :show="show"
    preset="card"
    :trap-focus="false"
    :title="liveTunnel ? `隧道详情 · ${liveTunnel.name}` : '隧道详情'"
    :bordered="false"
    class="nfr-modal-card"
    @update:show="emit('update:show', $event)"
  >
    <div v-if="liveTunnel" class="detail-body">
      <div class="detail-addr-box">
        <div class="detail-addr-top">
          <span class="detail-label">公网访问地址</span>
          <span class="status-pill" :class="statusInfo.cls">{{ statusInfo.label }}</span>
        </div>
        <div class="detail-addr nfr-mono nfr-break">{{ publicAddr }}</div>
        <div class="detail-addr-actions">
          <NButton size="small" type="primary" @click="handleCopyAddr">
            <template #icon>
              <Transition name="nfr-icon-swap" mode="out-in">
                <NIcon :key="copiedAddr ? 'ok' : 'copy'">
                  <CheckmarkOutline v-if="copiedAddr" />
                  <CopyOutline v-else />
                </NIcon>
              </Transition>
            </template>
            {{ copiedAddr ? '已复制' : '复制地址' }}
          </NButton>
          <NButton
            v-if="httpHref"
            size="small"
            secondary
            tag="a"
            :href="httpHref"
            target="_blank"
            rel="noopener noreferrer"
          >
            <template #icon><NIcon><OpenOutline /></NIcon></template>
            直接打开
          </NButton>
          <NButton size="small" secondary :loading="isProbing" @click="handleProbe">
            <template #icon><NIcon><PulseOutline /></NIcon></template>
            一键测通
          </NButton>
        </div>
        <div v-if="probeRes || isProbing" class="detail-probe">
          <ProbeBadge :result="probeRes" :loading="isProbing" />
        </div>
      </div>

      <div class="detail-list">
        <div class="detail-row">
          <span class="row-k">隧道 ID</span>
          <span class="id-pill nfr-mono"># {{ liveTunnel.id }}</span>
        </div>
        <div class="detail-row">
          <span class="row-k">协议类型</span>
          <span class="row-v nfr-mono">{{ liveTunnel.type.toUpperCase() }}</span>
        </div>
        <div class="detail-row">
          <span class="row-k">本地目标</span>
          <span class="row-v nfr-mono nfr-break">{{ liveTunnel.local }}</span>
        </div>
        <div v-if="liveTunnel.remote_port > 0" class="detail-row">
          <span class="row-k">远程端口</span>
          <span class="row-v nfr-mono">{{ liveTunnel.remote_port }}</span>
        </div>
        <div v-if="liveTunnel.hosts && liveTunnel.hosts.length > 0" class="detail-row">
          <span class="row-k">绑定域名</span>
          <span class="row-v nfr-mono nfr-break">{{ liveTunnel.hosts.join(', ') }}</span>
        </div>
        <div class="detail-row">
          <span class="row-k">实时速率</span>
          <span class="row-v nfr-mono">
            ↓ {{ formatRate(liveTunnel.in_rate) }} · ↑ {{ formatRate(liveTunnel.out_rate) }}
          </span>
        </div>
        <div class="detail-row">
          <span class="row-k">累计流量</span>
          <span class="row-v nfr-mono">
            {{ formatBytes((liveTunnel.in || 0) + (liveTunnel.out || 0)) }}
          </span>
        </div>
        <div class="detail-row">
          <span class="row-k">限速 / 配额</span>
          <span class="row-v nfr-mono">
            {{ liveTunnel.bandwidth_kb ? `${liveTunnel.bandwidth_kb} KB/s` : '不限速' }}
            ·
            {{ liveTunnel.quota_mb ? `${liveTunnel.quota_mb} MB/月` : '不限额' }}
          </span>
        </div>
        <div v-if="liveTunnel.target_down || liveTunnel.quota_paused" class="detail-row">
          <span class="row-k">异常提示</span>
          <span class="row-v warn-text">
            {{ liveTunnel.quota_paused ? '已达本月流量配额上限' : '客户端已在线，但无法连接本地服务端口' }}
          </span>
        </div>
      </div>
    </div>
  </NModal>
</template>

<style scoped>
.detail-body {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.detail-addr-box {
  padding: 14px;
  border-radius: 10px;
  border: 1px solid var(--nfr-border);
  background: var(--nfr-subtle);
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.detail-addr-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.detail-label {
  font-size: 12px;
  color: var(--nfr-text-secondary);
}

.detail-addr {
  font-size: 16px;
  font-weight: 600;
  color: var(--nfr-text);
}

.detail-addr-actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}

.detail-probe {
  padding-top: 2px;
}

.detail-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.detail-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  font-size: 13.5px;
}

.row-k {
  color: var(--nfr-text-secondary);
  flex-shrink: 0;
}

.row-v {
  color: var(--nfr-text);
  text-align: right;
}

.warn-text {
  color: var(--nfr-offline-text);
}

.status-pill {
  font-size: 12px;
  font-weight: 500;
  padding: 2px 10px;
  border-radius: 6px;
  line-height: 1.4;
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

.id-pill {
  display: inline-block;
  padding: 1px 8px;
  border-radius: 6px;
  background: var(--nfr-id-bg);
  color: var(--nfr-id-text);
  font-size: 12.5px;
  font-weight: 500;
}
</style>

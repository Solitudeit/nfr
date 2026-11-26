<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import {
  NAlert,
  NButton,
  NCode,
  NCollapse,
  NCollapseItem,
  NIcon,
  NModal,
  NRadioButton,
  NRadioGroup,
  NSkeleton,
  useMessage,
} from 'naive-ui'
import { CheckmarkOutline, CopyOutline, DownloadOutline } from '@vicons/ionicons5'
import { apiRequest } from '@/api/client'
import { useDashboardStore } from '@/stores/dashboard'
import type { Tunnel, TunnelConfigResponse } from '@/types/api'
import { copyToClipboard, downloadTextFile } from '@/utils/format'
import ClientDownloadCard from '@/components/ClientDownloadCard.vue'

const props = defineProps<{
  show: boolean
  tunnel: Tunnel | null
}>()

const emit = defineEmits<{
  'update:show': [val: boolean]
}>()

const message = useMessage()
const dashStore = useDashboardStore()
const format = ref<'toml' | 'yaml'>('toml')
const loading = ref(false)
const errMsg = ref('')
const data = ref<TunnelConfigResponse | null>(null)
const configRev = ref(0)
const copiedConfig = ref(false)
const copiedCmd = ref(false)
let copiedConfigTimer: number | undefined
let copiedCmdTimer: number | undefined
let fetchSeq = 0

const liveTunnel = computed(() => {
  if (!props.tunnel) return null
  return dashStore.tunnels.find((t) => t.id === props.tunnel?.id) ?? props.tunnel
})

onBeforeUnmount(() => {
  window.clearTimeout(copiedConfigTimer)
  window.clearTimeout(copiedCmdTimer)
})

const activeText = computed(() => {
  if (!data.value) return ''
  return format.value === 'toml' ? data.value.toml : data.value.yaml
})

const activeFilename = computed(() => {
  if (!data.value) return 'client.toml'
  return format.value === 'toml' ? data.value.filename_toml : data.value.filename_yaml
})

const runCommand = computed(() => `nfrc -c ${activeFilename.value}`)

watch(
  () => [
    props.show,
    liveTunnel.value?.id,
    liveTunnel.value?.updated_at,
    liveTunnel.value?.name,
    liveTunnel.value?.local,
    liveTunnel.value?.remote_port,
  ] as const,
  async ([open, id]) => {
    if (!open || !id) {
      fetchSeq++
      loading.value = false
      errMsg.value = ''
      data.value = null
      return
    }
    const seq = ++fetchSeq
    loading.value = true
    errMsg.value = ''
    data.value = null
    try {
      const res = await apiRequest<TunnelConfigResponse>(
        `/tunnels/${encodeURIComponent(String(id))}/config?_t=${Date.now()}`
      )
      if (seq !== fetchSeq) return
      data.value = res
      configRev.value += 1
    } catch (err) {
      if (seq !== fetchSeq) return
      errMsg.value = err instanceof Error ? err.message : '生成配置失败'
    } finally {
      if (seq === fetchSeq) {
        loading.value = false
      }
    }
  }
)

async function handleCopyConfig() {
  if (!activeText.value) return
  if (await copyToClipboard(activeText.value)) {
    copiedConfig.value = true
    window.clearTimeout(copiedConfigTimer)
    copiedConfigTimer = window.setTimeout(() => {
      copiedConfig.value = false
    }, 1600)
    message.success('启动配置已写入剪贴板')
  } else {
    message.warning('当前浏览器禁止自动复制，请点「下载文件」或长按下方配置手动复制')
  }
}

function handleDownloadConfig() {
  if (!activeText.value) return
  downloadTextFile(activeFilename.value, activeText.value)
  message.success(`已下载 ${activeFilename.value}`)
}

async function handleCopyCmd() {
  if (await copyToClipboard(runCommand.value)) {
    copiedCmd.value = true
    window.clearTimeout(copiedCmdTimer)
    copiedCmdTimer = window.setTimeout(() => {
      copiedCmd.value = false
    }, 1600)
    message.success('启动命令已复制')
  } else {
    message.warning('自动复制受限，请长按命令手动复制')
  }
}
</script>

<template>
  <NModal
    :show="show"
    preset="card"
    :trap-focus="false"
    :title="liveTunnel ? `生成启动配置 · ${liveTunnel.name}` : '生成启动配置'"
    :bordered="false"
    class="nfr-modal-card"
    @update:show="emit('update:show', $event)"
  >
    <div v-if="loading" class="cfg-loading">
      <NSkeleton text :repeat="6" />
    </div>

    <NAlert v-else-if="errMsg" type="error" :bordered="false">
      {{ errMsg }}
    </NAlert>

    <div v-else-if="data" class="cfg-body">
      <div class="cfg-bar">
        <NRadioGroup v-model:value="format" size="small">
          <NRadioButton value="toml">TOML</NRadioButton>
          <NRadioButton value="yaml">YAML</NRadioButton>
        </NRadioGroup>

        <div class="cfg-actions">
          <NButton size="small" secondary @click="handleCopyConfig">
            <template #icon>
              <Transition name="nfr-icon-swap" mode="out-in">
                <NIcon :key="copiedConfig ? 'ok' : 'copy'">
                  <CheckmarkOutline v-if="copiedConfig" />
                  <CopyOutline v-else />
                </NIcon>
              </Transition>
            </template>
            {{ copiedConfig ? '已复制' : '复制配置' }}
          </NButton>
          <NButton size="small" type="primary" @click="handleDownloadConfig">
            <template #icon><NIcon><DownloadOutline /></NIcon></template>
            下载文件
          </NButton>
        </div>
      </div>

      <Transition name="nfr-fade" mode="out-in">
        <div :key="`${format}-${data.tunnel_id}-${configRev}`" class="nfr-code-box cfg-code">
          <NCode :code="activeText" :language="format === 'toml' ? 'ini' : 'yaml'" />
        </div>
      </Transition>

      <div class="cfg-cmd-row">
        <span class="cfg-cmd-label">启动命令</span>
        <div class="cfg-cmd-box">
          <code class="nfr-mono nfr-break">{{ runCommand }}</code>
          <NButton text size="tiny" @click="handleCopyCmd">
            {{ copiedCmd ? '已复制' : '复制' }}
          </NButton>
        </div>
      </div>

      <NCollapse class="cfg-dl-collapse">
        <NCollapseItem title="还没有客户端？选系统直接下载 nfrc" name="dl">
          <ClientDownloadCard compact />
        </NCollapseItem>
      </NCollapse>
    </div>
  </NModal>
</template>

<style scoped>
.cfg-loading {
  padding: 12px 0;
}

.cfg-body {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.cfg-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
}

.cfg-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.cfg-code {
  max-height: 280px;
  overflow-y: auto;
}

.cfg-cmd-row {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.cfg-cmd-label {
  font-size: 12px;
  color: var(--nfr-text-secondary);
}

.cfg-cmd-box {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 8px 12px;
  border-radius: 8px;
  border: 1px solid var(--nfr-border);
  background: var(--nfr-subtle);
  font-size: 12.5px;
}

.cfg-dl-collapse {
  border-top: 1px solid var(--nfr-border);
  padding-top: 10px;
}
</style>

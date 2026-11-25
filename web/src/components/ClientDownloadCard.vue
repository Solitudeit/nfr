<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { NButton, NIcon, NSelect, NSkeleton } from 'naive-ui'
import { DownloadOutline, RefreshOutline } from '@vicons/ionicons5'
import { apiRequest } from '@/api/client'
import type { DownloadItem, DownloadSource, DownloadsPayload, MirrorNode } from '@/types/api'
import { formatBytes } from '@/utils/format'

const SOURCE_PREF_KEY = 'nfr_download_source'
const PROBE_TIMEOUT_MS = 3500

const props = defineProps<{
  compact?: boolean
}>()

const sources = ref<DownloadSource[]>([])
const mirrors = ref<MirrorNode[]>([])
const items = ref<DownloadItem[]>([])
const loading = ref(true)
const selectedSource = ref('')
const selectedOS = ref('')
const selectedArch = ref('')

// In-browser mirror speed-test state
const probingMirrors = ref(false)
const bestMirror = ref<{ node: MirrorNode; rttMs: number } | null>(null)
const probeFailed = ref(false)
let activeControllers: AbortController[] = []
let probeSeq = 0

function readSavedSource(): string {
  try {
    const raw = window.localStorage.getItem(SOURCE_PREF_KEY) || ''
    // Migrate retired Gitee preference cleanly to Mirror download.
    if (raw === 'gitee') return 'mirror'
    return raw
  } catch {
    return ''
  }
}

function saveSource(key: string) {
  try {
    window.localStorage.setItem(SOURCE_PREF_KEY, key)
  } catch {
    // Ignore storage restrictions
  }
}

function detectClientOS(): string {
  if (typeof navigator === 'undefined') return 'linux'
  const ua = navigator.userAgent.toLowerCase()
  if (ua.includes('android')) return 'android'
  if (ua.includes('win')) return 'windows'
  if (ua.includes('mac')) return 'darwin'
  return 'linux'
}

function cancelActiveProbes() {
  for (const ctrl of activeControllers) {
    try {
      ctrl.abort()
    } catch {
      // Ignore abort errors
    }
  }
  activeControllers = []
}

async function probeSingleMirror(node: MirrorNode, seq: number): Promise<{ node: MirrorNode; rttMs: number }> {
  const ctrl = new AbortController()
  activeControllers.push(ctrl)
  const timer = window.setTimeout(() => ctrl.abort(), PROBE_TIMEOUT_MS)
  const sep = node.probe_url.includes('?') ? '&' : '?'
  const probeTarget = `${node.probe_url}${sep}_nfr_ping=${Date.now()}_${seq}`
  const t0 = performance.now()
  try {
    const res = await fetch(probeTarget, {
      method: 'GET',
      mode: 'no-cors',
      cache: 'no-store',
      signal: ctrl.signal,
    })
    if (res.type !== 'opaque' && !res.ok) {
      throw new Error(`HTTP ${res.status}`)
    }
    const rttMs = Math.max(1, Math.round(performance.now() - t0))
    return { node, rttMs }
  } finally {
    window.clearTimeout(timer)
  }
}

async function runMirrorSpeedTest(force = false) {
  if (mirrors.value.length === 0) return
  if (!force && bestMirror.value && !probeFailed.value) return

  cancelActiveProbes()
  const seq = ++probeSeq
  probingMirrors.value = true
  probeFailed.value = false
  if (force) {
    bestMirror.value = null
  }

  let settledWinner = false
  let pending = mirrors.value.length

  await new Promise<void>((resolve) => {
    for (const node of mirrors.value) {
      probeSingleMirror(node, seq)
        .then((hit) => {
          if (seq !== probeSeq) return
          if (!settledWinner) {
            settledWinner = true
            bestMirror.value = hit
            probingMirrors.value = false
            // First healthy mirror to respond from the same launch timestamp has lowest RTT.
            cancelActiveProbes()
            resolve()
          }
        })
        .catch(() => {
          if (seq !== probeSeq) return
          pending -= 1
          if (pending <= 0 && !settledWinner) {
            probingMirrors.value = false
            probeFailed.value = true
            resolve()
          }
        })
    }
  })
}

const sourceOptions = computed(() =>
  sources.value.map((s) => ({ label: s.label, value: s.key }))
)

const availableItems = computed(() => {
  if (selectedSource.value === 'local') {
    return items.value.filter((it) => it.local_available)
  }
  return items.value
})

const osOptions = computed(() => {
  const seen = new Set<string>()
  const out: Array<{ label: string; value: string }> = []
  for (const it of availableItems.value) {
    if (!seen.has(it.os)) {
      seen.add(it.os)
      out.push({ label: it.os_label, value: it.os })
    }
  }
  return out
})

const archOptions = computed(() =>
  availableItems.value
    .filter((it) => it.os === selectedOS.value)
    .map((it) => ({ label: it.arch_label, value: it.arch }))
)

const currentItem = computed(
  () =>
    availableItems.value.find((it) => it.os === selectedOS.value && it.arch === selectedArch.value) ??
    availableItems.value.find((it) => it.os === selectedOS.value) ??
    availableItems.value[0] ??
    null
)

const downloadURL = computed(() => {
  if (!currentItem.value) return ''
  if (selectedSource.value === 'mirror') {
    const gh = currentItem.value.urls.github || ''
    const chosen = bestMirror.value?.node ?? mirrors.value[0]
    if (chosen && gh) {
      const prefix = chosen.prefix.endsWith('/') ? chosen.prefix : `${chosen.prefix}/`
      return `${prefix}${gh}`
    }
  }
  return currentItem.value.urls[selectedSource.value] || ''
})

const isExternalSource = computed(() => selectedSource.value !== 'local')

watch(selectedSource, (val) => {
  if (val) saveSource(val)
  if (osOptions.value.length > 0 && !osOptions.value.some((o) => o.value === selectedOS.value)) {
    selectedOS.value = osOptions.value[0].value
  }
  if (val === 'mirror') {
    void runMirrorSpeedTest(false)
  } else {
    cancelActiveProbes()
    probingMirrors.value = false
  }
})

watch(selectedOS, () => {
  const list = archOptions.value
  if (list.length > 0 && !list.some((o) => o.value === selectedArch.value)) {
    selectedArch.value = list[0].value
  }
})

onMounted(async () => {
  try {
    const res = await apiRequest<DownloadsPayload>('/downloads')
    sources.value = res.sources || []
    mirrors.value = res.mirrors || []
    items.value = res.items || []

    const saved = readSavedSource()
    if (saved && sources.value.some((s) => s.key === saved)) {
      selectedSource.value = saved
    } else if (res.default_source && sources.value.some((s) => s.key === res.default_source)) {
      selectedSource.value = res.default_source
    } else if (sources.value.length > 0) {
      selectedSource.value = sources.value[0].key
    }

    if (availableItems.value.length > 0) {
      const preferredOS = detectClientOS()
      selectedOS.value = availableItems.value.some((i) => i.os === preferredOS)
        ? preferredOS
        : availableItems.value[0].os
      const firstArch = availableItems.value.find((i) => i.os === selectedOS.value)
      selectedArch.value = firstArch ? firstArch.arch : ''
    }
  } catch {
    sources.value = []
    mirrors.value = []
    items.value = []
  } finally {
    loading.value = false
  }
})

onBeforeUnmount(() => {
  cancelActiveProbes()
})
</script>

<template>
  <section :class="props.compact ? 'dl-compact' : 'nfr-card dl-card'">
    <div v-if="!props.compact" class="dl-head">
      <h2 class="dl-title">客户端下载</h2>
      <span v-if="currentItem" class="dl-meta nfr-mono">
        {{ currentItem.filename }}
        <template v-if="currentItem.size_bytes && selectedSource === 'local'">
          · {{ formatBytes(currentItem.size_bytes) }}
        </template>
      </span>
    </div>

    <div v-if="loading" class="dl-loading">
      <NSkeleton text :repeat="2" />
    </div>

    <div v-else-if="availableItems.length === 0" class="dl-empty">
      暂无可用客户端下载包，请切换其他下载源。
    </div>

    <div v-else class="dl-controls" :class="{ compact: props.compact }">
      <NSelect
        v-model:value="selectedSource"
        :options="sourceOptions"
        placeholder="下载源"
        aria-label="选择客户端下载源"
        class="dl-select"
      />
      <NSelect
        v-model:value="selectedOS"
        :options="osOptions"
        placeholder="系统"
        aria-label="选择客户端操作系统"
        class="dl-select"
      />
      <NSelect
        v-model:value="selectedArch"
        :options="archOptions"
        placeholder="架构"
        aria-label="选择客户端 CPU 架构"
        class="dl-select"
      />
      <NButton
        v-if="currentItem && downloadURL"
        type="primary"
        tag="a"
        :href="downloadURL"
        :download="isExternalSource ? undefined : currentItem.filename"
        :target="isExternalSource ? '_blank' : undefined"
        :rel="isExternalSource ? 'noopener noreferrer' : undefined"
        class="nfr-touch-btn dl-btn"
      >
        <template #icon>
          <NIcon><DownloadOutline /></NIcon>
        </template>
        下载客户端
      </NButton>
    </div>

    <div v-if="currentItem" class="dl-hint-row">
      <span class="dl-hint">
        压缩包内为 <code class="nfr-mono">{{ currentItem.binary_name }}</code>，解压后直接运行即可
      </span>

      <span v-if="selectedSource === 'mirror'" class="dl-mirror-status">
        <template v-if="probingMirrors">正在测速优选镜像节点…</template>
        <template v-else-if="bestMirror">
          已优选 <span class="nfr-mono">{{ bestMirror.node.name }}</span>
          <span class="dl-rtt nfr-mono">{{ bestMirror.rttMs }}ms</span>
        </template>
        <template v-else-if="probeFailed">测速超时，已使用默认镜像</template>
        <NButton
          v-if="!probingMirrors && mirrors.length > 0"
          text
          size="tiny"
          class="dl-retest-btn"
          @click="runMirrorSpeedTest(true)"
        >
          <template #icon><NIcon :size="13"><RefreshOutline /></NIcon></template>
          重测
        </NButton>
      </span>
    </div>
  </section>
</template>

<style scoped>
.dl-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 14px;
}

.dl-title {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  color: var(--nfr-text);
}

.dl-meta {
  font-size: 12px;
  color: var(--nfr-text-secondary);
}

.dl-loading {
  padding: 6px 0;
}

.dl-empty {
  font-size: 13px;
  color: var(--nfr-text-secondary);
}

.dl-controls {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr) minmax(0, 1fr) auto;
  gap: 10px;
  align-items: center;
}

.dl-controls.compact {
  grid-template-columns: 1fr;
}

.dl-select {
  width: 100%;
}

.dl-btn {
  white-space: nowrap;
}

.dl-controls.compact .dl-btn {
  width: 100%;
}

.dl-hint-row {
  margin-top: 10px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px 14px;
  flex-wrap: wrap;
}

.dl-hint {
  font-size: 12px;
  color: var(--nfr-text-secondary);
}

.dl-mirror-status {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: var(--nfr-text-secondary);
  flex-wrap: wrap;
}

.dl-rtt {
  padding: 1px 6px;
  border-radius: 5px;
  background: var(--nfr-online-bg);
  color: var(--nfr-online-text);
  font-size: 11px;
  font-weight: 500;
}

.dl-retest-btn {
  font-size: 12px;
}

@media (max-width: 767px) {
  .dl-controls {
    grid-template-columns: 1fr;
  }

  .dl-btn {
    width: 100%;
  }
}
</style>

import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { apiRequest } from '@/api/client'
import type {
  ActivityEvent,
  DashboardResponse,
  DashboardSettingsSummary,
  InstanceInfo,
  NodeItem,
  ProbeResult,
  RatePoint,
  Tunnel,
  UserProfile,
} from '@/types/api'

export type ConnectionState = 'live' | 'reconnecting' | 'disconnected'

const BACKOFF_MS = [1000, 2000, 4000, 8000, 15000]

export const useDashboardStore = defineStore('dashboard', () => {
  const info = ref<InstanceInfo | null>(null)
  const user = ref<UserProfile | null>(null)
  const tunnels = ref<Tunnel[]>([])
  const nodes = ref<NodeItem[]>([])
  const events = ref<ActivityEvent[]>([])
  const rates = ref<RatePoint[]>([])
  const settings = ref<DashboardSettingsSummary | null>(null)

  const loading = ref(false)
  const initialized = ref(false)
  const error = ref('')

  const sseOpen = ref(false)
  const consecutiveFailures = ref(0)
  const unreadAlerts = ref(0)

  const probeResults = ref<Record<string, ProbeResult>>({})
  const probingIds = ref<Record<string, boolean>>({})
  const probingAll = ref(false)
  const probeSummary = ref<{ total: number; ok: number; failed: number; other: number } | null>(null)

  let eventSource: EventSource | null = null
  let reconnectTimer: number | null = null
  let reconnectStep = 0
  let pollTimer: number | null = null
  let sseRefreshTimer: number | null = null
  let started = false

  const connectionState = computed<ConnectionState>(() => {
    if (consecutiveFailures.value >= 3) return 'disconnected'
    if (sseOpen.value && consecutiveFailures.value === 0) return 'live'
    return 'reconnecting'
  })

  const onlineNodeCount = computed(() => nodes.value.filter((n) => n.online).length)
  const runningTunnelCount = computed(() => tunnels.value.filter((t) => t.enabled && t.online).length)
  const totalInRate = computed(() => tunnels.value.reduce((acc, t) => acc + (t.in_rate || 0), 0))
  const totalOutRate = computed(() => tunnels.value.reduce((acc, t) => acc + (t.out_rate || 0), 0))
  const totalInBytes = computed(() => user.value?.total_in_bytes ?? 0)
  const totalOutBytes = computed(() => user.value?.total_out_bytes ?? 0)
  const totalTrafficBytes = computed(() => totalInBytes.value + totalOutBytes.value)

  async function fetchDashboard(silent = false) {
    if (!silent && !initialized.value) {
      loading.value = true
    }
    try {
      const data = await apiRequest<DashboardResponse>('/dashboard')
      info.value = data.info
      user.value = data.user || null
      tunnels.value = data.tunnels || []
      nodes.value = data.nodes || []
      events.value = (data.events || []).slice().reverse()
      rates.value = data.rates || []
      settings.value = data.settings
      error.value = ''
      initialized.value = true
      consecutiveFailures.value = 0
    } catch (err) {
      consecutiveFailures.value += 1
      if (!initialized.value) {
        error.value = err instanceof Error ? err.message : '加载仪表盘数据失败'
      }
    } finally {
      loading.value = false
    }
  }

  function connectSSE() {
    if (!started || eventSource) return
    const es = new EventSource('/nfr/api/events', { withCredentials: true })
    eventSource = es

    es.onopen = () => {
      const wasReconnect = reconnectStep > 0
      sseOpen.value = true
      reconnectStep = 0
      consecutiveFailures.value = 0
      if (wasReconnect) {
        void fetchDashboard(true)
      }
    }

    const handleEvent = (ev: MessageEvent) => {
      try {
        const parsed = JSON.parse(ev.data) as ActivityEvent
        if (parsed && parsed.type) {
          const exists = events.value.some(
            (e) => e.at === parsed.at && e.type === parsed.type && e.message === parsed.message
          )
          if (!exists) {
            events.value = [parsed, ...events.value].slice(0, 100)
            if (parsed.type.includes('down') || parsed.type.includes('offline') || parsed.type.includes('quota')) {
              unreadAlerts.value += 1
            }
          }
        }
      } catch {
        // Ignore keepalive comments or non-JSON frames
      }
      if (sseRefreshTimer !== null) window.clearTimeout(sseRefreshTimer)
      sseRefreshTimer = window.setTimeout(() => {
        sseRefreshTimer = null
        if (started) void fetchDashboard(true)
      }, 150)
    }

    for (const evType of [
      'change',
      'node-joined',
      'node-online',
      'node-offline',
      'tunnel-bound',
      'tunnel-target-down',
      'config-changed',
      'quota-paused',
      'message',
    ]) {
      es.addEventListener(evType, handleEvent as EventListener)
    }

    es.onerror = () => {
      sseOpen.value = false
      es.close()
      eventSource = null
      consecutiveFailures.value += 1
      if (!started) return
      const delay = BACKOFF_MS[Math.min(reconnectStep, BACKOFF_MS.length - 1)]
      reconnectStep = Math.min(reconnectStep + 1, BACKOFF_MS.length - 1)
      if (reconnectTimer !== null) window.clearTimeout(reconnectTimer)
      reconnectTimer = window.setTimeout(() => {
        reconnectTimer = null
        connectSSE()
      }, delay)
    }
  }

  function schedulePoll() {
    if (pollTimer !== null) window.clearTimeout(pollTimer)
    if (!started) return
    const interval = document.hidden ? 15000 : 4000
    pollTimer = window.setTimeout(async () => {
      await fetchDashboard(true)
      schedulePoll()
    }, interval)
  }

  function onVisibilityChange() {
    if (!started) return
    if (!document.hidden) {
      void fetchDashboard(true)
    }
    schedulePoll()
  }

  function startRealtime() {
    if (started) return
    started = true
    void fetchDashboard(false)
    connectSSE()
    schedulePoll()
    document.addEventListener('visibilitychange', onVisibilityChange)
  }

  function stopRealtime() {
    started = false
    sseOpen.value = false
    if (eventSource) {
      eventSource.close()
      eventSource = null
    }
    if (reconnectTimer !== null) {
      window.clearTimeout(reconnectTimer)
      reconnectTimer = null
    }
    if (pollTimer !== null) {
      window.clearTimeout(pollTimer)
      pollTimer = null
    }
    if (sseRefreshTimer !== null) {
      window.clearTimeout(sseRefreshTimer)
      sseRefreshTimer = null
    }
    document.removeEventListener('visibilitychange', onVisibilityChange)
  }

  function clearUnreadAlerts() {
    unreadAlerts.value = 0
  }

  function upsertTunnel(updated: Tunnel) {
    const idx = tunnels.value.findIndex((t) => t.id === updated.id)
    if (idx >= 0) {
      const next = tunnels.value.slice()
      next[idx] = updated
      tunnels.value = next
    } else {
      tunnels.value = [...tunnels.value, updated]
    }
  }

  async function probeTunnel(tunnelID: string): Promise<ProbeResult> {
    probingIds.value = { ...probingIds.value, [tunnelID]: true }
    try {
      const res = await apiRequest<ProbeResult>(`/tunnels/${encodeURIComponent(tunnelID)}/probe`, {
        method: 'POST',
      })
      probeResults.value = { ...probeResults.value, [tunnelID]: res }
      void fetchDashboard(true)
      return res
    } finally {
      const next = { ...probingIds.value }
      delete next[tunnelID]
      probingIds.value = next
    }
  }

  async function probeAllTunnels(): Promise<ProbeResult[]> {
    probingAll.value = true
    try {
      const data = await apiRequest<{ results: ProbeResult[] }>('/tunnels/probe-all', {
        method: 'POST',
      })
      const list = data.results || []
      const nextMap = { ...probeResults.value }
      let ok = 0
      let failed = 0
      let other = 0
      for (const item of list) {
        nextMap[item.tunnel_id] = item
        if (item.status === 'ok') ok++
        else if (item.status === 'failed' || item.status === 'timeout') failed++
        else other++
      }
      probeResults.value = nextMap
      probeSummary.value = { total: list.length, ok, failed, other }
      void fetchDashboard(true)
      return list
    } finally {
      probingAll.value = false
    }
  }

  function clearProbeSummary() {
    probeSummary.value = null
  }

  function reset() {
    stopRealtime()
    info.value = null
    user.value = null
    tunnels.value = []
    nodes.value = []
    events.value = []
    rates.value = []
    settings.value = null
    initialized.value = false
    error.value = ''
    unreadAlerts.value = 0
    probeResults.value = {}
    probingIds.value = {}
    probeSummary.value = null
  }

  return {
    info,
    user,
    tunnels,
    nodes,
    events,
    rates,
    settings,
    loading,
    initialized,
    error,
    connectionState,
    unreadAlerts,
    probeResults,
    probingIds,
    probingAll,
    probeSummary,
    onlineNodeCount,
    runningTunnelCount,
    totalInRate,
    totalOutRate,
    totalInBytes,
    totalOutBytes,
    totalTrafficBytes,
    fetchDashboard,
    startRealtime,
    stopRealtime,
    reset,
    clearUnreadAlerts,
    upsertTunnel,
    probeTunnel,
    probeAllTunnels,
    clearProbeSummary,
  }
})

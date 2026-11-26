import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { apiRequest } from '@/api/client'
import type { UpdateMirrorProbe, UpdateStatus, UpgradeProgress } from '@/types/api'

const DISMISSED_VER_KEY = 'nfr_dismissed_update_version'
const UPDATE_SOURCE_KEY = 'nfr_update_source'
const UPGRADED_NOTICE_KEY = 'nfr_upgraded_to_version'

function saveUpgradedNotice(ver: string) {
  try {
    window.sessionStorage.setItem(UPGRADED_NOTICE_KEY, ver)
  } catch {
    try {
      window.localStorage.setItem(UPGRADED_NOTICE_KEY, ver)
    } catch {
      // Ignore storage errors
    }
  }
}

function consumeUpgradedNotice(): string {
  try {
    const val = window.sessionStorage.getItem(UPGRADED_NOTICE_KEY)
    if (val) {
      window.sessionStorage.removeItem(UPGRADED_NOTICE_KEY)
      return val
    }
  } catch {
    // Ignore storage errors
  }
  try {
    const val = window.localStorage.getItem(UPGRADED_NOTICE_KEY)
    if (val) {
      window.localStorage.removeItem(UPGRADED_NOTICE_KEY)
      return val
    }
  } catch {
    // Ignore storage errors
  }
  return ''
}

function readSavedSource(): 'github' | 'mirror' {
  try {
    return window.localStorage.getItem(UPDATE_SOURCE_KEY) === 'mirror' ? 'mirror' : 'github'
  } catch {
    return 'github'
  }
}

function saveSource(val: 'github' | 'mirror') {
  try {
    window.localStorage.setItem(UPDATE_SOURCE_KEY, val)
  } catch {
    // Ignore storage errors
  }
}

function readDismissedVersion(): string {
  try {
    return window.localStorage.getItem(DISMISSED_VER_KEY) || ''
  } catch {
    return ''
  }
}

function saveDismissedVersion(ver: string) {
  try {
    window.localStorage.setItem(DISMISSED_VER_KEY, ver)
  } catch {
    // Ignore storage errors
  }
}

export const useUpdateStore = defineStore('update', () => {
  const status = ref<UpdateStatus | null>(null)
  const loading = ref(false)
  const checking = ref(false)
  const savingPrefs = ref(false)
  const modalOpen = ref(false)

  const selectedSource = ref<'github' | 'mirror'>(readSavedSource())
  const probingMirrors = ref(false)
  const mirrorProbe = ref<UpdateMirrorProbe | null>(null)

  const upgrading = ref(false)
  const upgradeProgress = ref<UpgradeProgress>({ state: 'idle', percent: 0 })

  let pollTimer: number | null = null
  let autoPopupShown = false

  const hasUpdate = computed(() => Boolean(status.value?.has_update && status.value?.latest_version))

  function setSource(val: 'github' | 'mirror') {
    selectedSource.value = val
    saveSource(val)
    if (val === 'mirror' && !mirrorProbe.value) {
      void probeMirrors(false)
    }
  }

  function clearPoll() {
    if (pollTimer !== null) {
      window.clearTimeout(pollTimer)
      pollTimer = null
    }
  }

  async function fetchStatus(autoOpenModal = false): Promise<UpdateStatus | null> {
    loading.value = true
    try {
      const data = await apiRequest<UpdateStatus>('/update')
      status.value = data
      if (data.upgrade) {
        upgradeProgress.value = data.upgrade
        if (
          data.upgrade.state === 'downloading' ||
          data.upgrade.state === 'verifying' ||
          data.upgrade.state === 'installing' ||
          data.upgrade.state === 'restarting'
        ) {
          upgrading.value = true
          modalOpen.value = true
          scheduleProgressPoll(data.upgrade.target_version || data.latest_version || '')
        }
      }
      if (
        autoOpenModal &&
        !autoPopupShown &&
        data.has_update &&
        data.latest_version &&
        readDismissedVersion() !== data.latest_version
      ) {
        autoPopupShown = true
        modalOpen.value = true
        if (selectedSource.value === 'mirror' && !mirrorProbe.value) {
          void probeMirrors(false)
        }
      }
      return data
    } catch {
      return null
    } finally {
      loading.value = false
    }
  }

  async function probeMirrors(force = false): Promise<UpdateMirrorProbe | null> {
    if (probingMirrors.value) return mirrorProbe.value
    probingMirrors.value = true
    try {
      const res = await apiRequest<UpdateMirrorProbe>('/update/mirrors', {
        method: 'POST',
        body: { force },
      })
      mirrorProbe.value = res
      return res
    } catch {
      return null
    } finally {
      probingMirrors.value = false
    }
  }

  async function checkNow(openModalOnUpdate = true): Promise<UpdateStatus> {
    checking.value = true
    try {
      const data = await apiRequest<UpdateStatus>('/update/check', {
        method: 'POST',
        body: {
          source: selectedSource.value,
          channel: status.value?.channel || 'auto',
        },
      })
      status.value = data
      if (openModalOnUpdate && data.has_update && data.latest_version) {
        modalOpen.value = true
        if (selectedSource.value === 'mirror' && !mirrorProbe.value) {
          void probeMirrors(false)
        }
      }
      return data
    } finally {
      checking.value = false
    }
  }

  async function savePreferences(autoCheck: boolean, channel?: string): Promise<UpdateStatus> {
    savingPrefs.value = true
    try {
      const data = await apiRequest<UpdateStatus>('/update/settings', {
        method: 'PATCH',
        body: {
          auto_check: autoCheck,
          channel: channel || status.value?.channel || 'auto',
        },
      })
      status.value = data
      return data
    } finally {
      savingPrefs.value = false
    }
  }

  function openModal() {
    modalOpen.value = true
    if (selectedSource.value === 'mirror' && !mirrorProbe.value) {
      void probeMirrors(false)
    }
  }

  function dismissModal() {
    if (upgrading.value) return
    if (status.value?.latest_version) {
      saveDismissedVersion(status.value.latest_version)
    }
    modalOpen.value = false
  }

  async function startUpgrade(): Promise<void> {
    if (upgrading.value || !status.value?.latest_version) return
    const target = status.value.latest_version
    upgrading.value = true
    upgradeProgress.value = {
      state: 'downloading',
      percent: 5,
      target_version: target,
      source: selectedSource.value,
      message: '正在启动更新任务…',
    }
    try {
      const res = await apiRequest<UpgradeProgress>('/update/apply', {
        method: 'POST',
        body: {
          source: selectedSource.value,
          mirror_prefix: selectedSource.value === 'mirror' ? mirrorProbe.value?.mirror?.prefix || '' : '',
          target_version: target,
        },
      })
      upgradeProgress.value = res
      scheduleProgressPoll(target)
    } catch (err) {
      upgrading.value = false
      const msg = err instanceof Error ? err.message : '启动升级失败'
      upgradeProgress.value = {
        state: 'error',
        percent: 0,
        target_version: target,
        message: msg,
        error: msg,
      }
      throw err
    }
  }

  function scheduleProgressPoll(targetVersion: string, restartStartedAt = 0) {
    clearPoll()
    pollTimer = window.setTimeout(async () => {
      try {
        if (restartStartedAt > 0) {
          // Wait for new nfrs process to come back up and report its version.
          const me = await apiRequest<{ authenticated: boolean; version?: string }>('/auth/me', {
            skipAuthRedirect: true,
          })
          if (me.version === targetVersion || Date.now() - restartStartedAt > 12000) {
            const finalVer = me.version || targetVersion
            upgrading.value = false
            upgradeProgress.value = {
              state: 'done',
              percent: 100,
              target_version: finalVer,
              message: `升级完成（${finalVer}），正在刷新页面…`,
            }
            saveUpgradedNotice(finalVer)
            window.setTimeout(() => window.location.reload(), 600)
            return
          }
          scheduleProgressPoll(targetVersion, restartStartedAt)
          return
        }

        const prog = await apiRequest<UpgradeProgress>('/update/progress', {
          skipAuthRedirect: true,
        })
        upgradeProgress.value = prog
        if (prog.state === 'error') {
          upgrading.value = false
          return
        }
        if (prog.state === 'restarting' || prog.state === 'done') {
          scheduleProgressPoll(targetVersion, Date.now())
          return
        }
        scheduleProgressPoll(targetVersion)
      } catch {
        // Expected brief disconnect while nfrs restarts with the new binary.
        const since = restartStartedAt > 0 ? restartStartedAt : Date.now()
        if (Date.now() - since > 90000) {
          upgrading.value = false
          upgradeProgress.value = {
            state: 'error',
            percent: 100,
            target_version: targetVersion,
            message: '等待服务重启超时，请手动刷新页面检查版本状态',
            error: 'timeout',
          }
          return
        }
        upgradeProgress.value = {
          state: 'restarting',
          percent: 100,
          target_version: targetVersion,
          message: '服务端正在重启切换新版本，等待恢复连接…',
        }
        scheduleProgressPoll(targetVersion, since)
      }
    }, restartStartedAt > 0 ? 1200 : 500)
  }

  function resetUpgradeError() {
    if (!upgrading.value) {
      upgradeProgress.value = { state: 'idle', percent: 0 }
    }
  }

  return {
    status,
    loading,
    checking,
    savingPrefs,
    modalOpen,
    selectedSource,
    probingMirrors,
    mirrorProbe,
    upgrading,
    upgradeProgress,
    hasUpdate,
    setSource,
    fetchStatus,
    probeMirrors,
    checkNow,
    savePreferences,
    openModal,
    dismissModal,
    startUpgrade,
    resetUpgradeError,
    consumeUpgradedNotice,
  }
})

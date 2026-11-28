import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { apiRequest, setCSRFToken } from '@/api/client'
import type { DBStatus, InstanceInfo, UserProfile } from '@/types/api'

interface AuthResponse {
  authenticated: boolean
  csrf?: string
  user?: UserProfile
  db?: DBStatus
  info?: InstanceInfo
  version?: string
}

const REMEMBER_FLAG_KEY = 'nfr_login_remember'
const LEGACY_CRED_KEY = 'nfr_login_saved_pw'

function scrubLegacySavedPassword(): void {
  try {
    window.localStorage.removeItem(LEGACY_CRED_KEY)
  } catch {
    // Ignore storage restrictions
  }
}

export function getRememberPreference(): boolean {
  scrubLegacySavedPassword()
  try {
    return window.localStorage.getItem(REMEMBER_FLAG_KEY) === '1'
  } catch {
    return false
  }
}

export function setRememberPreference(remember: boolean): void {
  scrubLegacySavedPassword()
  try {
    if (remember) {
      window.localStorage.setItem(REMEMBER_FLAG_KEY, '1')
    } else {
      window.localStorage.removeItem(REMEMBER_FLAG_KEY)
    }
  } catch {
    // Ignore storage restrictions
  }
}

export const useAuthStore = defineStore('auth', () => {
  const checked = ref(false)
  const authenticated = ref(false)
  const user = ref<UserProfile | null>(null)
  const dbStatus = ref<DBStatus | null>(null)
  const info = ref<InstanceInfo | null>(null)
  const version = ref('')

  const isAdmin = computed(() => user.value?.role === 'admin')
  const requiresDbSetup = computed(
    () => authenticated.value && isAdmin.value && !!dbStatus.value?.mandatory_setup,
  )

  async function checkAuth(): Promise<boolean> {
    try {
      const res = await apiRequest<AuthResponse>('/auth/me', { skipAuthRedirect: true })
      authenticated.value = !!res.authenticated
      version.value = res.version || ''
      if (res.authenticated) {
        setCSRFToken(res.csrf || '')
        user.value = res.user || null
        dbStatus.value = res.db || null
        info.value = res.info || null
      } else {
        setCSRFToken('')
        user.value = null
        dbStatus.value = null
        info.value = null
      }
    } catch {
      authenticated.value = false
      setCSRFToken('')
      user.value = null
      dbStatus.value = null
    } finally {
      checked.value = true
    }
    return authenticated.value
  }

  async function ensureChecked(): Promise<boolean> {
    if (checked.value) return authenticated.value
    return checkAuth()
  }

  async function login(username: string, password: string, remember = false): Promise<void> {
    const res = await apiRequest<AuthResponse>('/auth/login', {
      method: 'POST',
      body: { username: username.trim(), password, remember },
      skipAuthRedirect: true,
    })
    authenticated.value = !!res.authenticated
    setCSRFToken(res.csrf || '')
    user.value = res.user || null
    dbStatus.value = res.db || null
    info.value = res.info || null
    version.value = res.version || ''
    checked.value = true
  }

  async function refreshProfile(): Promise<void> {
    if (!authenticated.value) return
    const res = await apiRequest<{ user: UserProfile }>('/profile')
    user.value = res.user
  }

  function setDBStatus(next: DBStatus) {
    dbStatus.value = next
  }

  async function logout(): Promise<void> {
    try {
      await apiRequest('/auth/logout', { method: 'POST', skipAuthRedirect: true })
    } finally {
      clearSession()
    }
  }

  function clearSession() {
    authenticated.value = false
    setCSRFToken('')
    user.value = null
    dbStatus.value = null
    info.value = null
  }

  return {
    checked,
    authenticated,
    user,
    isAdmin,
    dbStatus,
    requiresDbSetup,
    info,
    version,
    checkAuth,
    ensureChecked,
    login,
    refreshProfile,
    setDBStatus,
    logout,
    clearSession,
  }
})

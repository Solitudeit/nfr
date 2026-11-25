import { ref } from 'vue'
import { defineStore } from 'pinia'
import { apiRequest, setCSRFToken } from '@/api/client'
import type { InstanceInfo } from '@/types/api'

interface AuthResponse {
  authenticated: boolean
  csrf?: string
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
  const info = ref<InstanceInfo | null>(null)
  const version = ref('')

  async function checkAuth(): Promise<boolean> {
    try {
      const res = await apiRequest<AuthResponse>('/auth/me', { skipAuthRedirect: true })
      authenticated.value = !!res.authenticated
      version.value = res.version || ''
      if (res.authenticated) {
        setCSRFToken(res.csrf || '')
        info.value = res.info || null
      } else {
        setCSRFToken('')
        info.value = null
      }
    } catch {
      authenticated.value = false
      setCSRFToken('')
    } finally {
      checked.value = true
    }
    return authenticated.value
  }

  async function ensureChecked(): Promise<boolean> {
    if (checked.value) return authenticated.value
    return checkAuth()
  }

  async function login(password: string, remember = false): Promise<void> {
    const res = await apiRequest<AuthResponse>('/auth/login', {
      method: 'POST',
      body: { password, remember },
      skipAuthRedirect: true,
    })
    authenticated.value = !!res.authenticated
    setCSRFToken(res.csrf || '')
    info.value = res.info || null
    version.value = res.version || ''
    checked.value = true
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
    info.value = null
  }

  return {
    checked,
    authenticated,
    info,
    version,
    checkAuth,
    ensureChecked,
    login,
    logout,
    clearSession,
  }
})

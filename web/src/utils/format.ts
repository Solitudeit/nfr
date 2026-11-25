import type { Tunnel } from '@/types/api'

export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let idx = 0
  while (v >= 1024 && idx < units.length - 1) {
    v /= 1024
    idx++
  }
  const digits = v >= 100 || idx === 0 ? 0 : 1
  return `${v.toFixed(digits)} ${units[idx]}`
}

export function formatRate(bytesPerSec: number): string {
  return `${formatBytes(bytesPerSec)}/s`
}

export function formatRelativeTime(iso?: string): string {
  if (!iso) return '从未'
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return iso
  const diffSec = Math.max(0, Math.floor((Date.now() - t) / 1000))
  if (diffSec < 10) return '刚刚'
  if (diffSec < 60) return `${diffSec} 秒前`
  if (diffSec < 3600) return `${Math.floor(diffSec / 60)} 分钟前`
  if (diffSec < 86400) return `${Math.floor(diffSec / 3600)} 小时前`
  return `${Math.floor(diffSec / 86400)} 天前`
}

export function formatClock(ts: number | string): string {
  const d = typeof ts === 'number' ? new Date(ts * 1000) : new Date(ts)
  if (Number.isNaN(d.getTime())) return ''
  return d.toLocaleTimeString('zh-CN', { hour12: false })
}

export function tunnelPublicAddress(t: Tunnel): string {
  if (t.type === 'http') {
    if (t.hosts && t.hosts.length > 0) {
      return `http://${t.hosts[0]}`
    }
    if (t.public_host && t.remote_port > 0) {
      return `http://${t.public_host}:${t.remote_port}`
    }
    if (t.remote_port > 0) {
      return `:${t.remote_port}`
    }
    return '待配置域名或端口'
  }
  if (t.public_host && t.remote_port > 0) {
    return `${t.public_host}:${t.remote_port}`
  }
  if (t.remote_port > 0) {
    return `:${t.remote_port}`
  }
  return '未分配端口'
}

export function tunnelHttpHref(t: Tunnel): string | null {
  if (t.type !== 'http') return null
  if (t.hosts && t.hosts.length > 0) return `http://${t.hosts[0]}`
  if (t.public_host && t.remote_port > 0) return `http://${t.public_host}:${t.remote_port}`
  return null
}

export async function copyToClipboard(text: string): Promise<boolean> {
  if (!text) return false

  // Synchronous fallback helper so plain-HTTP mobile browsers preserve user gesture
  // and mount inside active modal/drawer containers to avoid Naive UI focus-trap stealing focus.
  const execFallback = (): boolean => {
    try {
      const activeEl = document.activeElement as HTMLElement | null
      const container =
        (activeEl?.closest?.('.n-modal, .n-drawer, [role="dialog"]') as HTMLElement | null) ||
        (document.querySelector('.n-modal, .n-drawer') as HTMLElement | null) ||
        document.body

      const el = document.createElement('textarea')
      el.value = text
      el.setAttribute('inputmode', 'none')
      el.setAttribute('aria-hidden', 'true')
      el.style.position = 'fixed'
      el.style.top = '0'
      el.style.left = '0'
      el.style.width = '2em'
      el.style.height = '2em'
      el.style.padding = '0'
      el.style.border = 'none'
      el.style.outline = 'none'
      el.style.boxShadow = 'none'
      el.style.background = 'transparent'
      el.style.opacity = '0.01'
      el.style.fontSize = '16px'
      el.style.zIndex = '99999'

      container.appendChild(el)
      el.focus({ preventScroll: true })
      el.select()
      el.setSelectionRange(0, text.length)

      const ok = document.execCommand('copy')
      container.removeChild(el)
      activeEl?.focus?.({ preventScroll: true })
      return ok
    } catch {
      return false
    }
  }

  if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function' && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      return execFallback()
    }
  }

  return execFallback()
}

export function downloadTextFile(filename: string, content: string) {
  const blob = new Blob([content], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}

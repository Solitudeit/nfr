export class APIError extends Error {
  status: number
  retryAfterSeconds?: number

  constructor(status: number, message: string, retryAfterSeconds?: number) {
    super(message)
    this.status = status
    this.retryAfterSeconds = retryAfterSeconds
  }
}

let csrfToken = ''
let unauthorizedHandler: (() => void) | null = null

export function setCSRFToken(token: string) {
  csrfToken = token
}

export function setUnauthorizedHandler(fn: () => void) {
  unauthorizedHandler = fn
}

export async function apiRequest<T>(
  path: string,
  options: {
    method?: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE'
    body?: unknown
    skipAuthRedirect?: boolean
  } = {}
): Promise<T> {
  const method = options.method ?? 'GET'
  const headers: Record<string, string> = {
    Accept: 'application/json',
  }
  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json'
  }
  if (method !== 'GET' && csrfToken) {
    headers['X-CSRF'] = csrfToken
  }

  const ctrl = typeof AbortController !== 'undefined' ? new AbortController() : null
  const timer = ctrl ? setTimeout(() => ctrl.abort(), 15000) : null
  let res: Response
  try {
    res = await fetch(`/nfr/api${path}`, {
      method,
      headers,
      credentials: 'same-origin',
      cache: 'no-store',
      body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
      signal: ctrl ? ctrl.signal : undefined,
    })
  } catch (err) {
    if (err instanceof Error && err.name === 'AbortError') {
      throw new APIError(0, '请求超时，请检查网络后重试')
    }
    throw err
  } finally {
    if (timer) clearTimeout(timer)
  }

  const text = await res.text()
  let data: Record<string, unknown> = {}
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = { error: text }
    }
  }

  if (!res.ok) {
    if (res.status === 401 && !options.skipAuthRedirect && unauthorizedHandler) {
      unauthorizedHandler()
    }
    const msg = typeof data.error === 'string' && data.error ? data.error : `请求失败 (${res.status})`
    const retry = typeof data.retry_after_seconds === 'number' ? data.retry_after_seconds : undefined
    throw new APIError(res.status, msg, retry)
  }

  return data as T
}

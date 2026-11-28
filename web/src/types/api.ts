export interface InstanceInfo {
  id: string
  name: string
  version: string
  revision: number
  domain: string
  port_range: [number, number]
  api_enabled: boolean
  control_addr: string
  panel_url: string
  pin: string
  started_at: string
  nodes: number
  tunnels: number
}

export interface Tunnel {
  id: string
  name: string
  node_id: string
  node_name?: string
  type: 'tcp' | 'udp' | 'http' | string
  local: string
  remote_port: number
  hosts?: string[]
  enabled: boolean
  bandwidth_kb?: number
  quota_mb?: number
  used_bytes?: number
  quota_paused?: boolean
  online: boolean
  node_missing?: boolean
  awaiting?: boolean
  target_down?: boolean
  public_host?: string
  status: string
  status_kind: 'on' | 'idle' | 'warn' | 'err' | 'off' | string
  in: number
  out: number
  conns: number
  active: number
  errs: number
  in_rate: number
  out_rate: number
  created_at: string
  updated_at: string
}

export interface NodeItem {
  id: string
  name: string
  os?: string
  arch?: string
  version?: string
  online: boolean
  region?: string
  remote_addr?: string
  last_seen?: string
  tunnels: number
  created_at: string
}

export interface ActivityEvent {
  type: string
  at: string
  message: string
}

export interface RatePoint {
  ts: number
  in_rate: number
  out_rate: number
}

export interface DashboardSettingsSummary {
  domain: string
  public_host: string
  port_range: [number, number]
  panel_https: boolean
  join_addr: string
}

export interface DashboardResponse {
  info: InstanceInfo
  tunnels: Tunnel[]
  nodes: NodeItem[]
  events: ActivityEvent[]
  rates: RatePoint[]
  settings: DashboardSettingsSummary
}

export interface ProbeResult {
  tunnel_id: string
  status: 'ok' | 'failed' | 'offline' | 'disabled' | 'awaiting' | 'unsupported' | 'busy' | 'timeout'
  rtt_ms?: number
  local_ms?: number
  message?: string
}

export interface TunnelRequest {
  name: string
  node_id: string
  type: string
  local: string
  remote_port: number
  hosts?: string[]
  enabled?: boolean
  lease_id?: string
  bandwidth_kb?: number
  quota_mb?: number
}

export interface AllocateResponse {
  port: number
  lease_id: string
  expires_at: string
}

export interface JoinCodeResponse {
  code: string
  expires_at: string
  command: string
}

export interface NodeConfigBundle {
  node_id: string
  name: string
  online: boolean
  toml: string
  yaml: string
}

export interface ConfigsPayload {
  server_addr: string
  auth_token: string
  pin: string
  ca: boolean
  domain: string
  public_host: string
  panel_https: boolean
  server_toml: string
  server_yaml: string
  template: NodeConfigBundle
  nodes: NodeConfigBundle[]
}

export interface APIKeyItem {
  id: string
  label: string
  scope: string
  prefix?: string
  created_at: string
  last_used?: string
}

export interface SettingsPayload {
  name: string
  domain: string
  public_host: string
  port_range: [number, number]
  email: string
  api_enabled: boolean
  auth_token: string
  auth_token_from_config: boolean
  panel_https: boolean
  panel_url: string
  control_addr: string
  pin: string
  cert_mode: string
  cert_source: string
  cert_expiry: string
  keys: APIKeyItem[]
}

export interface TunnelConfigResponse {
  tunnel_id: string
  name: string
  filename_toml: string
  filename_yaml: string
  toml: string
  yaml: string
  run_hint: string
  quick_token?: string
  quick_cmd_unix?: string
  quick_cmd_win?: string
}

export interface KickResponse {
  ok: boolean
  kicked: boolean
  message: string
}

export interface DownloadSource {
  key: string
  label: string
  base_url: string
}

export interface DownloadItem {
  os: string
  os_label: string
  arch: string
  arch_label: string
  filename: string
  binary_name: string
  urls: Record<string, string>
}

export interface MirrorNode {
  id: string
  name: string
  prefix: string
  probe_url: string
}

export interface DownloadsPayload {
  default_source: string
  sources: DownloadSource[]
  mirrors?: MirrorNode[]
  checksum_urls?: Record<string, string>
  items: DownloadItem[]
}

export interface UpdateMirrorProbe {
  ok: boolean
  mirror?: MirrorNode
  rtt_ms?: number
  message?: string
}

export interface UpgradeProgress {
  state: 'idle' | 'downloading' | 'verifying' | 'installing' | 'restarting' | 'done' | 'error' | string
  percent: number
  target_version?: string
  source?: string
  checksum_source?: 'official' | 'mirror' | string
  message?: string
  error?: string
}

export interface UpdateStatus {
  current_version: string
  latest_version?: string
  has_update: boolean
  is_prerelease?: boolean
  published_at?: string
  notes?: string
  release_url?: string
  checked_at?: string
  last_error?: string
  auto_check: boolean
  channel: 'auto' | 'preview' | 'stable' | string
  can_self_update: boolean
  cannot_update_reason?: string
  source_used?: string
  mirror_used?: string
  mirrors: MirrorNode[]
  upgrade: UpgradeProgress
}

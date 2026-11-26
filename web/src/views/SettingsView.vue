<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NAlert,
  NButton,
  NForm,
  NFormItem,
  NIcon,
  NInput,
  NInputNumber,
  NRadioButton,
  NRadioGroup,
  NSelect,
  NSkeleton,
  NSwitch,
  NTabPane,
  NTabs,
  NTag,
  useDialog,
  useMessage,
} from 'naive-ui'
import {
  CloudDownloadOutline,
  CopyOutline,
  KeyOutline,
  LogOutOutline,
  RefreshOutline,
  TrashOutline,
} from '@vicons/ionicons5'
import { apiRequest } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { PRIMARY_PRESETS, useThemeStore } from '@/stores/theme'
import { useUpdateStore } from '@/stores/update'
import type { SettingsPayload } from '@/types/api'
import { copyToClipboard, formatRelativeTime } from '@/utils/format'

const router = useRouter()
const message = useMessage()
const dialog = useDialog()
const authStore = useAuthStore()
const dashStore = useDashboardStore()
const themeStore = useThemeStore()
const updateStore = useUpdateStore()

const loading = ref(true)
const savingBasic = ref(false)
const savingToken = ref(false)
const togglingAPI = ref(false)
const creatingKey = ref(false)
const changingPw = ref(false)

const settings = ref<SettingsPayload | null>(null)
const newlyCreatedKey = ref('')

const basicForm = reactive({
  name: '',
  public_host: '',
  domain: '',
  port_low: 20000,
  port_high: 29999,
  email: '',
})

const authTokenInput = ref('')

const keyForm = reactive({
  label: '',
  scope: 'read',
})

const pwForm = reactive({
  old_password: '',
  new_password: '',
  confirm_password: '',
})

const scopeOptions = [
  { label: '只读 (read)', value: 'read' },
  { label: '读写 (write)', value: 'write' },
  { label: '管理员 (admin)', value: 'admin' },
]

async function fetchSettings() {
  loading.value = true
  try {
    const data = await apiRequest<SettingsPayload>('/settings')
    settings.value = data
    basicForm.name = data.name || ''
    basicForm.public_host = data.public_host || ''
    basicForm.domain = data.domain || ''
    basicForm.port_low = data.port_range?.[0] ?? 20000
    basicForm.port_high = data.port_range?.[1] ?? 29999
    basicForm.email = data.email || ''
    authTokenInput.value = data.auth_token || ''
  } catch (err) {
    message.error(err instanceof Error ? err.message : '加载设置失败')
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void fetchSettings()
  void updateStore.fetchStatus(false)
})

async function handleCheckUpdate() {
  try {
    const res = await updateStore.checkNow(true)
    if (res.has_update && res.latest_version) {
      message.success(`发现新版本 ${res.latest_version}`)
    } else {
      message.success(`当前已是最新版本（${res.current_version}）`)
    }
  } catch (err) {
    message.error(err instanceof Error ? err.message : '检查更新失败')
  }
}

async function handleToggleAutoCheck(val: boolean) {
  try {
    await updateStore.savePreferences(val)
    message.success(val ? '已开启自动检查更新' : '已关闭自动检查更新')
  } catch (err) {
    message.error(err instanceof Error ? err.message : '保存更新偏好失败')
  }
}

async function saveBasic() {
  savingBasic.value = true
  try {
    const data = await apiRequest<SettingsPayload>('/settings', {
      method: 'PATCH',
      body: {
        name: basicForm.name.trim(),
        public_host: basicForm.public_host.trim(),
        domain: basicForm.domain.trim(),
        port_range: [basicForm.port_low, basicForm.port_high],
        email: basicForm.email.trim(),
      },
    })
    settings.value = data
    message.success('基础设置已保存')
    void dashStore.fetchDashboard(true)
  } catch (err) {
    message.error(err instanceof Error ? err.message : '保存失败')
  } finally {
    savingBasic.value = false
  }
}

async function saveAuthToken(regenerate = false) {
  savingToken.value = true
  try {
    const res = await apiRequest<{ auth_token: string }>('/settings/auth-token', {
      method: 'POST',
      body: { auth_token: regenerate ? '' : authTokenInput.value.trim() },
    })
    authTokenInput.value = res.auth_token
    if (settings.value) settings.value.auth_token = res.auth_token
    message.success(regenerate ? '已重新生成客户端连接密钥' : '连接密钥已更新')
  } catch (err) {
    message.error(err instanceof Error ? err.message : '更新密钥失败')
  } finally {
    savingToken.value = false
  }
}

async function handleToggleAPI(val: boolean) {
  togglingAPI.value = true
  try {
    const res = await apiRequest<{ api_enabled: boolean }>('/settings/api-toggle', {
      method: 'POST',
      body: { enabled: val },
    })
    if (settings.value) settings.value.api_enabled = res.api_enabled
    message.success(res.api_enabled ? 'REST API 已开启' : 'REST API 已关闭')
  } catch (err) {
    message.error(err instanceof Error ? err.message : '切换失败')
  } finally {
    togglingAPI.value = false
  }
}

async function handleCreateKey() {
  if (!keyForm.label.trim()) {
    message.warning('请填写密钥备注名称')
    return
  }
  creatingKey.value = true
  try {
    const res = await apiRequest<{ id: string; key: string }>('/api-keys', {
      method: 'POST',
      body: { label: keyForm.label.trim(), scope: keyForm.scope },
    })
    newlyCreatedKey.value = res.key
    keyForm.label = ''
    message.success('API 密钥已创建，请立即复制保存')
    await fetchSettings()
  } catch (err) {
    message.error(err instanceof Error ? err.message : '创建密钥失败')
  } finally {
    creatingKey.value = false
  }
}

function handleDeleteKey(id: string, label: string) {
  dialog.warning({
    title: '吊销 API 密钥',
    content: `确认吊销密钥「${label || id}」？`,
    positiveText: '吊销',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await apiRequest(`/api-keys/${encodeURIComponent(id)}`, { method: 'DELETE' })
        message.success('密钥已吊销')
        await fetchSettings()
      } catch (err) {
        message.error(err instanceof Error ? err.message : '吊销失败')
      }
    },
  })
}

async function handleChangePassword() {
  if (!pwForm.old_password || !pwForm.new_password) {
    message.warning('请填写当前密码和新密码')
    return
  }
  if (pwForm.new_password.length < 8) {
    message.warning('新密码长度不能少于 8 位')
    return
  }
  if (pwForm.new_password !== pwForm.confirm_password) {
    message.warning('两次输入的新密码不一致')
    return
  }
  changingPw.value = true
  try {
    await apiRequest('/settings/password', {
      method: 'POST',
      body: {
        old_password: pwForm.old_password,
        new_password: pwForm.new_password,
      },
    })
    pwForm.old_password = ''
    pwForm.new_password = ''
    pwForm.confirm_password = ''
    message.success('管理员密码已更新')
  } catch (err) {
    message.error(err instanceof Error ? err.message : '修改密码失败')
  } finally {
    changingPw.value = false
  }
}

async function handleCopy(val: string, tip: string) {
  if (await copyToClipboard(val)) {
    message.success(tip)
  } else {
    message.warning('自动复制受限，请长按手动复制')
  }
}

async function handleLogout() {
  dashStore.stopRealtime()
  await authStore.logout()
  message.success('已退出登录')
  await router.replace({ name: 'login' })
}
</script>

<template>
  <div class="settings-wrap">
    <section v-if="loading && !settings" class="nfr-card">
      <NSkeleton text :repeat="6" />
    </section>

    <section v-else-if="settings" class="nfr-card settings-card">
      <NTabs type="line" animated>
        <!-- Tab 1: Basic Node Settings -->
        <NTabPane name="basic" tab="基础配置">
          <div class="tab-body">
            <NForm label-placement="top" @submit.prevent="saveBasic">
              <div class="two-col">
                <NFormItem label="本机节点名称">
                  <NInput v-model:value="basicForm.name" placeholder="如 HK-01" />
                </NFormItem>
                <NFormItem label="对外公网地址">
                  <NInput v-model:value="basicForm.public_host" placeholder="公网 IP 或域名" />
                </NFormItem>
              </div>

              <div class="two-col">
                <NFormItem label="主域名（HTTP 子域路由，选填）">
                  <NInput v-model:value="basicForm.domain" placeholder="如 tunnel.example.com" />
                </NFormItem>
                <NFormItem label="证书邮箱（ACME 选填）">
                  <NInput v-model:value="basicForm.email" placeholder="ops@example.com" />
                </NFormItem>
              </div>

              <div class="two-col">
                <NFormItem label="端口池起始">
                  <NInputNumber v-model:value="basicForm.port_low" :min="1024" :max="65535" class="full-w" />
                </NFormItem>
                <NFormItem label="端口池结束">
                  <NInputNumber v-model:value="basicForm.port_high" :min="1024" :max="65535" class="full-w" />
                </NFormItem>
              </div>

              <NButton type="primary" attr-type="submit" class="nfr-touch-btn" :loading="savingBasic">
                保存基础配置
              </NButton>
            </NForm>

            <div class="divider" />

            <div class="sub-block">
              <div class="sub-title">客户端连接密钥（auth_token）</div>
              <p class="sub-desc">生成启动配置时自动写入该密钥；如由配置文件锁定则需在 server.toml 中修改。</p>
              <div class="token-row">
                <NInput
                  v-model:value="authTokenInput"
                  :disabled="settings.auth_token_from_config"
                  class="nfr-mono token-input"
                />
                <NButton secondary @click="handleCopy(authTokenInput, '连接密钥已复制')">
                  <template #icon><NIcon><CopyOutline /></NIcon></template>
                  复制
                </NButton>
                <NButton
                  v-if="!settings.auth_token_from_config"
                  secondary
                  :loading="savingToken"
                  @click="saveAuthToken(false)"
                >
                  保存
                </NButton>
                <NButton
                  v-if="!settings.auth_token_from_config"
                  quaternary
                  :loading="savingToken"
                  @click="saveAuthToken(true)"
                >
                  <template #icon><NIcon><RefreshOutline /></NIcon></template>
                  随机重置
                </NButton>
              </div>
            </div>

            <div class="divider" />

            <div class="sub-block update-block">
              <div class="api-head">
                <div>
                  <div class="sub-title">版本与系统更新</div>
                  <div class="sub-desc">
                    当前版本
                    <span class="nfr-mono">{{ updateStore.status?.current_version || authStore.version || 'dev' }}</span>
                    <template v-if="updateStore.hasUpdate && updateStore.status?.latest_version">
                      · 可升级至 <strong class="nfr-mono">{{ updateStore.status.latest_version }}</strong>
                    </template>
                    <template v-else-if="updateStore.status?.checked_at">
                      · 已是最新
                    </template>
                  </div>
                </div>
                <NTag
                  v-if="updateStore.hasUpdate"
                  size="small"
                  type="success"
                  :bordered="false"
                >
                  有新版本
                </NTag>
              </div>

              <div class="upd-row">
                <div class="upd-source-wrap">
                  <span class="sub-desc">更新下载源</span>
                  <NRadioGroup
                    :value="updateStore.selectedSource"
                    size="small"
                    @update:value="(v) => updateStore.setSource(v as 'github' | 'mirror')"
                  >
                    <NRadioButton value="github">GitHub</NRadioButton>
                    <NRadioButton value="mirror">镜像下载</NRadioButton>
                  </NRadioGroup>
                </div>

                <div class="upd-auto-wrap">
                  <span class="sub-desc">自动检查更新</span>
                  <NSwitch
                    :value="updateStore.status?.auto_check ?? true"
                    :loading="updateStore.savingPrefs"
                    @update:value="handleToggleAutoCheck"
                  />
                </div>
              </div>

              <div v-if="updateStore.selectedSource === 'mirror'" class="upd-mirror-hint">
                <template v-if="updateStore.probingMirrors">正在测速优选镜像节点…</template>
                <template v-else-if="updateStore.mirrorProbe?.ok && updateStore.mirrorProbe.mirror">
                  已优选 <span class="nfr-mono">{{ updateStore.mirrorProbe.mirror.name }}</span>
                  <span class="upd-rtt nfr-mono">{{ updateStore.mirrorProbe.rtt_ms }}ms</span>
                </template>
                <template v-else>
                  {{ updateStore.mirrorProbe?.message || '切换后将由服务端自动优选最快加速镜像' }}
                </template>
                <NButton
                  v-if="!updateStore.probingMirrors"
                  text
                  size="tiny"
                  @click="updateStore.probeMirrors(true)"
                >
                  <template #icon><NIcon :size="13"><RefreshOutline /></NIcon></template>
                  重测
                </NButton>
              </div>

              <div class="upd-btn-row">
                <NButton
                  secondary
                  :loading="updateStore.checking"
                  class="nfr-touch-btn"
                  @click="handleCheckUpdate"
                >
                  <template #icon><NIcon><RefreshOutline /></NIcon></template>
                  检查更新
                </NButton>
                <NButton
                  v-if="updateStore.hasUpdate"
                  type="primary"
                  class="nfr-touch-btn"
                  @click="updateStore.openModal()"
                >
                  <template #icon><NIcon><CloudDownloadOutline /></NIcon></template>
                  查看更新与一键升级
                </NButton>
              </div>
            </div>
          </div>
        </NTabPane>

        <!-- Tab 2: Security & API -->
        <NTabPane name="security" tab="安全与接口">
          <div class="tab-body">
            <div class="sub-block">
              <div class="sub-title">修改管理员密码</div>
              <NForm label-placement="top" class="pw-form" @submit.prevent="handleChangePassword">
                <NFormItem label="当前密码">
                  <NInput v-model:value="pwForm.old_password" type="password" show-password-on="click" />
                </NFormItem>
                <div class="two-col">
                  <NFormItem label="新密码（至少 8 位）">
                    <NInput v-model:value="pwForm.new_password" type="password" show-password-on="click" />
                  </NFormItem>
                  <NFormItem label="确认新密码">
                    <NInput v-model:value="pwForm.confirm_password" type="password" show-password-on="click" />
                  </NFormItem>
                </div>
                <NButton type="primary" attr-type="submit" class="nfr-touch-btn" :loading="changingPw">
                  更新密码
                </NButton>
              </NForm>
            </div>

            <div class="divider" />

            <div class="sub-block">
              <div class="api-head">
                <div>
                  <div class="sub-title">REST API 接口</div>
                  <div class="sub-desc">开启后可通过 Bearer 密钥调用 /api/v1/* 与 /metrics</div>
                </div>
                <NSwitch
                  :value="settings.api_enabled"
                  :loading="togglingAPI"
                  @update:value="handleToggleAPI"
                />
              </div>

              <NAlert v-if="newlyCreatedKey" type="success" :bordered="false" class="new-key-alert">
                <div class="new-key-row">
                  <span class="nfr-mono nfr-break">{{ newlyCreatedKey }}</span>
                  <NButton size="tiny" secondary @click="handleCopy(newlyCreatedKey, 'API 密钥已复制')">
                    复制
                  </NButton>
                </div>
              </NAlert>

              <div class="key-create-row">
                <NInput v-model:value="keyForm.label" placeholder="密钥名称（如 监控采集）" class="key-name-in" />
                <NSelect v-model:value="keyForm.scope" :options="scopeOptions" class="key-scope-in" />
                <NButton type="primary" :loading="creatingKey" @click="handleCreateKey">
                  <template #icon><NIcon><KeyOutline /></NIcon></template>
                  创建密钥
                </NButton>
              </div>

              <div v-if="settings.keys.length > 0" class="key-list">
                <div v-for="k in settings.keys" :key="k.id" class="key-item">
                  <div class="key-info">
                    <span class="key-label">{{ k.label || k.id }}</span>
                    <NTag size="small" :bordered="false">{{ k.scope }}</NTag>
                    <span class="key-meta nfr-mono">
                      {{ k.prefix ? `${k.prefix}…` : `#${k.id}` }} · {{ formatRelativeTime(k.last_used) }}
                    </span>
                  </div>
                  <NButton
                    size="small"
                    quaternary
                    type="error"
                    @click="handleDeleteKey(k.id, k.label)"
                  >
                    <template #icon><NIcon><TrashOutline /></NIcon></template>
                    吊销
                  </NButton>
                </div>
              </div>
            </div>
          </div>
        </NTabPane>

        <!-- Tab 3: Appearance -->
        <NTabPane name="appearance" tab="外观偏好">
          <div class="tab-body">
            <div class="pref-row">
              <div>
                <div class="sub-title">深色模式</div>
                <div class="sub-desc">默认浅色，切换后浏览器将自动记住你上次的选择</div>
              </div>
              <NSwitch :value="themeStore.isDark" @update:value="themeStore.setDark" />
            </div>

            <div class="divider" />

            <div class="sub-block">
              <div class="sub-title">主题主色</div>
              <div class="preset-grid">
                <button
                  v-for="preset in PRIMARY_PRESETS"
                  :key="preset.key"
                  type="button"
                  class="preset-btn"
                  :class="{ active: themeStore.primaryKey === preset.key }"
                  @click="themeStore.setPrimary(preset.key)"
                >
                  <span
                    class="preset-dot"
                    :style="{
                      backgroundColor: themeStore.isDark ? preset.dark.color : preset.light.color,
                    }"
                  />
                  <span>{{ preset.label }}</span>
                </button>
              </div>
            </div>

            <div class="divider" />

            <div class="pref-row">
              <div>
                <div class="sub-title">退出当前会话</div>
                <div class="sub-desc">清除本机浏览器登录状态并返回登录页</div>
              </div>
              <NButton secondary type="error" @click="handleLogout">
                <template #icon><NIcon><LogOutOutline /></NIcon></template>
                退出登录
              </NButton>
            </div>
          </div>
        </NTabPane>
      </NTabs>
    </section>
  </div>
</template>

<style scoped>
.settings-wrap {
  max-width: 760px;
  margin: 0 auto;
  width: 100%;
}

.settings-card {
  padding: 16px 20px 22px;
}

.tab-body {
  padding-top: 12px;
  display: flex;
  flex-direction: column;
  gap: 18px;
}

.two-col {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}

.full-w {
  width: 100%;
}

.divider {
  height: 1px;
  background: var(--nfr-border);
}

.sub-block {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.sub-title {
  font-size: 14px;
  font-weight: 600;
  color: var(--nfr-text);
}

.sub-desc {
  margin: 0;
  font-size: 12.5px;
  color: var(--nfr-text-secondary);
}

.token-row {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}

.token-input {
  flex: 1 1 220px;
}

.upd-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.upd-source-wrap,
.upd-auto-wrap {
  display: inline-flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.upd-mirror-hint {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  font-size: 12px;
  color: var(--nfr-text-secondary);
}

.upd-rtt {
  padding: 1px 6px;
  border-radius: 5px;
  background: var(--nfr-online-bg);
  color: var(--nfr-online-text);
  font-size: 11px;
  font-weight: 500;
}

.upd-btn-row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.api-head,
.pref-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
}

.new-key-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
}

.key-create-row {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}

.key-name-in {
  flex: 1 1 180px;
}

.key-scope-in {
  width: 140px;
}

.key-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.key-item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 10px 12px;
  border-radius: 8px;
  border: 1px solid var(--nfr-border);
  background: var(--nfr-subtle);
  flex-wrap: wrap;
}

.key-info {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.key-label {
  font-weight: 500;
  color: var(--nfr-text);
}

.key-meta {
  font-size: 12px;
  color: var(--nfr-text-secondary);
}

.preset-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
}

.preset-btn {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  min-height: 44px;
  padding: 8px 12px;
  border-radius: 8px;
  border: 1px solid var(--nfr-border);
  background: var(--nfr-subtle);
  color: var(--nfr-text);
  font-size: 13.5px;
  cursor: pointer;
  font-family: inherit;
  -webkit-tap-highlight-color: transparent;
  transition:
    transform 220ms var(--nfr-ease-spring),
    border-color var(--nfr-dur-fast) var(--nfr-ease-out),
    background-color var(--nfr-dur-base) var(--nfr-ease-out),
    box-shadow var(--nfr-dur-base) var(--nfr-ease-out);
}

.preset-btn:active {
  transform: scale(0.96);
  transition-duration: 90ms;
}

.preset-btn.active {
  border-color: var(--nfr-primary);
  background: var(--nfr-primary-soft);
  font-weight: 600;
}

.preset-dot {
  width: 12px;
  height: 12px;
  border-radius: 50%;
  border: 1px solid var(--nfr-border);
  flex-shrink: 0;
  transition: transform 260ms var(--nfr-ease-spring);
}

.preset-btn.active .preset-dot {
  transform: scale(1.18);
}

@media (max-width: 600px) {
  .settings-card {
    padding: 12px 14px 18px;
  }

  .two-col,
  .preset-grid {
    grid-template-columns: 1fr;
  }

  .key-scope-in {
    width: 100%;
  }
}
</style>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NEmpty,
  NForm,
  NFormItem,
  NIcon,
  NInput,
  NInputNumber,
  NModal,
  NProgress,
  NSkeleton,
  NSwitch,
  NTag,
  useDialog,
  useMessage,
} from 'naive-ui'
import {
  ArrowBackOutline,
  BanOutline,
  CheckmarkCircleOutline,
  KeyOutline,
  PowerOutline,
  TrashOutline,
} from '@vicons/ionicons5'
import { apiRequest } from '@/api/client'
import type { Tunnel, UserDetailResponse } from '@/types/api'
import { formatBytes, tunnelPublicAddress } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const message = useMessage()
const dialog = useDialog()

const userId = computed(() => String(route.params.id || ''))
const loading = ref(true)
const detail = ref<UserDetailResponse | null>(null)
const savingQuota = ref(false)
const togglingDisabled = ref(false)
const resetPwOpen = ref(false)
const resetPwValue = ref('')
const resettingPw = ref(false)
const busyTunnelId = ref('')

const quotaForm = reactive({
  max_tunnels: 0,
  max_bandwidth_kb: 0,
  monthly_quota_mb: 0,
})

const monthlyPct = computed(() => {
  const u = detail.value?.user
  if (!u || !u.monthly_quota_mb) return 0
  const cap = u.monthly_quota_mb * 1024 * 1024
  return Math.min(100, Math.round((u.used_bytes / cap) * 100))
})

async function fetchDetail() {
  if (!userId.value) return
  loading.value = true
  try {
    const res = await apiRequest<UserDetailResponse>(`/users/${encodeURIComponent(userId.value)}`)
    detail.value = res
    quotaForm.max_tunnels = res.user.max_tunnels
    quotaForm.max_bandwidth_kb = res.user.max_bandwidth_kb
    quotaForm.monthly_quota_mb = res.user.monthly_quota_mb
  } catch (err) {
    message.error(err instanceof Error ? err.message : '加载用户详情失败')
    void router.replace({ name: 'settings', query: { tab: 'users' } })
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void fetchDetail()
})

function goBack() {
  void router.push({ name: 'settings', query: { tab: 'users' } })
}

async function handleSaveQuota() {
  if (!detail.value) return
  savingQuota.value = true
  try {
    await apiRequest(`/users/${encodeURIComponent(userId.value)}`, {
      method: 'PATCH',
      body: {
        max_tunnels: quotaForm.max_tunnels || 0,
        max_bandwidth_kb: quotaForm.max_bandwidth_kb || 0,
        monthly_quota_mb: quotaForm.monthly_quota_mb || 0,
      },
    })
    message.success('配额已保存')
    await fetchDetail()
  } catch (err) {
    message.error(err instanceof Error ? err.message : '保存配额失败')
  } finally {
    savingQuota.value = false
  }
}

async function handleToggleDisabled() {
  const u = detail.value?.user
  if (!u || u.role === 'admin') return
  const next = !u.disabled
  togglingDisabled.value = true
  try {
    await apiRequest(`/users/${encodeURIComponent(userId.value)}`, {
      method: 'PATCH',
      body: { disabled: next },
    })
    message.success(next ? '已停用该账号并断开其在线隧道' : '已重新启用该账号')
    await fetchDetail()
  } catch (err) {
    message.error(err instanceof Error ? err.message : '操作失败')
  } finally {
    togglingDisabled.value = false
  }
}

async function handleResetPassword() {
  if (resetPwValue.value.length < 6) {
    message.warning('新密码至少需要 6 位')
    return
  }
  resettingPw.value = true
  try {
    await apiRequest(`/users/${encodeURIComponent(userId.value)}/reset-password`, {
      method: 'POST',
      body: { password: resetPwValue.value },
    })
    resetPwOpen.value = false
    resetPwValue.value = ''
    message.success('密码已重置，该用户需重新登录')
  } catch (err) {
    message.error(err instanceof Error ? err.message : '重置密码失败')
  } finally {
    resettingPw.value = false
  }
}

function handleDeleteUser() {
  const u = detail.value?.user
  if (!u || u.role === 'admin') return
  dialog.error({
    title: '删除用户账号',
    content: `确定永久删除用户「${u.username}」？该用户的所有隧道、节点与会话将被立即清理且无法恢复。`,
    positiveText: '永久删除',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await apiRequest(`/users/${encodeURIComponent(userId.value)}`, { method: 'DELETE' })
        message.success('用户已删除')
        await router.replace({ name: 'settings', query: { tab: 'users' } })
      } catch (err) {
        message.error(err instanceof Error ? err.message : '删除失败')
      }
    },
  })
}

async function handleToggleTunnel(t: Tunnel, enabled: boolean) {
  busyTunnelId.value = t.id
  try {
    await apiRequest(`/users/${encodeURIComponent(userId.value)}/tunnels/${encodeURIComponent(t.id)}/toggle`, {
      method: 'POST',
      body: { enabled },
    })
    message.success(enabled ? '隧道已启用' : '隧道已停用')
    await fetchDetail()
  } catch (err) {
    message.error(err instanceof Error ? err.message : '切换失败')
  } finally {
    busyTunnelId.value = ''
  }
}

async function handleKickTunnel(t: Tunnel) {
  busyTunnelId.value = t.id
  try {
    const res = await apiRequest<{ message: string }>(
      `/users/${encodeURIComponent(userId.value)}/tunnels/${encodeURIComponent(t.id)}/kick`,
      { method: 'POST' },
    )
    message.info(res.message || '已下发下线指令')
    await fetchDetail()
  } catch (err) {
    message.error(err instanceof Error ? err.message : '下线失败')
  } finally {
    busyTunnelId.value = ''
  }
}

function handleDeleteTunnel(t: Tunnel) {
  dialog.warning({
    title: '删除用户隧道',
    content: `确认删除隧道「${t.name}」？`,
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: async () => {
      busyTunnelId.value = t.id
      try {
        await apiRequest(`/users/${encodeURIComponent(userId.value)}/tunnels/${encodeURIComponent(t.id)}`, {
          method: 'DELETE',
        })
        message.success('隧道已删除')
        await fetchDetail()
      } catch (err) {
        message.error(err instanceof Error ? err.message : '删除失败')
      } finally {
        busyTunnelId.value = ''
      }
    },
  })
}

function statusTagType(kind: string): 'success' | 'warning' | 'error' | 'default' | 'info' {
  switch (kind) {
    case 'on':
      return 'success'
    case 'idle':
      return 'info'
    case 'warn':
      return 'warning'
    case 'err':
      return 'error'
    default:
      return 'default'
  }
}
</script>

<template>
  <div class="detail-wrap">
    <div class="back-bar">
      <NButton quaternary size="small" @click="goBack">
        <template #icon><NIcon><ArrowBackOutline /></NIcon></template>
        返回用户管理
      </NButton>
    </div>

    <section v-if="loading && !detail" class="nfr-card">
      <NSkeleton text :repeat="6" />
    </section>

    <template v-else-if="detail">
      <!-- User Summary Card -->
      <section class="nfr-card user-head-card">
        <div class="head-top">
          <div class="head-left">
            <span class="avatar-md">
              <img
                v-if="detail.user.has_avatar && detail.user.avatar_url"
                :src="detail.user.avatar_url"
                :alt="detail.user.username"
              />
              <span v-else>{{ detail.user.username.slice(0, 1).toUpperCase() }}</span>
            </span>
            <div>
              <div class="head-name-row">
                <h2 class="head-name">{{ detail.user.username }}</h2>
                <NTag size="small" :type="detail.user.role === 'admin' ? 'info' : 'default'" :bordered="false">
                  {{ detail.user.role === 'admin' ? '管理员' : '普通用户' }}
                </NTag>
                <NTag size="small" :type="detail.user.disabled ? 'error' : 'success'" :bordered="false">
                  {{ detail.user.disabled ? '已停用' : '正常' }}
                </NTag>
              </div>
              <div class="head-sub nfr-mono">
                隧道 {{ detail.user.tunnel_count }} 条 · 在线 {{ detail.user.online_tunnels }} 条 · 本月流量 {{ formatBytes(detail.user.used_bytes) }}
              </div>
            </div>
          </div>

          <div class="head-actions">
            <NButton size="small" secondary @click="resetPwOpen = true">
              <template #icon><NIcon><KeyOutline /></NIcon></template>
              重置密码
            </NButton>
            <template v-if="detail.user.role !== 'admin'">
              <NButton
                size="small"
                secondary
                :type="detail.user.disabled ? 'success' : 'warning'"
                :loading="togglingDisabled"
                @click="handleToggleDisabled"
              >
                <template #icon>
                  <NIcon>
                    <CheckmarkCircleOutline v-if="detail.user.disabled" />
                    <BanOutline v-else />
                  </NIcon>
                </template>
                {{ detail.user.disabled ? '启用账号' : '停用账号' }}
              </NButton>
              <NButton size="small" quaternary type="error" @click="handleDeleteUser">
                <template #icon><NIcon><TrashOutline /></NIcon></template>
                删除
              </NButton>
            </template>
          </div>
        </div>

        <div v-if="detail.user.monthly_quota_mb > 0" class="usage-bar-box">
          <div class="usage-bar-head">
            <span>本月流量配额使用</span>
            <span class="nfr-mono">
              {{ formatBytes(detail.user.used_bytes) }} / {{ detail.user.monthly_quota_mb }} MB ({{ monthlyPct }}%)
            </span>
          </div>
          <NProgress
            type="line"
            :percentage="monthlyPct"
            :status="monthlyPct >= 100 ? 'error' : monthlyPct >= 80 ? 'warning' : 'success'"
            :show-indicator="false"
          />
        </div>
      </section>

      <!-- Quota Editor -->
      <section v-if="detail.user.role !== 'admin'" class="nfr-card quota-card">
        <h3 class="sec-title">账号配额上限（0 表示不限制）</h3>
        <NForm label-placement="top" @submit.prevent="handleSaveQuota">
          <div class="three-col">
            <NFormItem label="最大隧道数">
              <NInputNumber v-model:value="quotaForm.max_tunnels" :min="0" :max="10000" class="full-w" />
            </NFormItem>
            <NFormItem label="单隧道带宽上限 (KB/s)">
              <NInputNumber v-model:value="quotaForm.max_bandwidth_kb" :min="0" :max="4194304" class="full-w" />
            </NFormItem>
            <NFormItem label="月流量配额 (MB)">
              <NInputNumber v-model:value="quotaForm.monthly_quota_mb" :min="0" :max="10485760" class="full-w" />
            </NFormItem>
          </div>
          <NButton type="primary" attr-type="submit" :loading="savingQuota">保存配额</NButton>
        </NForm>
      </section>

      <!-- User's Tunnels List -->
      <section class="nfr-card tunnels-card">
        <div class="sec-head">
          <h3 class="sec-title">名下隧道列表（{{ detail.tunnels.length }}）</h3>
        </div>

        <NEmpty v-if="detail.tunnels.length === 0" description="该用户暂未创建任何隧道" />

        <div v-else class="tunnel-list">
          <div v-for="t in detail.tunnels" :key="t.id" class="tunnel-item">
            <div class="tun-main">
              <div class="tun-title-row">
                <span class="tun-name">{{ t.name }}</span>
                <NTag size="tiny" :bordered="false">{{ t.type.toUpperCase() }}</NTag>
                <NTag size="tiny" :type="statusTagType(t.status_kind)" :bordered="false">{{ t.status }}</NTag>
              </div>
              <div class="tun-addrs nfr-mono">
                <span>公网：{{ tunnelPublicAddress(t) }}</span>
                <span>→ 本地：{{ t.local }}</span>
              </div>
              <div class="tun-stats nfr-mono">
                入 {{ formatBytes(t.in) }} · 出 {{ formatBytes(t.out) }} · 本月 {{ formatBytes(t.used_bytes || 0) }}
              </div>
            </div>

            <div class="tun-actions">
              <NSwitch
                :value="t.enabled"
                :loading="busyTunnelId === t.id"
                @update:value="(v) => handleToggleTunnel(t, v)"
              />
              <NButton
                v-if="t.online"
                size="tiny"
                secondary
                type="warning"
                :loading="busyTunnelId === t.id"
                @click="handleKickTunnel(t)"
              >
                <template #icon><NIcon><PowerOutline /></NIcon></template>
                下线
              </NButton>
              <NButton
                size="tiny"
                quaternary
                type="error"
                :loading="busyTunnelId === t.id"
                @click="handleDeleteTunnel(t)"
              >
                <template #icon><NIcon><TrashOutline /></NIcon></template>
                删除
              </NButton>
            </div>
          </div>
        </div>
      </section>
    </template>

    <NModal
      v-model:show="resetPwOpen"
      preset="card"
      title="重置用户密码"
      :style="{ width: '400px', maxWidth: 'calc(100vw - 24px)' }"
    >
      <NForm label-placement="top" @submit.prevent="handleResetPassword">
        <NFormItem label="新密码（至少 6 位）">
          <NInput v-model:value="resetPwValue" type="password" show-password-on="click" placeholder="输入新的登录密码" />
        </NFormItem>
        <div class="modal-foot">
          <NButton quaternary @click="resetPwOpen = false">取消</NButton>
          <NButton type="primary" attr-type="submit" :loading="resettingPw">确认重置</NButton>
        </div>
      </NForm>
    </NModal>
  </div>
</template>

<style scoped>
.detail-wrap {
  max-width: 860px;
  margin: 0 auto;
  width: 100%;
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.back-bar {
  display: flex;
  align-items: center;
}

.user-head-card,
.quota-card,
.tunnels-card {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.head-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
  flex-wrap: wrap;
}

.head-left {
  display: flex;
  align-items: center;
  gap: 12px;
  min-width: 0;
}

.avatar-md {
  width: 48px;
  height: 48px;
  border-radius: 50%;
  border: 1.5px solid var(--nfr-border);
  background: var(--nfr-primary-soft);
  color: var(--nfr-primary);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-weight: 700;
  font-size: 18px;
  overflow: hidden;
  flex-shrink: 0;
}

.avatar-md img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}

.head-name-row {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.head-name {
  margin: 0;
  font-size: 17px;
  font-weight: 600;
  color: var(--nfr-text);
}

.head-sub {
  margin-top: 3px;
  font-size: 12px;
  color: var(--nfr-text-secondary);
}

.head-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.usage-bar-box {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.usage-bar-head {
  display: flex;
  justify-content: space-between;
  gap: 8px;
  font-size: 12px;
  color: var(--nfr-text-secondary);
  flex-wrap: wrap;
}

.sec-title {
  margin: 0;
  font-size: 14.5px;
  font-weight: 600;
  color: var(--nfr-text);
}

.three-col {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 12px;
}

.full-w {
  width: 100%;
}

.tunnel-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.tunnel-item {
  padding: 12px 14px;
  border-radius: 10px;
  border: 1px solid var(--nfr-border);
  background: var(--nfr-subtle);
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.tun-main {
  min-width: 0;
  flex: 1 1 240px;
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.tun-title-row {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.tun-name {
  font-size: 14px;
  font-weight: 600;
  color: var(--nfr-text);
}

.tun-addrs,
.tun-stats {
  font-size: 12px;
  color: var(--nfr-text-secondary);
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
  overflow-wrap: anywhere;
}

.tun-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-shrink: 0;
}

.modal-foot {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}

@media (max-width: 640px) {
  .three-col {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>

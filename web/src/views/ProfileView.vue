<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import {
  NButton,
  NForm,
  NFormItem,
  NIcon,
  NInput,
  NSkeleton,
  NTag,
  useDialog,
  useMessage,
} from 'naive-ui'
import {
  CameraOutline,
  CopyOutline,
  RefreshOutline,
  TrashOutline,
} from '@vicons/ionicons5'
import { apiRequest, apiUpload } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import type { UserProfile } from '@/types/api'
import { copyToClipboard, formatBytes } from '@/utils/format'

const message = useMessage()
const dialog = useDialog()
const authStore = useAuthStore()

const loading = ref(true)
const profile = ref<UserProfile | null>(null)
const fileInputRef = ref<HTMLInputElement | null>(null)
const uploadingAvatar = ref(false)
const savingUsername = ref(false)
const savingPw = ref(false)
const rotatingToken = ref(false)

const usernameInput = ref('')
const pwForm = reactive({
  old_password: '',
  new_password: '',
  confirm_password: '',
})

const userInitial = computed(() => {
  const n = profile.value?.username?.trim() || authStore.user?.username?.trim() || 'U'
  return n.slice(0, 1).toUpperCase()
})

const quotaText = computed(() => {
  const u = profile.value
  if (!u) return { tunnels: '-', bw: '-', monthly: '-' }
  return {
    tunnels: u.max_tunnels > 0 ? `${u.tunnel_count} / ${u.max_tunnels} 条` : `${u.tunnel_count} 条（不限）`,
    bw: u.max_bandwidth_kb > 0 ? `${u.max_bandwidth_kb} KB/s` : '不限',
    monthly:
      u.monthly_quota_mb > 0
        ? `${formatBytes(u.used_bytes)} / ${u.monthly_quota_mb} MB`
        : `${formatBytes(u.used_bytes)}（不限）`,
  }
})

async function fetchProfile() {
  loading.value = true
  try {
    const res = await apiRequest<{ user: UserProfile }>('/profile')
    profile.value = res.user
    usernameInput.value = res.user.username
  } catch (err) {
    message.error(err instanceof Error ? err.message : '加载用户资料失败')
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void fetchProfile()
})

function triggerAvatarPick() {
  fileInputRef.value?.click()
}

async function compressAvatar(file: File): Promise<Blob> {
  const imgUrl = URL.createObjectURL(file)
  try {
    const img = await new Promise<HTMLImageElement>((resolve, reject) => {
      const el = new Image()
      el.onload = () => resolve(el)
      el.onerror = () => reject(new Error('无法读取该图片文件，请选择有效的 PNG / JPEG / WebP 图片'))
      el.src = imgUrl
    })
    const size = 160
    const canvas = document.createElement('canvas')
    canvas.width = size
    canvas.height = size
    const ctx = canvas.getContext('2d')
    if (!ctx) throw new Error('浏览器不支持图片裁剪')

    const side = Math.min(img.naturalWidth || img.width, img.naturalHeight || img.height)
    const sx = Math.max(0, Math.floor(((img.naturalWidth || img.width) - side) / 2))
    const sy = Math.max(0, Math.floor(((img.naturalHeight || img.height) - side) / 2))
    ctx.drawImage(img, sx, sy, side, side, 0, 0, size, size)

    const toBlob = (type: string, quality?: number) =>
      new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, type, quality))

    let blob = await toBlob('image/webp', 0.85)
    if (!blob || blob.type !== 'image/webp' || blob.size > 128 * 1024) {
      blob = await toBlob('image/jpeg', 0.85)
    }
    if (!blob || blob.size > 128 * 1024) {
      blob = await toBlob('image/png')
    }
    if (!blob) throw new Error('生成头像图片失败')
    if (blob.size > 128 * 1024) {
      throw new Error('压缩后头像仍超过 128 KB，请更换较简洁的图片')
    }
    return blob
  } finally {
    URL.revokeObjectURL(imgUrl)
  }
}

async function onFileSelected(ev: Event) {
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return
  uploadingAvatar.value = true
  try {
    const blob = await compressAvatar(file)
    await apiUpload<{ ok: boolean; avatar_url: string }>('/profile/avatar', blob)
    await fetchProfile()
    await authStore.refreshProfile()
    message.success('头像已更新')
  } catch (err) {
    message.error(err instanceof Error ? err.message : '头像上传失败')
  } finally {
    uploadingAvatar.value = false
  }
}

async function handleClearAvatar() {
  uploadingAvatar.value = true
  try {
    await apiRequest('/profile/avatar', { method: 'DELETE' })
    await fetchProfile()
    await authStore.refreshProfile()
    message.success('已恢复默认头像')
  } catch (err) {
    message.error(err instanceof Error ? err.message : '清除头像失败')
  } finally {
    uploadingAvatar.value = false
  }
}

async function handleSaveUsername() {
  const next = usernameInput.value.trim()
  if (next.length < 2) {
    message.warning('用户名至少需要 2 个字符')
    return
  }
  savingUsername.value = true
  try {
    const res = await apiRequest<{ user: UserProfile }>('/profile', {
      method: 'PATCH',
      body: { username: next },
    })
    profile.value = res.user
    await authStore.refreshProfile()
    message.success('用户名已更新')
  } catch (err) {
    message.error(err instanceof Error ? err.message : '修改用户名失败')
  } finally {
    savingUsername.value = false
  }
}

async function handleChangePassword() {
  if (!pwForm.old_password || !pwForm.new_password) {
    message.warning('请填写当前密码和新密码')
    return
  }
  if (pwForm.new_password.length < 6) {
    message.warning('新密码长度至少需要 6 位')
    return
  }
  if (pwForm.new_password !== pwForm.confirm_password) {
    message.warning('两次输入的新密码不一致')
    return
  }
  savingPw.value = true
  try {
    await apiRequest('/profile', {
      method: 'PATCH',
      body: {
        old_password: pwForm.old_password,
        new_password: pwForm.new_password,
      },
    })
    pwForm.old_password = ''
    pwForm.new_password = ''
    pwForm.confirm_password = ''
    message.success('密码已更新')
  } catch (err) {
    message.error(err instanceof Error ? err.message : '修改密码失败')
  } finally {
    savingPw.value = false
  }
}

function handleRotateToken() {
  dialog.warning({
    title: '重置个人连接密钥',
    content: '重置后当前账号已连接的 nfrc 客户端将会立即下线，需使用新密钥或新配置重新启动。确定继续吗？',
    positiveText: '确认重置',
    negativeText: '取消',
    onPositiveClick: async () => {
      rotatingToken.value = true
      try {
        const res = await apiRequest<{ auth_token: string }>('/profile/auth-token', {
          method: 'POST',
        })
        if (profile.value) {
          profile.value.auth_token = res.auth_token
        }
        message.success('连接密钥已重置')
      } catch (err) {
        message.error(err instanceof Error ? err.message : '重置失败')
      } finally {
        rotatingToken.value = false
      }
    },
  })
}

async function handleCopy(val?: string) {
  if (!val) return
  if (await copyToClipboard(val)) {
    message.success('连接密钥已复制')
  } else {
    message.warning('自动复制受限，请手动选中复制')
  }
}
</script>

<template>
  <div class="profile-wrap">
    <section v-if="loading && !profile" class="nfr-card">
      <NSkeleton text :repeat="6" />
    </section>

    <template v-else-if="profile">
      <!-- Avatar & Identity Card -->
      <section class="nfr-card profile-hero">
        <div class="hero-left">
          <div class="avatar-lg">
            <img
              v-if="profile.has_avatar && profile.avatar_url"
              :src="profile.avatar_url"
              :alt="profile.username"
              class="avatar-lg-img"
            />
            <span v-else class="avatar-lg-fallback">{{ userInitial }}</span>
          </div>
          <div class="hero-meta">
            <div class="hero-name-row">
              <h2 class="hero-name">{{ profile.username }}</h2>
              <NTag size="small" :type="profile.role === 'admin' ? 'info' : 'default'" :bordered="false">
                {{ profile.role === 'admin' ? '管理员' : '普通用户' }}
              </NTag>
            </div>
            <div class="hero-sub">支持上传本地 PNG / JPEG / WebP 图片，自动居中裁剪为 160×160</div>
            <div class="avatar-actions">
              <input
                ref="fileInputRef"
                type="file"
                accept="image/png,image/jpeg,image/webp"
                class="hidden-file"
                @change="onFileSelected"
              />
              <NButton size="small" secondary :loading="uploadingAvatar" @click="triggerAvatarPick">
                <template #icon><NIcon><CameraOutline /></NIcon></template>
                上传本地头像
              </NButton>
              <NButton
                v-if="profile.has_avatar"
                size="small"
                quaternary
                type="error"
                :loading="uploadingAvatar"
                @click="handleClearAvatar"
              >
                <template #icon><NIcon><TrashOutline /></NIcon></template>
                恢复默认
              </NButton>
            </div>
          </div>
        </div>

        <div class="quota-pills">
          <div class="quota-pill">
            <span class="pill-label">隧道配额</span>
            <span class="pill-val nfr-mono">{{ quotaText.tunnels }}</span>
          </div>
          <div class="quota-pill">
            <span class="pill-label">带宽上限</span>
            <span class="pill-val nfr-mono">{{ quotaText.bw }}</span>
          </div>
          <div class="quota-pill">
            <span class="pill-label">本月流量</span>
            <span class="pill-val nfr-mono">{{ quotaText.monthly }}</span>
          </div>
        </div>
      </section>

      <!-- Account Username & Password -->
      <section class="nfr-card profile-section">
        <h3 class="sec-title">账号资料</h3>
        <NForm label-placement="top" @submit.prevent="handleSaveUsername">
          <div class="inline-form">
            <NFormItem label="用户名" class="grow-item">
              <NInput v-model:value="usernameInput" placeholder="输入新用户名" />
            </NFormItem>
            <NButton type="primary" attr-type="submit" :loading="savingUsername" class="save-name-btn">
              保存用户名
            </NButton>
          </div>
        </NForm>

        <div class="divider" />

        <h3 class="sec-title">修改登录密码</h3>
        <NForm label-placement="top" @submit.prevent="handleChangePassword">
          <NFormItem label="当前密码">
            <NInput v-model:value="pwForm.old_password" type="password" show-password-on="click" />
          </NFormItem>
          <div class="two-col">
            <NFormItem label="新密码（至少 6 位）">
              <NInput v-model:value="pwForm.new_password" type="password" show-password-on="click" />
            </NFormItem>
            <NFormItem label="确认新密码">
              <NInput v-model:value="pwForm.confirm_password" type="password" show-password-on="click" />
            </NFormItem>
          </div>
          <NButton type="primary" attr-type="submit" :loading="savingPw" class="nfr-touch-btn">
            更新密码
          </NButton>
        </NForm>
      </section>

      <!-- Personal Client Auth Token -->
      <section class="nfr-card profile-section">
        <h3 class="sec-title">个人客户端连接密钥（auth_token）</h3>
        <p class="sec-desc">
          该密钥仅归属当前账号，你在隧道管理中生成的一键启动命令与客户端配置文件会自动使用此密钥。
        </p>
        <div class="token-row">
          <NInput :value="profile.auth_token || ''" readonly class="nfr-mono token-in" />
          <NButton secondary @click="handleCopy(profile.auth_token)">
            <template #icon><NIcon><CopyOutline /></NIcon></template>
            复制
          </NButton>
          <NButton quaternary type="warning" :loading="rotatingToken" @click="handleRotateToken">
            <template #icon><NIcon><RefreshOutline /></NIcon></template>
            重置密钥
          </NButton>
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.profile-wrap {
  max-width: 760px;
  margin: 0 auto;
  width: 100%;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.profile-hero {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.hero-left {
  display: flex;
  align-items: center;
  gap: 16px;
  flex-wrap: wrap;
}

.avatar-lg {
  width: 72px;
  height: 72px;
  border-radius: 50%;
  border: 2px solid var(--nfr-border);
  background: var(--nfr-primary-soft);
  color: var(--nfr-primary);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  overflow: hidden;
  flex-shrink: 0;
}

.avatar-lg-img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}

.avatar-lg-fallback {
  font-size: 28px;
  font-weight: 700;
}

.hero-meta {
  flex: 1 1 220px;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.hero-name-row {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.hero-name {
  margin: 0;
  font-size: 18px;
  font-weight: 600;
  color: var(--nfr-text);
}

.hero-sub {
  font-size: 12.5px;
  color: var(--nfr-text-secondary);
}

.avatar-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 2px;
}

.hidden-file {
  display: none;
}

.quota-pills {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
}

.quota-pill {
  padding: 10px 12px;
  border-radius: 8px;
  background: var(--nfr-subtle);
  border: 1px solid var(--nfr-border);
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.pill-label {
  font-size: 11.5px;
  color: var(--nfr-text-secondary);
}

.pill-val {
  font-size: 13.5px;
  font-weight: 600;
  color: var(--nfr-text);
  overflow-wrap: anywhere;
}

.profile-section {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.sec-title {
  margin: 0;
  font-size: 14.5px;
  font-weight: 600;
  color: var(--nfr-text);
}

.sec-desc {
  margin: 0;
  font-size: 12.5px;
  color: var(--nfr-text-secondary);
}

.inline-form {
  display: flex;
  align-items: flex-end;
  gap: 10px;
  flex-wrap: wrap;
}

.grow-item {
  flex: 1 1 220px;
  margin-bottom: 0;
}

.save-name-btn {
  margin-bottom: 24px;
}

.two-col {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}

.divider {
  height: 1px;
  background: var(--nfr-border);
  margin: 4px 0;
}

.token-row {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}

.token-in {
  flex: 1 1 220px;
}

@media (max-width: 640px) {
  .quota-pills,
  .two-col {
    grid-template-columns: minmax(0, 1fr);
  }
  .save-name-btn {
    margin-bottom: 0;
  }
}
</style>

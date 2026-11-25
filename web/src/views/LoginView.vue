<script setup lang="ts">
import { ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { NAlert, NButton, NCheckbox, NForm, NFormItem, NIcon, NInput, useMessage } from 'naive-ui'
import { LockClosedOutline, MoonOutline, SunnyOutline } from '@vicons/ionicons5'
import { APIError } from '@/api/client'
import {
  getRememberPreference,
  setRememberPreference,
  useAuthStore,
} from '@/stores/auth'
import { useThemeStore } from '@/stores/theme'

const router = useRouter()
const message = useMessage()
const authStore = useAuthStore()
const themeStore = useThemeStore()

const password = ref('')
const remember = ref(getRememberPreference())
const loading = ref(false)
const errMsg = ref('')

watch(remember, (checked) => {
  setRememberPreference(checked)
})

async function handleSubmit() {
  if (loading.value) return
  if (!password.value) {
    errMsg.value = '请输入控制台密码'
    return
  }
  errMsg.value = ''
  loading.value = true
  try {
    await authStore.login(password.value, remember.value)
    setRememberPreference(remember.value)
    message.success('欢迎回来')
    await router.replace({ name: 'dashboard' })
  } catch (err) {
    if (err instanceof APIError && err.status === 429 && err.retryAfterSeconds) {
      const mins = Math.max(1, Math.ceil(err.retryAfterSeconds / 60))
      errMsg.value = `连续输错次数过多，当前 IP 已暂时锁定，请约 ${mins} 分钟后重试`
    } else {
      errMsg.value = err instanceof Error ? err.message : '登录失败'
    }
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <main class="login-page">
    <div class="login-theme-btn">
      <NButton
        quaternary
        circle
        :aria-label="themeStore.isDark ? '切换浅色模式' : '切换暗黑模式'"
        @click="themeStore.toggleDark()"
      >
        <template #icon>
          <Transition name="nfr-icon-swap" mode="out-in">
            <NIcon :key="themeStore.isDark ? 'sun' : 'moon'" :size="19">
              <SunnyOutline v-if="themeStore.isDark" />
              <MoonOutline v-else />
            </NIcon>
          </Transition>
        </template>
      </NButton>
    </div>

    <div class="nfr-card login-card">
      <div class="login-brand">
        <div class="login-mark">
          <img src="/logo.png" alt="NFR Logo" class="login-logo-img" />
        </div>
        <div>
          <h1 class="login-title">NFR 控制台</h1>
          <p class="login-sub">单机反向穿透控制面</p>
        </div>
      </div>

      <NAlert v-if="errMsg" type="error" :bordered="false" class="login-alert">
        {{ errMsg }}
      </NAlert>

      <NForm label-placement="top" @submit.prevent="handleSubmit">
        <input
          type="text"
          name="username"
          value="admin"
          autocomplete="username"
          tabindex="-1"
          aria-hidden="true"
          class="sr-only-user"
        />
        <NFormItem label="管理员密码" class="pw-item">
          <NInput
            v-model:value="password"
            type="password"
            show-password-on="click"
            placeholder="请输入终端打印的初始密码或已设密码"
            autocomplete="current-password"
            autofocus
          >
            <template #prefix>
              <NIcon><LockClosedOutline /></NIcon>
            </template>
          </NInput>
        </NFormItem>

        <div class="login-options">
          <NCheckbox v-model:checked="remember">30 天免登录（记住登录状态）</NCheckbox>
        </div>

        <NButton
          type="primary"
          block
          size="large"
          class="nfr-touch-btn"
          attr-type="submit"
          :loading="loading"
        >
          登录控制台
        </NButton>
      </NForm>

      <div class="login-foot">
        <span>首次部署密码可在启动终端或</span>
        <span class="nfr-mono">initial-password.txt</span>
        <span>查看</span>
      </div>
    </div>
  </main>
</template>

<style scoped>
.login-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 20px 14px;
  position: relative;
  background: var(--nfr-canvas);
}

.login-theme-btn {
  position: fixed;
  top: 14px;
  right: 14px;
}

.login-card {
  width: 100%;
  max-width: 390px;
  padding: 28px 24px;
}

.login-brand {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 22px;
}

.login-mark {
  width: 44px;
  height: 44px;
  border-radius: 10px;
  background: #ffffff;
  border: 1px solid var(--nfr-border);
  padding: 4px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  overflow: hidden;
}

.login-logo-img {
  width: 100%;
  height: 100%;
  object-fit: contain;
  display: block;
}

.sr-only-user {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}

.pw-item {
  margin-bottom: 4px;
}

.login-options {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin: 0 0 18px;
}

.login-title {
  margin: 0;
  font-size: 20px;
  font-weight: 600;
  color: var(--nfr-text);
  line-height: 1.25;
}

.login-sub {
  margin: 2px 0 0;
  font-size: 13px;
  color: var(--nfr-text-secondary);
}

.login-alert {
  margin-bottom: 14px;
}

.login-foot {
  margin-top: 18px;
  font-size: 12px;
  color: var(--nfr-text-secondary);
  text-align: center;
  display: flex;
  justify-content: center;
  gap: 4px;
  flex-wrap: wrap;
}
</style>

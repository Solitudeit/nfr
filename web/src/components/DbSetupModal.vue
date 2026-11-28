<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NAlert,
  NButton,
  NForm,
  NFormItem,
  NIcon,
  NInput,
  NInputNumber,
  NModal,
  useMessage,
} from 'naive-ui'
import { CheckmarkCircleOutline, ServerOutline } from '@vicons/ionicons5'
import { apiRequest } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import type { DBSetupResponse, DBTestResponse } from '@/types/api'

const router = useRouter()
const message = useMessage()
const authStore = useAuthStore()
const dashStore = useDashboardStore()

const form = reactive({
  host: '127.0.0.1',
  port: 3306,
  database: '',
  username: '',
  password: '',
})

const testing = ref(false)
const saving = ref(false)
const feedback = ref<{ type: 'success' | 'error' | 'info'; text: string } | null>(null)

function validate(): boolean {
  if (!form.host.trim()) {
    feedback.value = { type: 'error', text: '请填写数据库主机地址（本机可填 127.0.0.1）' }
    return false
  }
  if (!form.database.trim()) {
    feedback.value = { type: 'error', text: '请填写数据库名称' }
    return false
  }
  if (!form.username.trim()) {
    feedback.value = { type: 'error', text: '请填写数据库用户名' }
    return false
  }
  return true
}

async function handleTest() {
  if (!validate() || testing.value) return
  testing.value = true
  feedback.value = null
  try {
    const res = await apiRequest<DBTestResponse>('/db/test', {
      method: 'POST',
      body: {
        host: form.host.trim(),
        port: form.port || 3306,
        database: form.database.trim(),
        username: form.username.trim(),
        password: form.password,
      },
    })
    feedback.value = {
      type: 'success',
      text: `${res.message || '连接成功'}${res.version ? `（MySQL ${res.version}）` : ''}`,
    }
  } catch (err) {
    feedback.value = {
      type: 'error',
      text: err instanceof Error ? err.message : '数据库连接测试失败',
    }
  } finally {
    testing.value = false
  }
}

async function handleSetup() {
  if (!validate() || saving.value) return
  saving.value = true
  feedback.value = null
  try {
    const res = await apiRequest<DBSetupResponse>('/db/setup', {
      method: 'POST',
      body: {
        host: form.host.trim(),
        port: form.port || 3306,
        database: form.database.trim(),
        username: form.username.trim(),
        password: form.password,
      },
    })
    authStore.setDBStatus(res.status)
    message.success(res.message || 'MySQL 数据库配置完成')
    if (res.reauth_required) {
      authStore.clearSession()
      await router.replace({ name: 'login' })
      return
    }
    await authStore.refreshProfile()
    await dashStore.fetchDashboard(true)
  } catch (err) {
    feedback.value = {
      type: 'error',
      text: err instanceof Error ? err.message : '初始化数据库失败',
    }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <NModal
    :show="authStore.requiresDbSetup"
    preset="card"
    :closable="false"
    :mask-closable="false"
    :close-on-esc="false"
    :auto-focus="false"
    class="db-setup-modal"
    :style="{ width: '480px', maxWidth: 'calc(100vw - 24px)' }"
  >
    <template #header>
      <div class="db-modal-head">
        <span class="db-modal-icon">
          <NIcon :size="20"><ServerOutline /></NIcon>
        </span>
        <div>
          <div class="db-modal-title">初始化 MySQL 数据库</div>
          <div class="db-modal-sub">多用户面板需先完成数据库连接配置后方可使用</div>
        </div>
      </div>
    </template>

    <div class="db-modal-body">
      <NAlert type="info" :bordered="false" class="db-tip">
        首次连接空库时将自动建表并迁移现有配置与隧道数据；若目标库已存有 NFR 数据，将安全接入并保留原有记录。
      </NAlert>

      <NAlert
        v-if="feedback"
        :type="feedback.type"
        :bordered="false"
        class="db-feedback"
      >
        {{ feedback.text }}
      </NAlert>

      <NForm label-placement="top" @submit.prevent="handleSetup">
        <div class="db-two-col">
          <NFormItem label="数据库主机">
            <NInput v-model:value="form.host" placeholder="127.0.0.1" />
          </NFormItem>
          <NFormItem label="端口">
            <NInputNumber v-model:value="form.port" :min="1" :max="65535" class="full-w" />
          </NFormItem>
        </div>

        <div class="db-two-col">
          <NFormItem label="数据库名">
            <NInput v-model:value="form.database" placeholder="请输入已创建的数据库名" />
          </NFormItem>
          <NFormItem label="用户名">
            <NInput v-model:value="form.username" placeholder="数据库用户名" />
          </NFormItem>
        </div>

        <NFormItem label="数据库密码">
          <NInput
            v-model:value="form.password"
            type="password"
            show-password-on="click"
            placeholder="请输入数据库密码"
          />
        </NFormItem>

        <div class="db-actions">
          <NButton secondary :loading="testing" :disabled="saving" class="nfr-touch-btn" @click="handleTest">
            测试连接
          </NButton>
          <NButton
            type="primary"
            attr-type="submit"
            :loading="saving"
            :disabled="testing"
            class="nfr-touch-btn"
          >
            <template #icon><NIcon><CheckmarkCircleOutline /></NIcon></template>
            连接并初始化数据库
          </NButton>
        </div>
      </NForm>
    </div>
  </NModal>
</template>

<style scoped>
.db-modal-head {
  display: flex;
  align-items: center;
  gap: 12px;
}

.db-modal-icon {
  width: 38px;
  height: 38px;
  border-radius: 10px;
  background: var(--nfr-primary-soft);
  color: var(--nfr-primary);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}

.db-modal-title {
  font-size: 16px;
  font-weight: 600;
  color: var(--nfr-text);
  line-height: 1.3;
}

.db-modal-sub {
  font-size: 12px;
  color: var(--nfr-text-secondary);
  margin-top: 2px;
}

.db-modal-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.db-tip,
.db-feedback {
  font-size: 12.5px;
}

.db-two-col {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px;
}

.full-w {
  width: 100%;
}

.db-actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  margin-top: 4px;
  flex-wrap: wrap;
}

@media (max-width: 480px) {
  .db-two-col {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>

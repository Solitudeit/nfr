<script setup lang="ts">
import { computed } from 'vue'
import {
  NAlert,
  NButton,
  NIcon,
  NModal,
  NProgress,
  NRadioButton,
  NRadioGroup,
  NTag,
  useMessage,
} from 'naive-ui'
import {
  ArrowForwardOutline,
  CloudDownloadOutline,
  OpenOutline,
  RefreshOutline,
} from '@vicons/ionicons5'
import { useUpdateStore } from '@/stores/update'
import { renderReleaseNotesMarkdown } from '@/utils/markdown'

const message = useMessage()
const updateStore = useUpdateStore()

const st = computed(() => updateStore.status)
const prog = computed(() => updateStore.upgradeProgress)

const renderedNotes = computed(() => {
  const raw = (st.value?.notes || '').trim()
  const md = raw || '本次发布包含功能优化与问题修复，详见 GitHub Release 说明。'
  return renderReleaseNotesMarkdown(md)
})

const publishedDate = computed(() => {
  const raw = st.value?.published_at || ''
  if (!raw) return ''
  const d = new Date(raw)
  if (Number.isNaN(d.getTime())) return raw.slice(0, 10)
  return d.toLocaleDateString('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit' })
})

function handleShowChange(val: boolean) {
  if (!val) {
    updateStore.dismissModal()
  }
}

async function handleApply() {
  try {
    await updateStore.startUpgrade()
  } catch (err) {
    message.error(err instanceof Error ? err.message : '启动一键升级失败')
  }
}
</script>

<template>
  <NModal
    :show="updateStore.modalOpen"
    preset="card"
    :trap-focus="false"
    :closable="!updateStore.upgrading"
    :mask-closable="!updateStore.upgrading"
    :close-on-esc="!updateStore.upgrading"
    title="发现新版本"
    :bordered="false"
    class="nfr-modal-card update-modal"
    @update:show="handleShowChange"
  >
    <div v-if="st" class="upd-body">
      <div class="upd-ver-row">
        <span class="ver-pill nfr-mono">{{ st.current_version }}</span>
        <NIcon :size="16" class="ver-arrow"><ArrowForwardOutline /></NIcon>
        <span class="ver-pill latest nfr-mono">{{ st.latest_version }}</span>
        <NTag v-if="st.is_prerelease" size="small" :bordered="false">预览版</NTag>
        <span v-if="publishedDate" class="upd-date nfr-mono">{{ publishedDate }}</span>
      </div>

      <div class="upd-notes-box">
        <!-- eslint-disable-next-line vue/no-v-html -->
        <div class="upd-notes-md" v-html="renderedNotes" />
      </div>

      <div class="upd-source-block">
        <div class="upd-source-head">
          <span class="upd-label">下载源</span>
          <NRadioGroup
            :value="updateStore.selectedSource"
            size="small"
            :disabled="updateStore.upgrading"
            @update:value="(v) => updateStore.setSource(v as 'github' | 'mirror')"
          >
            <NRadioButton value="github">GitHub</NRadioButton>
            <NRadioButton value="mirror">镜像下载</NRadioButton>
          </NRadioGroup>
        </div>

        <div v-if="updateStore.selectedSource === 'mirror'" class="upd-mirror-row">
          <template v-if="updateStore.probingMirrors">正在从服务端测速优选镜像节点…</template>
          <template v-else-if="updateStore.mirrorProbe?.ok && updateStore.mirrorProbe.mirror">
            已优选 <span class="nfr-mono">{{ updateStore.mirrorProbe.mirror.name }}</span>
            <span class="upd-rtt nfr-mono">{{ updateStore.mirrorProbe.rtt_ms }}ms</span>
          </template>
          <template v-else>
            {{ updateStore.mirrorProbe?.message || '将使用默认加速镜像节点' }}
          </template>
          <NButton
            v-if="!updateStore.probingMirrors && !updateStore.upgrading"
            text
            size="tiny"
            @click="updateStore.probeMirrors(true)"
          >
            <template #icon><NIcon :size="13"><RefreshOutline /></NIcon></template>
            重测
          </NButton>
        </div>
      </div>

      <NAlert
        v-if="!st.can_self_update"
        type="warning"
        :bordered="false"
        class="upd-alert"
      >
        {{ st.cannot_update_reason || '当前环境不支持原地一键升级，请前往发布页手动更新。' }}
      </NAlert>

      <div v-if="prog.state !== 'idle'" class="upd-progress-box">
        <NProgress
          type="line"
          :percentage="prog.percent"
          :status="prog.state === 'error' ? 'error' : prog.state === 'done' ? 'success' : 'default'"
          :processing="updateStore.upgrading"
        />
        <div v-if="prog.checksum_source" class="upd-checksum-row">
          <NTag
            size="small"
            :type="prog.checksum_source === 'mirror' ? 'warning' : 'success'"
            :bordered="false"
          >
            {{
              prog.checksum_source === 'mirror'
                ? '校验清单：已回退镜像 SHA256SUMS（官方不可达）'
                : '校验清单：GitHub 官方 SHA256SUMS'
            }}
          </NTag>
        </div>
        <div class="upd-progress-msg" :class="{ err: prog.state === 'error' }">
          {{ prog.message }}
        </div>
      </div>

      <div class="upd-actions">
        <NButton
          v-if="st.release_url"
          quaternary
          tag="a"
          :href="st.release_url"
          target="_blank"
          rel="noopener noreferrer"
          :disabled="updateStore.upgrading"
        >
          <template #icon><NIcon><OpenOutline /></NIcon></template>
          发布页
        </NButton>
        <div class="upd-actions-right">
          <NButton
            secondary
            :disabled="updateStore.upgrading"
            @click="updateStore.dismissModal()"
          >
            稍后提醒
          </NButton>
          <NButton
            v-if="st.can_self_update"
            type="primary"
            :loading="updateStore.upgrading"
            :disabled="updateStore.upgrading"
            @click="handleApply"
          >
            <template #icon><NIcon><CloudDownloadOutline /></NIcon></template>
            {{ updateStore.upgrading ? '正在升级…' : '一键升级' }}
          </NButton>
        </div>
      </div>
    </div>
  </NModal>
</template>

<style scoped>
.upd-body {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.upd-ver-row {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.ver-pill {
  padding: 2px 9px;
  border-radius: 6px;
  font-size: 12.5px;
  font-weight: 500;
  background: var(--nfr-id-bg);
  color: var(--nfr-id-text);
}

.ver-pill.latest {
  background: var(--nfr-online-bg);
  color: var(--nfr-online-text);
  font-weight: 600;
}

.ver-arrow {
  color: var(--nfr-text-secondary);
}

.upd-date {
  margin-left: auto;
  font-size: 12px;
  color: var(--nfr-text-secondary);
}

.upd-notes-box {
  max-height: 240px;
  overflow-y: auto;
  padding: 12px 14px;
  border-radius: 10px;
  border: 1px solid var(--nfr-border);
  background: var(--nfr-subtle);
}

.upd-notes-md {
  font-size: 13px;
  line-height: 1.65;
  color: var(--nfr-text);
  word-break: break-word;
  overflow-wrap: anywhere;
}

.upd-notes-md :deep(blockquote) {
  margin: 0 0 10px;
  padding: 8px 10px;
  border-left: 3px solid var(--nfr-accent, #18a058);
  border-radius: 0 6px 6px 0;
  background: var(--nfr-surface);
  color: var(--nfr-text-secondary);
  font-size: 12.5px;
}

.upd-notes-md :deep(blockquote p) {
  margin: 0;
}

.upd-notes-md :deep(h4) {
  margin: 10px 0 6px;
  font-size: 13px;
  font-weight: 600;
  color: var(--nfr-text);
}

.upd-notes-md :deep(h4:first-child) {
  margin-top: 0;
}

.upd-notes-md :deep(ul) {
  margin: 0 0 8px;
  padding-left: 18px;
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.upd-notes-md :deep(li) {
  margin: 0;
}

.upd-notes-md :deep(li.sub-item) {
  margin-left: 14px;
  list-style-type: circle;
  color: var(--nfr-text-secondary);
  font-size: 12.5px;
}

.upd-notes-md :deep(p) {
  margin: 0 0 8px;
}

.upd-notes-md :deep(p:last-child),
.upd-notes-md :deep(ul:last-child) {
  margin-bottom: 0;
}

.upd-notes-md :deep(code) {
  padding: 1px 5px;
  border-radius: 4px;
  background: var(--nfr-id-bg);
  color: var(--nfr-text);
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-size: 12px;
}

.upd-notes-md :deep(a) {
  color: var(--nfr-accent, #18a058);
  text-decoration: none;
}

.upd-notes-md :deep(a:hover) {
  text-decoration: underline;
}

.upd-notes-md :deep(.md-changelog) {
  margin-top: 10px;
  padding-top: 8px;
  border-top: 1px dashed var(--nfr-border);
  font-size: 12px;
}

.upd-source-block {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 10px 12px;
  border-radius: 8px;
  border: 1px solid var(--nfr-border);
  background: var(--nfr-subtle);
}

.upd-source-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
}

.upd-label {
  font-size: 13px;
  font-weight: 500;
  color: var(--nfr-text);
}

.upd-mirror-row {
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

.upd-progress-box {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.upd-checksum-row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
}

.upd-progress-msg {
  font-size: 12.5px;
  color: var(--nfr-text-secondary);
}

.upd-progress-msg.err {
  color: #dc2626;
}

.upd-actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
  padding-top: 4px;
}

.upd-actions-right {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-left: auto;
  flex-wrap: wrap;
}
</style>

<script setup lang="ts">
import { computed } from 'vue'
import { NTag, NTooltip } from 'naive-ui'
import type { ProbeResult } from '@/types/api'

const props = defineProps<{
  result?: ProbeResult
  loading?: boolean
}>()

const view = computed(() => {
  if (props.loading) {
    return {
      type: 'info' as const,
      label: '测通中…',
      detail: '正在向内网客户端发送探测帧',
    }
  }
  const r = props.result
  if (!r) return null
  switch (r.status) {
    case 'ok': {
      const rtt = r.rtt_ms ?? 0
      const local = r.local_ms ?? 0
      return {
        type: 'success' as const,
        label: `${rtt}ms 已连通`,
        detail: `总往返 ${rtt}ms · 本地拨号 ${local}ms`,
      }
    }
    case 'failed':
      return {
        type: 'error' as const,
        label: '连接失败',
        detail: r.message || '本地服务不可达',
      }
    case 'unsupported':
      return {
        type: 'info' as const,
        label: 'UDP 不支持握手探测',
        detail: r.message || 'UDP 为无连接协议，不支持主动握手探测',
      }
    case 'offline':
      return {
        type: 'default' as const,
        label: '节点离线',
        detail: r.message || '所属节点当前离线',
      }
    case 'disabled':
      return {
        type: 'default' as const,
        label: '已停用',
        detail: r.message || '隧道已停用，启用后可测通',
      }
    case 'awaiting':
      return {
        type: 'warning' as const,
        label: '等待接入',
        detail: r.message || '隧道尚未绑定在线节点',
      }
    case 'busy':
      return {
        type: 'warning' as const,
        label: '探测繁忙',
        detail: r.message || '节点探测繁忙，请稍后重试',
      }
    default:
      return {
        type: 'warning' as const,
        label: '响应超时',
        detail: r.message || '客户端版本较旧或响应超时，请升级 nfrc',
      }
  }
})
</script>

<template>
  <NTooltip v-if="view" trigger="hover">
    <template #trigger>
      <NTag size="small" :type="view.type" :bordered="false" round class="nfr-num probe-tag">
        {{ view.label }}
      </NTag>
    </template>
    <span class="nfr-break">{{ view.detail }}</span>
  </NTooltip>
</template>

<style scoped>
.probe-tag {
  max-width: 100%;
}
</style>

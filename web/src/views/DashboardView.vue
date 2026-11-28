<script setup lang="ts">
import { computed } from 'vue'
import { NAlert, NButton, NIcon, NSkeleton, NTag } from 'naive-ui'
import { ArrowDownOutline, ArrowUpOutline, SwapVerticalOutline } from '@vicons/ionicons5'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { formatBytes, formatRate } from '@/utils/format'
import ClientDownloadCard from '@/components/ClientDownloadCard.vue'
import RateChart from '@/components/RateChart.vue'

const authStore = useAuthStore()
const dashStore = useDashboardStore()

const nodeDisplayName = computed(() => dashStore.info?.name || '本机节点')
const nodeHost = computed(
  () => dashStore.settings?.public_host || dashStore.settings?.domain || dashStore.settings?.join_addr || '运行中'
)
const accountName = computed(() => dashStore.user?.username || authStore.user?.username || '')
const monthlyQuotaLabel = computed(() => {
  const u = dashStore.user
  if (!u || !u.monthly_quota_mb) return ''
  return `本月已用 ${formatBytes(u.used_bytes)} / ${u.monthly_quota_mb} MB`
})
</script>

<template>
  <div class="dash-page">
    <NAlert v-if="dashStore.error && !dashStore.initialized" type="error" :bordered="false">
      <div class="err-row">
        <span>{{ dashStore.error }}</span>
        <NButton size="small" secondary @click="dashStore.fetchDashboard(false)">重试</NButton>
      </div>
    </NAlert>

    <!-- 1. Compact 4-Metric Strip -->
    <section class="kpi-grid" aria-label="核心运行指标">
      <template v-if="dashStore.loading && !dashStore.initialized">
        <div v-for="i in 4" :key="i" class="nfr-card kpi-card">
          <NSkeleton text style="width: 45%; margin-bottom: 10px" />
          <NSkeleton text style="width: 65%; height: 24px" />
        </div>
      </template>

      <template v-else>
        <div class="nfr-card kpi-card">
          <div class="kpi-label">本机节点</div>
          <div class="kpi-value kpi-node">
            <span class="status-dot" aria-hidden="true" />
            <span class="kpi-node-name">{{ nodeDisplayName }}</span>
          </div>
          <div class="kpi-sub nfr-mono">{{ nodeHost }}</div>
        </div>

        <div class="nfr-card kpi-card">
          <div class="kpi-label">在线隧道</div>
          <div class="kpi-value nfr-num">
            <span>{{ dashStore.runningTunnelCount }}</span>
            <span class="kpi-total">/ {{ dashStore.tunnels.length }}</span>
          </div>
        </div>

        <div class="nfr-card kpi-card">
          <div class="kpi-label">实时入站</div>
          <div class="kpi-value nfr-num">{{ formatRate(dashStore.totalInRate) }}</div>
        </div>

        <div class="nfr-card kpi-card">
          <div class="kpi-label">实时出站</div>
          <div class="kpi-value nfr-num">{{ formatRate(dashStore.totalOutRate) }}</div>
        </div>
      </template>
    </section>

    <!-- 2. Per-Account Cumulative Inbound / Outbound Traffic Record -->
    <section class="nfr-card traffic-card" aria-label="账号累计出入站流量">
      <template v-if="dashStore.loading && !dashStore.initialized">
        <NSkeleton text :repeat="2" />
      </template>
      <template v-else>
        <div class="traffic-head">
          <div class="traffic-title-wrap">
            <span class="traffic-title">出入站总流量记录</span>
            <NTag v-if="accountName" size="tiny" :bordered="false" type="info">{{ accountName }}</NTag>
          </div>
          <span v-if="monthlyQuotaLabel" class="traffic-quota nfr-mono">{{ monthlyQuotaLabel }}</span>
          <span v-else class="traffic-hint">各账号独立统计 · 持久记录</span>
        </div>

        <div class="traffic-grid">
          <div class="traffic-cell">
            <div class="traffic-cell-label">
              <NIcon :size="14" class="traffic-icon in"><ArrowDownOutline /></NIcon>
              <span>累计入站流量</span>
            </div>
            <div class="traffic-cell-val nfr-num" data-metric="total-in">
              {{ formatBytes(dashStore.totalInBytes) }}
            </div>
          </div>

          <div class="traffic-cell">
            <div class="traffic-cell-label">
              <NIcon :size="14" class="traffic-icon out"><ArrowUpOutline /></NIcon>
              <span>累计出站流量</span>
            </div>
            <div class="traffic-cell-val nfr-num" data-metric="total-out">
              {{ formatBytes(dashStore.totalOutBytes) }}
            </div>
          </div>

          <div class="traffic-cell total">
            <div class="traffic-cell-label">
              <NIcon :size="14" class="traffic-icon sum"><SwapVerticalOutline /></NIcon>
              <span>累计总流量</span>
            </div>
            <div class="traffic-cell-val nfr-num" data-metric="total-sum">
              {{ formatBytes(dashStore.totalTrafficBytes) }}
            </div>
          </div>
        </div>
      </template>
    </section>

    <!-- 3. Direct Panel Client Downloader -->
    <ClientDownloadCard />

    <!-- 4. 60s Per-Account Rate Chart -->
    <RateChart :rates="dashStore.rates" />
  </div>
</template>

<style scoped>
.dash-page {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.err-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.kpi-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 14px;
}

.kpi-card {
  padding: 16px 18px;
}

@media (hover: hover) and (pointer: fine) {
  .kpi-card:hover {
    transform: translateY(-1.5px);
    box-shadow: var(--nfr-shadow-hover);
  }
}

.kpi-label {
  font-size: 12px;
  color: var(--nfr-text-secondary);
  margin-bottom: 6px;
}

.kpi-value {
  font-size: 22px;
  font-weight: 600;
  color: var(--nfr-text);
  line-height: 1.25;
  display: flex;
  align-items: baseline;
  gap: 6px;
}

.kpi-node {
  align-items: center;
  font-size: 18px;
}

.kpi-node-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.status-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--nfr-online-text);
  flex-shrink: 0;
  box-shadow: 0 0 0 0 var(--nfr-online-bg);
  animation: nfr-status-pulse 2.6s var(--nfr-ease-out) infinite;
}

@keyframes nfr-status-pulse {
  0% {
    box-shadow: 0 0 0 0 rgba(22, 163, 74, 0.45);
  }
  70% {
    box-shadow: 0 0 0 7px rgba(22, 163, 74, 0);
  }
  100% {
    box-shadow: 0 0 0 0 rgba(22, 163, 74, 0);
  }
}

.kpi-sub {
  margin-top: 4px;
  font-size: 11.5px;
  color: var(--nfr-text-secondary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.kpi-total {
  font-size: 13px;
  font-weight: 400;
  color: var(--nfr-text-secondary);
}

.traffic-card {
  padding: 15px 18px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.traffic-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  flex-wrap: wrap;
}

.traffic-title-wrap {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.traffic-title {
  font-size: 13.5px;
  font-weight: 600;
  color: var(--nfr-text);
}

.traffic-hint,
.traffic-quota {
  font-size: 12px;
  color: var(--nfr-text-secondary);
}

.traffic-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
}

.traffic-cell {
  padding: 11px 14px;
  border-radius: 9px;
  border: 1px solid var(--nfr-border);
  background: var(--nfr-subtle);
  display: flex;
  flex-direction: column;
  gap: 5px;
  min-width: 0;
}

.traffic-cell.total {
  background: var(--nfr-primary-soft);
  border-color: transparent;
}

.traffic-cell-label {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 12px;
  color: var(--nfr-text-secondary);
}

.traffic-icon.in {
  color: #16a34a;
}

.traffic-icon.out {
  color: #2563eb;
}

.traffic-icon.sum {
  color: var(--nfr-primary);
}

.traffic-cell-val {
  font-size: 19px;
  font-weight: 600;
  color: var(--nfr-text);
  line-height: 1.2;
  overflow-wrap: anywhere;
}

@media (max-width: 900px) {
  .kpi-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 10px;
  }

  .kpi-card {
    padding: 14px;
  }

  .kpi-value {
    font-size: 19px;
  }
}

@media (max-width: 600px) {
  .traffic-card {
    padding: 14px;
  }

  .traffic-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 8px;
  }

  .traffic-cell.total {
    grid-column: span 2;
  }

  .traffic-cell-val {
    font-size: 17px;
  }
}
</style>

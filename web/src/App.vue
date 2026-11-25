<script setup lang="ts">
import {
  NConfigProvider,
  NDialogProvider,
  NMessageProvider,
  zhCN,
  dateZhCN,
} from 'naive-ui'
import { useThemeStore } from '@/stores/theme'
import hljs from '@/utils/hljs'

const themeStore = useThemeStore()
</script>

<template>
  <NConfigProvider
    :theme="themeStore.naiveTheme"
    :theme-overrides="themeStore.themeOverrides"
    :locale="zhCN"
    :date-locale="dateZhCN"
    :hljs="hljs"
  >
    <NMessageProvider placement="top">
      <NDialogProvider>
        <router-view v-slot="{ Component }">
          <Transition name="nfr-page" mode="out-in">
            <component :is="Component" />
          </Transition>
        </router-view>
      </NDialogProvider>
    </NMessageProvider>
  </NConfigProvider>
</template>

import { computed, ref, watch } from 'vue'
import { defineStore } from 'pinia'
import { darkTheme, type GlobalThemeOverrides } from 'naive-ui'

export interface PrimaryPreset {
  key: string
  label: string
  light: {
    color: string
    hover: string
    pressed: string
    onPrimary: string
    soft: string
  }
  dark: {
    color: string
    hover: string
    pressed: string
    onPrimary: string
    soft: string
  }
}

export const PRIMARY_PRESETS: PrimaryPreset[] = [
  {
    key: 'graphite',
    label: '极简石墨',
    light: {
      color: '#18181B',
      hover: '#27272A',
      pressed: '#09090B',
      onPrimary: '#FFFFFF',
      soft: 'rgba(24, 24, 27, 0.08)',
    },
    dark: {
      color: '#F4F4F5',
      hover: '#FFFFFF',
      pressed: '#D4D4D8',
      onPrimary: '#09090B',
      soft: 'rgba(244, 244, 245, 0.12)',
    },
  },
  {
    key: 'cobalt',
    label: '云蓝石板',
    light: {
      color: '#2563EB',
      hover: '#3B82F6',
      pressed: '#1D4ED8',
      onPrimary: '#FFFFFF',
      soft: 'rgba(37, 99, 235, 0.1)',
    },
    dark: {
      color: '#3B82F6',
      hover: '#60A5FA',
      pressed: '#2563EB',
      onPrimary: '#FFFFFF',
      soft: 'rgba(59, 130, 246, 0.16)',
    },
  },
  {
    key: 'ocean',
    label: '冰蓝青',
    light: {
      color: '#0284C7',
      hover: '#0EA5E9',
      pressed: '#0369A1',
      onPrimary: '#FFFFFF',
      soft: 'rgba(2, 132, 199, 0.1)',
    },
    dark: {
      color: '#38BDF8',
      hover: '#7DD3FC',
      pressed: '#0EA5E9',
      onPrimary: '#09090B',
      soft: 'rgba(56, 189, 248, 0.16)',
    },
  },
]

const DARK_KEY = 'nfr_theme_dark'
const PRIMARY_KEY = 'nfr_theme_primary'

function safeGet(key: string): string | null {
  try {
    return window.localStorage.getItem(key)
  } catch {
    return null
  }
}

function safeSet(key: string, val: string): void {
  try {
    window.localStorage.setItem(key, val)
  } catch {
    // Ignore restricted mobile WebView storage errors
  }
}

function resolveInitialPrimary(): string {
  const saved = safeGet(PRIMARY_KEY)
  if (saved && PRIMARY_PRESETS.some((p) => p.key === saved)) {
    return saved
  }
  // Migrate legacy emerald/old presets cleanly to Swiss Monochrome default.
  safeSet(PRIMARY_KEY, 'graphite')
  return 'graphite'
}

export const useThemeStore = defineStore('theme', () => {
  const isDark = ref(safeGet(DARK_KEY) === '1')
  const primaryKey = ref(resolveInitialPrimary())

  const activePreset = computed(
    () => PRIMARY_PRESETS.find((p) => p.key === primaryKey.value) ?? PRIMARY_PRESETS[0]
  )

  const activeTone = computed(() =>
    isDark.value ? activePreset.value.dark : activePreset.value.light
  )

  const naiveTheme = computed(() => (isDark.value ? darkTheme : null))

  const themeOverrides = computed<GlobalThemeOverrides>(() => {
    const tone = activeTone.value
    const dark = isDark.value
    const switchRailActive = dark
      ? primaryKey.value === 'graphite'
        ? '#52525B'
        : primaryKey.value === 'ocean'
          ? '#0284C7'
          : tone.color
      : tone.color
    return {
      common: {
        primaryColor: tone.color,
        primaryColorHover: tone.hover,
        primaryColorPressed: tone.pressed,
        primaryColorSuppl: tone.hover,
        borderRadius: '8px',
        borderRadiusSmall: '6px',
        fontFamily: "'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', 'Segoe UI', sans-serif",
        fontFamilyMono: "ui-monospace, 'SFMono-Regular', 'Cascadia Code', Menlo, Consolas, monospace",
        bodyColor: dark ? '#09090B' : '#FAFAFA',
        cardColor: dark ? '#18181B' : '#FFFFFF',
        modalColor: dark ? '#18181B' : '#FFFFFF',
        popoverColor: dark ? '#1F1F23' : '#FFFFFF',
        tableColor: dark ? '#18181B' : '#FFFFFF',
        inputColor: dark ? '#18181B' : '#FFFFFF',
        borderColor: dark ? 'rgba(255, 255, 255, 0.09)' : '#E4E4E7',
        dividerColor: dark ? 'rgba(255, 255, 255, 0.08)' : '#E4E4E7',
        textColorBase: dark ? '#FAFAFA' : '#18181B',
        textColor1: dark ? '#FAFAFA' : '#18181B',
        textColor2: dark ? '#A1A1AA' : '#52525B',
        textColor3: dark ? '#71717A' : '#71717A',
      },
      Button: {
        textColorPrimary: tone.onPrimary,
        textColorHoverPrimary: tone.onPrimary,
        textColorPressedPrimary: tone.onPrimary,
        textColorFocusPrimary: tone.onPrimary,
        textColorDisabledPrimary: tone.onPrimary,
      },
      Switch: {
        railColorActive: switchRailActive,
        buttonColor: '#FFFFFF',
      },
      Checkbox: {
        checkMarkColor: tone.onPrimary,
      },
      Radio: {
        buttonTextColorActive: tone.color,
        buttonColorActive: tone.soft,
        buttonBorderColorActive: tone.color,
      },
      Menu: {
        itemColorActive: tone.soft,
        itemColorActiveHover: tone.soft,
        itemTextColorActive: tone.color,
        itemTextColorActiveHover: tone.color,
        itemIconColorActive: tone.color,
        itemIconColorActiveHover: tone.color,
      },
      Card: {
        borderRadius: '12px',
      },
      Modal: {
        borderRadius: '12px',
      },
      Drawer: {
        borderRadius: '12px',
      },
      Dialog: {
        borderRadius: '12px',
      },
      Dropdown: {
        borderRadius: '10px',
      },
    }
  })

  function syncDOM() {
    const root = document.documentElement
    const tone = activeTone.value
    root.classList.toggle('dark', isDark.value)
    root.style.setProperty('--nfr-primary', tone.color)
    root.style.setProperty('--nfr-on-primary', tone.onPrimary)
    root.style.setProperty('--nfr-primary-soft', tone.soft)
  }

  function setDark(val: boolean) {
    isDark.value = val
    safeSet(DARK_KEY, val ? '1' : '0')
  }

  function toggleDark() {
    setDark(!isDark.value)
  }

  function setPrimary(key: string) {
    if (!PRIMARY_PRESETS.some((p) => p.key === key)) return
    primaryKey.value = key
    safeSet(PRIMARY_KEY, key)
  }

  watch([isDark, activeTone], syncDOM, { immediate: true })

  return {
    isDark,
    primaryKey,
    activePreset,
    activeTone,
    naiveTheme,
    themeOverrides,
    setDark,
    toggleDark,
    setPrimary,
  }
})

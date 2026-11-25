import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import path from 'node:path'

export default defineConfig({
  base: '/nfr/',
  plugins: [vue()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, 'src'),
    },
  },
  build: {
    target: ['es2018', 'safari13', 'chrome70'],
    outDir: '../internal/panel/dist',
    emptyOutDir: false,
    sourcemap: false,
    chunkSizeWarningLimit: 1200,
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (id.includes('node_modules/naive-ui')) return 'naive-ui'
          if (id.includes('node_modules/echarts') || id.includes('node_modules/zrender')) return 'echarts'
          if (id.includes('node_modules/highlight.js')) return 'highlight'
        },
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/nfr/api': {
        target: 'http://127.0.0.1:8443',
        changeOrigin: true,
      },
    },
  },
})

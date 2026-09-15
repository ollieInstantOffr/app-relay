import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

const target = process.env.RELAY_API ?? 'http://127.0.0.1:8181'

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target, changeOrigin: false },
      '/mcp': { target, changeOrigin: false },
    },
  },
})

import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'
import { webCrypto } from './build/crypto'

// The API server for `npm run dev`: the demo server (or a real incus-backup-server web listener
// reached through a plain-HTTP shim) on 127.0.0.1:8099. Override with IB_API=https://host:8443.
const target = process.env.IB_API || 'http://127.0.0.1:8099'

export default defineConfig({
  plugins: [vue(), webCrypto(fileURLToPath(new URL('.', import.meta.url)))],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target,
        secure: false, // self-signed web certificate of a development server
        // The server rejects a state-changing request whose Origin host is not its own Host
        // (CSRF defence). The browser sends Origin: http://localhost:5173, so rewrite it to the target.
        changeOrigin: true,
        configure: (proxy) => {
          proxy.on('proxyReq', (req) => {
            if (req.getHeader('origin')) req.setHeader('origin', target)
          })
        },
      },
    },
  },
  build: {
    outDir: fileURLToPath(new URL('../internal/server/webui/dist', import.meta.url)),
    emptyOutDir: true,
    target: 'es2022',
    assetsInlineLimit: 0, // CSP img-src allows data:, but fonts and icons stay real files (cacheable)
    chunkSizeWarningLimit: 300,
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.ts'],
  },
})

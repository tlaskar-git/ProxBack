import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// ProxBack web control panel.
// The dev server proxies the REST API and the agent downloads to the Go server
// on :8443. `build.outDir` stays the Vite default (`dist`) — the Go build copies
// it into internal/api/webdist for go:embed.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8443',
        changeOrigin: true,
        secure: false,
      },
      '/downloads': {
        target: 'http://localhost:8443',
        changeOrigin: true,
        secure: false,
      },
    },
  },
})

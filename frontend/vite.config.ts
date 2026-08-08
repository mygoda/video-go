import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, '.', 'VITE_');
  return {
    plugins: [react()],
    resolve: {
      alias: { '@': new URL('./src', import.meta.url).pathname },
    },
    server: {
      port: 5173,
      proxy: {
        // 真后端模式下前端仍然请求同源 /api，由 dev server 转发，避开 CORS 与 SSE 缓冲
        '/api': {
          target: env.VITE_API_PROXY_TARGET ?? 'http://localhost:18080',
          changeOrigin: true,
        },
      },
    },
  };
});

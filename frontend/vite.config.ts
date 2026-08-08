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
      port: 15173,
      // 端口被占时直接失败退出，不允许 Vite 自动 +1 漂到别的端口：
      // 漂走之后浏览器里那个还开着的旧页面仍然指向原端口，看起来一切正常，
      // 实际调的是另一个进程。宁可起不来，也不要起在一个没人知道的端口上。
      strictPort: true,
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

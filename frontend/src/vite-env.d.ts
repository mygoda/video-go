/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** 'true' = 走内置内存 mock 后端；其余值 = 走 VITE_API_BASE 指向的真后端 */
  readonly VITE_USE_MOCK?: string;
  /** 真后端基址，默认 '/api'（由 vite dev server 代理） */
  readonly VITE_API_BASE?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}

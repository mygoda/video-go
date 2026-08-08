const TOKEN_KEY = 'aigc_pool_token';

/** 内存镜像，避免每个请求都读一次 localStorage（frontend-design.md §6.4） */
let token: string | null = localStorage.getItem(TOKEN_KEY);
const listeners = new Set<(t: string | null) => void>();

export function getToken(): string | null {
  return token;
}

export function setToken(next: string | null): void {
  token = next;
  if (next) localStorage.setItem(TOKEN_KEY, next);
  else localStorage.removeItem(TOKEN_KEY);
  listeners.forEach((fn) => fn(next));
}

export function onTokenChange(fn: (t: string | null) => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

import type { ApiErrorBody, TaskError } from './types';
import { getToken, setToken } from './token';
import { mockHandle } from '@/mock/backend';

export const USE_MOCK = (import.meta.env.VITE_USE_MOCK ?? 'true') !== 'false';
export const API_BASE = import.meta.env.VITE_API_BASE ?? '/api';

export class ApiError extends Error {
  readonly status: number;
  readonly body: ApiErrorBody['error'];

  constructor(status: number, body: ApiErrorBody['error']) {
    super(body.message);
    this.name = 'ApiError';
    this.status = status;
    this.body = body;
  }

  /** 提交任务失败时，把 HTTP 错误转成任务卡片能直接呈现的失败对象 */
  asTaskError(): TaskError {
    return {
      code: (this.body.code as TaskError['code']) ?? 'internal_error',
      message: this.body.message,
      field_errors: this.body.field_errors,
      retryable: this.body.retryable ?? true,
      charged: this.body.charged ?? false,
    };
  }
}

/** 401 统一拦截：清 token 后跳登录，避免每个页面各自处理（frontend-design.md §1.2） */
function handleUnauthorized(): void {
  setToken(null);
  const here = window.location.pathname + window.location.search;
  if (!here.startsWith('/login')) {
    window.location.replace(`/login?next=${encodeURIComponent(here)}`);
  }
}

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'DELETE';
  body?: unknown;
  signal?: AbortSignal;
}

export async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const method = opts.method ?? 'GET';

  if (USE_MOCK) {
    const res = await mockHandle(method, path, opts.body, getToken());
    if (res.status === 401) {
      handleUnauthorized();
      throw new ApiError(401, res.body.error);
    }
    if (res.status >= 400) throw new ApiError(res.status, (res.body as ApiErrorBody).error);
    return res.body as T;
  }

  const headers: Record<string, string> = { Accept: 'application/json' };
  const auth = getToken();
  if (auth) headers.Authorization = `Bearer ${auth}`;
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json';

  const res = await fetch(API_BASE + path, {
    method,
    headers,
    body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
    signal: opts.signal,
  });

  if (res.status === 401) {
    handleUnauthorized();
    throw new ApiError(401, { code: 'unauthorized', message: '登录已失效', retryable: false, charged: false });
  }

  if (res.status === 204) return undefined as T;

  const text = await res.text();
  const parsed: unknown = text ? JSON.parse(text) : null;

  if (!res.ok) {
    const err = (parsed as ApiErrorBody | null)?.error;
    throw new ApiError(res.status, err ?? {
      code: 'internal_error',
      message: `请求失败（${res.status}）`,
      retryable: true,
      charged: false,
    });
  }

  return parsed as T;
}

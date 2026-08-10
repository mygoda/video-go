import type { ApiErrorBody, TaskError } from './types';
import { getToken, setToken } from './token';
import { mockHandle } from '@/mock/backend';

/**
 * 离线开发模式，必须在 dev 下显式 VITE_USE_MOCK=true 才启用；
 * 缺省与任何其他取值都走真后端。
 *
 * 前面那半个 import.meta.env.DEV 不是保险丝，是熔断：生产构建里它是编译期常量
 * false，整个 mockHandle 分支连同 @/mock/backend 会被摇掉，产物里一个字节的假
 * 后端都不剩。少了它，构建时手滑一个环境变量就能把一整个假 App 发上线——
 * 那正是「用户端不许再看到 mock」这条线最容易被绕过的地方。
 */
export const USE_MOCK = import.meta.env.DEV && import.meta.env.VITE_USE_MOCK === 'true';
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
  /** 传 FormData 走 multipart 直传字节，其余一律按 JSON 序列化 */
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
  // FormData 的 Content-Type 只能由浏览器写。boundary 是 fetch 序列化 body 时才生成的
  // 随机串，手写一个 `multipart/form-data` 头等于交出一个没有 boundary 的类型，
  // 后端的 ParseMultipartForm 会当场解析失败。所以这里是「刻意不设」，不是漏了。
  const isForm = opts.body instanceof FormData;
  if (opts.body !== undefined && !isForm) headers['Content-Type'] = 'application/json';

  const res = await fetch(API_BASE + path, {
    method,
    headers,
    body: opts.body === undefined ? undefined : isForm ? (opts.body as FormData) : JSON.stringify(opts.body),
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

function blobAsDataURL(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(reader.error ?? new Error('读取文件失败'));
    reader.readAsDataURL(blob);
  });
}

/**
 * multipart 提交。POST /api/uploads 只认 multipart —— 它进门第一步就是
 * ParseMultipartForm，再用 FormFile("file") 取件，JSON body 到不了业务代码。
 *
 * 刻意不设 Content-Type：boundary 只有 fetch 自己知道，手写这个头会让服务端
 * 解析出一个空表单。
 *
 * mock 那条分支走 request：假后端收的是 data_url，与真接口形状本来就不同，
 * 在这里换形状比让离线开发模式整块失效好。
 */
export async function postForm<T>(path: string, form: FormData): Promise<T> {
  if (USE_MOCK) {
    const file = form.get('file');
    if (!(file instanceof Blob)) {
      throw new ApiError(400, { code: 'invalid_param', message: '缺少文件', retryable: false, charged: false });
    }
    const name = file instanceof File ? file.name : 'upload';
    return request<T>(path, { method: 'POST', body: { data_url: await blobAsDataURL(file), name } });
  }

  const headers: Record<string, string> = { Accept: 'application/json' };
  const auth = getToken();
  if (auth) headers.Authorization = `Bearer ${auth}`;

  const res = await fetch(API_BASE + path, { method: 'POST', headers, body: form });

  if (res.status === 401) {
    handleUnauthorized();
    throw new ApiError(401, { code: 'unauthorized', message: '登录已失效', retryable: false, charged: false });
  }

  const text = await res.text();
  const parsed: unknown = text ? JSON.parse(text) : null;

  if (!res.ok) {
    const err = (parsed as ApiErrorBody | null)?.error;
    throw new ApiError(res.status, err ?? {
      code: 'internal_error',
      message: `上传失败（${res.status}）`,
      retryable: true,
      charged: false,
    });
  }

  return parsed as T;
}

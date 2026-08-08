import type {
  AdminTask,
  AdminUser,
  Asset,
  AuthResponse,
  CanvasChatResponse,
  CanvasOp,
  CanvasProject,
  CanvasState,
  CircuitBreakerState,
  CleanupResult,
  CleanupTarget,
  CreateTaskResponse,
  CreditLedgerEntry,
  Me,
  ModelConfig,
  ModelConfigUpsert,
  Paged,
  ProbeResult,
  Provider,
  ProviderUpsert,
  Skill,
  StorageUsage,
  Task,
  TaskStats,
  UploadResponse,
  UserRole,
  UserStatus,
} from './types';
import type { JsonValue, ModelCapabilitySchema } from '@/schema/types';
import { request } from './client';

export const api = {
  login: (username: string, password: string) =>
    request<AuthResponse>('/auth/login', { method: 'POST', body: { username, password } }),

  register: (username: string, password: string) =>
    request<AuthResponse>('/auth/register', { method: 'POST', body: { username, password } }),

  logout: () => request<void>('/auth/logout', { method: 'POST' }),

  me: () => request<Me>('/me'),

  creditLedger: () => request<Paged<CreditLedgerEntry>>('/me/credit-ledger'),

  changePassword: (old_password: string, new_password: string) =>
    request<void>('/me/password', { method: 'POST', body: { old_password, new_password } }),

  models: (modality: 'image' | 'video') =>
    request<{ models: ModelCapabilitySchema[] }>(`/models?modality=${modality}`).then((r) => r.models),

  skills: () => request<{ skills: Skill[] }>('/skills').then((r) => r.skills),

  upload: (dataUrl: string, name: string) =>
    request<UploadResponse>('/uploads', { method: 'POST', body: { data_url: dataUrl, name } }),

  tasks: () => request<Paged<Task>>('/tasks').then((r) => r.items),

  activeTasks: () => request<Paged<Task>>('/tasks?status=active').then((r) => r.items),

  createTask: (payload: {
    model_id: string;
    prompt: string;
    inputs: Record<string, string[]>;
    params: Record<string, JsonValue>;
    client_token: string;
    canvas_id?: string;
    card_id?: string;
  }) => request<CreateTaskResponse>('/tasks', { method: 'POST', body: payload }),

  cancelTask: (id: string) => request<void>(`/tasks/${id}/cancel`, { method: 'POST' }),

  deleteTask: (id: string) => request<void>(`/tasks/${id}`, { method: 'DELETE' }),

  assets: (params: { type?: string; cursor?: string | null }) => {
    const q = new URLSearchParams();
    if (params.type && params.type !== 'all') q.set('type', params.type);
    if (params.cursor) q.set('cursor', params.cursor);
    const qs = q.toString();
    return request<Paged<Asset> & { total: number }>(`/assets${qs ? `?${qs}` : ''}`);
  },

  asset: (id: string) => request<Asset>(`/assets/${id}`),

  deleteAsset: (id: string) => request<void>(`/assets/${id}`, { method: 'DELETE' }),

  projects: () => request<{ items: CanvasProject[] }>('/projects').then((r) => r.items),

  createProject: (name: string) =>
    request<CanvasProject>('/projects', { method: 'POST', body: { name } }),

  canvas: (projectId: string) => request<CanvasState>(`/projects/${projectId}/canvas`),

  patchCanvas: (projectId: string, base_revision: number, ops: CanvasOp[]) =>
    request<{ revision: number }>(`/projects/${projectId}/canvas`, {
      method: 'PATCH',
      body: { base_revision, ops },
    }),

  canvasChat: (projectId: string, message: string, ref_card_ids: string[], skill_id: string | null) =>
    request<CanvasChatResponse>(`/canvas/${projectId}/chat`, {
      method: 'POST',
      body: { message, ref_card_ids, skill_id },
    }),
};

/** 管理后台。路由前缀固定 /admin，越权由后端返回 403，前端路由守卫只是第一道 */
export const adminApi = {
  providers: () => request<{ providers: Provider[] }>('/admin/providers').then((r) => r.providers),

  createProvider: (payload: ProviderUpsert) =>
    request<Provider>('/admin/providers', { method: 'POST', body: payload }),

  updateProvider: (id: string, payload: ProviderUpsert) =>
    request<Provider>(`/admin/providers/${id}`, { method: 'PATCH', body: payload }),

  deleteProvider: (id: string) => request<void>(`/admin/providers/${id}`, { method: 'DELETE' }),

  models: () => request<{ models: ModelConfig[] }>('/admin/models').then((r) => r.models),

  model: (id: string) => request<ModelConfig>(`/admin/models/${id}`),

  createModel: (payload: ModelConfigUpsert) =>
    request<ModelConfig>('/admin/models', { method: 'POST', body: payload }),

  updateModel: (id: string, payload: ModelConfigUpsert) =>
    request<ModelConfig>(`/admin/models/${id}`, { method: 'PATCH', body: payload }),

  deleteModel: (id: string) => request<void>(`/admin/models/${id}`, { method: 'DELETE' }),

  probeModel: (id: string, dry_run: boolean) =>
    request<ProbeResult>(`/admin/models/${id}/probe`, { method: 'POST', body: { dry_run } }),

  users: (q: string) =>
    request<Paged<AdminUser>>(`/admin/users${q ? `?q=${encodeURIComponent(q)}` : ''}`),

  updateUser: (id: string, patch: { role?: UserRole; status?: UserStatus; storage_quota_bytes?: number }) =>
    request<AdminUser>(`/admin/users/${id}`, { method: 'PATCH', body: patch }),

  adjustCredits: (id: string, payload: { amount: number; reason: string; idempotency_key: string }) =>
    request<CreditLedgerEntry>(`/admin/users/${id}/credits`, { method: 'POST', body: payload }),

  userLedger: (id: string) => request<Paged<CreditLedgerEntry>>(`/admin/users/${id}/credit-ledger`),

  tasks: (filter: { status?: string; error_code?: string }) => {
    const q = new URLSearchParams();
    if (filter.status) q.set('status', filter.status);
    if (filter.error_code) q.set('error_code', filter.error_code);
    const qs = q.toString();
    return request<Paged<AdminTask>>(`/admin/tasks${qs ? `?${qs}` : ''}`);
  },

  taskStats: (window: string) => request<TaskStats>(`/admin/tasks/stats?window=${window}`),

  retryTask: (id: string) => request<AdminTask>(`/admin/tasks/${id}/retry`, { method: 'POST' }),

  cancelTask: (id: string) => request<AdminTask>(`/admin/tasks/${id}/cancel`, { method: 'POST' }),

  circuitBreakers: () =>
    request<{ breakers: CircuitBreakerState[] }>('/admin/circuit-breakers').then((r) => r.breakers),

  storageUsage: () => request<StorageUsage>('/admin/storage/usage'),

  storageCleanup: (payload: { dry_run: boolean; older_than_days: number; target: CleanupTarget }) =>
    request<CleanupResult>('/admin/storage/cleanup', { method: 'POST', body: payload }),
};

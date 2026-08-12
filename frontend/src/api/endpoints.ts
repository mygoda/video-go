import type {
  AdminTask,
  AdminUser,
  Asset,
  AuthResponse,
  CanvasChatResponse,
  CanvasOp,
  CanvasProject,
  CanvasRefineResponse,
  CanvasState,
  CanvasStoryboardResponse,
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
import { postForm, request } from './client';

export const api = {
  login: (username: string, password: string) =>
    request<AuthResponse>('/auth/login', { method: 'POST', body: { username, password } }),

  register: (username: string, password: string) =>
    request<AuthResponse>('/auth/register', { method: 'POST', body: { username, password } }),

  // 没有 logout：JWT 是无状态的，服务端没有可作废的会话。
  // 退出登录 = 前端丢掉 token 并清空查询缓存，见 ProfilePage。

  me: () => request<Me>('/me'),

  creditLedger: () => request<Paged<CreditLedgerEntry>>('/me/credit-ledger'),

  changePassword: (old_password: string, new_password: string) =>
    request<void>('/me/password', { method: 'POST', body: { old_password, new_password } }),

  // text 档是写剧本用的 chat 模型目录。它与 image / video 两档共用一个端点，
  // 但只有画布的剧本这一步会拉它 —— 生成器的模型下拉传的仍是各自的 modality，
  // 多出来的这一档不会漏进那两个页面。
  models: (modality: 'image' | 'video' | 'text') =>
    request<{ models: ModelCapabilitySchema[] }>(`/models?modality=${modality}`).then((r) => r.models),

  skills: () => request<{ skills: Skill[] }>('/skills').then((r) => r.skills),

  // 直传字节，不走 base64 data URL：后端按真实字节嗅探 MIME，
  // 且 base64 会把传输量放大 33%，视频素材上还得先在内存里拼出整个字符串。
  upload: (file: File) => {
    const form = new FormData();
    form.append('file', file, file.name);
    return request<UploadResponse>('/uploads', { method: 'POST', body: form });
  },

  /**
   * 上传一份字节。slot 只是给回收扫描留的标记，约束仍按提交任务那一步的槽声明校验。
   *
   * 与上面那个 upload 并存而不是替掉它：那一条是 InputSlotStrip 在用的路径，
   * 换掉它等于顺手改一个不属于本票的组件。
   */
  uploadFile: (blob: Blob, name: string, slot?: string) => {
    const form = new FormData();
    form.append('file', blob, name);
    if (slot) form.append('slot', slot);
    return postForm<UploadResponse>('/uploads', form);
  },

  tasks: () => request<Paged<Task>>('/tasks?limit=100').then((r) => r.items),

  activeTasks: () => request<Paged<Task>>('/tasks?status=active').then((r) => r.items),

  task: (id: string) => request<Task>(`/tasks/${id}`),

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

  // model_id 是用户这次点名用哪个模型写剧本。null 表示没点名，此时整个键
  // 不进 JSON（undefined 会被 JSON.stringify 丢掉），后端按它自己的默认规则选 ——
  // 前端替它"默认选第一个再传上去"就是偷偷改了默认模型。
  canvasChat: (
    projectId: string,
    message: string,
    ref_card_ids: string[],
    skill_id: string | null,
    model_id: string | null,
  ) =>
    request<CanvasChatResponse>(`/canvas/${projectId}/chat`, {
      method: 'POST',
      body: { message, ref_card_ids, skill_id, model_id: model_id ?? undefined },
    }),

  // 按一句指令定向改写一张剧本卡。改动前的正文由服务端追加进 params.versions，
  // 所以调用方拿到 200 之后必须重新拉一次画布才看得到新版本（响应里的 card 不带 params）。
  refineScriptCard: (projectId: string, cardId: string, instruction: string, model_id: string | null) =>
    request<CanvasRefineResponse>(`/canvas/${projectId}/cards/${cardId}/refine`, {
      method: 'POST',
      body: { instruction, model_id: model_id ?? undefined },
    }),

  // 按一句指令让模型润色一张镜头卡（同时改画面描述与台词）。返回后前端重拉画布。
  refineShotCard: (projectId: string, cardId: string, instruction: string, model_id: string | null) =>
    request<CanvasRefineResponse>(`/canvas/${projectId}/cards/${cardId}/refine-shot`, {
      method: 'POST',
      body: { instruction, model_id: model_id ?? undefined },
    }),

  // 拆分镜由用户在剧本卡上主动发起，剧本那一步不会自己走到这里。
  // shot_count 的上限（12）后端会 400 顶回来，前端在流程条上提前拦一道：
  // 让人点了才知道自己填的数不合法，等于没有上限。
  storyboard: (projectId: string, card_id: string, shot_count: number) =>
    request<CanvasStoryboardResponse>(`/canvas/${projectId}/storyboard`, {
      method: 'POST',
      body: { card_id, shot_count },
    }),

  // card_ids 是**有序**的，顺序即成片里的片段顺序，后端不排序也不去歧义。
  // client_token 由调用方给：断网重发拿回同一条任务，而不是又拼一次、又扣一次钱。
  //
  // mute / subtitles 是两个布尔，不是字幕正文：正文由后端按卡片顺序取各镜的
  // 台词、按各段的实际时长排时间轴。前端算这件事得先把每件产物的 duration_ms
  // 逐个拉一遍，而后端提交那一步本来就要逐件查资产的归属。
  compose: (
    projectId: string,
    card_ids: string[],
    title: string,
    client_token: string,
    options: { mute: boolean; subtitles: boolean },
  ) =>
    request<CreateTaskResponse>(`/canvas/${projectId}/compose`, {
      method: 'POST',
      body: { card_ids, title, client_token, ...options },
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

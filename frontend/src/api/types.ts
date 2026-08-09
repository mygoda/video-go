/**
 * 后端 REST 契约的前端侧类型。以 docs/openapi.yaml 为准。
 * 仍与契约有出入的地方记在 frontend/README.md「与契约的对齐」。
 */
import type { JsonValue, ModelCapabilitySchema, TaskError, TaskErrorCode, TaskStatus } from '@/schema/types';

export type { TaskError, TaskErrorCode, TaskStatus };

export interface ApiErrorBody {
  error: {
    code: string;
    message: string;
    field_errors?: { key: string; message: string }[];
    retryable: boolean;
    charged: boolean;
  };
}

export interface AuthResponse {
  token: string;
  user: User;
}

export type UserRole = 'user' | 'admin';
export type UserStatus = 'active' | 'disabled';

export interface User {
  id: string;
  username: string;
  created_at: string;
}

export interface Me extends User {
  role: UserRole;
  credits: number;
  /** 排队中任务的预扣额度，不能再拿去下单 */
  credits_held: number;
  storage_used: number;
  storage_quota: number;
}

export type CreditLedgerType = 'hold' | 'charge' | 'refund' | 'topup' | 'adjust';

export interface CreditLedgerEntry {
  id: string;
  type: CreditLedgerType;
  /** 正数进账、负数消耗 */
  amount: number;
  balance_after: number;
  created_at: string;
  user_id?: string;
  task_id?: string;
  reason?: string;
  /** 管理员操作时记录，用户自发行为为 null */
  operator_id?: string | null;
}

/**
 * type=text 是管线内部产物（分镜脚本的对话回复、合成任务的片段清单）。
 * 后端的资产库列表已经把它挡在外面了，这里保留这个成员只是因为
 * `GET /api/assets?type=text` 与按 id 取单件仍然会返回它。
 */
export type AssetType = 'image' | 'video' | 'text';

export interface Asset {
  id: string;
  type: AssetType;
  /** 分级渲染依赖多档 URL（frontend-design.md §5.5）。mock 下是 data/占位 URL */
  thumb_512: string | null;
  original: string;
  poster?: string | null;
  /** text 产物没有尺寸，后端整个字段不返回 */
  width?: number;
  height?: number;
  /** 毫秒。字段名跟后端走，别写成 duration_seconds——那样读到的永远是 undefined */
  duration_ms?: number;
  created_at: string;
  /** 「做同款」与「单卡重跑」的唯一依据 */
  source: {
    model_id: string;
    /** 后端 source 里没有这个字段，展示前必须兜底 */
    model_name?: string;
    prompt: string;
    params: Record<string, JsonValue>;
    input_asset_ids: string[];
  };
}

/**
 * 一件资产在卡片上该显示哪张图。
 *
 * thumb_512 会是 null，而且不是异常：派生是尽力而为的（视频缺 ffmpeg、
 * webp 解不了都会跳过），上传提升成的资产更是从来没派生过。原图当兜底比
 * 留一个空 src 强——空 src 渲染出来是一块纯色，和「这条产物是假的」
 * 在用户眼里没有区别。
 *
 * text 产物返回 null：它的 original 是一份 text/plain，喂给 `<img>` 必然裂图。
 * 返回 null 而不是兜底到 original，是为了让 `<img src>` 的类型不再接受它 ——
 * 编译期就挡住「忘了写 text 分支」，正文渲染见 AssetTextBody。
 */
export function assetPreview(asset: Asset): string | null {
  if (asset.type === 'text') return null;
  return asset.poster ?? asset.thumb_512 ?? asset.original;
}

export interface Task {
  id: string;
  status: TaskStatus;
  model_id: string;
  /** text 是分镜拆解这类平台内部任务，用户不会主动提交，但它会出现在任务列表里 */
  modality: 'image' | 'video' | 'text';
  prompt: string;
  params: Record<string, JsonValue>;
  /** 后端不回显任务的输入槽，只在提交时用得上 */
  inputs?: Record<string, string[]>;
  estimated_cost: number;
  eta_seconds: number | null;
  queue_position: number | null;
  progress: number | null;
  /** 只有 succeeded 的任务才有；排队 / 失败时后端整个字段不返回 */
  assets?: Asset[];
  error: TaskError | null;
  created_at: string;
  canvas_id?: string;
  /** 前端乐观态专用：提交中的临时卡片。后端永远不返回这个值 */
  local?: 'submitting' | 'failed';
  client_token?: string;
}

export interface CreateTaskResponse {
  task_id: string;
  status: TaskStatus;
  estimated_cost: number;
  queue_position: number | null;
  eta_seconds: number | null;
}

export interface UploadResponse {
  upload_id: string;
  preview_url: string;
}

export interface Paged<T> {
  items: T[];
  next_cursor: string | null;
}

export interface Skill {
  id: string;
  name: string;
  description: string;
  default_model_id: string;
  default_params: Record<string, JsonValue>;
}

export interface CanvasProject {
  id: string;
  name: string;
  revision: number;
  card_count: number;
  cover_asset_id: string | null;
  updated_at: string;
}

export type CardKind = 'text' | 'image' | 'video';

export interface CanvasCard {
  id: string;
  kind: CardKind;
  x: number;
  y: number;
  w: number;
  h: number;
  z: number;
  task_id?: string;
  asset_id?: string;
  /** 后端 Card 目前没有这个字段，提交后会被丢弃 —— 渲染一律走 cardTitle() 兜底 */
  title?: string;
  text?: string;
  model_id?: string;
  prompt?: string;
  params?: Record<string, JsonValue>;
  /** 血缘：这张卡由哪些卡片作为输入生成。不画连线，只在选中时高亮 */
  refs: string[];
  /** 后端可能整个字段不返回，读之前必须判空 */
  history?: { asset_id: string; prompt: string; at: number }[];
  auto_placed: boolean;
  /** RFC3339 字符串。后端是 time.Time，发数字会被 Time.UnmarshalJSON 顶回 400 */
  created_at: string;
}

export interface CanvasMessage {
  id: string;
  role: 'user' | 'assistant' | 'system';
  content: string;
  ref_card_ids: string[];
  created_at: string;
  skill_id?: string;
  task_ids?: string[];
}

export interface CanvasViewport {
  x: number;
  y: number;
  k: number;
}

export interface CanvasState {
  revision: number;
  cards: CanvasCard[];
  conversation: CanvasMessage[];
  viewport?: CanvasViewport;
}

export type CanvasOp =
  | { type: 'card.create'; card: CanvasCard }
  | { type: 'card.move'; id: string; x: number; y: number }
  | { type: 'card.update'; id: string; patch: Partial<CanvasCard> }
  | { type: 'card.delete'; id: string }
  | { type: 'viewport.set'; viewport: CanvasViewport };

export interface CanvasChatResponse {
  message_id: string;
  revision: number;
  task_ids?: string[];
}

export interface StreamEvent {
  event:
    | 'task.updated'
    | 'task.succeeded'
    | 'task.failed'
    | 'credit.updated'
    | 'canvas.card.updated'
    | 'ping';
  id?: string;
  data: Record<string, JsonValue> & {
    task_id?: string;
    status?: TaskStatus;
    progress?: number | null;
    queue_position?: number | null;
    eta_seconds?: number | null;
    balance?: number;
    canvas_id?: string;
    card_id?: string;
    asset_id?: string;
  };
}

/* ─────────────────────────── 管理后台（/api/admin/*） ─────────────────────────── */

export type ProtocolFamily = 'chat' | 'images' | 'video' | 'mock' | 'compose' | 'predictions';
export type VideoProtocol = 'ark' | 'openai_video' | 'google_lro' | 'mock' | 'predictions';
export type AuthStyle = 'bearer' | 'header' | 'query';

/**
 * 用户端目录可见性，与 capability.enabled 是两回事：
 * enabled=false 是停用（谁都用不了），internal 是「能用，但不摆进用户的模型下拉」。
 * QA 夹具、mock 驱动、画布内部调用的合成模型都是 internal。
 */
export type ModelVisibility = 'public' | 'internal';

export interface Provider {
  id: string;
  name: string;
  base_url: string;
  /**
   * **环境变量名，不是密钥本身。** 密钥只从 env 读，不入库、不回显。
   * 界面上能看到的只有这个名字和 credential_present。
   */
  credential_ref: string;
  auth_style?: AuthStyle;
  auth_header_name?: string;
  enabled: boolean;
  timeout_ms?: number;
  max_concurrency?: number;
  /** 对应 env 变量当前是否已配置。只读布尔，永远拿不到明文 */
  credential_present?: boolean;
  created_at?: string;
}

export type ProviderUpsert = Omit<Provider, 'credential_present' | 'created_at'>;

export interface MappingRule {
  from?: string;
  const?: JsonValue;
  to: string;
  value_map?: Record<string, JsonValue>;
  wrap?: 'raw' | 'text_part' | 'image_url_part' | 'video_url_part';
  cast?: 'string' | 'int' | 'float' | 'bool' | 'seconds_suffix';
  omit_when_empty?: boolean;
}

export interface RequestMapping {
  body_template?: Record<string, JsonValue>;
  rules?: MappingRule[];
}

export interface ModelConfig {
  id: string;
  provider_id: string;
  upstream_model: string;
  protocol_family: ProtocolFamily;
  /** 仅 protocol_family=video 时必填 */
  video_protocol?: VideoProtocol | null;
  visibility?: ModelVisibility;
  capability: ModelCapabilitySchema;
  request_mapping?: RequestMapping;
  poll_interval_seconds?: number | null;
  updated_at?: string;
}

export type ModelConfigUpsert = Omit<ModelConfig, 'updated_at'>;

export interface ProbeResult {
  ok: boolean;
  dry_run?: boolean;
  /** 已脱敏，不含凭证 */
  rendered_request?: Record<string, JsonValue>;
  upstream_status?: number | null;
  error?: TaskError;
  latency_ms?: number | null;
}

export interface AdminUser extends Me {
  status: UserStatus;
  task_count?: number;
  last_active_at?: string | null;
}

export interface AdminTask extends Task {
  user_id?: string;
  username?: string;
  provider_id?: string;
  attempt?: number;
  upstream_ref?: string | null;
  /** 归一化前的上游原始状态，排障用 */
  upstream_status_raw?: string | null;
}

export interface TaskStats {
  window: string;
  queued: number;
  running: number;
  oldest_queued_age_seconds?: number | null;
  by_status: Record<string, number>;
  /** 失败分布，key 为 TaskErrorCode */
  by_error_code: Record<string, number>;
  by_model?: { model_id: string; total: number; failed: number; p50_duration_seconds?: number | null }[];
}

export interface CircuitBreakerState {
  provider_id: string;
  state: 'closed' | 'open' | 'half_open';
  failure_count?: number;
  opened_at?: string | null;
  next_probe_at?: string | null;
}

export interface StorageUsage {
  total_bytes: number;
  asset_count: number;
  upload_pending_bytes?: number;
  soft_deleted_bytes?: number;
  top_users?: { user_id: string; username: string; used_bytes: number; quota_bytes: number }[];
}

export type CleanupTarget = 'orphan_files' | 'expired_uploads' | 'soft_deleted_assets';

export interface CleanupResult {
  dry_run: boolean;
  candidate_count: number;
  reclaimed_bytes: number;
  samples?: string[];
}

import type {
  Asset,
  CanvasProject,
  CanvasState,
  CreditLedgerEntry,
  ModelConfig,
  Provider,
  Skill,
  Task,
  UserRole,
  UserStatus,
} from '@/api/types';
import type { ModelCapabilitySchema } from '@/schema/types';
import { artUrl } from './art';
import modelsJson from './models.json';

export interface MockUser {
  id: string;
  username: string;
  password: string;
  role: UserRole;
  status: UserStatus;
  created_at: string;
  credits: number;
  credits_held: number;
  storage_used: number;
  storage_quota: number;
  last_active_at: string | null;
}

/** 后端的 tasks 表是有归属的，mock 也照做，否则管理端的「按用户看任务」无从谈起 */
export interface MockTask extends Task {
  user_id: string;
  attempt: number;
  upstream_ref: string | null;
  upstream_status_raw: string | null;
}

export interface MockUpload {
  preview_url: string;
  bytes: number;
  created_at: string;
}

/** 软删除的资产先进回收站，管理端的存储清理才有东西可清 */
export interface TrashEntry {
  id: string;
  bytes: number;
  deleted_at: string;
}

export interface MockDb {
  version: number;
  users: MockUser[];
  sessions: Record<string, string>;
  ledger: CreditLedgerEntry[];
  assets: Asset[];
  tasks: MockTask[];
  projects: CanvasProject[];
  canvases: Record<string, CanvasState>;
  uploads: Record<string, MockUpload>;
  providers: Provider[];
  modelConfigs: ModelConfig[];
  trash: TrashEntry[];
  /** 手工充值的幂等键 → 已产生的流水 id */
  idempotency: Record<string, string>;
}

const STORAGE_KEY = 'aigc_pool_mock_db';
const VERSION = 6;

const CAPABILITIES = (modelsJson as { models: ModelCapabilitySchema[] }).models;

/** capability.vendor → provider id。真后端由外键给出，mock 里按 vendor 归拢 */
const PROVIDER_OF_VENDOR: Record<string, string> = {
  volcengine: 'pv_ark',
  minimax: 'pv_minimax',
  internal: 'pv_internal',
};

export function uid(prefix: string): string {
  return `${prefix}_${Math.random().toString(36).slice(2, 10)}`;
}

function iso(minutesAgo: number): string {
  return new Date(Date.now() - minutesAgo * 60_000).toISOString();
}

function imageAsset(seed: string, prompt: string, aspect: string, minutesAgo: number): Asset {
  const [w, h] = aspect.split(':').map(Number);
  const width = w >= h ? 1024 : Math.round((1024 * w) / h);
  const height = h >= w ? 1024 : Math.round((1024 * h) / w);
  return {
    id: uid('a'),
    type: 'image',
    thumb_512: artUrl(seed, Math.round(width / 2), Math.round(height / 2)),
    original: artUrl(seed, width, height),
    width,
    height,
    created_at: iso(minutesAgo),
    source: {
      model_id: 'seedream-5-pro',
      model_name: 'Seedream 5.0 Pro',
      prompt,
      params: { style_model: 'none', aspect, count: 1 },
      input_asset_ids: [],
    },
  };
}

function videoAsset(seed: string, prompt: string, minutesAgo: number): Asset {
  return {
    id: uid('a'),
    type: 'video',
    thumb_512: artUrl(seed, 512, 288),
    original: artUrl(seed, 1280, 720),
    poster: artUrl(seed, 1280, 720),
    width: 1280,
    height: 720,
    duration_ms: 5000,
    created_at: iso(minutesAgo),
    source: {
      model_id: 'hailuo-2-3',
      model_name: '海螺 2.3',
      prompt,
      params: { resolution: '768p', mode: 'standard', duration: 5 },
      input_asset_ids: [],
    },
  };
}

/** 资产没有 size 字段，mock 里按像素/时长估一个，够存储页有数可看 */
export function assetBytes(asset: Asset): number {
  if (asset.type === 'video') return Math.round(((asset.duration_ms ?? 5000) / 1000) * 1.8 * 1024 ** 2);
  return Math.round(((asset.width ?? 512) * (asset.height ?? 512)) / 4);
}

/**
 * 供应商的 base_url 一律用占位域名，credential_ref 是 **环境变量名**而不是密钥本身 ——
 * 真密钥只在后端进程的 env 里，任何一层都不落库、不回传。
 */
function seedProviders(): Provider[] {
  return [
    {
      id: 'pv_ark',
      name: '火山方舟',
      base_url: 'https://ark.example.com/api/v3',
      credential_ref: 'ARK_API_KEY',
      auth_style: 'bearer',
      enabled: true,
      timeout_ms: 60_000,
      max_concurrency: 8,
      credential_present: true,
      created_at: '2026-07-01T09:00:00Z',
    },
    {
      id: 'pv_minimax',
      name: 'MiniMax',
      base_url: 'https://minimax.example.com/v1',
      credential_ref: 'MINIMAX_API_KEY',
      auth_style: 'bearer',
      enabled: true,
      timeout_ms: 120_000,
      max_concurrency: 4,
      credential_present: true,
      created_at: '2026-07-01T09:00:00Z',
    },
    {
      id: 'pv_internal',
      name: '内部实验池',
      base_url: 'https://internal.example.com/v1',
      credential_ref: 'INTERNAL_API_KEY',
      auth_style: 'header',
      auth_header_name: 'X-Api-Key',
      enabled: true,
      timeout_ms: 30_000,
      max_concurrency: 2,
      credential_present: false,
      created_at: '2026-07-20T09:00:00Z',
    },
  ];
}

/** 模型配置直接内嵌 capability，`GET /models` 就是从这里投影出去的 —— 改配置立刻改目录 */
function seedModelConfigs(): ModelConfig[] {
  return CAPABILITIES.map((capability) => ({
    id: capability.id,
    provider_id: PROVIDER_OF_VENDOR[capability.vendor] ?? 'pv_internal',
    upstream_model: capability.id,
    protocol_family: capability.modality === 'video' ? 'video' : 'images',
    video_protocol: capability.modality === 'video' ? 'ark' : null,
    capability,
    poll_interval_seconds: capability.modality === 'video' ? 5 : null,
    updated_at: iso(60 * 24),
  }));
}

function seed(): MockDb {
  const user: MockUser = {
    id: 'u_1',
    username: 'yuchen',
    password: 'password',
    role: 'user',
    status: 'active',
    created_at: '2026-07-12T09:00:00Z',
    credits: 1240,
    credits_held: 0,
    storage_used: 3.2 * 1024 ** 3,
    storage_quota: 10 * 1024 ** 3,
    last_active_at: iso(4),
  };

  const admin: MockUser = {
    id: 'u_admin',
    username: 'admin',
    password: 'password',
    role: 'admin',
    status: 'active',
    created_at: '2026-07-01T09:00:00Z',
    credits: 9999,
    credits_held: 0,
    storage_used: 0.4 * 1024 ** 3,
    storage_quota: 50 * 1024 ** 3,
    last_active_at: iso(1),
  };

  const guest: MockUser = {
    id: 'u_2',
    username: 'linlin',
    password: 'password',
    role: 'user',
    status: 'active',
    created_at: '2026-07-28T04:00:00Z',
    credits: 60,
    credits_held: 0,
    storage_used: 0.9 * 1024 ** 3,
    storage_quota: 10 * 1024 ** 3,
    last_active_at: iso(60 * 30),
  };

  const assets: Asset[] = [
    imageAsset('s1', '深夜便利店的收银台，冷白色灯管，货架反光，窗外下着雨，赛博朋克质感', '16:9', 12),
    videoAsset('s2', '镜头缓慢推进，收银员抬头看向门口，霓虹灯在玻璃上流动', 40),
    imageAsset('s3', '雨夜街道，霓虹倒影', '9:16', 95),
    imageAsset('s4', '人物特写，冷调打光', '1:1', 150),
    imageAsset('s5', '货架反光细节', '3:4', 200),
    videoAsset('s6', '空镜，雨水顺着玻璃流下', 260),
    imageAsset('s7', '门口的男人，逆光', '16:9', 1500),
    imageAsset('s8', '收银台俯视', '9:16', 1600),
  ];

  const baseTask = (over: Partial<MockTask>): MockTask => ({
    id: uid('t'),
    user_id: user.id,
    status: 'succeeded',
    model_id: 'seedream-5-pro',
    modality: 'image',
    prompt: '',
    params: {},
    inputs: {},
    estimated_cost: 8,
    eta_seconds: 14,
    queue_position: null,
    progress: 1,
    assets: [],
    error: null,
    created_at: iso(30),
    attempt: 1,
    upstream_ref: null,
    upstream_status_raw: null,
    ...over,
  });

  // 三类失败各留一张卡片，让「失败三分类分别呈现」在首屏就能看到
  const tasks: MockTask[] = [
    baseTask({ prompt: assets[0].source.prompt, assets: [assets[0]], created_at: iso(12) }),
    baseTask({ prompt: assets[2].source.prompt, assets: [assets[2]], created_at: iso(95) }),
    baseTask({
      status: 'failed',
      prompt: '一段 7 秒的空镜',
      modality: 'image',
      created_at: iso(20),
      progress: null,
      assets: [],
      error: {
        code: 'invalid_param',
        message: '参数不合法',
        field_errors: [{ key: 'count', message: '该模型单次最多 4 张' }],
        retryable: true,
        charged: false,
      },
      params: { style_model: 'none', aspect: '16:9', count: 4 },
    }),
    baseTask({
      status: 'failed',
      prompt: '雨中奔跑的人群，长焦压缩',
      user_id: guest.id,
      created_at: iso(24),
      progress: null,
      assets: [],
      upstream_ref: 'ark_job_7f21c9',
      upstream_status_raw: 'RATE_LIMIT_EXCEEDED',
      attempt: 2,
      error: {
        code: 'upstream_rate_limited',
        message: '上游繁忙',
        retryable: true,
        charged: false,
        retry: { attempt: 2, max_attempts: 5 },
      },
    }),
    baseTask({
      status: 'failed',
      prompt: '（含受限内容的提示词）',
      user_id: guest.id,
      created_at: iso(26),
      progress: null,
      assets: [],
      upstream_status_raw: 'CONTENT_POLICY_VIOLATION',
      error: { code: 'content_rejected', message: '提示词包含受限内容', retryable: false, charged: false },
    }),
    baseTask({ status: 'canceled', prompt: '街角的旧书店', created_at: iso(28), progress: null, assets: [] }),
  ];

  const projectId = 'p_1';
  const projects: CanvasProject[] = [
    { id: projectId, name: '深夜便利店', revision: 42, card_count: 6, cover_asset_id: assets[0].id, updated_at: iso(2) },
    { id: 'p_2', name: '城市孤岛 MV', revision: 118, card_count: 28, cover_asset_id: assets[2].id, updated_at: iso(60 * 26) },
    { id: 'p_3', name: '产品广告 · 咖啡机', revision: 9, card_count: 6, cover_asset_id: assets[4].id, updated_at: iso(60 * 72) },
    { id: 'p_4', name: '末班地铁', revision: 203, card_count: 41, cover_asset_id: assets[6].id, updated_at: iso(60 * 24 * 8) },
  ];

  const scriptCardId = 'cd_1';
  const shot1 = 'cd_2';
  const canvases: Record<string, CanvasState> = {
    [projectId]: {
      revision: 42,
      cards: [
        {
          id: scriptCardId,
          kind: 'text',
          x: 60, y: 96, w: 220, h: 140, z: 1,
          title: '剧本',
          text: '深夜 23:47，便利店只剩收银员一人。门铃响起，进来一个浑身湿透的男人……',
          refs: [], history: [], auto_placed: true, created_at: iso(15),
        },
        {
          id: shot1,
          kind: 'image',
          x: 340, y: 96, w: 240, h: 160, z: 2,
          title: '分镜 1',
          asset_id: assets[0].id,
          model_id: assets[0].source.model_id,
          prompt: assets[0].source.prompt,
          params: assets[0].source.params,
          refs: [scriptCardId],
          history: [],
          auto_placed: true,
          created_at: iso(13),
        },
        {
          id: 'cd_3', kind: 'image', x: 620, y: 96, w: 240, h: 160, z: 3,
          title: '分镜 2', asset_id: assets[2].id,
          model_id: assets[2].source.model_id, prompt: assets[2].source.prompt, params: assets[2].source.params,
          refs: [scriptCardId], history: [], auto_placed: true, created_at: iso(12),
        },
        {
          id: 'cd_4', kind: 'text', x: 60, y: 328, w: 220, h: 140, z: 4,
          title: '分镜脚本',
          text: '1. 全景，店内冷光\n2. 特写，雨水滴落\n3. 过肩，男人递出纸条',
          refs: [], history: [], auto_placed: true, created_at: iso(10),
        },
        {
          id: 'cd_5', kind: 'image', x: 340, y: 328, w: 240, h: 160, z: 5,
          title: '分镜 3', asset_id: assets[4].id,
          model_id: assets[4].source.model_id, prompt: assets[4].source.prompt, params: assets[4].source.params,
          refs: [shot1], history: [], auto_placed: true, created_at: iso(8),
        },
        {
          id: 'cd_6', kind: 'video', x: 620, y: 328, w: 240, h: 160, z: 6,
          title: '视频 1', asset_id: assets[1].id,
          model_id: assets[1].source.model_id, prompt: assets[1].source.prompt, params: assets[1].source.params,
          refs: [shot1], history: [], auto_placed: true, created_at: iso(7),
        },
      ],
      conversation: [
        { id: 'm_1', role: 'user', content: '按剧本拆 6 个分镜，冷色调，赛博朋克质感', ref_card_ids: [], created_at: iso(15) },
        { id: 'm_2', role: 'assistant', content: '已生成 6 张分镜图，落在画布右侧。', ref_card_ids: [], created_at: iso(14) },
      ],
    },
  };
  for (const p of projects) {
    if (!canvases[p.id]) canvases[p.id] = { revision: 1, cards: [], conversation: [] };
  }

  return {
    version: VERSION,
    users: [user, admin, guest],
    sessions: {},
    ledger: [
      { id: uid('l'), type: 'charge', amount: -16, balance_after: 1240, user_id: user.id, reason: '图片生成 ×2', created_at: iso(12) },
      { id: uid('l'), type: 'charge', amount: -120, balance_after: 1256, user_id: user.id, reason: '视频生成 · 5s', created_at: iso(40) },
      { id: uid('l'), type: 'refund', amount: 120, balance_after: 1376, user_id: user.id, reason: '任务失败退回 · 上游限流', created_at: iso(41) },
      { id: uid('l'), type: 'charge', amount: -32, balance_after: 1256, user_id: user.id, reason: '图片生成 ×4', created_at: iso(200) },
      { id: uid('l'), type: 'topup', amount: 1000, balance_after: 1288, user_id: user.id, reason: '管理员发放', operator_id: admin.id, created_at: iso(60 * 24 * 26) },
      { id: uid('l'), type: 'topup', amount: 100, balance_after: 60, user_id: guest.id, reason: '新用户赠送', created_at: iso(60 * 24 * 11) },
    ],
    assets,
    tasks,
    projects,
    canvases,
    uploads: {},
    providers: seedProviders(),
    modelConfigs: seedModelConfigs(),
    trash: [
      { id: uid('a'), bytes: 128 * 1024 ** 2, deleted_at: iso(60 * 24 * 45) },
      { id: uid('a'), bytes: 64 * 1024 ** 2, deleted_at: iso(60 * 24 * 38) },
      { id: uid('a'), bytes: 12 * 1024 ** 2, deleted_at: iso(60 * 3) },
    ],
    idempotency: {},
  };
}

export const SKILLS: Skill[] = [
  { id: 'sk_drama', name: '短剧', description: '按剧本拆分镜、统一风格', default_model_id: 'seedream-5-pro', default_params: {} },
  { id: 'sk_mv', name: 'MV', description: '按歌词生成画面节奏', default_model_id: 'seedream-5-pro', default_params: {} },
  { id: 'sk_ad', name: '产品广告', description: '产品图 + 卖点分镜', default_model_id: 'seedream-5-pro', default_params: {} },
];

let saveTimer: number | undefined;
let pendingWrite: MockDb | null = null;

function flush(): void {
  if (!pendingWrite) return;
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(pendingWrite));
  } catch {
    // 超出配额时放弃持久化，不影响本次会话
  }
  pendingWrite = null;
}

// 写入是防抖的，否则任务进度每 700ms 就要序列化一次整个库。
// 但离开页面时必须把欠的那一次补上，不然「登录后立刻硬跳转」会丢掉刚建的 session。
window.addEventListener('pagehide', flush);

function load(): MockDb {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw) {
      const parsed = JSON.parse(raw) as MockDb;
      if (parsed.version === VERSION) return parsed;
    }
  } catch {
    // 存储损坏就重新播种，mock 数据没有保留价值
  }
  const fresh = seed();
  save(fresh);
  return fresh;
}

export const db: MockDb = load();

export function save(next: MockDb = db): void {
  pendingWrite = next;
  window.clearTimeout(saveTimer);
  saveTimer = window.setTimeout(flush, 200);
}

export function resetDb(): void {
  localStorage.removeItem(STORAGE_KEY);
  window.location.reload();
}

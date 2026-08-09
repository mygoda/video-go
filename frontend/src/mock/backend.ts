/**
 * 内存 mock 后端。VITE_USE_MOCK=true 时 api/client.ts 把请求路由到这里，
 * 形状严格按 frontend-design.md §7 的诉求清单，DEM-62 的 openapi.yaml 落盘后以契约为准对齐。
 *
 * 只做「够跑通全链路」的模拟：任务会真的排队、推进度、出产物、扣积分、推事件。
 */
import type {
  AdminTask,
  AdminUser,
  ApiErrorBody,
  Asset,
  CanvasChatResponse,
  CanvasOp,
  CanvasState,
  CircuitBreakerState,
  CleanupResult,
  CleanupTarget,
  CreateTaskResponse,
  CreditLedgerEntry,
  ModelConfig,
  Paged,
  ProbeResult,
  Provider,
  StorageUsage,
  Task,
  TaskStats,
} from '@/api/types';
import type { JsonValue, ModelCapabilitySchema } from '@/schema/types';
import { estimateCost } from '@/schema/pricing';
import { artUrl } from './art';
import { SKILLS, assetBytes, db, save, uid, type MockTask, type MockUser } from './db';
import { emit } from './stream';

/**
 * 模型目录直接从管理端配置投影出来 —— 管理员新增一条 ModelConfig，
 * 生成器的模型下拉立刻能看到，这正是管理后台存在的理由。
 */
function catalog(): ModelCapabilitySchema[] {
  return db.modelConfigs.map((c) => c.capability);
}

function findCapability(id: string): ModelCapabilitySchema | undefined {
  return db.modelConfigs.find((c) => c.id === id)?.capability;
}

/** 真实 eta 可能是几分钟，mock 里压到 12s 上限，否则没法演示 */
const MAX_SIMULATED_SECONDS = 12;

interface MockResponse {
  status: number;
  body: any;
}

function ok(body: unknown = null): MockResponse {
  return { status: 200, body };
}

function fail(
  status: number,
  code: string,
  message: string,
  extra: Partial<ApiErrorBody['error']> = {},
): MockResponse {
  return { status, body: { error: { code, message, retryable: status >= 500, charged: false, ...extra } } };
}

function currentUser(token: string | null): MockUser | null {
  if (!token) return null;
  const userId = db.sessions[token];
  return db.users.find((u) => u.id === userId) ?? null;
}

function publicUser(u: MockUser) {
  return { id: u.id, username: u.username, created_at: u.created_at };
}

function meBody(u: MockUser) {
  return {
    ...publicUser(u),
    role: u.role,
    credits: u.credits,
    credits_held: u.credits_held,
    storage_used: u.storage_used,
    storage_quota: u.storage_quota,
  };
}

function adminUserBody(u: MockUser): AdminUser {
  return {
    ...meBody(u),
    status: u.status,
    task_count: db.tasks.filter((t) => t.user_id === u.id).length,
    last_active_at: u.last_active_at,
  };
}

function delay(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

/* ────────────────────────── 任务生命周期模拟 ────────────────────────── */

function makeAssets(task: Task, model: ModelCapabilitySchema): Asset[] {
  const countKey = model.pricing.multiplier_param;
  const raw = countKey ? Number(task.params[countKey]) : 1;
  const n = task.modality === 'video' ? 1 : Math.max(1, Math.min(Number.isFinite(raw) ? raw : 1, 8));
  const aspect = String(task.params.aspect ?? '1:1');
  const [aw, ah] = aspect.includes(':') ? aspect.split(':').map(Number) : [1, 1];

  return Array.from({ length: n }, (_, i) => {
    const seed = `${task.id}_${i}`;
    if (task.modality === 'video') {
      return {
        id: uid('a'),
        type: 'video' as const,
        thumb_512: artUrl(seed, 512, 288),
        original: artUrl(seed, 1280, 720),
        poster: artUrl(seed, 1280, 720),
        width: 1280,
        height: 720,
        duration_seconds: Number(task.params.duration ?? 5),
        created_at: new Date().toISOString(),
        source: {
          model_id: task.model_id,
          model_name: model.name,
          prompt: task.prompt,
          params: task.params,
          input_asset_ids: Object.values(task.inputs ?? {}).flat(),
        },
      };
    }
    const width = aw >= ah ? 1024 : Math.round((1024 * aw) / ah);
    const height = ah >= aw ? 1024 : Math.round((1024 * ah) / aw);
    return {
      id: uid('a'),
      type: 'image' as const,
      thumb_512: artUrl(seed, Math.round(width / 2), Math.round(height / 2)),
      original: artUrl(seed, width, height),
      width,
      height,
      created_at: new Date().toISOString(),
      source: {
        model_id: task.model_id,
        model_name: model.name,
        prompt: task.prompt,
        params: task.params,
        input_asset_ids: Object.values(task.inputs ?? {}).flat(),
      },
    };
  });
}

async function runTask(task: Task, model: ModelCapabilitySchema, cardId?: string): Promise<void> {
  // 取消发生在另一个 tick 里改同一个对象，所以每次 await 之后都要重新读
  const canceled = () => task.status === 'canceled';

  await delay(700);
  if (canceled()) return;

  task.status = 'running';
  task.queue_position = null;
  task.progress = 0;
  emit('task.updated', { task_id: task.id, status: 'running', progress: 0, eta_seconds: task.eta_seconds });

  const totalMs = Math.min(task.eta_seconds ?? 10, MAX_SIMULATED_SECONDS) * 1000;
  const step = 700;
  for (let elapsed = step; elapsed < totalMs; elapsed += step) {
    await delay(step);
    if (canceled()) return;
    task.progress = Math.min(0.92, elapsed / totalMs);
    emit('task.updated', {
      task_id: task.id,
      status: 'running',
      progress: task.progress,
      eta_seconds: Math.max(0, Math.round((totalMs - elapsed) / 1000)),
    });
  }

  await delay(step);
  if (canceled()) return;

  const assets = makeAssets(task, model);
  task.status = 'succeeded';
  task.progress = 1;
  task.assets = assets;
  db.assets.unshift(...assets);
  save();

  emit('task.succeeded', { task_id: task.id, assets: assets as unknown as JsonValue });
  if (task.canvas_id && cardId) {
    emit('canvas.card.updated', {
      canvas_id: task.canvas_id,
      card_id: cardId,
      status: 'succeeded',
      asset_id: assets[0].id,
    });
  }
}

/* ──────────────────────────────── 路由 ──────────────────────────────── */

export async function mockHandle(
  method: string,
  rawPath: string,
  body: any,
  token: string | null,
): Promise<MockResponse> {
  await delay(80);

  const [path, queryString = ''] = rawPath.split('?');
  const query = new URLSearchParams(queryString);
  const segments = path.split('/').filter(Boolean);
  const user = currentUser(token);

  /* --- 无需登录 --- */
  if (path === '/auth/login' && method === 'POST') {
    const found = db.users.find((u) => u.username === body?.username);
    if (!found || found.password !== body?.password) {
      return fail(401, 'invalid_credentials', '账号或密码不正确');
    }
    const t = uid('tok');
    db.sessions[t] = found.id;
    save();
    return ok({ token: t, user: publicUser(found) });
  }

  if (path === '/auth/register' && method === 'POST') {
    const username = String(body?.username ?? '').trim();
    const password = String(body?.password ?? '');
    if (username.length < 2) return fail(400, 'invalid_param', '用户名至少 2 位');
    if (password.length < 8) return fail(400, 'invalid_param', '密码至少 8 位');
    if (db.users.some((u) => u.username === username)) return fail(409, 'conflict', '该用户名已被占用');
    const created: MockUser = {
      id: uid('u'),
      username,
      password,
      role: 'user',
      status: 'active',
      created_at: new Date().toISOString(),
      credits: 500,
      credits_held: 0,
      storage_used: 0,
      storage_quota: 10 * 1024 ** 3,
      last_active_at: new Date().toISOString(),
    };
    db.users.push(created);
    db.ledger.unshift({
      id: uid('l'),
      type: 'topup',
      amount: 500,
      balance_after: created.credits,
      user_id: created.id,
      reason: '新用户赠送',
      created_at: created.created_at,
    });
    const t = uid('tok');
    db.sessions[t] = created.id;
    save();
    return ok({ token: t, user: publicUser(created) });
  }

  /* --- 模型目录不设防：未登录也要能浏览模型、渲染芯片，只有提交任务才要求登录 --- */
  if (path === '/models' && method === 'GET') {
    const modality = query.get('modality');
    const list = catalog()
      .filter((m) => !modality || m.modality === modality)
      .sort((a, b) => a.order - b.order || a.name.localeCompare(b.name));
    return ok({ models: list });
  }

  if (path === '/skills' && method === 'GET') return ok({ skills: SKILLS });

  /* --- 以下全部要登录 --- */
  if (!user) return fail(401, 'unauthorized', '登录已失效');

  if (path === '/me' && method === 'GET') return ok(meBody(user));

  if (path === '/me/credit-ledger' && method === 'GET') {
    const mine = db.ledger.filter((e) => !e.user_id || e.user_id === user.id);
    return ok({ items: mine.slice(0, 20), next_cursor: null });
  }

  if (path === '/me/password' && method === 'POST') {
    if (body?.old_password !== user.password) return fail(400, 'invalid_param', '当前密码不正确');
    if (String(body?.new_password ?? '').length < 8) return fail(400, 'invalid_param', '新密码至少 8 位');
    user.password = body.new_password;
    save();
    return ok({ ok: true });
  }

  if (path === '/uploads' && method === 'POST') {
    const id = uid('up');
    const dataUrl = typeof body?.data_url === 'string' ? body.data_url : artUrl(id, 256, 256);
    db.uploads[id] = {
      preview_url: dataUrl,
      // data URL 的 base64 段长度 ×3/4 约等于原始字节数，够存储页有数可看
      bytes: Math.round((dataUrl.length * 3) / 4),
      created_at: new Date().toISOString(),
    };
    save();
    return ok({ upload_id: id, preview_url: db.uploads[id].preview_url });
  }

  /* --- 任务 --- */
  if (path === '/tasks' && method === 'GET') {
    const status = query.get('status');
    const list = db.tasks.filter((t) =>
      status === 'active' ? t.status === 'queued' || t.status === 'running' : true,
    );
    return ok({ items: list, next_cursor: null } satisfies Paged<Task>);
  }

  if (path === '/tasks' && method === 'POST') {
    const model = findCapability(String(body?.model_id ?? ''));
    if (!model) return fail(400, 'invalid_param', '模型不存在');

    const existing = db.tasks.find((t) => t.client_token && t.client_token === body?.client_token);
    if (existing) {
      // client_token 是幂等键：断网重发不产生第二个任务
      return ok({
        task_id: existing.id,
        status: existing.status,
        estimated_cost: existing.estimated_cost,
        queue_position: existing.queue_position,
        eta_seconds: existing.eta_seconds,
      } satisfies CreateTaskResponse);
    }

    const prompt = String(body?.prompt ?? '');
    const params = (body?.params ?? {}) as Record<string, JsonValue>;
    const inputs = (body?.inputs ?? {}) as Record<string, string[]>;

    if (!prompt.trim()) return fail(400, 'invalid_param', '提示词不能为空', {
      field_errors: [{ key: 'prompt', message: '提示词不能为空' }],
    });
    if (prompt.length > model.limits.prompt_max_length) {
      return fail(400, 'invalid_param', `提示词最长 ${model.limits.prompt_max_length} 字`, {
        field_errors: [{ key: 'prompt', message: `最长 ${model.limits.prompt_max_length} 字` }],
      });
    }
    for (const slot of model.inputs) {
      const got = inputs[slot.key] ?? [];
      if (slot.required && got.length < Math.max(1, slot.min_count)) {
        return fail(400, 'invalid_param', `${slot.label}为必填`, {
          field_errors: [{ key: slot.key, message: `${slot.label}为必填` }],
        });
      }
      if (got.length > slot.max_count) {
        return fail(400, 'invalid_param', `${slot.label}最多 ${slot.max_count} 个`, {
          field_errors: [{ key: slot.key, message: `最多 ${slot.max_count} 个` }],
        });
      }
    }

    const active = db.tasks.filter(
      (t) => t.model_id === model.id && (t.status === 'queued' || t.status === 'running'),
    ).length;
    if (active >= model.limits.max_concurrent_per_user) {
      return fail(429, 'upstream_rate_limited', `该模型最多同时 ${model.limits.max_concurrent_per_user} 个任务`);
    }

    const cost = estimateCost(model, params, inputs);
    if (cost > user.credits) {
      return fail(402, 'insufficient_credit', `积分不足，还需 ${cost - user.credits} 积分`);
    }

    const etaBase = model.eta.p50_seconds;
    const scale = model.eta.scales_with
      ? Number(params[model.eta.scales_with]) /
        Number(
          model.params.find((p) => p.key === model.eta.scales_with)?.default ??
            params[model.eta.scales_with] ??
            1,
        )
      : 1;

    const task: MockTask = {
      id: uid('t'),
      user_id: user.id,
      status: 'queued',
      model_id: model.id,
      modality: model.modality,
      prompt,
      params,
      inputs,
      estimated_cost: cost,
      eta_seconds: Math.round(etaBase * (Number.isFinite(scale) && scale > 0 ? scale : 1)),
      queue_position: model.limits.queue_position_available ? active + 1 : null,
      progress: null,
      assets: [],
      error: null,
      created_at: new Date().toISOString(),
      client_token: body?.client_token,
      canvas_id: body?.canvas_id,
      attempt: 1,
      upstream_ref: null,
      upstream_status_raw: null,
    };
    db.tasks.unshift(task);

    user.credits -= cost;
    db.ledger.unshift({
      id: uid('l'),
      type: 'charge',
      amount: -cost,
      balance_after: user.credits,
      user_id: user.id,
      task_id: task.id,
      reason: `${model.modality === 'video' ? '视频' : '图片'}生成 · ${model.name}`,
      created_at: task.created_at,
    });
    save();
    emit('credit.updated', { balance: user.credits });

    void runTask(task, model, body?.card_id);

    return ok({
      task_id: task.id,
      status: task.status,
      estimated_cost: task.estimated_cost,
      queue_position: task.queue_position,
      eta_seconds: task.eta_seconds,
    } satisfies CreateTaskResponse);
  }

  if (segments[0] === 'tasks' && segments[2] === 'cancel' && method === 'POST') {
    const task = db.tasks.find((t) => t.id === segments[1]);
    if (!task) return fail(404, 'not_found', '任务不存在');
    if (task.status === 'queued' || task.status === 'running') {
      task.status = 'canceled';
      task.progress = null;
      user.credits += task.estimated_cost;
      db.ledger.unshift({
        id: uid('l'),
        type: 'refund',
        amount: task.estimated_cost,
        balance_after: user.credits,
        task_id: task.id,
        reason: '任务取消退回',
        created_at: new Date().toISOString(),
      });
      save();
      emit('task.updated', { task_id: task.id, status: 'canceled', progress: null });
      emit('credit.updated', { balance: user.credits });
    }
    return ok({ ok: true });
  }

  if (segments[0] === 'tasks' && segments.length === 2 && method === 'DELETE') {
    db.tasks = db.tasks.filter((t) => t.id !== segments[1]);
    save();
    return ok({ ok: true });
  }

  /* --- 资产 --- */
  if (path === '/assets' && method === 'GET') {
    const type = query.get('type');
    const cursor = Number(query.get('cursor') ?? 0);
    const limit = Number(query.get('limit') ?? 24);
    const filtered = db.assets.filter((a) => !type || type === 'all' || a.type === type);
    const items = filtered.slice(cursor, cursor + limit);
    const next = cursor + limit < filtered.length ? String(cursor + limit) : null;
    return ok({ items, next_cursor: next, total: filtered.length } satisfies Paged<Asset> & { total: number });
  }

  if (segments[0] === 'assets' && segments.length === 2) {
    const asset = db.assets.find((a) => a.id === segments[1]);
    if (!asset) return fail(404, 'not_found', '资产不存在');
    if (method === 'DELETE') {
      db.assets = db.assets.filter((a) => a.id !== segments[1]);
      // 软删除：先进回收站，管理端的存储清理才有东西可清
      db.trash.unshift({ id: asset.id, bytes: assetBytes(asset), deleted_at: new Date().toISOString() });
      save();
      return ok({ ok: true });
    }
    return ok(asset);
  }

  /* --- 画布项目 --- */
  if (path === '/projects' && method === 'GET') {
    return ok({ items: [...db.projects].sort((a, b) => b.updated_at.localeCompare(a.updated_at)) });
  }

  if (path === '/projects' && method === 'POST') {
    const project = {
      id: uid('p'),
      name: String(body?.name ?? '未命名项目'),
      revision: 1,
      card_count: 0,
      cover_asset_id: null,
      updated_at: new Date().toISOString(),
    };
    db.projects.unshift(project);
    db.canvases[project.id] = { revision: 1, cards: [], conversation: [] };
    save();
    return ok(project);
  }

  if (segments[0] === 'projects' && segments[2] === 'canvas') {
    const projectId = segments[1];
    const canvas = db.canvases[projectId];
    if (!canvas) return fail(404, 'not_found', '项目不存在');

    if (method === 'GET') return ok(canvas satisfies CanvasState);

    if (method === 'PATCH') {
      if (body?.base_revision !== canvas.revision) {
        return { status: 409, body: { error: { code: 'revision_conflict', message: '画布已被其他标签页修改', retryable: false, charged: false }, canvas } };
      }
      for (const op of (body.ops ?? []) as CanvasOp[]) {
        if (op.type === 'card.create') canvas.cards.push(op.card);
        else if (op.type === 'card.move') {
          const c = canvas.cards.find((x) => x.id === op.id);
          if (c) { c.x = op.x; c.y = op.y; c.auto_placed = false; }
        } else if (op.type === 'card.update') {
          const c = canvas.cards.find((x) => x.id === op.id);
          if (c) Object.assign(c, op.patch);
        } else if (op.type === 'card.delete') {
          canvas.cards = canvas.cards.filter((x) => x.id !== op.id);
        } else if (op.type === 'viewport.set') {
          canvas.viewport = op.viewport;
        }
      }
      canvas.revision += 1;
      const project = db.projects.find((p) => p.id === projectId);
      if (project) {
        project.revision = canvas.revision;
        project.card_count = canvas.cards.length;
        project.updated_at = new Date().toISOString();
      }
      save();
      return ok({ revision: canvas.revision });
    }
  }

  if (segments[0] === 'canvas' && segments[2] === 'chat' && method === 'POST') {
    const canvas = db.canvases[segments[1]];
    if (!canvas) return fail(404, 'not_found', '项目不存在');
    const refs = (body?.ref_card_ids ?? []) as string[];
    const now = () => new Date().toISOString();
    canvas.conversation.push({
      id: uid('m'), role: 'user', content: String(body?.message ?? ''), ref_card_ids: refs, created_at: now(),
    });
    const reply = refs.length
      ? `已引用 ${refs.length} 张卡片作为参考，可以直接在卡片上「重跑」应用。`
      : '收到。在画布上选中卡片后再发送，可以把它们作为参考。';
    const message = { id: uid('m'), role: 'assistant' as const, content: reply, ref_card_ids: [], created_at: now() };
    canvas.conversation.push(message);
    canvas.revision += 1;
    save();
    return ok({ message_id: message.id, revision: canvas.revision } satisfies CanvasChatResponse);
  }

  /* --- 管理后台。前端路由守卫只是第一道，越权在这里同样要被挡住 --- */
  if (segments[0] === 'admin') {
    if (user.role !== 'admin') return fail(403, 'forbidden', '需要管理员权限');
    return adminHandle(method, path, segments, query, body, user);
  }

  return fail(404, 'not_found', `mock 后端未实现：${method} ${path}`);
}

/* ────────────────────────────── 管理后台 ────────────────────────────── */

function providerOfModel(modelId: string): string | undefined {
  return db.modelConfigs.find((c) => c.id === modelId)?.provider_id;
}

function adminTaskBody(t: MockTask): AdminTask {
  return {
    ...t,
    username: db.users.find((u) => u.id === t.user_id)?.username,
    provider_id: providerOfModel(t.model_id),
  };
}

const WINDOW_MINUTES: Record<string, number> = { '1h': 60, '24h': 60 * 24, '7d': 60 * 24 * 7 };

/** 资产没有被任何任务引用 = 孤儿文件，清理页要能先看到再动手 */
function orphanAssets(): Asset[] {
  const referenced = new Set(db.tasks.flatMap((t) => (t.assets ?? []).map((a) => a.id)));
  return db.assets.filter((a) => !referenced.has(a.id));
}

async function adminHandle(
  method: string,
  path: string,
  segments: string[],
  query: URLSearchParams,
  body: any,
  operator: MockUser,
): Promise<MockResponse> {
  /* --- 供应商 --- */
  if (path === '/admin/providers' && method === 'GET') {
    return ok({ providers: db.providers });
  }

  if (path === '/admin/providers' && method === 'POST') {
    const name = String(body?.name ?? '').trim();
    if (!name) return fail(400, 'invalid_param', '供应商名称不能为空', {
      field_errors: [{ key: 'name', message: '不能为空' }],
    });
    const created: Provider = {
      ...(body as Provider),
      id: uid('pv'),
      name,
      // 密钥本身永远不经过这里：credential_ref 只是 env 变量名，是否已配置由后端回一个布尔
      credential_present: Boolean(body?.credential_ref),
      created_at: new Date().toISOString(),
    };
    db.providers.push(created);
    save();
    return { status: 201, body: created };
  }

  if (segments[1] === 'providers' && segments.length === 3) {
    const provider = db.providers.find((p) => p.id === segments[2]);
    if (!provider) return fail(404, 'not_found', '供应商不存在');

    if (method === 'GET') return ok(provider);

    if (method === 'PATCH') {
      Object.assign(provider, body, {
        id: provider.id,
        created_at: provider.created_at,
        credential_present: Boolean(body?.credential_ref ?? provider.credential_ref),
      });
      save();
      return ok(provider);
    }

    if (method === 'DELETE') {
      const used = db.modelConfigs.filter((c) => c.provider_id === provider.id);
      if (used.length) {
        return fail(409, 'conflict', `仍被 ${used.length} 个模型引用，请先改掉这些模型的供应商`);
      }
      db.providers = db.providers.filter((p) => p.id !== provider.id);
      save();
      return { status: 204, body: null };
    }
  }

  /* --- 模型配置 --- */
  if (path === '/admin/models' && method === 'GET') {
    const modality = query.get('modality');
    const list = db.modelConfigs.filter((c) => !modality || c.capability.modality === modality);
    return ok({ models: list });
  }

  if (path === '/admin/models' && method === 'POST') {
    const invalid = validateModelConfig(body, null);
    if (invalid) return invalid;
    const created: ModelConfig = {
      ...(body as ModelConfig),
      id: String(body.id).trim(),
      updated_at: new Date().toISOString(),
    };
    db.modelConfigs.push(created);
    save();
    return { status: 201, body: created };
  }

  if (segments[1] === 'models' && segments.length === 3) {
    const config = db.modelConfigs.find((c) => c.id === segments[2]);
    if (!config) return fail(404, 'not_found', '模型配置不存在');

    if (method === 'GET') return ok(config);

    if (method === 'PATCH') {
      const invalid = validateModelConfig(body, config.id);
      if (invalid) return invalid;
      Object.assign(config, body, { id: config.id, updated_at: new Date().toISOString() });
      save();
      return ok(config);
    }

    if (method === 'DELETE') {
      db.modelConfigs = db.modelConfigs.filter((c) => c.id !== config.id);
      save();
      return { status: 204, body: null };
    }
  }

  if (segments[1] === 'models' && segments[3] === 'probe' && method === 'POST') {
    const config = db.modelConfigs.find((c) => c.id === segments[2]);
    if (!config) return fail(404, 'not_found', '模型配置不存在');
    const provider = db.providers.find((p) => p.id === config.provider_id);
    const dryRun = body?.dry_run !== false;

    // 渲染出的请求体里绝不带凭证，只带 env 变量名，方便管理员核对 mapping 而不泄密
    const rendered: Record<string, JsonValue> = {
      url: `${provider?.base_url ?? '(未配置供应商)'}/generations`,
      authorization: provider?.credential_ref ? `$${provider.credential_ref}` : '(未配置)',
      body: { model: config.upstream_model, prompt: '连通性自检', ...(config.request_mapping?.body_template ?? {}) },
    };

    if (!provider || !provider.credential_present) {
      return ok({
        ok: false,
        dry_run: dryRun,
        rendered_request: rendered,
        error: {
          code: 'internal_error',
          message: provider ? `环境变量 ${provider.credential_ref} 未配置` : '供应商不存在',
          retryable: false,
          charged: false,
        },
      } satisfies ProbeResult);
    }

    await delay(dryRun ? 120 : 900);
    return ok({
      ok: true,
      dry_run: dryRun,
      rendered_request: rendered,
      upstream_status: dryRun ? null : 200,
      latency_ms: dryRun ? null : 640,
    } satisfies ProbeResult);
  }

  /* --- 用户与积分 --- */
  if (path === '/admin/users' && method === 'GET') {
    const q = (query.get('q') ?? '').trim().toLowerCase();
    const items = db.users
      .filter((u) => !q || u.username.toLowerCase().includes(q) || u.id.toLowerCase().includes(q))
      .map(adminUserBody);
    return ok({ items, next_cursor: null } satisfies Paged<AdminUser>);
  }

  if (segments[1] === 'users' && segments.length === 3) {
    const target = db.users.find((u) => u.id === segments[2]);
    if (!target) return fail(404, 'not_found', '用户不存在');

    if (method === 'GET') return ok(adminUserBody(target));

    if (method === 'PATCH') {
      if (target.id === operator.id && body?.role && body.role !== 'admin') {
        return fail(400, 'invalid_param', '不能撤销自己的管理员权限');
      }
      if (body?.role) target.role = body.role;
      if (body?.status) target.status = body.status;
      if (typeof body?.storage_quota_bytes === 'number') {
        if (body.storage_quota_bytes < 0) return fail(400, 'invalid_param', '配额不能为负');
        target.storage_quota = body.storage_quota_bytes;
      }
      save();
      return ok(adminUserBody(target));
    }
  }

  if (segments[1] === 'users' && segments[3] === 'credits' && method === 'POST') {
    const target = db.users.find((u) => u.id === segments[2]);
    if (!target) return fail(404, 'not_found', '用户不存在');

    const amount = Number(body?.amount);
    const key = String(body?.idempotency_key ?? '');
    if (!Number.isInteger(amount) || amount === 0) {
      return fail(400, 'invalid_param', '数额必须是非零整数', {
        field_errors: [{ key: 'amount', message: '必须是非零整数' }],
      });
    }
    if (!key) return fail(400, 'invalid_param', '缺少幂等键');
    // 管理员手滑连点两次不该充两次钱
    if (db.idempotency[key]) {
      const existing = db.ledger.find((e) => e.id === db.idempotency[key]);
      if (existing) return { status: 409, body: { error: { code: 'conflict', message: '该操作已执行过', retryable: false, charged: false }, entry: existing } };
    }
    if (target.credits + amount < 0) {
      return fail(400, 'invalid_param', `扣减后余额为负，当前仅 ${target.credits} 积分`);
    }

    target.credits += amount;
    const entry: CreditLedgerEntry = {
      id: uid('l'),
      type: amount > 0 ? 'topup' : 'adjust',
      amount,
      balance_after: target.credits,
      user_id: target.id,
      reason: String(body?.reason ?? (amount > 0 ? '管理员充值' : '管理员扣减')),
      operator_id: operator.id,
      created_at: new Date().toISOString(),
    };
    db.ledger.unshift(entry);
    db.idempotency[key] = entry.id;
    save();
    // 被充值的用户如果正开着页面，余额要立刻跟着动
    if (target.id === operator.id) emit('credit.updated', { balance: target.credits });
    return ok(entry);
  }

  if (segments[1] === 'users' && segments[3] === 'credit-ledger' && method === 'GET') {
    const items = db.ledger.filter((e) => e.user_id === segments[2]).slice(0, 50);
    return ok({ items, next_cursor: null } satisfies Paged<CreditLedgerEntry>);
  }

  /* --- 任务监控 --- */
  if (path === '/admin/tasks' && method === 'GET') {
    const status = query.get('status');
    const errorCode = query.get('error_code');
    const modelId = query.get('model_id');
    const userId = query.get('user_id');
    const items = db.tasks
      .filter((t) => !status || t.status === status)
      .filter((t) => !errorCode || t.error?.code === errorCode)
      .filter((t) => !modelId || t.model_id === modelId)
      .filter((t) => !userId || t.user_id === userId)
      .map(adminTaskBody);
    return ok({ items, next_cursor: null } satisfies Paged<AdminTask>);
  }

  if (path === '/admin/tasks/stats' && method === 'GET') {
    const windowKey = query.get('window') ?? '24h';
    const cutoff = Date.now() - (WINDOW_MINUTES[windowKey] ?? WINDOW_MINUTES['24h']) * 60_000;
    const inWindow = db.tasks.filter((t) => Date.parse(t.created_at) >= cutoff);

    const byStatus: Record<string, number> = {};
    const byErrorCode: Record<string, number> = {};
    const perModel = new Map<string, { total: number; failed: number }>();
    for (const t of inWindow) {
      byStatus[t.status] = (byStatus[t.status] ?? 0) + 1;
      if (t.error) byErrorCode[t.error.code] = (byErrorCode[t.error.code] ?? 0) + 1;
      const bucket = perModel.get(t.model_id) ?? { total: 0, failed: 0 };
      bucket.total += 1;
      if (t.status === 'failed') bucket.failed += 1;
      perModel.set(t.model_id, bucket);
    }

    const queued = inWindow.filter((t) => t.status === 'queued');
    const oldest = queued.reduce<number | null>((acc, t) => {
      const age = Math.round((Date.now() - Date.parse(t.created_at)) / 1000);
      return acc === null || age > acc ? age : acc;
    }, null);

    return ok({
      window: windowKey,
      queued: queued.length,
      running: inWindow.filter((t) => t.status === 'running').length,
      oldest_queued_age_seconds: oldest,
      by_status: byStatus,
      by_error_code: byErrorCode,
      by_model: [...perModel].map(([model_id, v]) => ({ model_id, ...v })),
    } satisfies TaskStats);
  }

  if (segments[1] === 'tasks' && segments[3] === 'retry' && method === 'POST') {
    const task = db.tasks.find((t) => t.id === segments[2]);
    if (!task) return fail(404, 'not_found', '任务不存在');
    if (task.status !== 'failed' && task.status !== 'canceled') {
      return fail(409, 'conflict', '只有失败或已取消的任务可以重试');
    }
    const model = findCapability(task.model_id);
    if (!model) return fail(409, 'conflict', '该任务使用的模型配置已被删除');

    // 重用原冻结额度，不二次扣费（openapi §adminRetryTask）
    task.status = 'queued';
    task.error = null;
    task.progress = null;
    task.attempt += 1;
    task.upstream_status_raw = null;
    save();
    emit('task.updated', { task_id: task.id, status: 'queued', progress: null });
    void runTask(task, model);
    return ok(adminTaskBody(task));
  }

  if (segments[1] === 'tasks' && segments[3] === 'cancel' && method === 'POST') {
    const task = db.tasks.find((t) => t.id === segments[2]);
    if (!task) return fail(404, 'not_found', '任务不存在');
    if (task.status === 'queued' || task.status === 'running') {
      task.status = 'canceled';
      task.progress = null;
      save();
      emit('task.updated', { task_id: task.id, status: 'canceled', progress: null });
    }
    return ok(adminTaskBody(task));
  }

  if (path === '/admin/circuit-breakers' && method === 'GET') {
    const breakers: CircuitBreakerState[] = db.providers.map((p) => {
      const failures = db.tasks.filter(
        (t) => t.status === 'failed' && providerOfModel(t.model_id) === p.id,
      ).length;
      const state = failures >= 3 ? 'open' : failures > 0 ? 'half_open' : 'closed';
      return {
        provider_id: p.id,
        state,
        failure_count: failures,
        opened_at: state === 'open' ? new Date(Date.now() - 120_000).toISOString() : null,
        next_probe_at: state === 'closed' ? null : new Date(Date.now() + 60_000).toISOString(),
      };
    });
    return ok({ breakers });
  }

  /* --- 存储配额 --- */
  if (path === '/admin/storage/usage' && method === 'GET') {
    return ok({
      total_bytes: db.users.reduce((sum, u) => sum + u.storage_used, 0),
      asset_count: db.assets.length,
      upload_pending_bytes: Object.values(db.uploads).reduce((sum, u) => sum + u.bytes, 0),
      soft_deleted_bytes: db.trash.reduce((sum, t) => sum + t.bytes, 0),
      top_users: [...db.users]
        .sort((a, b) => b.storage_used - a.storage_used)
        .map((u) => ({
          user_id: u.id,
          username: u.username,
          used_bytes: u.storage_used,
          quota_bytes: u.storage_quota,
        })),
    } satisfies StorageUsage);
  }

  if (path === '/admin/storage/cleanup' && method === 'POST') {
    const dryRun = body?.dry_run !== false;
    const olderThanDays = Number(body?.older_than_days ?? 30);
    const target = (body?.target ?? 'expired_uploads') as CleanupTarget;
    const cutoff = Date.now() - olderThanDays * 24 * 60 * 60_000;

    if (target === 'expired_uploads') {
      const stale = Object.entries(db.uploads).filter(([, u]) => Date.parse(u.created_at) < cutoff);
      const result: CleanupResult = {
        dry_run: dryRun,
        candidate_count: stale.length,
        reclaimed_bytes: stale.reduce((sum, [, u]) => sum + u.bytes, 0),
        samples: stale.slice(0, 5).map(([id]) => id),
      };
      if (!dryRun) {
        for (const [id] of stale) delete db.uploads[id];
        save();
      }
      return ok(result);
    }

    if (target === 'soft_deleted_assets') {
      const stale = db.trash.filter((t) => Date.parse(t.deleted_at) < cutoff);
      const result: CleanupResult = {
        dry_run: dryRun,
        candidate_count: stale.length,
        reclaimed_bytes: stale.reduce((sum, t) => sum + t.bytes, 0),
        samples: stale.slice(0, 5).map((t) => t.id),
      };
      if (!dryRun) {
        const doomed = new Set(stale.map((t) => t.id));
        db.trash = db.trash.filter((t) => !doomed.has(t.id));
        save();
      }
      return ok(result);
    }

    const orphans = orphanAssets().filter((a) => Date.parse(a.created_at) < cutoff);
    const result: CleanupResult = {
      dry_run: dryRun,
      candidate_count: orphans.length,
      reclaimed_bytes: orphans.reduce((sum, a) => sum + assetBytes(a), 0),
      samples: orphans.slice(0, 5).map((a) => a.id),
    };
    if (!dryRun) {
      const doomed = new Set(orphans.map((a) => a.id));
      db.assets = db.assets.filter((a) => !doomed.has(a.id));
      save();
    }
    return ok(result);
  }

  return fail(404, 'not_found', `mock 后端未实现：${method} ${path}`);
}

/**
 * 新增/修改模型配置时的服务端校验。真后端会做 capability schema 的完整校验，
 * mock 只挡住会让前端目录直接崩掉的那几条：id 冲突、供应商不存在、视频协议缺失。
 */
function validateModelConfig(body: any, selfId: string | null): MockResponse | null {
  const id = String(body?.id ?? '').trim();
  if (!id) return fail(400, 'invalid_param', '模型 id 不能为空', {
    field_errors: [{ key: 'id', message: '不能为空' }],
  });
  if (id !== selfId && db.modelConfigs.some((c) => c.id === id)) {
    return fail(409, 'conflict', `模型 id「${id}」已存在`, {
      field_errors: [{ key: 'id', message: '已存在' }],
    });
  }
  if (!db.providers.some((p) => p.id === body?.provider_id)) {
    return fail(400, 'invalid_param', '供应商不存在', {
      field_errors: [{ key: 'provider_id', message: '请选择一个供应商' }],
    });
  }
  if (!String(body?.upstream_model ?? '').trim()) {
    return fail(400, 'invalid_param', '上游 model 名不能为空', {
      field_errors: [{ key: 'upstream_model', message: '不能为空' }],
    });
  }
  // 三家视频协议的请求/轮询结构完全不同，缺了这个字段执行器无从下手
  if (body?.protocol_family === 'video' && !body?.video_protocol) {
    return fail(400, 'invalid_param', '视频模型必须选择视频协议', {
      field_errors: [{ key: 'video_protocol', message: '必填' }],
    });
  }
  const capability = body?.capability as ModelCapabilitySchema | undefined;
  if (!capability?.name?.trim()) {
    return fail(400, 'invalid_param', '展示名不能为空', {
      field_errors: [{ key: 'capability.name', message: '不能为空' }],
    });
  }
  if (capability.id !== id) {
    return fail(400, 'invalid_param', 'capability.id 必须与模型 id 一致', {
      field_errors: [{ key: 'capability.id', message: `应为 ${id}` }],
    });
  }
  return null;
}

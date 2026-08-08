# AIGC Pool 前端

Vite + React 18 + TypeScript(strict) 的单页应用。实现 `docs/frontend-design.md` 描述的
登录、生成器（图片 / 视频）、任务流、资产库、个人中心、短剧无限画布七个页面。

## 起步

```bash
cd frontend
npm install
npm run dev                  # http://localhost:15173
```

**默认直连真后端**，先按项目根 README 把后端起起来（代理默认指向
`http://localhost:18080`，端口对不上见根 README「本地端口规范」）。
打开后默认落在 `/create/image`，用种子账号 **demo / user-dev-only** 登录，
或者直接注册一个新账号。

dev server 固定监听 **15173**（`vite.config.ts` 里开了 `strictPort`）：端口被占时
直接失败退出，不会像 Vite 默认那样 +1 漂到别的端口。漂走最坑的地方在于浏览器里
那个还开着的旧页面仍指向原端口，看起来一切正常，实际调的是另一个进程。
本仓库所有监听端口必须 >= 10000、禁用 8080，来龙去脉见根 README「本地端口规范」。

后端没起、只想调界面时再开离线 mock：`cp .env.example .env.local`，
把里面的 `VITE_USE_MOCK=true` 取消注释。

## mock 后端 / 真后端切换

一个 env 开关，代码零改动：

| `VITE_USE_MOCK` | 行为 |
| --- | --- |
| 未设置 / 其他任何值（默认） | 请求打到 `VITE_API_BASE`（默认 `/api`），dev server 代理到 `VITE_API_PROXY_TARGET`（默认 `http://localhost:18080`） |
| `true` | 请求在 `src/api/client.ts` 里被拦下，路由到 `src/mock/backend.ts` 的内存后端，不发任何网络请求 |

只认字符串 `true`，是为了让「忘了配」和「配错了」都落到真后端而不是悄悄给假数据。
`.env.local` 优先级高于代码缺省值，排查数据来源时先看它在不在。

拦截发生在 `request()` 这一层，`src/api/endpoints.ts` 里的调用代码两种模式下完全一致。
实时事件同样跟着切：mock 模式用进程内 event bus，真后端用 SSE（`src/realtime/adapters.ts`）。

**没有用 MSW**，用的是等价的进程内 mock：少一个依赖、不用起 service worker，
而且 SSE 这类流式响应在进程内模拟比在 worker 里简单得多。

mock 数据落在 `localStorage`（`aigc_pool_mock_db`），所以刷新后任务、资产、画布都还在。
想重置就清掉这个 key。

### mock 模式的已知取舍

- 任务真实 eta 可能几分钟，mock 里压到 **12 秒上限**，否则没法演示。
- 产物是按 seed 生成的占位图（`src/mock/art.ts`），视频资产的 `original` 也是一张图，
  所以画布上视频卡片放大后不会真的播放。真后端接上就正常。

## 目录

```
src/
  api/         client / endpoints / types —— 唯一的网络出口
  schema/      能力 schema 引擎：evaluate 条件、pricing 估价、validate、migrate
  components/  控件（8 种 control）、芯片栏、任务卡片、资产瓦片
  pages/       七个页面
  canvas/      画布视口、同步、卡片渲染
  realtime/    SSE / 轮询 / mock 三种适配器 + Provider
  mock/        进程内 mock 后端（不算前端逻辑，CI 守卫会跳过这个目录）
```

## 唯一一条硬约束：schema 驱动

参数芯片全部由后端返回的 **能力 schema** 渲染，前端不认识任何具体模型。
源码里出现模型 id / 供应商名就算破线，有脚本守着：

```bash
npm run check:no-model-ids
```

不认识的 control 类型也不会白屏：带 `options` 的降级成下拉，不带的渲染成只读芯片
并按 `default` 提交。`src/mock/models.json` 里故意放了两个这样的控件用来演示。

## 自检

```bash
npm run typecheck && npm run build && npm run check:no-model-ids
```

## 与契约的对齐

`docs/openapi.yaml` 已落盘，前端类型以它为准（`src/api/types.ts` / `src/api/endpoints.ts`），
mock 后端也按同一份形状返回。原先按 `docs/frontend-design.md` §7 写的部分已改齐：

| 位置 | 原实现 | 契约 |
| --- | --- | --- |
| 积分流水 | `GET /me/credits` | `GET /me/credit-ledger` |
| 改密码 | `current_password` | `old_password` |
| `Me` | — | 多一个 `credits_held`（排队任务的预扣额） |
| `CreditLedgerEntry` | `{id, amount, reason, created_at}` | 加 `type`(hold/charge/refund/topup/adjust)、`balance_after`，`reason` 变可选 |
| `CanvasProject` | — | 多一个 `revision` |
| `CanvasMessage` | `text` / `at` | `content` / `created_at`，`role` 多一个 `system` |
| `CanvasOp` | 4 种 | 多一种 `viewport.set` |
| 画布对话 | 请求体 `text`，返回 `{message}` | 请求体 `message`，返回 `{message_id, revision, task_ids?}` |

`POST /tasks` 的 `card_id` 契约里有，之前多带的那个可选字段不用改了。

仍未跟进的差异（都不影响现有链路，等真后端接上再补）：

- `TaskError` 契约有可选的 `retry {attempt, max_attempts, next_at}`，前端没建模，
  自动重试的倒计时目前由前端自己算。
- 轮询降级（`src/realtime/adapters.ts`）合成的 `task.succeeded` 只带 `asset_id`，
  契约要求 `assets: Asset[]` + 可选 `actual_cost`。SSE 正常时走的是真事件，不受影响。
- `GET /assets` 契约没有 `total`，资产库列表头改成报「已加载条数」，游标见底时它才等于总数。
- `Card.title` 后端 Card 结构里没有，提交后会被丢弃；渲染一律走 `src/canvas/cardTitle.ts`
  按 `kind` 兜底。`Card.history` 后端可能整个字段不返回，读长度前必须判空。


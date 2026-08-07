# AIGC Pool 技术设计（Go + MySQL）

> 阶段：DEM-62 技术设计。本文件是**设计与骨架**，不含业务实现。
> 实现拆到 DEM-64（后端）/ DEM-65（前端），两者并行的依据是 [`openapi.yaml`](./openapi.yaml)。

## 0. 这份文档与它的前置

| 文件 | 是什么 | 谁是权威 |
|---|---|---|
| [`openapi.yaml`](./openapi.yaml) | 前后端唯一契约，40 个端点 / 82 个 schema | **本阶段最关键产出**，前端据此 mock |
| [`contracts/capability-schema.ts`](./contracts/capability-schema.ts) | 动态表单契约 | TS 侧权威，`openapi.yaml` 是它的传输层镜像 |
| [`frontend-design.md`](./frontend-design.md) | 前端结构与对后端的诉求清单（§7） | 后端必须满足其 §7 全部条目 |
| [`ui-design.md`](./ui-design.md) | 视觉与交互，失败三分类的呈现 | — |
| **本文** | 五层架构、数据模型、driver 设计 | — |

技术栈已定，不再讨论：**Go 后端 + MySQL 8.0 + 前后端分离 + 本地跑不部署**。
（先前文档中的 Python / FastAPI 字样已作废，`frontend-design.md` §4.3 理由 3 里残留一处，
不影响结论——SSE 选型的四条理由中另外三条与语言无关。）

---

## 1. 五层总览

```
                    ┌──────────────────────────────────────────────┐
   前端 ─ HTTP ────► │  httpapi   路由 / 鉴权 / SSE / 管理端隔离      │
                    └───────────────┬──────────────────────────────┘
                                    │
   ┌────────────────────────────────▼────────────────────────────────┐
   │ L1 capability   能力声明：schema 下发 · 服务端校验 · 计价         │
   └────────────────────────────────┬────────────────────────────────┘
                                    │
   ┌────────────────────────────────▼────────────────────────────────┐
   │ L2 executor     状态机 · 队列租约 · 轮询/回调 · 重试 · 熔断        │
   └───────┬─────────────────────────────────────────┬───────────────┘
           │                                         │
   ┌───────▼──────────────────────┐        ┌─────────▼───────────────┐
   │ L0 adapter                   │        │ L3 asset                │
   │  openaicompat / ark /        │        │  转存 · 派生 · 血缘 · 配额 │
   │  openaivideo / googlelro /   │        └─────────┬───────────────┘
   │  mock                        │                  │
   └───────┬──────────────────────┘        ┌─────────▼───────────────┐
           │                               │ billing 积分记账          │
      上游 HTTP                            └─────────────────────────┘
```

**依赖方向单向向下，且 L0 只被 L2 调用。**
canvas / skill / stream / billing 是横向服务，不构成新的层。

### 一次图片生成的完整调用链

```
POST /api/tasks
  └ httpapi        鉴权、解 body
  └ L1 Validator   按该模型 schema 校验 params/inputs（visible_when 为假的参数直接拒）
  └ L1 Pricer      算 estimated_cost
  └ billing.Hold   冻结积分（余额不足 → 402，不入队）
  └ L2 enqueue     写 tasks(status=queued)，写 task_events → SSE
  ◄ 201 {task_id, estimated_cost, queue_position, eta_seconds}

worker（独立 goroutine 池，不在请求线程里）
  └ L2 Queue.Claim         租约抢一批 queued 任务
  └ L0 Registry.For(model) 按 protocol_family / video_protocol 取 driver
  └ L0 driver.Submit       mapping 渲染上游 body → 发请求 → 拿 upstream_ref
  └ L2 置 running，设 next_poll_at
  ⟳ L2 poll loop → L0 driver.Poll → 归一化状态
  └ 成功那一刻：
      └ L0 driver.FetchArtifact  （URL / 二进制流 / base64 三种取法归一）
      └ L3 Transferor            立即下载转存到本地存储       ← 硬约束，见 §5.1
      └ L3 Deriver               生成 thumb_512 / poster
      └ L3 LineageRecorder       写血缘边
      └ billing.Charge           冻结转实扣
      └ stream.Publish           task.succeeded / credit.updated
```

---

## 2. L0 适配层

### 2.1 红线

> **上层任何位置出现 `if provider == "veo"` 就是设计漏了。**
> 协议差异只允许存在于 `internal/adapter/<driver>/` 内部。

强制手段：`internal/adapter` 之上的包（executor / capability / httpapi / asset）
**不允许 import 任何具体 driver 包**，只 import `internal/adapter` 的接口。
driver 包只在 `Registry` 的构造处被 import 一次。这条可以用 `go vet` 之外的一条
import 检查在 CI 上机器校验（实现阶段加）。

### 2.2 接口

```go
// internal/adapter
type Status string // 平台自己的枚举，与 domain.TaskStatus 对齐
const (StatusQueued, StatusRunning, StatusSucceeded, StatusFailed)

type SyncDriver interface {          // chat / images：一次调用直接返回结果
    Driver
    Invoke(ctx, *SubmitInput) (*InvokeResult, error)
}

type AsyncDriver interface {         // video：提交 → 轮询 → 取产物
    Driver
    Submit(ctx, *SubmitInput) (*SubmitResult, error)
    Poll(ctx, *PollRequest) (*PollResult, error)
    FetchArtifact(ctx, *ArtifactRef) (*ArtifactStream, error)
    DefaultPollInterval() time.Duration
}
```

三个方法而不是一个 `Generate`，是因为**生成是分钟级的**：进程重启、worker 换机器
都不能让在途任务丢失。`upstream_ref` 落库后，任何一个 worker 都能接着轮询下去。
任何同步等待的设计在这里都是错的。

### 2.3 视频三家：四个维度全都不同

「统一 OpenAI 协议」在 chat 和 images 上成立，**在视频上不成立**。不是端点命名的差异，
是结构的差异：

| | `ark`（方舟 / Seedance·豆包） | `openai_video`（Sora 2） | `google_lro`（Veo 3.x） |
|---|---|---|---|
| **提交** | `POST {base}/api/v3/contents/generations/tasks` | `POST {base}/v1/videos` | `POST {base}/v1beta/models/{model}:predictLongRunning` |
| **轮询寻址** | 用返回的 task id | 用返回的 video id | **用返回体里的 `operation name`**，不是 id |
| **轮询** | `GET .../tasks/{id}` | `GET /v1/videos/{id}` | `GET {base}/{operation_name}` |
| **状态** | `queued/running/succeeded/failed/expired` | `queued/in_progress/completed/failed` | **无枚举**，只有 `done` 布尔 |
| **产物** | `content.video_url`，**仅 24h 有效** | `GET /v1/videos/{id}/content` **直接流二进制 mp4** | base64 内联 **或** GCS URI，取决于配置 |
| **鉴权** | `Authorization: Bearer` | `Authorization: Bearer` | Bearer / ADC |

所以：**三个内置 driver，藏在同一个 interface 后面**，`models.video_protocol` 选驱动。
试图用一套 URL 模板硬套三家，会在 `google_lro` 的「轮询地址由响应体决定」和
`openai_video` 的「产物不是 URL 是字节流」这两处直接崩掉。

Ark 的请求字段（**以官方页为准，不写死成代码常量**）：`model`、`content` 数组
（text / image / video / audio 混合，**顺序影响生成语义**——所以 `request_mapping`
的规则顺序即数组顺序，见 §2.5）、`parameters`（`duration` / `resolution` / `ratio` / `seed`）。
另支持 `callback_url` 与 `service_tier`（`flex` 离线推理约半价）。
官方参考：创建 `volcengine.com/docs/82379/1520757`、查询 `/1521309`、总览 `/1520758`。
第三方整理的 `fps`、模型 ID 与官方有出入，不采信。

### 2.4 状态归一化（driver 内部完成，L2 以上只认平台状态）

| 平台状态 | ark | openai_video | google_lro | openaicompat |
|---|---|---|---|---|
| `queued` | `queued` | `queued` | `done=false` 且无 metadata 进度 | —（同步） |
| `running` | `running` | `in_progress` | `done=false` | — |
| `succeeded` | `succeeded` | `completed` | `done=true` 且无 `error` | HTTP 200 |
| `failed` | `failed` / `expired` | `failed` | `done=true` 且有 `error` | 非 2xx |

`expired` 归到 `failed`（错误码 `internal_error`，可重试）——上游任务记录只留 7 天，
过期就是我们轮询得太晚，属于我们的问题不是用户的问题，**不扣费**。

原始状态串写进 `tasks.upstream_status_raw` 保留，只给管理后台排障看，业务逻辑不许读它。

### 2.5 产物取法归一化

`ArtifactRef.Kind` 有四种，`FetchArtifact` 把它们统一成一个 `io.ReadCloser`：

| Kind | 来自 | FetchArtifact 做什么 |
|---|---|---|
| `url` | ark 的 `content.video_url` | GET 该 URL，流式返回 |
| `binary` | Sora 的 `/content` 端点 | 带鉴权 GET，流式返回 |
| `base64` | Veo 内联 | base64 解码成流 |
| `gcs_uri` | Veo 配置了 GCS 输出 | 按 GCS 协议取，流式返回 |

上层（L3 Transferor）只看到一个流，不知道这四种的存在。

### 2.6 `request_mapping`：「零代码接新模型」这条判定线的承载者

driver 负责**协议骨架**（端点、鉴权、轮询寻址、产物取法）——这些一家一个样、必须写代码。
字段差异**不进 driver**，落在模型配置的 `request_mapping` JSON 里：

```jsonc
{
  "body_template": { "parameters": {} },
  "rules": [
    { "from": "model.upstream_model", "to": "model" },
    { "from": "prompt",               "to": "content[]", "wrap": "text_part" },
    { "from": "inputs.first_frame",   "to": "content[]", "wrap": "image_url_part",
      "when": { "op": "has_input", "slot": "first_frame" } },
    { "from": "params.resolution",    "to": "parameters.resolution",
      "value_map": { "768p": "720p" } },
    { "from": "params.duration",      "to": "parameters.duration", "cast": "int" },
    { "from": "params.seed",          "to": "parameters.seed", "omit_when_empty": true }
  ]
}
```

- `to` 以 `[]` 结尾表示追加进数组，**规则顺序即数组顺序**——Ark 的 content 顺序有生成语义，
  这不是实现细节，是必须在配置层可控的东西。
- `when` 复用 L1 的 `Condition`，与前端 `visible_when` 同一套语义，不再发明第二套表达式。
- `value_map` 吸收「平台叫 768p、上游叫 720p」这类纯命名差异——**这是最常见的接入摩擦，
  留给配置解决，绝不留给代码分支解决。**

没有这一层，每接一个新模型都要改 driver，「模型池每周在换」这条现实就落不了地。

### 2.7 `mock` driver

本地**没有任何真实上游 API Key**，这是既定前提，不向用户索要。
`mock` 走**完全相同**的 interface、状态机、转存、记账路径，只替换最底层 HTTP 调用：
按配置的 ETA 睡一段时间，生成一张带任务参数水印的占位图 / 一段纯色 mp4，
以 `binary` Kind 交给 `FetchArtifact`。

它同时能按配置概率注入三类失败（`invalid_param` / `upstream_rate_limited` /
`content_rejected`），否则失败三分类的前端呈现在本地永远测不到。

**它不是"假接口"，它是全链路的唯一可执行路径。** 实现阶段的验收应当能在零凭证下
跑完「提交 → 排队 → 生成 → 转存 → 缩略图 → 血缘 → 扣费 → SSE 推送」全程。

### 2.8 ⭐ 接入一个新的文生视频供应商，要动哪几个文件

**情况 A：协议已支持（新模型走 ark / openai_video / google_lro 之一）—— 零代码**

改的是数据，不是文件：

| 动作 | 位置 |
|---|---|
| 加一条 provider（若是新公司） | `POST /api/admin/providers`，`credential_ref` 填**环境变量名** |
| 把密钥写进环境 | `configs/.env`（不进库、不进 git） |
| 加一条 model 记录 | `POST /api/admin/models`：`provider_id` / `upstream_model` / `protocol_family=video` / `video_protocol=<已有值>` / `capability` / `request_mapping` |
| 验证 | `POST /api/admin/models/{id}/probe`（先 `dry_run=true` 看渲染出的 body，再关掉打真实请求） |

**改动的 Go 文件数：0。改动的前端文件数：0。需要重启 / 发版：否**（配置热生效，见 §9）。

**情况 B：协议形态全新（第四家，比如某家用 gRPC 或用 multipart 提交）**

| 要动 | 文件 | 改什么 |
|---|---|---|
| 新增 | `internal/adapter/<newvendor>/driver.go` | 实现 `adapter.AsyncDriver` 四个方法 + 状态映射表 + 产物 Kind 判定 |
| 新增 | `internal/adapter/<newvendor>/driver_test.go` | 用录制的响应样本测状态归一化与 mapping 渲染 |
| 改 1 行 | `internal/adapter/adapter.go`（`Registry` 接口的实现处） | 注册表加一个 case。`Registry` 是全系统唯一按协议名分发的地方，具体实现与装配在 DEM-64 落地 |
| 改 1 行 | `internal/domain/model.go` | `VideoProtocol` 常量加一个值 |
| 改 1 行 | `migrations/00000X_add_video_protocol.up.sql` | `ALTER TABLE models MODIFY video_protocol ENUM(...)` 加一个枚举值 |
| 改 1 处 | `docs/openapi.yaml` | `VideoProtocol` enum 加一个值 |

**`internal/executor`、`internal/capability`、`internal/asset`、`internal/httpapi`、
以及前端：一行不动。** 这是本设计成立与否的判据。

---

## 3. L1 能力声明层

### 3.1 一个模型 = 一份 schema

`models.capability` JSON 列存**完整的 `ModelCapabilitySchema`**，
`GET /api/models` 原样下发（只做 enabled 过滤与排序）。字段定义见
`contracts/capability-schema.ts`，后端不得另起一套。

### 3.2 同一套语义必须在两端跑出同样的结果

三件事前端和后端都要算：

| | 前端为什么要算 | 后端为什么要算 |
|---|---|---|
| `Condition` 求值 | 决定参数是否渲染、是否禁用 | 决定提交时该参数**是否合法**（不该出现的 key 直接拒） |
| 计价 | 用户改一下芯片就要立刻看到积分，不能每次打后端 | **钱的最终判定**，冻结/扣费按它 |
| 输入槽约束 | 上传前就校验，不让用户传完才报错 | 边界防御，前端可绕过 |

**两端算出的数必须一致**，否则用户看到 8 积分、被扣了 10 积分。
落地约束：`Condition` 的语义以 `capability-schema.ts` 的注释为准，
Go 侧 `capability.Evaluator` 与前端那个 40 行纯函数用**同一组测试用例**（JSON fixtures）
双向对拍。这组 fixtures 放 `docs/contracts/condition.cases.json`（实现阶段产出）。

计价算法（两端逐字一致）：

```
subtotal = pricing.base
for m in pricing.modifiers:            # 按数组顺序
    if evaluate(m.when): subtotal = (m.op == 'mul') ? subtotal*m.value : subtotal+m.value
if pricing.multiplier_param: subtotal *= Number(params[multiplier_param])
cost = rounding(subtotal)              # ceil / round / floor
```

### 3.3 隐式分流

`POST /api/tasks` **没有 mode 字段**。`inputs.reference_images` 非空 = 图生图，
空 = 文生图。判定在后端，前端不参与。落到 L0 就是 `request_mapping` 里那条
带 `when: has_input` 的规则是否命中——**分流没有 if，只有条件规则**。

### 3.4 验收线

> 接一个走已知协议的新模型 = 后台加一条配置记录，**前端零改动、后端零发版**。

唯一允许破例的情况：新模型需要一种全新的 `ControlKind`（8 种控件表达不了的交互）。
那时才允许改前端，且要先确认现有控件真的表达不了。

---

## 4. L2 执行层

### 4.1 状态机

```
                 ┌──────────────────────────────── cancel ──────┐
                 │                                              ▼
  [创建] ──► queued ──claim+Submit──► running ──Poll:succeeded──► succeeded
                 │                      │
                 │                      ├──Poll:failed(可重试)──► queued（attempt+1，退避）
                 │                      └──Poll:failed(终态)────► failed
                 └──校验失败/余额不足──► failed（未入队，同步返回）
```

- 只有 5 个状态，与 `capability-schema.ts` 的 `TaskStatus` 一致。
- 终态（`succeeded` / `failed` / `canceled`）**不可再迁移**，靠 DB 上的
  `UPDATE ... WHERE status IN ('queued','running')` 保证，不靠内存判断。
- 每次状态迁移**在同一个事务里**写一条 `task_events`，SSE 从这张表读。
  这样「状态变了但事件没推」和「事件推了但状态没落库」两种不一致都不可能出现。

### 4.2 worker 与租约

worker 不是「一个任务一个 goroutine 跑到底」，而是两个循环：

- **submit loop**：`UPDATE tasks SET lease_owner=?, lease_expires_at=now()+30s
  WHERE status='queued' AND (lease_expires_at IS NULL OR lease_expires_at<now()) LIMIT N`
  抢一批 → 调 `Submit` → 置 `running` + `next_poll_at`。
- **poll loop**：按索引 `(status, next_poll_at)` 扫到期任务 → `Poll` → 更新。

租约到期自动可被其他 worker 接管，所以**进程随时可以重启**，在途任务不丢。
这也是 `upstream_ref` 必须落库的原因：内存里的东西重启就没了，上游的任务还在跑。

### 4.3 轮询间隔与回调

| driver | 首次延迟 | 间隔 | 说明 |
|---|---|---|---|
| ark | 10s | 12s 固定 | 官方建议 10–15s；5s 视频约 1–3 分钟出 |
| openai_video | 10s | 10s 起，指数退避至 30s | 官方建议 10–20s |
| google_lro | 10s | 15s 固定 | LRO 无进度信息 |
| mock | 1s | 1s | 本地要跑得快 |

可被 `models.poll_interval_seconds` 覆盖。

**回调是预留，不是主路径。** Ark 与 Sora 都支持 webhook，端点
`POST /api/webhooks/tasks/{driver}` 已在契约里定义，签名密钥从 env 读。
本地开发无公网地址，默认关闭走轮询。

关键设计：**回调与轮询是同源的两条幂等路径**。回调只是把下一次轮询提前
（把 `next_poll_at` 置为 now），**不直接推进状态机**。这样打开 webhook 不引入
第二套状态迁移逻辑，也不怕重复投递。

### 4.4 重试策略

```go
var DefaultRetryPolicy = RetryPolicy{
    MaxAttempts: 5, BaseDelay: 2s, MaxDelay: 60s, Multiplier: 2.0, Jitter: 0.2,
}
```

| 情况 | 重试？ |
|---|---|
| 网络错误 / 5xx / 上游超时 | 是，指数退避 |
| `upstream_rate_limited`（429） | 是，退避更长，且前端显示「正在自动重试 (2/5)」 |
| `invalid_param`（400） | **否**，重试一万次也是 400 |
| `content_rejected` | **否** |
| `insufficient_credit` | **否**（根本没入队） |

重试期间任务停在 `queued`，`attempt` 递增，**不重复冻结积分**。

### 4.5 故障隔离

每个 provider 一个熔断器（`internal/executor/breaker.go`）：
连续失败 N 次 → `open`，该 provider 的任务直接快速失败并标 `internal_error`（不扣费），
其余 provider 不受影响；冷却后 `half_open` 放一个探针。
状态可在 `GET /api/admin/circuit-breakers` 看到。

外加 `providers.max_concurrency` 做并发闸门——一家慢不能把 worker 池全占满，
这是比熔断更早生效的一道闸。

### 4.6 失败三分类（三类都不扣费）

| code | 判定来源 | 前端呈现 | retryable | charged |
|---|---|---|---|---|
| `invalid_param` | L1 校验失败，或上游 400 且能定位到字段 | 红色，回填并高亮出错芯片 | true（改完重提） | **false** |
| `upstream_rate_limited` | 上游 429 / 配额错误 | 黄色，显示自动重试进度，**不给手动重试按钮** | true（系统自动） | **false** |
| `content_rejected` | 上游审核拒绝码 | 灰色，引导改提示词，不提供直接重试 | false | **false** |

映射发生在 driver 内部（每家的错误码格式不同），`domain.TaskError` 是归一化后的形态。
`charged` 字段即便恒为 false 也**显式回传**，前端据此显示「未扣费」——
这句话必须让用户看见，否则失败的心理成本会被记在钱上。

---

## 5. L3 资产层

### 5.1 ⭐ 转存：这条漏了会在上线后才炸

> **Ark 的 `video_url` 只有 24 小时有效期、任务记录只留 7 天；Sora 的产物同样有 `expires_at`。**

因此：**任务成功的那一刻，立即下载产物并转存到本地/对象存储，
数据库里存自己的 `storage_key`，绝不能存上游 URL 了事。**

```
Poll → succeeded
  └ FetchArtifact  → io.ReadCloser（四种 Kind 已归一）
  └ Store.Put      → storage_key = assets/{user_id}/{yyyymm}/{asset_id}.mp4
  └ Deriver        → thumb_512（图）/ poster + thumb_512（视频首帧）
  └ 计算 checksum、bytes、width/height/duration
  └ 写 assets 行、写 asset_lineage 边
  └ 只有这一切都成功，任务才置 succeeded、才 Charge
```

**顺序不能反。** 转存失败 = 任务失败（`internal_error`，可重试，不扣费）——
宁可让用户重跑，也不能给出一条 24 小时后变成 404 的记录。
`tasks.artifact_expires_at` 记下上游给的过期时间，用于监控「转存是否跑在过期前面」。

`GET /api/assets/{id}/content` 由本平台提供，支持 Range（视频拖动要用）。
**任何对外返回的 URL 都指向我们自己**，上游 URL 在离开 driver 后不复存在。

### 5.2 存储抽象

```go
type Store interface { Put(ctx, key string, r io.Reader, mime string) (ObjectInfo, error)
                       Get(ctx, key) (io.ReadCloser, error); Delete; Stat }
```

MVP 用本地 FS（`AIGC_STORAGE_ROOT`），换 S3/OSS 只是换一个实现。
不做预签名直传——本地跑没有意义，且上传要过服务端做输入槽校验。

### 5.3 血缘：两半缺一不可

| | 存哪 | 回答什么问题 |
|---|---|---|
| **怎么来的** | `assets.source` JSON（model_id / prompt / params / input_asset_ids / skill_id） | 「做同款」要把这些原样回填进生成器 |
| **从谁来的** | `asset_lineage` 表（parent, child, relation） | 「单卡重跑」「片段重拍」要顺着边找上游 |

`relation` 三种：`input`（A 作为输入生成了 B）、`rerun_of`（B 是 A 的重跑版本）、
`composed_from`（一键成片：B 由多个 A 拼成）。

画布卡片的 `refs` 是同一份血缘在画布坐标系里的投影，**不是可视连线**——
默认不画线，选中卡片时来源卡片描虚线高亮。这是明确取舍：既保住血缘，
又不把产品滑向 ComfyUI 的连线编排。

**丢了这层，「做同款」和「单卡重跑」同时废掉，且后面加不回来**
（历史产物没有留下 source 就永远补不上）——所以它是地基不是功能。

### 5.4 配额

`users.storage_quota_bytes` / `storage_used_bytes`。
提交任务前 `QuotaGuard` 预检（按 eta 估算产物大小），超了直接 `quota_exceeded`，不入队不冻结。
删除资产是软删（`deleted_at`），立即释放配额计数，物理文件由
`POST /api/admin/storage/cleanup` 批量回收——避免用户误删后无法恢复。
`uploads` 24h 未被引用即过期回收。

---

## 6. 画布持久化

### 6.1 增量 op，不做全量 PUT

```
PATCH /api/projects/{id}/canvas
{ "base_revision": 41, "ops": [ {"type":"card.move","id":"cd_7","x":120,"y":300}, ... ] }
→ { "revision": 42 }
```

服务端在一个事务里：校验 `base_revision == projects.revision` → 顺序应用 ops →
`revision+1` → 把整串 ops 写进 `canvas_ops`。

- 不匹配 → 409 `revision_conflict`，前端拉全量覆盖本地。**MVP 不做 CRDT**（明确不做协作）。
- `canvas_ops` 这张表天然满足 PRD 的「创作过程可回放」，本阶段只存不用。

### 6.2 Skill 是一条记录，不是一个分支

`skills` 表：`system_prompt` + `default_model_id` + `default_params`。
MVP 内置 3 个（短剧 / MV / 产品广告），管理后台可增删改。
`GET /api/skills` **不下发 `system_prompt` 原文**，只给 `system_prompt_ref`——
提示词是资产，没必要放进浏览器。以后接 Skill 市场只是换数据源。

### 6.3 一键成片

选中若干视频卡片 → 前端只负责顺序确认 → `POST /api/canvas/{id}/compose {card_ids[]}`
→ 后端建一个 `composed_from` 血缘的新任务，产出一张新视频卡片。
拼接在后端（实现阶段用 ffmpeg，属 DEM-64 范围）。

---

## 7. 积分记账

### 7.1 三段式

| 时机 | 动作 | ledger type |
|---|---|---|
| 提交，校验通过后 | 冻结 `estimated_cost` | `hold` |
| 成功 | 冻结转实扣（实际值可能因上游返回而修正） | `charge` |
| 失败 / 取消 | 全额释放 | `refund` |
| 管理员充值 | 直接加 | `topup` |

`users.credits`（可用）与 `credits_held`（冻结）两个字段，前端 `GET /api/me` 都拿得到。

### 7.2 两条铁律

1. **`credit_ledger` append-only，是余额的唯一真相**；`users.credits` 只是它的物化缓存，
   任何时候都能从流水重算校验。对账查不清的账 = 没有账。
2. **失败三分类一律不扣费。** `charge` 只在 `succeeded` 且资产已落库后发生。

### 7.3 幂等

- 用户侧：`tasks` 上的 `UNIQUE(user_id, client_token)`。断网重发返回**已存在的任务**，
  不产生第二次 `hold`。
- 管理员充值：`credit_ledger.idempotency_key` UNIQUE。管理员双击不会充两次。

余额变动一律推 `credit.updated` SSE 事件。**前端不做乐观扣减**——
乐观扣减在失败退费时会出现余额闪烁，钱的事不猜。

---

## 8. 数据层（MySQL 8.0+）

### 8.1 约定

- `utf8mb4` / `utf8mb4_0900_ai_ci`，InnoDB，`DATETIME(3)` UTC。
- 半结构化数据（capability schema、生成参数、画布卡片、request_mapping、事件 payload）
  用**真正的 `JSON` 列**，不退化成裸 TEXT。理由：管理后台要按 JSON 路径查
  （「哪些模型用了 1080p 选项」），裸文本做不到。
- 积分是 `BIGINT`，不用浮点。
- 实体主键 `CHAR(36)` 应用生成 UUID；**例外**：`task_events.id` 与 `canvas_ops.id` 是
  `BIGINT AUTO_INCREMENT`——前者直接充当 SSE 的 `Last-Event-ID` 游标，
  后者是画布 revision 的物理序。
- **连接串只从 `AIGC_MYSQL_DSN` 读，不进代码、不进样例**（`configs/.env.example` 用占位符）。

### 8.2 表清单

| 层 | 表 |
|---|---|
| auth | `users` |
| L0 配置 | `providers`、`models`、`config_versions` |
| L2 | `tasks`、`task_events` |
| L3 | `uploads`、`assets`、`asset_lineage` |
| billing | `credit_ledger` |
| canvas | `projects`、`canvas_cards`、`canvas_messages`、`canvas_ops` |
| skill | `skills` |

完整 DDL 见 [`../migrations/`](../migrations/)。

### 8.3 索引覆盖的高频查询

| 查询 | 索引 |
|---|---|
| 按用户查任务（列表页） | `tasks(user_id, created_at DESC)` |
| **按状态扫待轮询任务**（全系统最热） | `tasks(status, next_poll_at)` |
| 幂等命中 | `tasks(user_id, client_token)` UNIQUE |
| SSE 断线补发 | `task_events(user_id, id)` |
| 资产瀑布流游标分页 | `assets(user_id, created_at DESC, id)` |
| 按血缘查派生资产 | `asset_lineage(parent_asset_id)` + PK `(child, parent, relation)` |
| 积分流水 | `credit_ledger(user_id, id DESC)` |
| 模型列表 | `models(modality, enabled, display_order)` |

### 8.4 从零建库

```bash
# 1. 起库（MySQL 8.0+）
mysql -uroot -p -e "CREATE DATABASE aigc_pool CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"

# 2. 配环境（复制模板后填自己的值，.env 不进 git）
cp configs/.env.example configs/.env

# 3. 装 migrate CLI（一次性）
go install -tags 'mysql' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

# 4. 建表 + 灌种子（mock provider / 两个 mock 模型 / 3 个 skill）
make migrate-up

# 5. 回滚一步
make migrate-down
```

迁移**可重复执行**：每个 `.up.sql` 都有对应 `.down.sql`，`migrate down` 后再 `up` 得到同样的库。

---

## 9. 配置热更新（「模型池每周在换」的直接要求）

**改配置不重启、不发版。**

```
管理后台写 models/providers
  └ 同一事务里 INSERT config_versions(note)
进程内 ModelCache
  └ 每 5s SELECT MAX(id) FROM config_versions
  └ 变了就整表重载（模型数量是几十，全量重载比增量失效简单且不会漏）
  └ MAX(id) 同时是 GET /api/models 的 ETag 来源
```

前端 `GET /api/models` 带 `If-None-Match`，配置没变拿 304。
`Cache-Control: no-cache`（必须回源校验）而不是 `max-age`——模型池每周在变，
缓存住一个小时意味着新模型一小时后才可见。

---

## 10. 管理后台（本阶段只做后端 API 与数据模型，页面另开 issue）

| 能力 | 端点 | 为什么必须有 |
|---|---|---|
| 模型 / 供应商 CRUD | `/api/admin/providers`、`/api/admin/models` | **管理后台存在的首要理由**，也正是 L1 的操作界面 |
| 配置自检 | `/api/admin/models/{id}/probe` | 新增配置后立刻验证，不必等真实用户踩坑 |
| 用户管理 + 手工充值 | `/api/admin/users`、`/api/admin/users/{id}/credits` | PRD 早已定「管理员手工充值」却一直没有承载它的界面，**补缺口不是加需求** |
| 任务监控 | `/api/admin/tasks`、`/stats`、`/{id}/retry`、`/{id}/cancel` | 队列状态、失败分布、手动重试 |
| 熔断可观测 | `/api/admin/circuit-breakers` | 故障隔离得看得见才有用 |
| 存储配额与清理 | `/api/admin/storage/usage`、`/cleanup` | 配额是设计的一部分，回收必须有入口 |
| Skill 管理 | `/api/admin/skills` | Skill 是记录不是分支，就必须能改 |

**权限隔离**：`users.role ∈ {user, admin}`，管理接口独立前缀 `/api/admin`，
一个中间件统一拦截，非 admin 一律 403 `forbidden`。
不做细粒度 RBAC——两个角色够用，加了就是过度设计。

**凭证永不回显**：`GET /api/admin/providers` 只返回 `credential_ref`（环境变量名）
与 `credential_present`（布尔），**永远不回显密钥本身**。

---

## 11. 目录结构（分层边界在目录上可见）

```
cmd/aigcd/                  # 单二进制：API + worker，-mode 切分
internal/
  config/                   # 环境变量加载（唯一读 env 的地方）
  domain/                   # 纯类型，零依赖，被所有层引用
  capability/               # L1 schema / 条件求值 / 计价 / 校验
  adapter/                  # L0 接口 + mapping 引擎
    openaicompat/           #   chat + images（OpenAI 协议）
    ark/ openaivideo/ googlelro/   #   三个视频协议
    mock/                   #   无凭证跑全链路
  executor/                 # L2 状态机 / 队列租约 / 轮询 / 重试 / 熔断
  asset/                    # L3 存储 / 转存 / 派生 / 血缘 / 配额
  billing/                  # 积分记账
  canvas/ skill/ stream/    # 横向服务（画布 op / Skill / SSE Hub）
  store/                    # 仓储接口
    mysql/                  #   MySQL 实现
  httpapi/                  # 路由 / 中间件 / handler
migrations/                 # golang-migrate
configs/.env.example        # 占位符，无真实值
docs/                       # 本目录
```

骨架内**只有接口、类型、常量与包注释**，加上两处完整代码：`internal/config`（真实的
env 加载器）与 `cmd/aigcd/main.go`（真实的最小程序：加载配置、`/healthz`、优雅退出）。
**没有 `panic("not implemented")`、没有 TODO 桩**——半成品比空白更难维护。

---

## 12. 契约增补记录（需前端同步）

本阶段在 `capability-schema.ts` 之外新增了 **1 个**字段，实现阶段需在 TS 侧补齐：

- `CreateTaskRequest.card_id?: string` —— 单卡重跑时携带，产物原地替换该卡片、
  旧版本进 `history`，位置/大小/refs 不变（前端设计 §5.7 的「锚点锁定」）。
  没有它，重跑只能新建卡片，锚点锁定实现不了。

另有 1 处**拆分**（非新增语义）：`openapi.yaml` 把错误码分成
`TaskErrorCode`（与 TS 逐值一致的 5 个产品语义码）与
`ApiErrorCode`（= 前者 + `unauthorized` / `forbidden` / `not_found` / `conflict` /
`revision_conflict` / `rate_limited` / `quota_exceeded` 等传输语义码）。
不合并的理由：401/403/404 混进 `TaskErrorCode` 会污染前端的失败三分类分支。
前端遇到未知 code 按 `internal_error` 降级，不允许白屏。

---

## 13. 本阶段明确不做

- 不写业务实现（DEM-64）
- 管理端页面（另开 issue）
- 视频 driver 只此三个 + mock，不做更多供应商
- 支付网关、社区 / 作品流、Skill 市场与 UGC 上传、多人协作、
  WebUI / ComfyUI / LoRA 训练、数字人 / 动作模仿、视频精剪时间轴
- 不部署，`release_target` 留空
- 细粒度 RBAC、多租户、审计日志（两个角色够用）

## 14. 实现阶段的第一批决定（DEM-64 开工时先定）

这几件本阶段可以不定，但实现第一天就得定，留在这里免得漏：

1. **依赖清单**：骨架是 stdlib-only（本地无网络，`go mod tidy` 拉不动）。实现阶段需加
   `go-sql-driver/mysql`、`golang-jwt/jwt`、`golang.org/x/crypto/bcrypt`、
   `golang-migrate`；ffmpeg 走外部进程不进 go.mod。
2. **`condition.cases.json`**：前后端条件求值的对拍用例，两边 CI 都跑。
3. **首个 admin 账号**：一条 CLI 子命令创建，不在迁移里硬编码密码。
4. **SSE 事件保留期**：`task_events` 需要一个清理策略（建议保留 7 天），
   否则这张表只涨不跌。

# AIGC Pool

多模型 AIGC 创作平台。Go 后端 + MySQL 8.0，前后端分离，本地开发（本阶段不部署）。

**当前状态：前后端已联调。** 后端分层骨架已填成可运行的服务：认证、模型配置热读、
任务状态机、worker 池与轮询、SSE、产物转存与血缘、积分记账、管理接口、画布持久化。
前端（`frontend/`）默认直连真后端，内置的进程内 mock 后端退为显式 opt-in 的离线开发模式。

## 文档

| 文件 | 内容 |
|---|---|
| [`docs/tech-design.md`](docs/tech-design.md) | 五层架构、三个视频协议 driver、状态机、资产血缘、积分记账、数据模型 |
| [`docs/openapi.yaml`](docs/openapi.yaml) | **前后端唯一契约**，40 端点 / 82 schema，前端据此 mock 并行开发 |
| [`docs/contracts/capability-schema.ts`](docs/contracts/capability-schema.ts) | 动态表单契约（TS 侧权威，openapi 是其镜像） |
| [`docs/frontend-design.md`](docs/frontend-design.md) | 前端结构与对后端的诉求清单 |
| [`docs/ui-design.md`](docs/ui-design.md) | 视觉与交互规范 |

## 目录

```
cmd/aigcd/       单二进制（API + worker）
internal/
  config/        环境变量加载（唯一读 env 的地方）
  domain/        纯类型，零依赖
  capability/    L1 能力声明：schema / 条件求值 / 计价 / 校验
  adapter/       L0 适配：接口 + mapping 引擎 + 5 个 driver
  executor/      L2 执行：状态机 / 队列租约 / 轮询 / 重试 / 熔断
  asset/         L3 资产：存储 / 转存 / 派生 / 血缘 / 配额
  billing/       积分记账
  canvas/ skill/ stream/   画布 op / Skill / SSE Hub
  store/         仓储接口 + MySQL 实现
  httpapi/       路由 / 中间件 / handler
migrations/      SQL 迁移（由 aigcd 自己执行，格式兼容 golang-migrate）
configs/         .env.example（占位符，无真实值）
docker-compose.yml  本地开发用的 MySQL 8.4
```

分层边界在目录上可见。依赖方向单向向下，且 `internal/adapter` 之上的包
**不允许 import 任何具体 driver 包**——协议差异只存在于 driver 内部。

## 从零跑起来

> **这个仓库不含任何真实凭证。** 上游 API Key、JWT 密钥、数据库口令一律只从项目根的
> `.env` 读（该文件在 `.gitignore` 里，从未进过版本库），部署时在各自的机器上自行填写。
> 仓库里出现的 `devpass` / `devroot` / `admin-dev-only` 这类值是 `docker-compose.yml`
> 与 `aigcd seed` 的**本地开发占位符**，任何对外可达的部署都必须换掉。

前置：Go 1.26+、Docker（本地 MySQL 用它拉起；已有 MySQL 8.0+ 也行，改 DSN 即可）。

```bash
# 1. 配环境（.env 在项目根，已在 .gitignore 里，进程启动时自动读）
cp configs/.env.example .env
$EDITOR .env          # 至少把 AIGC_JWT_SECRET 换成一串长随机字符

# 2. 一条龙：起库 → 建表 → 灌种子 → 起服务
make dev
```

`make dev` 等价于依次跑这四步，分开跑也行：

```bash
make db-up        # docker compose 起 MySQL 8.4，等到健康检查通过
make migrate-up   # 建表 + 灌参考数据（mock provider / 两个 mock 模型 / 三个 skill）
make seed         # 建 admin 与 demo 两个账号，各给一笔起始积分
make run          # 起 HTTP 服务与 worker 池
```

迁移由 `aigcd` 自己执行（`aigcd migrate up|down [n]`），**不需要装 golang-migrate CLI**。
文件命名与格式仍完全兼容它，需要时可以换回去。`make migrate-down` 回滚一步；
每个 `.up.sql` 都有对应 `.down.sql`，`down` 后再 `up` 得到同样的库。

`make db-down` 停掉本地 MySQL（数据留在 docker volume 里）。

### 种子账号

`make seed` 建两个账号，口令从环境变量读，未设置时用**明摆着的开发占位符**：

| 用户名 | 角色 | 口令来源 | 默认值（仅本地开发） |
|---|---|---|---|
| `admin` | admin | `AIGC_SEED_ADMIN_PASSWORD` | `admin-dev-only` |
| `demo` | user | `AIGC_SEED_USER_PASSWORD` | `user-dev-only` |

**这两个默认口令仅供本机开发。** 任何对外可达的部署都必须先设好这两个环境变量再 seed。
重复跑 `make seed` 不会覆盖已存在账号的口令。

## 本地端口规范

**三条硬规矩，对本仓库所有进程生效：**

1. **禁用 8080。** 任何监听都不许落在 8080 上。
2. **所有监听端口 >= 10000。**
3. **端口被占必须响亮地失败**，不许静默退化、不许自动漂到别的端口。

| 进程 | 端口 | 怎么改 |
|---|---|---|
| 后端 HTTP | `18080`（`AIGC_HTTP_ADDR` 缺省值） | `AIGC_HTTP_ADDR=:18081 make run` |
| 前端 dev server | `15173`（`frontend/vite.config.ts`，`strictPort`） | `npm run dev -- --port 15174` |
| 前端代理指向的后端 | `http://localhost:18080` | `frontend/.env.local` 里写 `VITE_API_PROXY_TARGET=` |
| 本地 MySQL（宿主机映射） | `13306`（容器内仍是 `3306`） | 改 `docker-compose.yml`，并同步 `.env` 里 `AIGC_MYSQL_DSN` 的端口 |

改后端端口时记得把前端代理目标一起改，两边对不上会整页 502。

### 为什么有这条规矩

2026-08-08 凌晨出过一次**持续 5.5 小时的静默中断**，根因就是 8080 冲突：

一个 `aigcd serve` 绑在 IPv6 的 `*:8080` 上，而同机另一个服务只发布在
IPv4 `127.0.0.1:8080`。macOS 上 `localhost` 优先解析到 `::1`，于是所有
`localhost:8080` 的请求都被 aigcd 接管，对每条不认识的路由返回 Go 默认的
`404 page not found`。

**表现极具迷惑性**：走 WebSocket 的心跳仍然显示「在线」，本地任务照常跑完，
只有 HTTP 回传全废——活干完了，服务端永远不知道。直到 5.5 小时后才被人发现。

所以规矩不是洁癖，是这次事故的直接对策，并且已经写成了代码约束而不只是文档约定：

- `internal/config` 在启动时校验 `AIGC_HTTP_ADDR`，端口 < 10000（含 8080）**直接拒绝启动**
- `aigcd serve` 在连库、起 worker **之前**就先抢监听口，被占时报出占用端口、
  查凶手的 `lsof` 命令和换端口的办法，然后退出
- 前端 dev server 开了 `strictPort`，端口被占直接失败，不会 +1 漂走

### 已知例外

`migrations/` 下**已经 apply 的迁移文件是不可变历史，不原地修改**。
`migrations/000002_seed_reference_data.up.sql` 里 mock provider 的 `base_url`
仍写着 `http://localhost:8080/__mock`——那是一条惰性数据：`internal/adapter/mock/`
根本不发 HTTP，也从不读 `Provider.BaseURL`（只有 `googlelro` / `openaivideo` /
`openaicompat` 三个真 driver 读它），它只是为了让 mock 与真实供应商保持相同的配置形态。
**「全仓 grep 不到 8080」这条验收标准排除 `migrations/` 下已 apply 的文件**，
不必再纠结一遍。

## 前端

前端是独立的 Vite + React 应用，源码在 `frontend/`，详见 [`frontend/README.md`](frontend/README.md)。

```bash
cd frontend
npm install       # 用 npm，仓库里是 package-lock.json
npm run dev       # http://localhost:15173
```

**默认直连真后端**，所以先按上面「从零跑起来」把后端起起来。前端请求同源 `/api`，
由 dev server 代理到后端，避开 CORS 与 SSE 缓冲。

### 离线开发模式（VITE_USE_MOCK）

`frontend/src/mock/` 有一个进程内的假后端，形状与 `docs/openapi.yaml` 一致，
数据落在 `localStorage`。它**只在显式打开时生效**：

| `VITE_USE_MOCK` | 行为 |
|---|---|
| 未设置 / 任何其他值（默认） | 请求打到真后端 |
| `true` | 请求在 `src/api/client.ts` 里被拦下，路由到进程内 mock，不发任何网络请求 |

```bash
cp .env.example .env.local    # 里面 VITE_USE_MOCK 是注释掉的，取消注释才开
```

后端没起、或者只想调界面时才需要它。注意 **`.env.local` 优先级高于代码缺省值**——
排查「为什么数据看着像假的」时先看这个文件在不在。

## 无凭证也能跑

本地**没有任何真实上游 API Key**，这是已知前提。种子数据里有一个 `mock` provider
和两个 mock 模型，走**完全相同**的 driver interface、状态机、转存与记账路径，
只把最底层的 HTTP 调用换成本地生成的占位产物。全链路（提交 → 排队 → 生成 → 转存 →
缩略图 → 血缘 → 扣费 → SSE 推送）在零凭证下可完整跑通，失败三分类也能按概率注入。

### 验收：一条命令走完整链路

`scripts/smoke` 是一个当客户端用的端到端走查器，把验收线上的每一环都真的跑一遍，
任意一环断掉就报错退出：

```bash
make run          # 终端 A
make smoke        # 终端 B
```

它覆盖：注册登录与错误口令被拒 → admin/普通用户隔离 → 充值 → 模型列表与
capability schema（含 `If-None-Match` 命中 304）→ 接 SSE → 图片任务（同步族，
base64 内联产物）→ 转存与资产入库 → 积分扣减与流水 → 同 `client_token` 幂等 →
视频任务（异步族，**走轮询**，二进制产物流式转存）→ `status=active` 对账 →
资产列表与血缘 → skill 与画布持久化 → 管理端统计。

写成 Go 而不是 curl 脚本，是因为 SSE 要当流读、边读边断言，curl 做不到。

## 凭证与密钥

**任何密钥都不进代码、不进库、不进 git。**
`providers.credential_ref` 存的是**环境变量名**（如 `AIGC_PROVIDER_ARK_KEY`），
不是密钥本身；管理接口只回显该变量是否已配置的布尔值，永不回显密钥。
数据库连接串只从 `AIGC_MYSQL_DSN` 读，`configs/.env.example` 里一律是占位符。

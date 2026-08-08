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

## 前端

前端是独立的 Vite + React 应用，源码在 `frontend/`，详见 [`frontend/README.md`](frontend/README.md)。

```bash
cd frontend
npm install       # 用 npm，仓库里是 package-lock.json
npm run dev       # http://localhost:5173
```

**默认直连真后端**，所以先按上面「从零跑起来」把后端起起来。前端请求同源 `/api`，
由 dev server 代理到后端，避开 CORS 与 SSE 缓冲。

### 端口

| 进程 | 端口 | 怎么改 |
|---|---|---|
| 后端 | `:8080`（`AIGC_HTTP_ADDR` 缺省值） | `AIGC_HTTP_ADDR=:18080 make run` |
| 前端 dev server | `5173` | `npm run dev -- --port 15173` |
| 前端代理指向的后端 | `http://localhost:18080` | `frontend/.env.local` 里写 `VITE_API_PROXY_TARGET=` |

代理默认指向 **18080** 而后端默认监听 **8080**，两边对不上就会整页 502。二选一：
起后端时带上 `AIGC_HTTP_ADDR=:18080`，或者在 `frontend/.env.local` 里把
`VITE_API_PROXY_TARGET` 指回 `http://localhost:8080`。

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

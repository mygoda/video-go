# AIGC Pool

多模型 AIGC 创作平台。Go 后端 + MySQL 8.0，前后端分离，本地开发（本阶段不部署）。

**当前状态：技术设计阶段（DEM-62）。** 仓库里是设计文档 + 分层骨架 + 数据库迁移，
**尚无业务实现**——实现拆在 DEM-64（后端）/ DEM-65（前端）。

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
migrations/      golang-migrate
configs/         .env.example（占位符，无真实值）
```

分层边界在目录上可见。依赖方向单向向下，且 `internal/adapter` 之上的包
**不允许 import 任何具体 driver 包**——协议差异只存在于 driver 内部。

## 从零跑起来

前置：Go 1.26+、MySQL 8.0+。

```bash
# 1. 建库
mysql -uroot -p -e "CREATE DATABASE aigc_pool CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"

# 2. 配环境（.env 不进 git）
cp configs/.env.example configs/.env
$EDITOR configs/.env

# 3. 装迁移工具（一次性）
go install -tags 'mysql' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

# 4. 建表 + 灌种子
make migrate-up

# 5. 编译并起服务
make build && make run
```

`make migrate-down` 回滚一步。每个 `.up.sql` 都有对应 `.down.sql`，
`down` 后再 `up` 得到同样的库。

## 无凭证也能跑

本地**没有任何真实上游 API Key**，这是已知前提。种子数据里有一个 `mock` provider
和两个 mock 模型，走**完全相同**的 driver interface、状态机、转存与记账路径，
只把最底层的 HTTP 调用换成本地生成的占位产物。全链路（提交 → 排队 → 生成 → 转存 →
缩略图 → 血缘 → 扣费 → SSE 推送）在零凭证下可完整跑通，失败三分类也能按概率注入。

## 凭证与密钥

**任何密钥都不进代码、不进库、不进 git。**
`providers.credential_ref` 存的是**环境变量名**（如 `AIGC_PROVIDER_ARK_KEY`），
不是密钥本身；管理接口只回显该变量是否已配置的布尔值，永不回显密钥。
数据库连接串只从 `AIGC_MYSQL_DSN` 读，`configs/.env.example` 里一律是占位符。

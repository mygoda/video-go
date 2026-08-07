-- =============================================================================
-- 000001_init_schema.up.sql
--
-- AIGC Pool 初始 schema。MySQL 8.0+ / InnoDB / utf8mb4_0900_ai_ci。
--
-- 约定：
--   * 实体主键为应用生成的 UUID（CHAR(36)），两处例外用自增 BIGINT：
--     task_events.id 兼作 SSE 的 Last-Event-ID 游标，canvas_ops.id 兼作 op 顺序号，
--     两者都必须单调递增，UUID 做不到。
--   * 所有时间列为 DATETIME(3)，语义上恒为 UTC。DATETIME 不带时区，
--     因此连接串必须固定 loc=UTC / time_zone='+00:00'，否则毫秒时间会漂。
--   * 积分与字节数一律 BIGINT 整数，绝不用浮点。
--   * 半结构化数据（capability / params / canvas op / SSE payload 等）用原生 JSON 列，
--     不用 TEXT 存 JSON —— 否则无法用 JSON_EXTRACT 做校验与查询。
--   * 供应商凭证只存**环境变量名**（credential_ref），密钥本身不入库。
--
-- golang-migrate 的 mysql driver 整文件一次 Exec，DSN 需带 multiStatements=true。
-- =============================================================================

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：auth —— 用户与配额
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE users (
  id                  CHAR(36)      NOT NULL,
  username            VARCHAR(64)   NOT NULL,
  password_hash       VARCHAR(255)  NOT NULL,
  role                ENUM('user','admin')     NOT NULL DEFAULT 'user',
  status              ENUM('active','disabled') NOT NULL DEFAULT 'active',
  -- credits 是 credit_ledger 的物化缓存，不是真相；真相永远是流水的累加值
  credits             BIGINT        NOT NULL DEFAULT 0,
  credits_held        BIGINT        NOT NULL DEFAULT 0,
  storage_used_bytes  BIGINT        NOT NULL DEFAULT 0,
  storage_quota_bytes BIGINT        NOT NULL DEFAULT 10737418240,
  last_active_at      DATETIME(3)   NULL,
  created_at          DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at          DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uq_users_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L0 适配器配置 —— 供应商接入点
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE providers (
  id               CHAR(36)      NOT NULL,
  name             VARCHAR(120)  NOT NULL,
  base_url         VARCHAR(512)  NOT NULL,
  -- 环境变量名，例 'AIGC_PROVIDER_ARK_KEY'。**绝不是密钥本身**，
  -- 密钥只从 env 读，不入库、不进代码、不进 seed
  credential_ref   VARCHAR(190)  NOT NULL,
  auth_style       ENUM('bearer','header','query') NOT NULL DEFAULT 'bearer',
  auth_header_name VARCHAR(120)  NULL,
  enabled          TINYINT(1)    NOT NULL DEFAULT 1,
  timeout_ms       INT           NOT NULL DEFAULT 30000,
  -- 该供应商的全局并发闸门，故障隔离的一部分
  max_concurrency  INT           NOT NULL DEFAULT 8,
  created_at       DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at       DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uq_providers_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L1 能力声明 —— 一条记录 = 一个可用模型。加模型不改代码、不发版
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE models (
  -- 人类可读 slug（如 'seedream-5-pro'），不是 UUID：它会原样出现在 API 与前端表单里
  id                    CHAR(64)      NOT NULL,
  provider_id           CHAR(36)      NOT NULL,
  upstream_model        VARCHAR(190)  NOT NULL,
  protocol_family       ENUM('chat','images','video','mock') NOT NULL,
  -- 仅 protocol_family='video' 时必填，见下方 CHECK
  video_protocol        ENUM('ark','openai_video','google_lro','mock') NULL,
  modality              ENUM('image','video') NOT NULL,
  enabled               TINYINT(1)    NOT NULL DEFAULT 1,
  display_order         INT           NOT NULL DEFAULT 0,
  name                  VARCHAR(190)  NOT NULL,
  vendor                VARCHAR(120)  NOT NULL,
  description           VARCHAR(500)  NULL,
  preview_url           VARCHAR(512)  NULL,
  -- 完整 ModelCapabilitySchema，逐字下发给 GET /api/models
  capability            JSON          NOT NULL,
  -- 声明式的 平台参数 → 上游请求体 翻译规则；没有它每接一个模型都要改 driver
  request_mapping       JSON          NULL,
  poll_interval_seconds INT           NULL,
  created_at            DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at            DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_models_modality_enabled_order (modality, enabled, display_order),
  KEY idx_models_provider (provider_id),
  CONSTRAINT fk_models_provider FOREIGN KEY (provider_id) REFERENCES providers (id) ON DELETE RESTRICT,
  CONSTRAINT chk_models_video_protocol CHECK (protocol_family <> 'video' OR video_protocol IS NOT NULL)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L0 适配器配置 —— 配置版本号（热更新的触发器）
-- providers / models / skills 任一变更就 INSERT 一行。服务端每隔几秒轮询 MAX(id)：
-- 变了就重建进程内模型配置缓存，并作为 GET /api/models 的 ETag。
-- 配置生效因此不需要重启、不需要重新部署，多实例也天然一致。
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE config_versions (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  changed_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  changed_by CHAR(36)        NULL,
  note       VARCHAR(200)    NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：skills —— 预置系统提示词 + 默认参数的可插拔记录，不是硬编码分支
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE skills (
  id                CHAR(36)      NOT NULL,
  name              VARCHAR(120)  NOT NULL,
  description       VARCHAR(500)  NULL,
  -- 原文不下发前端，GET /api/skills 只给 system_prompt_ref
  system_prompt     TEXT          NOT NULL,
  -- 不加 FK：模型配置可被管理员删除，Skill 的默认模型只是个软引用，失效时回落到默认模型
  default_model_id  CHAR(64)      NULL,
  default_params    JSON          NULL,
  enabled           TINYINT(1)    NOT NULL DEFAULT 1,
  display_order     INT           NOT NULL DEFAULT 0,
  created_at        DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at        DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_skills_enabled_order (enabled, display_order)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L2 执行 —— 任务状态机的持久化载体
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE tasks (
  id                  CHAR(36)      NOT NULL,
  user_id             CHAR(36)      NOT NULL,
  model_id            CHAR(64)      NOT NULL,
  -- 冗余自 models.provider_id：按供应商做熔断/并发统计时不必回表 join
  provider_id         CHAR(36)      NOT NULL,
  -- canvas_id / card_id 故意不加 FK：projects → canvas_cards → tasks → projects 会成环，
  -- 且画布可先于任务终结被删除。孤儿引用由应用层容忍（卡片没了就只是不回填）
  canvas_id           CHAR(36)      NULL,
  card_id             CHAR(36)      NULL,
  -- 幂等键：同一用户重复提交同一 token 直接返回已存在任务，断网重发不会双开任务、双重扣费
  client_token        VARCHAR(64)   NOT NULL,
  status              ENUM('queued','running','succeeded','failed','canceled') NOT NULL DEFAULT 'queued',
  prompt              TEXT          NULL,
  params              JSON          NULL,
  inputs              JSON          NULL,
  estimated_cost      BIGINT        NOT NULL DEFAULT 0,
  -- 仅成功后有值；失败三分类恒不扣费
  actual_cost         BIGINT        NULL,
  progress            FLOAT         NULL,
  queue_position      INT           NULL,
  eta_seconds         INT           NULL,
  attempt             INT           NOT NULL DEFAULT 0,
  max_attempts        INT           NOT NULL DEFAULT 5,
  -- 上游任务 id，或 google_lro 的 operation name（形如 projects/x/operations/y），故长且为字符串
  upstream_ref        VARCHAR(512)  NULL,
  -- 归一化前的上游原始状态，排障用
  upstream_status_raw VARCHAR(64)   NULL,
  next_poll_at        DATETIME(3)   NULL,
  -- 多实例下的轮询租约：抢到租约的实例独占推进该任务，租约过期后其他实例可接管
  lease_owner         VARCHAR(64)   NULL,
  lease_expires_at    DATETIME(3)   NULL,
  error_code          VARCHAR(32)   NULL,
  error_message       TEXT          NULL,
  error_field_errors  JSON          NULL,
  artifact_expires_at DATETIME(3)   NULL,
  created_at          DATETIME(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  started_at          DATETIME(3)   NULL,
  finished_at         DATETIME(3)   NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_tasks_user_client_token (user_id, client_token),
  KEY idx_tasks_user_created (user_id, created_at DESC),
  -- 全系统最热的查询：轮询器每几秒扫一次「到期该轮询的任务」
  KEY idx_tasks_status_next_poll (status, next_poll_at),
  KEY idx_tasks_canvas (canvas_id),
  KEY idx_tasks_model_created (model_id, created_at),
  KEY idx_tasks_provider_status (provider_id, status),
  CONSTRAINT fk_tasks_user     FOREIGN KEY (user_id)     REFERENCES users (id)     ON DELETE RESTRICT,
  CONSTRAINT fk_tasks_model    FOREIGN KEY (model_id)    REFERENCES models (id)    ON DELETE RESTRICT,
  CONSTRAINT fk_tasks_provider FOREIGN KEY (provider_id) REFERENCES providers (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L2 执行 —— SSE 事件流的持久化。id 单调递增，直接兼作 Last-Event-ID 游标，
-- 断线重连时按 (user_id, id > last) 补发，不需要额外的 offset 表
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE task_events (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id    CHAR(36)     NOT NULL,
  task_id    CHAR(36)     NULL,
  -- 'task.updated' / 'task.succeeded' / 'task.failed' / 'credit.updated' / 'canvas.card.updated'
  event_type VARCHAR(40)  NOT NULL,
  payload    JSON         NOT NULL,
  created_at DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  -- SSE 从 Last-Event-ID 续传
  KEY idx_task_events_user_id (user_id, id),
  KEY idx_task_events_task (task_id),
  CONSTRAINT fk_task_events_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
  CONSTRAINT fk_task_events_task FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L3 资产 —— 产物一律转存到本平台存储，绝不保留上游 URL（上游 24h 就失效）
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE assets (
  id          CHAR(36)     NOT NULL,
  user_id     CHAR(36)     NOT NULL,
  task_id     CHAR(36)     NULL,
  type        ENUM('image','video','audio','text') NOT NULL,
  storage_key VARCHAR(512) NOT NULL,
  thumb_key   VARCHAR(512) NULL,
  poster_key  VARCHAR(512) NULL,
  mime        VARCHAR(120) NOT NULL,
  bytes       BIGINT       NOT NULL DEFAULT 0,
  width       INT          NULL,
  height      INT          NULL,
  duration_ms INT          NULL,
  checksum    CHAR(64)     NULL,
  -- 生成参数快照 {model_id, prompt, params, input_asset_ids, skill_id}：
  -- 血缘的「怎么来的」那一半。没有它，「做同款」与「单卡重跑」同时废掉
  `source`    JSON         NOT NULL,
  -- 软删：释放配额但保留血缘图的节点，否则祖先链会断
  deleted_at  DATETIME(3)  NULL,
  created_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  -- 资产瀑布流的游标分页键（id 兜底同毫秒内的稳定排序）
  KEY idx_assets_user_created_id (user_id, created_at DESC, id),
  KEY idx_assets_task (task_id),
  KEY idx_assets_deleted (deleted_at),
  CONSTRAINT fk_assets_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
  CONSTRAINT fk_assets_task FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L3 资产 —— 输入素材的临时对象。24h 未被任务引用即由 reaper 回收；
-- 被任务引用后提升为 asset（promoted_asset_id 指向提升结果）
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE uploads (
  id                CHAR(36)     NOT NULL,
  user_id           CHAR(36)     NOT NULL,
  -- 目标输入槽 key，用于按 InputSlotSpec 做服务端校验
  slot              VARCHAR(64)  NULL,
  storage_key       VARCHAR(512) NOT NULL,
  mime              VARCHAR(120) NOT NULL,
  bytes             BIGINT       NOT NULL DEFAULT 0,
  width             INT          NULL,
  height            INT          NULL,
  status            ENUM('pending','promoted','expired') NOT NULL DEFAULT 'pending',
  expires_at        DATETIME(3)  NOT NULL,
  promoted_asset_id CHAR(36)     NULL,
  created_at        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  -- reaper 扫「已过期且仍 pending」
  KEY idx_uploads_status_expires (status, expires_at),
  KEY idx_uploads_user_created (user_id, created_at),
  KEY idx_uploads_promoted_asset (promoted_asset_id),
  CONSTRAINT fk_uploads_user  FOREIGN KEY (user_id)           REFERENCES users (id)  ON DELETE CASCADE,
  CONSTRAINT fk_uploads_asset FOREIGN KEY (promoted_asset_id) REFERENCES assets (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L3 资产 —— 血缘边（「从谁来的」那一半，与 assets.source 互补）。
-- PK 天然覆盖 子→父（追祖先）；额外索引覆盖 父→子（追派生）
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE asset_lineage (
  parent_asset_id CHAR(36)    NOT NULL,
  child_asset_id  CHAR(36)    NOT NULL,
  relation        ENUM('input','rerun_of','composed_from') NOT NULL,
  created_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (child_asset_id, parent_asset_id, relation),
  KEY idx_asset_lineage_parent (parent_asset_id),
  CONSTRAINT fk_asset_lineage_parent FOREIGN KEY (parent_asset_id) REFERENCES assets (id) ON DELETE CASCADE,
  CONSTRAINT fk_asset_lineage_child  FOREIGN KEY (child_asset_id)  REFERENCES assets (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：billing —— append-only 流水，余额的唯一真相。users.credits 只是它的物化缓存。
-- 只 INSERT，永不 UPDATE / DELETE；对账用 SUM(amount) 与 users.credits 比对。
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE credit_ledger (
  -- 应用生成 UUID；建议用 UUIDv7 以便 (user_id, id DESC) 与时间序一致，游标分页才稳定
  id              CHAR(36)     NOT NULL,
  user_id         CHAR(36)     NOT NULL,
  task_id         CHAR(36)     NULL,
  -- hold=冻结 charge=成功实扣 refund=失败/取消退回 topup=管理员充值 adjust=人工修正
  type            ENUM('hold','charge','refund','topup','adjust') NOT NULL,
  -- 有符号：正=入账，负=出账
  amount          BIGINT       NOT NULL,
  balance_after   BIGINT       NOT NULL,
  reason          VARCHAR(200) NULL,
  -- 管理员操作时记录；用户自发行为为 NULL。不加 FK 以免为其单独建索引
  operator_id     CHAR(36)     NULL,
  -- 防管理员重复点击充两次；NULL 不参与唯一性约束，故非管理操作可留空
  idempotency_key VARCHAR(64)  NULL,
  created_at      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uq_credit_ledger_idempotency (idempotency_key),
  KEY idx_credit_ledger_user_id (user_id, id DESC),
  KEY idx_credit_ledger_task (task_id),
  CONSTRAINT fk_credit_ledger_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
  CONSTRAINT fk_credit_ledger_task FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：canvas —— 画布项目。revision 是乐观锁：PATCH 的 base_revision 不匹配即 409
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE projects (
  id             CHAR(36)     NOT NULL,
  user_id        CHAR(36)     NOT NULL,
  name           VARCHAR(120) NOT NULL,
  revision       BIGINT       NOT NULL DEFAULT 0,
  -- 上次退出时的视口 {x, y, k}
  viewport       JSON         NULL,
  -- 软引用，不加 FK：封面资产可被用户删除，届时前端回落到首张卡片
  cover_asset_id CHAR(36)     NULL,
  deleted_at     DATETIME(3)  NULL,
  created_at     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_projects_user_deleted_updated (user_id, deleted_at, updated_at DESC),
  CONSTRAINT fk_projects_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：canvas —— 卡片。task_id / asset_id / model_id 均为软引用不加 FK：
-- 模型配置可被管理员删除、资产可被用户删除，卡片仍需保留历史形态
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE canvas_cards (
  id          CHAR(36)    NOT NULL,
  project_id  CHAR(36)    NOT NULL,
  kind        ENUM('text','image','video') NOT NULL,
  x           DOUBLE      NOT NULL DEFAULT 0,
  y           DOUBLE      NOT NULL DEFAULT 0,
  w           DOUBLE      NOT NULL DEFAULT 0,
  h           DOUBLE      NOT NULL DEFAULT 0,
  z           DOUBLE      NOT NULL DEFAULT 0,
  task_id     CHAR(36)    NULL,
  asset_id    CHAR(36)    NULL,
  `text`      TEXT        NULL,
  model_id    CHAR(64)    NULL,
  prompt      TEXT        NULL,
  params      JSON        NULL,
  -- 源卡片 id 数组。这是**血缘数据，不是画布上画出来的连线**：
  -- 「多卡引用为下次输入」靠它，不靠用户手动连线
  refs        JSON        NULL,
  -- 重跑产生的历史版本 [{asset_id, prompt, at}]，卡片左下角 ‹2/3› 版本切换器用
  history     JSON        NULL,
  -- 用户手动移动过就置 0，该区域不再参与自动重排
  auto_placed TINYINT(1)  NOT NULL DEFAULT 1,
  deleted_at  DATETIME(3) NULL,
  created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  -- 拉画布全量：一个项目的未删除卡片
  KEY idx_canvas_cards_project_deleted (project_id, deleted_at),
  CONSTRAINT fk_canvas_cards_project FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：canvas —— 画布内对话。ref_card_ids 是「多卡引用为下次输入」的载体（AutoLink 的本质）
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE canvas_messages (
  id           CHAR(36)    NOT NULL,
  project_id   CHAR(36)    NOT NULL,
  role         ENUM('user','assistant','system') NOT NULL,
  content      TEXT        NOT NULL,
  -- 软引用，不加 FK：Skill 可被管理员删除，历史消息仍要能显示当时用的是哪个
  skill_id     CHAR(36)    NULL,
  ref_card_ids JSON        NULL,
  task_ids     JSON        NULL,
  created_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_canvas_messages_project_created (project_id, created_at),
  CONSTRAINT fk_canvas_messages_project FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：canvas —— 增量 op 日志。按序落库这串 op 就天然满足「创作过程可回放」，
-- 也是不做全量 PUT 的理由（几百张卡片每次几百 KB）。id 单调递增即 op 的全局顺序
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE canvas_ops (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  project_id    CHAR(36)    NOT NULL,
  -- 应用这批 op 之后的项目 revision
  revision      BIGINT      NOT NULL,
  ops           JSON        NOT NULL,
  actor_user_id CHAR(36)    NULL,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_canvas_ops_project_revision (project_id, revision),
  CONSTRAINT fk_canvas_ops_project FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

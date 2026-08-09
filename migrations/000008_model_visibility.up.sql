-- =============================================================================
-- 000008_model_visibility.up.sql
--
-- 给 models 加一个**用户端可见性**维度，并用它把 mock 驱动与 QA 夹具
-- 从用户能选到的模型目录里摘掉。
--
-- ── 为什么不是 enabled = 0 ──────────────────────────────────────────────
-- enabled 是「这条配置能不能用」，置 0 之后连管理端探测、QA 的失败注入、
-- 画布内部调用一起失效。而我们要的是「能用，但不摆进用户的模型下拉」：
--   · mock-image-1 / mock-video-1  —— 本地假驱动，出的是占位产物
--   · qa-schema-probe              —— 校验器的畸形 schema 夹具
--   · qa-fail-probe                —— 挂在不可达供应商上的失败注入夹具
--   · local-compose-1              —— 需要 >=2 个 composed_from 片段，
--                                      创作页的下拉根本喂不出这种输入，
--                                      它唯一合法的入口是画布的合成接口
-- 这些行必须继续可调用，否则 QA 就没有东西可测了。所以是**两个正交的维度**，
-- 不是一个开关的两种用法。
--
-- ── 兑现点只有一个 ──────────────────────────────────────────────────────
-- 过滤发生在 GET /api/models（handlers_models.go）那一处。分镜拆解与画布
-- 合成走的是不带 Visibility 的 List，因此摘掉夹具不会顺手把平台内部能力
-- 也摘掉。
--
-- ── CHECK 兜的是绕过服务端的那条路 ──────────────────────────────────────
-- 仓储层已经拦了「mock 族必须 internal」，这里再加一条同义的 CHECK，是因为
-- 迁移、seed 脚本、DBA 手改都能直接写库，而拿假图冒充作品的代价高到值得
-- 在两处各写一遍。
--
-- 幂等：本文件依赖 golang-migrate 的版本号保证只跑一次（ADD COLUMN /
-- ADD CONSTRAINT 在 MySQL 里没有 IF NOT EXISTS）。UPDATE 本身可重复执行。
-- =============================================================================

ALTER TABLE models
  ADD COLUMN visibility ENUM('public','internal') NOT NULL DEFAULT 'public'
    COMMENT '用户端目录可见性；与 enabled 正交，internal = 能调用但不摆进用户下拉'
    AFTER enabled;

-- ─────────────────────────────────────────────────────────────────────────────
-- 先把该藏的行改掉，再加 CHECK —— 反过来做会因为存量的 mock 行还是 public
-- 而直接被约束拒掉。
-- ─────────────────────────────────────────────────────────────────────────────
UPDATE models
   SET visibility = 'internal'
 WHERE protocol_family = 'mock'
    OR video_protocol  = 'mock'
    OR protocol_family = 'compose'
    OR id IN ('qa-fail-probe', 'qa-schema-probe');

-- 假驱动没有「要不要摆进用户目录」这个自由度：它出的是占位产物。
ALTER TABLE models
  ADD CONSTRAINT chk_models_mock_internal CHECK (
    (protocol_family <> 'mock' AND (video_protocol IS NULL OR video_protocol <> 'mock'))
    OR visibility = 'internal'
  );

-- ─────────────────────────────────────────────────────────────────────────────
-- 用户端列表的最热查询是 (visibility, enabled, modality) 三个等值条件 +
-- display_order 排序。把 visibility 放在最左，因为它的选择性最稳定
-- （用户端永远只查 public 这一个值）。原来那条 idx_models_modality_enabled_order
-- 留着不动：管理端与画布内部查找不带 visibility，仍然要走它。
-- ─────────────────────────────────────────────────────────────────────────────
CREATE INDEX idx_models_visibility_enabled_modality_order
  ON models (visibility, enabled, modality, display_order);

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'model visibility: hide mock drivers and QA fixtures from the user catalog');

-- =============================================================================
-- 000009_text_modality.up.sql
--
-- 订正六条 GPUGeek 对话模型的 modality：它们被标成了 'image'。
--
-- ── 为什么这是个必须修的错，而不是一个标签问题 ──────────────────────────
-- modality 决定前端把模型放进哪个 tab，也决定用户拿它去做什么。这六条是
-- 纯文本 / 视觉理解模型（qwen-flash、qwen3-max、gpt-5-2、qwen3-vl-*、
-- qwen-vl-max），走 OpenAI 兼容层的 /v1/chat/completions，产物是一段文字。
-- 标成 image 之后，它们出现在创作页的「图片」下拉里，用户选中、提交、
-- **任务变成 succeeded**，然后拿到一张裂图 —— 因为产物是文本，前端按图片渲染。
-- 这类错的代价是绿灯下的坏产物，比直接报错难发现得多。
--
-- 当时之所以标成 image，是因为 000001 的 modality 枚举里根本没有 'text'
-- 这个取值，写配置的人只能在 image / video 里挑一个。所以这条迁移先补枚举，
-- 再改数据 —— 顺序反了会被枚举拒。
--
-- ── 两份数据要一起改 ────────────────────────────────────────────────────
-- modality 在库里有两份：一份是 models.modality 列（为了能建索引），一份在
-- capability JSON 里（逐字下发前端）。列由 capability 派生，两份必须同时改，
-- 只改列会让前端仍然收到 "modality":"image"。
--
-- ── 顺带把它们移出用户目录 ──────────────────────────────────────────────
-- text 没有对应的 tab，改完 modality 它们就不会出现在图片/视频下拉里了。
-- 仍然显式置 visibility='internal'，是因为「不在任何 tab 里」是前端当下的
-- 渲染方式，而不是一条服务端约束：将来多一个 tab、或者某个页面直接拉全量
-- 目录，它们就又冒出来了。这六条是分镜拆解这类平台内部能力的载体，不是
-- 用户能选的产品，internal 才是对这件事的准确表达。
-- 分镜拆解按 protocol_family='chat' 挑模型，走的是不带 visibility 的查询，
-- 因此这一步不影响它。
--
-- 幂等：MODIFY COLUMN 与按条件的 UPDATE 都可重复执行。
-- =============================================================================

-- ─────────────────────────────────────────────────────────────────────────────
-- 枚举加一个取值。MySQL 的 ENUM 只能整列重写，老取值必须原样抄回来
-- —— 漏抄任何一个都会让已有的模型行在下次写入时被拒（同 000006 / 000007）。
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE models
  MODIFY COLUMN modality ENUM('image','video','text') NOT NULL;

UPDATE models
   SET modality   = 'text',
       capability = JSON_SET(capability, '$.modality', 'text'),
       visibility = 'internal'
 WHERE protocol_family = 'chat';

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'fix modality: chat models are text, not image');

-- =============================================================================
-- 000016_deepseek_text_models_public.down.sql
--
-- 把用户目录恢复成 000009 之后的样子：文本模型一律 internal，DeepSeek 两条
-- 撤掉。
--
-- ── 为什么 DeepSeek 是删行，而不是置 internal ────────────────────────────
-- 这两条是本迁移**新播**的，回滚的语义是「它们从来没存在过」。留成 internal
-- 会在下次 up 时被 ON DUPLICATE KEY UPDATE 撞上，看着无害，但它让「回滚到
-- 000014」和「从零跑到 000014」两条路径的库状态不一致，而那种不一致只会在
-- 很久以后以一条查不明白的差异暴露出来。
--
-- models 被 tasks.model_id 外键引用；真跑过任务的模型删不掉，DELETE 会报
-- 1451。这是对的行为，不在这里 try/catch 掩盖：真出片过就说明这条模型已经
-- 是历史数据的一部分，该由人来决定怎么处理，而不是让一条回滚把它连带清掉。
-- =============================================================================

UPDATE models
   SET visibility = 'internal'
 WHERE id IN ('gpugeek-qwen-flash', 'gpugeek-qwen3-max', 'gpugeek-gpt-5-2');

DELETE FROM models
 WHERE id IN ('gpugeek-deepseek-v4-pro', 'gpugeek-deepseek-v4-flash');

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback: drop deepseek models and hide text models again');

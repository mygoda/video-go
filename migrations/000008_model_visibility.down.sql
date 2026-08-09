-- =============================================================================
-- 000008_model_visibility.down.sql
--
-- 回滚顺序与 up 相反：先拆约束与索引，再删列。列一删，所有模型就重新
-- 无差别地出现在用户端目录里 —— 包括 mock 与 QA 夹具。这是回滚本身的
-- 含义，不是遗漏。
-- =============================================================================

ALTER TABLE models DROP CHECK chk_models_mock_internal;

DROP INDEX idx_models_visibility_enabled_modality_order ON models;

ALTER TABLE models DROP COLUMN visibility;

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback: model visibility column');

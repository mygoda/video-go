-- =============================================================================
-- 000017_task_step.down.sql
--
-- 回滚会让画布 chat / storyboard / refine 落的那些行重新变得"可重试"——
-- 来源这件事只存在于这一列，删掉它，管理端就再也分不出哪一行是同步链路落的。
-- =============================================================================

ALTER TABLE tasks DROP COLUMN step;

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback: tasks.step');

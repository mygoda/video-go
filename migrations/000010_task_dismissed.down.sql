-- =============================================================================
-- 000010_task_dismissed.down.sql
--
-- 回滚会让被移除的任务卡片重新出现在用户的任务流里。没有别的办法：
-- 「被移除过」这件事只存在于这一列。
-- =============================================================================

ALTER TABLE tasks DROP COLUMN dismissed_at;

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback: tasks.dismissed_at');

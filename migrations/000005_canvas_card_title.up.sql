-- =============================================================================
-- 000005_canvas_card_title.up.sql
--
-- 给 canvas_cards 补 title 列。
--
-- 为什么现在才加：前端契约（frontend/src/api/types.ts 的 CanvasCard）从第一版
-- 就声明了 title，新建卡片时也一直在发它，但后端 domain.Card 没有这个字段，
-- 于是 title 在 card.create 的 JSON 解码那一步被静默丢弃——用户改完标题一刷新
-- 就没了，而且没有任何报错可查。
--
-- 不改 000001：那份迁移已经在开发库上跑过，改已应用的迁移文件会让
-- schema_migrations 的版本号与库里的真实结构对不上，下一个人从零建库和
-- 从现有库升级会得到两种不同的表。
--
-- 允许为空且默认空串：历史卡片本来就没有标题，回填一个编造的标题
-- 会让"这张卡用户命名过吗"永远分不清。前端对空串有 cardTitle() 兜底。
--
-- 幂等：先查 information_schema 再决定要不要 ADD COLUMN，可重复执行。
-- MySQL 8.4 没有 ADD COLUMN IF NOT EXISTS，只能用这种预处理语句的写法。
-- =============================================================================

SET @ddl := (
  SELECT IF(
    COUNT(*) = 0,
    'ALTER TABLE canvas_cards ADD COLUMN title VARCHAR(255) NOT NULL DEFAULT '''' AFTER kind',
    'DO 0'
  )
  FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 'canvas_cards'
    AND COLUMN_NAME = 'title'
);

PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

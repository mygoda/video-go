-- =============================================================================
-- 000005_canvas_card_title.down.sql
--
-- 回滚 title 列。用户命名过的卡片标题会随之丢失，这是删列不可避免的代价。
-- =============================================================================

SET @ddl := (
  SELECT IF(
    COUNT(*) = 1,
    'ALTER TABLE canvas_cards DROP COLUMN title',
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

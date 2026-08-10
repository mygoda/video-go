-- =============================================================================
-- 000014_canvas_script_shot_cards.down.sql
--
-- 收回 script / shot 两个枚举值。
--
-- ── 为什么要先处理存量行 ──────────────────────────────────────────────────
-- ENUM 底层存的是序号。直接 MODIFY 把两个值摘掉，库里已有的 script / shot
-- 行会在严格模式下报 1265 让整条迁移失败，非严格模式下更糟——它们会被静默
-- 截成空串，变成一批 kind 既不是 text 也不是任何合法值的僵尸卡片。
--
-- ── 为什么是「软删 + 转 text」而不是硬删 ──────────────────────────────────
-- 硬删会让 canvas_ops 里引用这些卡片的历史 op 指向一个不存在的 id，创作过程
-- 的回放就断了——这正是 applyOp 的 card.delete 分支坚持软删的原因，回滚没有
-- 理由比正常删除更粗暴。转成 text 则让正文和 params 留在库里：万一是误回滚，
-- 用户的剧本和台词还找得回来。
--
-- 代价说清楚：回滚之后这些卡片在画布上不再出现，且再跑一次 up 也不会自动变
-- 回 script / shot——kind 已经被改写成 text，谁是剧本谁是镜头这件事没了。
-- 因此这条 down 只在确实要整体退回 000013 之前的代码时才该执行。
-- =============================================================================

UPDATE canvas_cards
   SET deleted_at = COALESCE(deleted_at, UTC_TIMESTAMP(3)),
       kind       = 'text'
 WHERE kind IN ('script', 'shot');

SET @ddl := (
  SELECT IF(
    LOCATE('''shot''', COLUMN_TYPE) > 0,
    'ALTER TABLE canvas_cards MODIFY COLUMN kind ENUM(''text'',''image'',''video'') NOT NULL',
    'DO 0'
  )
  FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 'canvas_cards'
    AND COLUMN_NAME = 'kind'
);

PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

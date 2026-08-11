-- =============================================================================
-- 000019_canvas_character_card.down.sql
--
-- 收回 character 枚举值。
--
-- ── 为什么要先处理存量行 ──────────────────────────────────────────────────
-- 同 000014：ENUM 底层存序号，直接把值摘掉，库里已有的 character 行在严格
-- 模式下报 1265 让整条迁移失败，非严格模式下被静默截成空串——变成一批 kind
-- 不合法的僵尸卡片。
--
-- ── 为什么是「软删 + 转 text」而不是硬删 ──────────────────────────────────
-- 同 000014：硬删会让 canvas_ops 里引用这些卡片的历史 op 指向不存在的 id，
-- 创作过程的回放就断了。转成 text 则让 params 里那份外观描述留在库里，
-- 万一是误回滚还找得回来。
--
-- 镜头卡的 params.character_ids 不动：它是一个白名单内的键，回滚之后指向的
-- 角色卡已经不是 character 了，前端 shotCharacters 查不到就当没选（那个函数
-- 本来就要容忍"角色卡被删了、id 还留着"）。把它清掉反而会把用户的选择抹平，
-- 而重新跑一次 up 也换不回来。
--
-- 代价说清楚：回滚之后角色卡在画布上不再出现，且再跑一次 up 也不会自动变回
-- character——kind 已被改写成 text。这条 down 只在确实要整体退回 000018 之前
-- 的代码时才该执行。
-- =============================================================================

UPDATE canvas_cards
   SET deleted_at = COALESCE(deleted_at, UTC_TIMESTAMP(3)),
       kind       = 'text'
 WHERE kind = 'character';

SET @ddl := (
  SELECT IF(
    LOCATE('''character''', COLUMN_TYPE) > 0,
    'ALTER TABLE canvas_cards MODIFY COLUMN kind ENUM(''text'',''image'',''video'',''script'',''shot'') NOT NULL',
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

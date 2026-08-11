-- =============================================================================
-- 000019_canvas_character_card.up.sql
--
-- canvas_cards.kind 增加 character 一个取值。
--
-- ── 为什么必须落在数据层 ──────────────────────────────────────────────────
-- 上游没有任何"参考图"入口，这是实测证否的（DEM-90/DEM-92）：出图模型收到
-- 图片参数会**静默丢弃**，出片模型的 images[] 只收 1~2 个字符串、语义是
-- 首帧/尾帧。于是跨镜头的角色一致性只剩文字这一条路——同一份结构化外观
-- （发型 / 衣着 / 体貌…）被逐字段拼进每一镜的首帧 prompt。
--
-- 那份外观必须有个地方长期住着：写在每一镜的描述里，用户改第三镜时必然
-- 漏掉某一项，而三张图不像时根本查不出是漏了哪一项。所以它是一张卡，
-- 不是一段文字。定妆图则是这张卡自己的产物，落在已有的 asset_id 列。
--
-- ── 为什么不新建表 ────────────────────────────────────────────────────────
-- 同 000014：结构化外观落已有的 params JSON 列（domain.CharacterParams 按
-- 白名单校验），血缘复用 refs（角色卡的 refs 指回它所属的 script 卡）。
--
-- ── 为什么是扩枚举 ────────────────────────────────────────────────────────
-- 同 000014 的判断，一字不改：ENUM 让「有哪些卡片类型」在库里可查，代价是
-- 每加一种卡片要来一次 ALTER。新值追加在末尾，不动前五个已有值的相对顺序
-- —— MySQL 的 ENUM 底层存序号，重排会让已入库的卡片集体串味。
--
-- 幂等：先查 information_schema 里的 COLUMN_TYPE，已经含 'character' 就不动。
-- MySQL 8.4 没有条件式 ALTER，只能用预处理语句这种写法（同 000005 / 000014）。
-- =============================================================================

SET @ddl := (
  SELECT IF(
    LOCATE('''character''', COLUMN_TYPE) = 0,
    'ALTER TABLE canvas_cards MODIFY COLUMN kind ENUM(''text'',''image'',''video'',''script'',''shot'',''character'') NOT NULL',
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

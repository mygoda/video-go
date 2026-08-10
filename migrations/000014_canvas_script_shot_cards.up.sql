-- =============================================================================
-- 000014_canvas_script_shot_cards.up.sql
--
-- canvas_cards.kind 增加 script / shot 两个取值。
--
-- ── 为什么必须落在数据层 ──────────────────────────────────────────────────
-- 在这之前「剧本」只是 canvas_messages 里的一条聊天记录，「镜头」在拆分完
-- 变成出片任务的 prompt 之后就不存在了。两者都不是实体，于是用户既看不见
-- 第 2 镜写的是什么，更没法在花钱出片之前改它。把这两种 kind 变成画布上真
-- 实的卡片，是「创作过程可见、可改、可重来」这件事唯一的地基。
--
-- ── 为什么不新建表 ────────────────────────────────────────────────────────
-- 镜头的结构化字段（镜号 / 镜头描述 / 台词 / 时长 / 机位 / 景别）全部落在
-- 已有的 params JSON 列，血缘复用已有的 refs 列（shot 卡的 refs 指回它的来源
-- script 卡）。新建 shots 表或边表都要再造一套增删改查、软删、revision 推进
-- 和回放，而画布的这一整套 canvas_cards 已经有了——多一张表换不来任何新能力，
-- 只换来两份必须同步演进的代码。
--
-- ── 为什么不改 000001 ─────────────────────────────────────────────────────
-- 000001 已经在开发库上跑过。改一份已应用的迁移，schema_migrations 的版本号
-- 与库里的真实结构就对不上了：从零建库的人和从现有库升级的人会得到两种不同
-- 的表，而且两边都认为自己是对的。
--
-- ── 为什么是扩枚举而不是换成 VARCHAR ──────────────────────────────────────
-- ENUM 让「有哪些卡片类型」这件事在库里是可查的（DESC canvas_cards 一眼看完），
-- 换成 VARCHAR 等于把这份约束整个交给应用层，写错一个 kind 要等到前端渲染
-- 不出来才发现。代价是每加一种卡片要来一次 ALTER——这个频率一年一两次，
-- 换得起。
--
-- 新值一律追加在末尾，不动 text / image / video 三个已有值的相对顺序：
-- MySQL 的 ENUM 在底层存的是序号，重排会让已入库的 51 行卡片集体串味。
--
-- 幂等：先查 information_schema 里的 COLUMN_TYPE，已经含 'shot' 就不动。
-- MySQL 8.4 没有条件式 ALTER，只能用预处理语句这种写法（同 000005）。
-- =============================================================================

SET @ddl := (
  SELECT IF(
    LOCATE('''shot''', COLUMN_TYPE) = 0,
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

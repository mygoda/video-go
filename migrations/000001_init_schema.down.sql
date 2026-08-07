-- =============================================================================
-- 000001_init_schema.down.sql
--
-- 完全反转 000001_init_schema.up.sql：按外键依赖的**逆序**逐表删除，
-- 保证不依赖 FOREIGN_KEY_CHECKS=0 也能干净回滚。
-- =============================================================================

DROP TABLE IF EXISTS canvas_ops;
DROP TABLE IF EXISTS canvas_messages;
DROP TABLE IF EXISTS canvas_cards;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS credit_ledger;
DROP TABLE IF EXISTS asset_lineage;
DROP TABLE IF EXISTS uploads;
DROP TABLE IF EXISTS assets;
DROP TABLE IF EXISTS task_events;
DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS skills;
DROP TABLE IF EXISTS config_versions;
DROP TABLE IF EXISTS models;
DROP TABLE IF EXISTS providers;
DROP TABLE IF EXISTS users;

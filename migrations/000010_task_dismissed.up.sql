-- =============================================================================
-- 000010_task_dismissed.up.sql
--
-- 补上 DELETE /api/tasks/{id} 这条路由的存储侧：任务的「从我的列表里移掉」。
--
-- ── 为什么不是真删 ──────────────────────────────────────────────────────
-- credit_ledger.task_id 指向 tasks，tasks.model_id 是 ON DELETE RESTRICT。
-- 真删一条任务，要么把它的记账流水一起删（账就对不上了——流水是 append-only
-- 的真相，余额只是它的物化缓存），要么让流水悬空。两条都不能接受。
--
-- 而用户按下那个「移除」按钮想要的也不是销毁审计记录，是**别再让这张卡
-- 占着我的任务流**。dismissed_at 正好表达这件事：任务还在、账还在、
-- 产物还在（资产库是另一个入口，删产物走 DELETE /api/assets/{id}），
-- 只是不再出现在 GET /api/tasks 里。
--
-- 管理端的任务列表不带这个过滤：运维要看的是全量，用户把卡片收起来
-- 不该让一条失败任务从故障排查视野里消失。
-- =============================================================================

ALTER TABLE tasks
  ADD COLUMN dismissed_at DATETIME(3) NULL
    COMMENT '用户把这条任务从自己的任务流里移掉的时刻；不影响记账与产物'
    AFTER finished_at;

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'tasks.dismissed_at: user-side dismiss for DELETE /api/tasks/{id}');

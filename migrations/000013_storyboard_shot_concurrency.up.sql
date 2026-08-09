-- =============================================================================
-- 000013_storyboard_shot_concurrency.up.sql
--
-- 分镜一次派 3 条出片任务，出片模型的 max_concurrent_per_user 得容得下它。
--
-- ── 为什么 2 不够 ─────────────────────────────────────────────────────────
-- 画布拆完分镜会为每个镜头起一条真任务（httpapi.submitShots），默认
-- storyboardDefaultShots = 3 条一起提交。而 CountRunningByModel 统计的是
-- status IN ('queued','running')，**排队中的也算**，所以上限 2 会让第 3 个
-- 镜头在提交时就被 rate_limited 顶回来——功能按自己的默认值必然半途失败。
--
-- ── 为什么放宽是安全的 ────────────────────────────────────────────────────
-- 这个上限管的是「同时挂在系统里的提交数」，不是「同时打到上游的请求数」。
-- 真正并发调上游的是 worker 池，由 AIGC_WORKER_CONCURRENCY 约束；多出来的
-- 任务只是排在队列里等 worker，对上游没有额外压力。
--
-- 取 4 而不是 3：留一个余量，用户在分镜跑着的时候还能自己手动出一条片，
-- 不会撞上"你已经有 3 个任务在跑"这种自己挡自己的路。
--
-- 只改 seedance-2-0-fast 一个：它是三个技能里唯一被分镜批量调用的模型。
-- 其余模型的上限是按各自上游的脾气定的，没有理由跟着动。
--
-- 幂等：JSON_SET 幂等，重复执行结果相同。
-- =============================================================================

UPDATE models
   SET capability = JSON_SET(capability, '$.limits.max_concurrent_per_user', 4)
 WHERE id = 'gpugeek-seedance-2-0-fast';

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'storyboard: raise seedance-fast per-user concurrency to fit a 3-shot batch');

-- =============================================================================
-- 000017_task_step.up.sql
--
-- 给 tasks 加一列 step：**这一行是哪条链路落的**。
--
-- ── 为什么需要这一列 ──────────────────────────────────────────────────────
-- 画布 chat / storyboard / refine 三条同步链路开始为计费落 task 行之后
-- （见 internal/httpapi/chatbill.go 的包注释），tasks 表里第一次出现了
-- **不由 executor 执行**的行：它们在 HTTP handler 里同步打完上游、把产物写进
-- canvas_cards，然后就地置终态，executor 从头到尾没碰过。
--
-- 而管理端的 POST /api/admin/tasks/{id}/retry 只判终态，不判来源。它把这种行
-- 打回 queued，worker 就会真的拿 prompt 去打一次上游——花的是真钱，产物却只能
-- 落成一个没人认领的 text 资产：那三条链路的产物落点是画布卡片，executor 的
-- 转存路径不认识画布。用户那张卡纹丝不动，账单上多一笔。
--
-- ── 为什么是显式一列，而不是靠现有字段推断 ────────────────────────────────
-- 看上去 canvas_id + card_id + modality='text' 能认出这三条链路，实际不能：
-- 普通的 POST /api/tasks 也接受 canvas_id 与 card_id（画布内发起、单卡重跑），
-- 而文本模型在 000016 之后是 public 的，用户自己就能提交。靠这三个字段判定
-- 等于把"画布内重跑一张图"误伤成不可重试。
--
-- 靠 prompt 内容猜更不行——tasks.prompt 里装的是用户自己那句话，
-- 与普通任务的 prompt 没有任何形态差别。
--
-- 来源是一个事实，不是一个可以推断的特征，所以它该被写下来。值取
-- internal/httpapi 里那三条链路早就在用的 step 名字（script / storyboard /
-- refine，见 chatCall.step），不新造一套词汇。
--
-- NULL 表示"走 executor 的正常任务"，也就是绝大多数行；这一列因此不建索引：
-- 它只在按主键取到单行之后被读一次（retry 的闸），从来不做为查询条件。
-- =============================================================================

ALTER TABLE tasks
  ADD COLUMN step VARCHAR(32) NULL
    COMMENT '画布同步链路的步骤名（script/storyboard/refine/legacy）；NULL 表示走 executor 的普通任务'
    AFTER card_id;

-- 回填本迁移之前落下的那批行。
--
-- 它们的具体步骤已不可考（三条链路当时没留下任何区分痕迹），统一标 legacy——
-- 闸只看这一列非空，legacy 与三个真实步骤在闸面前等价。
--
-- 过滤条件比运行时的判据严得多，因为这里只需要**认出历史数据**，不必对未来的
-- 行负责：文本模态 + 有画布归属 + 从未被 executor 碰过（没领过租约、没有上游
-- 引用、没有开始时刻）。同时满足这四条的只可能是那三条同步链路落的行——
-- executor 执行过的任何任务都至少会留下 started_at。
UPDATE tasks t
  JOIN models m ON m.id = t.model_id
   SET t.step = 'legacy'
 WHERE t.step IS NULL
   AND m.modality = 'text'
   AND t.canvas_id IS NOT NULL
   AND t.started_at IS NULL
   AND t.upstream_ref IS NULL
   AND t.lease_owner IS NULL;

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'tasks.step: mark canvas chat/storyboard/refine rows so admin retry can refuse them');

// Package executor is the L2 execution layer: the task state machine, the
// worker queue, the poll loop, retry policy and per-provider fault isolation.
//
// # 状态机
//
//	queued ──claim──> running ──┬──> succeeded
//	   │                        ├──> failed
//	   └────cancel──────────────┴──> canceled
//
// 五个状态就是 domain.TaskStatus 的全部取值，没有隐藏中间态。
// succeeded / failed / canceled 是终态，进入终态后不再被领取、不再轮询。
//
// 转移规则:
//   - queued → running    worker 成功 claim 到租约，且上游 Submit 返回后置位
//   - running → succeeded 上游归一化状态为 succeeded **且产物已完成转存**。
//     顺序不能反：先置 succeeded 再转存，中间进程挂掉就会留下一条
//     "成功但没有产物"的任务，而这个状态是不可自愈的。
//   - running → failed    上游失败，或重试耗尽，或产物转存失败
//   - queued/running → canceled 用户取消。queued 全额退回冻结额度；
//     running 尽力取消（上游支持就调上游取消，不支持就本地标记），**一律不扣费**
//
// # 租约式领取（lease-based claim）
//
// worker 不是从内存 channel 里取任务，而是**在数据库里抢一条租约**：
// UPDATE ... SET lease_owner=?, lease_expires_at=? WHERE status='queued' AND
// (lease_expires_at IS NULL OR lease_expires_at < NOW()) LIMIT 1。
//
// 这么做的理由是进程会崩。内存队列在进程崩溃时会把在途任务丢干净，
// 而租约在过期后自动回到可领取状态，另一个 worker 接着做——**任务不会因为
// 一次重启就永远卡在 running**。Runner 在长任务期间要周期性 Renew 租约，
// 证明自己还活着。
//
// # 轮询与 webhook：两条同源的幂等路径
//
// 视频任务的状态推进有两个触发源：PollScheduler 的定时轮询，以及上游打过来的
// webhook。它们**推进的是同一个状态机、走的是同一段代码**——webhook 的作用
// 只是"把下一次轮询提前到现在"，而不是另一条独立的状态更新逻辑。
//
// 因此两者都必须幂等：同一个上游状态被处理两次、乱序到达、或者 webhook 和
// 轮询同时到达，最终状态必须一致。实现上靠"状态转移带前置条件"保证
// （UPDATE ... WHERE status='running'），而不是靠加锁串行化。
//
// 本地开发没有公网地址，webhook 默认关闭，全靠轮询——这也正是两条路径必须
// 等价的现实理由：开发时走的是轮询那条，生产可能走 webhook 那条，
// 如果两条路径的逻辑不同，开发环境验证过的东西在生产就不算数。
//
// # 按供应商的故障隔离
//
// **一家供应商挂掉，不能让其他家的任务跟着停。** 三道闸门:
//
//  1. 并发闸门：每个 provider 有独立的 MaxConcurrency，
//     一家变慢时它的 worker 被自己的闸门挡住，不占别人的额度。
//  2. 熔断器：连续失败达阈值就把该 provider 的 breaker 打开，
//     后续任务直接快速失败并退款，不再耗时间等超时。半开态放少量探测流量。
//  3. 重试退避：RetryPolicy 带抖动，避免一家恢复的瞬间全部积压任务同时打过去
//     把它再打挂一次。
//
// 没有这三道，一家超时 30s 的供应商能在几分钟内把整个 worker 池占满，
// 表现为"所有模型都卡住了"——而实际只坏了一家。
package executor

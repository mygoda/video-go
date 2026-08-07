package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/stream"
)

// poller 是 PollScheduler 的对外门面。
//
// 它存在的唯一原因是方法名冲突：PollScheduler 声明 Stop(ctx, taskID)（停止轮询
// 某条任务），而 Service 作为一个后台设施要有 Stop(ctx)（停机）。同一个类型
// 挂不了两个 Stop，而两者都不该改名——前者是接口契约，后者是 cmd/aigcd 里
// 和 http.Server 并列的生命周期方法。用一个零成本的包装类型把两个 Stop
// 分到两个类型上，比给任何一个起个别扭的名字都干净。
type poller struct{ s *Service }

func (p poller) Schedule(ctx context.Context, taskID string, interval time.Duration) error {
	return p.s.Schedule(ctx, taskID, interval)
}

func (p poller) Advance(ctx context.Context, taskID string, upstreamStatus string) error {
	return p.s.Advance(ctx, taskID, upstreamStatus)
}

func (p poller) Stop(ctx context.Context, taskID string) error {
	p.s.stopPolling(taskID)
	return nil
}

// Schedule 登记一条已提交到上游的任务，排定它的下一次轮询时刻。
//
// 真相写在 tasks.next_poll_at 而不是进程内的定时器：进程崩了之后，
// 别的实例靠 ClaimPoll 扫这张表就能把任务接管过去继续轮询。进程内的
// intervals 只是缓存，省掉每次重算间隔时对模型配置的一次读库。
func (s *Service) Schedule(ctx context.Context, taskID string, interval time.Duration) error {
	if taskID == "" {
		return fmt.Errorf("executor: Schedule 缺少 task_id")
	}
	if interval <= 0 {
		interval = s.deps.DefaultPollInterval
	}
	s.intervals.Store(taskID, interval)
	if err := s.deps.Tasks.SetNextPoll(ctx, taskID, s.now().Add(interval).UTC()); err != nil {
		return fmt.Errorf("executor: 排定轮询时刻: %w", err)
	}
	return nil
}

// Advance 用一次上游状态推进状态机。**它是幂等的**，也是定时轮询与 webhook
// 共用的那一段代码。
//
// # 为什么不信 upstreamStatus，而是重新问一次上游
//
// 参数里的状态只是一个**提示**：webhook 会乱序到达（上游先发 running 后发
// succeeded，网络把顺序颠倒过来是常态），而且它不带产物地址。若照着这个字符串
// 改状态，一条迟到的 running 就能把已经 succeeded 的任务打回去——那是这个
// 状态机唯一绝对不能发生的事。因此 Advance 只把它当作"该去看一眼了"的信号，
// 权威结果一律以 driver.Poll 的返回为准。重复调用、乱序调用因此天然收敛到
// 同一个终态。
//
// 三道幂等闸门叠在一起：
//
//  1. 终态任务直接返回（终态不可逆，这是硬规则）；
//  2. 同一条任务同时只允许一次 Advance 在跑（进程内），后来者直接返回——
//     它想知道的结果正在被前一个求出来；
//  3. 所有状态迁移走 UpdateStatus 的 CAS，跨进程的并发由数据库仲裁。
func (s *Service) Advance(ctx context.Context, taskID string, upstreamStatus string) error {
	if taskID == "" {
		return fmt.Errorf("executor: Advance 缺少 task_id")
	}

	if _, busy := s.advancing.LoadOrStore(taskID, struct{}{}); busy {
		// 已经有一次推进在跑，它算出来的就是本次想要的结果。
		return nil
	}
	defer s.advancing.Delete(taskID)

	task, err := s.deps.Tasks.Get(ctx, taskID)
	if err != nil {
		return err
	}
	if task.Status.IsTerminal() {
		// 终态不可逆。乱序到达的旧回调、重复投递的 webhook 都在这里止步。
		s.stopPolling(taskID)
		return nil
	}
	if task.UpstreamRef == nil || *task.UpstreamRef == "" {
		// 还没提交到上游就收到了回调，无从查起。等提交路径把 upstream_ref
		// 写下来之后，下一次定时轮询会正常推进。
		s.log.Info("任务尚无上游引用，跳过本次推进",
			"task_id", taskID, "upstream_status", upstreamStatus)
		return nil
	}

	return s.advance(ctx, task)
}

// pollLoop 是轮询调度循环：按节奏扫出到点该轮询的任务并逐一推进。
//
// 节奏（PollTick）不是轮询间隔——真正的间隔由每条任务的 next_poll_at 决定，
// 这里只决定"到点之后最多晚多久被发现"。两者分开之后，一条 60s 轮询一次的
// 视频任务和一条 5s 轮询一次的图片任务可以共用同一个扫描循环。
func (s *Service) pollLoop(ctx context.Context) {
	t := time.NewTicker(s.deps.PollTick)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		leases, err := s.deps.Queue.ClaimPoll(ctx, s.owner, s.deps.LeaseTTL, s.deps.PollBatch)
		if err != nil {
			// 与 submitLoop 同理：停机导致的取消不是故障，不该记成 ERROR。
			if ctx.Err() != nil {
				return
			}
			s.log.Error("领取待轮询任务失败", "err", err)
			continue
		}
		for _, lease := range leases {
			s.trackLease(lease.Task.ID)
			// 每条任务一个 goroutine：一条命中 succeeded 的任务要在这里
			// 完成几十上百 MB 的转存，串行处理会让同批次里其余任务的
			// 下一次轮询被这一条拖到几分钟之后。
			s.wg.Add(1)
			go func(l Lease) {
				defer s.wg.Done()
				if err := s.pollOne(ctx, l); err != nil {
					s.log.Error("推进任务失败", "task_id", l.Task.ID, "err", err)
				}
			}(lease)
		}
	}
}

// pollOne 处理一条从队列里领到的待轮询任务。
func (s *Service) pollOne(ctx context.Context, lease Lease) error {
	task := lease.Task
	if task.Status.IsTerminal() {
		s.stopPolling(task.ID)
		s.release(ctx, task.ID)
		return nil
	}
	if task.UpstreamRef == nil || *task.UpstreamRef == "" {
		// 领到一条没有上游引用的 running 任务：它多半是提交途中被接管的。
		// 归还租约让提交路径继续，不在这里判死。
		s.release(ctx, task.ID)
		return nil
	}

	if _, busy := s.advancing.LoadOrStore(task.ID, struct{}{}); busy {
		s.release(ctx, task.ID)
		return nil
	}
	defer s.advancing.Delete(task.ID)

	// 轮询命中成功时要在这一段里完成转存，几分钟起步，全程续租。
	stopRenew := s.keepAlive(ctx, task.ID)
	defer stopRenew()

	return s.advance(ctx, task)
}

// advance 是真正推进一条任务的地方：查一次上游，按结果落状态。
//
// 调用方负责持有 advancing 闸门（进程内互斥）与租约；本函数只管状态迁移。
func (s *Service) advance(ctx context.Context, task domain.Task) error {
	model, provider, err := s.config(ctx, task)
	if err != nil {
		return s.fail(ctx, task, task.Status, internalError("模型或供应商配置不可用", err))
	}

	name := driverName(model)
	drv, ok := s.deps.Drivers.Async(name)
	if !ok {
		// 同步族不该走到轮询：它一次往返就该有结果。走到这里说明配置被改过
		// （family 从 video 改成了 chat），任务已经没人能推进了，判失败退款
		// 比让它永远挂在 running 上好。
		return s.fail(ctx, task, task.Status,
			internalError(fmt.Sprintf("协议 %q 不支持轮询", name), nil))
	}

	req := s.pollRequest(task, model, provider)
	cctx := ctx
	if provider.TimeoutMS > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, time.Duration(provider.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	res, err := drv.Poll(cctx, req)
	if err != nil {
		return s.pollFailed(ctx, task, model, provider, err)
	}
	_ = s.breakers.Success(ctx, provider.ID)

	if res.RawStatus != "" {
		if err := s.deps.Tasks.SetUpstreamRef(ctx, task.ID, req.UpstreamRef, res.RawStatus); err != nil {
			s.log.Warn("记录上游原始状态失败", "task_id", task.ID, "err", err)
		}
	}

	// webhook 可能赶在提交路径置 running 之前就把我们带到这里。先补上那一步，
	// 后续所有 CAS 才有一致的前置状态可用。
	if task.Status == domain.TaskStatusQueued {
		if err := s.deps.Tasks.UpdateStatus(ctx, task.ID, domain.TaskStatusQueued, domain.TaskStatusRunning, nil); err != nil {
			s.log.Info("置 running 未生效，任务已被其他路径推进", "task_id", task.ID, "err", err)
			s.release(ctx, task.ID)
			return nil
		}
		task.Status = domain.TaskStatusRunning
	}

	switch res.Status {
	case adapter.StatusSucceeded:
		if len(res.Artifacts) == 0 {
			return s.fail(ctx, task, domain.TaskStatusRunning,
				internalError("上游报告成功但没有返回任何产物", nil))
		}
		return s.finish(ctx, task, res.Artifacts, req, s.recallInputs(ctx, task))

	case adapter.StatusFailed:
		terr := res.Err
		if terr == nil {
			terr = internalError("上游返回失败但未给出原因", nil)
		}
		terr.Charged = false
		if IsRetryable(terr.Code) {
			_ = s.breakers.Failure(ctx, provider.ID)
		}
		return s.fail(ctx, task, domain.TaskStatusRunning, terr)

	case adapter.StatusCanceled:
		return s.cancelTask(ctx, task, domain.TaskStatusRunning)

	default:
		return s.reschedule(ctx, task, model, req, res)
	}
}

// reschedule 更新进度并排定下一次轮询。任务仍在上游跑着。
func (s *Service) reschedule(ctx context.Context, task domain.Task, model domain.ModelConfig, req adapter.PollRequest, res adapter.PollResult) error {
	if res.Progress != nil || res.QueuePosition != nil || res.ETASeconds != nil {
		if err := s.deps.Tasks.UpdateProgress(ctx, task.ID, res.Progress, res.QueuePosition, res.ETASeconds); err != nil {
			s.log.Warn("更新进度失败", "task_id", task.ID, "err", err)
		}
		task.Progress = res.Progress
		task.QueuePosition = res.QueuePosition
		task.ETASeconds = res.ETASeconds
	}
	task.Status = domain.TaskStatusRunning
	s.publish(ctx, stream.TaskUpdated(task.UserID, task))

	if err := s.Schedule(ctx, task.ID, s.pollInterval(model, req)); err != nil {
		s.log.Error("排定下一次轮询失败，任务将等租约过期后被重新领取",
			"task_id", task.ID, "err", err)
	}
	// 租约立刻归还：下一次轮询由 ClaimPoll 重新领。攥着它只会让这条任务
	// 在两次轮询之间的几十秒里，白白占住一个别人本可以用的租约。
	s.release(ctx, task.ID)
	return nil
}

// pollFailed 处理"查状态时出错"，与"上游明确报告失败"是两回事。
//
// 前者极可能只是网络抖了一下，任务本身好好地在上游跑着；就此判死等于扔掉
// 一次已经花掉的真实生成。因此这里按 PollRetry（比提交更密集、更有耐心）
// 退避重排下一次轮询，只有把次数用尽才判失败。
func (s *Service) pollFailed(ctx context.Context, task domain.Task, model domain.ModelConfig, provider domain.Provider, cause error) error {
	terr := classify(cause)
	if IsRetryable(terr.Code) {
		_ = s.breakers.Failure(ctx, provider.ID)
	} else {
		_ = s.breakers.Success(ctx, provider.ID)
	}

	policy := s.deps.PollRetry
	attempt, err := s.deps.Tasks.IncrementAttempt(ctx, task.ID)
	if err != nil {
		s.log.Warn("自增轮询计数失败", "task_id", task.ID, "err", err)
		attempt = task.Attempt + 1
	}

	if !policy.ShouldRetry(attempt, terr) {
		terr.Charged = false
		return s.fail(ctx, task, task.Status, terr)
	}

	delay := policy.Delay(attempt)
	next := s.now().Add(delay)
	s.log.Info("轮询失败，稍后重试",
		"task_id", task.ID, "attempt", attempt, "max", policy.MaxAttempts,
		"delay", delay, "code", terr.Code, "err", cause)

	task.Status = domain.TaskStatusRunning
	task.Attempt = attempt
	s.publish(ctx, stream.TaskUpdated(task.UserID, task))

	if err := s.deps.Tasks.SetNextPoll(ctx, task.ID, next.UTC()); err != nil {
		s.log.Error("排定重试轮询失败", "task_id", task.ID, "err", err)
	}
	s.release(ctx, task.ID)
	return nil
}

// pollInterval 决定两次轮询之间等多久。
//
// 优先级是 模型配置 → 驱动建议 → 全局兜底。模型配置排第一，是因为同一个协议
// 下不同模型的生成时长能差一个量级（5s 短视频 vs 分钟级长视频），
// 用同一个间隔要么把快的拖慢，要么为慢的白打几十次请求。
func (s *Service) pollInterval(m domain.ModelConfig, _ adapter.PollRequest) time.Duration {
	if m.PollIntervalSeconds != nil && *m.PollIntervalSeconds > 0 {
		return time.Duration(*m.PollIntervalSeconds) * time.Second
	}
	if drv, ok := s.deps.Drivers.Async(driverName(m)); ok {
		if d := drv.DefaultPollInterval(); d > 0 {
			return d
		}
	}
	return s.deps.DefaultPollInterval
}

// stopPolling 清掉本进程为某条任务保留的轮询缓存。
//
// 它不去改 next_poll_at：ClaimPoll 只捞 running 的任务，而调用 stopPolling 时
// 任务已经是终态了，那一列的值再也不会被读到。多打一次库换不来任何东西。
func (s *Service) stopPolling(taskID string) {
	s.intervals.Delete(taskID)
	s.inputs.Delete(taskID)
}

// rememberInputs 把本次生成用到的输入资产 id 缓存起来，供成功时写血缘。
//
// 只是缓存：轮询完全可能落在另一个进程上，那时 recallInputs 会从任务的
// inputs 字段重新解析。
func (s *Service) rememberInputs(taskID string, ids []string) {
	if len(ids) == 0 {
		return
	}
	s.inputs.Store(taskID, ids)
}

// recallInputs 取回本次生成的输入资产 id。
//
// 缓存未命中时（进程重启、任务被别的实例接管）从 task.Inputs 重新解析。
// 这里**只读不提升**：upload 在提交路径已经被提升过了，此刻再提升一次
// 会凭空多出一份重复资产。解析不出来就少写几条血缘边——血缘缺一条边
// 是可惜，但为了它去动资产表是本末倒置。
func (s *Service) recallInputs(ctx context.Context, task domain.Task) []string {
	if v, ok := s.inputs.Load(task.ID); ok {
		if ids, ok := v.([]string); ok {
			return ids
		}
	}

	ids := make([]string, 0, len(task.Inputs))
	seen := make(map[string]struct{})
	add := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	for slot, values := range task.Inputs {
		if slot == "rerun_of" {
			continue
		}
		for _, raw := range values {
			if raw == "" {
				continue
			}
			if s.deps.Assets != nil {
				if a, err := s.deps.Assets.Get(ctx, raw); err == nil {
					add(a.ID)
					continue
				}
			}
			if s.deps.Uploads != nil {
				if u, err := s.deps.Uploads.Get(ctx, raw); err == nil && u.AssetID != nil {
					add(*u.AssetID)
				}
			}
		}
	}
	return ids
}

// 保证接线出错时在编译期就炸而不是运行时。
var (
	_ PollScheduler = poller{}
	_ Runner        = (*Service)(nil)
)

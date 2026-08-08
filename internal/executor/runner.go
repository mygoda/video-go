package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/asset"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/stream"
)

// submitLoop 是提交 worker 的主循环：抢一条 queued 任务的租约并跑完它。
//
// 没有可领的任务时退避一小会儿再问，而不是空转打库。用固定间隔而不是
// 指数退避：队列空是常态不是故障，而新任务提交后被发现的延迟直接就是
// 用户点下生成到卡片开始转圈的那段等待。
func (s *Service) submitLoop(ctx context.Context) {
	idle := time.NewTimer(0)
	defer idle.Stop()
	if !idle.Stop() {
		<-idle.C
	}

	for {
		if ctx.Err() != nil {
			return
		}

		lease, ok, err := s.claim(ctx)
		switch {
		case err != nil:
			// 停机时在途的那次查询会因 ctx 取消而失败，那不是故障。
			// 按 ERROR 记会让每次正常发布都在日志里留下一排假告警，
			// 久了就没人再看这条日志了。
			if ctx.Err() != nil {
				return
			}
			s.log.Error("领取任务失败", "err", err)
		case ok:
			if err := s.Run(ctx, lease); err != nil {
				s.log.Error("执行任务失败", "task_id", lease.Task.ID, "err", err)
			}
			continue
		}

		idle.Reset(s.deps.PollTick)
		select {
		case <-ctx.Done():
			return
		case <-idle.C:
		}
	}
}

// claim 领取一条任务，只从当前没被熔断的供应商里领。
//
// **过滤在领取处而不是执行处**，这是熔断真正省下资源的地方：被熔断那家的
// 任务留在队列里等恢复，而不是被一条条领出来快速失败——后者在用户眼里
// 是"我的任务一提交就全挂了"，前者只是"有点慢"。
func (s *Service) claim(ctx context.Context) (Lease, bool, error) {
	providers, err := s.deps.Providers.List(ctx)
	if err != nil {
		return Lease{}, false, fmt.Errorf("executor: 读取供应商列表: %w", err)
	}
	candidates := make([]string, 0, len(providers))
	for _, p := range providers {
		if p.Enabled {
			candidates = append(candidates, p.ID)
		}
	}
	if len(candidates) == 0 {
		return Lease{}, false, nil
	}

	allowed, err := AvailableProviders(ctx, s.breakers, candidates)
	if err != nil {
		return Lease{}, false, fmt.Errorf("executor: 过滤熔断供应商: %w", err)
	}
	if len(allowed) == 0 {
		return Lease{}, false, nil
	}

	lease, ok, err := s.deps.Queue.Claim(ctx, s.owner, s.deps.LeaseTTL, allowed)
	if err != nil {
		return Lease{}, false, err
	}
	if ok {
		s.trackLease(lease.Task.ID)
	}
	return lease, ok, nil
}

// Run 执行一条已领取的任务，覆盖 queued → 终态 / 交接给轮询的完整过程。
//
// 返回前任务必然处于两种情形之一：已进终态，或已登记进轮询调度。
// 「既不是终态又没人管」的第三种情形是不允许出现的——那条任务会一直
// 占着 running 直到租约过期，然后被另一个 worker 重新提交给上游，
// 于是同一次生成被付费两次。
func (s *Service) Run(ctx context.Context, lease Lease) error {
	task := lease.Task
	if task.ID == "" {
		return fmt.Errorf("executor: 租约里没有任务")
	}
	if task.Status.IsTerminal() {
		// 领到一条已经终结的任务只可能是租约与状态更新交错的结果，
		// 归还租约即可，不是错误。
		s.release(ctx, task.ID)
		return nil
	}

	stopRenew := s.keepAlive(ctx, task.ID)
	defer stopRenew()

	model, provider, err := s.config(ctx, task)
	if err != nil {
		// 配置读不到（模型被删、供应商被禁）是我们这边的问题，不扣费。
		return s.fail(ctx, task, task.Status, internalError("模型或供应商配置不可用", err))
	}

	allowed, err := s.breakers.Allow(ctx, provider.ID)
	if err != nil {
		return s.fail(ctx, task, task.Status, internalError("熔断器状态不可读", err))
	}
	if !allowed {
		// 快速失败并全额退款，比让用户等一个必然超时的调用体验好，
		// 也不占用 worker 名额。任务可由管理端 Requeue 或用户重提。
		return s.fail(ctx, task, task.Status, &domain.TaskError{
			Code:      domain.TaskErrorUpstreamRateLimited,
			Message:   "该供应商暂时不可用，请稍后重试",
			Retryable: true,
			Charged:   false,
		})
	}

	inputs, inputAssetIDs, err := s.resolveInputs(ctx, task)
	if err != nil {
		var derr *domain.Error
		if errors.As(err, &derr) && derr.Code == domain.CodeNotFound {
			return s.fail(ctx, task, task.Status, &domain.TaskError{
				Code:      domain.TaskErrorInvalidParam,
				Message:   "输入素材不存在或已过期",
				Retryable: true,
				Charged:   false,
			})
		}
		return s.fail(ctx, task, task.Status, internalError("解析输入素材失败", err))
	}

	in := adapter.SubmitInput{
		TaskID:         task.ID,
		Provider:       provider,
		Model:          model,
		UpstreamModel:  model.UpstreamModel,
		Prompt:         task.Prompt,
		Params:         task.Params,
		Inputs:         inputs,
		Credential:     s.deps.Credential(provider.CredentialRef),
		IdempotencyKey: task.ID,
	}
	req := s.pollRequest(task, model, provider)

	// 置 running 发生在**发起上游调用之前**，理由有两条:
	//  1. 重试期间任务停留在 running（见 retry.go），从 queued 里重试会让
	//     用户看到状态来回跳；
	//  2. webhook 可能在 Submit 的响应还没回到我们手上时就到了，而它推进
	//     状态机靠的是 `WHERE status='running'`。先置位，那条回调才有落点。
	if task.Status == domain.TaskStatusQueued {
		if err := s.deps.Tasks.UpdateStatus(ctx, task.ID, domain.TaskStatusQueued, domain.TaskStatusRunning, nil); err != nil {
			// CAS 输了说明有人（取消）先动了手，让它去，不是错误。
			s.log.Info("置 running 未生效，任务已被其他路径推进", "task_id", task.ID, "err", err)
			s.release(ctx, task.ID)
			return nil
		}
		task.Status = domain.TaskStatusRunning
		s.publish(ctx, stream.TaskUpdated(task.UserID, task))
	}

	res, terr := s.submitWithRetry(ctx, task, model, in, req)
	if terr != nil {
		return s.fail(ctx, task, domain.TaskStatusRunning, terr)
	}

	if err := s.deps.Tasks.SetUpstreamRef(ctx, task.ID, res.UpstreamRef, res.RawStatus); err != nil {
		s.log.Warn("记录上游引用失败", "task_id", task.ID, "err", err)
	}
	if res.QueuePosition != nil || res.ETASeconds != nil {
		if err := s.deps.Tasks.UpdateProgress(ctx, task.ID, nil, res.QueuePosition, res.ETASeconds); err != nil {
			s.log.Warn("更新排队信息失败", "task_id", task.ID, "err", err)
		}
		task.QueuePosition = res.QueuePosition
		task.ETASeconds = res.ETASeconds
		s.publish(ctx, stream.TaskUpdated(task.UserID, task))
	}

	switch {
	case res.Status == adapter.StatusFailed:
		// SubmitResult 不带 Err：提交失败的正常表达是 invoke 返回 error
		// （已由 submitWithRetry 归类）。走到这里说明驱动的 HTTP 调用成功了，
		// 但响应体里写着 failed——上游没给我们可归类的东西。
		return s.fail(ctx, task, domain.TaskStatusRunning,
			internalError("上游在提交阶段即返回失败："+res.RawStatus, nil))

	case res.Status == adapter.StatusCanceled:
		return s.cancelTask(ctx, task, domain.TaskStatusRunning)

	case len(res.Inline) > 0:
		// 同步族（chat / images）一次往返就拿到产物，直接走收尾。
		req.UpstreamRef = res.UpstreamRef
		return s.finish(ctx, task, res.Inline, req, inputAssetIDs)

	case res.UpstreamRef != "":
		// 异步族交接给轮询调度。这是 Run 的第二种正当出口：
		// 任务不是终态，但有人管着。
		req.UpstreamRef = res.UpstreamRef
		s.rememberInputs(task.ID, inputAssetIDs)
		if err := s.Schedule(ctx, task.ID, s.pollInterval(model, req)); err != nil {
			return s.fail(ctx, task, domain.TaskStatusRunning, internalError("登记轮询失败", err))
		}
		// 租约还给队列：轮询由 ClaimPoll 那条路重新领取，两个池子各扫各的索引，
		// 不该让一条几分钟的视频任务一直占着提交 worker。
		s.release(ctx, task.ID)
		return nil

	default:
		return s.fail(ctx, task, domain.TaskStatusRunning,
			internalError("上游既没有返回产物也没有返回任务引用", nil))
	}
}

// submitWithRetry 按 SubmitRetry 退避重试地把任务提交给上游。
//
// 只有可重试的失败分类才会再来一次（见 IsRetryable）：参数错了重试一万次
// 还是错，审核拒了重试也还是拒，而每一次重试都可能是一次真实计费调用。
//
// 重试期间任务停在 running、冻结额度不释放：中途退回再冻结会在余额已被
// 别的任务占满时，把一条本来能成功的任务卡死在一个它本可以通过的地方。
func (s *Service) submitWithRetry(ctx context.Context, task domain.Task, model domain.ModelConfig, in adapter.SubmitInput, req adapter.PollRequest) (adapter.SubmitResult, *domain.TaskError) {
	policy := s.deps.SubmitRetry
	attempt := task.Attempt
	if attempt < 1 {
		attempt = 1
	}

	for {
		res, err := s.invoke(ctx, model, in)
		if err == nil {
			_ = s.breakers.Success(ctx, in.Provider.ID)
			return res, nil
		}

		terr := classify(err)
		if IsRetryable(terr.Code) {
			_ = s.breakers.Failure(ctx, in.Provider.ID)
		} else {
			// 参数错、审核拒是这一条请求的问题，不是供应商坏了。
			// 把它们计入熔断会让一批参数写错的任务把好好的供应商熔掉。
			_ = s.breakers.Success(ctx, in.Provider.ID)
		}

		if !policy.ShouldRetry(attempt, terr) {
			terr.Charged = false
			if IsRetryable(terr.Code) {
				terr.Retry = RetryInfoFor(policy, attempt, time.Time{})
			}
			return adapter.SubmitResult{}, terr
		}

		next, incErr := s.deps.Tasks.IncrementAttempt(ctx, task.ID)
		// 本地计数是这个退避循环的真相，库里那一列只用于展示与跨进程续接。
		// 库值不前进时（自增失败，或进程重启后 attempt 列从比本地更小的值起算）
		// 必须以本地 +1 为准——否则这个循环不收敛，一条限流的任务会被
		// 无限次提交给上游，而每一次都可能是真实计费调用。
		if incErr != nil {
			s.log.Warn("自增重试计数失败", "task_id", task.ID, "err", incErr)
		}
		if incErr != nil || next <= attempt {
			next = attempt + 1
		}
		attempt = next

		delay := policy.Delay(attempt - 1)
		task.Status = domain.TaskStatusRunning
		task.Attempt = attempt
		s.publish(ctx, stream.TaskUpdated(task.UserID, task))
		s.log.Info("提交失败，准备重试",
			"task_id", task.ID, "attempt", attempt, "max", policy.MaxAttempts,
			"delay", delay, "code", terr.Code, "err", err)

		if !sleepCtx(ctx, delay) {
			terr := internalError("任务在重试等待中被中断", ctx.Err())
			return adapter.SubmitResult{}, terr
		}
	}
}

// invoke 按协议族把任务交给对应的驱动。
//
// **这是 L2 里唯一一处按协议分发的地方**，而且它只在 sync / async 两条路
// 之间选，不认识任何具体供应商——具体驱动名是从 ModelConfig 里读出来的
// 一个字符串，Registry 负责把它变成实现。
func (s *Service) invoke(ctx context.Context, model domain.ModelConfig, in adapter.SubmitInput) (adapter.SubmitResult, error) {
	name := driverName(model)
	if name == "" {
		return adapter.SubmitResult{}, fmt.Errorf("模型 %s 没有可用的协议名", model.ID)
	}

	cctx := ctx
	if in.Provider.TimeoutMS > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, time.Duration(in.Provider.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	// 同步还是异步交给 adapter.IsAsync 判：同一个驱动名可能同时出现在两张
	// 表里（mock 即如此），无脑先查异步会把图像任务送进产视频的驱动。
	if adapter.IsAsync(s.deps.Drivers, model) {
		async, _ := s.deps.Drivers.Async(name)
		return async.Submit(cctx, in)
	}
	if sync, ok := s.deps.Drivers.Sync(name); ok {
		return sync.Invoke(cctx, in)
	}
	return adapter.SubmitResult{}, fmt.Errorf("驱动 %q 未注册", name)
}
// finish 是任务成功的收尾：转存 → 派生 → 血缘 → 置 succeeded → 结算 → 推事件。
//
// # 顺序不能反
//
// 产物必须在任务被置为 succeeded **之前**完整落到本平台存储。上游 URL 24h
// 就过期，一条写着 succeeded 却没有本地字节的记录是**不可自愈的**：那时
// 上游已经不认那个 URL，我们手上没有任何字节可以补救，用户第二天回来看到的
// 是一屏打不开的卡片。宁可让他重跑一次（退款、重新生成），也不能留下这种记录。
//
// 派生（缩略图 / 封面帧）在转存之后、置 succeeded 之前跑，但**允许失败**：
// 原始字节已经安全了，缺张缩略图只是前端回退到原图，而任务失败要退款、
// 要让用户重跑一次真实生成——代价完全不对等。
func (s *Service) finish(ctx context.Context, task domain.Task, refs []adapter.ArtifactRef, req adapter.PollRequest, inputAssetIDs []string) error {
	if s.deps.Transferor == nil {
		return s.fail(ctx, task, domain.TaskStatusRunning, internalError("未配置转存器，产物无法落地", nil))
	}

	assets, err := s.deps.Transferor.TransferAll(ctx, task.UserID, task.ID, refs, req)
	if err != nil {
		// 转存失败必须让任务失败并退款，而不是"标记成功、产物之后再补"——
		// 之后就没有之后了，URL 已经过期。
		s.log.Error("转存产物失败，任务判失败", "task_id", task.ID, "err", err)
		return s.fail(ctx, task, domain.TaskStatusRunning, &domain.TaskError{
			Code:      domain.TaskErrorInternal,
			Message:   "产物转存失败，已全额退还冻结积分，请重试",
			Retryable: true,
			Charged:   false,
		})
	}

	s.derive(ctx, assets)
	s.recordLineage(ctx, task, inputAssetIDs, assets)

	// 到这里产物才算真的在我们手上，现在才允许置终态。
	if err := s.deps.Tasks.UpdateStatus(ctx, task.ID, domain.TaskStatusRunning, domain.TaskStatusSucceeded, nil); err != nil {
		// CAS 输了：另一条路（取消、并发的轮询）已经把它推进过了。
		// 资产已经落库并挂着 task_id，不回滚——它们是真实产物，
		// 用户在资产库里看得到，删掉才是数据丢失。
		s.log.Info("置 succeeded 未生效，任务已被其他路径推进", "task_id", task.ID, "err", err)
		s.release(ctx, task.ID)
		return nil
	}

	actual := task.EstimatedCost
	if err := s.deps.Tasks.SetActualCost(ctx, task.ID, actual); err != nil {
		s.log.Warn("记录实扣额度失败", "task_id", task.ID, "err", err)
	}
	if s.deps.Ledger != nil {
		if _, err := s.deps.Ledger.Charge(context.WithoutCancel(ctx), task.UserID, task.ID, actual); err != nil {
			// 结算失败不回滚 succeeded：产物已经交付给用户了，把一次成功的
			// 生成改判失败会让他白白丢掉产物。少收一笔的账由流水对账修复，
			// 那是可查可补的；已交付的产物被撤销是不可逆的。
			s.log.Error("结算积分失败，需人工对账", "task_id", task.ID, "user_id", task.UserID, "amount", actual, "err", err)
		}
	}

	s.stopPolling(task.ID)
	s.release(ctx, task.ID)

	// 事件里的资产必须带 URL：StorageKey 这类内部键对前端毫无意义，而
	// task.succeeded 正是前端把占位卡片换成真实产物的那一刻。投影复用
	// asset.DecorateURLs——REST 响应走的是同一个函数，两条路才不会分叉。
	s.publish(ctx, stream.TaskSucceeded(task.UserID, task.ID, asset.DecorateURLs(s.deps.PublicBaseURL, assets), actual))
	s.publishCredit(ctx, task.UserID)
	if len(assets) > 0 {
		id := assets[0].ID
		s.publishCard(ctx, task, domain.TaskStatusSucceeded, &id)
	} else {
		s.publishCard(ctx, task, domain.TaskStatusSucceeded, nil)
	}
	return nil
}

// derive 生成缩略图与封面帧。**它永远不返回错误。**
//
// 派生是转存之后的独立一步，失败只让前端回退到原图；把它做成能让任务失败的
// 一步，等于用一次真实生成的钱去赌一次缩略图编码。
//
// 派生出的键要回填进入参切片：紧随其后的 task.succeeded 事件按这几个键投影
// URL，不回填的话事件里没有缩略图、REST 拉同一条任务却有——前端两边对不上。
func (s *Service) derive(ctx context.Context, assets []domain.Asset) {
	if s.deps.Deriver == nil {
		return
	}
	for i := range assets {
		variants, err := s.deps.Deriver.Derive(context.WithoutCancel(ctx), assets[i])
		if err != nil {
			s.log.Warn("生成派生规格失败，资产仍然有效", "asset_id", assets[i].ID, "err", err)
		}
		if k, ok := variants[asset.VariantThumb512]; ok {
			assets[i].ThumbKey = &k
		}
		if k, ok := variants[asset.VariantPoster]; ok {
			assets[i].PosterKey = &k
		}
	}
}

// recordLineage 写「从谁来的」那一半血缘。
//
// 每次生成都要写：血缘是「做同款」与「单卡重跑」的依据，而它**补不回来**——
// 历史产物当时没记下来源，之后再想追就永远追不到了。因此这里失败只记日志，
// 但绝不因为"这次没有输入"就跳过整段逻辑。
func (s *Service) recordLineage(ctx context.Context, task domain.Task, inputAssetIDs []string, outputs []domain.Asset) {
	if s.deps.Lineage == nil || len(outputs) == 0 {
		return
	}
	edges := asset.InputEdges(inputAssetIDs, outputs)
	// 单卡重跑：新产物指向被替换的旧产物。旧产物 id 由提交方写进
	// inputs 的 rerun_of 槽——它不是喂给上游的素材，只是血缘的锚点。
	for _, prev := range task.Inputs["rerun_of"] {
		edges = append(edges, asset.RerunEdges(prev, outputs)...)
	}
	if len(edges) == 0 {
		return
	}
	if err := s.deps.Lineage.Record(context.WithoutCancel(ctx), edges); err != nil {
		s.log.Error("记录血缘失败", "task_id", task.ID, "err", err)
	}
}

// fail 把任务判为失败：CAS 置 failed → 退款 → 推事件。
//
// **CAS 是谁来记账的仲裁者。** 轮询与 webhook 会并发到达，两边都想把同一条
// 任务判失败；让 UPDATE ... WHERE status = expect 决出唯一的赢家，
// 输的那个安静退出。用锁串行化做不到这一点——多实例部署下进程内的锁
// 管不住别的进程，而退两次款是真实的钱。
func (s *Service) fail(ctx context.Context, task domain.Task, expect domain.TaskStatus, terr *domain.TaskError) error {
	if terr == nil {
		terr = internalError("任务失败但未给出原因", nil)
	}
	// 五类失败一律不扣费，且这件事由服务端显式断言而不是让前端推断。
	terr.Charged = false

	if expect == "" {
		expect = domain.TaskStatusRunning
	}
	if err := s.deps.Tasks.UpdateStatus(ctx, task.ID, expect, domain.TaskStatusFailed, terr); err != nil {
		s.log.Info("置 failed 未生效，任务已被其他路径推进", "task_id", task.ID, "err", err)
		s.stopPolling(task.ID)
		s.release(ctx, task.ID)
		return nil
	}

	s.refund(ctx, task, "任务失败："+string(terr.Code))
	s.stopPolling(task.ID)
	s.release(ctx, task.ID)

	s.publish(ctx, stream.TaskFailed(task.UserID, task.ID, *terr))
	s.publishCredit(ctx, task.UserID)
	s.publishCard(ctx, task, domain.TaskStatusFailed, nil)
	return nil
}

// Cancel 尽力取消一条任务。**两种情况都不扣费。**
//
//	queued   直接置 canceled，全额退回冻结额度。上游还不知道这回事。
//	running  上游支持取消就先调上游（省下它那边的算力），不支持就本地标记。
//	         上游取消失败也照样本地置 canceled——用户按了取消就该看到取消，
//	         上游那边多跑一会儿是我们的成本，不是他该忍受的体验。
func (s *Service) Cancel(ctx context.Context, taskID string) error {
	task, err := s.deps.Tasks.Get(ctx, taskID)
	if err != nil {
		return err
	}
	if task.Status.IsTerminal() {
		return &domain.Error{
			Code:    domain.CodeConflict,
			Message: "任务已经结束，无法取消",
		}
	}

	if task.Status == domain.TaskStatusRunning {
		s.cancelUpstream(ctx, task)
	}
	return s.cancelTask(ctx, task, task.Status)
}

// cancelTask 置 canceled 并退款。
func (s *Service) cancelTask(ctx context.Context, task domain.Task, expect domain.TaskStatus) error {
	if err := s.deps.Tasks.UpdateStatus(ctx, task.ID, expect, domain.TaskStatusCanceled, nil); err != nil {
		s.log.Info("置 canceled 未生效，任务已被其他路径推进", "task_id", task.ID, "err", err)
		s.stopPolling(task.ID)
		return nil
	}

	s.refund(ctx, task, "任务已取消")
	s.stopPolling(task.ID)
	s.release(ctx, task.ID)

	task.Status = domain.TaskStatusCanceled
	s.publish(ctx, stream.TaskUpdated(task.UserID, task))
	s.publishCredit(ctx, task.UserID)
	s.publishCard(ctx, task, domain.TaskStatusCanceled, nil)
	return nil
}

// cancelUpstream 在驱动支持时请求上游取消。失败只记日志，见 Cancel 的注释。
func (s *Service) cancelUpstream(ctx context.Context, task domain.Task) {
	if task.UpstreamRef == nil || *task.UpstreamRef == "" {
		return
	}
	model, provider, err := s.config(ctx, task)
	if err != nil {
		s.log.Warn("取消时读取配置失败，仅本地标记", "task_id", task.ID, "err", err)
		return
	}
	drv, ok := s.deps.Drivers.Async(driverName(model))
	if !ok {
		return
	}
	c, ok := drv.(Canceler)
	if !ok {
		// 该协议不支持取消，本地标记即可——这正是"尽力取消"的含义。
		return
	}
	req := s.pollRequest(task, model, provider)
	req.UpstreamRef = *task.UpstreamRef
	if err := c.CancelUpstream(ctx, req); err != nil {
		s.log.Warn("上游取消失败，仅本地标记", "task_id", task.ID, "err", err)
	}
}

// refund 全额退还冻结额度。
//
// 失败三分类 + insufficient_credit + internal_error，一律全额退——
// "只要没拿到产物就不收钱"。因此这里没有任何按失败类型分支的余地。
func (s *Service) refund(ctx context.Context, task domain.Task, reason string) {
	if s.deps.Ledger == nil {
		return
	}
	if _, err := s.deps.Ledger.Refund(context.WithoutCancel(ctx), task.UserID, task.ID, reason); err != nil {
		s.log.Error("退还冻结积分失败，需人工对账",
			"task_id", task.ID, "user_id", task.UserID, "amount", task.EstimatedCost, "err", err)
	}
}

// config 取任务对应的模型与供应商配置。
func (s *Service) config(ctx context.Context, task domain.Task) (domain.ModelConfig, domain.Provider, error) {
	model, err := s.deps.Models.Get(ctx, task.ModelID)
	if err != nil {
		return domain.ModelConfig{}, domain.Provider{}, fmt.Errorf("读取模型 %s: %w", task.ModelID, err)
	}
	providerID := model.ProviderID
	if providerID == "" {
		providerID = task.ProviderID
	}
	provider, err := s.deps.Providers.Get(ctx, providerID)
	if err != nil {
		return domain.ModelConfig{}, domain.Provider{}, fmt.Errorf("读取供应商 %s: %w", providerID, err)
	}
	return model, provider, nil
}

// pollRequest 拼出一次轮询/取产物的入参。
func (s *Service) pollRequest(task domain.Task, model domain.ModelConfig, provider domain.Provider) adapter.PollRequest {
	req := adapter.PollRequest{
		TaskID:     task.ID,
		Provider:   provider,
		Model:      model,
		Credential: s.deps.Credential(provider.CredentialRef),
		Attempt:    task.Attempt,
	}
	if task.UpstreamRef != nil {
		req.UpstreamRef = *task.UpstreamRef
	}
	return req
}

// resolveInputs 把输入槽里的 upload_id / asset_id 解析成可喂给上游的素材，
// 同时返回本次生成用到的全部资产 id（血缘的 input 边要用）。
//
// upload 在被任务引用的这一刻**提升为 asset**：它本来是 24h 就回收的临时对象，
// 而血缘会长期指向它。不提升的话，回收扫描迟早会删掉一件正在被血缘引用的
// 输入素材，那条创作链从此断掉且补不回来。
func (s *Service) resolveInputs(ctx context.Context, task domain.Task) ([]adapter.InputRef, []string, error) {
	if len(task.Inputs) == 0 {
		return nil, nil, nil
	}

	refs := make([]adapter.InputRef, 0, len(task.Inputs))
	ids := make([]string, 0, len(task.Inputs))
	seen := make(map[string]struct{})

	for slot, values := range task.Inputs {
		// rerun_of 是血缘锚点而不是喂给上游的素材，它由 recordLineage 单独处理。
		if slot == "rerun_of" {
			continue
		}
		for _, raw := range values {
			if raw == "" {
				continue
			}
			a, err := s.resolveOne(ctx, raw)
			if err != nil {
				return nil, nil, err
			}
			if _, dup := seen[a.ID]; !dup {
				seen[a.ID] = struct{}{}
				ids = append(ids, a.ID)
			}
			refs = append(refs, adapter.InputRef{
				Slot:       slot,
				URL:        asset.ContentURL(s.deps.PublicBaseURL, a.ID, asset.VariantOriginal),
				StorageKey: a.StorageKey,
				MIME:       a.MIME,
				Bytes:      a.Bytes,
			})
		}
	}
	return refs, ids, nil
}

// resolveOne 把一个 id 解析成资产：先当 asset 查，查不到再当 upload 提升。
//
// 顺序是"先 asset 后 upload"而不是反过来：已提升的 upload 在 assets 里也有一份，
// 先查 asset 能让重复引用同一份素材的第二次直接命中，不必再走一遍提升。
func (s *Service) resolveOne(ctx context.Context, id string) (domain.Asset, error) {
	if s.deps.Assets != nil {
		if a, err := s.deps.Assets.Get(ctx, id); err == nil {
			return a, nil
		}
	}
	if s.deps.Transferor == nil {
		return domain.Asset{}, &domain.Error{
			Code:    domain.CodeNotFound,
			Message: "输入素材 " + id + " 不存在",
		}
	}
	a, err := s.deps.Transferor.Promote(ctx, id)
	if err != nil {
		return domain.Asset{}, err
	}
	return a, nil
}

// classify 把驱动返回的 error 归到平台的失败分类。
//
// driver 已经把上游错误码归类过了，以 *domain.Error 的形态回来
// （domain.TaskError 是个纯数据结构、不实现 error，只出现在 PollResult.Err
// 那种结果字段上，不会出现在 error 链里）。这里只处理"连归类都没有"的
// 情况——网络错、超时、驱动自己的 bug——一律 internal_error 且可重试。
// **L2 不认识任何上游错误码**，一旦在这里出现按供应商分支的判断，
// L0 的隔离就破了。
func classify(err error) *domain.TaskError {
	if err == nil {
		return nil
	}

	var derr *domain.Error
	if errors.As(err, &derr) && derr != nil {
		if code, ok := taskCodeOf(derr.Code); ok {
			return &domain.TaskError{
				Code:        code,
				Message:     derr.Message,
				FieldErrors: derr.FieldErrors,
				Retryable:   derr.Retryable,
				Charged:     false,
			}
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return &domain.TaskError{
			Code:      domain.TaskErrorInternal,
			Message:   "上游响应超时",
			Retryable: true,
			Charged:   false,
		}
	}
	return internalError("上游调用失败", err)
}

// taskCodeOf 把传输层错误码翻译成任务失败分类。只有产品语义的那几个有对应，
// 401/403/404 这类不该出现在任务失败上。
func taskCodeOf(c domain.ErrorCode) (domain.TaskErrorCode, bool) {
	switch c {
	case domain.CodeInvalidParam:
		return domain.TaskErrorInvalidParam, true
	case domain.CodeUpstreamRateLimited, domain.CodeRateLimited:
		return domain.TaskErrorUpstreamRateLimited, true
	case domain.CodeContentRejected:
		return domain.TaskErrorContentRejected, true
	case domain.CodeInsufficientCredit:
		return domain.TaskErrorInsufficientCredit, true
	case domain.CodeInternal:
		return domain.TaskErrorInternal, true
	default:
		return "", false
	}
}

// internalError 造一条"我们自己的锅"的失败：可重试、不扣费。
//
// 原始 error 只进日志不进 Message：Message 是给用户看的，
// 把 `dial tcp 10.0.0.3:443: connect: connection refused` 显示给他
// 既解释不了什么，又泄露了内网拓扑。
func internalError(msg string, cause error) *domain.TaskError {
	if cause != nil {
		msg = msg + "，请重试"
	}
	return &domain.TaskError{
		Code:      domain.TaskErrorInternal,
		Message:   msg,
		Retryable: true,
		Charged:   false,
	}
}

// driverName 由模型配置推出驱动名。video 族看 VideoProtocol，其余看 Family。
func driverName(m domain.ModelConfig) string {
	if m.Family == domain.FamilyVideo {
		if m.VideoProtocol == nil {
			return ""
		}
		return string(*m.VideoProtocol)
	}
	return string(m.Family)
}

// sleepCtx 等待 d，ctx 先结束时返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// 画布那三条同步 chat 链路（写剧本 / 拆分镜 / 优化剧本）的计价闸。
//
// 这三条一直在真打上游、烧真 token，却一分积分都不扣：Pricer 与 Ledger 在
// 它们的调用路径上出现次数为 0。挂个脚本刷 refine，账单是平台的。
//
// # 为什么这几次调用也要在 tasks 表落一行
//
// credit_ledger.task_id 上有指向 tasks 的外键，而 hold / charge / refund 三条
// 流水是按 (user_id, task_id) 配对结算的（见 store/mysql.taskHoldTx）。不落这
// 一行，冻结就只能挂在 task_id = NULL 上，同一用户并发的两次写剧本会共用同
// 一笔"冻结总额"，先结算的那次把对方的钱一起结掉。落这一行不是为了伪造一条
// 任务，是为了让这笔钱有一个可对账的对象——ProfilePage 的账单文案也正是靠
// task_id 回查模态与模型名的。
//
// 状态直接落 running 而不是 queued：queued 会被 worker 的 Claim 捞走，同一次
// 调用于是在上游跑第二遍、花第二份钱。running 且 next_poll_at 为 NULL 的行
// Claim 与 ClaimPoll 两个查询都扫不到（见 store/mysql.queue），执行链路碰不到它。
//
// # 为什么是每次调用一口价，而不是按 token
//
// 这里走的是模型 capability 里已经声明好的 pricing.base（qwen-flash 1 分、
// gpt-5-2 6 分……），一次调用一口价。按 token 计费在当前这套 schema 下拼不
// 出来，缺的是两样东西，任缺一样都只能得到一个假的数：
//
//   - PricingSpec 只能乘一个**请求参数**（multiplier_param），而请求参数在
//     调用前就已知；token 用量要等上游回话才知道。要按 token 报价，
//     PricingSpec 得多一个「每千 token 单价」的字段。
//   - adapter.SubmitResult 不带 usage。openaicompat 的响应结构体里根本没有
//     usage 这个字段（见 openaicompat/errors.go 的 apiResponse），上游报回来的
//     total_tokens 在解码那一步就丢了。要按实际用量结算，driver 得把它带回来，
//     charge 才有 actual 可填（Ledger.Charge 本来就允许 actual < held，
//     少收这条路是通的，多收则被硬拦）。
//
// 两样都属于跨层改动，而这张票要的是**闸**：一口价先让这三条链路从"完全
// 不扣"变成"扣得少了点"，比继续白烧强。闸比精度重要。
//
// # 哪一步算"成功"
//
// 出片链路的成功点是产物落进资产库（executor.finish）；这三条链路的产物是
// 文字，落点是画布。因此成功点定在 Canvas.Apply 返回之后：在那之前的任何
// 失败——选模型、上游 4xx/5xx、超时、内容审核被拒、回复解析不出来、落卡撞
// 版本——一律全额退，用户一分钱不掏；在那之后的失败（补一条对话消息没写
// 进去）不再退，改写后的正文已经在画布上了，用户拿到了他买的东西。
package httpapi

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aigc-pool/aigc-pool/internal/billing"
	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// chatCall 描述一次要计费的同步 chat 调用。
//
// step 是这次调用属于哪一步（"script" / "refine" / "storyboard"）。它进日志、
// 进上游的任务标识，也**落进 tasks.step**——管理端的重试靠它认出"这一行不是
// executor 执行的"，见 handleAdminRetryTask；
// projectID / cardID 落进 tasks 的 canvas_id / card_id，账单因此答得出
// "这笔钱是哪张画布、哪张卡花的"。
type chatCall struct {
	step      string
	userID    string
	modelID   string // 用户这次点名的模型，空串表示按默认规则选
	projectID string
	cardID    string

	// prompt 是真正发给上游的那一串，含我们拼的系统提示词。它只进
	// adapter.SubmitInput（见 chatcall.chatOnce），**不落库**。
	prompt string

	// userInput 是这次调用里用户自己的那一份，落进 tasks.prompt：写剧本是他发的
	// 那句话，优化是他写的修改要求，拆分镜是被拆的那张剧本卡的正文摘要。
	//
	// 两者必须分开，因为 tasks.prompt 会原样出现在用户自己就能拉的
	// GET /api/tasks 里——把 prompt 直接落下去，等于把系统提示词发给每一个
	// 问得出这个接口的人。而 tasks.prompt 在其余所有链路上装的本来就是用户
	// 提交的那段话（见 handlers_tasks.submitTask 的 Prompt: req.Prompt），
	// 这三条落 userInput 才是与全局一致的那个语义，不是为了遮挡开的特例。
	userInput string
}

// chatBill 是一次同步 chat 调用已经冻结的那笔积分。
//
// 拿到它就意味着钱已经冻上了，调用方**必须**在两个出口之一把它结掉：
// 产物落到画布上走 charge，其余一切走 refund。
type chatBill struct {
	srv    *server
	userID string
	taskID string
	cost   int
}

// holdChat 在调用上游**之前**把这次 chat 的积分冻上。
//
// 顺序与 handlers_tasks.submitTask 一字不差：Estimate → Balance → 落库 → Hold。
// 落库必须在 Hold 之前，因为流水的外键指向 tasks；余额检查必须在落库之前，
// 让 Hold 在正常路径上不可能失败。真撞上并发耗尽余额时 Hold 会顶回来，
// 那条刚落的任务随即被判 failed 且不扣费。
func (s *server) holdChat(ctx context.Context, call chatCall, model domain.ModelConfig,
	schema capability.ModelCapabilitySchema) (*chatBill, error) {

	params := defaultParams(schema)
	cost, err := s.deps.Pricer.Estimate(schema.Pricing, capability.EvalContext{Params: params})
	if err != nil {
		return nil, err
	}

	if bal, err := s.deps.Ledger.Balance(ctx, call.userID); err == nil && bal.Available < cost {
		return nil, &domain.Error{
			Code:    domain.CodeInsufficientCredit,
			Message: "积分不足：需要 " + itoa(cost) + "，可用 " + itoa(bal.Available),
		}
	}

	task, err := s.deps.Tasks.Create(ctx, domain.Task{
		UserID:        call.userID,
		ModelID:       model.ID,
		ProviderID:    model.ProviderID,
		Status:        domain.TaskStatusRunning,
		Prompt:        call.userInput, // 不是 call.prompt，理由见 chatCall 的字段注释

		// Step 让这一行认得出自己的来源。管理端的重试据此拒绝它：这条链路
		// 的产物落点是画布卡片，把行打回 queued 只会让 worker 重打一次上游、
		// 花第二份钱，而产物落成一个孤儿 text 资产（见 handleAdminRetryTask）。
		Step: call.step,

		Params:        params,
		EstimatedCost: cost,
		CanvasID:      strPtrOrNil(call.projectID),
		CardID:        strPtrOrNil(call.cardID),
		CreatedAt:     s.now(),
	})
	if err != nil {
		return nil, err
	}

	if _, err := s.deps.Ledger.Hold(ctx, call.userID, task.ID, cost); err != nil {
		terr := taskErrorFrom(err)
		_ = s.deps.Tasks.UpdateStatus(detachedContext(), task.ID,
			domain.TaskStatusRunning, domain.TaskStatusFailed, &terr)
		return nil, err
	}

	s.publishBalance(ctx, call.userID)
	return &chatBill{srv: s, userID: call.userID, taskID: task.ID, cost: cost}, nil
}

// charge 结算这笔冻结：产出已经落到画布上了。
//
// 用 WithoutCancel 而不是请求的 ctx：用户在模型回话之后、落卡之前关掉页面
// 是常态，而那时钱已经花了，结算不能跟着请求一起被取消——那会留下一笔永远
// 挂着的冻结。
//
// CAS 输了就直接退出，不再结算：说明另一条路径（并发的重复请求）已经把这条
// 任务推进过了，那一次已经连带把冻结释放掉。结算本身失败**不回滚 succeeded**，
// 理由同 executor.finish：产出已经交付，少收一笔可以靠流水对账补，
// 把一次成功的交付改判失败不可逆。
func (b *chatBill) charge(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	s := b.srv

	if err := s.deps.Tasks.UpdateStatus(ctx, b.taskID,
		domain.TaskStatusRunning, domain.TaskStatusSucceeded, nil); err != nil {
		slog.Info("置 succeeded 未生效，chat 任务已被其他路径推进", "task_id", b.taskID, "err", err)
		return
	}
	if err := s.deps.Tasks.SetActualCost(ctx, b.taskID, b.cost); err != nil {
		slog.Warn("记录 chat 实扣额度失败", "task_id", b.taskID, "err", err)
	}
	if _, err := s.deps.Ledger.Charge(ctx, b.userID, b.taskID, b.cost); err != nil {
		if errors.Is(err, billing.ErrAlreadySettled) {
			slog.Info("结算已由另一条路径完成，跳过", "task_id", b.taskID, "user_id", b.userID)
		} else {
			slog.Error("结算 chat 积分失败，需人工对账",
				"task_id", b.taskID, "user_id", b.userID, "amount", b.cost, "err", err)
		}
	}
	s.publishBalance(ctx, b.userID)
}

// refund 全额退回这笔冻结：这次调用没有拿到能交付的产出。
//
// 不按失败类型分支——「只要没拿到产物就不收钱」（见 internal/billing 的包
// 注释）。reason 的措辞与 executor.fail 保持一致（"任务失败：<code>"），
// 前端的账单文案按冒号切最后一段取错误码，两条路分叉就会显示成泛化文案。
func (b *chatBill) refund(ctx context.Context, cause error) {
	ctx = context.WithoutCancel(ctx)
	s := b.srv

	terr := taskErrorFrom(cause)
	if err := s.deps.Tasks.UpdateStatus(ctx, b.taskID,
		domain.TaskStatusRunning, domain.TaskStatusFailed, &terr); err != nil {
		slog.Info("置 failed 未生效，chat 任务已被其他路径推进", "task_id", b.taskID, "err", err)
		return
	}
	if _, err := s.deps.Ledger.Refund(ctx, b.userID, b.taskID, "任务失败："+string(terr.Code)); err != nil {
		s.logRefund(b.taskID, err)
	}
	s.publishBalance(ctx, b.userID)
}

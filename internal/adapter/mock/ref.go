package mock

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// refPrefix 给 UpstreamRef 打上版本化前缀。
// 带版本是因为这个串会**落库**：mock 的编码换一次形状，库里还留着旧串的
// 在途任务不能因此炸掉，得能认出"这是旧版本"并给出可读的错误。
const refPrefix = "mock:v1:"

// upstreamRef 是 mock 编码进 adapter.SubmitResult.UpstreamRef 的全部状态。
//
// # 为什么状态在 ref 里而不在内存 map 里
//
// 真实驱动的任务状态在上游那边，进程重启不影响；mock 若把状态放进
// 进程内的 map，重启就全丢了，在途任务永远卡在 running——而"进程随时可以重启、
// 在途任务不丢"正是 upstream_ref 必须落库这条设计要验证的东西。
// 把状态编码进这个不透明串，mock 与真实驱动在这一点上才真正等价：
// 任何一个 worker 拿到这个串都能算出同样的状态，Poll 天然幂等。
type upstreamRef struct {
	// TaskID 冗余一份，便于在日志里把 ref 与任务对上。
	TaskID string `json:"t"`
	// SubmittedAtUnixNano 是提交时刻，状态完全由 now-它 推出来。
	SubmittedAtUnixNano int64 `json:"s"`
	// DelayNanos 是本次模拟的总耗时。
	DelayNanos int64 `json:"d"`
	// Outcome 是本次任务的注定结局，提交时就定死。
	// 让 Poll 每次自己掷骰子会产生"上一次轮询说成功、这一次说失败"的
	// 不幂等行为，那是真实上游绝不会有的病。
	Outcome outcome `json:"o"`
	// FailureCode 仅 Outcome 为 outcomeFailed 时有意义。
	FailureCode domain.TaskErrorCode `json:"c,omitempty"`
	// 产物元信息。视频产物是固定的内嵌样片，尺寸时长在提交时就确定。
	Width      int `json:"w,omitempty"`
	Height     int `json:"h,omitempty"`
	DurationMS int `json:"ms,omitempty"`
}

type outcome string

const (
	outcomeSucceeded outcome = "ok"
	outcomeFailed    outcome = "fail"
)

func encodeRef(r upstreamRef) (string, error) {
	buf, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("mock: upstream_ref 编码失败: %w", err)
	}
	return refPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func decodeRef(s string) (upstreamRef, error) {
	if !strings.HasPrefix(s, refPrefix) {
		return upstreamRef{}, fmt.Errorf("mock: upstream_ref %q 不是本驱动生成的（缺少 %s 前缀）", s, refPrefix)
	}
	buf, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, refPrefix))
	if err != nil {
		return upstreamRef{}, fmt.Errorf("mock: upstream_ref 解码失败: %w", err)
	}
	var r upstreamRef
	if err := json.Unmarshal(buf, &r); err != nil {
		return upstreamRef{}, fmt.Errorf("mock: upstream_ref 反序列化失败: %w", err)
	}
	return r, nil
}

// 状态推进的两个时间分界，均为总耗时的比例。
//
// 留一段 queued 是必要的：真实上游几乎总要排一会儿队，
// 前端的"排队中"这一态如果本地永远看不到，就等于没实现过。
const (
	queuedFraction  = 0.2
	runningCeiling  = 0.95 // 进度上限，succeeded 之前不给满格
	runningFloorPct = 0.02
)

// stateAt 由"提交至今过了多久"推出当前状态与进度。
//
// 纯函数：同一个 ref + 同一个时刻恒得同一个结果。这就是 Poll 幂等的全部实现——
// 不需要锁、不需要去重表，因为它压根不写任何东西。
func (r upstreamRef) stateAt(now time.Time) (raw string, progress float64) {
	elapsed := now.UnixNano() - r.SubmittedAtUnixNano
	total := r.DelayNanos
	if total <= 0 {
		total = int64(DefaultSimulatedDelay)
	}
	frac := float64(elapsed) / float64(total)

	switch {
	case frac < queuedFraction:
		return "queued", 0
	case frac < 1:
		// 把 [queuedFraction, 1) 线性拉伸到 [0.02, 0.95)，
		// 让进度条在整个 running 期间都在动。
		p := (frac - queuedFraction) / (1 - queuedFraction)
		p = runningFloorPct + p*(runningCeiling-runningFloorPct)
		return "running", p
	}
	if r.Outcome == outcomeFailed {
		return "failed", 0
	}
	return "succeeded", 1
}

// etaAt 是剩余秒数，供前端做乐观进度推进。
func (r upstreamRef) etaAt(now time.Time) float64 {
	total := r.DelayNanos
	if total <= 0 {
		total = int64(DefaultSimulatedDelay)
	}
	remaining := (r.SubmittedAtUnixNano + total) - now.UnixNano()
	if remaining < 0 {
		remaining = 0
	}
	return time.Duration(remaining).Seconds()
}

// decideOutcome 按 FailureRate 决定这个任务是成是败。
//
// 用 TaskID 的哈希而不是 math/rand：同一个任务无论被谁、被重放多少次
// 都得到同一个结局。随机数会让"这条任务为什么失败了"变成不可复现的问题，
// 而失败路径正是 mock 最需要能被反复触发的那条。
func decideOutcome(taskID string, failureRate float64) outcome {
	if failureRate <= 0 {
		return outcomeSucceeded
	}
	if failureRate >= 1 {
		return outcomeFailed
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(taskID))
	// 取哈希的低 32 位映射到 [0,1)，与 failureRate 比较。
	bucket := float64(h.Sum64()&0xFFFFFFFF) / float64(1<<32)
	if bucket < failureRate {
		return outcomeFailed
	}
	return outcomeSucceeded
}

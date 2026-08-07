package domain

import "time"

// LedgerEntryType 是流水条目的类型。
//
//	hold   冻结（提交任务时预扣，防止并发提交把余额透支）
//	charge 成功实扣（结算冻结额度）
//	refund 失败退回（释放冻结额度）
//	topup  管理员充值（不接支付网关，PRD 定的手工充值）
//	adjust 人工修正
type LedgerEntryType string

const (
	LedgerHold   LedgerEntryType = "hold"
	LedgerCharge LedgerEntryType = "charge"
	LedgerRefund LedgerEntryType = "refund"
	LedgerTopup  LedgerEntryType = "topup"
	LedgerAdjust LedgerEntryType = "adjust"
)

// LedgerEntry 是积分流水的一条记录。
//
// **append-only 的流水是余额的唯一真相**，User.Credits 只是它的物化缓存。
// 任何时候两者对不上，以流水重算的结果为准。
//
// Amount 有符号：正=入账，负=出账。BalanceAfter 是这条记录落库后的余额快照，
// 用于对账时无需从头重放整条流水。
type LedgerEntry struct {
	ID           string          `json:"id"`
	UserID       string          `json:"user_id"`
	TaskID       *string         `json:"task_id"`
	Type         LedgerEntryType `json:"type"`
	Amount       int             `json:"amount"`
	BalanceAfter int             `json:"balance_after"`
	Reason       string          `json:"reason,omitempty"`
	// OperatorID 管理员操作时记录，用户自发行为为 nil。
	OperatorID *string `json:"operator_id"`
	// IdempotencyKey 防止管理员重复点击充两次；同 key 二次写入返回 409。
	IdempotencyKey string    `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
}

// Balance 是某用户的积分余额视图。Available 已扣除 Held，
// 因此判断「够不够付这次任务」只看 Available。
type Balance struct {
	Available int `json:"balance"`
	Held      int `json:"held"`
}

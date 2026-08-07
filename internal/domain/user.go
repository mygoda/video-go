package domain

import "time"

// Role 是用户角色。admin 额外可访问 /api/admin/* 路由组。
type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

// UserStatus 是账号状态。disabled 的账号能通过 token 校验但一律拒绝业务操作。
type UserStatus string

const (
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
)

// User 是一个账号。
//
// PasswordHash 只在服务端流转，json 标签为 "-"，任何路径都不下发。
//
// Credits 是**物化缓存**，不是真相：真相是 append-only 的 LedgerEntry 流水
// （见 internal/billing）。CreditsHeld 是进行中任务冻结的额度，
// Credits 已经扣除了它，因此 Credits 就是可用余额。
type User struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	PasswordHash string     `json:"-"`
	Role         Role       `json:"role"`
	Status       UserStatus `json:"status"`

	Credits     int `json:"credits"`
	CreditsHeld int `json:"credits_held"`

	StorageUsed  int64 `json:"storage_used"`
	StorageQuota int64 `json:"storage_quota"`

	TaskCount    int        `json:"task_count,omitempty"`
	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Session 是一次成功认证的结果，对应 openapi 的 AuthResponse。
type Session struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	User      User      `json:"user"`
}

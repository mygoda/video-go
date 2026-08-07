package httpapi

import (
	"net/http"
	"strconv"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// pageEnvelope 是全部游标分页响应的形状：{items, next_cursor}。
//
// 泛型而不是每个资源写一个：形状一旦有第二种写法，前端就要为它多写一个分支。
type pageEnvelope[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// pageOf 包装一页数据。items 为 nil 时替换成空切片——
// JSON 里的 null 会让前端的 .map 直接炸，而"这一页没有条目"是正常结果。
func pageOf[T any](items []T, next string) pageEnvelope[T] {
	if items == nil {
		items = []T{}
	}
	return pageEnvelope[T]{Items: items, NextCursor: next}
}

// pathID 取一个路径参数并拒绝空值。
func pathID(r *http.Request, name string) (string, error) {
	v := r.PathValue(name)
	if v == "" {
		return "", errInvalid("缺少路径参数 %s", name)
	}
	return v, nil
}

// queryInt 读一个整型查询参数，缺失或非法时返回 def。
func queryInt(r *http.Request, name string, def int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// requireOwner 校验资源属于调用方。
//
// **管理员在用户端路由上不豁免**：/api/* 是"我的东西"，管理员要看别人的
// 资产得走 /api/admin/*。豁免会让一个管理员账号在普通页面里看到别人的数据，
// 而那些页面的 UI 根本没有"这是谁的"这个概念。
func requireOwner(id Identity, ownerID, what string) error {
	if ownerID != id.UserID {
		// 回 404 而不是 403：403 等于确认了"这个 id 是存在的，只是不是你的"。
		return errNotFound(what)
	}
	return nil
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// taskErrorFrom 把一个 *domain.Error 翻译成任务侧的 TaskError。
//
// 两套码有意分开（ApiErrorCode 讲"这次 HTTP 调用怎么了"，TaskErrorCode 讲
// "这次生成怎么了"），因此翻译必须显式且集中在这里；散在各处现写迟早会把
// 一个 not_found 塞进 task.error.code，而前端的失败卡片没有这个分支。
func taskErrorFrom(err error) domain.TaskError {
	de := asDomainError(err)
	code := domain.TaskErrorInternal
	switch de.Code {
	case domain.CodeInvalidParam:
		code = domain.TaskErrorInvalidParam
	case domain.CodeUpstreamRateLimited, domain.CodeRateLimited:
		code = domain.TaskErrorUpstreamRateLimited
	case domain.CodeContentRejected:
		code = domain.TaskErrorContentRejected
	case domain.CodeInsufficientCredit:
		code = domain.TaskErrorInsufficientCredit
	}
	return domain.TaskError{
		Code:        code,
		Message:     de.Message,
		FieldErrors: de.FieldErrors,
		// 失败三分类恒不扣费，见 internal/billing 的包注释。
		Charged: false,
	}
}

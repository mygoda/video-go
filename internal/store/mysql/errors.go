package mysql

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// MySQL 错误号。用常量而不是散在各处的字面量，读的人不必去查手册。
const (
	erDupEntry        = 1062 // 唯一键冲突
	erRowIsReferenced = 1451 // 删除父行但仍被外键引用
	erNoReferencedRow = 1452 // 插入的外键值在父表里不存在
	erCheckConstraint = 3819 // CHECK 约束不满足
)

// notFound 构造一个 CodeNotFound 错误。entity 是给人看的实体名。
func notFound(entity, id string) error {
	return &domain.Error{
		Code:    domain.CodeNotFound,
		Message: entity + " " + id + " not found",
	}
}

// conflict 构造一个 CodeConflict 错误。
//
// 「前置状态不匹配」「唯一键撞了」「还有引用不能删」在传输层是同一个语义：
// 请求本身没错，是当前状态不允许，重试也不会自己变好，因此都归到 409。
func conflict(msg string, cause error) error {
	return &domain.Error{
		Code:    domain.CodeConflict,
		Message: msg,
		Err:     cause,
	}
}

// revisionConflict 构造一个 CodeRevisionConflict 错误。
//
// 它与 conflict 分开，是因为前端对它有专门的处理：拉全量画布覆盖本地，
// 而不是像普通 409 那样提示用户重试。
func revisionConflict(msg string) error {
	return &domain.Error{
		Code:    domain.CodeRevisionConflict,
		Message: msg,
	}
}

// insufficientCredit 构造一个 CodeInsufficientCredit 错误（HTTP 402）。
func insufficientCredit(msg string) error {
	return &domain.Error{
		Code:    domain.CodeInsufficientCredit,
		Message: msg,
	}
}

// quotaExceeded 构造一个 CodeQuotaExceeded 错误。
func quotaExceeded(msg string) error {
	return &domain.Error{
		Code:    domain.CodeQuotaExceeded,
		Message: msg,
	}
}

// invalidParam 构造一个 CodeInvalidParam 错误，用于调用方传进来的参数
// 在落库前就能判定为非法的场景（如空 id）。
func invalidParam(msg string) error {
	return &domain.Error{
		Code:    domain.CodeInvalidParam,
		Message: msg,
	}
}

// wrap 把一个驱动层错误翻译成 domain.Error。
//
// 翻译发生在这里而不是 handler：约束冲突的语义只有写 SQL 的人知道
// （1062 在 uploads 上和在 credit_ledger 上是两件事），让 handler 去认
// MySQL 错误号等于把存储细节泄露到传输层。
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	var already *domain.Error
	if errors.As(err, &already) {
		return err
	}

	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case erDupEntry:
			return conflict(op+": duplicate key", err)
		case erRowIsReferenced:
			return conflict(op+": row is still referenced", err)
		case erNoReferencedRow:
			return conflict(op+": referenced row does not exist", err)
		case erCheckConstraint:
			return invalidParam(op + ": check constraint violated")
		}
	}

	return &domain.Error{
		Code:      domain.CodeInternal,
		Message:   "storage: " + op,
		Retryable: true,
		Err:       err,
	}
}

// wrapScan 与 wrap 同理，但把 sql.ErrNoRows 翻成 CodeNotFound。
func wrapScan(entity, id, op string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return notFound(entity, id)
	}
	return wrap(op, err)
}

// isNoRows 报告 err 是否为"查了但没有行"。
//
// 有些方法把"没有"当成正常答案（GetByClientToken 的"这个 token 没提交过"、
// Queue.Claim 的"当前没有可领的任务"），它们需要在翻错误之前先把这一种分出来。
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// isDup 报告 err 是否为唯一键冲突。
//
// 幂等写（LedgerRepo 的幂等键、UserRepo 的用户名）要在撞键时走"读回已有行"
// 而不是报错，因此需要单独判定这一个错误号。
func isDup(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == erDupEntry
}

// affected 读取影响行数，顺手把驱动错误翻译掉。
func affected(res sql.Result, op string) (int64, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, wrap(op, err)
	}
	return n, nil
}

// requireID 拒绝空 id。空 id 落到 SQL 里会安静地匹配 0 行，
// 调用方拿到的是 not_found，排查时看不出是自己传空了。
func requireID(name, id string) error {
	if id == "" {
		return invalidParam(fmt.Sprintf("%s must not be empty", name))
	}
	return nil
}

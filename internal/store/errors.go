package store

import (
	"errors"
	"strings"

	"github.com/mattn/go-sqlite3"
)

// IsUniqueViolation 判定错误是否由唯一约束冲突引起。
//
// 用途：把"uid 已被占用"这类可预期的业务冲突与真正的数据库故障区分开，
// 前者对外是 409 + 友好文案，后者是 500 + 统一文案（不泄漏 SQL 文本）。
//
// 实现上优先使用驱动暴露的错误码；只有在无法断言类型时才退化为
// 字符串判断，避免把驱动细节散落到业务代码里。
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}

	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqlite3.ErrConstraint &&
			sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique
	}

	// 退化路径：GORM 可能包装错误，导致类型断言失败。
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

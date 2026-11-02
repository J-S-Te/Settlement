package dunning

import (
	"errors"

	"github.com/go-sql-driver/mysql"
)

// duplicateKeyError 判断错误是否来自唯一键冲突（MySQL 1062）。
// 参数：err 为驱动返回的错误。
// 返回值：唯一键冲突时为真；连接、语法、死锁等其他错误为假，调用方必须原样返回，
// 不能把未知状态当作“已有并发案件”继续执行。
func duplicateKeyError(err error) bool {
	var dbErr *mysql.MySQLError
	return errors.As(err, &dbErr) && dbErr.Number == 1062
}

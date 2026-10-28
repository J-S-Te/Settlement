package platform

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestLoginPersistsUTCWithoutDriverTimezoneConversion 用途：防止登录事务被MySQL连接时区延长。
// 参数：t 为测试上下文。
// 返回值：无；使用本仓库既有SQL替身，不连接业务库。
func TestLoginPersistsUTCWithoutDriverTimezoneConversion(t *testing.T) {
	a, db, _ := logoutFixture(t)
	before := time.Now().UTC()
	response := httptest.NewRecorder()
	a.Login(response, httptest.NewRequest("GET", "/auth/login", nil))
	if response.Code != 302 || len(db.writes) != 1 {
		t.Fatalf("login status=%d writes=%d", response.Code, len(db.writes))
	}
	for _, index := range []int{5, 6} {
		raw, ok := db.writes[0].args[index].Value.(string)
		if !ok {
			t.Fatalf("timestamp argument %d must be UTC text independent of driver loc; got %T", index, db.writes[0].args[index].Value)
		}
		stamp, err := time.ParseInLocation("2006-01-02 15:04:05.000", raw, time.UTC)
		if err != nil {
			t.Fatal("invalid UTC timestamp")
		}
		if index == 5 {
			stamp = stamp.Add(-10 * time.Minute)
		}
		if stamp.Before(before.Add(-time.Millisecond)) || stamp.After(time.Now().UTC()) {
			t.Fatal("login time does not match UTC instant")
		}
	}
}

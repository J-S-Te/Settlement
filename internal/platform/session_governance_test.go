package platform

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"testing"

	central "github.com/j-s-te/settlement/internal/sessiongovernance"
)

type settlementCentralTestClient struct {
	central.Client
	err        error
	activities int
}

// State 提供确定失效或暂时故障，普通读取不更新活动。
// 参数：请求与服务器绑定秘密。
// 返回值：空窗口及指定错误。
func (c *settlementCentralTestClient) State(context.Context, string) (central.Context, error) {
	return central.Context{}, c.err
}

// Activity 只统计显式活动代理。
// 参数：请求与服务器绑定秘密。
// 返回值：空窗口及指定错误。
func (c *settlementCentralTestClient) Activity(context.Context, string) (central.Context, error) {
	c.activities++
	return central.Context{}, c.err
}

// TestSettlementCentralAuthorityFailure 验证中央失效和依赖故障区分，旧记录不补造周期。
// 参数：t为测试上下文。
// 返回值：无；错误分类或活动来源不符时失败。
func TestSettlementCentralAuthorityFailure(t *testing.T) {
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	codec, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		bound          bool
		upstream, want error
	}{{false, nil, ErrUnauthenticated}, {true, central.ErrInactive, ErrUnauthenticated}, {true, central.ErrUnavailable, ErrSessionUnavailable}} {
		client := &settlementCentralTestClient{err: test.upstream}
		auth := &Authenticator{codec: codec, central: client}
		row := centralSession{}
		if test.bound {
			row.BindingCipher = auth.encrypt([]byte("test-binding"))
		}
		_, err := auth.readCentral(context.Background(), row, false)
		if !errors.Is(err, test.want) || client.activities != 0 {
			t.Fatalf("got%v activities%d", err, client.activities)
		}
	}
}

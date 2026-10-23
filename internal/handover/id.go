package handover

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// NewID 生成 32 字符标识（时间前缀 + 加密随机后缀），与结算仓既有 ID 约定一致。
func NewID(now time.Time) (string, error) {
	milliseconds := now.UTC().UnixMilli()
	if milliseconds < 0 || milliseconds > 0xFFFFFFFFFFFF {
		return "", fmt.Errorf("handover id timestamp is out of range")
	}
	raw := make([]byte, 16)
	raw[0] = byte(milliseconds >> 40)
	raw[1] = byte(milliseconds >> 32)
	raw[2] = byte(milliseconds >> 24)
	raw[3] = byte(milliseconds >> 16)
	raw[4] = byte(milliseconds >> 8)
	raw[5] = byte(milliseconds)
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", fmt.Errorf("read handover id randomness: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

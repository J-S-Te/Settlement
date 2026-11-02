package handover

import (
	"fmt"
	"testing"
)

// 回执主键必须是 32 字符、确定性、且不同幂等键不撞键（旧实现 mod 1e8 会撞）。
func TestShortReceiptIDIsStable(t *testing.T) {
	keys := []string{
		"handover:batch:item-1",
		"handover:batch:item-6609",
		"handover:batch:item-10020",
		"tenant-a|dunning-action-0001",
		"",
		"中文幂等键-测试",
	}
	for _, key := range keys {
		first := shortReceiptID(key)
		second := shortReceiptID(key)
		if first != second {
			t.Fatalf("shortReceiptID(%q) is not deterministic: %q != %q", key, first, second)
		}
	}
}

func TestShortReceiptIDLengthAndCharset(t *testing.T) {
	keys := []string{
		"handover:batch:item-1",
		"handover:batch:item-6609",
		"",
		"tenant/with:odd chars\t\n",
	}
	for _, key := range keys {
		id := shortReceiptID(key)
		if len(id) != 32 {
			t.Fatalf("shortReceiptID(%q) length = %d, want 32", key, len(id))
		}
		if id[:len(receiptIDPrefix)] != receiptIDPrefix {
			t.Fatalf("shortReceiptID(%q) = %q, want prefix %q", key, id, receiptIDPrefix)
		}
		for _, r := range id {
			if r >= 'A' && r <= 'Z' {
				continue
			}
			if r >= '0' && r <= '9' {
				continue
			}
			t.Fatalf("shortReceiptID(%q) = %q contains non-ASCII-alphanumeric rune %q", key, id, r)
		}
	}
}

// 已知易撞样本：旧实现（sum = (sum*31 + r) % 1e8）把下列键滚动哈希到同一余数。
//
// handover:batch:item-920009 与 handover:batch:item-1008670 的旧余数同为 45234789；
// 旧主键又是全局 PRIMARY KEY(id)（不区分租户），撞键后 writeReceipt 按
// (tenant_id, idempotency_key) 查不到既有回执而报错，导致整个事务回滚并让该幂等键永久失败。
// 需求中提到的 handover:batch:item-6609 / item-10020 在本仓该哈希变体下并不相撞，
// 仍作为回归样本保留。
func TestShortReceiptIDDoesNotCollideOnKnownKeys(t *testing.T) {
	pairs := [][2]string{
		{"handover:batch:item-920009", "handover:batch:item-1008670"},
		{"handover:batch:item-6609", "handover:batch:item-10020"},
	}
	for _, pair := range pairs {
		left, right := shortReceiptID(pair[0]), shortReceiptID(pair[1])
		if left == right {
			t.Fatalf("collision: %q and %q both map to %q", pair[0], pair[1], left)
		}
	}
}

// 大样本：批量相邻序号与各租户前缀组合，不应出现任何碰撞。
func TestShortReceiptIDHasNoCollisionsAcrossSamples(t *testing.T) {
	keys := make([]string, 0, 200000)
	tenants := []string{"tenant-a", "tenant-b", "tenant-c", "tenant-d"}
	for _, tenant := range tenants {
		for item := 1; item <= 20000; item++ {
			keys = append(keys, fmt.Sprintf("handover:batch:item-%d", item))
			keys = append(keys, fmt.Sprintf("%s|handover:batch:item-%d", tenant, item))
		}
	}
	seen := make(map[string]string, len(keys))
	for _, key := range keys {
		id := shortReceiptID(key)
		if previous, ok := seen[id]; ok && previous != key {
			t.Fatalf("collision: %q and %q both map to %q", previous, key, id)
		}
		seen[id] = key
	}
}

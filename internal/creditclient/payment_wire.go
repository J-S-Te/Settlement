package creditclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// normalizePaymentWire 移除旧持久化报文的内部字段并严格检查出站结构。
// 参数：tenant为调用方可信租户，payload为原始持久化付款事件。
// 返回值：CRM付款契约报文；未知字段、重复键、类型或租户错配返回错误。
func normalizePaymentWire(tenant string, payload []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("CRM credit payload must be one JSON object")
	}
	fields := make(map[string]json.RawMessage)
	seen := make(map[string]bool)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok || seen[key] {
			return nil, fmt.Errorf("duplicate or invalid CRM credit field")
		}
		seen[key] = true
		var raw json.RawMessage
		if err = decoder.Decode(&raw); err != nil {
			return nil, err
		}
		internal, err := validatePaymentWireField(key, raw, tenant)
		if err != nil {
			return nil, err
		}
		if !internal {
			fields[key] = raw
		}
	}
	if _, err = decoder.Token(); err != nil {
		return nil, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("CRM credit payload contains trailing content")
	}
	return json.Marshal(fields)
}

// validatePaymentWireField 校验单字段类型并识别可删除的内部元数据。
// 参数：key和raw为原始JSON字段，tenant为投递所用可信租户。
// 返回值：字段是否仅供内部使用；未知字段、类型及租户不符返回错误。
func validatePaymentWireField(key string, raw json.RawMessage, tenant string) (bool, error) {
	var target any
	internal := false
	switch key {
	case "tenant_id", "receipt_id", "allocation_id":
		target, internal = new(string), true
	case "event_id", "event_type", "payment_id", "reversal_of_payment_id",
		"due_amount", "paid_amount", "contract_no", "period_no", "source_system":
		target = new(string)
	case "customer_id":
		target = new(uint64)
	case "due_date", "paid_date":
		target = new(time.Time)
	default:
		return false, fmt.Errorf("unknown CRM credit field %q", key)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if key == "paid_date" {
			return false, nil
		}
		return false, fmt.Errorf("CRM credit field %q cannot be null", key)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return false, fmt.Errorf("invalid CRM credit field %q", key)
	}
	if key == "tenant_id" && *target.(*string) != tenant {
		return false, fmt.Errorf("CRM credit payload tenant does not match delivery tenant")
	}
	return internal, nil
}

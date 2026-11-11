package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

type ReimbursementCostSettings struct {
	Enabled                                                             bool
	URL, TokenURL, ClientID, ClientSecret, Scope, TenantID, Environment string
}

// ReimbursementCosts 读取独立报销候选发送用途，默认关闭且配置完整才可启用。
// 参数：无，读取当前进程环境。
// 返回值：完整配置；缺键、身份不匹配或非法URL启动失败。
func (c Config) ReimbursementCosts() (ReimbursementCostSettings, error) {
	var out ReimbursementCostSettings
	var e error
	out.Enabled, e = boolEnv("SETTLEMENT_REIMBURSEMENT_COST_ENABLED", false)
	if e != nil {
		return out, e
	}
	if !out.Enabled {
		return out, nil
	}
	for key, dest := range map[string]*string{"URL": &out.URL, "TOKEN_URL": &out.TokenURL, "CLIENT_ID": &out.ClientID, "CLIENT_SECRET": &out.ClientSecret, "SCOPE": &out.Scope, "TENANT_ID": &out.TenantID, "ENVIRONMENT": &out.Environment} {
		*dest = os.Getenv("SETTLEMENT_REIMBURSEMENT_COST_" + key)
		if *dest == "" || strings.TrimSpace(*dest) != *dest {
			return out, fmt.Errorf("missing SETTLEMENT_REIMBURSEMENT_COST_%s", key)
		}
	}
	if out.TenantID != c.OIDCTenantID || out.Environment != c.OIDCEnvironmentCode || out.Scope != "reimbursement.cost_candidate.read" {
		return out, fmt.Errorf("reimbursement cost identity binding mismatch")
	}
	for _, raw := range []string{out.URL, out.TokenURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" {
			return out, fmt.Errorf("invalid reimbursement cost endpoint")
		}
	}
	return out, nil
}

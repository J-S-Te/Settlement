package config

import "testing"

func TestLoadPlatformMachineTokenRequiresCompleteExplicitBinding(t *testing.T) {
	for _, missing := range []string{"PLATFORM_MACHINE_TOKEN_PUBLIC_KEY_PATH", "PLATFORM_MACHINE_TOKEN_ISSUER", "CONTRACT_INTEGRATION_CLIENT_ID", "CONTRACT_INTEGRATION_AUDIENCE", "CONTRACT_INTEGRATION_OAUTH_CLIENT_ID", "CONTRACT_INTEGRATION_APPLICATION_ID", "CONTRACT_INTEGRATION_ENVIRONMENT_ID"} {
		t.Run(missing, func(t *testing.T) {
			setValidProductionEnvironment(t)
			t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")
			t.Setenv("SETTLEMENT_INTEGRATION_ENABLED", "true")
			for key, value := range map[string]string{"PLATFORM_MACHINE_TOKEN_PUBLIC_KEY_PATH": "/test/public.pem", "PLATFORM_MACHINE_TOKEN_ISSUER": "basic-platform", "CONTRACT_INTEGRATION_CLIENT_ID": "contract-prod-settlement-integration", "CONTRACT_INTEGRATION_AUDIENCE": "basic-platform-application", "CONTRACT_INTEGRATION_OAUTH_CLIENT_ID": "oauth-contract", "CONTRACT_INTEGRATION_APPLICATION_ID": "app-contract", "CONTRACT_INTEGRATION_ENVIRONMENT_ID": "env-prod"} {
				t.Setenv(key, value)
			}
			t.Setenv(missing, "")
			if _, err := Load(); err == nil {
				t.Fatal("partial platform machine binding must fail closed")
			}
		})
	}
}

func TestLoadAcceptsCompletePlatformMachineBinding(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")
	t.Setenv("SETTLEMENT_INTEGRATION_ENABLED", "true")
	for key, value := range map[string]string{"PLATFORM_MACHINE_TOKEN_PUBLIC_KEY_PATH": "/test/public.pem", "PLATFORM_MACHINE_TOKEN_ISSUER": "basic-platform", "CONTRACT_INTEGRATION_CLIENT_ID": "contract-prod-settlement-integration", "CONTRACT_INTEGRATION_AUDIENCE": "basic-platform-application", "CONTRACT_INTEGRATION_OAUTH_CLIENT_ID": "oauth-contract", "CONTRACT_INTEGRATION_APPLICATION_ID": "app-contract", "CONTRACT_INTEGRATION_ENVIRONMENT_ID": "env-prod"} {
		t.Setenv(key, value)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ContractOAuthClientID != "oauth-contract" || c.ContractApplicationID != "app-contract" || c.ContractEnvironmentID != "env-prod" || c.PlatformMachineTokenIssuer != "basic-platform" {
		t.Fatal("platform binding not loaded")
	}
}

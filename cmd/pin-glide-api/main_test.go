package main

import (
	"strings"
	"testing"
)

// Identity provider configuration is request-time state, not a startup gate.
// Each matrix entry reaches the first unrelated startup dependency instead of
// failing because a provider is absent or partial.
func TestRunDoesNotRequireIdentityProviderConfiguration(t *testing.T) {
	tests := []struct {
		name         string
		wechatAppID  string
		wechatSecret string
		qqAppID      string
		qqAppKey     string
	}{
		{name: "neither configured"},
		{name: "only wechat", wechatAppID: "wx-app", wechatSecret: "wx-secret"},
		{name: "only qq", qqAppID: "qq-app", qqAppKey: "qq-key"},
		{name: "both configured", wechatAppID: "wx-app", wechatSecret: "wx-secret", qqAppID: "qq-app", qqAppKey: "qq-key"},
		{name: "wechat partial", wechatAppID: "wx-app"},
		{name: "qq partial", qqAppKey: "qq-key"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("PIN_GLIDE_ENV", "production")
			t.Setenv("PIN_GLIDE_DATABASE_URL", "postgres://example@127.0.0.1:1/pin_glide?sslmode=disable")
			t.Setenv("PIN_GLIDE_WECHAT_APP_ID", test.wechatAppID)
			t.Setenv("PIN_GLIDE_WECHAT_APP_SECRET", test.wechatSecret)
			t.Setenv("PIN_GLIDE_QQ_APP_ID", test.qqAppID)
			t.Setenv("PIN_GLIDE_QQ_APP_KEY", test.qqAppKey)
			t.Setenv("PIN_GLIDE_BAILIAN_API_KEY", "parent-key")

			err := run(false, false)
			if err == nil || !strings.Contains(err.Error(), "connect to PostgreSQL") {
				t.Fatalf("run() = %v, want the unrelated database startup failure", err)
			}
			if strings.Contains(err.Error(), "required") {
				t.Fatalf("run() = %v, provider configuration became a startup gate", err)
			}
		})
	}
}

// A production api without the Bailian parent key can serve nothing but
// provider failures, so the process refuses to start and the operator learns
// at deployment time instead of on the first extraction.
func TestRunRequiresBailianCredentialsInProduction(t *testing.T) {
	t.Setenv("PIN_GLIDE_ENV", "production")
	t.Setenv("PIN_GLIDE_DATABASE_URL", "postgres://example@db.internal:5432/pin_glide")
	t.Setenv("PIN_GLIDE_WECHAT_APP_ID", "wx-app")
	t.Setenv("PIN_GLIDE_WECHAT_APP_SECRET", "wx-secret")
	t.Setenv("PIN_GLIDE_BAILIAN_API_KEY", "")

	err := run(false, false)
	if err == nil || !strings.Contains(err.Error(), "PIN_GLIDE_BAILIAN_API_KEY") {
		t.Fatalf("run() = %v, want a missing Bailian credentials error", err)
	}
}

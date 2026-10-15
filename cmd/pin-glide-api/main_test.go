package main

import (
	"strings"
	"testing"
)

// A production api without the WeChat application credentials cannot verify
// any identity, so the process refuses to start instead of failing every login
// later. The migrate process shares the configuration loader and needs neither.
func TestRunRequiresWeChatCredentialsInProduction(t *testing.T) {
	t.Setenv("PIN_GLIDE_ENV", "production")
	t.Setenv("PIN_GLIDE_DATABASE_URL", "postgres://example@db.internal:5432/pin_glide")
	t.Setenv("PIN_GLIDE_WECHAT_APP_ID", "")
	t.Setenv("PIN_GLIDE_WECHAT_APP_SECRET", "")

	err := run(false, false)
	if err == nil || !strings.Contains(err.Error(), "PIN_GLIDE_WECHAT_APP_ID") {
		t.Fatalf("run() = %v, want a missing WeChat credentials error", err)
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

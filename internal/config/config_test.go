package config_test

import (
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/config"
)

func TestLoadDefaultsToDevelopment(t *testing.T) {
	loaded, err := config.Load(func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Environment != config.Development {
		t.Errorf("environment = %q, want %q", loaded.Environment, config.Development)
	}
	if loaded.APIAddress != ":8080" {
		t.Errorf("api address = %q, want :8080", loaded.APIAddress)
	}
	if loaded.DatabaseURL != "postgres://pin_glide:pin_glide@127.0.0.1:5432/pin_glide?sslmode=disable" {
		t.Errorf("database URL = %q", loaded.DatabaseURL)
	}
}

func TestLoadRejectsUnknownEnvironment(t *testing.T) {
	_, err := config.Load(func(name string) string {
		if name == "PIN_GLIDE_ENV" {
			return "staging"
		}
		return ""
	})
	if err == nil {
		t.Fatal("Load accepted an unknown environment")
	}
}

func TestLoadRequiresDatabaseURLInProduction(t *testing.T) {
	_, err := config.Load(func(name string) string {
		if name == "PIN_GLIDE_ENV" {
			return "production"
		}
		return ""
	})
	if err == nil {
		t.Fatal("Load accepted production without a database URL")
	}
}

// Neither identity provider is required at startup; an operation that uses an
// unavailable provider reports that only when invoked.
func TestLoadAllowsProductionWithoutIdentityProviders(t *testing.T) {
	loaded, err := config.Load(func(name string) string {
		switch name {
		case "PIN_GLIDE_ENV":
			return "production"
		case "PIN_GLIDE_DATABASE_URL":
			return "postgres://example@db.internal:5432/pin_glide"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("Load rejected production configuration without identity providers: %v", err)
	}
	if loaded.WeChatAppID != "" || loaded.WeChatAppSecret != "" ||
		loaded.QQAppID != "" || loaded.QQAppKey != "" || loaded.BailianAPIKey != "" {
		t.Errorf("provider credentials = %q/%q/%q/%q/%q, want absent",
			loaded.WeChatAppID, loaded.WeChatAppSecret,
			loaded.QQAppID, loaded.QQAppKey, loaded.BailianAPIKey)
	}
}

func TestLoadKeepsExplicitValues(t *testing.T) {
	values := map[string]string{
		"PIN_GLIDE_ENV":               " production ",
		"PIN_GLIDE_API_ADDRESS":       " 127.0.0.1:9090 ",
		"PIN_GLIDE_DATABASE_URL":      "postgres://example@db.internal:5432/pin_glide",
		"PIN_GLIDE_WECHAT_APP_ID":     " wx-app ",
		"PIN_GLIDE_WECHAT_APP_SECRET": " wx-secret ",
		"PIN_GLIDE_QQ_APP_ID":         " qq-app ",
		"PIN_GLIDE_QQ_APP_KEY":        " qq-key ",
		"PIN_GLIDE_BAILIAN_API_KEY":   " sk-parent ",
	}
	loaded, err := config.Load(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Environment != config.Production {
		t.Errorf("environment = %q, want %q", loaded.Environment, config.Production)
	}
	if loaded.APIAddress != "127.0.0.1:9090" {
		t.Errorf("api address = %q", loaded.APIAddress)
	}
	if loaded.DatabaseURL != values["PIN_GLIDE_DATABASE_URL"] {
		t.Errorf("database URL = %q", loaded.DatabaseURL)
	}
	if loaded.WeChatAppID != "wx-app" || loaded.WeChatAppSecret != "wx-secret" {
		t.Errorf("wechat credentials = %q/%q, want the configured values", loaded.WeChatAppID, loaded.WeChatAppSecret)
	}
	if loaded.QQAppID != "qq-app" || loaded.QQAppKey != "qq-key" {
		t.Errorf("qq credentials = %q/%q, want the configured values", loaded.QQAppID, loaded.QQAppKey)
	}
	if loaded.BailianAPIKey != "sk-parent" {
		t.Errorf("bailian API key = %q, want the configured value", loaded.BailianAPIKey)
	}
}

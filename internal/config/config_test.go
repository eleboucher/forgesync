package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "forgesync.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadYAML(t *testing.T) {
	t.Setenv("FORGESYNC_SOURCE_URL", "")
	t.Setenv("FORGESYNC_SOURCE_TOKEN", "")
	t.Setenv("FORGESYNC_BOT_USERNAME", "")

	path := writeYAML(t, `
source:
  url: https://forgejo.example.com
  token: abc123
pollInterval: 1m
bot:
  username: botty
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Source.URL != "https://forgejo.example.com" {
		t.Errorf("URL: %q", cfg.Source.URL)
	}
	if cfg.PollInterval != time.Minute {
		t.Errorf("PollInterval: %v", cfg.PollInterval)
	}
	if cfg.HealthListen != ":8080" {
		t.Errorf("HealthListen default: %q", cfg.HealthListen)
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	t.Setenv("FORGESYNC_SOURCE_URL", "https://override.example.com")
	t.Setenv("FORGESYNC_SOURCE_TOKEN", "envtoken")
	t.Setenv("FORGESYNC_BOT_USERNAME", "envbot")

	path := writeYAML(t, `
source:
  url: https://yaml.example.com
  token: yamltoken
pollInterval: 1m
bot:
  username: yamlbot
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Source.URL != "https://override.example.com" {
		t.Errorf("env did not override: %q", cfg.Source.URL)
	}
	if cfg.Source.Token != "envtoken" {
		t.Errorf("token: %q", cfg.Source.Token)
	}
	if cfg.Bot.Username != "envbot" {
		t.Errorf("bot: %q", cfg.Bot.Username)
	}
}

func TestEnvOnlyNoFile(t *testing.T) {
	t.Setenv("FORGESYNC_SOURCE_URL", "https://forgejo.example.com")
	t.Setenv("FORGESYNC_SOURCE_TOKEN", "tok")
	t.Setenv("FORGESYNC_BOT_USERNAME", "bot")
	t.Setenv("FORGESYNC_POLL_INTERVAL", "30s")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollInterval != 30*time.Second {
		t.Errorf("PollInterval: %v", cfg.PollInterval)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"missing url", "source:\n  token: t\nbot:\n  username: b\n"},
		{"missing token", "source:\n  url: https://f\nbot:\n  username: b\n"},
		{"bad scheme", "source:\n  url: ftp://x\n  token: t\nbot:\n  username: b\n"},
		{"missing bot", "source:\n  url: https://f\n  token: t\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FORGESYNC_SOURCE_URL", "")
			t.Setenv("FORGESYNC_SOURCE_TOKEN", "")
			t.Setenv("FORGESYNC_BOT_USERNAME", "")

			path := writeYAML(t, tc.yaml)
			if _, err := Load(path); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestWindowIsTwicePollInterval(t *testing.T) {
	c := &Config{PollInterval: 2 * time.Minute}
	if c.Window() != 4*time.Minute {
		t.Errorf("Window: %v", c.Window())
	}
}

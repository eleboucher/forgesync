package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Source          ForgejoEndpoint `yaml:"source"           envPrefix:"FORGESYNC_SOURCE_"`
	PollInterval    time.Duration   `yaml:"pollInterval"     env:"FORGESYNC_POLL_INTERVAL"`
	InitialBackfill time.Duration   `yaml:"initialBackfill"  env:"FORGESYNC_INITIAL_BACKFILL"`
	HealthListen    string          `yaml:"healthListen"     env:"FORGESYNC_HEALTH_LISTEN"`
	LogLevel        string          `yaml:"logLevel"         env:"FORGESYNC_LOG_LEVEL"`
	LogFormat       string          `yaml:"logFormat"        env:"FORGESYNC_LOG_FORMAT"`
	Bot             Bot             `yaml:"bot"              envPrefix:"FORGESYNC_BOT_"`
	Targets         Targets         `yaml:"targets"`
}

type ForgejoEndpoint struct {
	URL   string `yaml:"url"   env:"URL"`
	Token string `yaml:"token" env:"TOKEN"`
}

type Bot struct {
	Username string `yaml:"username" env:"USERNAME"`
}

type Targets struct {
	GitHub GitHubTarget `yaml:"github" envPrefix:"FORGESYNC_GITHUB_"`
}

type GitHubTarget struct {
	Token string `yaml:"token" env:"TOKEN"`
}

// Load reads YAML from path (if it exists), then layers env vars on top, then
// applies defaults and validates. An empty path skips the file step.
func Load(path string) (*Config, error) {
	cfg := &Config{}

	if path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // operator-provided path
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config: %w", err)
			}
		case errors.Is(err, os.ErrNotExist):
			// Fall through to env-only.
		default:
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parse env: %w", err)
	}

	applyDefaults(cfg)
	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

func applyDefaults(c *Config) {
	if c.PollInterval == 0 {
		c.PollInterval = 5 * time.Minute
	}
	if c.InitialBackfill == 0 {
		c.InitialBackfill = 1 * time.Hour
	}
	if c.HealthListen == "" {
		c.HealthListen = ":8080"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.LogFormat == "" {
		c.LogFormat = "text"
	}
}

func validate(c *Config) error {
	if c.Source.URL == "" {
		return errors.New("source.url (or FORGESYNC_SOURCE_URL) is required")
	}
	u, err := url.Parse(c.Source.URL)
	if err != nil {
		return fmt.Errorf("source.url invalid: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("source.url must be http(s), got %q", u.Scheme)
	}
	if c.Source.Token == "" {
		return errors.New("source.token (or FORGESYNC_SOURCE_TOKEN) is required")
	}
	if c.Bot.Username == "" {
		return errors.New("bot.username (or FORGESYNC_BOT_USERNAME) is required")
	}
	if c.PollInterval <= 0 {
		return errors.New("pollInterval must be > 0")
	}
	return nil
}

// Window is the look-back used when polling sources, generous enough to absorb
// missed ticks and clock skew.
func (c *Config) Window() time.Duration {
	return 2 * c.PollInterval
}

package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a YAML duration string such as "48h" backed by time.Duration.
type Duration time.Duration

// UnmarshalYAML parses a duration string.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(strings.TrimSpace(node.Value))
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", node.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

// Config is the validated runtime configuration.
type Config struct {
	Telegram    TelegramConfig    `yaml:"telegram"`
	Database    DatabaseConfig    `yaml:"database"`
	Moderation  ModerationConfig  `yaml:"moderation"`
	Metadata    MetadataConfig    `yaml:"metadata"`
	Operational OperationalConfig `yaml:"operational"`
}

// OperationalConfig configures optional local health endpoints.
type OperationalConfig struct {
	HealthListen string `yaml:"health_listen"`
}

// TelegramConfig configures the Telegram Bot API connection and chat scope.
type TelegramConfig struct {
	Token string `yaml:"token"`
}

// DatabaseConfig configures durable state.
type DatabaseConfig struct {
	Path string `yaml:"path"`
}

// ModerationConfig configures policy timing and language.
type ModerationConfig struct {
	NewcomerSandbox Duration `yaml:"newcomer_sandbox"`
	NoticeLanguage  string   `yaml:"notice_language"`
	NoticeCatalog   string   `yaml:"notice_catalog"`
}

// MetadataConfig configures safe HTTP retrieval.
type MetadataConfig struct {
	RequestTimeout Duration `yaml:"request_timeout"`
	TotalTimeout   Duration `yaml:"total_timeout"`
	MaxRedirects   int      `yaml:"max_redirects"`
	MaxHTMLBytes   int64    `yaml:"max_html_bytes"`
	UserAgent      string   `yaml:"user_agent"`
}

// Defaults returns the initial operational defaults.
func Defaults() Config {
	return Config{
		Moderation: ModerationConfig{NewcomerSandbox: Duration(48 * time.Hour), NoticeLanguage: "fi"},
		Metadata: MetadataConfig{
			RequestTimeout: Duration(5 * time.Second),
			TotalTimeout:   Duration(10 * time.Second),
			MaxRedirects:   5,
			MaxHTMLBytes:   2 << 20,
			UserAgent:      "linkkilinko/0.1",
		},
	}
}

// Load reads, defaults, resolves the token, and validates a YAML config file.
func Load(path string) (Config, error) {
	config := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if config.Telegram.Token == "" {
		config.Telegram.Token = os.Getenv("LINKKILINKO_TELEGRAM_TOKEN")
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate checks required fields and hard safety ceilings.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Telegram.Token) == "" {
		return errors.New("config: telegram token is empty")
	}
	if strings.TrimSpace(c.Database.Path) == "" {
		return errors.New("config: database path is empty")
	}
	if time.Duration(c.Moderation.NewcomerSandbox) <= 0 {
		return errors.New("config: newcomer_sandbox must be positive")
	}
	if strings.TrimSpace(c.Moderation.NoticeLanguage) == "" {
		return errors.New("config: notice_language is empty")
	}
	if time.Duration(c.Metadata.RequestTimeout) <= 0 || time.Duration(c.Metadata.TotalTimeout) <= 0 {
		return errors.New("config: metadata timeouts must be positive")
	}
	if time.Duration(c.Metadata.RequestTimeout) >= time.Duration(c.Metadata.TotalTimeout) {
		return errors.New("config: request_timeout must be less than total_timeout")
	}
	if time.Duration(c.Metadata.RequestTimeout) > time.Minute || time.Duration(c.Metadata.TotalTimeout) > 2*time.Minute {
		return errors.New("config: metadata timeout exceeds safety ceiling")
	}
	if c.Metadata.MaxRedirects < 0 || c.Metadata.MaxRedirects > 10 {
		return errors.New("config: max_redirects must be between 0 and 10")
	}
	if c.Metadata.MaxHTMLBytes <= 0 || c.Metadata.MaxHTMLBytes > 16<<20 {
		return errors.New("config: max_html_bytes must be between 1 and 16777216")
	}
	if strings.TrimSpace(c.Metadata.UserAgent) == "" {
		return errors.New("config: user_agent is empty")
	}
	return nil
}

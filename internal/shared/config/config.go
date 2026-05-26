// Package config loads application configuration from a YAML file with
// environment-variable overrides (12-factor). Env vars use the APP_ prefix,
// e.g. APP_DB_DSN overrides db.dsn.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Env    string       `mapstructure:"env"`
	HTTP   HTTPConfig   `mapstructure:"http"`
	DB     DBConfig     `mapstructure:"db"`
	Outbox OutboxConfig `mapstructure:"outbox"`
	Log    LogConfig    `mapstructure:"log"`
}

type HTTPConfig struct {
	Port            int           `mapstructure:"port"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

type DBConfig struct {
	DSN             string        `mapstructure:"dsn"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	AutoMigrate     bool          `mapstructure:"auto_migrate"`
}

type OutboxConfig struct {
	PollInterval time.Duration `mapstructure:"poll_interval"`
	BatchSize    int           `mapstructure:"batch_size"`
	// MaxAttempts is how many times we retry a failing event before moving
	// it to outbox_dead. Counts the initial delivery too.
	MaxAttempts int `mapstructure:"max_attempts"`
	// BaseBackoff is the first retry delay; subsequent retries grow
	// exponentially up to MaxBackoff, with ±25% jitter.
	BaseBackoff time.Duration `mapstructure:"base_backoff"`
	MaxBackoff  time.Duration `mapstructure:"max_backoff"`
}

type LogConfig struct {
	Level string `mapstructure:"level"`
}

// Load reads configuration from path and applies APP_* env overrides.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	setDefaults(v)

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	v.SetEnvPrefix("APP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("env", "local")
	v.SetDefault("http.port", 8080)
	v.SetDefault("http.shutdown_timeout", "10s")
	v.SetDefault("db.max_open_conns", 25)
	v.SetDefault("db.max_idle_conns", 25)
	v.SetDefault("db.conn_max_lifetime", "30m")
	v.SetDefault("db.auto_migrate", true)
	v.SetDefault("outbox.poll_interval", "2s")
	v.SetDefault("outbox.batch_size", 100)
	v.SetDefault("outbox.max_attempts", 5)
	v.SetDefault("outbox.base_backoff", "1s")
	v.SetDefault("outbox.max_backoff", "1m")
	v.SetDefault("log.level", "info")
}

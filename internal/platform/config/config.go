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
	Env       string          `mapstructure:"env"`
	HTTP      HTTPConfig      `mapstructure:"http"`
	DB        DBConfig        `mapstructure:"db"`
	Outbox    OutboxConfig    `mapstructure:"outbox"`
	RabbitMQ  RabbitMQConfig  `mapstructure:"rabbitmq"`
	Redis     RedisConfig     `mapstructure:"redis"`
	Consumers ConsumersConfig `mapstructure:"consumers"`
	Log       LogConfig       `mapstructure:"log"`
	// FeatureFlags is the static initial set loaded into the in-memory
	// featureflags.Provider at startup. Values are typed dynamically
	// (bool / string / number); see platform/featureflags for accessors.
	FeatureFlags map[string]any `mapstructure:"feature_flags"`
}

// ConsumersConfig tunes every RabbitMQ consumer Subscriber the app builds.
// Per-topic overrides are wired in code at the composition root via the
// consumers.Config map — keeping YAML schema simple (no nested per-module
// maps).
type ConsumersConfig struct {
	// DefaultConcurrency caps in-flight handlers per (consumer, topic).
	// Also sets AMQP prefetch — broker stops delivering once N are in
	// flight, so one slow topic doesn't starve others.
	DefaultConcurrency int `mapstructure:"default_concurrency"`
	// HandlerTimeout bounds a single handler invocation.
	HandlerTimeout time.Duration `mapstructure:"handler_timeout"`
	// QueueDepthPollInterval is how often consumer_queue_depth gauge is
	// refreshed via passive QueueDeclare.
	QueueDepthPollInterval time.Duration `mapstructure:"queue_depth_poll_interval"`
}

// RedisConfig: empty DSN disables every Redis-backed feature (e.g.
// idempotency middleware becomes a passthrough). Set to e.g.
// "redis://localhost:6379/0" to enable.
type RedisConfig struct {
	DSN string `mapstructure:"dsn"`
}

type RabbitMQConfig struct {
	DSN      string `mapstructure:"dsn"`
	Exchange string `mapstructure:"exchange"`
	DLX      string `mapstructure:"dlx"`
}

type HTTPConfig struct {
	Port int `mapstructure:"port"`
	// RequestTimeout is the deadline applied to every incoming request via
	// the timeout middleware. Handlers and services using c.UserContext()
	// will see ctx cancel after this duration; pair with statement_timeout
	// at the DB level so even ctx-ignoring code eventually unblocks.
	RequestTimeout  time.Duration `mapstructure:"request_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

type DBConfig struct {
	DSN             string        `mapstructure:"dsn"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	// StatementTimeout is applied as a server-side guarantee via the
	// postgres `statement_timeout` parameter. postgres.Connect appends it
	// to the DSN at startup; cmd/migrate bypasses this (uses raw DSN) so
	// long migrations are not killed.
	StatementTimeout time.Duration `mapstructure:"statement_timeout"`
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
	v.SetDefault("http.request_timeout", "10s")
	v.SetDefault("http.shutdown_timeout", "15s")
	v.SetDefault("db.max_open_conns", 25)
	v.SetDefault("db.max_idle_conns", 25)
	v.SetDefault("db.conn_max_lifetime", "30m")
	v.SetDefault("db.statement_timeout", "5s")
	v.SetDefault("outbox.poll_interval", "2s")
	v.SetDefault("outbox.batch_size", 100)
	v.SetDefault("outbox.max_attempts", 5)
	v.SetDefault("outbox.base_backoff", "1s")
	v.SetDefault("outbox.max_backoff", "1m")
	v.SetDefault("rabbitmq.exchange", "events")
	v.SetDefault("rabbitmq.dlx", "events.dlx")
	v.SetDefault("consumers.default_concurrency", 4)
	v.SetDefault("consumers.handler_timeout", "30s")
	v.SetDefault("consumers.queue_depth_poll_interval", "30s")
	v.SetDefault("log.level", "info")
}

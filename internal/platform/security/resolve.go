package security

import (
	"context"
	"fmt"
	"strings"

	"github.com/monorepo/internal/platform/config"
)

// secretPrefix tags a config string as a reference to be resolved through
// the SecretsProvider. Values not starting with the prefix pass through.
const secretPrefix = "secret:"

// ResolveSecrets walks the known secret-bearing config fields and
// substitutes any "secret:NAME" reference with the value returned by p.
// Non-prefixed values are left untouched — the pattern is opt-in per
// field.
//
// The field list is explicit (not reflection): adding a new secret-bearing
// config field is one line here, and the resolver remains greppable.
func ResolveSecrets(ctx context.Context, cfg *config.Config, p SecretsProvider) error {
	fields := map[string]*string{
		"db.dsn":       &cfg.DB.DSN,
		"rabbitmq.dsn": &cfg.RabbitMQ.DSN,
		"redis.dsn":    &cfg.Redis.DSN,
	}
	for name, ptr := range fields {
		if !strings.HasPrefix(*ptr, secretPrefix) {
			continue
		}
		ref := strings.TrimPrefix(*ptr, secretPrefix)
		val, err := p.Get(ctx, ref)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", name, err)
		}
		*ptr = val
	}
	return nil
}

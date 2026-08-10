// Package config loads email-provisioner's runtime configuration.
package config

import (
	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
)

// Config is email-provisioner's full runtime configuration. This service is
// deliberately a "dumb bridge" (see .claude/plans/codevertex-email-hosting-service-plan.md
// Part 3) — it owns no database of its own, only NATS consumption + calls out to
// Stalwart's REST API and to subscriptions-api for entitlement lookups.
type Config struct {
	Port int `envconfig:"PORT" default:"8080"`

	EventsNATSURL string `envconfig:"EVENTS_NATS_URL" required:"true"`

	RedisAddr string `envconfig:"REDIS_ADDR" required:"true"`

	StalwartAdminURL      string `envconfig:"STALWART_ADMIN_URL" required:"true"`
	StalwartAdminUser     string `envconfig:"STALWART_ADMIN_USER" default:"admin"`
	StalwartAdminPassword string `envconfig:"STALWART_ADMIN_PASSWORD" required:"true"`

	SubscriptionsAPIBaseURL string `envconfig:"SUBSCRIPTION_BASE_URL" required:"true"`
	InternalServiceKey      string `envconfig:"INTERNAL_SERVICE_KEY" required:"true"`

	SecurityJWKSURL string `envconfig:"SECURITY_JWKS_URL"`
}

// Load reads config from the environment (and a local .env in dev).
func Load() (*Config, error) {
	_ = godotenv.Load()

	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

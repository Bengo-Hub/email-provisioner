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

	// RedisAddr: declared for the Part 3E caching layer, not yet wired into any
	// code path — not required until a Redis client actually exists.
	RedisAddr string `envconfig:"REDIS_ADDR"`

	StalwartAdminURL      string `envconfig:"STALWART_ADMIN_URL" required:"true"`
	StalwartAdminUser     string `envconfig:"STALWART_ADMIN_USER" default:"admin"`
	StalwartAdminPassword string `envconfig:"STALWART_ADMIN_PASSWORD" required:"true"`

	// Credential-delivery (plan §13.2's flagged gap): rather than ever putting
	// Stalwart's own randomly-generated initial mailbox secret on the wire
	// (NATS is at-least-once/replayable, email isn't much better), a freshly
	// provisioned mailbox gets a signed, time-limited "choose your password"
	// link instead. ProvisioningTokenSecret must match mail-ui's own copy —
	// see mail-ui/src/lib/setup-token.ts, which verifies the same HMAC.
	//
	// Deliberately NOT required: this is a new, additive feature — a
	// not-yet-provisioned secret must degrade to "setup links aren't sent
	// yet" (see the empty-secret guard in modules/events/license_events.go),
	// never to "the whole bridge won't start." A required field here once
	// crash-looped this service in production the moment this code deployed
	// ahead of the Secret existing (2026-08-18) — do not reintroduce that
	// failure mode by making any new, non-critical config required.
	ProvisioningTokenSecret string `envconfig:"PROVISIONING_TOKEN_SECRET"`
	MailUIBaseURL           string `envconfig:"MAIL_UI_BASE_URL" default:"https://webmail.codevertexafrica.com"`
	PlatformFromEmail       string `envconfig:"PLATFORM_FROM_EMAIL" default:"no-reply@codevertexafrica.com"`

	// SubscriptionsAPIBaseURL / InternalServiceKey: declared for the future
	// entitlement-lookup S2S calls (Part 3E), not yet called anywhere — not
	// required until that client actually exists.
	SubscriptionsAPIBaseURL string `envconfig:"SUBSCRIPTION_BASE_URL"`
	InternalServiceKey      string `envconfig:"INTERNAL_SERVICE_KEY"`

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

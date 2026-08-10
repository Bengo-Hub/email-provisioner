module github.com/bengobox/email-provisioner

go 1.26.0

// Map module path to correct GitHub repository, matching every other Go service.
replace github.com/Bengo-Hub/shared-auth-client => github.com/Bengo-Hub/auth-client v0.10.0

require (
	github.com/Bengo-Hub/shared-events v0.6.1
	github.com/go-chi/chi/v5 v5.0.12
	github.com/joho/godotenv v1.5.1
	github.com/kelseyhightower/envconfig v1.4.0
	github.com/nats-io/nats.go v1.52.0
	go.uber.org/zap v1.27.1
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/crypto v0.49.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)

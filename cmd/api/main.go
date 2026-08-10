// Command email-provisioner bridges subscriptions-api's license lifecycle to
// Stalwart Mail Server mailbox provisioning. See
// .claude/plans/codevertex-email-hosting-service-plan.md Part 3E.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/bengobox/email-provisioner/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx)
	if err != nil {
		log.Fatalf("failed to initialise app: %v", err)
	}
	defer a.Close()

	if err := a.Run(ctx); err != nil {
		log.Fatalf("runtime error: %v", err)
	}
}

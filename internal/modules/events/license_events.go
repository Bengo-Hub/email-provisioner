// Package events wires email-provisioner's NATS consumers. Per plan Part 3E,
// this service is a deliberately dumb bridge: it only ever consumes
// email.license.* (published by subscriptions-api) and auth.user.deactivated
// (published by auth-api), and only ever calls Stalwart's REST API in response.
// All licensing/billing business logic stays in subscriptions-api.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/email-provisioner/internal/platform/stalwart"
	"github.com/bengobox/email-provisioner/internal/platform/token"
)

// setupTokenTTL bounds how long a "choose your password" link stays valid —
// long enough for a tenant admin to forward it, short enough that a stale
// unused link isn't a standing risk.
const setupTokenTTL = 72 * time.Hour

// Handler dispatches email.license.* and auth.user.deactivated events to the
// Stalwart client, and publishes email.mailbox.* outbound events for others
// to react to (plan Part 3E). email-provisioner owns no database, so these
// are plain at-least-once NATS publishes, not the transactional outbox
// pattern the DB-backed services use — there's no local transaction to tie
// them to.
type Handler struct {
	log      *zap.Logger
	stalwart *stalwart.Client
	nats     *nats.Conn

	// Credential-delivery config — see config.go's ProvisioningTokenSecret
	// comment for why a signed link, not a raw password, is what gets sent.
	tokenSecret   string
	mailUIBaseURL string
	fromEmail     string
}

// New builds the event handler. tokenSecret/mailUIBaseURL/fromEmail wire the
// credential-delivery flow (plan §13.2's flagged gap) — a freshly
// provisioned mailbox's owner gets a signed "choose your password" link,
// never the Stalwart-generated random initial secret itself.
func New(log *zap.Logger, stalwartClient *stalwart.Client, tokenSecret, mailUIBaseURL, fromEmail string) *Handler {
	return &Handler{
		log:           log,
		stalwart:      stalwartClient,
		tokenSecret:   tokenSecret,
		mailUIBaseURL: mailUIBaseURL,
		fromEmail:     fromEmail,
	}
}

// Subscribe registers all queue subscriptions on the given NATS connection.
// Uses core QueueSubscribe (not JetStream) — subscriptions-api's outbox pattern
// provides at-least-once delivery; handlers below are idempotent (Stalwart
// operations tolerate "already exists"/"already disabled").
func (h *Handler) Subscribe(conn *nats.Conn) error {
	h.nats = conn
	if _, err := sharedevents.QueueSubscribe(h.log, conn, "email.license.>", "mail-license-events", h.handleLicenseEvent); err != nil {
		return err
	}
	if _, err := sharedevents.QueueSubscribe(h.log, conn, "auth.user.deactivated", "mail-user-deactivated", h.handleUserDeactivated); err != nil {
		return err
	}
	return nil
}

// publishMailboxEvent emits an email.mailbox.* event. Best-effort: a failed
// publish is logged, never fatal to the caller's own Stalwart operation,
// which has already succeeded or failed on its own terms by this point.
func (h *Handler) publishMailboxEvent(eventType string, tenantID uuid.UUID, email string, extra map[string]any) {
	if h.nats == nil {
		return
	}
	payload := map[string]any{"email": email}
	for k, v := range extra {
		payload[k] = v
	}
	ev := sharedevents.NewEvent(eventType, "email", uuid.New(), tenantID, payload)
	data, err := ev.ToJSON()
	if err != nil {
		h.log.Error("marshal outbound mailbox event failed", zap.String("event_type", eventType), zap.Error(err))
		return
	}
	if err := h.nats.Publish(eventType, data); err != nil {
		h.log.Error("publish outbound mailbox event failed", zap.String("event_type", eventType), zap.Error(err))
	}
}

// licensePayload is the subset of EmailLicense/EmailPlan fields email-provisioner
// needs from the event payload — subscriptions-api denormalizes these at
// publish time so this service never needs to call back for them synchronously.
type licensePayload struct {
	Email               string `json:"assigned_to_email"`
	Domain              string `json:"domain"`
	StorageQuotaGB      int    `json:"storage_quota_gb"`
	SuspendReason       string `json:"suspend_reason"`
	MaxSendsPerDay      int    `json:"max_sends_per_day"`
	MaxSendsPerHour     int    `json:"max_sends_per_hour"`
	MaxSendsPerMinute   int    `json:"max_sends_per_minute"`
	MaxRecipientsPerDay int    `json:"max_recipients_per_day"`
	// NotifyEmail is the tenant admin's own already-reachable address,
	// captured at assign time (subscriptions-api's email_handler.go, stored
	// in EmailLicense.metadata) — deliberately NOT the new mailbox address
	// itself, which its owner can't read yet.
	NotifyEmail string `json:"notify_email"`
}

func (h *Handler) handleLicenseEvent(msg *nats.Msg) {
	ctx := context.Background()

	ev, err := sharedevents.FromJSON(msg.Data)
	if err != nil {
		h.log.Error("decode email.license event failed", zap.Error(err))
		return
	}

	var p licensePayload
	if b, err := json.Marshal(ev.Payload); err == nil {
		_ = json.Unmarshal(b, &p)
	}

	if p.Email == "" {
		h.log.Warn("email.license event missing assigned_to_email, skipping",
			zap.String("event_type", ev.EventType), zap.String("event_id", ev.ID.String()))
		return
	}

	switch ev.EventType {
	case "license.assigned":
		spec := stalwart.MailboxSpec{
			Email:               p.Email,
			Domain:              p.Domain,
			QuotaBytes:          int64(p.StorageQuotaGB) * 1024 * 1024 * 1024,
			MaxSendsPerDay:      p.MaxSendsPerDay,
			MaxSendsPerHour:     p.MaxSendsPerHour,
			MaxSendsPerMinute:   p.MaxSendsPerMinute,
			MaxRecipientsPerDay: p.MaxRecipientsPerDay,
		}
		secret, err := h.stalwart.CreateMailbox(ctx, spec)
		if err != nil {
			h.log.Error("provision mailbox failed", zap.String("email", p.Email), zap.Error(err))
			h.publishMailboxEvent("email.mailbox.provision_failed", ev.TenantID, p.Email, map[string]any{
				"error": err.Error(),
			})
			return
		}
		if secret == "" {
			h.log.Info("mailbox already provisioned, skipping", zap.String("email", p.Email))
			return
		}
		// The initial secret is intentionally NOT included in this event's
		// payload — NATS events are at-least-once and can be replayed/logged
		// by more than one consumer, which is the wrong exposure surface for
		// a raw credential. Delivering it to the end user is handled below
		// instead, via a signed one-time "choose your password" link — the
		// user picks their own password, so the random secret Stalwart just
		// generated is never revealed to anyone at all.
		h.publishMailboxEvent("email.mailbox.provisioned", ev.TenantID, p.Email, nil)
		h.log.Info("mailbox provisioned", zap.String("email", p.Email))

		if p.NotifyEmail != "" {
			h.sendSetupNotification(ctx, ev.TenantID, p.Email, p.NotifyEmail)
		} else {
			h.log.Warn("mailbox provisioned with no notify_email — no setup link sent, owner has no known password yet",
				zap.String("email", p.Email))
		}

	case "license.upgraded":
		if err := h.stalwart.UpdateQuota(ctx, p.Email, int64(p.StorageQuotaGB)*1024*1024*1024); err != nil {
			h.log.Error("upgrade mailbox quota failed", zap.String("email", p.Email), zap.Error(err))
		}

	case "license.unassigned":
		if err := h.stalwart.SuspendMailbox(ctx, p.Email); err != nil {
			h.log.Error("disable mailbox on unassign failed", zap.String("email", p.Email), zap.Error(err))
		}

	case "license.suspended":
		if err := h.stalwart.SuspendMailbox(ctx, p.Email); err != nil {
			h.log.Error("suspend mailbox failed", zap.String("email", p.Email),
				zap.String("reason", p.SuspendReason), zap.Error(err))
			return
		}
		h.log.Info("mailbox suspended", zap.String("email", p.Email), zap.String("reason", p.SuspendReason))

	case "license.expired":
		if err := h.stalwart.ArchiveMailbox(ctx, p.Email); err != nil {
			h.log.Error("archive mailbox on expiry failed", zap.String("email", p.Email), zap.Error(err))
		}

	default:
		h.log.Debug("unhandled email.license event type", zap.String("event_type", ev.EventType))
	}
}

// sendSetupNotification delivers a signed, time-limited "choose your
// password" link for a freshly provisioned mailbox to its owner's existing,
// already-reachable contact address. Best-effort: a delivery failure here
// doesn't unwind the mailbox that was already successfully created — it's
// logged so it can be re-sent manually, matching this bridge's existing
// error-handling style elsewhere in this file.
func (h *Handler) sendSetupNotification(ctx context.Context, tenantID uuid.UUID, mailboxEmail, notifyEmail string) {
	setupToken := token.GenerateSetupToken(h.tokenSecret, mailboxEmail, setupTokenTTL)
	setupURL := fmt.Sprintf("%s/setup?token=%s", h.mailUIBaseURL, setupToken)

	subject := "Your Codevertex mailbox is ready"
	body := fmt.Sprintf(
		"Your new mailbox %s has been created.\n\n"+
			"Choose your password here (link expires in 72 hours):\n%s\n\n"+
			"If you weren't expecting this, you can ignore this message.",
		mailboxEmail, setupURL,
	)

	if err := h.stalwart.SendSystemEmail(ctx, h.fromEmail, notifyEmail, subject, body); err != nil {
		h.log.Error("send mailbox setup notification failed",
			zap.String("mailbox", mailboxEmail), zap.String("notify_email", notifyEmail), zap.Error(err))
		h.publishMailboxEvent("email.mailbox.setup_notification_failed", tenantID, mailboxEmail, map[string]any{
			"notify_email": notifyEmail,
			"error":        err.Error(),
		})
		return
	}
	h.log.Info("mailbox setup notification sent", zap.String("mailbox", mailboxEmail), zap.String("notify_email", notifyEmail))
}

func (h *Handler) handleUserDeactivated(msg *nats.Msg) {
	ctx := context.Background()

	ev, err := sharedevents.FromJSON(msg.Data)
	if err != nil {
		h.log.Error("decode auth.user.deactivated event failed", zap.Error(err))
		return
	}

	email, _ := ev.Payload["email"].(string)
	if email == "" {
		return
	}

	// Security/offboarding lockout — independent of and faster than any
	// billing-driven suspension. No-op if this user never had a mailbox.
	if err := h.stalwart.SuspendMailbox(ctx, email); err != nil {
		h.log.Warn("suspend mailbox on user deactivation failed (may not have a mailbox)",
			zap.String("email", email), zap.Error(err))
	}
}

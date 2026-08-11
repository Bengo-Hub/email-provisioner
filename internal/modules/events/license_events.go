// Package events wires email-provisioner's NATS consumers. Per plan Part 3E,
// this service is a deliberately dumb bridge: it only ever consumes
// email.license.* (published by subscriptions-api) and auth.user.deactivated
// (published by auth-api), and only ever calls Stalwart's REST API in response.
// All licensing/billing business logic stays in subscriptions-api.
package events

import (
	"context"
	"encoding/json"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/email-provisioner/internal/platform/stalwart"
)

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
}

// New builds the event handler.
func New(log *zap.Logger, stalwartClient *stalwart.Client) *Handler {
	return &Handler{log: log, stalwart: stalwartClient}
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
		// a raw credential. Delivering it to the end user (a "set your
		// password" link via notifications-api) is separate, not-yet-built
		// work — this event only announces that provisioning succeeded.
		h.publishMailboxEvent("email.mailbox.provisioned", ev.TenantID, p.Email, nil)
		h.log.Info("mailbox provisioned", zap.String("email", p.Email))

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

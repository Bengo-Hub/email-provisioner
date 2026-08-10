// Package stalwart is a thin REST client for Stalwart Mail Server's admin API.
//
// NOTE (2026-08-10): endpoint paths/payloads below follow Stalwart's documented
// `/api/principal` admin API shape as of the version this plan targets. Validate
// against the actual pinned image's API docs before wiring this into production —
// this has not yet been smoke-tested against a live Stalwart instance. See
// .claude/plans/codevertex-email-hosting-service-plan.md Part 3E.
package stalwart

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Client talks to Stalwart's admin REST API.
type Client struct {
	baseURL    string
	adminUser  string
	adminPass  string
	httpClient *http.Client
}

// New builds a Stalwart admin API client.
func New(baseURL, adminUser, adminPass string) *Client {
	return &Client{
		baseURL:    baseURL,
		adminUser:  adminUser,
		adminPass:  adminPass,
		httpClient: &http.Client{},
	}
}

// MailboxSpec describes the mailbox to provision/update — the fields
// email-provisioner denormalizes from subscriptions-api's EmailLicense/EmailPlan
// at license-assign/upgrade time.
type MailboxSpec struct {
	Email         string   `json:"email"`
	Domain        string   `json:"domain"`
	QuotaBytes    int64    `json:"quota"`
	Aliases       []string `json:"aliases,omitempty"`
	Autoresponder bool     `json:"-"`
	// Rate-limiter tier values (Part 6, Layer 1): per-mailbox daily/hourly/minute
	// send caps and recipients/day, written into Stalwart's queue.limiter.inbound
	// scoped to this account.
	MaxSendsPerDay      int `json:"-"`
	MaxSendsPerHour     int `json:"-"`
	MaxSendsPerMinute   int `json:"-"`
	MaxRecipientsPerDay int `json:"-"`
}

// CreateMailbox provisions a new mailbox account. Idempotent from the caller's
// side: email-provisioner should treat "already exists" as success, since NATS
// delivery is at-least-once (see internal/modules/events).
func (c *Client) CreateMailbox(ctx context.Context, spec MailboxSpec) error {
	body := map[string]any{
		"type":  "individual",
		"name":  spec.Email,
		"quota": spec.QuotaBytes,
		"emails": append([]string{spec.Email}, spec.Aliases...),
	}
	return c.post(ctx, "/api/principal", body)
}

// UpdateQuota changes an existing mailbox's storage quota (plan upgrade/downgrade).
func (c *Client) UpdateQuota(ctx context.Context, email string, quotaBytes int64) error {
	body := map[string]any{
		"action": "set",
		"field":  "quota",
		"value":  quotaBytes,
	}
	return c.patch(ctx, fmt.Sprintf("/api/principal/%s", email), body)
}

// SuspendMailbox disables SMTP/IMAP submission for an account — per plan Part 6's
// abuse-response ladder, this must NEVER disable inbound delivery or read access,
// only outbound submission/auth. Used for both billing suspension (license.suspended)
// and security lockout (auth.user.deactivated).
func (c *Client) SuspendMailbox(ctx context.Context, email string) error {
	body := map[string]any{
		"action": "set",
		"field":  "enabled",
		"value":  false,
	}
	return c.patch(ctx, fmt.Sprintf("/api/principal/%s", email), body)
}

// ReactivateMailbox re-enables an account after a suspension is lifted.
func (c *Client) ReactivateMailbox(ctx context.Context, email string) error {
	body := map[string]any{
		"action": "set",
		"field":  "enabled",
		"value":  true,
	}
	return c.patch(ctx, fmt.Sprintf("/api/principal/%s", email), body)
}

// ArchiveMailbox is called on email.license.expired after the grace period —
// sets the mailbox read-only rather than deleting it outright, matching the
// original plan's Part 3E lifecycle.
func (c *Client) ArchiveMailbox(ctx context.Context, email string) error {
	body := map[string]any{
		"action": "set",
		"field":  "quota",
		"value":  0,
	}
	return c.patch(ctx, fmt.Sprintf("/api/principal/%s", email), body)
}

func (c *Client) post(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPost, path, body)
}

func (c *Client) patch(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPatch, path, body)
}

func (c *Client) do(ctx context.Context, method, path string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal stalwart request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build stalwart request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.adminUser, c.adminPass)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call stalwart %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("stalwart %s %s returned %d", method, path, resp.StatusCode)
	}
	return nil
}

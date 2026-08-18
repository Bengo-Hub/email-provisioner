// Package stalwart is a JMAP admin client for Stalwart Mail Server.
//
// CORRECTED 2026-08-11: the original version of this file guessed a
// `/api/principal` REST shape (Stalwart's old v0.11.x API). That endpoint
// does not exist on the now-current v0.16.x Stalwart at all — it 404s.
// v0.16.x replaced it with vendor JMAP methods (`x:Account/set`,
// `x:Domain/query`, etc.) under the `urn:stalwart:jmap` capability, called
// via `POST /jmap/`. Schema confirmed live against the running server's own
// self-documenting `GET /api/schema` endpoint — see
// shared-docs/internal/operations/email-hosting-stalwart-setup.md for the
// full discovery notes. Do not reintroduce `/api/principal` calls.
package stalwart

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// Client talks to Stalwart's JMAP admin API.
type Client struct {
	baseURL    string
	adminUser  string
	adminPass  string
	httpClient *http.Client

	mu        sync.Mutex
	domainIDs map[string]string // domain name -> x:Domain id, cached for the process lifetime
}

// New builds a Stalwart admin API client. baseURL is the JMAP HTTP endpoint's
// origin, e.g. "http://stalwart-mail.email.svc.cluster.local:8080".
func New(baseURL, adminUser, adminPass string) *Client {
	return &Client{
		baseURL:    baseURL,
		adminUser:  adminUser,
		adminPass:  adminPass,
		httpClient: &http.Client{},
		domainIDs:  make(map[string]string),
	}
}

// MailboxSpec describes the mailbox to provision/update — the fields
// email-provisioner denormalizes from subscriptions-api's EmailLicense/EmailPlan
// at license-assign/upgrade time.
type MailboxSpec struct {
	Email      string   `json:"email"`
	Domain     string   `json:"domain"`
	QuotaBytes int64    `json:"quota"`
	Aliases    []string `json:"aliases,omitempty"`
	// Rate-limiter tier values (Part 6, Layer 1) are meant to be written
	// directly into Stalwart's queue.limiter.inbound TOML config at
	// license-assign time, not via any per-account JMAP field (none exists).
	// That write path is not yet implemented (plan §13.3) — these are kept
	// on the spec so callers can already pass them once it is, but
	// CreateMailbox below does not consume them today.
	MaxSendsPerDay      int `json:"-"`
	MaxSendsPerHour     int `json:"-"`
	MaxSendsPerMinute   int `json:"-"`
	MaxRecipientsPerDay int `json:"-"`
}

// CreateMailbox provisions a new mailbox account and returns its freshly
// generated initial password. Idempotent from the caller's side: if an
// account with this local part already exists under the domain, returns
// ("", nil) rather than erroring — NATS delivery is at-least-once (see
// internal/modules/events), retries must be safe.
//
// Credential delivery to the end user (emailing a "set your password" link,
// etc.) is NOT handled here — that's a separate, not-yet-built piece (plan
// §13.2's "email-provisioner's outbound events" gap). The caller is
// responsible for deciding what to do with the returned secret.
func (c *Client) CreateMailbox(ctx context.Context, spec MailboxSpec) (initialSecret string, err error) {
	domainID, err := c.resolveDomainID(ctx, spec.Domain)
	if err != nil {
		return "", err
	}

	localPart, domainPart, ok := strings.Cut(spec.Email, "@")
	if !ok || domainPart == "" {
		return "", fmt.Errorf("invalid email %q", spec.Email)
	}

	existingID, err := c.findAccountIDByLocalPart(ctx, domainID, localPart, spec.Email)
	if err != nil {
		return "", err
	}
	if existingID != "" {
		return "", nil
	}

	secret, err := generateSecret()
	if err != nil {
		return "", fmt.Errorf("generate initial secret: %w", err)
	}

	create := map[string]any{
		"@type":    "User",
		"name":     localPart,
		"domainId": domainID,
		"credentials": map[string]any{
			"0": map[string]any{"@type": "Password", "secret": secret},
		},
		"roles":       map[string]any{"@type": "User"},
		"permissions": map[string]any{"@type": "Inherit"},
	}
	if spec.QuotaBytes > 0 {
		create["quotas"] = map[string]any{"maxDiskQuota": spec.QuotaBytes}
	}
	if aliases, err := c.buildAliases(ctx, spec.Domain, domainID, spec.Aliases); err != nil {
		return "", err
	} else if len(aliases) > 0 {
		create["aliases"] = aliases
	}

	res, err := c.call(ctx, "x:Account/set", map[string]any{
		"create": map[string]any{"new1": create},
	})
	if err != nil {
		return "", fmt.Errorf("create mailbox %s: %w", spec.Email, err)
	}
	if notCreated, _ := res["notCreated"].(map[string]any); notCreated != nil {
		if reason, ok := notCreated["new1"]; ok {
			return "", fmt.Errorf("create mailbox %s rejected: %v", spec.Email, reason)
		}
	}
	return secret, nil
}

// QueueStats reports the outbound delivery queue depth — the one mail-specific
// signal the platform's lightweight, non-Prometheus monitoring pattern
// (auth-api's k8s.Client.Overview) can't get any other way. Confirmed live
// against the real x:QueuedMessage/query method with calculateTotal.
type QueueStats struct {
	QueueDepth int `json:"queue_depth"`
}

func (c *Client) QueueStats(ctx context.Context) (QueueStats, error) {
	res, err := c.call(ctx, "x:QueuedMessage/query", map[string]any{
		"calculateTotal": true,
		"limit":          0,
	})
	if err != nil {
		return QueueStats{}, fmt.Errorf("query queue depth: %w", err)
	}
	total, _ := res["total"].(float64)
	return QueueStats{QueueDepth: int(total)}, nil
}

// UpdateQuota changes an existing mailbox's storage quota (plan upgrade/downgrade).
func (c *Client) UpdateQuota(ctx context.Context, email string, quotaBytes int64) error {
	return c.patchAccount(ctx, email, map[string]any{
		"quotas": map[string]any{"maxDiskQuota": quotaBytes},
	})
}

// SuspendMailbox clears the account's credentials so it can no longer
// authenticate — per plan Part 6's abuse-response ladder, this is used for
// both billing suspension (license.suspended) and security lockout
// (auth.user.deactivated). x:UserAccount has no single "disabled" boolean in
// the confirmed schema; an empty `credentials` map is the confirmed-live
// mechanism (verified against a real account: update accepted, subsequent
// Basic Auth with the old password returns 401) and leaves the mailbox and
// its stored mail fully intact for inbound delivery/read access — an
// earlier version of this method tried an "expired" credential instead,
// which the server rejected outright (`x:PasswordCredential.secret` is
// required even on update, so a partial patch omitting it fails validation).
func (c *Client) SuspendMailbox(ctx context.Context, email string) error {
	return c.patchAccount(ctx, email, map[string]any{
		"credentials": map[string]any{},
	})
}

// ReactivateMailbox re-enables an account after a suspension is lifted by
// issuing a fresh credential — SuspendMailbox cleared credentials entirely,
// so the caller must supply a new password.
func (c *Client) ReactivateMailbox(ctx context.Context, email string) (newSecret string, err error) {
	secret, err := generateSecret()
	if err != nil {
		return "", fmt.Errorf("generate reactivation secret: %w", err)
	}
	if err := c.patchAccount(ctx, email, map[string]any{
		"credentials": map[string]any{
			"0": map[string]any{"@type": "Password", "secret": secret},
		},
	}); err != nil {
		return "", err
	}
	return secret, nil
}

// ArchiveMailbox is called on email.license.expired after the grace period —
// zeroes the disk quota (blocks new mail without deleting existing data) and
// suspends submission, matching the original plan's Part 3E lifecycle.
func (c *Client) ArchiveMailbox(ctx context.Context, email string) error {
	if err := c.patchAccount(ctx, email, map[string]any{
		"quotas": map[string]any{"maxDiskQuota": 0},
	}); err != nil {
		return err
	}
	return c.SuspendMailbox(ctx, email)
}

// SendSystemEmail sends a plain-text transactional message from an existing
// Stalwart-hosted mailbox (normally the platform's no-reply@ address) via
// standard JMAP mail submission — Email/set (create in Drafts) then
// EmailSubmission/set. Two round trips rather than one JMAP request with a
// result reference, since call() issues a single method call per request;
// simplicity over one saved round trip for what's a low-volume operation.
//
// Kept inside this client (talking only to Stalwart, never a third service)
// rather than routed through notifications-api — this bridge is meant to
// stay "deliberately dumb" (plan Part 3E), and this is still just "talk to
// Stalwart," only over its mail-submission API instead of its admin API.
func (c *Client) SendSystemEmail(ctx context.Context, fromEmail, toEmail, subject, textBody string) error {
	fromLocal, fromDomain, ok := strings.Cut(fromEmail, "@")
	if !ok || fromDomain == "" {
		return fmt.Errorf("invalid from address %q", fromEmail)
	}
	domainID, err := c.resolveDomainID(ctx, fromDomain)
	if err != nil {
		return err
	}
	accountID, err := c.findAccountIDByLocalPart(ctx, domainID, fromLocal, fromEmail)
	if err != nil {
		return err
	}
	if accountID == "" {
		return fmt.Errorf("sender account %s not found in Stalwart", fromEmail)
	}

	identityID, err := c.firstIdentityID(ctx, accountID, fromEmail)
	if err != nil {
		return err
	}
	draftsID, sentID, err := c.draftsAndSentMailboxIDs(ctx, accountID, fromEmail)
	if err != nil {
		return err
	}

	bodyPartID := "b1"
	createRes, err := c.call(ctx, "Email/set", map[string]any{
		"accountId": accountID,
		"create": map[string]any{
			"msg1": map[string]any{
				"mailboxIds": map[string]any{draftsID: true},
				"from":       []map[string]any{{"email": fromEmail}},
				"to":         []map[string]any{{"email": toEmail}},
				"subject":    subject,
				"bodyValues": map[string]any{bodyPartID: map[string]any{"value": textBody, "charset": "utf-8"}},
				"textBody":   []map[string]any{{"partId": bodyPartID, "type": "text/plain"}},
				"keywords":   map[string]any{"$draft": true},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create system email to %s: %w", toEmail, err)
	}
	created, _ := createRes["created"].(map[string]any)
	msg1, _ := created["msg1"].(map[string]any)
	emailID, _ := msg1["id"].(string)
	if emailID == "" {
		if notCreated, _ := createRes["notCreated"].(map[string]any); notCreated != nil {
			return fmt.Errorf("system email to %s rejected: %v", toEmail, notCreated["msg1"])
		}
		return fmt.Errorf("system email to %s: no id in create response", toEmail)
	}

	submitArgs := map[string]any{
		"accountId": accountID,
		"create": map[string]any{
			"sub1": map[string]any{"emailId": emailID, "identityId": identityID},
		},
	}
	if sentID != "" {
		submitArgs["onSuccessUpdateEmail"] = map[string]any{
			emailID: map[string]any{
				fmt.Sprintf("mailboxIds/%s", draftsID): nil,
				fmt.Sprintf("mailboxIds/%s", sentID):   true,
				"keywords/$draft":                      nil,
			},
		}
	}
	submitRes, err := c.call(ctx, "EmailSubmission/set", submitArgs)
	if err != nil {
		return fmt.Errorf("submit system email to %s: %w", toEmail, err)
	}
	if notCreated, _ := submitRes["notCreated"].(map[string]any); notCreated != nil {
		if reason, ok := notCreated["sub1"]; ok {
			return fmt.Errorf("submit system email to %s rejected: %v", toEmail, reason)
		}
	}
	return nil
}

func (c *Client) firstIdentityID(ctx context.Context, accountID, email string) (string, error) {
	res, err := c.call(ctx, "Identity/get", map[string]any{"accountId": accountID, "ids": nil})
	if err != nil {
		return "", fmt.Errorf("resolve identity for %s: %w", email, err)
	}
	list, _ := res["list"].([]any)
	if len(list) == 0 {
		return "", fmt.Errorf("no send identity found for %s", email)
	}
	first, _ := list[0].(map[string]any)
	id, _ := first["id"].(string)
	return id, nil
}

func (c *Client) draftsAndSentMailboxIDs(ctx context.Context, accountID, email string) (draftsID, sentID string, err error) {
	res, err := c.call(ctx, "Mailbox/get", map[string]any{"accountId": accountID, "ids": nil})
	if err != nil {
		return "", "", fmt.Errorf("list mailboxes for %s: %w", email, err)
	}
	list, _ := res["list"].([]any)
	for _, m := range list {
		mm, _ := m.(map[string]any)
		role, _ := mm["role"].(string)
		id, _ := mm["id"].(string)
		switch role {
		case "drafts":
			draftsID = id
		case "sent":
			sentID = id
		}
	}
	if draftsID == "" {
		return "", "", fmt.Errorf("no Drafts mailbox found for %s", email)
	}
	return draftsID, sentID, nil
}

func (c *Client) patchAccount(ctx context.Context, email string, patch map[string]any) error {
	localPart, domainPart, ok := strings.Cut(email, "@")
	if !ok || domainPart == "" {
		return fmt.Errorf("invalid email %q", email)
	}
	domainID, err := c.resolveDomainID(ctx, domainPart)
	if err != nil {
		return err
	}
	accountID, err := c.findAccountIDByLocalPart(ctx, domainID, localPart, email)
	if err != nil {
		return err
	}
	if accountID == "" {
		return fmt.Errorf("account %s not found in Stalwart", email)
	}

	res, err := c.call(ctx, "x:Account/set", map[string]any{
		"update": map[string]any{accountID: patch},
	})
	if err != nil {
		return fmt.Errorf("update mailbox %s: %w", email, err)
	}
	if notUpdated, _ := res["notUpdated"].(map[string]any); notUpdated != nil {
		if reason, ok := notUpdated[accountID]; ok {
			return fmt.Errorf("update mailbox %s rejected: %v", email, reason)
		}
	}
	return nil
}

// resolveDomainID looks up an x:Domain's id by name, caching hits for the
// process lifetime (domains are created rarely, via the setup flow — not
// worth a TTL/invalidation scheme for the current scale).
func (c *Client) resolveDomainID(ctx context.Context, domain string) (string, error) {
	c.mu.Lock()
	if id, ok := c.domainIDs[domain]; ok {
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	res, err := c.call(ctx, "x:Domain/query", map[string]any{
		"filter": map[string]any{"name": domain},
	})
	if err != nil {
		return "", fmt.Errorf("resolve domain %s: %w", domain, err)
	}
	ids, _ := res["ids"].([]any)
	if len(ids) == 0 {
		return "", fmt.Errorf("domain %s does not exist in Stalwart (create it via the admin panel first)", domain)
	}
	id, _ := ids[0].(string)

	c.mu.Lock()
	c.domainIDs[domain] = id
	c.mu.Unlock()
	return id, nil
}

// findAccountIDByLocalPart returns the existing account's id, or "" if none
// exists yet. x:Account/query has no exact-email filter (confirmed live —
// "Filter on property email is not supported"), only a fuzzy `text` filter,
// so results are verified against the account's real emailAddress via
// x:Account/get before being trusted.
func (c *Client) findAccountIDByLocalPart(ctx context.Context, domainID, localPart, wantEmail string) (string, error) {
	res, err := c.call(ctx, "x:Account/query", map[string]any{
		"filter": map[string]any{"text": localPart},
	})
	if err != nil {
		return "", fmt.Errorf("lookup account %s: %w", wantEmail, err)
	}
	ids, _ := res["ids"].([]any)
	if len(ids) == 0 {
		return "", nil
	}

	getRes, err := c.call(ctx, "x:Account/get", map[string]any{
		"ids":        ids,
		"properties": []string{"emailAddress"},
	})
	if err != nil {
		return "", fmt.Errorf("verify account lookup for %s: %w", wantEmail, err)
	}
	list, _ := getRes["list"].([]any)
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		if addr, _ := m["emailAddress"].(string); strings.EqualFold(addr, wantEmail) {
			id, _ := m["id"].(string)
			return id, nil
		}
	}
	return "", nil
}

// buildAliases resolves each alias address's domain (which may differ from
// the primary domain) into the x:EmailAlias shape x:Account/set expects.
func (c *Client) buildAliases(ctx context.Context, primaryDomain, primaryDomainID string, aliases []string) ([]map[string]any, error) {
	if len(aliases) == 0 {
		return nil, nil
	}
	out := make([]map[string]any, 0, len(aliases))
	for _, a := range aliases {
		aliasLocal, aliasDomain, ok := strings.Cut(a, "@")
		if !ok || aliasDomain == "" {
			return nil, fmt.Errorf("invalid alias address %q", a)
		}
		domainID := primaryDomainID
		if !strings.EqualFold(aliasDomain, primaryDomain) {
			var err error
			domainID, err = c.resolveDomainID(ctx, aliasDomain)
			if err != nil {
				return nil, fmt.Errorf("resolve alias domain %s: %w", aliasDomain, err)
			}
		}
		out = append(out, map[string]any{"name": aliasLocal, "domainId": domainID, "enabled": true})
	}
	return out, nil
}

func generateSecret() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// call issues one JMAP request with a single method call and returns that
// call's response arguments. Returns an error if the server responds with a
// JMAP-level "error" method response (e.g. unknownMethod, invalidArguments).
func (c *Client) call(ctx context.Context, method string, args map[string]any) (map[string]any, error) {
	reqBody := map[string]any{
		// Declares both the vendor admin capability (x:Account/x:Domain, used
		// throughout this file) and standard JMAP mail/submission (used by
		// SendSystemEmail below) — broader than any single call strictly
		// needs, but this client only ever issues one method call per
		// request, so a single fixed capability list is simpler than
		// threading a per-call "using" set through every caller.
		"using": []string{
			"urn:ietf:params:jmap:core",
			"urn:ietf:params:jmap:mail",
			"urn:ietf:params:jmap:submission",
			"urn:stalwart:jmap",
		},
		"methodCalls": []any{[]any{method, args, "c1"}},
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal jmap request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/jmap/", bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build jmap request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(c.adminUser, c.adminPass)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call jmap %s: %w", method, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jmap %s returned HTTP %d", method, resp.StatusCode)
	}

	var parsed struct {
		MethodResponses []json.RawMessage `json:"methodResponses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode jmap response for %s: %w", method, err)
	}
	if len(parsed.MethodResponses) == 0 {
		return nil, fmt.Errorf("jmap %s: empty methodResponses", method)
	}

	var triple [3]json.RawMessage
	if err := json.Unmarshal(parsed.MethodResponses[0], &triple); err != nil {
		return nil, fmt.Errorf("decode jmap method response tuple for %s: %w", method, err)
	}
	var name string
	if err := json.Unmarshal(triple[0], &name); err != nil {
		return nil, fmt.Errorf("decode jmap method name for %s: %w", method, err)
	}

	var respArgs map[string]any
	if err := json.Unmarshal(triple[1], &respArgs); err != nil {
		return nil, fmt.Errorf("decode jmap response args for %s: %w", method, err)
	}
	if name == "error" {
		return nil, fmt.Errorf("jmap %s: %v", method, respArgs)
	}
	return respArgs, nil
}

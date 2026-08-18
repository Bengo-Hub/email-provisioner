// Package token generates the signed, time-limited "set your mailbox
// password" links used for credential delivery — see the config-level
// comment on ProvisioningTokenSecret for why this exists instead of ever
// transmitting Stalwart's own randomly-generated initial secret. Verified
// independently by mail-ui (same HMAC construction) in
// mail-ui/src/lib/setup-token.ts — keep the two in sync if this changes.
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// GenerateSetupToken returns a token of the form
// base64url(email).base64url(unixExpiry).base64url(hmacSHA256(secret, email|expiry)).
func GenerateSetupToken(secret, email string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	payload := fmt.Sprintf("%s|%d", email, exp)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)

	return strings.Join([]string{
		base64.RawURLEncoding.EncodeToString([]byte(email)),
		base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(exp, 10))),
		base64.RawURLEncoding.EncodeToString(sig),
	}, ".")
}

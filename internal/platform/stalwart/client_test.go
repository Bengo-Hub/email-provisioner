package stalwart

import "testing"

// Real zone-file fragment captured live from a disposable test domain probe
// (2026-08-19), not fabricated — exercises the actual multi-line RSA record
// shape (parenthesized, wrapped across several quoted strings).
const sampleZoneFile = `v1-ed25519-20260819._domainkey.example.com. IN TXT "v=DKIM1; k=ed25519; h=sha256; p=T7GYjXkFcAjAUm9mWMnTp53oqdZZFNt24JL1gZ088kg="
v1-rsa-20260819._domainkey.example.com. IN TXT (
    "v=DKIM1; k=rsa; h=sha256; p=MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA"
    "N/iz9UoRqMBFBlH5vDm1DOfp/obkHbbI4DZMr2GhvJ2dg0vs9asXUK/Ivawh+AKD4hTRW1ql"
)
example.com. IN TXT "v=spf1 mx -all"
example.com. IN MX 10 mx1.codevertexafrica.com.
_dmarc.example.com. IN TXT "v=DMARC1; p=reject; rua=mailto:postmaster@example.com"
`

func TestExtractDKIMSelector(t *testing.T) {
	cases := []struct {
		name     string
		zoneFile string
		domain   string
		want     string
	}{
		{"real zone file, prefers rsa over ed25519", sampleZoneFile, "example.com", "v1-rsa-20260819"},
		{"only ed25519 present falls back to it", "v1-ed25519-20260819._domainkey.example.com. IN TXT \"...\"\n", "example.com", "v1-ed25519-20260819"},
		{"no domainkey records at all", "example.com. IN MX 10 mx1.codevertexafrica.com.\n", "example.com", ""},
		{"empty zone file", "", "example.com", ""},
		{"domainkey line for a different domain is ignored", "v1-rsa-20260819._domainkey.other.com. IN TXT \"...\"\n", "example.com", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractDKIMSelector(c.zoneFile, c.domain); got != c.want {
				t.Errorf("extractDKIMSelector(...) = %q, want %q", got, c.want)
			}
		})
	}
}

package offsite

// SSH host-key pinning for the SFTP destination (P4 review D1). The bundle
// contains the shares key — an unpinned connection lets a machine-in-the-
// middle steal it, so the server's host key is REQUIRED configuration and
// enforced with ssh.FixedHostKey on every dial.

import (
	"encoding/base64"
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ParseHostKey accepts the two operator-friendly shapes of a public host
// key and returns the parsed key:
//   - known-hosts style: "ssh-ed25519 AAAA… [comment]" (also tolerates a
//     leading hostname field, i.e. raw `ssh-keyscan <host>` output),
//   - bare base64 key blob: "AAAA…".
//
// Garbage is refused with an actionable error naming ssh-keyscan. The
// settings save path parses AND normalizes with this; the dial path parses
// the stored (already normalized) value again as defense in depth.
func ParseHostKey(raw string) (ssh.PublicKey, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("host key is empty — paste the server's public host key (get it with: ssh-keyscan <host>)")
	}
	fields := strings.Fields(s)
	// Candidate base64 blobs: bare form (field 0), known-hosts form
	// (field 1), or ssh-keyscan output with a leading hostname (field 2).
	var candidates []string
	for i := 0; i < len(fields) && i < 3; i++ {
		candidates = append(candidates, fields[i])
	}
	for _, cand := range candidates {
		der, err := base64.StdEncoding.DecodeString(cand)
		if err != nil {
			continue
		}
		if key, err := ssh.ParsePublicKey(der); err == nil {
			return key, nil
		}
	}
	return nil, errors.New("not a valid SSH public host key — expected 'ssh-ed25519 AAAA…' (known-hosts style) or a bare base64 key; get it with: ssh-keyscan <host>")
}

// NormalizeHostKey renders a parsed key in the canonical stored form:
// "<type> <base64>" (no comment, no hostname).
func NormalizeHostKey(key ssh.PublicKey) string {
	return key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal())
}

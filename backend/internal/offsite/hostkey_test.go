package offsite

// D1: host-key parsing accepts the operator-friendly shapes and refuses
// garbage with an actionable ssh-keyscan hint.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func genPub(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestParseHostKeyShapes(t *testing.T) {
	k := genPub(t)
	b64 := base64.StdEncoding.EncodeToString(k.Marshal())
	for _, in := range []string{
		"ssh-ed25519 " + b64,                    // known-hosts style
		"ssh-ed25519 " + b64 + " comment@host",  // with trailing comment
		b64,                                     // bare base64
		"  " + b64 + "  ",                       // whitespace tolerated
		"backup.example.com ssh-ed25519 " + b64, // raw ssh-keyscan output
	} {
		got, err := ParseHostKey(in)
		if err != nil {
			t.Fatalf("ParseHostKey(%q): %v", in, err)
		}
		if NormalizeHostKey(got) != "ssh-ed25519 "+b64 {
			t.Fatalf("ParseHostKey(%q) normalized to %q", in, NormalizeHostKey(got))
		}
	}
}

func TestParseHostKeyGarbage(t *testing.T) {
	for _, in := range []string{"", "   ", "not-a-key", "ssh-ed25519 @@@", "AAAA"} {
		_, err := ParseHostKey(in)
		if err == nil {
			t.Fatalf("ParseHostKey(%q) must fail", in)
		}
		if !strings.Contains(err.Error(), "ssh-keyscan") {
			t.Fatalf("error must mention ssh-keyscan: %v", err)
		}
	}
}

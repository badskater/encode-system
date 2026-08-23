package auth

import "testing"

func TestNewTokenIsUniqueAndSized(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 || len(b) != 64 {
		t.Fatalf("token length: %d, %d", len(a), len(b))
	}
	if a == b {
		t.Fatal("tokens must be random/unique")
	}
}

func TestHashAndVerifyRoundTrip(t *testing.T) {
	tok, _ := NewToken()
	h := HashToken(tok)
	if !VerifyToken(tok, h) {
		t.Fatal("valid token must verify")
	}
	if VerifyToken(tok+"x", h) {
		t.Fatal("wrong token must not verify")
	}
	if VerifyToken("", h) {
		t.Fatal("empty token must not verify")
	}
}

func TestHashIsDeterministic(t *testing.T) {
	if HashToken("abc") != HashToken("abc") {
		t.Fatal("hash must be deterministic")
	}
}

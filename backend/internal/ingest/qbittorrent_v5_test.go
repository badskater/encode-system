package ingest

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The ≥5.2 protocol generation, verified against upstream
// qbittorrent/qBittorrent (src/webui/api/authcontroller.cpp +
// src/webui/webapplication.cpp on v5_2_x/v5_3_x/master): login success
// sets only the API status and null result data renders as 204 (no
// "Ok." body); bad credentials throw APIErrorType::Unauthorized → 401
// (not 200 "Fails."). The 4.x–5.1 generation (200 "Ok."/"Fails.",
// expiry 403) stays covered by the tests in qbittorrent_test.go.

// TestQBit5Login204Success proves a ≥5.2 login (204 No Content, empty
// body) is accepted and the session works end to end.
func TestQBit5Login204Success(t *testing.T) {
	f := newFakeQBit(t)
	f.gen5 = true
	f.torrents = []qbitTorrent{
		{Hash: "aaa111", Name: "Show.S01E01.1080p", Category: "anime", SavePath: "/downloads/Show.S01E01.1080p"},
	}
	f.filesByHash["aaa111"] = []qbitFile{{Name: "show.s01e01.mkv", Size: 1000}}

	q := f.client(t)
	got, err := q.Completed(context.Background(), "anime")
	if err != nil {
		t.Fatalf("5.x 204 login must succeed: %v", err)
	}
	if len(got) != 1 || got[0].Hash != "aaa111" || len(got[0].Files) != 1 ||
		got[0].Files[0].Name != "show.s01e01.mkv" {
		t.Fatalf("torrent round-trip after 204 login: %+v", got)
	}
	if f.logins.Load() != 1 {
		t.Fatalf("expected exactly 1 login, got %d", f.logins.Load())
	}
}

// TestQBit5BadCredentials401 proves ≥5.2 bad credentials (401, not
// 200 "Fails.") are a clear authentication error with no retry loop.
func TestQBit5BadCredentials401(t *testing.T) {
	f := newFakeQBit(t)
	f.gen5 = true
	f.badCreds = true
	q := f.client(t)
	_, err := q.Completed(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("5.x 401 must be a clear auth error, got %v", err)
	}
	if f.logins.Load() != 1 {
		t.Fatalf("bad credentials must not retry login, got %d", f.logins.Load())
	}
}

// TestQBit5SessionExpiry401Relogin proves a 401 on a data request
// (≥5.2 Unauthorized path / expired session) triggers exactly one
// re-login and a successful retry — the old 403-only retry never fired
// on the 5.x generation.
func TestQBit5SessionExpiry401Relogin(t *testing.T) {
	f := newFakeQBit(t)
	f.gen5 = true
	f.expiryStatus = http.StatusUnauthorized
	f.torrents = []qbitTorrent{{Hash: "aaa", Name: "A S01E01", SavePath: "/d/a"}}
	f.filesByHash["aaa"] = []qbitFile{{Name: "a.mkv", Size: 1}}

	q := f.client(t)
	// Warm the session so the first info call carries a (soon-expired) SID.
	if _, err := q.Completed(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	f.failFirst.Store(true)
	got, err := q.Completed(context.Background(), "")
	if err != nil {
		t.Fatalf("401 expiry must re-login and retry: %v", err)
	}
	if len(got) != 1 || got[0].Hash != "aaa" {
		t.Fatalf("retry lost the list: %+v", got)
	}
	if f.logins.Load() != 2 {
		t.Fatalf("expected 2 logins (initial + re-login), got %d", f.logins.Load())
	}
}

// TestQBit5LoginBan403 proves a 403 on LOGIN is still classified as an
// IP ban on the ≥5.2 generation (upstream validateCredentials throws
// ForbiddenHTTPError when banned), not mistaken for a retryable expiry.
func TestQBit5LoginBan403(t *testing.T) {
	f := newFakeQBit(t)
	f.gen5 = true
	f.banLogin = true
	q := f.client(t)
	_, err := q.Completed(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "IP banned") {
		t.Fatalf("login 403 must be classified as an IP ban, got %v", err)
	}
}

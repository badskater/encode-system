package ingest

import (
	"context"
	"net/http"
	"time"
)

// Torrent is one completed download as reported by a client. Files carry
// paths RELATIVE to SavePath (the client-host absolute save directory).
type Torrent struct {
	Hash     string // client torrent hash (the dedupe ledger key)
	Name     string // release name (parsed into series/episode)
	Category string // qBittorrent category / ruTorrent label (custom1)
	SavePath string // absolute save directory on the CLIENT host
	Files    []File
}

// File is one file inside a torrent, relative to the torrent save path.
type File struct {
	Name string // forward-slash relative path
	Size int64
}

// Client is the download-client surface the poll loop needs. Kept narrow
// so tests can inject fakes; the production implementations are the
// bounded qBittorrent WebAPI v2 client and the best-effort ruTorrent
// httprpc client.
type Client interface {
	// Completed lists COMPLETED torrents, optionally restricted to one
	// category (empty = all categories). Implementations must bound every
	// HTTP exchange (client timeout + caller ctx) and never use
	// http.DefaultClient.
	Completed(ctx context.Context, category string) ([]Torrent, error)
}

// httpTimeout bounds every individual HTTP exchange with a download
// client (P9 lesson: explicit timeouts everywhere — a wedged client must
// never hang the poll loop). The caller's ctx bounds the whole tick on
// top of this.
const httpTimeout = 30 * time.Second

// newBoundedClient builds the shared-shape HTTP client both providers
// use: explicit whole-request timeout, NO default-client fallback. The
// cookie jar is provider-specific (qBittorrent sessions); pass nil for
// clients that do not use cookies.
func newBoundedClient(jar http.CookieJar) *http.Client {
	return &http.Client{
		Timeout: httpTimeout,
		Jar:     jar,
	}
}

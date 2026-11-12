package ingest

// The ingest↔scanner source-extension contract (Q8). parse.go documents
// that videoExtensions mirrors scanner.SourceExtensions; this test is the
// pin that comment promises — it fails on drift in EITHER direction.

import (
	"testing"

	"github.com/badskater/encode-system/backend/internal/scanner"
)

// TestVideoExtensionsContract proves the staged-extension set and the
// scanner's accepted-source set stay identical: every extension the
// scanner accepts must be staged by ingest (else a completed download
// never becomes a source), and ingest must never stage an extension the
// scanner ignores (a staged file the scanner skips is a dead source).
func TestVideoExtensionsContract(t *testing.T) {
	scannerSet := make(map[string]bool, len(scanner.SourceExtensions))
	for _, ext := range scanner.SourceExtensions {
		scannerSet[ext] = true
		if !IsVideoFile("x" + ext) {
			t.Errorf("scanner accepts %s but ingest does not stage it", ext)
		}
	}
	for _, ext := range videoExtensions {
		if !scannerSet[ext] {
			t.Errorf("ingest stages %s but the scanner does not accept it", ext)
		}
	}
	// Common non-source extensions must stay excluded on both sides.
	for _, ext := range []string{".avi", ".wmv", ".mov", ".part", ".tmp"} {
		if IsVideoFile("x" + ext) {
			t.Errorf("ingest must not stage %s", ext)
		}
		if scannerSet[ext] {
			t.Errorf("scanner must not accept %s", ext)
		}
	}
}

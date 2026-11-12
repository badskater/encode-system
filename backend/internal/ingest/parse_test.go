package ingest

import "testing"

// TestParseReleaseTable pins the conservative release-name parser: every
// supported shape, every refusal. A false positive here stages files into
// the WRONG series folder, so the ambiguous cases must stay refused.
func TestParseReleaseTable(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		series  string
		episode int
		ok      bool
	}{
		// SxxEyy, dots/underscores, mixed case, quality suffixes.
		{"sxxeyy dots", "Some.Show.S01E01.1080p.BluRay-GROUP", "Some Show", 1, true},
		{"sxxeyy underscores", "Some_Show_S01E07_720p", "Some Show", 7, true},
		{"sxxeyy lowercase", "some.show.s01e12.hdtv", "some show", 12, true},
		{"sxxeyy three digit", "Show.S01E100.1080p", "Show", 100, true},
		{"sxxeyy no suffix", "Show Name S01E03", "Show Name", 3, true},
		{"sxxeyy single digit season", "Show S1E3", "Show", 3, true},
		{"sxxeyy year in title", "Show 2020 S01E05", "Show 2020", 5, true},
		{"sxxeyy paren year", "Show (2020) S01E05", "Show (2020)", 5, true},
		// NNxNN.
		{"nnxnn", "Some.Show.01x03.720p", "Some Show", 3, true},
		{"nnxnn bare", "Show 01x12", "Show", 12, true},
		// Anime dash form with fansub brackets.
		{"anime dash", "[Group] Show Name - 03 [1080p][ABCD1234]", "Show Name", 3, true},
		{"anime dash no group", "Show Name - 03 [1080p]", "Show Name", 3, true},
		{"anime dash version", "Show Name - 03v2 [720p]", "Show Name", 3, true},
		{"anime dash dots", "Show.Name.-.03.[1080p]", "Show Name", 3, true},
		{"anime dash three digit", "[Subs] Long Running Show - 123 [1080p]", "Long Running Show", 123, true},
		// Bare trailing number.
		{"bare number", "Show Name 03", "Show Name", 3, true},
		{"bare number dots", "Show.Name.03.1080p", "", 0, false}, // "1080p" residue is not a bare number; no dash/SxxEyy → refuse
		{"bare two digit", "Show 12", "Show", 12, true},
		// Refusals: multi-season (would collide with season 1 folders).
		{"season 2 refused", "Show.S02E03.1080p", "", 0, false},
		{"season 10 refused", "Show.S10E01", "", 0, false},
		{"nnxnn season 2 refused", "Show.02x03", "", 0, false},
		// Refusals: multi-episode packs.
		{"double episode refused", "Show.S01E01E02.1080p", "", 0, false},
		{"episode range refused", "Show.S01E01-02.1080p", "", 0, false},
		{"episode range e refused", "Show.S01E01-E02", "", 0, false},
		// Refusals: episode bounds.
		{"episode zero refused", "Show.S01E00", "", 0, false},
		{"episode 1000 refused", "Show.S01E1000", "", 0, false},
		{"dash episode zero refused", "Show - 00", "", 0, false},
		// Refusals: ambiguous bare numbers.
		{"year refused", "Movie 2024", "", 0, false},
		{"resolution refused", "Show 1080", "", 0, false},
		{"two numbers refused", "Show 2 05", "", 0, false},
		{"title number refused", "Gundam 00 05", "", 0, false},
		// Refusals: no episode signal at all.
		{"no episode", "Some.Show.1080p.BluRay", "", 0, false},
		{"movie name", "Some Movie 2024 1080p BluRay", "", 0, false},
		{"empty", "", "", 0, false},
		{"only brackets", "[Group][1080p]", "", 0, false},
		{"only number", "03", "", 0, false},
		{"season pack", "Show.S01.1080p", "", 0, false},
		// Path-safety: separators/control chars can never survive parsing.
		{"traversal refused", "../evil S01E01", "", 0, false},
		{"slash refused", "a/b S01E01", "", 0, false},
		// Extension stripping.
		{"mkv extension", "Show.Name.S01E01.1080p.mkv", "Show Name", 1, true},
		{"anime extension", "[Group] Show - 01 [1080p].mkv", "Show", 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			series, ep, ok := ParseRelease(tc.in)
			if ok != tc.ok || series != tc.series || ep != tc.episode {
				t.Fatalf("ParseRelease(%q) = (%q, %d, %v), want (%q, %d, %v)",
					tc.in, series, ep, ok, tc.series, tc.episode, tc.ok)
			}
		})
	}
}

// TestValidSeriesName pins the folder-name guard shared by the parser and
// the category-map validation.
func TestValidSeriesName(t *testing.T) {
	good := []string{"Show Name", "Show (2020)", "ショー", "A", "Show-Name"}
	bad := []string{"", ".", "..", "a/b", `a\b`, "../x", "a..b", "a\x00b", "a\nb", "a\x7fb", string(make([]byte, 201))}
	for _, s := range good {
		if !ValidSeriesName(s) {
			t.Errorf("ValidSeriesName(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidSeriesName(s) {
			t.Errorf("ValidSeriesName(%q) = true, want false", s)
		}
	}
}

// TestIsVideoFile pins the staged-extension set (must mirror the scanner).
func TestIsVideoFile(t *testing.T) {
	yes := []string{"a.mkv", "a.MKV", "a.mp4", "a.ts", "a.m2ts", "dir/a.mkv"}
	no := []string{"a.nfo", "a.txt", "a.srt", "a.ass", "a.jpg", "a", "a.mkv.part"}
	for _, s := range yes {
		if !IsVideoFile(s) {
			t.Errorf("IsVideoFile(%q) = false", s)
		}
	}
	for _, s := range no {
		if IsVideoFile(s) {
			t.Errorf("IsVideoFile(%q) = true", s)
		}
	}
}

// TestEpisodeDirName pins the scanner-convention folder shape.
func TestEpisodeDirName(t *testing.T) {
	if got := EpisodeDirName(1); got != "Ep 01" {
		t.Fatalf("EpisodeDirName(1) = %q", got)
	}
	if got := EpisodeDirName(123); got != "Ep 123" {
		t.Fatalf("EpisodeDirName(123) = %q", got)
	}
}

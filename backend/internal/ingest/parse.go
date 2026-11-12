// Package ingest implements EXPERIMENTAL download ingestion (P11): it
// polls a BitTorrent client (qBittorrent first-class, ruTorrent
// best-effort) for completed downloads, parses release names into
// series/episode, and stages the source video files into the scripts
// share. It stages SOURCES only — filter scripts (.avs/.vpy) remain
// hand-authored, and the scanner creates jobs once both are present.
//
// The package is off by default: the controller only runs the poll loop
// body when settings.ingest.enabled is true.
package ingest

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// maxSeriesNameLen caps a parsed/mapped series folder name. Folder names
// beyond this are almost certainly a misparse, and pathological lengths
// break filesystem limits downstream.
const maxSeriesNameLen = 200

// videoExtensions are the file extensions staged as encode sources. They
// mirror scanner.SourceExtensions (the scanner's source detection) — kept
// as a local list so the ingest package does not import the scanner just
// for four strings; the two MUST stay in sync (a contract test pins it).
var videoExtensions = []string{".m2ts", ".ts", ".mkv", ".mp4"}

// IsVideoFile reports whether name carries a source-video extension
// (case-insensitive). Only these files are staged; samples, NFOs and
// other extras stay in the download directory.
func IsVideoFile(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	for _, e := range videoExtensions {
		if ext == e {
			return true
		}
	}
	return false
}

// ValidSeriesName reports whether s is safe to use as a series folder
// name on the scripts share: non-empty, bounded length, no path
// separators, no traversal, no control characters. Both the release-name
// parser and the category→series map validation enforce it — a folder
// name is a path component, never a path.
func ValidSeriesName(s string) bool {
	if s == "" || len(s) > maxSeriesNameLen || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\") || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// EpisodeDirName renders the scanner-convention episode folder for an
// episode number ("Ep 01", zero-padded to at least two digits — the same
// shape flow.EpisodeNumber extracts).
func EpisodeDirName(episode int) string {
	return fmt.Sprintf("Ep %02d", episode)
}

var (
	// sxxeyy: "Show S01E03" (any case). Season is captured so multi-season
	// releases can be refused (see ParseRelease).
	sxxeyyRe = regexp.MustCompile(`(?i)\bS(\d{1,2})E(\d{1,3})`)
	// nnxnn: "Show 01x03".
	nnxnnRe = regexp.MustCompile(`(?i)\b(\d{1,2})x(\d{1,3})\b`)
	// anime dash form: "Title - 03" with an optional version suffix (v2).
	dashEpRe = regexp.MustCompile(`^(.*?)\s+-\s+(\d{1,3})(?:v\d+)?$`)
	// bare trailing number: "Title 03".
	bareEpRe = regexp.MustCompile(`^(.*?)\s+(\d{1,3})$`)
	// multiEpisodeTail detects a second episode right after the first
	// ("S01E01E02", "S01E01-02", "S01E01 - E02") — a pack, not a single
	// episode; conservative parsing refuses it.
	multiEpisodeTail = regexp.MustCompile(`(?i)^[-.\s]*E\d{1,3}|^[-.\s]*-\s*\d{1,3}`)
	// leadingBracket / trailingBracket strip fansub tags and CRC/quality
	// brackets: "[Group] Title - 01 [1080p][ABCD1234]".
	leadingBracketRe  = regexp.MustCompile(`^\s*\[[^\]]*\]\s*`)
	trailingBracketRe = regexp.MustCompile(`\s*\[[^\]]*\]\s*$`)
)

// ParseRelease extracts a series name and episode number from a torrent
// release name. It is deliberately CONSERVATIVE: anything ambiguous
// returns ok=false so the caller skips and logs — the parser never
// guesses. Supported shapes (dots/underscores normalize to spaces):
//
//   - Show.Name.S01E03.1080p-GROUP  → "Show Name", 3 (season 1 only)
//   - Show.Name.01x03.720p          → "Show Name", 3 (season 1 only)
//   - [Group] Show Name - 03 [1080p] → "Show Name", 3 (anime dash form)
//   - Show Name - 03v2               → "Show Name", 3 (version suffix)
//   - Show Name 03                   → "Show Name", 3 (bare trailing number,
//     refused for years, resolutions, and titles carrying other numbers)
//
// Multi-season releases (S02E03, 02x03) are refused: staging them as
// "Ep 03" would collide with season 1 in the same series folder.
// Multi-episode packs (S01E01E02, S01E01-02) are refused likewise.
// Episode 0 and episodes above 999 are refused.
func ParseRelease(name string) (series string, episode int, ok bool) {
	s := strings.TrimSpace(name)
	// Strip a known video extension (torrent names sometimes carry it).
	if ext := strings.ToLower(path.Ext(s)); IsVideoFile("x" + ext) {
		s = strings.TrimSuffix(s, path.Ext(s))
	}
	// Strip leading [Group] and trailing [tags] brackets.
	for {
		trimmed := leadingBracketRe.ReplaceAllString(s, "")
		trimmed = trailingBracketRe.ReplaceAllString(trimmed, "")
		if trimmed == s {
			break
		}
		s = trimmed
	}
	// Dots and underscores are release-name separators.
	s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	s = collapseSpaces(s)
	if s == "" {
		return "", 0, false
	}

	// 1) SxxEyy — the strongest signal.
	if m := sxxeyyRe.FindStringSubmatchIndex(s); m != nil {
		// A digit right after the episode digits means the number was
		// truncated by the {1,3} cap ("E1000" → "E100"+"0"): refuse
		// rather than stage a wrong episode. Same for the season.
		if m[5] < len(s) && isDigit(s[m[5]]) {
			return "", 0, false
		}
		if m[3] < len(s) && isDigit(s[m[3]]) {
			return "", 0, false
		}
		season, _ := strconv.Atoi(s[m[2]:m[3]])
		ep, _ := strconv.Atoi(s[m[4]:m[5]])
		head := strings.TrimSpace(s[:m[0]])
		tail := strings.TrimSpace(s[m[1]:])
		if season != 1 || !validEpisode(ep) || !cleanSeries(&head) {
			return "", 0, false
		}
		if multiEpisodeTail.MatchString(tail) {
			return "", 0, false // double-episode pack
		}
		return head, ep, true
	}
	// 2) NNxNN.
	if m := nnxnnRe.FindStringSubmatchIndex(s); m != nil {
		season, _ := strconv.Atoi(s[m[2]:m[3]])
		ep, _ := strconv.Atoi(s[m[4]:m[5]])
		head := strings.TrimSpace(s[:m[0]])
		if season != 1 || !validEpisode(ep) || !cleanSeries(&head) {
			return "", 0, false
		}
		return head, ep, true
	}
	// 3) Anime dash form: "Title - 03" (optionally "03v2").
	if m := dashEpRe.FindStringSubmatch(s); m != nil {
		ep, _ := strconv.Atoi(m[2])
		head := strings.TrimSpace(m[1])
		if validEpisode(ep) && cleanSeries(&head) && !hasStandaloneNumber(head) {
			return head, ep, true
		}
		return "", 0, false
	}
	// 4) Bare trailing number: "Title 03". The most ambiguous shape, so
	// the guards are strict: no years, no resolutions, no other numbers
	// in the title (a second number means we cannot know which is the
	// episode).
	if m := bareEpRe.FindStringSubmatch(s); m != nil {
		ep, _ := strconv.Atoi(m[2])
		head := strings.TrimSpace(m[1])
		if validEpisode(ep) && !isYear(ep) && !isResolution(ep) &&
			cleanSeries(&head) && !hasStandaloneNumber(head) {
			return head, ep, true
		}
	}
	return "", 0, false
}

// validEpisode bounds episode numbers to 1-999 (three digits is the
// longest realistic episode count; 0 is never a valid episode).
func validEpisode(ep int) bool { return ep >= 1 && ep <= 999 }

// isDigit reports whether b is an ASCII digit.
func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// isYear refuses bare numbers that look like a release year.
func isYear(n int) bool { return n >= 1900 && n <= 2099 }

// isResolution refuses bare numbers that are common video resolutions.
func isResolution(n int) bool {
	switch n {
	case 480, 576, 720, 1080, 2160:
		return true
	}
	return false
}

// standaloneNumberRe finds any run of digits in a title — used to refuse
// ambiguous bare-number parses ("Show 2 05": which number is the episode?).
var standaloneNumberRe = regexp.MustCompile(`\d+`)

func hasStandaloneNumber(s string) bool { return standaloneNumberRe.MatchString(s) }

// cleanSeries trims separator residue from a candidate series name and
// validates it as a folder name. It mutates *s to the cleaned form.
func cleanSeries(s *string) bool {
	c := strings.TrimSpace(*s)
	c = strings.Trim(c, "-–— ")
	c = collapseSpaces(c)
	if !ValidSeriesName(c) {
		return false
	}
	// A purely numeric "series" is a misparse (e.g. a bare "1080p"
	// residue), never a real show name.
	hasLetter := false
	for _, r := range c {
		if unicode.IsLetter(r) {
			hasLetter = true
			break
		}
	}
	if !hasLetter {
		return false
	}
	*s = c
	return true
}

// collapseSpaces runs every whitespace run down to a single space.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

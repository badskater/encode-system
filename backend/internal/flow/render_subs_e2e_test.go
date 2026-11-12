package flow

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Stub-exec end-to-end tests for the P6 extraction/mux builtins (both
// dialects). mkvtoolnix is NOT a test-host dependency: mkvmerge answers
// -J from an identify.json sidecar (real schema-v20 shape) and records +
// structurally validates mux-mode arguments; mkvextract writes REAL files
// for every track/attachment/chapter spec it is handed. jq is stubbed with
// python3, faithful to the exact queries the bash templates issue (same
// convention as render_quality_e2e_test.go).
//
// The bash suite always runs; the PowerShell suite runs for real when pwsh
// is installed (it is on this host) and skips otherwise.

// subsMkvmergeStub: -J mode cats the identify sidecar; mux mode records
// every argument (one per line AND NUL-separated for exactness) for the Go
// assertions, enforces the per-invocation temp-file PATTERN and the
// strip-flags-before-encoded contract, then writes a recognizable output
// derived from the encoded input. Sidecars: mkvmerge_exit, mkvmerge_no_output,
// mux_sleep (sleep before writing), mux_write_sleep (write PART1, sleep, then
// append PART2 + inputs — exposes concurrent tmp-file corruption).
const subsMkvmergeStub = `#!/usr/bin/env bash
dir="$(cd "$(dirname "$0")" && pwd)"
for a in "$@"; do
  if [ "$a" = "-J" ]; then
    if [ -f "$dir/identify_exit" ]; then exit "$(cat "$dir/identify_exit")"; fi
    cat "$dir/identify.json"
    exit 0
  fi
done
# --- mux mode ---
printf '%s\n' "$@" > "$dir/mkvmerge_args.txt"
printf '%s\0' "$@" > "$dir/mkvmerge_args.nul"
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  prev="$a"
done
case "$(basename "$out")" in
  .mux_extras.*.tmp.mkv) ;;
  *) echo "mkvmerge stub: output must match the atomic temp pattern .mux_extras.*.tmp.mkv, got: $out" >&2; exit 1 ;;
esac
rc=0
[ -f "$dir/mkvmerge_exit" ] && rc=$(cat "$dir/mkvmerge_exit")
if [ "$rc" != "0" ]; then exit "$rc"; fi
if [ -f "$dir/mkvmerge_no_output" ]; then exit 0; fi
if [ -f "$dir/mux_sleep" ]; then sleep "$(cat "$dir/mux_sleep")"; fi
# Encoded input = first EXISTING .mkv argument (the temp does not exist
# yet; subtitle/font inputs carry other extensions).
enc=""
for a in "$@"; do
  case "$a" in
    *.mkv)
      if [ -f "$a" ] && [ "$a" != "$out" ]; then enc="$a"; break; fi
      ;;
  esac
done
if [ -z "$enc" ]; then
  echo "mkvmerge stub: no encoded input found in arguments" >&2
  exit 1
fi
# Contract: the encoded input must be preceded by the three strip flags
# (keep video+audio, replace old extras on re-runs).
prev3=""
prev2=""
prev1=""
for a in "$@"; do
  if [ "$a" = "$enc" ]; then break; fi
  prev3="$prev2"; prev2="$prev1"; prev1="$a"
done
if [ "$prev3" != "--no-subtitles" ] || [ "$prev2" != "--no-chapters" ] || [ "$prev1" != "--no-attachments" ]; then
  echo "mkvmerge stub: encoded input must follow --no-subtitles --no-chapters --no-attachments" >&2
  exit 1
fi
# Write-then-sleep mode: a partial file exists DURING the mux so a
# concurrent run deleting/overwriting the shared tmp is observable.
if [ -f "$dir/mux_write_sleep" ]; then
  printf 'PART1-' > "$out"
  sleep "$(cat "$dir/mux_write_sleep")"
  printf 'PART2\n' >> "$out"
  for a in "$@"; do
    if [ -f "$a" ] && [ "$a" != "$out" ]; then echo "--- input: $a" >> "$out"; cat "$a" >> "$out"; fi
  done
  exit 0
fi
{ cat "$enc"; echo "MUXED-EXTRAS"; } > "$out"
echo "mkvmerge stub: muxed $enc -> $out" >&2
exit 0
`

// subsMkvextractStub: writes real files for every extraction spec.
// Sidecars: mkvextract_exit (fail code), mkvextract_skip_file (exit 0 but
// write nothing), chapters_payload.xml (custom chapter XML content).
const subsMkvextractStub = `#!/usr/bin/env bash
dir="$(cd "$(dirname "$0")" && pwd)"
printf '%s\n' "$@" > "$dir/mkvextract_args.txt"
rc=0
[ -f "$dir/mkvextract_exit" ] && rc=$(cat "$dir/mkvextract_exit")
if [ "$rc" != "0" ]; then exit "$rc"; fi
mode="$2"
case "$mode" in
  tracks)
    shift 2
    for spec in "$@"; do
      tid="${spec%%:*}"
      dest="${spec#*:}"
      if [ -f "$dir/mkvextract_skip_file" ]; then continue; fi
      echo "subtitle-track-$tid" > "$dest"
      case "$dest" in
        *.sub) echo "vobsub-index-$tid" > "${dest%.sub}.idx" ;;
      esac
    done
    ;;
  attachments)
    shift 2
    for spec in "$@"; do
      aid="${spec%%:*}"
      dest="${spec#*:}"
      if [ -f "$dir/mkvextract_skip_file" ]; then continue; fi
      mkdir -p "$(dirname "$dest")"
      echo "font-attachment-$aid" > "$dest"
    done
    ;;
  chapters)
    dest="$3"
    if [ -f "$dir/mkvextract_skip_file" ]; then exit 0; fi
    if [ -f "$dir/chapters_payload.xml" ]; then
      cp "$dir/chapters_payload.xml" "$dest"
    else
      printf '<?xml version="1.0"?>\n<Chapters>\n  <EditionEntry>\n    <ChapterAtom>\n      <ChapterTimeStart>00:00:00.000000000</ChapterTimeStart>\n      <ChapterDisplay><ChapterString>OP</ChapterString></ChapterDisplay>\n    </ChapterAtom>\n  </EditionEntry>\n</Chapters>\n' > "$dest"
    fi
    ;;
  *)
    echo "mkvextract stub: unknown mode $mode" >&2
    exit 1
    ;;
esac
exit 0
`

// subsJQStub implements EXACTLY the jq invocations the four bash templates
// issue, FAITHFUL to real jq 1.7 semantics (the previous stub crashed on
// empty input, masking the silent-success bug on empty manifests — P6
// review Q3/Q5):
//   - empty input → NO output, exit 0 (with -e: exit 4)
//   - -e → exit 1 when the last output value is false/null, 4 when nothing
//     was output, 0 otherwise
//   - parse errors exit 2; runtime errors (iterate/index scalars, mixed
//     add) exit 5; unsupported queries exit 3 (loud, never silent)
//   - 'length': null→0, number→abs, string/array/object→len, bool→error
//   - // substitutes ONLY null/false; tostring: bools lowercase,
//     integral floats normalize to int form (jq: 4.0|tostring == "4")
//   - gsub("[\\u0000-\\u001f\\u007f]";"") strips C0+DEL (Q1b sanitization)
const subsJQStub = `#!/usr/bin/env python3
import sys, json, re

args = sys.argv[1:]
raw = '-r' in args
compact = '-c' in args
eflag = '-e' in args

def die(msg, code=5):
    sys.stderr.write('jq: error: ' + msg + '\n')
    sys.exit(code)

last = None
n_out = 0

def emit(v):
    global last, n_out
    last = v
    n_out += 1
    if raw and isinstance(v, str):
        print(v)
    else:
        print(json.dumps(v))

def finish():
    if eflag:
        if n_out == 0:
            sys.exit(4)
        if last is None or last is False:
            sys.exit(1)
    sys.exit(0)

def alt(v, default):
    # jq '//': substitute only for null/false (empty strings are truthy).
    return default if v is None or v is False else v

def tostring(v):
    if isinstance(v, bool):
        return 'true' if v else 'false'
    if v is None:
        return 'null'
    if isinstance(v, str):
        return v
    if isinstance(v, float) and v.is_integer():
        return str(int(v))
    return json.dumps(v)

def index(v, key):
    if v is None:
        return None
    if isinstance(v, dict):
        return v.get(key)
    die('Cannot index %s with "%s"' % (type(v).__name__, key))

def iterate(v):
    # .[] — arrays yield elements, objects yield values, scalars error.
    if isinstance(v, list):
        return v
    if isinstance(v, dict):
        return list(v.values())
    die('Cannot iterate over %s' % json.dumps(v))

def sanitize(v):
    # gsub("[\\u0000-\\u001f\\u007f]"; "") — strip C0 controls + DEL.
    return re.sub('[\x00-\x1f\x7f]', '', v)

def slurp(text):
    dec = json.JSONDecoder()
    vals, idx = [], 0
    text = text.strip()
    while idx < len(text):
        obj, end = dec.raw_decode(text, idx)
        vals.append(obj)
        idx = end
        while idx < len(text) and text[idx] in ' \t\r\n':
            idx += 1
    return vals

if '-n' in args:
    out = {}
    i = 0
    while i < len(args):
        if args[i] == '--arg':
            out[args[i+1]] = args[i+2]
            i += 3
        elif args[i] == '--argjson':
            out[args[i+1]] = json.loads(args[i+2])
            i += 3
        else:
            i += 1
    print(json.dumps(out))
    sys.exit(0)

if '-s' in args:
    data = slurp(sys.stdin.read())
    q = [a for a in args if not a.startswith('-')][0]
    if q == '.':
        print(json.dumps(data))
    elif q == '.[].file':
        for v in data:
            emit(index(v, 'file'))
    else:
        die('unsupported slurp query ' + q, 3)
    finish()

pos = [a for a in args if not a.startswith('-')]
q = pos[0]
try:
    text = open(pos[1], encoding='utf-8').read() if len(pos) > 1 else sys.stdin.read()
except UnicodeDecodeError:
    die('invalid UTF-8 input', 2)
if text.strip() == '':
    # Real jq on empty input: no output, exit 0 (exit 4 with -e).
    finish()
try:
    data = json.loads(text)
except ValueError as e:
    die('parse error: %s' % e, 2)

if q == 'type == "array"':
    emit(isinstance(data, list))
elif q == '[.tracks[]? | select(.type=="subtitles")] | length':
    tracks = data.get('tracks') if isinstance(data, dict) else None
    n = 0
    if isinstance(tracks, (list, dict)):
        for t in (tracks if isinstance(tracks, list) else tracks.values()):
            if index(t, 'type') == 'subtitles':
                n += 1
    emit(n)
elif q == '.tracks[]? | select(.type=="subtitles") | ((.properties // {}) | if type == "object" then . else {} end) as $p | [.id, ($p.language // "und" | tostring | gsub("[\\\\u0000-\\\\u001f\\\\u007f]"; "")), ($p.track_name // "" | tostring | gsub("[\\\\u0000-\\\\u001f\\\\u007f]"; "") | .[0:200]), ($p.default_track // false), ($p.forced_track // false), ($p.codec_id // "")] | map(tostring) | join("\\u001f")':
    tracks = data.get('tracks') if isinstance(data, dict) else None
    if isinstance(tracks, (list, dict)):
        for t in (tracks if isinstance(tracks, list) else tracks.values()):
            if index(t, 'type') != 'subtitles':
                continue
            p = alt(index(t, 'properties'), {})
            if not isinstance(p, dict):
                p = {}
            vals = [index(t, 'id'),
                    sanitize(tostring(alt(index(p, 'language'), 'und'))),
                    sanitize(tostring(alt(index(p, 'track_name'), '')))[:200],
                    alt(index(p, 'default_track'), False),
                    alt(index(p, 'forced_track'), False),
                    alt(index(p, 'codec_id'), '')]
            emit('\x1f'.join(tostring(v) for v in vals))
elif q == '.attachments[]? | [.id, (.file_name // ""), (.content_type // .mime_type // "")] | map(tostring) | join("\\u001f")':
    atts = data.get('attachments') if isinstance(data, dict) else None
    if isinstance(atts, (list, dict)):
        for a in (atts if isinstance(atts, list) else atts.values()):
            vals = [index(a, 'id'), alt(index(a, 'file_name'), ''),
                    alt(alt(index(a, 'content_type'), index(a, 'mime_type')), '')]
            emit('\x1f'.join(tostring(v) for v in vals))
elif q == '(.chapters | if . == null then [] elif type == "array" then . else [.] end)[] | if type == "object" and has("num_entries") and (.num_entries != null) then (.num_entries | tostring) else "INVALID" end':
    ch = data.get('chapters') if isinstance(data, dict) else None
    if ch is None:
        ch = []
    elif not isinstance(ch, list):
        ch = [ch]
    for c in ch:
        if isinstance(c, dict) and 'num_entries' in c and c['num_entries'] is not None:
            emit(tostring(c['num_entries']))
        else:
            emit('INVALID')
elif q == 'length':
    if data is None:
        emit(0)
    elif isinstance(data, bool):
        die('boolean has no length', 5)
    elif isinstance(data, (int, float)):
        emit(abs(data))
    else:
        emit(len(data))
elif q == '.[].file':
    for v in iterate(data):
        emit(index(v, 'file'))
elif q == '.[] | (.file // "")':
    # fonts manifest single-pass read: missing/null file -> "" so bash
    # fails closed with "entry without a file name" (PS parity).
    for v in iterate(data):
        emit(alt(index(v, 'file'), ''))
elif q == '.[] | [(.file // ""), (.language // "und" | tostring | gsub("[\\\\u0000-\\\\u001f\\\\u007f]"; "")), (.track_name // "" | tostring | gsub("[\\\\u0000-\\\\u001f\\\\u007f]"; "") | .[0:200]), (.default_track // "no"), (.forced_track // "no")] | map(tostring) | join("\\u001f")':
    for v in iterate(data):
        vals = [alt(index(v, 'file'), ''),
                sanitize(tostring(alt(index(v, 'language'), 'und'))),
                sanitize(tostring(alt(index(v, 'track_name'), '')))[:200],
                alt(index(v, 'default_track'), 'no'),
                alt(index(v, 'forced_track'), 'no')]
        emit('\x1f'.join(tostring(x) for x in vals))
elif q == '.[]':
    for v in iterate(data):
        if compact:
            print(json.dumps(v, separators=(',', ':')))
        else:
            print(json.dumps(v))
        n_out += 1
        last = v
elif q == '.file':
    emit(alt(index(data, 'file'), ''))
elif q == '.language // "und"':
    emit(alt(index(data, 'language'), 'und'))
elif q == '.track_name // ""':
    emit(alt(index(data, 'track_name'), ''))
elif q == '.default_track // "no"':
    emit(alt(index(data, 'default_track'), 'no'))
elif q == '.forced_track // "no"':
    emit(alt(index(data, 'forced_track'), 'no'))
else:
    die('unsupported query: ' + q, 3)
finish()
`

// --- identify fixtures (real schema-v20 shape) -------------------------

func subsIdentify(tracks, attachments, chapters []any) string {
	m := map[string]any{
		"identification_format_version": 20,
		"container": map[string]any{
			"type":       "Matroska",
			"recognized": true,
			"supported":  true,
			"properties": map[string]any{"duration": 1440000000000},
		},
		"tracks":      tracks,
		"attachments": attachments,
		"chapters":    chapters,
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func subsVideoAudio() []any {
	return []any{
		map[string]any{"id": 0, "type": "video", "codec": "HEVC",
			"properties": map[string]any{"codec_id": "V_MPEGH/ISO/HEVC", "language": "jpn"}},
		map[string]any{"id": 1, "type": "audio", "codec": "Opus",
			"properties": map[string]any{"codec_id": "A_OPUS", "language": "jpn"}},
	}
}

func subsTrack(id any, codecID, lang, name string, def, forced bool) map[string]any {
	return map[string]any{
		"id": id, "type": "subtitles", "codec": codecID,
		"properties": map[string]any{
			"codec_id": codecID, "language": lang, "track_name": name,
			"default_track": def, "forced_track": forced,
		},
	}
}

// subsDefaultIdentify: 2 subtitle tracks (forced ASS jpn + default SRT
// eng), a TTF font + a non-font PNG attachment, 4 chapter entries.
func subsDefaultIdentify() string {
	tracks := append(subsVideoAudio(),
		subsTrack(2, "S_TEXT/ASS", "jpn", "Signs & Songs", false, true),
		subsTrack(3, "S_TEXT/UTF8", "eng", "English", true, false),
	)
	return subsIdentify(tracks,
		[]any{
			map[string]any{"id": 1, "file_name": "OpenSans-Regular.ttf", "content_type": "application/x-truetype-font", "size": 12345},
			map[string]any{"id": 2, "file_name": "cover.png", "content_type": "image/png", "size": 54321},
		},
		[]any{map[string]any{"num_entries": 4}},
	)
}

// subsEmptyIdentify: video+audio only, no attachments, no chapters.
func subsEmptyIdentify() string {
	return subsIdentify(subsVideoAudio(), []any{}, []any{})
}

// --- case model ---------------------------------------------------------

type subsPreSub struct {
	file, language, trackName, def, frc string
}

type subsManifestEntry struct {
	File         string `json:"file"`
	Language     string `json:"language"`
	TrackName    string `json:"track_name"`
	DefaultTrack string `json:"default_track"`
	ForcedTrack  string `json:"forced_track"`
}

type subsCase struct {
	name   string
	step   string // builtin under test (single-step flow)
	params map[string]string

	identify       string // identify.json sidecar ("" = subsDefaultIdentify)
	noMkvmerge     bool
	noMkvextract   bool
	identifyExit   int
	mkvextractExit int
	mkvextractSkip bool // exit 0 but write no files
	mkvmergeExit   int
	mkvmergeNoOut  bool

	noSource    bool
	noEncoded   bool
	encodedName string // default: the standard release-named MKV
	sourceLink  string // "symlink"|"hardlink": encoded output links to src.mkv
	extraMKV    string // additional MKV (newer than the encoded one)
	extraMKV2   string // additional MKV (older than the encoded one)
	extraSetup  func(t *testing.T, epDir string)

	preSubs         []subsPreSub // pre-placed subs.json + files (mux_extras cases)
	preFonts        []string     // pre-placed fonts.json + files
	preChapters     bool
	corruptSubsJSON bool
	emptySubsJSON   bool
	rawSubsJSON     string // pre-placed subs.json VERBATIM (shape/traversal attacks)
	rawFontsJSON    string // pre-placed fonts.json VERBATIM
	muxWriteSleep   string // mkvmerge stub: write PART1, sleep N, append PART2

	wantFail   bool
	wantSubstr []string
	wantPS     []string
	wantBash   []string
	wantAbsent []string
	// wantAbsentLines: no output LINE (trimmed) may equal any of these —
	// catches forged protocol markers embedded mid-line in echoed args.
	wantAbsentLines []string

	wantFiles              []string
	wantNoFiles            []string
	wantNoMuxTmp           bool // no .mux_extras.*.tmp.mkv leftover in epDir
	wantNoMkvmergeCall     bool
	wantNoMkvextractCall   bool
	wantMkvmergeArgs       []string                    // exact recorded mkvmerge argument sequence
	wantMkvmergeArgsFn     func(epDir string) []string // args expectation needing the case's epDir
	wantMkvmergeArgsNulFn  func(epDir string) []string // exact argv via the NUL log (newline-safe)
	wantMkvmergeArgsCheck  func(args []string, epDir string) error
	checkOutput            string // output file to inspect (default: standard name)
	wantOutputContains     string
	wantOutputUnchanged    bool
	outputOriginal         string // captured pre-run content for wantOutputUnchanged
	wantSubsManifest       []subsManifestEntry
	wantSubsManifestEmpty  bool
	wantFontsManifest      []string
	wantFontsManifestEmpty bool
}

const subsSeries = "Subs Series"

func subsOutputName() string { return OutputName(subsSeries, "01", "1080p") }

// subsSetup builds the stub toolchain + episode tree for one case.
func subsSetup(t *testing.T, tc subsCase, exeSuffix string) (Vars, string, string) {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	scriptsDir := filepath.Join(root, "scripts")
	releaseDir := filepath.Join(root, "release")
	for _, d := range []string{binDir, scriptsDir, releaseDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeStub := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	identify := tc.identify
	if identify == "" {
		identify = subsDefaultIdentify()
	}
	if !tc.noMkvmerge {
		writeStub(binDir, "mkvmerge"+exeSuffix, subsMkvmergeStub)
		writeStub(binDir, "identify.json", identify)
		if tc.identifyExit != 0 {
			writeStub(binDir, "identify_exit", fmt.Sprintf("%d", tc.identifyExit))
		}
		if tc.mkvmergeExit != 0 {
			writeStub(binDir, "mkvmerge_exit", fmt.Sprintf("%d", tc.mkvmergeExit))
		}
		if tc.mkvmergeNoOut {
			writeStub(binDir, "mkvmerge_no_output", "1")
		}
	}
	if !tc.noMkvextract {
		writeStub(binDir, "mkvextract"+exeSuffix, subsMkvextractStub)
		if tc.mkvextractExit != 0 {
			writeStub(binDir, "mkvextract_exit", fmt.Sprintf("%d", tc.mkvextractExit))
		}
		if tc.mkvextractSkip {
			writeStub(binDir, "mkvextract_skip_file", "1")
		}
	}
	writeStub(binDir, "jq", subsJQStub)

	epDir := filepath.Join(scriptsDir, subsSeries, "Ep 01")
	if err := os.MkdirAll(epDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !tc.noSource {
		if err := os.WriteFile(filepath.Join(epDir, "src.mkv"), []byte("fake-source"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	encodedName := tc.encodedName
	if encodedName == "" {
		encodedName = subsOutputName()
	}
	if !tc.noEncoded {
		mkv := filepath.Join(epDir, encodedName)
		if err := os.WriteFile(mkv, []byte("fake-encoded"), 0o644); err != nil {
			t.Fatal(err)
		}
		if tc.sourceLink != "" && !tc.noSource {
			src := filepath.Join(epDir, "src.mkv")
			if err := os.Remove(mkv); err != nil {
				t.Fatal(err)
			}
			var err error
			if tc.sourceLink == "symlink" {
				err = os.Symlink(src, mkv)
			} else {
				err = os.Link(src, mkv)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	// Extra MKVs for discovery tests: encoded is backdated 1h; extraMKV is
	// newer (now), extraMKV2 older (2h) — deterministic newest-first order.
	now := time.Now()
	if !tc.noEncoded {
		if err := os.Chtimes(filepath.Join(epDir, encodedName), now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if tc.extraMKV != "" {
		p := filepath.Join(epDir, tc.extraMKV)
		if err := os.WriteFile(p, []byte("extra-encoded"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if tc.extraMKV2 != "" {
		p := filepath.Join(epDir, tc.extraMKV2)
		if err := os.WriteFile(p, []byte("older-encoded"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// Pre-placed extraction artifacts (mux_extras single-step cases).
	if len(tc.preSubs) > 0 || tc.corruptSubsJSON || tc.emptySubsJSON || tc.rawSubsJSON != "" {
		entries := make([]subsManifestEntry, 0, len(tc.preSubs))
		for _, s := range tc.preSubs {
			if err := os.WriteFile(filepath.Join(epDir, s.file), []byte("sub-content"), 0o644); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, subsManifestEntry{
				File: s.file, Language: s.language, TrackName: s.trackName,
				DefaultTrack: s.def, ForcedTrack: s.frc,
			})
		}
		raw := "{not valid json"
		if tc.rawSubsJSON != "" {
			raw = tc.rawSubsJSON
		} else if tc.emptySubsJSON {
			raw = ""
		} else if !tc.corruptSubsJSON {
			b, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			raw = string(b)
		}
		if err := os.WriteFile(filepath.Join(epDir, "subs.json"), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if len(tc.preFonts) > 0 || tc.rawFontsJSON != "" {
		if err := os.MkdirAll(filepath.Join(epDir, "fonts"), 0o755); err != nil {
			t.Fatal(err)
		}
		type fontEntry struct {
			File string `json:"file"`
		}
		entries := make([]fontEntry, 0, len(tc.preFonts))
		for _, f := range tc.preFonts {
			if err := os.WriteFile(filepath.Join(epDir, f), []byte("font-content"), 0o644); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, fontEntry{File: f})
		}
		raw := ""
		if tc.rawFontsJSON != "" {
			raw = tc.rawFontsJSON
		} else {
			b, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			raw = string(b)
		}
		if err := os.WriteFile(filepath.Join(epDir, "fonts.json"), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if tc.muxWriteSleep != "" {
		writeStub(binDir, "mux_write_sleep", tc.muxWriteSleep)
	}
	if tc.preChapters {
		if err := os.WriteFile(filepath.Join(epDir, "chapters.xml"), []byte("<Chapters></Chapters>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if tc.extraSetup != nil {
		tc.extraSetup(t, epDir)
	}

	v := Vars{BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: releaseDir, Group: "TestGroup", Tag: "1080p"}

	path := binDir + ":/usr/bin:/bin:/usr/local/bin"
	if tc.noMkvmerge || tc.noMkvextract {
		// Minimal PATH so a host-installed mkvtoolnix can never satisfy
		// the lookup (same pattern as the quality_gate e2e suite).
		minDir := filepath.Join(root, "minbin")
		if err := os.MkdirAll(minDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, tool := range []string{"bash", "find", "sort", "head", "tr", "grep", "sed", "wc", "cut", "tail", "python3", "cat", "dirname", "cp", "mkdir", "rm", "mv", "stat", "basename", "flock", "realpath", "sleep"} {
			p, err := exec.LookPath(tool)
			if err != nil {
				t.Skipf("coreutils tool %q not available: %v", tool, err)
			}
			if err := os.Symlink(p, filepath.Join(minDir, tool)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(filepath.Join(binDir, "jq"), filepath.Join(minDir, "jq")); err != nil {
			t.Fatal(err)
		}
		// Keep the PRESENT tool stubs reachable, with their sidecars (the
		// stubs resolve sidecars relative to their own $0 directory).
		for _, tool := range []string{"mkvmerge" + exeSuffix, "mkvextract" + exeSuffix} {
			p := filepath.Join(binDir, tool)
			if _, err := os.Stat(p); err == nil {
				if err := os.Symlink(p, filepath.Join(minDir, tool)); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, sidecar := range []string{"identify.json", "identify_exit", "mkvmerge_exit", "mkvmerge_no_output", "mkvextract_exit", "mkvextract_skip_file", "chapters_payload.xml", "mux_write_sleep", "mux_sleep"} {
			p := filepath.Join(binDir, sidecar)
			if _, err := os.Stat(p); err == nil {
				if err := os.Symlink(p, filepath.Join(minDir, sidecar)); err != nil {
					t.Fatal(err)
				}
			}
		}
		path = minDir
	}
	return v, epDir, path
}

func subsRender(t *testing.T, tc subsCase, v Vars, d Dialect) string {
	t.Helper()
	f := &model.Flow{Name: "subs-e2e", Steps: []model.Step{
		{Type: model.StepType(tc.step), Params: tc.params},
	}}
	j := &model.Job{ID: 80, Series: subsSeries, Episode: "01",
		EpisodeDir: subsSeries + "/Ep 01", ScriptType: "vpy"}
	script, err := RenderDialect(f, j, v, BuiltinResolver(), d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return script
}

// subsReadNulArgs reads the NUL-separated argv log the mkvmerge stub
// writes (exact even when an argument contains newlines).
func subsReadNulArgs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("mkvmerge was not invoked (no NUL argv log): %v", err)
	}
	if len(raw) == 0 {
		return []string{}
	}
	return strings.Split(string(raw[:len(raw)-1]), "\x00")
}

func subsAssert(t *testing.T, tc subsCase, text string, runErr error, epDir, binDir string, ps bool) {
	t.Helper()
	if tc.wantFail {
		if runErr == nil {
			t.Fatalf("expected failure but script exited 0\n%s", text)
		}
		if !strings.Contains(text, "ENCODE_STEP_FAILED "+tc.step) {
			t.Errorf("output missing ENCODE_STEP_FAILED %s\n%s", tc.step, text)
		}
		if strings.Contains(text, "ENCODE_STEP "+tc.step+" 100") {
			t.Errorf("failing case emitted the pass marker\n%s", text)
		}
	} else {
		if runErr != nil {
			t.Fatalf("expected pass but script failed: %v\n%s", runErr, text)
		}
		if strings.Contains(text, "ENCODE_STEP_FAILED") {
			t.Errorf("unexpected ENCODE_STEP_FAILED in passing case\n%s", text)
		}
		if !strings.Contains(text, "ENCODE_STEP "+tc.step+" 100") {
			t.Errorf("output missing pass marker ENCODE_STEP %s 100\n%s", tc.step, text)
		}
	}
	for _, want := range tc.wantSubstr {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q\n%s", want, text)
		}
	}
	for _, want := range tc.wantPS {
		if ps && !strings.Contains(text, want) {
			t.Errorf("PS output missing %q\n%s", want, text)
		}
	}
	for _, want := range tc.wantBash {
		if !ps && !strings.Contains(text, want) {
			t.Errorf("bash output missing %q\n%s", want, text)
		}
	}
	for _, absent := range tc.wantAbsent {
		if strings.Contains(text, absent) {
			t.Errorf("output must NOT contain %q\n%s", absent, text)
		}
	}
	if len(tc.wantAbsentLines) > 0 {
		for _, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			for _, absent := range tc.wantAbsentLines {
				if trimmed == absent {
					t.Errorf("output must NOT contain a line equal to %q (forged marker)\n%s", absent, text)
				}
			}
		}
	}
	if tc.wantNoMuxTmp {
		leftovers, _ := filepath.Glob(filepath.Join(epDir, ".mux_extras.*.tmp.mkv"))
		if len(leftovers) > 0 {
			t.Errorf("mux temp leftovers must be cleaned up: %v\n%s", leftovers, text)
		}
		if _, err := os.Stat(filepath.Join(epDir, MuxExtrasTempName)); err == nil {
			t.Errorf("legacy fixed-name temp leftover present\n%s", text)
		}
	}
	for _, f := range tc.wantFiles {
		if _, err := os.Stat(filepath.Join(epDir, f)); err != nil {
			t.Errorf("expected file %s missing: %v\n%s", f, err, text)
		}
	}
	for _, f := range tc.wantNoFiles {
		if _, err := os.Stat(filepath.Join(epDir, f)); err == nil {
			t.Errorf("file %s must NOT exist\n%s", f, text)
		}
	}
	argsPath := filepath.Join(binDir, "mkvmerge_args.txt")
	if tc.wantNoMkvmergeCall {
		if _, err := os.Stat(argsPath); err == nil {
			raw, _ := os.ReadFile(argsPath)
			t.Errorf("mkvmerge must NOT be invoked (no-extras path)\nargs:\n%s\n%s", raw, text)
		}
	}
	if len(tc.wantMkvmergeArgs) > 0 {
		raw, err := os.ReadFile(argsPath)
		if err != nil {
			t.Fatalf("mkvmerge was not invoked (no args recorded): %v\n%s", err, text)
		}
		got := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if len(got) != len(tc.wantMkvmergeArgs) {
			t.Fatalf("mkvmerge arg count = %d, want %d\ngot:  %q\nwant: %q\n%s",
				len(got), len(tc.wantMkvmergeArgs), got, tc.wantMkvmergeArgs, text)
		}
		for i := range got {
			if got[i] != tc.wantMkvmergeArgs[i] {
				t.Errorf("mkvmerge arg %d = %q, want %q\n%s", i, got[i], tc.wantMkvmergeArgs[i], text)
			}
		}
	}
	if tc.wantMkvmergeArgsNulFn != nil {
		want := tc.wantMkvmergeArgsNulFn(epDir)
		got := subsReadNulArgs(t, filepath.Join(binDir, "mkvmerge_args.nul"))
		if len(got) != len(want) {
			t.Fatalf("mkvmerge argv count = %d, want %d\ngot:  %q\nwant: %q\n%s", len(got), len(want), got, want, text)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("mkvmerge argv[%d] = %q, want %q\n%s", i, got[i], want[i], text)
			}
		}
	}
	if tc.wantMkvmergeArgsCheck != nil {
		got := subsReadNulArgs(t, filepath.Join(binDir, "mkvmerge_args.nul"))
		if err := tc.wantMkvmergeArgsCheck(got, epDir); err != nil {
			t.Errorf("mkvmerge argv contract: %v\ngot: %q\n%s", err, got, text)
		}
	}
	extractArgsPath := filepath.Join(binDir, "mkvextract_args.txt")
	if tc.wantNoMkvextractCall {
		if _, err := os.Stat(extractArgsPath); err == nil {
			raw, _ := os.ReadFile(extractArgsPath)
			t.Errorf("mkvextract must NOT be invoked (skip path)\nargs:\n%s\n%s", raw, text)
		}
	}
	outName := tc.checkOutput
	if outName == "" {
		outName = subsOutputName()
	}
	if tc.wantOutputContains != "" || tc.wantOutputUnchanged {
		raw, err := os.ReadFile(filepath.Join(epDir, outName))
		if err != nil {
			t.Fatalf("output %s unreadable: %v\n%s", outName, err, text)
		}
		if tc.wantOutputContains != "" && !strings.Contains(string(raw), tc.wantOutputContains) {
			t.Errorf("output %s missing %q (content: %q)\n%s", outName, tc.wantOutputContains, raw, text)
		}
		if tc.wantOutputUnchanged && string(raw) != tc.outputOriginal {
			t.Errorf("output %s was modified (content: %q, original: %q) - must stay untouched\n%s", outName, raw, tc.outputOriginal, text)
		}
	}
	if tc.wantSubsManifestEmpty || len(tc.wantSubsManifest) > 0 {
		raw, err := os.ReadFile(filepath.Join(epDir, "subs.json"))
		if err != nil {
			t.Fatalf("subs.json not written: %v\n%s", err, text)
		}
		var entries []subsManifestEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			t.Fatalf("subs.json unparseable: %v\n%s\n%s", err, raw, text)
		}
		if tc.wantSubsManifestEmpty && len(entries) != 0 {
			t.Errorf("subs.json must be empty, got %+v\n%s", entries, text)
		}
		if len(tc.wantSubsManifest) > 0 {
			if len(entries) != len(tc.wantSubsManifest) {
				t.Fatalf("subs.json entries = %d, want %d (%+v)\n%s", len(entries), len(tc.wantSubsManifest), entries, text)
			}
			for i, want := range tc.wantSubsManifest {
				if entries[i] != want {
					t.Errorf("subs.json entry %d = %+v, want %+v\n%s", i, entries[i], want, text)
				}
			}
		}
	}
	if tc.wantFontsManifestEmpty || len(tc.wantFontsManifest) > 0 {
		raw, err := os.ReadFile(filepath.Join(epDir, "fonts.json"))
		if err != nil {
			t.Fatalf("fonts.json not written: %v\n%s", err, text)
		}
		var entries []struct {
			File string `json:"file"`
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			t.Fatalf("fonts.json unparseable: %v\n%s\n%s", err, raw, text)
		}
		if tc.wantFontsManifestEmpty && len(entries) != 0 {
			t.Errorf("fonts.json must be empty, got %+v\n%s", entries, text)
		}
		if len(tc.wantFontsManifest) > 0 {
			if len(entries) != len(tc.wantFontsManifest) {
				t.Fatalf("fonts.json entries = %d, want %d\n%s", len(entries), len(tc.wantFontsManifest), text)
			}
			for i, want := range tc.wantFontsManifest {
				if entries[i].File != want {
					t.Errorf("fonts.json entry %d = %q, want %q\n%s", i, entries[i].File, want, text)
				}
			}
		}
	}
}

// subsRunCase executes one case in one dialect.
func subsRunCase(t *testing.T, tc subsCase, ps bool) {
	t.Helper()
	exeSuffix := ""
	d := Dialect(BashDialect{})
	scriptName := "job.sh"
	if ps {
		exeSuffix = ".exe"
		d = PowerShellDialect{}
		scriptName = "job.ps1"
	}
	v, epDir, path := subsSetup(t, tc, exeSuffix)
	if tc.wantMkvmergeArgsFn != nil {
		tc.wantMkvmergeArgs = tc.wantMkvmergeArgsFn(epDir)
	}
	if tc.wantOutputUnchanged {
		outName := tc.checkOutput
		if outName == "" {
			outName = subsOutputName()
		}
		raw, err := os.ReadFile(filepath.Join(epDir, outName))
		if err != nil {
			t.Fatalf("cannot capture original output %s: %v", outName, err)
		}
		tc.outputOriginal = string(raw)
	}
	script := subsRender(t, tc, v, d)
	scriptPath := filepath.Join(epDir, scriptName)
	mode := os.FileMode(0o755)
	if ps {
		mode = 0o644
	}
	if err := os.WriteFile(scriptPath, []byte(script), mode); err != nil {
		t.Fatal(err)
	}
	var cmd *exec.Cmd
	if ps {
		pwsh, err := exec.LookPath("pwsh")
		if err != nil {
			t.Skip("pwsh not installed; skipping PowerShell integration test")
		}
		libPath, err := filepath.Abs("../../../powershell/EncodeLib.ps1")
		if err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
		cmd.Env = append(os.Environ(), "PATH="+path, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
	} else {
		bash, err := exec.LookPath("bash")
		if err != nil {
			t.Skip("bash not installed; skipping bash execution test")
		}
		cmd = exec.Command(bash, scriptPath)
		cmd.Env = append(os.Environ(), "PATH="+path)
	}
	out, runErr := cmd.CombinedOutput()
	text := string(out)
	t.Logf("output:\n%s", text)
	if strings.Contains(text, "command not found") {
		t.Fatalf("command not found in output (linkage broken):\n%s", text)
	}
	subsAssert(t, tc, text, runErr, epDir, v.BinDir, ps)
}

func subsRunBothDialects(t *testing.T, cases []subsCase) {
	t.Helper()
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			subsRunCase(t, tc, false)
		})
		t.Run(tc.name+"_ps", func(t *testing.T) {
			subsRunCase(t, tc, true)
		})
	}
}

// --- subtitle_extract scenarios -----------------------------------------

func subtitleExtractCases() []subsCase {
	cases := []subsCase{
		{
			name: "two_subs_flags_preserved", step: "subtitle_extract",
			wantSubstr: []string{"ENCODE_STEP subtitle_extract 50", "extracted 2 subtitle track(s)"},
			wantFiles:  []string{"subs_2_jpn.ass", "subs_3_eng.srt"},
			wantSubsManifest: []subsManifestEntry{
				{File: "subs_2_jpn.ass", Language: "jpn", TrackName: "Signs & Songs", DefaultTrack: "no", ForcedTrack: "yes"},
				{File: "subs_3_eng.srt", Language: "eng", TrackName: "English", DefaultTrack: "yes", ForcedTrack: "no"},
			},
		},
		{
			name: "no_subtitle_tracks_graceful_skip", step: "subtitle_extract",
			identify:              subsEmptyIdentify(),
			wantSubstr:            []string{"no subtitle tracks", "- skipping"},
			wantNoMkvextractCall:  true,
			wantSubsManifestEmpty: true,
			wantAbsent:            []string{"ENCODE_STEP subtitle_extract 50"},
		},
		{
			name: "missing_mkvmerge", step: "subtitle_extract", noMkvmerge: true,
			wantFail: true, wantSubstr: []string{"mkvtoolnix"},
		},
		{
			name: "missing_mkvextract", step: "subtitle_extract", noMkvextract: true,
			wantFail: true, wantSubstr: []string{"mkvtoolnix"},
		},
		{
			name: "no_source", step: "subtitle_extract", noSource: true, noEncoded: true,
			wantFail: true, wantSubstr: []string{"no source media file found"},
		},
		{
			name: "identify_crash", step: "subtitle_extract", identifyExit: 2,
			wantFail: true, wantSubstr: []string{"mkvmerge -J exited with code 2"},
		},
		{
			name: "mkvextract_crash", step: "subtitle_extract", mkvextractExit: 1,
			wantFail: true, wantSubstr: []string{"mkvextract exited with code 1"},
		},
		{
			name: "mkvextract_missing_output", step: "subtitle_extract", mkvextractSkip: true,
			wantFail: true, wantSubstr: []string{"was not created"},
		},
		{
			name: "invalid_track_id", step: "subtitle_extract",
			identify: subsIdentify(append(subsVideoAudio(),
				subsTrack("abc", "S_TEXT/UTF8", "eng", "Bad", false, false)), []any{}, []any{}),
			wantFail: true, wantSubstr: []string{"invalid subtitle track id"},
		},
	}
	// Track-selection table: every codec id maps to its extraction
	// extension; unknown codecs fall back to raw .sub. VobSub also gets
	// its .idx sidecar (mkvextract writes the pair).
	codecTable := []struct {
		id      int
		codecID string
		ext     string
	}{
		{10, "S_TEXT/UTF8", "srt"},
		{11, "S_TEXT/ASCII", "srt"},
		{12, "S_TEXT/ASS", "ass"},
		{13, "S_ASS", "ass"},
		{14, "S_TEXT/SSA", "ssa"},
		{15, "S_SSA", "ssa"},
		{16, "S_TEXT/WEBVTT", "vtt"},
		{17, "S_HDMV/PGS", "sup"},
		{18, "S_VOBSUB", "sub"},
		{19, "S_DVBSUB", "sub"},
		{20, "S_IMAGE/BMP", "sub"},
	}
	var tracks []any
	var wantFiles []string
	for _, row := range codecTable {
		tracks = append(tracks, subsTrack(row.id, row.codecID, "eng", "", false, false))
		wantFiles = append(wantFiles, fmt.Sprintf("subs_%d_eng.%s", row.id, row.ext))
	}
	wantFiles = append(wantFiles, "subs_18_eng.idx")
	cases = append(cases, subsCase{
		name: "codec_extension_table", step: "subtitle_extract",
		identify:   subsIdentify(append(subsVideoAudio(), tracks...), []any{}, []any{}),
		wantFiles:  wantFiles,
		wantSubstr: []string{fmt.Sprintf("extracted %d subtitle track(s)", len(codecTable))},
	})
	// Language validation: invalid/missing codes fall back to und.
	langTracks := append(subsVideoAudio(),
		subsTrack(30, "S_TEXT/UTF8", "not-a-code", "", false, false),
	)
	noLang := subsTrack(31, "S_TEXT/UTF8", "eng", "", false, false)
	delete(noLang["properties"].(map[string]any), "language")
	langTracks = append(langTracks, noLang)
	cases = append(cases, subsCase{
		name: "language_fallback", step: "subtitle_extract",
		identify:  subsIdentify(langTracks, []any{}, []any{}),
		wantFiles: []string{"subs_30_und.srt", "subs_31_und.srt"},
	})
	// --- hostile untrusted metadata (P6 review Q1b/Q8/Q10/Q11) ---------
	// track_name/language come from the SOURCE FILE (untrusted). Control
	// characters must be stripped and length capped BEFORE they reach
	// manifests, TSV fields, filenames or echoed command lines.
	cases = append(cases,
		subsCase{
			name: "hostile_name_quotes_flags", step: "subtitle_extract",
			identify: subsIdentify(append(subsVideoAudio(),
				subsTrack(2, "S_TEXT/UTF8", "eng", `x" --attach-file C:\secret y" z`, false, false)), []any{}, []any{}),
			wantSubsManifest: []subsManifestEntry{
				{File: "subs_2_eng.srt", Language: "eng", TrackName: `x" --attach-file C:\secret y" z`, DefaultTrack: "no", ForcedTrack: "no"},
			},
		},
		subsCase{
			name: "hostile_name_newline_marker", step: "subtitle_extract",
			identify: subsIdentify(append(subsVideoAudio(),
				subsTrack(2, "S_TEXT/UTF8", "eng", "line1\nENCODE_STEP mux_extras 100", false, false)), []any{}, []any{}),
			wantSubsManifest: []subsManifestEntry{
				{File: "subs_2_eng.srt", Language: "eng", TrackName: "line1ENCODE_STEP mux_extras 100", DefaultTrack: "no", ForcedTrack: "no"},
			},
			wantAbsentLines: []string{"ENCODE_STEP mux_extras 100"},
		},
		subsCase{
			// Q10: unit-separator in the name must not shift the bash TSV
			// fields — both dialects produce the IDENTICAL manifest.
			name: "hostile_name_unit_separator", step: "subtitle_extract",
			identify: subsIdentify(append(subsVideoAudio(),
				subsTrack(2, "S_TEXT/UTF8", "eng", "a\x1ftrue\x1fyes", false, false)), []any{}, []any{}),
			wantSubsManifest: []subsManifestEntry{
				{File: "subs_2_eng.srt", Language: "eng", TrackName: "atrueyes", DefaultTrack: "no", ForcedTrack: "no"},
			},
		},
		subsCase{
			name: "hostile_name_control_chars", step: "subtitle_extract",
			identify: subsIdentify(append(subsVideoAudio(),
				subsTrack(2, "S_TEXT/UTF8", "eng", "a\rb	c\x00d\x7fe", false, false)), []any{}, []any{}),
			wantSubsManifest: []subsManifestEntry{
				{File: "subs_2_eng.srt", Language: "eng", TrackName: "abcde", DefaultTrack: "no", ForcedTrack: "no"},
			},
		},
		subsCase{
			name: "hostile_name_length_cap", step: "subtitle_extract",
			identify: subsIdentify(append(subsVideoAudio(),
				subsTrack(2, "S_TEXT/UTF8", "eng", strings.Repeat("x", 300), false, false)), []any{}, []any{}),
			wantSubsManifest: []subsManifestEntry{
				{File: "subs_2_eng.srt", Language: "eng", TrackName: strings.Repeat("x", 200), DefaultTrack: "no", ForcedTrack: "no"},
			},
		},
		subsCase{
			// Q8: .NET regex $ allows a trailing newline — sanitization must
			// strip it so 'eng\n' validates as eng (bash parity).
			name: "language_trailing_newline", step: "subtitle_extract",
			identify: subsIdentify(append(subsVideoAudio(),
				subsTrack(2, "S_TEXT/UTF8", "eng\n", "N", false, false)), []any{}, []any{}),
			wantFiles: []string{"subs_2_eng.srt"},
			wantSubsManifest: []subsManifestEntry{
				{File: "subs_2_eng.srt", Language: "eng", TrackName: "N", DefaultTrack: "no", ForcedTrack: "no"},
			},
		},
		subsCase{
			// Q8: a track WITHOUT a properties object — PS StrictMode must
			// not throw; both dialects default everything.
			name: "track_without_properties", step: "subtitle_extract",
			identify: subsIdentify(append(subsVideoAudio(),
				map[string]any{"id": 2, "type": "subtitles", "codec": "S_TEXT/UTF8"}), []any{}, []any{}),
			wantFiles: []string{"subs_2_und.sub"},
			wantSubsManifest: []subsManifestEntry{
				{File: "subs_2_und.sub", Language: "und", TrackName: "", DefaultTrack: "no", ForcedTrack: "no"},
			},
		},
		subsCase{
			// Q8: numeric default_track=1 — PS truthiness said yes, bash
			// =="true" said no. Normalized truthiness: true/1/"true"/"yes".
			name: "numeric_default_track", step: "subtitle_extract",
			identify: subsIdentify(append(subsVideoAudio(),
				map[string]any{"id": 2, "type": "subtitles", "codec": "S_TEXT/UTF8",
					"properties": map[string]any{"codec_id": "S_TEXT/UTF8", "language": "eng",
						"track_name": "N", "default_track": 1, "forced_track": 0}}), []any{}, []any{}),
			wantSubsManifest: []subsManifestEntry{
				{File: "subs_2_eng.srt", Language: "eng", TrackName: "N", DefaultTrack: "yes", ForcedTrack: "no"},
			},
		},
		subsCase{
			name: "string_flag_variants", step: "subtitle_extract",
			identify: subsIdentify(append(subsVideoAudio(),
				map[string]any{"id": 2, "type": "subtitles", "codec": "S_TEXT/UTF8",
					"properties": map[string]any{"codec_id": "S_TEXT/UTF8", "language": "eng",
						"track_name": "A", "default_track": "true", "forced_track": "yes"}},
				map[string]any{"id": 3, "type": "subtitles", "codec": "S_TEXT/UTF8",
					"properties": map[string]any{"codec_id": "S_TEXT/UTF8", "language": "eng",
						"track_name": "B", "default_track": "1", "forced_track": "false"}}), []any{}, []any{}),
			wantSubsManifest: []subsManifestEntry{
				{File: "subs_2_eng.srt", Language: "eng", TrackName: "A", DefaultTrack: "yes", ForcedTrack: "yes"},
				{File: "subs_3_eng.srt", Language: "eng", TrackName: "B", DefaultTrack: "yes", ForcedTrack: "no"},
			},
		},
	)
	return cases
}

func TestSubtitleExtractStubExec(t *testing.T) {
	subsRunBothDialects(t, subtitleExtractCases())
}

// --- font_extract scenarios ----------------------------------------------

func fontExtractCases() []subsCase {
	return []subsCase{
		{
			name: "font_by_mime_png_skipped", step: "font_extract",
			wantSubstr:        []string{"extracted 1 font attachment(s)"},
			wantFiles:         []string{"fonts/1_OpenSans-Regular.ttf"},
			wantNoFiles:       []string{"fonts/2_cover.png"},
			wantFontsManifest: []string{"fonts/1_OpenSans-Regular.ttf"},
		},
		{
			name: "font_by_extension_case_insensitive", step: "font_extract",
			identify: subsIdentify(subsVideoAudio(), []any{
				map[string]any{"id": 5, "file_name": "MyFont.OTF", "content_type": "application/octet-stream"},
			}, []any{}),
			wantFiles:         []string{"fonts/5_MyFont.OTF"},
			wantFontsManifest: []string{"fonts/5_MyFont.OTF"},
		},
		{
			name: "font_legacy_mime_type_field", step: "font_extract",
			identify: subsIdentify(subsVideoAudio(), []any{
				map[string]any{"id": 6, "file_name": "legacy.ttf", "mime_type": "application/x-truetype-font"},
			}, []any{}),
			wantFiles:         []string{"fonts/6_legacy.ttf"},
			wantFontsManifest: []string{"fonts/6_legacy.ttf"},
		},
		{
			name: "font_unsafe_name_sanitized", step: "font_extract",
			identify: subsIdentify(subsVideoAudio(), []any{
				map[string]any{"id": 7, "file_name": "../../evil.ttf", "content_type": "font/ttf"},
			}, []any{}),
			wantFiles:         []string{"fonts/7_.._.._evil.ttf"},
			wantFontsManifest: []string{"fonts/7_.._.._evil.ttf"},
		},
		{
			name: "font_no_name_mime_fallback", step: "font_extract",
			identify: subsIdentify(subsVideoAudio(), []any{
				map[string]any{"id": 8, "content_type": "font/otf"},
			}, []any{}),
			wantFiles:         []string{"fonts/8_attachment_8.otf"},
			wantFontsManifest: []string{"fonts/8_attachment_8.otf"},
		},
		{
			name: "no_fonts_graceful_skip", step: "font_extract",
			identify:               subsEmptyIdentify(),
			wantSubstr:             []string{"no font attachments", "- skipping"},
			wantNoMkvextractCall:   true,
			wantFontsManifestEmpty: true,
		},
		{
			name: "only_non_font_attachments", step: "font_extract",
			identify: subsIdentify(subsVideoAudio(), []any{
				map[string]any{"id": 9, "file_name": "cover.jpg", "content_type": "image/jpeg"},
			}, []any{}),
			wantSubstr:             []string{"no font attachments"},
			wantNoMkvextractCall:   true,
			wantFontsManifestEmpty: true,
		},
		{
			name: "missing_mkvmerge", step: "font_extract", noMkvmerge: true,
			wantFail: true, wantSubstr: []string{"mkvtoolnix"},
		},
		{
			name: "missing_mkvextract", step: "font_extract", noMkvextract: true,
			wantFail: true, wantSubstr: []string{"mkvtoolnix"},
		},
		{
			name: "mkvextract_crash", step: "font_extract", mkvextractExit: 1,
			wantFail: true, wantSubstr: []string{"mkvextract exited with code 1"},
		},
		{
			name: "mkvextract_missing_output", step: "font_extract", mkvextractSkip: true,
			wantFail: true, wantSubstr: []string{"was not created"},
		},
		{
			// Q8: uppercase MIME — PS -in is case-insensitive (extracted),
			// bash case patterns were case-sensitive (skipped). Both must
			// extract after lowercasing the MIME.
			name: "font_uppercase_mime", step: "font_extract",
			identify: subsIdentify(subsVideoAudio(), []any{
				map[string]any{"id": 10, "file_name": "cover.png", "content_type": "FONT/TTF"},
				map[string]any{"id": 11, "file_name": "mixed.otf", "content_type": "Application/Vnd.Ms-OpenType"},
			}, []any{}),
			wantFiles:         []string{"fonts/10_cover.png", "fonts/11_mixed.otf"},
			wantFontsManifest: []string{"fonts/10_cover.png", "fonts/11_mixed.otf"},
			wantSubstr:        []string{"extracted 2 font attachment(s)"},
		},
	}
}

func TestFontExtractStubExec(t *testing.T) {
	subsRunBothDialects(t, fontExtractCases())
}

// --- chapter_extract scenarios -------------------------------------------

func chapterExtractCases() []subsCase {
	return []subsCase{
		{
			name: "chapters_present", step: "chapter_extract",
			wantSubstr: []string{"extracted 4 chapter entries"},
			wantFiles:  []string{"chapters.xml"},
		},
		{
			name: "no_chapters_graceful_skip", step: "chapter_extract",
			identify:             subsEmptyIdentify(),
			wantSubstr:           []string{"no chapters", "- skipping"},
			wantNoFiles:          []string{"chapters.xml"},
			wantNoMkvextractCall: true,
		},
		{
			name: "zero_entry_edition_skipped", step: "chapter_extract",
			identify:             subsIdentify(subsVideoAudio(), []any{}, []any{map[string]any{"num_entries": 0}}),
			wantSubstr:           []string{"no chapters"},
			wantNoMkvextractCall: true,
		},
		{
			name: "stale_chapters_removed_on_skip", step: "chapter_extract",
			identify:    subsEmptyIdentify(),
			preChapters: true,
			wantSubstr:  []string{"removed stale chapters.xml"},
			wantNoFiles: []string{"chapters.xml"},
		},
		{
			name: "missing_mkvextract", step: "chapter_extract", noMkvextract: true,
			wantFail: true, wantSubstr: []string{"mkvtoolnix"},
		},
		{
			name: "mkvextract_crash", step: "chapter_extract", mkvextractExit: 1,
			wantFail: true, wantSubstr: []string{"mkvextract exited with code 1"},
		},
		{
			name: "mkvextract_missing_output", step: "chapter_extract", mkvextractSkip: true,
			wantFail: true, wantSubstr: []string{"was not created"},
		},
		// --- num_entries validation parity (P6 review Q6) ---------------
		// Both dialects must reject the SAME invalid shapes: >9 digits,
		// negative, float, scalar edition, missing num_entries. PS used to
		// silently skip invalid editions; bash swallowed jq errors.
		{
			name: "num_entries_ten_digits", step: "chapter_extract",
			identify:             subsIdentify(subsVideoAudio(), []any{}, []any{map[string]any{"num_entries": 1234567890}}),
			wantFail:             true,
			wantSubstr:           []string{"invalid chapter num_entries"},
			wantNoMkvextractCall: true,
		},
		{
			name: "num_entries_negative", step: "chapter_extract",
			identify:             subsIdentify(subsVideoAudio(), []any{}, []any{map[string]any{"num_entries": -5}}),
			wantFail:             true,
			wantSubstr:           []string{"invalid chapter num_entries"},
			wantNoMkvextractCall: true,
		},
		{
			name: "num_entries_float", step: "chapter_extract",
			identify:             subsIdentify(subsVideoAudio(), []any{}, []any{map[string]any{"num_entries": 2.5}}),
			wantFail:             true,
			wantSubstr:           []string{"invalid chapter num_entries"},
			wantNoMkvextractCall: true,
		},
		{
			name: "num_entries_scalar_edition", step: "chapter_extract",
			identify:             subsIdentify(subsVideoAudio(), []any{}, []any{5}),
			wantFail:             true,
			wantSubstr:           []string{"invalid chapter edition"},
			wantNoMkvextractCall: true,
		},
		{
			name: "num_entries_missing", step: "chapter_extract",
			identify:             subsIdentify(subsVideoAudio(), []any{}, []any{map[string]any{}}),
			wantFail:             true,
			wantSubstr:           []string{"invalid chapter edition"},
			wantNoMkvextractCall: true,
		},
		{
			// String "4" is valid in both dialects (tostring then digits).
			name: "num_entries_string_ok", step: "chapter_extract",
			identify:   subsIdentify(subsVideoAudio(), []any{}, []any{map[string]any{"num_entries": "4"}}),
			wantSubstr: []string{"extracted 4 chapter entries"},
			wantFiles:  []string{"chapters.xml"},
		},
	}
}

func TestChapterExtractStubExec(t *testing.T) {
	subsRunBothDialects(t, chapterExtractCases())
}

// --- mux_extras scenarios -------------------------------------------------

func muxExtrasCases() []subsCase {
	stdSubs := []subsPreSub{
		{file: "subs_2_jpn.ass", language: "jpn", trackName: "Signs & Songs", def: "no", frc: "yes"},
		{file: "subs_3_eng.srt", language: "eng", trackName: "English", def: "yes", frc: "no"},
	}
	stdFonts := []string{"fonts/1_OpenSans-Regular.ttf"}
	// fullArgsRest: the exact mkvmerge argument sequence AFTER the -o/tmp
	// pair (the tmp name is per-invocation unique — checked by pattern).
	fullArgsRest := func(epDir string) []string {
		return []string{
			"--quiet",
			"--chapters", filepath.Join(epDir, "chapters.xml"),
			"--attach-file", filepath.Join(epDir, "fonts/1_OpenSans-Regular.ttf"),
			"--no-subtitles", "--no-chapters", "--no-attachments", filepath.Join(epDir, subsOutputName()),
			"--no-video", "--no-audio", "--language", "0:jpn",
			"--track-name", "0:Signs & Songs",
			"--default-track-flag", "0:no", "--forced-display-flag", "0:yes",
			filepath.Join(epDir, "subs_2_jpn.ass"),
			"--no-video", "--no-audio", "--language", "0:eng",
			"--track-name", "0:English",
			"--default-track-flag", "0:yes", "--forced-display-flag", "0:no",
			filepath.Join(epDir, "subs_3_eng.srt"),
		}
	}
	fullArgsCheck := func(args []string, epDir string) error {
		if len(args) < 2 || args[0] != "-o" {
			return fmt.Errorf("argv must start with -o <tmp>, got %q", args)
		}
		base := filepath.Base(args[1])
		if !strings.HasPrefix(base, ".mux_extras.") || !strings.HasSuffix(base, ".tmp.mkv") || base == MuxExtrasTempName {
			return fmt.Errorf("tmp %q must be a UNIQUE .mux_extras.<pid>.<rand>.tmp.mkv name", base)
		}
		if filepath.Dir(args[1]) != epDir {
			return fmt.Errorf("tmp %q must live in the episode dir", args[1])
		}
		want := fullArgsRest(epDir)
		got := args[2:]
		if len(got) != len(want) {
			return fmt.Errorf("arg count after -o/tmp = %d, want %d", len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				return fmt.Errorf("arg %d = %q, want %q", i+2, got[i], want[i])
			}
		}
		return nil
	}
	cases := []subsCase{
		{
			name: "full_mux_atomic_replace", step: "mux_extras",
			preSubs: stdSubs, preFonts: stdFonts, preChapters: true,
			wantSubstr:         []string{"ENCODE_STEP mux_extras 20", "ENCODE_STEP mux_extras 80", "muxed 2 subtitle track(s), 1 font attachment(s), chapters: yes"},
			wantOutputContains: "MUXED-EXTRAS",
			wantNoMuxTmp:       true,
			// exact args resolved per-case (needs epDir); tmp checked by pattern
			wantMkvmergeArgsCheck: fullArgsCheck,
		},
		{
			name: "no_extras_output_unchanged", step: "mux_extras",
			wantSubstr:          []string{"no extracted extras found", "output unchanged"},
			wantNoMkvmergeCall:  true,
			wantOutputUnchanged: true,
			wantAbsent:          []string{"ENCODE_STEP mux_extras 20"},
		},
		{
			name: "subs_only", step: "mux_extras",
			preSubs:            stdSubs[:1],
			wantSubstr:         []string{"muxed 1 subtitle track(s), 0 font attachment(s), chapters: no"},
			wantOutputContains: "MUXED-EXTRAS",
		},
		{
			name: "fonts_only", step: "mux_extras",
			preFonts:           stdFonts,
			wantSubstr:         []string{"muxed 0 subtitle track(s), 1 font attachment(s), chapters: no"},
			wantOutputContains: "MUXED-EXTRAS",
		},
		{
			name: "chapters_only", step: "mux_extras",
			preChapters:        true,
			wantSubstr:         []string{"muxed 0 subtitle track(s), 0 font attachment(s), chapters: yes"},
			wantOutputContains: "MUXED-EXTRAS",
		},
		{
			name: "explicit_output_param", step: "mux_extras",
			params:             map[string]string{"output": "custom.mkv"},
			encodedName:        "custom.mkv",
			preSubs:            stdSubs[:1],
			checkOutput:        "custom.mkv",
			wantOutputContains: "MUXED-EXTRAS",
		},
		{
			name: "explicit_output_missing", step: "mux_extras",
			params:   map[string]string{"output": "nope.mkv"},
			preSubs:  stdSubs[:1],
			wantFail: true, wantSubstr: []string{"output param file not found"},
		},
		{
			name: "explicit_output_is_source", step: "mux_extras",
			params:   map[string]string{"output": "src.mkv"},
			preSubs:  stdSubs[:1],
			wantFail: true, wantSubstr: []string{"refusing to modify the source"},
			wantOutputUnchanged: true, checkOutput: "src.mkv",
		},
		{
			name: "explicit_output_source_symlink", step: "mux_extras",
			params:  map[string]string{"output": "alias.mkv"},
			preSubs: stdSubs[:1],
			extraSetup: func(t *testing.T, epDir string) {
				if err := os.Symlink(filepath.Join(epDir, "src.mkv"), filepath.Join(epDir, "alias.mkv")); err != nil {
					t.Fatal(err)
				}
			},
			wantFail: true, wantSubstr: []string{"refusing to modify the source"},
		},
		{
			name: "explicit_output_source_hardlink", step: "mux_extras",
			params:  map[string]string{"output": "alias.mkv"},
			preSubs: stdSubs[:1],
			extraSetup: func(t *testing.T, epDir string) {
				if err := os.Link(filepath.Join(epDir, "src.mkv"), filepath.Join(epDir, "alias.mkv")); err != nil {
					t.Fatal(err)
				}
			},
			wantFail: true, wantSubstr: []string{"refusing to modify the source"},
		},
		{
			name: "unsafe_output_param", step: "mux_extras",
			params:   map[string]string{"output": "../evil.mkv"},
			preSubs:  stdSubs[:1],
			wantFail: true, wantSubstr: []string{"unsafe output name value"},
		},
		{
			name: "discovery_latest_non_source", step: "mux_extras",
			noEncoded: true, extraMKV: "newer.mkv", extraMKV2: "older.mkv",
			preSubs:            stdSubs[:1],
			checkOutput:        "newer.mkv",
			wantOutputContains: "MUXED-EXTRAS",
			wantSubstr:         []string{"encoded output:"},
		},
		{
			name: "discovery_skips_source_link", step: "mux_extras",
			sourceLink: "symlink", extraMKV: "real.mkv",
			preSubs:            stdSubs[:1],
			checkOutput:        "real.mkv",
			wantOutputContains: "MUXED-EXTRAS",
		},
		{
			name: "discovery_skips_source_hardlink", step: "mux_extras",
			sourceLink: "hardlink", extraMKV: "real.mkv",
			preSubs:            stdSubs[:1],
			checkOutput:        "real.mkv",
			wantOutputContains: "MUXED-EXTRAS",
		},
		{
			name: "discovery_only_source_link_fails", step: "mux_extras",
			sourceLink: "symlink",
			preSubs:    stdSubs[:1],
			wantFail:   true, wantSubstr: []string{"encoded output MKV not found"},
		},
		{
			name: "missing_output_loud_error", step: "mux_extras",
			noEncoded: true,
			preSubs:   stdSubs[:1],
			wantFail:  true, wantSubstr: []string{"encoded output MKV not found"},
		},
		{
			name: "mkvmerge_crash_output_untouched", step: "mux_extras",
			preSubs: stdSubs, preFonts: stdFonts, preChapters: true,
			mkvmergeExit:        1,
			wantFail:            true,
			wantSubstr:          []string{"mkvmerge exited with code 1"},
			wantOutputUnchanged: true,
			wantNoMuxTmp:        true,
		},
		{
			name: "mkvmerge_no_output", step: "mux_extras",
			preSubs: stdSubs, mkvmergeNoOut: true,
			wantFail:     true,
			wantSubstr:   []string{"was not created"},
			wantNoMuxTmp: true,
		},
		{
			name: "missing_sub_file_from_manifest", step: "mux_extras",
			preSubs: []subsPreSub{{file: "subs_9_eng.srt", language: "eng", def: "no", frc: "no"}},
			extraSetup: func(t *testing.T, epDir string) {
				// Remove the file the manifest references.
				if err := os.Remove(filepath.Join(epDir, "subs_9_eng.srt")); err != nil {
					t.Fatal(err)
				}
			},
			wantFail: true, wantSubstr: []string{"subtitle file from subs.json missing"},
		},
		{
			name: "corrupt_subs_json", step: "mux_extras",
			corruptSubsJSON: true,
			wantFail:        true,
			wantSubstr:      []string{"subs.json is unreadable"},
		},
		{
			name: "empty_subs_json_fail_closed", step: "mux_extras",
			emptySubsJSON: true,
			wantFail:      true,
			wantSubstr:    []string{"subs.json is unreadable"},
		},
		{
			name: "missing_mkvmerge", step: "mux_extras", noMkvmerge: true,
			preSubs:  stdSubs[:1],
			wantFail: true, wantSubstr: []string{"mkvtoolnix"},
		},
		// --- manifest shape validation (P6 review Q3/Q5/Q7) -------------
		// A manifest must be a JSON ARRAY. Scalars/objects/empty files are
		// fail-closed; an empty array is a graceful no-extras success.
		{
			name: "manifest_scalar_string", step: "mux_extras",
			rawSubsJSON:         `"hello"`,
			wantFail:            true,
			wantSubstr:          []string{"subs.json is unreadable", "JSON array"},
			wantOutputUnchanged: true,
		},
		{
			name: "manifest_bare_object", step: "mux_extras",
			rawSubsJSON: `{"file":"x.srt"}`,
			extraSetup: func(t *testing.T, epDir string) {
				if err := os.WriteFile(filepath.Join(epDir, "x.srt"), []byte("sub"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantFail:            true,
			wantSubstr:          []string{"subs.json is unreadable", "JSON array"},
			wantOutputUnchanged: true,
		},
		{
			name: "manifest_null", step: "mux_extras",
			rawSubsJSON:         `null`,
			wantFail:            true,
			wantSubstr:          []string{"subs.json is unreadable"},
			wantOutputUnchanged: true,
		},
		{
			name: "manifest_empty_array_no_extras", step: "mux_extras",
			rawSubsJSON:         `[]`,
			wantSubstr:          []string{"no extracted extras found", "output unchanged"},
			wantNoMkvmergeCall:  true,
			wantOutputUnchanged: true,
		},
		{
			name: "fonts_manifest_scalar", step: "mux_extras",
			rawFontsJSON:        `5`,
			wantFail:            true,
			wantSubstr:          []string{"fonts.json is unreadable", "JSON array"},
			wantOutputUnchanged: true,
		},
		// --- manifest file-entry traversal + containment (Q2) -----------
		{
			name: "manifest_traversal", step: "mux_extras",
			rawSubsJSON: `[{"file":"../../outside.srt","language":"eng"}]`,
			extraSetup: func(t *testing.T, epDir string) {
				p := filepath.Join(epDir, "..", "..", "outside.srt")
				if err := os.WriteFile(p, []byte("SECRET-OUTSIDE-DATA"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantFail:            true,
			wantSubstr:          []string{"unsafe subtitle file entry"},
			wantOutputUnchanged: true,
			wantNoMkvmergeCall:  true,
		},
		{
			name: "manifest_absolute_path", step: "mux_extras",
			rawSubsJSON:         `[{"file":"/etc/passwd","language":"eng"}]`,
			wantFail:            true,
			wantSubstr:          []string{"unsafe subtitle file entry"},
			wantOutputUnchanged: true,
		},
		{
			name: "manifest_backslash_traversal", step: "mux_extras",
			rawSubsJSON:         `[{"file":"..\\..\\outside.srt","language":"eng"}]`,
			wantFail:            true,
			wantSubstr:          []string{"unsafe subtitle file entry"},
			wantOutputUnchanged: true,
		},
		{
			name: "manifest_drive_path", step: "mux_extras",
			rawSubsJSON:         `[{"file":"C:\\evil.srt","language":"eng"}]`,
			wantFail:            true,
			wantSubstr:          []string{"unsafe subtitle file entry"},
			wantOutputUnchanged: true,
		},
		{
			name: "manifest_newline_file", step: "mux_extras",
			rawSubsJSON:         "[{\"file\":\"a\\nb.srt\",\"language\":\"eng\"}]",
			wantFail:            true,
			wantSubstr:          []string{"unsafe subtitle file entry"},
			wantOutputUnchanged: true,
		},
		{
			// A relative entry that is a SYMLINK out of the episode dir:
			// the resolved-path containment check must catch it.
			name: "manifest_symlink_escape", step: "mux_extras",
			rawSubsJSON: `[{"file":"link.srt","language":"eng"}]`,
			extraSetup: func(t *testing.T, epDir string) {
				outside := filepath.Join(epDir, "..", "outside.srt")
				if err := os.WriteFile(outside, []byte("SECRET-OUTSIDE-DATA"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(epDir, "link.srt")); err != nil {
					t.Fatal(err)
				}
			},
			wantFail:            true,
			wantSubstr:          []string{"escapes the episode dir"},
			wantOutputUnchanged: true,
			wantNoMkvmergeCall:  true,
		},
		{
			name: "fonts_manifest_traversal", step: "mux_extras",
			rawFontsJSON: `[{"file":"../../outside.ttf"}]`,
			extraSetup: func(t *testing.T, epDir string) {
				p := filepath.Join(epDir, "..", "..", "outside.ttf")
				if err := os.WriteFile(p, []byte("SECRET-FONT"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantFail:            true,
			wantSubstr:          []string{"unsafe font file entry"},
			wantOutputUnchanged: true,
		},
		{
			// Q11: a hand-placed manifest with a newline+marker track_name —
			// mux_extras sanitizes before echoing/arg-building; no forged
			// marker line may appear and the name stays ONE argv entry.
			// (The forged marker names a DIFFERENT step: mux_extras' own
			// success line "ENCODE_STEP mux_extras 100" is legitimate.)
			name: "hostile_manifest_track_name", step: "mux_extras",
			preSubs: []subsPreSub{
				{file: "subs_2_eng.srt", language: "eng", trackName: "x\nENCODE_STEP encode 99", def: "no", frc: "no"},
			},
			wantOutputContains: "MUXED-EXTRAS",
			wantAbsentLines:    []string{"ENCODE_STEP encode 99"},
			wantNoMuxTmp:       true,
			wantMkvmergeArgsCheck: func(args []string, epDir string) error {
				want := []string{
					"--no-subtitles", "--no-chapters", "--no-attachments", filepath.Join(epDir, subsOutputName()),
					"--no-video", "--no-audio", "--language", "0:eng",
					"--track-name", "0:xENCODE_STEP encode 99",
					"--default-track-flag", "0:no", "--forced-display-flag", "0:no",
					filepath.Join(epDir, "subs_2_eng.srt"),
				}
				if len(args) < 3 || args[0] != "-o" || args[2] != "--quiet" {
					return fmt.Errorf("argv must start -o <tmp> --quiet, got %q", args)
				}
				got := args[3:]
				if len(got) != len(want) {
					return fmt.Errorf("arg count = %d, want %d: %q", len(got), len(want), got)
				}
				for i := range got {
					if got[i] != want[i] {
						return fmt.Errorf("arg %d = %q, want %q", i, got[i], want[i])
					}
				}
				return nil
			},
		},
		{
			// Q9: an MKV filename with an embedded newline must be picked
			// whole (bash find -printf line parsing used to truncate it and
			// create a junk 'we' file).
			name: "discovery_newline_filename", step: "mux_extras",
			noEncoded: true, extraMKV: "we\nird.mkv",
			preSubs:            stdSubs[:1],
			checkOutput:        "we\nird.mkv",
			wantOutputContains: "MUXED-EXTRAS",
			wantNoFiles:        []string{"we"},
			wantNoMuxTmp:       true,
		},
	}
	return cases
}

func TestMuxExtrasStubExec(t *testing.T) {
	subsRunBothDialects(t, muxExtrasCases())
}

// --- full default-subs flow ----------------------------------------------

// TestDefaultSubsFlowStubExec runs the ENTIRE seeded flow (all four steps)
// end to end in both dialects: extraction artifacts appear, mux_extras
// merges them into the encoded output and replaces it atomically.
func TestDefaultSubsFlowStubExec(t *testing.T) {
	for _, ps := range []bool{false, true} {
		ps := ps
		name := "bash"
		if ps {
			name = "ps"
		}
		t.Run(name, func(t *testing.T) {
			tc := subsCase{step: "mux_extras"} // setup only; flow rendered below
			exeSuffix := ""
			d := Dialect(BashDialect{})
			if ps {
				exeSuffix = ".exe"
				d = PowerShellDialect{}
			}
			v, epDir, path := subsSetup(t, tc, exeSuffix)
			j := &model.Job{ID: 81, Series: subsSeries, Episode: "01",
				EpisodeDir: subsSeries + "/Ep 01", ScriptType: "vpy"}
			script, err := RenderDialect(DefaultSubsFlow(), j, v, BuiltinResolver(), d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			scriptPath := filepath.Join(epDir, "job"+d.Ext())
			if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			var cmd *exec.Cmd
			if ps {
				pwsh, err := exec.LookPath("pwsh")
				if err != nil {
					t.Skip("pwsh not installed; skipping PowerShell integration test")
				}
				libPath, err := filepath.Abs("../../../powershell/EncodeLib.ps1")
				if err != nil {
					t.Fatal(err)
				}
				cmd = exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
				cmd.Env = append(os.Environ(), "PATH="+path, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
			} else {
				bash, err := exec.LookPath("bash")
				if err != nil {
					t.Skip("bash not installed")
				}
				cmd = exec.Command(bash, scriptPath)
				cmd.Env = append(os.Environ(), "PATH="+path)
			}
			out, runErr := cmd.CombinedOutput()
			text := string(out)
			t.Logf("full flow output:\n%s", text)
			if runErr != nil {
				t.Fatalf("full flow failed: %v\n%s", runErr, text)
			}
			for _, marker := range []string{
				"ENCODE_STEP subtitle_extract 100",
				"ENCODE_STEP font_extract 100",
				"ENCODE_STEP chapter_extract 100",
				"ENCODE_STEP mux_extras 100",
				"ENCODE_JOB_DONE",
			} {
				if !strings.Contains(text, marker) {
					t.Errorf("missing marker %q\n%s", marker, text)
				}
			}
			if strings.Contains(text, "command not found") {
				t.Fatalf("linkage broken:\n%s", text)
			}
			for _, f := range []string{"subs_2_jpn.ass", "subs_3_eng.srt", "fonts/1_OpenSans-Regular.ttf", "chapters.xml", "subs.json", "fonts.json"} {
				if _, err := os.Stat(filepath.Join(epDir, f)); err != nil {
					t.Errorf("artifact %s missing: %v", f, err)
				}
			}
			raw, err := os.ReadFile(filepath.Join(epDir, subsOutputName()))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "MUXED-EXTRAS") {
				t.Errorf("encoded output was not replaced with the muxed result (content %q)", raw)
			}
			if leftovers, _ := filepath.Glob(filepath.Join(epDir, ".mux_extras.*.tmp.mkv")); len(leftovers) > 0 {
				t.Errorf("temp file(s) must be gone after the atomic replace: %v", leftovers)
			}
			// Source must be untouched.
			srcRaw, err := os.ReadFile(filepath.Join(epDir, "src.mkv"))
			if err != nil || string(srcRaw) != "fake-source" {
				t.Errorf("source was modified (content %q, err %v)", srcRaw, err)
			}
		})
	}
}

// TestDefaultSubsFlowNoExtrasStubExec: a source without subs/fonts/chapters
// runs the whole flow as a clean no-op — every step skips gracefully and
// the encoded output is byte-unchanged.
func TestDefaultSubsFlowNoExtrasStubExec(t *testing.T) {
	for _, ps := range []bool{false, true} {
		ps := ps
		name := "bash"
		if ps {
			name = "ps"
		}
		t.Run(name, func(t *testing.T) {
			tc := subsCase{step: "mux_extras", identify: subsEmptyIdentify()}
			exeSuffix := ""
			d := Dialect(BashDialect{})
			if ps {
				exeSuffix = ".exe"
				d = PowerShellDialect{}
			}
			v, epDir, path := subsSetup(t, tc, exeSuffix)
			j := &model.Job{ID: 82, Series: subsSeries, Episode: "01",
				EpisodeDir: subsSeries + "/Ep 01", ScriptType: "vpy"}
			script, err := RenderDialect(DefaultSubsFlow(), j, v, BuiltinResolver(), d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			scriptPath := filepath.Join(epDir, "job"+d.Ext())
			if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			var cmd *exec.Cmd
			if ps {
				pwsh, err := exec.LookPath("pwsh")
				if err != nil {
					t.Skip("pwsh not installed")
				}
				libPath, err := filepath.Abs("../../../powershell/EncodeLib.ps1")
				if err != nil {
					t.Fatal(err)
				}
				cmd = exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
				cmd.Env = append(os.Environ(), "PATH="+path, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
			} else {
				bash, err := exec.LookPath("bash")
				if err != nil {
					t.Skip("bash not installed")
				}
				cmd = exec.Command(bash, scriptPath)
				cmd.Env = append(os.Environ(), "PATH="+path)
			}
			out, runErr := cmd.CombinedOutput()
			text := string(out)
			t.Logf("no-extras flow output:\n%s", text)
			if runErr != nil {
				t.Fatalf("no-extras flow failed: %v\n%s", runErr, text)
			}
			if !strings.Contains(text, "output unchanged") || !strings.Contains(text, "ENCODE_JOB_DONE") {
				t.Errorf("expected clean no-op flow\n%s", text)
			}
			if strings.Contains(text, "ENCODE_STEP_FAILED") {
				t.Errorf("no-extras flow must not fail any step\n%s", text)
			}
			raw, err := os.ReadFile(filepath.Join(epDir, subsOutputName()))
			if err != nil || string(raw) != "fake-encoded" {
				t.Errorf("encoded output must be unchanged (content %q, err %v)", raw, err)
			}
			if _, err := os.Stat(filepath.Join(v.BinDir, "mkvmerge_args.txt")); err == nil {
				t.Error("mkvmerge must not be invoked when nothing was extracted")
			}
		})
	}
}

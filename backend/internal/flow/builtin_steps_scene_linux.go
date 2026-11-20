// Package flow: Linux (bash) port of scene_detect.
//
// SEMANTIC PARITY with builtin_steps_scene.go: same markers (0/30/60/100),
// same skip-if-fresh rule, same ffprobe contract (fps from r_frame_rate,
// nb_frames with a decoded-frame-count fallback), same STRICT scdet
// metadata-log parse (single-dot numerics, digit caps), same min_scene_len
// merge, same scenes.json shape (atomic write, normalized numbers).
// Tools: ffmpeg/ffprobe via command -v then $BinDir; jq is NOT required —
// awk parses the probe JSON fields and generates scenes.json (the same
// manual-JSON approach as the PS dialect, coreutils only). JSON string
// escaping uses a per-character loop (awk gsub replacement backslash
// semantics differ between mawk and gawk).
package flow

const bashSourceSceneDetect = `scene_detect () {
    echo 'ENCODE_STEP scene_detect 0'
    local ffmpeg ffprobe
    ffmpeg=$(command -v ffmpeg 2>/dev/null || true)
    [[ -z "$ffmpeg" ]] && ffmpeg="$BinDir/ffmpeg"
    ffprobe=$(command -v ffprobe 2>/dev/null || true)
    [[ -z "$ffprobe" ]] && ffprobe="$BinDir/ffprobe"
    if ! [[ -x "$ffmpeg" ]]; then
        echo "scene_detect: ffmpeg not found (install it or add it to $BinDir)"
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    if ! [[ -x "$ffprobe" ]]; then
        echo "scene_detect: ffprobe not found (install it or add it to $BinDir)"
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    local src
    src=$(find_source_file "$EpisodeDir") || {
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    }
    local out="$EpisodeDir/scenes.json"
    if [[ -f "$out" && "$out" -nt "$src" ]]; then
        echo "scenes.json already present and newer than source, reusing"
        echo 'ENCODE_STEP scene_detect 100'
        return 0
    fi

    # --- Params (renderer passes step params as env vars). Plain decimal
    # only, trimmed — same acceptance as the PS pre-validation regex. ---
    local threshold="${threshold:-10}" min_len="${min_scene_len:-0}"
    threshold=$(printf '%s' "$threshold" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
    if [[ ! "$threshold" =~ ^[0-9]{1,3}(\.[0-9]+)?$ ]]; then
        echo "scene_detect: invalid threshold '$(printf '%s' "$threshold" | tr '\r\n' '  ')'"
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    if ! awk -v t="$threshold" 'BEGIN{exit !(t>=0 && t<=100)}'; then
        echo 'scene_detect: threshold must be 0..100'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    # Normalize to the PS '0.###' format (3 decimals, trailing zeros
    # stripped): 010 -> 10, 0.000001 -> 0, 10.12345 -> 10.123 — identical
    # threshold reaches scdet and the JSON artifact in both dialects.
    threshold=$(awk -v t="$threshold" 'BEGIN{s=sprintf("%.3f",t); sub(/0+$/,"",s); sub(/\.$/,"",s); print s}')
    if [[ ! "$min_len" =~ ^[0-9]{1,6}$ ]]; then
        echo "scene_detect: invalid min_scene_len '$(printf '%s' "$min_len" | tr '\r\n' '  ')'"
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi

    # --- Probe: fps / frame count / duration (fail closed) ---
    local probe
    if ! probe=$("$ffprobe" -v error -select_streams v:0 -show_entries stream=nb_frames,r_frame_rate,duration -show_entries format=duration -of json "$src" 2>/dev/null); then
        echo "scene_detect: ffprobe failed on $(printf '%s' "$src" | tr '\r\n' '  ')"
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    local rate nb sdur fdur
    # Field extraction without jq: the ffprobe JSON is pretty-printed with
    # one "key": "value" per line; grep the wanted keys (first match wins,
    # stream fields precede format fields in the output).
    rate=$(printf '%s\n' "$probe" | grep -m1 '"r_frame_rate"' | sed 's/.*"r_frame_rate"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/') || rate=''
    if [[ ! "$rate" =~ ^[0-9]+/[0-9]+$ ]]; then
        echo "scene_detect: unusable r_frame_rate '$(printf '%s' "$rate" | tr '\r\n' '  ')'"
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    local fps
    fps=$(awk -F/ '{if ($1<=0 || $2<=0) exit 1; printf "%.6f", $1/$2}' <<< "$rate") || {
        echo 'scene_detect: r_frame_rate must be positive'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    }
    nb=$(printf '%s\n' "$probe" | grep -m1 '"nb_frames"' | sed 's/.*"nb_frames"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/') || nb=''
    sdur=$(printf '%s\n' "$probe" | grep -m1 '"duration"' | sed 's/.*"duration"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/') || sdur=''
    fdur=$(printf '%s\n' "$probe" | grep '"duration"' | tail -n1 | sed 's/.*"duration"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/') || fdur=''
    local dur=''
    if [[ "$sdur" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then dur="$sdur"
    elif [[ "$fdur" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then dur="$fdur"; fi
    if [[ -z "$dur" ]] || ! awk -v d="$dur" 'BEGIN{exit !(d>0)}'; then
        echo 'scene_detect: no usable duration from ffprobe'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    # nb_frames: digit cap BEFORE arithmetic (bash wraps mod 2^64; the PS
    # [int64] cast throws — parity is fail closed). The count itself is
    # finalized after the scdet pass (decoded-frame fallback).
    local nb_ok=0
    if [[ "$nb" =~ ^[0-9]{1,18}$ ]] && (( 10#$nb > 0 )); then nb_ok=1; fi
    echo 'ENCODE_STEP scene_detect 30'
    echo "[scene_detect] source=$(basename "$src") fps=$fps threshold=$threshold"

    # --- scdet pass (decode-only; metadata log is the parse contract) ---
    local log="$EpisodeDir/scdet.log"
    rm -f "$log" || {
        echo 'scene_detect: cannot remove stale scdet.log'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    }
    "$ffmpeg" -hide_banner -nostdin -i "$src" -an -vf "scdet=threshold=$threshold,metadata=print:file=$log" -f null - 2>&1 | sed 's/^/[ffmpeg] /'
    if (( ${PIPESTATUS[0]} != 0 )); then
        echo 'scene_detect: ffmpeg scdet pass failed'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    if [[ ! -s "$log" ]]; then
        echo 'scene_detect: scdet.log missing or empty after the ffmpeg pass'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    echo 'ENCODE_STEP scene_detect 60'

    # --- Boundaries: STRICT frame/score lines (single-dot numerics, digit
    # caps — malformed values are ignored exactly like the PS regexes).
    # Also reports the last decoded frame for the count fallback. ---
    local parsed bounds last_frame
    if ! parsed=$(awk -v t="$threshold" '
        /^frame:[0-9]{1,9}[[:space:]]+pts:[0-9]+[[:space:]]+pts_time:[0-9]+(\.[0-9]+)?[[:space:]]*$/ {
            split($1, a, ":"); frame = a[2] + 0
            split($3, b, ":"); ptime = b[2] + 0
            if (frame > last) last = frame
            have = 1
        }
        /^lavfi\.scd\.score=[0-9]+(\.[0-9]+)?[[:space:]]*$/ {
            if (!have) exit 1
            split($0, c, "="); score = c[2] + 0
            if (frame > 0 && score >= t) printf "%d %.6f %.3f\n", frame, ptime, score
        }
        END { if (!have) exit 1; printf "LAST %d\n", last }
    ' "$log"); then
        echo 'scene_detect: scdet.log has no valid frame lines (unexpected ffmpeg output)'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    last_frame=$(printf '%s\n' "$parsed" | awk '/^LAST /{print $2}')
    bounds=$(printf '%s\n' "$parsed" | grep -v '^LAST ')
    if [[ ! "$last_frame" =~ ^[0-9]+$ ]]; then
        echo 'scene_detect: scdet.log parse produced no frame count'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    # Frame count: nb_frames when the container carries it, else the ACTUAL
    # decoded count (last frame + 1) — exact for VFR too, unlike dur*fps.
    local frames
    if (( nb_ok == 1 )); then frames=$((10#$nb)); else frames=$(( last_frame + 1 )); fi
    if (( frames < 1 )); then
        echo 'scene_detect: frame count must be >= 1'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi

    # --- min_scene_len merge + scenes.json via awk (atomic write) ---
    local base
    base=$(basename "$src")
    if [[ "$base" =~ [[:cntrl:]] ]]; then
        echo 'scene_detect: source name contains control characters'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    local scenes_json
    if ! scenes_json=$(printf '%s\n' "$bounds" | awk \
            -v ml="$min_len" -v src="$base" -v fps="$fps" -v frames="$frames" \
            -v dur="$dur" -v t="$threshold" '
        function jesc(s,   i, ch, out) {
            out = ""
            for (i = 1; i <= length(s); i++) {
                ch = substr(s, i, 1)
                if (ch == "\\") out = out "\\\\"
                else if (ch == "\"") out = out "\\\""
                else out = out ch
            }
            return out
        }
        BEGIN { prev = 0; n = 0 }
        NF == 3 {
            if (ml > 0 && $1 - prev < ml) next
            f[n] = $1 + 0; tm[n] = $2 + 0; sc[n] = $3 + 0; n++; prev = $1
        }
        END {
            printf "{\n"
            printf "  \"source\": \"%s\",\n", jesc(src)
            printf "  \"fps\": %s,\n", fps
            printf "  \"frame_count\": %d,\n", frames
            printf "  \"duration\": %s,\n", dur
            printf "  \"threshold\": %s,\n", t
            printf "  \"min_scene_len\": %d,\n", ml
            printf "  \"scenes\": [\n"
            for (i = 0; i <= n; i++) {
                sf = (i == 0) ? 0 : f[i-1]
                st = (i == 0) ? 0 : tm[i-1]
                ss = (i == 0) ? 0 : sc[i-1]
                ef = (i < n) ? f[i] - 1 : frames - 1
                et = (i < n) ? tm[i] : dur
                if (ef < sf) ef = sf
                printf "    {\"index\": %d, \"start_frame\": %d, \"end_frame\": %d, \"start_time\": %.6f, \"end_time\": %.6f, \"score\": %.3f}%s\n", i+1, sf, ef, st, et, ss, (i < n) ? "," : ""
            }
            printf "  ]\n}\n"
        }
    '); then
        echo 'scene_detect: scenes.json generation failed'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    printf '%s\n' "$scenes_json" > "$out.tmp" || {
        echo 'scene_detect: cannot write scenes.json'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    }
    mv -f "$out.tmp" "$out" || {
        echo 'scene_detect: cannot install scenes.json'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    }
    if [[ ! -f "$out" ]]; then
        echo 'scene_detect: scenes.json missing after rename'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    local nscenes
    nscenes=$(grep -c '"index"' "$out")
    echo "[scene_detect] wrote scenes.json: $nscenes scene(s), $frames frame(s)"
    echo 'ENCODE_STEP scene_detect 100'
}`

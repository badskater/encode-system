// Package flow: Linux (bash) port of scene_detect.
//
// SEMANTIC PARITY with builtin_steps_scene.go: same markers (0/30/60/100),
// same skip-if-fresh rule, same ffprobe contract (fps from r_frame_rate,
// nb_frames with duration*fps fallback), same scdet metadata-log parse,
// same min_scene_len merge, same scenes.json shape (atomic write).
// Tools: ffmpeg/ffprobe via command -v then $BinDir; jq is NOT required —
// awk parses the ffprobe JSON fields and generates scenes.json (the same
// manual-JSON approach as the PS dialect, coreutils only).
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

    # --- Params (renderer passes step params as env vars) ---
    local threshold="${threshold:-10}" min_len="${min_scene_len:-0}"
    if [[ ! "$threshold" =~ ^[0-9]{1,3}(\.[0-9]+)?$ ]]; then
        echo "scene_detect: invalid threshold '$(printf '%s' "$threshold" | tr '\r\n' '  ')'"
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    if ! awk -v t="$threshold" 'BEGIN{exit !(t>=0 && t<=100)}'; then
        echo 'scene_detect: threshold must be 0..100'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
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
    local frames
    if [[ "$nb" =~ ^[0-9]+$ ]] && (( 10#$nb > 0 )); then
        frames=$((10#$nb))
    else
        # m2ts and friends lack nb_frames: derive from duration*fps.
        frames=$(awk -v d="$dur" -v f="$fps" 'BEGIN{printf "%d", int(d*f+0.5)}')
    fi
    if [[ ! "$frames" =~ ^[0-9]+$ ]] || (( frames < 1 )); then
        echo 'scene_detect: frame count must be >= 1'
        echo 'ENCODE_STEP_FAILED scene_detect'; exit 1
    fi
    echo 'ENCODE_STEP scene_detect 30'
    echo "[scene_detect] source=$(basename "$src") fps=$fps frames=$frames threshold=$threshold"

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

    # --- Boundaries: frame lines + score lines (frozen ffmpeg format) ---
    local bounds
    if ! bounds=$(awk -v t="$threshold" '
        /^frame:[0-9]+[[:space:]]+pts:[0-9]+[[:space:]]+pts_time:[0-9.]+/ {
            split($1, a, ":"); frame = a[2] + 0
            split($3, b, ":"); ptime = b[2] + 0
            have = 1
        }
        /^lavfi\.scd\.score=[0-9.]+[[:space:]]*$/ {
            if (!have) exit 1
            split($0, c, "="); score = c[2] + 0
            if (frame > 0 && score >= t) printf "%d %.6f %.3f\n", frame, ptime, score
        }
        END { if (!have) exit 1 }
    ' "$log"); then
        echo 'scene_detect: scdet.log has no valid frame lines (unexpected ffmpeg output)'
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
        BEGIN { prev = 0; n = 0 }
        NF == 3 {
            if (ml > 0 && $1 - prev < ml) next
            f[n] = $1 + 0; tm[n] = $2 + 0; sc[n] = $3 + 0; n++; prev = $1
        }
        END {
            gsub(/\\/, "\\\\", src); gsub(/"/, "\\\"", src)
            printf "{\n"
            printf "  \"source\": \"%s\",\n", src
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
    echo "[scene_detect] wrote scenes.json: $nscenes scene(s)"
    echo 'ENCODE_STEP scene_detect 100'
}`

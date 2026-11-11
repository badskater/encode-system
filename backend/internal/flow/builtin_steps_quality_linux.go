// Package flow: Linux (bash) port of the quality_gate built-in step.
//
// SEMANTIC PARITY with builtin_steps_quality.go (PS): same ENCODE_STEP
// markers (0/20/80/100), same param validation, same libvmaf probe + loud
// SSIM fallback, same sampled filtergraph, same quality.json artifact and
// ENCODE_METRIC lines, same distinct tool-error vs quality-verdict failures.
// Only the harness mechanics differ (jq/bc/grep instead of ConvertFrom-Json
// and [double] math; subshell cd instead of Push-Location).
package flow

// bashSourceQualityGate reads job context from the BashDialect.JobContext
// variables ($EpisodeDir, $OutputName, $BinDir) and step params from the
// StepCall locals ($metric, $threshold, $sampling). The shared
// find_source_file helper comes from the Preamble (same as verify_output).
const bashSourceQualityGate = `quality_gate () {
    echo 'ENCODE_STEP quality_gate 0'
    # Fail fast on the float-math / JSON helpers (same convention as
    # verify_output's bc guard): a silent fallback would fake a score.
    command -v bc >/dev/null 2>&1 || {
        echo 'ENCODE_STEP_FAILED quality_gate: bc not installed (required for score math; install the bc package)'
        exit 1
    }
    command -v jq >/dev/null 2>&1 || {
        echo 'ENCODE_STEP_FAILED quality_gate: jq not installed (required to parse the VMAF log; install the jq package)'
        exit 1
    }
    local ffmpeg
    ffmpeg=$(command -v ffmpeg 2>/dev/null || true)
    [[ -z "$ffmpeg" ]] && ffmpeg="$BinDir/ffmpeg"
    if ! [[ -x "$ffmpeg" ]]; then
        echo "quality_gate: ffmpeg not found (install it or copy it to $BinDir)"
        echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
    fi
    local mkv="$EpisodeDir/$OutputName"
    if ! [[ -e "$mkv" ]]; then
        echo "quality_gate: muxed MKV not found: $mkv (run AFTER the mux step)"
        echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
    fi
    local src
    src=$(find_source_file "$EpisodeDir" 2>/dev/null || true)
    # find_source_file's fallback picks ANY media file in the dir — with no
    # source present that can be the output MKV itself. Comparing the encode
    # against itself would fake a perfect score, so treat it as no source.
    if [[ -z "$src" || "$src" -ef "$mkv" ]]; then
        echo "quality_gate: no source media file found in $EpisodeDir (quality_gate compares the encode against the source)"
        echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
    fi

    # --- params + validation (mirrors the PS template) ---
    local m="$metric"
    [[ -z "$m" ]] && m='vmaf'
    m=$(printf '%s' "$m" | tr '[:upper:]' '[:lower:]')
    if [[ "$m" != 'vmaf' && "$m" != 'ssim' ]]; then
        echo "quality_gate: invalid metric '$m' (expected vmaf or ssim)"
        echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
    fi
    local sampling_n="$sampling"
    [[ -z "$sampling_n" ]] && sampling_n=12
    # Length cap keeps bash arithmetic from overflowing on absurd inputs
    # (the PS integer cast would throw); 1000 is the max, so >4 digits fails.
    if [[ ! "$sampling_n" =~ ^[0-9]+$ ]] || (( ${#sampling_n} > 4 )) || (( 10#$sampling_n < 1 || 10#$sampling_n > 1000 )); then
        echo "quality_gate: sampling must be an integer 1-1000 (got '$sampling_n')"
        echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
    fi
    local thr="$threshold" thr_explicit=false
    if [[ -z "$thr" ]]; then
        if [[ "$m" == 'vmaf' ]]; then thr='90'; else thr='0.95'; fi
    else
        thr_explicit=true
        if [[ ! "$thr" =~ ^[0-9]+([.][0-9]+)?$ ]] || (( ${#thr} > 12 )); then
            echo "quality_gate: threshold must be numeric (got '$thr')"
            echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
        fi
    fi
    local max='100'
    [[ "$m" == 'ssim' ]] && max='1'
    if (( $(echo "$thr < 0 || $thr > $max" | bc -l) )); then
        echo "quality_gate: threshold $thr out of range for $m (0-$max)"
        echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
    fi

    # --- libvmaf probe + loud SSIM fallback ---
    local fallback_used=false effective="$m"
    if [[ "$m" == 'vmaf' ]]; then
        local filters
        filters=$("$ffmpeg" -hide_banner -filters 2>/dev/null) || {
            echo 'quality_gate: ffmpeg -filters failed (cannot probe libvmaf support)'
            echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
        }
        if ! printf '%s' "$filters" | grep -q 'libvmaf'; then
            echo '[quality] WARNING: ffmpeg has no libvmaf support - falling back to SSIM. SSIM scores are NOT comparable to VMAF thresholds.'
            fallback_used=true
            effective='ssim'
            if [[ "$thr_explicit" == 'false' ]]; then
                thr='0.95'
            elif (( $(echo "$thr > 1" | bc -l) )); then
                echo "quality_gate: threshold $thr is VMAF-scale but libvmaf is unavailable; set an SSIM threshold (0-1) explicitly"
                echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
            fi
        fi
    fi
    echo "[quality] metric: $effective (requested: $m), threshold: $thr, sampling: every $sampling_n frame(s), fallback: $fallback_used"
    echo 'ENCODE_STEP quality_gate 20'

    # --- sampled comparison (same filtergraph as the PS template) ---
    local sel="select='not(mod(n\\,$sampling_n))',setpts=N/FRAME_RATE/TB,format=yuv420p10le"
    local graph="[0:v]${sel}[dist];[1:v]${sel}[ref];[ref][dist]scale2ref[refs][dists];[dists][refs]"
    local filter
    if [[ "$effective" == 'vmaf' ]]; then
        filter="${graph}libvmaf=log_fmt=json:log_path=quality_vmaf.json"
    else
        filter="${graph}ssim=stats_file=quality_ssim.log"
    fi
    echo "[quality] ffmpeg -nostdin -hide_banner -nostats -i $mkv -i $src -lavfi $filter -an -sn -dn -f null -"
    # Run from the episode dir so filtergraph artifact paths stay relative
    # (no escaping of special characters inside filter options).
    local rc=0
    (
        cd "$EpisodeDir" || exit 1
        "$ffmpeg" -nostdin -hide_banner -nostats -i "$mkv" -i "$src" \
            -lavfi "$filter" -an -sn -dn -f null - >quality_ffmpeg.log 2>&1
    ) || rc=$?
    if [[ -f "$EpisodeDir/quality_ffmpeg.log" ]]; then
        sed 's/^/[ffmpeg] /' "$EpisodeDir/quality_ffmpeg.log"
    fi
    if (( rc != 0 )); then
        echo "quality_gate: ffmpeg comparison failed with exit code $rc (see [ffmpeg] lines above; this is a tool failure, not a quality verdict)"
        echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
    fi

    # --- score parsing (distinct from a below-threshold verdict) ---
    local score='' sampled_frames=0
    if [[ "$effective" == 'vmaf' ]]; then
        local vlog="$EpisodeDir/quality_vmaf.json"
        if ! [[ -f "$vlog" ]]; then
            echo "quality_gate: ffmpeg exited 0 but wrote no VMAF log ($vlog) - cannot parse a score"
            echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
        fi
        score=$(jq -r '.pooled_metrics.vmaf.mean // empty' "$vlog" 2>/dev/null || true)
        sampled_frames=$(jq -r '.frames | length' "$vlog" 2>/dev/null || true)
        if [[ -z "$score" || ! "$score" =~ ^-?[0-9]+([.][0-9]+)?$ ]]; then
            echo "quality_gate: could not parse VMAF score from $vlog (pooled_metrics.vmaf.mean missing)"
            echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
        fi
    else
        # The ssim filter prints its summary to stderr: "SSIM Y:... All:<score> (...)".
        score=$(grep -o 'All:-\?[0-9.]*' "$EpisodeDir/quality_ffmpeg.log" 2>/dev/null | tail -n1 | cut -d: -f2)
        if [[ -f "$EpisodeDir/quality_ssim.log" ]]; then
            sampled_frames=$(wc -l < "$EpisodeDir/quality_ssim.log" | tr -d '[:space:]')
        fi
        if [[ -z "$score" || ! "$score" =~ ^-?[0-9]+([.][0-9]+)?$ ]]; then
            echo "quality_gate: could not parse SSIM score from ffmpeg output (no 'All:' summary line)"
            echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
        fi
    fi
    [[ -z "$sampled_frames" ]] && sampled_frames=0
    if (( sampled_frames <= 0 )); then
        echo 'quality_gate: comparison produced no sampled frames (check that source and encode are playable video)'
        echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
    fi
    echo 'ENCODE_STEP quality_gate 80'

    # --- artifact + metrics + verdict ---
    local score_str
    if [[ "$effective" == 'vmaf' ]]; then
        score_str=$(LC_ALL=C printf '%.4f' "$score")
    else
        score_str=$(LC_ALL=C printf '%.6f' "$score")
    fi
    local metric_code=0
    [[ "$effective" == 'vmaf' ]] && metric_code=1
    jq -n --arg metric "$effective" --argjson score "$score_str" \
        --argjson threshold "$thr" --argjson sampled_frames "$sampled_frames" \
        --argjson fallback_used "$fallback_used" \
        '{metric: $metric, score: $score, threshold: $threshold, sampled_frames: $sampled_frames, fallback_used: $fallback_used}' \
        > "$EpisodeDir/quality.json" || {
        echo 'quality_gate: failed to write quality.json'
        echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
    }
    echo "[quality] wrote $EpisodeDir/quality.json"
    # metrics_json is float-only: quality_metric is numeric (1=VMAF, 0=SSIM);
    # the human-readable metric name lives in quality.json and the log line.
    echo "ENCODE_METRIC quality_score=$score_str"
    echo "ENCODE_METRIC quality_metric=$metric_code"
    echo "[quality] $effective score $score_str vs threshold $thr ($sampled_frames sampled frames)"
    if (( $(echo "$score < $thr" | bc -l) )); then
        echo "quality_gate: $effective score $score_str is below threshold $thr ($sampled_frames sampled frames) - encode quality rejected"
        echo 'ENCODE_STEP_FAILED quality_gate'; exit 1
    fi
    echo 'ENCODE_STEP quality_gate 100'
}`

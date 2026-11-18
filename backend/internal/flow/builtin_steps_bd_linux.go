// Package flow: Linux (bash) ports of the Blu-ray playlist built-ins.
//
// SEMANTIC PARITY with builtin_steps_bd.go: same ENCODE_STEP markers
// (0/30/60/100), same playlist resolution (param > BDMV/PLAYLIST > flat,
// single-candidate rule), same detection-listing parse + validation, same
// meta generation (tracks reference the playlist; tsMuxeR expands per
// PlayItem), same artifact contracts (src.m2ts + chapters.xml; chapter-NN
// .m2ts/.xml; range mode rebased chapters.xml), same stale-artifact
// cleanup, same loud failures. Only the harness mechanics differ.
//
// TOOL RESOLUTION: `command -v tsMuxeR` first (system install), then
// $BinDir/tsMuxeR (bin package). The bin package may ship a tsMuxeR-lib/
// directory beside the binary (libfreetype/libpng/libbrotli for minimal
// hosts — the upstream AppImage does NOT bundle them); when present it is
// prepended to LD_LIBRARY_PATH for tsMuxeR invocations ONLY (env prefix,
// never exported script-wide, so other tools keep the system libraries).
package flow

const bashSourceBdMerge = `bd_merge () {
    echo 'ENCODE_STEP bd_merge 0'
    local tsmuxer
    tsmuxer=$(command -v tsMuxeR 2>/dev/null || true)
    [[ -z "$tsmuxer" ]] && tsmuxer="$BinDir/tsMuxeR"
    if ! [[ -x "$tsmuxer" ]]; then
        echo "bd_merge: tsMuxeR not found (add it to the bin package or copy it to $BinDir)"
        echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
    fi
    local -a tsrun=("$tsmuxer")
    if [[ "$tsmuxer" == "$BinDir/tsMuxeR" && -d "$BinDir/tsMuxeR-lib" ]]; then
        tsrun=(env "LD_LIBRARY_PATH=$BinDir/tsMuxeR-lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" "$tsmuxer")
    fi

    # --- Resolve the playlist: param > BDMV/PLAYLIST > flat *.mpls ---
    local mpls_rel=''
    if [[ -n "${playlist:-}" ]]; then
        local p
        p=$(printf '%s' "$playlist" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        p="${p//\\//}"
        # Untrusted param: no traversal, absolute paths/drive letters,
        # control chars or double quotes (the meta file quotes paths).
        if [[ "$p" =~ (^|/)\.\.(/|$) || "$p" == /* || "$p" =~ ^[A-Za-z]: || "$p" =~ [[:cntrl:]] || "$p" == *'"'* ]]; then
            echo "bd_merge: unsafe playlist parameter: $(printf '%s' "$p" | tr '\r\n' '  ')"
            echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
        fi
        if [[ ! -f "$EpisodeDir/$p" ]]; then
            echo "bd_merge: playlist not found: $(printf '%s' "$EpisodeDir/$p" | tr '\r\n' '  ')"
            echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
        fi
        mpls_rel="$p"
    else
        local search_dir="$EpisodeDir" prefix=''
        if [[ -d "$EpisodeDir/BDMV/PLAYLIST" ]]; then
            search_dir="$EpisodeDir/BDMV/PLAYLIST"
            prefix='BDMV/PLAYLIST/'
        fi
        local -a pls=()
        local f
        # Null-delimited discovery: a filename containing a newline must
        # not split into two candidates (or forge log lines).
        while IFS= read -r -d '' f; do
            [[ -n "$f" ]] && pls+=("$f")
        done < <(find "$search_dir" -maxdepth 1 -type f -iname '*.mpls' -printf '%f\0' 2>/dev/null | sort -z)
        if (( ${#pls[@]} == 0 )); then
            echo "bd_merge: no Blu-ray playlist (*.mpls) found in $EpisodeDir (expected BDMV/PLAYLIST or a loose .mpls beside the .m2ts)"
            echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
        fi
        if (( ${#pls[@]} > 1 )); then
            echo "bd_merge: multiple playlists in $EpisodeDir ($(printf '%s ' "${pls[@]}" | tr '\r\n' '  '))- set the playlist parameter"
            echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
        fi
        mpls_rel="$prefix${pls[0]}"
    fi
    local mpls="$EpisodeDir/$mpls_rel"
    if [[ "$mpls" == *'"'* || "$mpls" =~ [[:cntrl:]] ]]; then
        echo "bd_merge: playlist path contains quotes or newlines (cannot be meta-quoted)"
        echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
    fi
    echo "[bd_merge] playlist: $(printf '%s' "$mpls_rel" | tr '\r\n' '  ')"

    # --- Detection listing: tracks + chapter marks ---
    local det
    if ! det=$("${tsrun[@]}" "$mpls" 2>&1); then
        echo "bd_merge: tsMuxeR playlist detection failed: $(printf '%s' "$det" | tr '\r\n' '  ' | head -c 400)"
        echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
    fi
    local -a ids=() codecs=() marks=()
    local pending_id='' line sid m
    while IFS= read -r line; do
        line="${line%$'\r'}"
        case "$line" in
            'Track ID:'*)
                pending_id="${line#Track ID:}"
                pending_id="${pending_id//[[:space:]]/}"
                if [[ ! "$pending_id" =~ ^[0-9]+$ ]]; then
                    echo "bd_merge: tsMuxeR listing: invalid track id"; echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
                fi
                ;;
            'Stream ID:'*)
                sid="${line#Stream ID:}"
                sid="${sid//[[:space:]]/}"
                if [[ -z "$pending_id" ]]; then
                    echo "bd_merge: tsMuxeR listing: Stream ID line without a preceding Track ID"
                    echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
                fi
                if [[ ! "$sid" =~ ^[VAS]_[A-Za-z0-9/]+$ ]]; then
                    echo "bd_merge: tsMuxeR listing: unsupported stream id '$sid'"
                    echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
                fi
                ids+=("$pending_id"); codecs+=("$sid"); pending_id=''
                ;;
            Marks:*)
                local marks_str="${line#Marks:}"
                # read -ra splits on IFS WITHOUT glob expansion (a bare
                # "for m in $marks_str" would pathname-expand hostile
                # tokens before validation). Each token is validated.
                local -a mark_toks=()
                read -ra mark_toks <<< "$marks_str"
                for m in "${mark_toks[@]}"; do
                    if [[ ! "$m" =~ ^[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}$ ]]; then
                        echo "bd_merge: tsMuxeR listing: invalid chapter mark '$m'"
                        echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
                    fi
                    marks+=("$m")
                done
                ;;
        esac
    done <<< "$det"
    if (( ${#ids[@]} == 0 )); then
        echo "bd_merge: tsMuxeR found no muxable tracks in $mpls_rel"
        echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
    fi
    echo "[bd_merge] ${#ids[@]} track(s), ${#marks[@]} chapter mark(s)"
    echo 'ENCODE_STEP bd_merge 30'

    # --- Meta file: every track references the PLAYLIST (tsMuxeR expands
    # it per PlayItem, trimmed to IN/OUT). Plain printf > file: UTF-8, no BOM.
    local meta="$EpisodeDir/bd-merge.meta" out="$EpisodeDir/src.m2ts"
    {
        echo 'MUXOPT'
        local i
        for i in "${!ids[@]}"; do
            printf '%s, "%s", track=%s\n' "${codecs[$i]}" "$mpls" "${ids[$i]}"
        done
    } > "$meta" || {
        echo 'bd_merge: failed to write the meta file'
        echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
    }
    echo 'ENCODE_STEP bd_merge 60'
    # Clean stale split artifacts from a previous bd_split run in this dir
    # (mode-switch orphan guard — parity with bd_split's own cleanup).
    find "$EpisodeDir" -maxdepth 1 -type f \( -name 'chapter-[0-9]*.m2ts' -o -name 'chapter-[0-9]*.xml' -o -name 'bd-split.split.*.m2ts' \) -delete || {
        echo 'bd_merge: failed to clean stale split artifacts'
        echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
    }
    rm -f "$out"
    "${tsrun[@]}" "$meta" "$out" 2>&1 | sed 's/^/[tsMuxeR] /' || {
        echo 'bd_merge: tsMuxeR mux failed'
        echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
    }
    if [[ ! -f "$out" ]]; then
        echo "bd_merge: tsMuxeR finished but $out was not created"
        echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
    fi
    local size
    size=$(stat -c %s "$out")
    if (( size < 192 )); then
        echo 'bd_merge: src.m2ts is smaller than one 192-byte source packet - mux produced no data'
        echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
    fi

    # --- Chapters sidecar (Matroska XML, mkvmerge --chapters format) ---
    local chxml="$EpisodeDir/chapters.xml"
    if (( ${#marks[@]} == 0 )); then
        echo '[bd_merge] playlist has no chapter marks - no chapters.xml'
        if [[ -f "$chxml" ]]; then
            rm -f "$chxml"
            echo '[bd_merge] removed stale chapters.xml'
        fi
    else
        {
            echo '<?xml version="1.0" encoding="UTF-8"?>'
            echo '<Chapters>'
            echo '  <EditionEntry>'
            local i
            for i in "${!marks[@]}"; do
                echo '    <ChapterAtom>'
                # hh:mm:ss.zzz + six zeros = nanosecond-precision start.
                printf '      <ChapterTimeStart>%s000000</ChapterTimeStart>\n' "${marks[$i]}"
                echo '      <ChapterDisplay>'
                printf '        <ChapterString>Chapter %02d</ChapterString>\n' "$((i + 1))"
                echo '        <ChapterLanguage>eng</ChapterLanguage>'
                echo '      </ChapterDisplay>'
                echo '    </ChapterAtom>'
            done
            echo '  </EditionEntry>'
            echo '</Chapters>'
        } > "$chxml" || {
            echo 'bd_merge: failed to write chapters.xml'
            echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
        }
        echo "[bd_merge] wrote chapters.xml (${#marks[@]} chapters)"
    fi
    echo 'ENCODE_STEP bd_merge 100'
}`

const bashSourceBdSplit = `bd_split () {
    echo 'ENCODE_STEP bd_split 0'
    local tsmuxer
    tsmuxer=$(command -v tsMuxeR 2>/dev/null || true)
    [[ -z "$tsmuxer" ]] && tsmuxer="$BinDir/tsMuxeR"
    if ! [[ -x "$tsmuxer" ]]; then
        echo "bd_split: tsMuxeR not found (add it to the bin package or copy it to $BinDir)"
        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
    fi
    local -a tsrun=("$tsmuxer")
    if [[ "$tsmuxer" == "$BinDir/tsMuxeR" && -d "$BinDir/tsMuxeR-lib" ]]; then
        tsrun=(env "LD_LIBRARY_PATH=$BinDir/tsMuxeR-lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" "$tsmuxer")
    fi
    local mode="${mode:-chapters}"
    mode=$(printf '%s' "$mode" | tr '[:upper:]' '[:lower:]' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
    if [[ "$mode" != 'chapters' && "$mode" != 'range' ]]; then
        echo "bd_split: mode must be 'chapters' or 'range' (got '$(printf '%s' "$mode" | tr '\r\n' '  ')')"
        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
    fi

    # --- Resolve the playlist: param > BDMV/PLAYLIST > flat *.mpls ---
    local mpls_rel=''
    if [[ -n "${playlist:-}" ]]; then
        local p
        p=$(printf '%s' "$playlist" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        p="${p//\\//}"
        if [[ "$p" =~ (^|/)\.\.(/|$) || "$p" == /* || "$p" =~ ^[A-Za-z]: || "$p" =~ [[:cntrl:]] || "$p" == *'"'* ]]; then
            echo "bd_split: unsafe playlist parameter: $(printf '%s' "$p" | tr '\r\n' '  ')"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        if [[ ! -f "$EpisodeDir/$p" ]]; then
            echo "bd_split: playlist not found: $(printf '%s' "$EpisodeDir/$p" | tr '\r\n' '  ')"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        mpls_rel="$p"
    else
        local search_dir="$EpisodeDir" prefix=''
        if [[ -d "$EpisodeDir/BDMV/PLAYLIST" ]]; then
            search_dir="$EpisodeDir/BDMV/PLAYLIST"
            prefix='BDMV/PLAYLIST/'
        fi
        local -a pls=()
        local f
        # Null-delimited discovery: a filename containing a newline must
        # not split into two candidates (or forge log lines).
        while IFS= read -r -d '' f; do
            [[ -n "$f" ]] && pls+=("$f")
        done < <(find "$search_dir" -maxdepth 1 -type f -iname '*.mpls' -printf '%f\0' 2>/dev/null | sort -z)
        if (( ${#pls[@]} == 0 )); then
            echo "bd_split: no Blu-ray playlist (*.mpls) found in $EpisodeDir (expected BDMV/PLAYLIST or a loose .mpls beside the .m2ts)"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        if (( ${#pls[@]} > 1 )); then
            echo "bd_split: multiple playlists in $EpisodeDir ($(printf '%s ' "${pls[@]}" | tr '\r\n' '  '))- set the playlist parameter"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        mpls_rel="$prefix${pls[0]}"
    fi
    local mpls="$EpisodeDir/$mpls_rel"
    if [[ "$mpls" == *'"'* || "$mpls" =~ [[:cntrl:]] ]]; then
        echo "bd_split: playlist path contains quotes or newlines (cannot be meta-quoted)"
        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
    fi
    echo "[bd_split] playlist: $(printf '%s' "$mpls_rel" | tr '\r\n' '  ') (mode=$mode)"

    # --- Detection listing: tracks + chapter marks ---
    local det
    if ! det=$("${tsrun[@]}" "$mpls" 2>&1); then
        echo "bd_split: tsMuxeR playlist detection failed: $(printf '%s' "$det" | tr '\r\n' '  ' | head -c 400)"
        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
    fi
    local -a ids=() codecs=() marks=()
    local pending_id='' line sid m
    while IFS= read -r line; do
        line="${line%$'\r'}"
        case "$line" in
            'Track ID:'*)
                pending_id="${line#Track ID:}"
                pending_id="${pending_id//[[:space:]]/}"
                if [[ ! "$pending_id" =~ ^[0-9]+$ ]]; then
                    echo "bd_split: tsMuxeR listing: invalid track id"; echo 'ENCODE_STEP_FAILED bd_split'; exit 1
                fi
                ;;
            'Stream ID:'*)
                sid="${line#Stream ID:}"
                sid="${sid//[[:space:]]/}"
                if [[ -z "$pending_id" ]]; then
                    echo "bd_split: tsMuxeR listing: Stream ID line without a preceding Track ID"
                    echo 'ENCODE_STEP_FAILED bd_split'; exit 1
                fi
                if [[ ! "$sid" =~ ^[VAS]_[A-Za-z0-9/]+$ ]]; then
                    echo "bd_split: tsMuxeR listing: unsupported stream id '$sid'"
                    echo 'ENCODE_STEP_FAILED bd_split'; exit 1
                fi
                ids+=("$pending_id"); codecs+=("$sid"); pending_id=''
                ;;
            Marks:*)
                local marks_str="${line#Marks:}"
                # read -ra splits on IFS WITHOUT glob expansion (a bare
                # "for m in $marks_str" would pathname-expand hostile
                # tokens before validation). Each token is validated.
                local -a mark_toks=()
                read -ra mark_toks <<< "$marks_str"
                for m in "${mark_toks[@]}"; do
                    if [[ ! "$m" =~ ^[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}$ ]]; then
                        echo "bd_split: tsMuxeR listing: invalid chapter mark '$m'"
                        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
                    fi
                    marks+=("$m")
                done
                ;;
        esac
    done <<< "$det"
    if (( ${#ids[@]} == 0 )); then
        echo "bd_split: tsMuxeR found no muxable tracks in $mpls_rel"
        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
    fi
    if (( ${#marks[@]} == 0 )); then
        echo "bd_split: playlist $mpls_rel has no chapter marks - nothing to split"
        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
    fi

    # --- Range validation (1-based chapter numbers, inclusive) ---
    # Digit cap BEFORE arithmetic: bash wraps mod 2^64, so an uncapped
    # [0-9]+ would validate 2^64+2 as chapter 2 (the PS dialect's integer
    # cast throws on overflow — the cap keeps both dialects identical).
    # 9 digits is far above any real chapter count.
    local range_a=1 range_b=${#marks[@]}
    if [[ "$mode" == 'range' ]]; then
        local r="${range:-}"
        r=$(printf '%s' "$r" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        if [[ ! "$r" =~ ^([0-9]{1,9})(-([0-9]{1,9}))?$ ]]; then
            echo "bd_split: range must be N or N-M chapter numbers (got '$(printf '%s' "$r" | tr '\r\n' '  ')')"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        range_a=$((10#${BASH_REMATCH[1]}))
        range_b=$range_a
        if [[ -n "${BASH_REMATCH[3]:-}" ]]; then range_b=$((10#${BASH_REMATCH[3]})); fi
        if (( range_a < 1 || range_b < range_a || range_b > ${#marks[@]} )); then
            echo "bd_split: range $range_a-$range_b is outside the playlist's 1-${#marks[@]} chapters"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        # Range mode rebases chapters against the MEASURED output start —
        # ffprobe is required (fail fast, before the remux).
        local ffprobe
        ffprobe=$(command -v ffprobe 2>/dev/null || true)
        [[ -z "$ffprobe" ]] && ffprobe="$BinDir/ffprobe"
        if ! [[ -x "$ffprobe" ]]; then
            echo "bd_split: range mode requires ffprobe for exact chapter rebasing (install the ffmpeg package or add ffprobe to $BinDir)"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
    fi
    echo "[bd_split] ${#ids[@]} track(s), ${#marks[@]} chapter mark(s)"
    echo 'ENCODE_STEP bd_split 30'

    # --- Clean stale artifacts (ALL modes: a mode switch must not leave
    # orphans from a previous run; patterns cover >99-chapter names) ---
    find "$EpisodeDir" -maxdepth 1 -type f \( -name 'chapter-[0-9]*.m2ts' -o -name 'chapter-[0-9]*.xml' -o -name 'bd-split.split.*.m2ts' -o -name 'chapters.xml' \) -delete || {
        echo 'bd_split: failed to clean stale split artifacts'
        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
    }

    # --- Meta file ---
    local meta="$EpisodeDir/bd-split.meta" out=''
    local -a meta_lines=()
    if [[ "$mode" == 'chapters' ]]; then
        meta_lines+=('MUXOPT --split-chapters')
        out="$EpisodeDir/bd-split.m2ts"
    else
        # Keep chapters range_a..range_b. --cut-pictures inside starts at
        # the first key picture AT OR AFTER the cut — no previous-chapter
        # pre-roll (compliant BDs place entry marks on key pictures, so
        # the cut is exact there). --start-time pins the mux base to 600s
        # so the ffprobe rebase below recovers the source-relative content
        # start from the first video PTS.
        local muxopt="MUXOPT --cut-start=${marks[$((range_a - 1))]} --cut-pictures=inside --start-time=27000000"
        if (( range_b < ${#marks[@]} )); then muxopt+=" --cut-end=${marks[$range_b]}"; fi
        meta_lines+=("$muxopt")
        out="$EpisodeDir/src.m2ts"
        rm -f "$out"
    fi
    local i
    for i in "${!ids[@]}"; do
        meta_lines+=("$(printf '%s, "%s", track=%s' "${codecs[$i]}" "$mpls" "${ids[$i]}")")
    done
    printf '%s\n' "${meta_lines[@]}" > "$meta" || {
        echo 'bd_split: failed to write the meta file'
        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
    }
    echo 'ENCODE_STEP bd_split 60'
    "${tsrun[@]}" "$meta" "$out" 2>&1 | sed 's/^/[tsMuxeR] /' || {
        echo 'bd_split: tsMuxeR mux failed'
        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
    }

    if [[ "$mode" == 'range' ]]; then
        if [[ ! -f "$out" ]]; then
            echo "bd_split: tsMuxeR finished but $out was not created"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        local size
        size=$(stat -c %s "$out")
        if (( size < 192 )); then
            echo 'bd_split: src.m2ts is smaller than one 192-byte source packet - mux produced no data'
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        # Exact rebase against the MEASURED output start: tsMuxeR keeps
        # source-relative PTS offset by the pinned 600s mux base, so the
        # first video PTS minus 600 is the source time of the first kept
        # picture (the key picture at/after the cut mark). Nominal mark
        # math would silently shift every chapter by the keyframe snap.
        local first_pts
        if ! first_pts=$("$ffprobe" -v error -select_streams v:0 -show_entries packet=pts_time -of default=noprint_wrappers=1:nokey=1 -read_intervals '%+#1' "$out"); then
            echo 'bd_split: ffprobe failed on the cut output'
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        first_pts=$(printf '%s' "$first_pts" | head -n1 | tr -d '[:space:]')
        if [[ ! "$first_pts" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
            echo "bd_split: ffprobe returned no usable first video PTS ('$(printf '%s' "$first_pts" | tr '\r\n' '  ')')"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        local start_ms
        start_ms=$(awk -v p="$first_pts" 'BEGIN{printf "%d", (p - 600) * 1000 + 0.5}')
        if (( start_ms < 0 )); then
            echo "bd_split: cut output starts before the pinned mux base (first PTS $first_pts) - unexpected tsMuxeR timing"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        {
            echo '<?xml version="1.0" encoding="UTF-8"?>'
            echo '<Chapters>'
            echo '  <EditionEntry>'
            local ms rel
            for ((i = range_a - 1; i < range_b; i++)); do
                IFS=':.' read -r h m s z <<< "${marks[$i]}"
                ms=$(( (10#$h * 3600 + 10#$m * 60 + 10#$s) * 1000 + 10#$z ))
                rel=$((ms - start_ms))
                # The first kept mark can predate the key picture the cut
                # actually starts at (off-keyframe mark): clamp to 0.
                if (( rel < 0 )); then rel=0; fi
                echo '    <ChapterAtom>'
                printf '      <ChapterTimeStart>%02d:%02d:%02d.%09d</ChapterTimeStart>\n' \
                    $((rel / 3600000)) $((rel % 3600000 / 60000)) $((rel % 60000 / 1000)) $((rel % 1000 * 1000000))
                echo '      <ChapterDisplay>'
                printf '        <ChapterString>Chapter %02d</ChapterString>\n' "$((i + 1))"
                echo '        <ChapterLanguage>eng</ChapterLanguage>'
                echo '      </ChapterDisplay>'
                echo '    </ChapterAtom>'
            done
            echo '  </EditionEntry>'
            echo '</Chapters>'
        } > "$EpisodeDir/chapters.xml" || {
            echo 'bd_split: failed to write chapters.xml'
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        }
        echo "[bd_split] kept chapters $range_a-$range_b -> src.m2ts + chapters.xml"
        echo 'ENCODE_STEP bd_split 100'
        return 0
    fi

    # --- chapters mode: rename split parts to chapter-NN.m2ts + sidecars ---
    # A playlist whose FIRST mark is non-zero gets an extra pre-chapter
    # segment (0 -> first mark) from --split-chapters: N marks -> N+1
    # parts (verified against real tsMuxeR 3.1.0). It maps to chapter-00.
    local pre_part=0
    if [[ "${marks[0]}" != '00:00:00.000' ]]; then pre_part=1; fi
    local expected_parts=$(( ${#marks[@]} + pre_part ))
    local part_count
    part_count=$(find "$EpisodeDir" -maxdepth 1 -type f -name 'bd-split.split.*.m2ts' | wc -l)
    if (( part_count != expected_parts )); then
        echo "bd_split: tsMuxeR produced $part_count split parts but expected $expected_parts (${#marks[@]} chapters, pre-chapter segment: $pre_part)"
        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
    fi
    local p cn nn part size
    for ((p = 1; p <= expected_parts; p++)); do
        cn=$((p - pre_part))
        part="$EpisodeDir/bd-split.split.$p.m2ts"
        if [[ ! -f "$part" ]]; then
            echo "bd_split: tsMuxeR split part missing: $part"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        size=$(stat -c %s "$part")
        if (( size < 192 )); then
            echo "bd_split: split part $part is smaller than one 192-byte source packet"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        nn=$(printf '%02d' "$cn")
        mv -f "$part" "$EpisodeDir/chapter-$nn.m2ts" || {
            echo "bd_split: failed to rename split part $part"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        }
        # Post-condition: a stale DIRECTORY at the target name would make
        # mv move the part INSIDE it and still exit 0 — require a regular
        # file at the artifact path (the PS dialect's rename cmdlet throws
        # on an existing target, so this keeps fail-closed parity).
        if [[ ! -f "$EpisodeDir/chapter-$nn.m2ts" ]]; then
            echo "bd_split: chapter-$nn.m2ts is not a regular file after rename (stale directory at target?)"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        # Each part is exactly one chapter: a single mark at 0, original
        # chapter number kept in the display name (chapter-00 = the
        # pre-chapter segment when the first mark is non-zero).
        {
            echo '<?xml version="1.0" encoding="UTF-8"?>'
            echo '<Chapters>'
            echo '  <EditionEntry>'
            echo '    <ChapterAtom>'
            echo '      <ChapterTimeStart>00:00:00.000000000</ChapterTimeStart>'
            echo '      <ChapterDisplay>'
            printf '        <ChapterString>Chapter %s</ChapterString>\n' "$nn"
            echo '        <ChapterLanguage>eng</ChapterLanguage>'
            echo '      </ChapterDisplay>'
            echo '    </ChapterAtom>'
            echo '  </EditionEntry>'
            echo '</Chapters>'
        } > "$EpisodeDir/chapter-$nn.xml" || {
            echo "bd_split: failed to write chapter-$nn.xml"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        }
    done
    echo "[bd_split] wrote $expected_parts chapter files (chapter-$(printf '%02d' "$((1 - pre_part))").m2ts .. chapter-$(printf '%02d' "${#marks[@]}").m2ts)"
    echo 'ENCODE_STEP bd_split 100'
}`

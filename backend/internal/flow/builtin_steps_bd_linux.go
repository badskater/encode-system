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
        while IFS= read -r f; do
            [[ -n "$f" ]] && pls+=("$f")
        done < <(find "$search_dir" -maxdepth 1 -type f -iname '*.mpls' -printf '%f\n' 2>/dev/null | sort)
        if (( ${#pls[@]} == 0 )); then
            echo "bd_merge: no Blu-ray playlist (*.mpls) found in $EpisodeDir (expected BDMV/PLAYLIST or a loose .mpls beside the .m2ts)"
            echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
        fi
        if (( ${#pls[@]} > 1 )); then
            echo "bd_merge: multiple playlists in $EpisodeDir (${pls[*]}) - set the playlist parameter"
            echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
        fi
        mpls_rel="$prefix${pls[0]}"
    fi
    local mpls="$EpisodeDir/$mpls_rel"
    if [[ "$mpls" == *'"'* || "$mpls" =~ [[:cntrl:]] ]]; then
        echo "bd_merge: playlist path contains quotes or newlines (cannot be meta-quoted)"
        echo 'ENCODE_STEP_FAILED bd_merge'; exit 1
    fi
    echo "[bd_merge] playlist: $mpls_rel"

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
                # Intentional word splitting: marks are whitespace-separated
                # and each token is validated below before use.
                for m in $marks_str; do
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
    } > "$meta"
    echo 'ENCODE_STEP bd_merge 60'
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
        } > "$chxml"
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
        echo "bd_split: mode must be 'chapters' or 'range' (got '$mode')"
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
        while IFS= read -r f; do
            [[ -n "$f" ]] && pls+=("$f")
        done < <(find "$search_dir" -maxdepth 1 -type f -iname '*.mpls' -printf '%f\n' 2>/dev/null | sort)
        if (( ${#pls[@]} == 0 )); then
            echo "bd_split: no Blu-ray playlist (*.mpls) found in $EpisodeDir (expected BDMV/PLAYLIST or a loose .mpls beside the .m2ts)"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        if (( ${#pls[@]} > 1 )); then
            echo "bd_split: multiple playlists in $EpisodeDir (${pls[*]}) - set the playlist parameter"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        mpls_rel="$prefix${pls[0]}"
    fi
    local mpls="$EpisodeDir/$mpls_rel"
    if [[ "$mpls" == *'"'* || "$mpls" =~ [[:cntrl:]] ]]; then
        echo "bd_split: playlist path contains quotes or newlines (cannot be meta-quoted)"
        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
    fi
    echo "[bd_split] playlist: $mpls_rel (mode=$mode)"

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
                for m in $marks_str; do
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
    local range_a=1 range_b=${#marks[@]}
    if [[ "$mode" == 'range' ]]; then
        local r="${range:-}"
        r=$(printf '%s' "$r" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        if [[ ! "$r" =~ ^([0-9]+)(-([0-9]+))?$ ]]; then
            echo "bd_split: range must be N or N-M chapter numbers (got '$r')"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        range_a=$((10#${BASH_REMATCH[1]}))
        range_b=$range_a
        if [[ -n "${BASH_REMATCH[3]:-}" ]]; then range_b=$((10#${BASH_REMATCH[3]})); fi
        if (( range_a < 1 || range_b < range_a || range_b > ${#marks[@]} )); then
            echo "bd_split: range $range_a-$range_b is outside the playlist's 1-${#marks[@]} chapters"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
    fi
    echo "[bd_split] ${#ids[@]} track(s), ${#marks[@]} chapter mark(s)"
    echo 'ENCODE_STEP bd_split 30'

    # --- Meta file ---
    local meta="$EpisodeDir/bd-split.meta" out=''
    local -a meta_lines=()
    if [[ "$mode" == 'chapters' ]]; then
        meta_lines+=('MUXOPT --split-chapters')
        out="$EpisodeDir/bd-split.m2ts"
        # Clean stale artifacts so a re-run with fewer chapters never leaves
        # orphan parts behind (mux_extras would pick up stale chapters.xml).
        find "$EpisodeDir" -maxdepth 1 -type f \( -name 'chapter-[0-9][0-9].m2ts' -o -name 'chapter-[0-9][0-9].xml' -o -name 'bd-split.split.*.m2ts' -o -name 'chapters.xml' \) -delete
    else
        # Keep chapters range_a..range_b: cut from mark[range_a-1] up to the
        # START of the next chapter after the range (or the source end).
        local muxopt="MUXOPT --cut-start=${marks[$((range_a - 1))]}"
        if (( range_b < ${#marks[@]} )); then muxopt+=" --cut-end=${marks[$range_b]}"; fi
        meta_lines+=("$muxopt")
        out="$EpisodeDir/src.m2ts"
        rm -f "$out"
    fi
    local i
    for i in "${!ids[@]}"; do
        meta_lines+=("$(printf '%s, "%s", track=%s' "${codecs[$i]}" "$mpls" "${ids[$i]}")")
    done
    printf '%s\n' "${meta_lines[@]}" > "$meta"
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
        # Rebased chapters.xml for the kept range (times relative to the
        # range start; original chapter numbers kept in the names).
        local h m s z start_ms
        IFS=':.' read -r h m s z <<< "${marks[$((range_a - 1))]}"
        start_ms=$(( (10#$h * 3600 + 10#$m * 60 + 10#$s) * 1000 + 10#$z ))
        {
            echo '<?xml version="1.0" encoding="UTF-8"?>'
            echo '<Chapters>'
            echo '  <EditionEntry>'
            local ms rel
            for ((i = range_a - 1; i < range_b; i++)); do
                IFS=':.' read -r h m s z <<< "${marks[$i]}"
                ms=$(( (10#$h * 3600 + 10#$m * 60 + 10#$s) * 1000 + 10#$z ))
                rel=$((ms - start_ms))
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
        } > "$EpisodeDir/chapters.xml"
        echo "[bd_split] kept chapters $range_a-$range_b -> src.m2ts + chapters.xml"
        echo 'ENCODE_STEP bd_split 100'
        return 0
    fi

    # --- chapters mode: rename split parts to chapter-NN.m2ts + sidecars ---
    local part_count
    part_count=$(find "$EpisodeDir" -maxdepth 1 -type f -name 'bd-split.split.*.m2ts' | wc -l)
    if (( part_count != ${#marks[@]} )); then
        echo "bd_split: tsMuxeR produced $part_count split parts but the playlist has ${#marks[@]} chapters"
        echo 'ENCODE_STEP_FAILED bd_split'; exit 1
    fi
    local n nn part size
    for ((n = 1; n <= ${#marks[@]}; n++)); do
        part="$EpisodeDir/bd-split.split.$n.m2ts"
        if [[ ! -f "$part" ]]; then
            echo "bd_split: tsMuxeR split part missing: $part"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        size=$(stat -c %s "$part")
        if (( size < 192 )); then
            echo "bd_split: split part $part is smaller than one 192-byte source packet"
            echo 'ENCODE_STEP_FAILED bd_split'; exit 1
        fi
        nn=$(printf '%02d' "$n")
        mv -f "$part" "$EpisodeDir/chapter-$nn.m2ts"
        # Each part is exactly one chapter: a single mark at 0, original
        # chapter number kept in the display name.
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
        } > "$EpisodeDir/chapter-$nn.xml"
    done
    echo "[bd_split] wrote ${#marks[@]} chapter files (chapter-01.m2ts .. chapter-$(printf '%02d' "${#marks[@]}").m2ts)"
    echo 'ENCODE_STEP bd_split 100'
}`

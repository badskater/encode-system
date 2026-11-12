// Package flow: Linux (bash) ports of the P6 extraction/mux built-ins.
//
// SEMANTIC PARITY with builtin_steps_subs.go: same ENCODE_STEP markers
// (extract steps 0/50/100, mux_extras 0/20/80/100), same artifact
// contracts (subs.json / fonts.json / chapters.xml + the extracted files),
// same track/font selection rules, same graceful skips, same output
// discovery with the file-identity guard, same mkvmerge argument set and
// atomic temp+rename replace. Only the harness mechanics differ (jq
// instead of JSON cmdlets, find/stat instead of directory cmdlets).
//
// jq is REQUIRED (fail-fast guard, same convention as quality_gate): the
// identify output and the manifests are parsed with it.
package flow

// bashSourceSubtitleExtract reads job context from the BashDialect
// variables ($EpisodeDir, $BinDir) and the shared find_source_file helper
// from the Preamble. Manifest entries are built with simple jq -n object
// construction and slurped into an array (the e2e jq stub implements
// exactly these queries).
const bashSourceSubtitleExtract = `subtitle_extract () {
    echo 'ENCODE_STEP subtitle_extract 0'
    command -v jq >/dev/null 2>&1 || {
        echo 'ENCODE_STEP_FAILED subtitle_extract: jq not installed (required to parse mkvmerge identify output; install the jq package)'
        exit 1
    }
    local mkvmerge mkvextract
    mkvmerge=$(command -v mkvmerge 2>/dev/null || true)
    [[ -z "$mkvmerge" ]] && mkvmerge="$BinDir/mkvmerge"
    if ! [[ -x "$mkvmerge" ]]; then
        echo "subtitle_extract: mkvmerge not found (install the mkvtoolnix package or copy it to $BinDir)"
        echo 'ENCODE_STEP_FAILED subtitle_extract'; exit 1
    fi
    mkvextract=$(command -v mkvextract 2>/dev/null || true)
    [[ -z "$mkvextract" ]] && mkvextract="$BinDir/mkvextract"
    if ! [[ -x "$mkvextract" ]]; then
        echo "subtitle_extract: mkvextract not found (install the mkvtoolnix package or copy it to $BinDir)"
        echo 'ENCODE_STEP_FAILED subtitle_extract'; exit 1
    fi
    local src
    src=$(find_source_file "$EpisodeDir") || {
        echo "no source media file found in $EpisodeDir"
        echo 'ENCODE_STEP_FAILED subtitle_extract'; exit 1
    }
    local ident
    ident=$("$mkvmerge" -J "$src") || {
        echo "subtitle_extract: mkvmerge -J exited with code $?"
        echo 'ENCODE_STEP_FAILED subtitle_extract'; exit 1
    }
    local n_subs
    n_subs=$(printf '%s' "$ident" | jq '[.tracks[]? | select(.type=="subtitles")] | length')
    local entries=''
    if [[ "$n_subs" == "0" ]]; then
        echo "[subtitle_extract] no subtitle tracks in $src - skipping"
    else
        echo 'ENCODE_STEP subtitle_extract 50'
        # One mkvextract call carries every track spec (source FIRST, then
        # the 'tracks' mode, then id:dest pairs - mkvextract syntax).
        local extract_args=("$src" "tracks")
        local id lang name def frc codec ext file entry
        # Unit-separator (\x1f) delimited fields: unlike tab it is NOT IFS
        # whitespace, so read preserves EMPTY fields (empty track names).
        while IFS=$'\x1f' read -r id lang name def frc codec; do
            [[ -z "$id" ]] && continue
            if [[ ! "$id" =~ ^[0-9]+$ ]] || (( ${#id} > 6 )); then
                echo "subtitle_extract: invalid subtitle track id '$id'"
                echo 'ENCODE_STEP_FAILED subtitle_extract'; exit 1
            fi
            [[ "$lang" =~ ^[a-z]{2,3}$ ]] || lang='und'
            # Extension from the Matroska codec id; unknown codecs extract
            # as raw .sub rather than guessing a text format.
            ext='sub'
            case "$codec" in
                S_TEXT/UTF8|S_TEXT/ASCII) ext='srt' ;;
                S_TEXT/ASS|S_ASS) ext='ass' ;;
                S_TEXT/SSA|S_SSA) ext='ssa' ;;
                S_TEXT/WEBVTT) ext='vtt' ;;
                S_HDMV/PGS) ext='sup' ;;
            esac
            file="subs_${id}_${lang}.${ext}"
            extract_args+=("${id}:${EpisodeDir}/${file}")
            [[ "$def" == "true" ]] && def='yes' || def='no'
            [[ "$frc" == "true" ]] && frc='yes' || frc='no'
            entry=$(jq -n --arg file "$file" --arg language "$lang" --arg track_name "$name" --arg default_track "$def" --arg forced_track "$frc" '{file:$file,language:$language,track_name:$track_name,default_track:$default_track,forced_track:$forced_track}')
            entries="${entries}${entry}"$'\n'
            echo "[subtitle_extract] track $id ($lang, $codec) -> $file"
        done < <(printf '%s' "$ident" | jq -r '.tracks[]? | select(.type=="subtitles") | [.id, (.properties.language // "und"), (.properties.track_name // ""), (.properties.default_track // false), (.properties.forced_track // false), (.properties.codec_id // "")] | map(tostring) | join("\u001f")')
        "$mkvextract" "${extract_args[@]}" 2>&1 | sed 's/^/[mkvextract] /' || {
            echo "subtitle_extract: mkvextract exited with code $?"
            echo 'ENCODE_STEP_FAILED subtitle_extract'; exit 1
        }
        local mfile
        while IFS= read -r mfile; do
            [[ -z "$mfile" ]] && continue
            if ! [[ -e "$EpisodeDir/$mfile" ]]; then
                echo "subtitle_extract: mkvextract finished but $EpisodeDir/$mfile was not created"
                echo 'ENCODE_STEP_FAILED subtitle_extract'; exit 1
            fi
        done < <(printf '%s' "$entries" | jq -s -r '.[].file')
        echo "[subtitle_extract] extracted $n_subs subtitle track(s)"
    fi
    # Always rewrite the manifest (empty when nothing was found) so a
    # re-run reflects the current source and mux_extras never reads stale
    # entries.
    printf '%s' "$entries" | jq -s '.' > "$EpisodeDir/subs.json" || {
        echo 'subtitle_extract: failed to write subs.json'
        echo 'ENCODE_STEP_FAILED subtitle_extract'; exit 1
    }
    echo 'ENCODE_STEP subtitle_extract 100'
}`

// bashSourceFontExtract mirrors the PS font selection: font MIME types
// (content_type with a legacy mime_type fallback) or font file extensions.
// Attachment names are sanitized for the filesystem before use.
const bashSourceFontExtract = `font_extract () {
    echo 'ENCODE_STEP font_extract 0'
    command -v jq >/dev/null 2>&1 || {
        echo 'ENCODE_STEP_FAILED font_extract: jq not installed (required to parse mkvmerge identify output; install the jq package)'
        exit 1
    }
    local mkvmerge mkvextract
    mkvmerge=$(command -v mkvmerge 2>/dev/null || true)
    [[ -z "$mkvmerge" ]] && mkvmerge="$BinDir/mkvmerge"
    if ! [[ -x "$mkvmerge" ]]; then
        echo "font_extract: mkvmerge not found (install the mkvtoolnix package or copy it to $BinDir)"
        echo 'ENCODE_STEP_FAILED font_extract'; exit 1
    fi
    mkvextract=$(command -v mkvextract 2>/dev/null || true)
    [[ -z "$mkvextract" ]] && mkvextract="$BinDir/mkvextract"
    if ! [[ -x "$mkvextract" ]]; then
        echo "font_extract: mkvextract not found (install the mkvtoolnix package or copy it to $BinDir)"
        echo 'ENCODE_STEP_FAILED font_extract'; exit 1
    fi
    local src
    src=$(find_source_file "$EpisodeDir") || {
        echo "no source media file found in $EpisodeDir"
        echo 'ENCODE_STEP_FAILED font_extract'; exit 1
    }
    local ident
    ident=$("$mkvmerge" -J "$src") || {
        echo "font_extract: mkvmerge -J exited with code $?"
        echo 'ENCODE_STEP_FAILED font_extract'; exit 1
    }
    local entries='' n_fonts=0
    local id fname mime lower safe ext file entry is_font
    local extract_args=("$src" "attachments")
    # Unit-separator delimited (non-whitespace IFS keeps empty fields).
    while IFS=$'\x1f' read -r id fname mime; do
        [[ -z "$id" ]] && continue
        is_font=false
        case "$mime" in
            application/x-truetype-font|application/vnd.ms-opentype|application/font-sfnt|application/x-font-ttf|application/x-font-otf|application/font-woff|font/ttf|font/otf|font/woff|font/collection|font/sfnt) is_font=true ;;
            font/*) is_font=true ;;
        esac
        if [[ "$is_font" == "false" ]]; then
            lower=$(printf '%s' "$fname" | tr '[:upper:]' '[:lower:]')
            case "$lower" in
                *.ttf|*.otf|*.woff|*.ttc) is_font=true ;;
            esac
        fi
        [[ "$is_font" == "true" ]] || continue
        if [[ ! "$id" =~ ^[0-9]+$ ]] || (( ${#id} > 6 )); then
            echo "font_extract: invalid attachment id '$id'"
            echo 'ENCODE_STEP_FAILED font_extract'; exit 1
        fi
        # Sanitize the attachment name for the filesystem; fall back to a
        # MIME-derived name when the attachment carries no usable name.
        safe=$(printf '%s' "$fname" | tr -c 'A-Za-z0-9._-' '_')
        safe="${safe:0:100}"
        if [[ -z "$safe" || "$safe" == "." || "$safe" == ".." ]]; then
            ext='.bin'
            case "$mime" in
                application/x-truetype-font|application/x-font-ttf|font/ttf) ext='.ttf' ;;
                application/vnd.ms-opentype|application/x-font-otf|font/otf) ext='.otf' ;;
                application/font-woff|font/woff) ext='.woff' ;;
                font/collection) ext='.ttc' ;;
            esac
            safe="attachment_${id}${ext}"
        fi
        file="fonts/${id}_${safe}"
        extract_args+=("${id}:${EpisodeDir}/${file}")
        entry=$(jq -n --arg file "$file" '{file:$file}')
        entries="${entries}${entry}"$'\n'
        n_fonts=$((n_fonts+1))
        echo "[font_extract] attachment $id ($mime) -> $file"
    done < <(printf '%s' "$ident" | jq -r '.attachments[]? | [.id, (.file_name // ""), (.content_type // .mime_type // "")] | map(tostring) | join("\u001f")')
    if (( n_fonts == 0 )); then
        echo "[font_extract] no font attachments in $src - skipping"
    else
        echo 'ENCODE_STEP font_extract 50'
        mkdir -p -- "$EpisodeDir/fonts"
        "$mkvextract" "${extract_args[@]}" 2>&1 | sed 's/^/[mkvextract] /' || {
            echo "font_extract: mkvextract exited with code $?"
            echo 'ENCODE_STEP_FAILED font_extract'; exit 1
        }
        local mfile
        while IFS= read -r mfile; do
            [[ -z "$mfile" ]] && continue
            if ! [[ -e "$EpisodeDir/$mfile" ]]; then
                echo "font_extract: mkvextract finished but $EpisodeDir/$mfile was not created"
                echo 'ENCODE_STEP_FAILED font_extract'; exit 1
            fi
        done < <(printf '%s' "$entries" | jq -s -r '.[].file')
        echo "[font_extract] extracted $n_fonts font attachment(s)"
    fi
    printf '%s' "$entries" | jq -s '.' > "$EpisodeDir/fonts.json" || {
        echo 'font_extract: failed to write fonts.json'
        echo 'ENCODE_STEP_FAILED font_extract'; exit 1
    }
    echo 'ENCODE_STEP font_extract 100'
}`

// bashSourceChapterExtract: mkvextract chapters writes Matroska XML (the
// default format) straight to the destination file argument.
const bashSourceChapterExtract = `chapter_extract () {
    echo 'ENCODE_STEP chapter_extract 0'
    command -v jq >/dev/null 2>&1 || {
        echo 'ENCODE_STEP_FAILED chapter_extract: jq not installed (required to parse mkvmerge identify output; install the jq package)'
        exit 1
    }
    local mkvmerge mkvextract
    mkvmerge=$(command -v mkvmerge 2>/dev/null || true)
    [[ -z "$mkvmerge" ]] && mkvmerge="$BinDir/mkvmerge"
    if ! [[ -x "$mkvmerge" ]]; then
        echo "chapter_extract: mkvmerge not found (install the mkvtoolnix package or copy it to $BinDir)"
        echo 'ENCODE_STEP_FAILED chapter_extract'; exit 1
    fi
    mkvextract=$(command -v mkvextract 2>/dev/null || true)
    [[ -z "$mkvextract" ]] && mkvextract="$BinDir/mkvextract"
    if ! [[ -x "$mkvextract" ]]; then
        echo "chapter_extract: mkvextract not found (install the mkvtoolnix package or copy it to $BinDir)"
        echo 'ENCODE_STEP_FAILED chapter_extract'; exit 1
    fi
    local src
    src=$(find_source_file "$EpisodeDir") || {
        echo "no source media file found in $EpisodeDir"
        echo 'ENCODE_STEP_FAILED chapter_extract'; exit 1
    }
    local ident
    ident=$("$mkvmerge" -J "$src") || {
        echo "chapter_extract: mkvmerge -J exited with code $?"
        echo 'ENCODE_STEP_FAILED chapter_extract'; exit 1
    }
    # Chapter editions live at the top level; sum their entry counts.
    local total
    total=$(printf '%s' "$ident" | jq '[.chapters[]?.num_entries] | add // 0')
    local dest="$EpisodeDir/chapters.xml"
    if [[ "$total" == "0" ]]; then
        echo "[chapter_extract] no chapters in $src - skipping"
        if [[ -e "$dest" ]]; then
            rm -f -- "$dest"
            echo '[chapter_extract] removed stale chapters.xml'
        fi
    else
        echo 'ENCODE_STEP chapter_extract 50'
        "$mkvextract" "$src" chapters "$dest" 2>&1 | sed 's/^/[mkvextract] /' || {
            echo "chapter_extract: mkvextract exited with code $?"
            echo 'ENCODE_STEP_FAILED chapter_extract'; exit 1
        }
        if ! [[ -s "$dest" ]]; then
            echo "chapter_extract: mkvextract finished but $dest was not created"
            echo 'ENCODE_STEP_FAILED chapter_extract'; exit 1
        fi
        echo "[chapter_extract] extracted $total chapter entries"
    fi
    echo 'ENCODE_STEP chapter_extract 100'
}`

// bashSourceMuxExtras mirrors the PS discovery (explicit param, standard
// release name, newest non-source MKV), the mkvmerge argument set and the
// atomic temp+rename replace. The -ef identity test catches symlink AND
// hardlink aliases of the source (same guard class as quality_gate).
const bashSourceMuxExtras = `mux_extras () {
    echo 'ENCODE_STEP mux_extras 0'
    command -v jq >/dev/null 2>&1 || {
        echo 'ENCODE_STEP_FAILED mux_extras: jq not installed (required to read the extraction manifests; install the jq package)'
        exit 1
    }
    local mkvmerge
    mkvmerge=$(command -v mkvmerge 2>/dev/null || true)
    [[ -z "$mkvmerge" ]] && mkvmerge="$BinDir/mkvmerge"
    if ! [[ -x "$mkvmerge" ]]; then
        echo "mux_extras: mkvmerge not found (install the mkvtoolnix package or copy it to $BinDir)"
        echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
    fi
    # --- gather extracted extras (manifests written by the extract steps) ---
    local subs_json="$EpisodeDir/subs.json" fonts_json="$EpisodeDir/fonts.json" chapters_xml="$EpisodeDir/chapters.xml"
    local n_subs=0 n_fonts=0 has_chapters=false
    if [[ -f "$subs_json" ]]; then
        n_subs=$(jq 'length' "$subs_json") || {
            echo 'mux_extras: subs.json is unreadable'
            echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
        }
    fi
    if [[ -f "$fonts_json" ]]; then
        n_fonts=$(jq 'length' "$fonts_json") || {
            echo 'mux_extras: fonts.json is unreadable'
            echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
        }
    fi
    [[ -s "$chapters_xml" ]] && has_chapters=true
    if (( n_subs == 0 && n_fonts == 0 )) && [[ "$has_chapters" == "false" ]]; then
        echo '[mux_extras] no extracted extras found (subs/fonts/chapters) - output unchanged'
        echo 'ENCODE_STEP mux_extras 100'
        return 0
    fi
    # Validate manifest entries before touching anything.
    local sfile ffile
    if (( n_subs > 0 )); then
        while IFS= read -r sfile; do
            [[ -z "$sfile" ]] && continue
            if ! [[ -e "$EpisodeDir/$sfile" ]]; then
                echo "mux_extras: subtitle file from subs.json missing: $EpisodeDir/$sfile (re-run subtitle_extract)"
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            fi
        done < <(jq -r '.[].file' "$subs_json")
    fi
    if (( n_fonts > 0 )); then
        while IFS= read -r ffile; do
            [[ -z "$ffile" ]] && continue
            if ! [[ -e "$EpisodeDir/$ffile" ]]; then
                echo "mux_extras: font file from fonts.json missing: $EpisodeDir/$ffile (re-run font_extract)"
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            fi
        done < <(jq -r '.[].file' "$fonts_json")
    fi

    # --- locate the encoded output (NEVER the source) ---
    local src=''
    src=$(find_source_file "$EpisodeDir" 2>/dev/null || true)
    local out=''
    local explicit
    explicit=$(printf '%s' "$output" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
    if [[ -n "$explicit" ]]; then
        # Same guard as the shared safe-name check: no path separators,
        # '..' or reserved characters.
        local unsafe_re='[\\/:*?"<>|]'
        if [[ "$explicit" =~ $unsafe_re || "$explicit" == *..* ]]; then
            echo "unsafe output name value: '$explicit' (path separators, '..' or reserved characters are not allowed)"
            echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
        fi
        local candidate="$EpisodeDir/$explicit"
        if ! [[ -e "$candidate" ]]; then
            echo "mux_extras: output param file not found: $candidate"
            echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
        fi
        if [[ -n "$src" && "$candidate" -ef "$src" ]]; then
            echo "mux_extras: output param points at the source media ($candidate) - refusing to modify the source"
            echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
        fi
        out="$candidate"
    else
        # Standard release naming first (what mux writes and release_copy
        # reads), then newest non-source MKV (covers crc32_rename outputs).
        local c
        while IFS= read -r c; do
            [[ -z "$c" ]] && continue
            if [[ -n "$src" && "$c" -ef "$src" ]]; then continue; fi
            out="$c"
            break
        done < <(
            if [[ -e "$EpisodeDir/$OutputName" ]]; then printf '%s\n' "$EpisodeDir/$OutputName"; fi
            find "$EpisodeDir" -maxdepth 1 -type f -name '*.mkv' ! -name '.mux_extras.tmp.mkv' -printf '%T@ %p\n' | sort -rn | cut -d' ' -f2-
        )
    fi
    if [[ -z "$out" ]]; then
        echo "mux_extras: encoded output MKV not found in $EpisodeDir (expected $OutputName; run AFTER the encode flow or set the output param)"
        echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
    fi
    echo "[mux_extras] encoded output: $out"
    echo 'ENCODE_STEP mux_extras 20'

    # --- mkvmerge: encoded first (keep video+audio, strip old extras so
    # re-runs replace instead of duplicating), then one input per sub file
    # with its flags; chapters + fonts ride the global options. ---
    local tmp="$EpisodeDir/.mux_extras.tmp.mkv"
    rm -f -- "$tmp"
    local args=(-o "$tmp" --quiet)
    if [[ "$has_chapters" == "true" ]]; then
        args+=(--chapters "$chapters_xml")
    fi
    if (( n_fonts > 0 )); then
        while IFS= read -r ffile; do
            [[ -z "$ffile" ]] && continue
            args+=(--attach-file "$EpisodeDir/$ffile")
        done < <(jq -r '.[].file' "$fonts_json")
    fi
    args+=(--no-subtitles --no-chapters --no-attachments "$out")
    if (( n_subs > 0 )); then
        local line slang sname sdef sfrc def_flag frc_flag
        while IFS= read -r line; do
            [[ -z "$line" ]] && continue
            sfile=$(printf '%s' "$line" | jq -r '.file')
            slang=$(printf '%s' "$line" | jq -r '.language // "und"')
            [[ "$slang" =~ ^[a-z]{2,3}$ ]] || slang='und'
            sname=$(printf '%s' "$line" | jq -r '.track_name // ""')
            sdef=$(printf '%s' "$line" | jq -r '.default_track // "no"')
            sfrc=$(printf '%s' "$line" | jq -r '.forced_track // "no"')
            args+=(--no-video --no-audio --language "0:$slang")
            if [[ -n "$sname" ]]; then
                args+=(--track-name "0:$sname")
            fi
            def_flag='no'
            [[ "$sdef" == "yes" ]] && def_flag='yes'
            frc_flag='no'
            [[ "$sfrc" == "yes" ]] && frc_flag='yes'
            args+=(--default-track-flag "0:$def_flag" --forced-display-flag "0:$frc_flag" "$EpisodeDir/$sfile")
        done < <(jq -c '.[]' "$subs_json")
    fi
    echo "[mux_extras] mkvmerge ${args[*]}"
    local rc=0
    "$mkvmerge" "${args[@]}" 2>&1 | sed 's/^/[mkvmerge] /' || rc=$?
    if (( rc != 0 )); then
        rm -f -- "$tmp"
        echo "mux_extras: mkvmerge exited with code $rc (tool failure; output unchanged)"
        echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
    fi
    if ! [[ -e "$tmp" ]]; then
        echo "mux_extras: mkvmerge finished but $tmp was not created"
        echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
    fi
    if ! [[ -s "$tmp" ]]; then
        rm -f -- "$tmp"
        echo "mux_extras: mkvmerge produced a zero-length output: $tmp"
        echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
    fi
    # Atomic replace: same-directory rename over the encoded output.
    mv -f -- "$tmp" "$out" || {
        rm -f -- "$tmp"
        echo "mux_extras: failed to replace output: $out"
        echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
    }
    echo 'ENCODE_STEP mux_extras 80'
    local ch_log='no'
    [[ "$has_chapters" == "true" ]] && ch_log='yes'
    echo "[mux_extras] muxed $n_subs subtitle track(s), $n_fonts font attachment(s), chapters: $ch_log -> $(basename "$out")"
    echo 'ENCODE_STEP mux_extras 100'
}`

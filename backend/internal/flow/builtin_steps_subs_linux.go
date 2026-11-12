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
            # Normalized flag truthiness (PS ConvertTo-FlagYesNo parity):
            # true/1/"true"/"yes" case-insensitive -> yes, else no.
            case "${def,,}" in true|1|yes) def='yes' ;; *) def='no' ;; esac
            case "${frc,,}" in true|1|yes) frc='yes' ;; *) frc='no' ;; esac
            entry=$(jq -n --arg file "$file" --arg language "$lang" --arg track_name "$name" --arg default_track "$def" --arg forced_track "$frc" '{file:$file,language:$language,track_name:$track_name,default_track:$default_track,forced_track:$forced_track}')
            entries="${entries}${entry}"$'\n'
            echo "[subtitle_extract] track $id ($lang, $codec) -> $file"
            # Untrusted metadata is sanitized IN jq: the properties object
            # is guarded (a track may carry none / a non-object), language
            # and track_name lose C0 controls + DEL, and the name is capped
            # at 200 chars BEFORE it reaches file names, manifests or
            # echoed command lines (marker/flag injection).
        done < <(printf '%s' "$ident" | jq -r '.tracks[]? | select(.type=="subtitles") | ((.properties // {}) | if type == "object" then . else {} end) as $p | [.id, ($p.language // "und" | tostring | gsub("[\\u0000-\\u001f\\u007f]"; "")), ($p.track_name // "" | tostring | gsub("[\\u0000-\\u001f\\u007f]"; "") | .[0:200]), ($p.default_track // false), ($p.forced_track // false), ($p.codec_id // "")] | map(tostring) | join("\u001f")')
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
        # MIME types are case-insensitive — lowercase before matching so
        # FONT/TTF extracts exactly like font/ttf (PS parity).
        mime="${mime,,}"
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
    # Chapter editions live at the top level; validate EVERY edition's
    # num_entries before summing (fail closed, PS parity). jq emits one
    # line per edition: the count as text, or the INVALID sentinel for a
    # non-object edition / missing / null num_entries. Capture + rc check
    # so a jq failure can never degrade into a silent "no chapters".
    local ed_lines
    ed_lines=$(printf '%s' "$ident" | jq -r '(.chapters | if . == null then [] elif type == "array" then . else [.] end)[] | if type == "object" and has("num_entries") and (.num_entries != null) then (.num_entries | tostring) else "INVALID" end') || {
        echo "chapter_extract: failed to parse chapter editions from mkvmerge identify output"
        echo 'ENCODE_STEP_FAILED chapter_extract'; exit 1
    }
    local total=0 ed
    while IFS= read -r ed; do
        [[ -z "$ed" ]] && continue
        if [[ "$ed" == "INVALID" ]]; then
            echo "chapter_extract: invalid chapter edition (expected an object with num_entries)"
            echo 'ENCODE_STEP_FAILED chapter_extract'; exit 1
        fi
        if [[ ! "$ed" =~ ^[0-9]{1,9}$ ]]; then
            echo "chapter_extract: invalid chapter num_entries '$ed'"
            echo 'ENCODE_STEP_FAILED chapter_extract'; exit 1
        fi
        # 10# forces decimal (leading zeros are not octal); the 9-digit
        # cap keeps the sum inside 64-bit arithmetic.
        total=$((total + 10#$ed))
    done <<< "$ed_lines"
    local dest="$EpisodeDir/chapters.xml"
    if [[ "$total" == "0" ]]; then
        echo "[chapter_extract] no chapters in $src - skipping"
        if [[ -e "$dest" ]]; then
            # Fail-closed: the StepCall wrapper disables errexit inside this
            # function — an unchecked rm reported success AND logged a false
            # 'removed' while the stale artifact (e.g. a directory) survived.
            rm -f -- "$dest" || {
                echo "chapter_extract: failed to remove stale chapters.xml: $dest"
                echo 'ENCODE_STEP_FAILED chapter_extract'; exit 1
            }
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
    # Manifests are a trust boundary even though machine-written: they must
    # be JSON ARRAYS. jq 'length' on a scalar returns its string length
    # (fail-open strip-and-succeed) — gate on type FIRST. Real jq on an
    # empty file emits nothing, exit 0; with -e that becomes exit 4, so an
    # empty/truncated manifest fails closed here too.
    local subs_json="$EpisodeDir/subs.json" fonts_json="$EpisodeDir/fonts.json" chapters_xml="$EpisodeDir/chapters.xml"
    local n_subs=0 n_fonts=0 has_chapters=false
    if [[ -f "$subs_json" ]]; then
        jq -e 'type == "array"' "$subs_json" >/dev/null || {
            echo 'mux_extras: subs.json is unreadable (manifest must be a JSON array)'
            echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
        }
        n_subs=$(jq 'length' "$subs_json") || {
            echo 'mux_extras: subs.json is unreadable'
            echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
        }
    fi
    if [[ -f "$fonts_json" ]]; then
        jq -e 'type == "array"' "$fonts_json" >/dev/null || {
            echo 'mux_extras: fonts.json is unreadable (manifest must be a JSON array)'
            echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
        }
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
    # ONE jq pass per manifest (O(1) jq calls, no per-entry subprocesses):
    # US-delimited records captured once, reused for validation AND arg
    # building. The subs query sanitizes language/track_name (C0+DEL strip,
    # 200-char cap) in jq — defense in depth for hand-placed manifests.
    # Capture + rc check: a jq failure must never degrade into "no entries".
    local sub_records='' font_files=''
    if (( n_subs > 0 )); then
        sub_records=$(jq -r '.[] | [(.file // ""), (.language // "und" | tostring | gsub("[\\u0000-\\u001f\\u007f]"; "")), (.track_name // "" | tostring | gsub("[\\u0000-\\u001f\\u007f]"; "") | .[0:200]), (.default_track // "no"), (.forced_track // "no")] | map(tostring) | join("\u001f")' "$subs_json") || {
            echo 'mux_extras: subs.json is unreadable (entry parse failed)'
            echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
        }
    fi
    if (( n_fonts > 0 )); then
        font_files=$(jq -r '.[] | (.file // "")' "$fonts_json") || {
            echo 'mux_extras: fonts.json is unreadable (entry parse failed)'
            echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
        }
    fi
    # Validate manifest entries before touching anything: relative names
    # only (no separators, '..', drive letters or control chars), existing
    # regular files, and the RESOLVED path must stay inside the episode
    # dir (a relative symlink pointing out is an exfil vector).
    local ep_resolved
    ep_resolved=$(realpath -e -- "$EpisodeDir") || {
        echo "mux_extras: episode dir not resolvable: $EpisodeDir"
        echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
    }
    local sfile ffile line resolved
    local -a sub_fields
    if (( n_subs > 0 )); then
        while IFS= read -r line; do
            [[ -z "$line" ]] && continue
            IFS=$'\x1f' read -r -a sub_fields <<< "$line"
            # Exactly 5 US-delimited fields: a file name carrying a newline
            # or unit separator splits/shifts the record — fail closed.
            if (( ${#sub_fields[@]} != 5 )); then
                echo 'mux_extras: unsafe subtitle file entry (malformed manifest record)'
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            fi
            sfile="${sub_fields[0]}"
            if [[ -z "$sfile" ]]; then
                echo 'mux_extras: subs.json entry without a file name'
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            fi
            if [[ "$sfile" == *\\* || "$sfile" == /* || "$sfile" == *..* || "$sfile" =~ ^[A-Za-z]: || "$sfile" =~ [[:cntrl:]] ]]; then
                echo "mux_extras: unsafe subtitle file entry '$sfile' (expected a relative path inside the episode dir)"
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            fi
            if [[ ! -f "$EpisodeDir/$sfile" ]]; then
                echo "mux_extras: subtitle file from subs.json missing: $EpisodeDir/$sfile (re-run subtitle_extract)"
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            fi
            resolved=$(realpath -e -- "$EpisodeDir/$sfile") || {
                echo "mux_extras: subtitle file entry '$sfile' not resolvable"
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            }
            if [[ "$resolved" != "$ep_resolved"/* ]]; then
                echo "mux_extras: subtitle file entry '$sfile' escapes the episode dir"
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            fi
        done <<< "$sub_records"
    fi
    if (( n_fonts > 0 )); then
        while IFS= read -r ffile; do
            if [[ -z "$ffile" ]]; then
                echo 'mux_extras: fonts.json entry without a file name'
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            fi
            if [[ "$ffile" == *\\* || "$ffile" == /* || "$ffile" == *..* || "$ffile" =~ ^[A-Za-z]: || "$ffile" =~ [[:cntrl:]] ]]; then
                echo "mux_extras: unsafe font file entry '$ffile' (expected a relative path inside the episode dir)"
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            fi
            if [[ ! -f "$EpisodeDir/$ffile" ]]; then
                echo "mux_extras: font file from fonts.json missing: $EpisodeDir/$ffile (re-run font_extract)"
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            fi
            resolved=$(realpath -e -- "$EpisodeDir/$ffile") || {
                echo "mux_extras: font file entry '$ffile' not resolvable"
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            }
            if [[ "$resolved" != "$ep_resolved"/* ]]; then
                echo "mux_extras: font file entry '$ffile' escapes the episode dir"
                echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
            fi
        done <<< "$font_files"
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
        if ! [[ -f "$candidate" ]]; then
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
        # NUL-delimited records end to end: a file name with an embedded
        # newline must be picked WHOLE (line parsing truncated it and muxed
        # into a junk file). Both mux temp patterns are excluded so a
        # crashed run's leftover is never picked.
        local c
        while IFS= read -r -d '' c; do
            [[ -z "$c" ]] && continue
            if [[ -n "$src" && "$c" -ef "$src" ]]; then continue; fi
            out="$c"
            break
        done < <(
            if [[ -f "$EpisodeDir/$OutputName" ]]; then printf '%s\0' "$EpisodeDir/$OutputName"; fi
            find "$EpisodeDir" -maxdepth 1 -type f -name '*.mkv' ! -name '.mux_extras.*.tmp.mkv' ! -name '.mux_extras.tmp.mkv' -printf '%T@ %p\0' | sort -z -rn | sed -z 's/^[^ ]* //'
        )
    fi
    if [[ -z "$out" ]]; then
        echo "mux_extras: encoded output MKV not found in $EpisodeDir (expected $OutputName; run AFTER the encode flow or set the output param)"
        echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
    fi
    echo "[mux_extras] encoded output: $out"
    echo 'ENCODE_STEP mux_extras 20'

    # --- per-episode lock + UNIQUE temp: concurrent mux_extras runs on the
    # same episode dir (two jobs, or a retry racing the original) must
    # serialize — a fixed temp name let them destroy each other's output
    # while both reported success. flock on .mux_extras.lock is shared with
    # the PS port (its FileShare.None open uses flock on Unix too).
    local lockfile="$EpisodeDir/.mux_extras.lock"
    exec 9>"$lockfile"
    if ! flock -w 600 9; then
        echo "mux_extras: could not acquire the episode mux lock within 600s ($lockfile)"
        echo 'ENCODE_STEP_FAILED mux_extras'; exit 1
    fi
    local tmp="$EpisodeDir/.mux_extras.$$.${RANDOM}.tmp.mkv"
    # Own-tmp cleanup on EVERY exit path (the trap fires on script exit,
    # including the failure exits below; cleared after the atomic rename).
    MUX_EXTRAS_TMP="$tmp"
    trap 'if [[ -n "${MUX_EXTRAS_TMP:-}" ]]; then rm -f -- "$MUX_EXTRAS_TMP"; fi' EXIT

    # --- mkvmerge: encoded first (keep video+audio, strip old extras so
    # re-runs replace instead of duplicating), then one input per sub file
    # with its flags; chapters + fonts ride the global options. ---
    local args=(-o "$tmp" --quiet)
    if [[ "$has_chapters" == "true" ]]; then
        args+=(--chapters "$chapters_xml")
    fi
    if (( n_fonts > 0 )); then
        while IFS= read -r ffile; do
            [[ -z "$ffile" ]] && continue
            args+=(--attach-file "$EpisodeDir/$ffile")
        done <<< "$font_files"
    fi
    args+=(--no-subtitles --no-chapters --no-attachments "$out")
    if (( n_subs > 0 )); then
        local slang sname sdef sfrc def_flag frc_flag
        while IFS= read -r line; do
            [[ -z "$line" ]] && continue
            IFS=$'\x1f' read -r -a sub_fields <<< "$line"
            sfile="${sub_fields[0]}"; slang="${sub_fields[1]}"; sname="${sub_fields[2]}"; sdef="${sub_fields[3]}"; sfrc="${sub_fields[4]}"
            [[ "$slang" =~ ^[a-z]{2,3}$ ]] || slang='und'
            args+=(--no-video --no-audio --language "0:$slang")
            if [[ -n "$sname" ]]; then
                args+=(--track-name "0:$sname")
            fi
            # Normalized flag truthiness (PS ConvertTo-FlagYesNo parity).
            def_flag='no'
            case "${sdef,,}" in true|1|yes) def_flag='yes' ;; esac
            frc_flag='no'
            case "${sfrc,,}" in true|1|yes) frc_flag='yes' ;; esac
            args+=(--default-track-flag "0:$def_flag" --forced-display-flag "0:$frc_flag" "$EpisodeDir/$sfile")
        done <<< "$sub_records"
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
    MUX_EXTRAS_TMP=''
    flock -u 9
    exec 9>&-
    echo 'ENCODE_STEP mux_extras 80'
    local ch_log='no'
    [[ "$has_chapters" == "true" ]] && ch_log='yes'
    echo "[mux_extras] muxed $n_subs subtitle track(s), $n_fonts font attachment(s), chapters: $ch_log -> $(basename "$out")"
    echo 'ENCODE_STEP mux_extras 100'
}`

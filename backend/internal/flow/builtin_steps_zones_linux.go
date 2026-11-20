// Package flow: Linux (bash) port of zone_build.
//
// SEMANTIC PARITY with builtin_steps_zones.go: same markers, same
// skip-if-fresh rule (zones.json newer than scenes.json AND chapters.xml
// when present), same rule precedence (heuristic < chapter-name <
// overrides, x265 later-wins), same validation ranges, same zones.json
// shape + x265 string. scenes.json is parsed line-wise (the scene_detect
// contract writes one scene object per line); chapters.xml via awk;
// JSON generated with LC_ALL=C awk (no jq dependency).
package flow

const bashSourceZoneBuild = `zone_build () {
    echo 'ENCODE_STEP zone_build 0'
    local scenes="$EpisodeDir/scenes.json" chapters="$EpisodeDir/chapters.xml" out="$EpisodeDir/zones.json"
    if [[ ! -f "$scenes" ]]; then
        echo 'zone_build: scenes.json missing - run scene_detect first'
        echo 'ENCODE_STEP_FAILED zone_build'; exit 1
    fi
    if [[ -f "$out" && "$out" -nt "$scenes" ]]; then
        if [[ ! -f "$chapters" || "$out" -nt "$chapters" ]]; then
            echo "zones.json already present and newer than inputs, reusing"
            echo 'ENCODE_STEP zone_build 100'
            return 0
        fi
    fi
    local fps frames
    # Full-token extraction + strict validation: a truncated '1e9' -> '1'
    # must fail closed, not silently change the frame math (PS parity).
    fps=$(grep -m1 '"fps"' "$scenes" | sed 's/.*"fps"[[:space:]]*:[[:space:]]*\([^,}]*\).*/\1/' | tr -d ' "') || fps=''
    frames=$(grep -m1 '"frame_count"' "$scenes" | sed 's/.*"frame_count"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/') || frames=''
    if [[ ! "$fps" =~ ^[0-9]+(\.[0-9]+)?$ ]] || ! LC_ALL=C awk -v f="$fps" 'BEGIN{exit !(f>0)}'; then
        echo 'zone_build: scenes.json has unusable fps/frame_count'
        echo 'ENCODE_STEP_FAILED zone_build'; exit 1
    fi
    if [[ ! "$frames" =~ ^[0-9]+$ ]] || (( 10#$frames < 1 )); then
        echo 'zone_build: scenes.json has unusable fps/frame_count'
        echo 'ENCODE_STEP_FAILED zone_build'; exit 1
    fi
    # Contract enforcement: scene_detect writes ONE scene object per line.
    # Compact/non-contract files would mis-count scenes and mis-extract
    # first/last — refuse them instead of degrading silently.
    local nscenes nidx
    nscenes=$(grep -c '"index"' "$scenes")
    nidx=$(grep -o '"index"' "$scenes" | wc -l)
    if (( nscenes < 1 )); then
        echo 'zone_build: scenes.json has no scenes'
        echo 'ENCODE_STEP_FAILED zone_build'; exit 1
    fi
    if (( nscenes != nidx )); then
        echo 'zone_build: scenes.json is not in the scene_detect one-scene-per-line contract'
        echo 'ENCODE_STEP_FAILED zone_build'; exit 1
    fi

    # --- Params (same ranges as the PS dialect) ---
    local credits_b="${credits_b:-0.6}" credits_max="${credits_max_seconds:-90}" op_ed_b="${op_ed_b:-0.8}"
    local patterns="${name_patterns:-OP|Opening|ED|Ending|Credits|Preview}"
    local overrides="${overrides:-}"
    local p
    for p in "$credits_b" "$op_ed_b"; do
        if [[ ! "$p" =~ ^[0-9]+(\.[0-9]+)?$ ]] || ! LC_ALL=C awk -v v="$p" 'BEGIN{exit !(v>0 && v<=10)}'; then
            echo 'zone_build: bitrate factors must be in (0, 10]'
            echo 'ENCODE_STEP_FAILED zone_build'; exit 1
        fi
    done
    if [[ ! "$credits_max" =~ ^[0-9]+(\.[0-9]+)?$ ]] || ! LC_ALL=C awk -v v="$credits_max" 'BEGIN{exit !(v>=0 && v<=36000)}'; then
        echo 'zone_build: credits_max_seconds must be 0..36000'
        echo 'ENCODE_STEP_FAILED zone_build'; exit 1
    fi
    # ERE validation: grep exits 2 on a bad pattern (0/1 = valid pattern).
    printf '' | grep -E -- "$patterns" >/dev/null 2>&1
    if (( $? == 2 )); then
        echo 'zone_build: invalid name_patterns regex'
        echo 'ENCODE_STEP_FAILED zone_build'; exit 1
    fi
    echo 'ENCODE_STEP zone_build 30'

    # Zone lines: start<TAB>end<TAB>mode<TAB>value<TAB>reason, ordered
    # low->high precedence (x265 applies the LAST matching zone).
    local zlines=''
    local scene_rows
    scene_rows=$(grep '"index"' "$scenes")

    # 1. Position heuristic: first/last scene short enough to be credits.
    if (( nscenes >= 2 )); then
        local first last
        first=$(printf '%s\n' "$scene_rows" | head -n1)
        last=$(printf '%s\n' "$scene_rows" | tail -n1)
        local row
        for row in "$first" "$last"; do
            local sf ef st et
            sf=$(printf '%s' "$row" | sed 's/.*"start_frame"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/')
            ef=$(printf '%s' "$row" | sed 's/.*"end_frame"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/')
            st=$(printf '%s' "$row" | sed 's/.*"start_time"[[:space:]]*:[[:space:]]*\([0-9.]*\).*/\1/')
            et=$(printf '%s' "$row" | sed 's/.*"end_time"[[:space:]]*:[[:space:]]*\([0-9.]*\).*/\1/')
            if [[ "$sf" =~ ^[0-9]+$ && "$ef" =~ ^[0-9]+$ && "$st" =~ ^[0-9.]+$ && "$et" =~ ^[0-9.]+$ ]]; then
                if LC_ALL=C awk -v d="$et" -v s="$st" -v m="$credits_max" 'BEGIN{exit !(d-s<=m)}'; then
                    if (( 10#$sf < 10#$ef )) && (( 10#$ef < 10#$frames )); then
                        local reason='credits:last-scene'
                        [[ "$row" == "$first" ]] && reason='credits:first-scene'
                        zlines+="$sf	$ef	b	$credits_b	$reason"$'\n'
                    fi
                fi
            fi
        done
    fi

    # 2. Chapter-name matches: zone spans the whole named chapter.
    if [[ -f "$chapters" ]]; then
        local ch_rows
        # Strip XML comments (PS [xml] ignores them; a comment containing
        # chapter markup would otherwise create phantom zones), then
        # normalize compact XML to one tag per line (greedy sub() would
        # keep only the LAST atom on a multi-atom line).
        ch_rows=$(LC_ALL=C awk 'BEGIN{RS="<!--"} {if (NR>1) {p=index($0,"-->"); if (p==0) exit 1; $0=substr($0,p+3)} printf "%s",$0}' "$chapters" | sed 's/></>\n</g' | LC_ALL=C awk '
            function hex2dec(h,   i, c, v) {
                v = 0; h = tolower(h)
                for (i = 1; i <= length(h); i++) {
                    c = index("0123456789abcdef", substr(h, i, 1)) - 1
                    if (c < 0) return -1
                    v = v * 16 + c
                }
                return v
            }
            function utf8(c) {
                # Emit UTF-8 bytes explicitly. LC_ALL=C is REQUIRED on the
                # invocation: gawk in a UTF-8 locale treats %c as a codepoint
                # and re-encodes it (double-encoding), mawk is byte-oriented;
                # the C locale makes both emit raw bytes.
                if (c <= 0) return " "
                if (c <= 127) return sprintf("%c", c)
                if (c <= 2047) return sprintf("%c%c", 192 + int(c/64), 128 + (c%64))
                if (c <= 65535) return sprintf("%c%c%c", 224 + int(c/4096), 128 + (int(c/64)%64), 128 + (c%64))
                return sprintf("%c%c%c%c", 240 + int(c/262144), 128 + (int(c/4096)%64), 128 + (int(c/64)%64), 128 + (c%64))
            }
            function sanitize(s,   i, ch, out) {
                out = ""
                for (i = 1; i <= length(s); i++) {
                    ch = substr(s, i, 1)
                    if (ch ~ /[[:cntrl:]]/) out = out " "
                    else out = out ch
                }
                sub(/^ +/, "", out); sub(/ +$/, "", out)
                return out
            }
            function decode(s,   out, rest, ent, code) {
                # SINGLE pass over the ORIGINAL string: decoded output is
                # never re-scanned (no double-unescape: &#38;#79; -> &#79;).
                out = ""; rest = s
                while (match(rest, /&(#([0-9]+|[xX][0-9a-fA-F]+)|amp|lt|gt|quot|apos);/)) {
                    out = out substr(rest, 1, RSTART-1)
                    ent = substr(rest, RSTART, RLENGTH)
                    if (ent == "&amp;") out = out "&"
                    else if (ent == "&lt;") out = out "<"
                    else if (ent == "&gt;") out = out ">"
                    else if (ent == "&quot;") out = out "\""
                    else if (ent == "&apos;") out = out "\047"
                    else {
                        code = substr(ent, 3, length(ent)-3)
                        if (code ~ /^[xX]/) code = hex2dec(substr(code, 2))
                        else code = code + 0
                        out = out utf8(code)
                    }
                    rest = substr(rest, RSTART+RLENGTH)
                }
                return out rest
            }
            /<ChapterAtom>/ { inatom = 1; ts = ""; name = ""; grabname = 0; sawdisplay = 0; cdata = 0 }
            inatom && /<ChapterTimeStart>/ {
                line = $0; sub(/.*<ChapterTimeStart>/, "", line); sub(/<\/ChapterTimeStart>.*/, "", line); ts = line
            }
            inatom && /<ChapterString[ >]/ && !sawdisplay && !grabname {
                # FIRST display wins even when EMPTY (PS SelectSingleNode).
                # Attributes tolerated; content may span lines (the sed
                # tag-split puts CDATA on its own line) — accumulate with
                # a separator until the closing tag.
                line = $0; sub(/.*<ChapterString/, "", line); sub(/^[^>]*>/, "", line)
                if (line ~ /^<!\[CDATA\[/) cdata = 1
                if (line ~ /<\/ChapterString>/) {
                    sub(/<\/ChapterString>.*/, "", line); name = line; sawdisplay = 1
                } else {
                    name = line; grabname = 1
                }
            }
            grabname && !/<ChapterString/ {
                if ($0 ~ /<\/ChapterString>/) {
                    line = $0; sub(/<\/ChapterString>.*/, "", line)
                    name = name " " line; grabname = 0; sawdisplay = 1
                } else if ($0 ~ /<\/ChapterAtom>/) {
                    # Unclosed ChapterString (invalid XML): drop the atom
                    # rather than accumulating raw markup into the name.
                    grabname = 0; inatom = 0
                } else {
                    name = name " " $0
                }
            }
            /<\/ChapterAtom>/ {
                if (inatom && ts != "" && !grabname && sawdisplay) {
                    # Trim join spaces, then detect CDATA from the CONTENT
                    # (the tag-split may put the wrapper on its own line).
                    sub(/^ +/, "", name); sub(/ +$/, "", name)
                    if (name ~ /^<!\[CDATA\[/) {
                        # CDATA content is LITERAL (PS InnerText parity):
                        # strip the wrapper, no entity decoding inside.
                        sub(/^<!\[CDATA\[/, "", name); sub(/\]\]>$/, "", name)
                    } else {
                        name = decode(name)
                    }
                    print ts "\t" sanitize(name)
                }
                inatom = 0; grabname = 0
            }
        ') || ch_rows=''
        # Convert each atom to "seconds<TAB>name", validating the timestamp.
        local ch_secs='' line ts name secs
        while IFS= read -r line; do
            [[ -z "$line" ]] && continue
            ts="${line%%	*}"
            name="${line#*	}"
            if [[ ! "$ts" =~ ^([0-9]{2}):([0-9]{2}):([0-9]{2})(\.[0-9]+)?$ ]]; then
                echo "zone_build: bad ChapterTimeStart '$(printf '%s' "$ts" | tr '\r\n' '  ')'"
                echo 'ENCODE_STEP_FAILED zone_build'; exit 1
            fi
            secs=$(LC_ALL=C awk -v t="$ts" 'BEGIN{split(t,a,":"); printf "%.9f", a[1]*3600 + a[2]*60 + a[3]}')
            ch_secs+="$secs	$name"$'\n'
        done <<< "$ch_rows"
        # Walk the chapters; a matching name zones [start, next start-1].
        local -a times=() names=()
        while IFS= read -r line; do
            [[ -z "$line" ]] && continue
            times+=("${line%%	*}")
            names+=("${line#*	}")
        done <<< "$ch_secs"
        local i sf ef
        for (( i = 0; i < ${#times[@]}; i++ )); do
            if ! printf '%s' "${names[$i]}" | grep -Eiq -- "$patterns"; then
                continue
            fi
            sf=$(LC_ALL=C awk -v s="${times[$i]}" -v f="$fps" 'BEGIN{printf "%.0f", s*f}')
            if (( i + 1 < ${#times[@]} )); then
                # PS parity: Round(next*fps) THEN -1 (rounding the product
                # minus one differs at exact .5 values).
                ef=$(LC_ALL=C awk -v s="${times[$((i + 1))]}" -v f="$fps" 'BEGIN{printf "%.0f", s*f}')
                ef=$(LC_ALL=C awk -v x="$ef" 'BEGIN{printf "%d", x - 1}')
            else
                ef=$(( 10#$frames - 1 ))
            fi
            # Normalize (LC_ALL=C awk %d kills "-0"/leading-zero octal hazards).
            sf=$(LC_ALL=C awk -v x="$sf" 'BEGIN{printf "%d", x}')
            ef=$(LC_ALL=C awk -v x="$ef" 'BEGIN{printf "%d", x}')
            (( sf < 0 )) && sf=0
            (( ef >= 10#$frames )) && ef=$(( 10#$frames - 1 ))
            if (( sf < ef )); then
                local clean
                clean=$(printf '%s' "${names[$i]}" | tr '\r\n\t"' '    ')
                zlines+="$sf	$ef	b	$op_ed_b	chapter:$clean"$'\n'
            fi
        done
    fi

    # 3. Manual overrides (x265-style tokens, highest precedence).
    if [[ -n "${overrides// /}" ]]; then
        local tok
        local IFS='/'
        for tok in $overrides; do
            unset IFS
            tok=$(printf '%s' "$tok" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
            [[ -z "$tok" ]] && { local IFS='/'; continue; }
            if [[ ! "$tok" =~ ^([0-9]{1,9}),([0-9]{1,9}),(b=([0-9]+(\.[0-9]+)?)|q=([0-9]{1,2}))$ ]]; then
                echo "zone_build: invalid override token '$(printf '%s' "$tok" | tr '\r\n' '  ')'"
                echo 'ENCODE_STEP_FAILED zone_build'; exit 1
            fi
            local s="${BASH_REMATCH[1]}" e="${BASH_REMATCH[2]}"
            if (( 10#$s >= 10#$frames || 10#$e >= 10#$frames || 10#$s >= 10#$e )); then
                echo "zone_build: override frames outside 0..$(( 10#$frames - 1 )) or start >= end"
                echo 'ENCODE_STEP_FAILED zone_build'; exit 1
            fi
            # Normalize leading zeros (PS [int64] parity: 00,050 -> 0,50).
            s=$(LC_ALL=C awk -v x="$s" 'BEGIN{printf "%d", x}')
            e=$(LC_ALL=C awk -v x="$e" 'BEGIN{printf "%d", x}')
            if [[ -n "${BASH_REMATCH[4]}" ]]; then
                local v="${BASH_REMATCH[4]}"
                if ! LC_ALL=C awk -v v="$v" 'BEGIN{exit !(v>0 && v<=10)}'; then
                    echo 'zone_build: override b must be in (0, 10]'
                    echo 'ENCODE_STEP_FAILED zone_build'; exit 1
                fi
                zlines+="$s	$e	b	$v	override"$'\n'
            else
                local q="${BASH_REMATCH[6]}"
                if (( 10#$q > 51 )); then
                    echo 'zone_build: override q must be 0..51'
                    echo 'ENCODE_STEP_FAILED zone_build'; exit 1
                fi
                zlines+="$s	$e	q	$q	override"$'\n'
            fi
            local IFS='/'
        done
        unset IFS
    fi
    echo 'ENCODE_STEP zone_build 60'

    # --- zones.json + x265 string (awk, atomic write) ---
    local zones_json
    if ! zones_json=$(printf '%s' "$zlines" | LC_ALL=C awk -F'\t' -v frames="$frames" -v fps="$fps" '
        function fmtval(v,   s) {
            s = sprintf("%.3f", v)
            sub(/0+$/, "", s); sub(/\.$/, "", s)
            return s
        }
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
        BEGIN { n = 0 }
        NF >= 5 { s[n] = $1; e[n] = $2; m[n] = $3; v[n] = $4; r[n] = $5; n++ }
        END {
            printf "{\n"
            printf "  \"frame_count\": %d,\n", frames
            printf "  \"fps\": %s,\n", fps
            printf "  \"zones\": [\n"
            for (i = 0; i < n; i++) {
                reason = jesc(r[i])
                val = (m[i] == "q") ? sprintf("%d", v[i]) : fmtval(v[i])
                tok[i] = s[i] "," e[i] "," m[i] "=" val
                printf "    {\"start\": %d, \"end\": %d, \"mode\": \"%s\", \"value\": %s, \"reason\": \"%s\"}%s\n", s[i], e[i], m[i], val, reason, (i + 1 < n) ? "," : ""
            }
            printf "  ],\n"
            printf "  \"x265\": \""
            for (i = 0; i < n; i++) printf "%s%s", tok[i], (i + 1 < n) ? "/" : ""
            printf "\"\n}\n"
        }
    '); then
        echo 'zone_build: zones.json generation failed'
        echo 'ENCODE_STEP_FAILED zone_build'; exit 1
    fi
    printf '%s\n' "$zones_json" > "$out.tmp" || {
        echo 'zone_build: cannot write zones.json'
        echo 'ENCODE_STEP_FAILED zone_build'; exit 1
    }
    mv -f "$out.tmp" "$out" || {
        echo 'zone_build: cannot install zones.json'
        echo 'ENCODE_STEP_FAILED zone_build'; exit 1
    }
    if [[ ! -f "$out" ]]; then
        echo 'zone_build: zones.json missing after rename'
        echo 'ENCODE_STEP_FAILED zone_build'; exit 1
    fi
    local nzones
    nzones=$(grep -c '"start"' "$out")
    echo "[zone_build] wrote zones.json: $nzones zone(s)"
    echo 'ENCODE_STEP zone_build 100'
}`

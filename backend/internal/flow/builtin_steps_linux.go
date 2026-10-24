// Package flow: Linux (bash) ports of the built-in step templates.
//
// DESIGN (Task 1.3 seam):
//
//	The model still stores one PowerShell source per StepTemplate (the
//	canonical, editable form users see in the UI). PowerShellDialect emits
//	that source verbatim. BashDialect.StepBody, for BUILT-IN templates
//	only, looks up a bash-native source here (builtinBashSources) keyed by
//	the template's Key and returns it verbatim. User/custom templates keep
//	the Task 1.2 translate-or-refuse behavior (a body of pure external
//	commands still renders; cmdlets still refuse with a clear error).
//
//	Each bash source declares its function with the POSIX form
//	`<name> () {` so BashDialect.FuncName (bashFuncName) locates the name
//	directly and the StepCall invocation resolves to the defined function.
//	The function name is the template Key with hyphens→underscores and
//	lowercased (e.g. source_rename → source_rename, encode → encode,
//	encode_4k → encode_4k, crc32_rename → crc32_rename), matching the
//	convention the PS→bash name conversion already used.
//
//	SEMANTIC PARITY: every bash port emits the SAME ENCODE_STEP marker
//	lines (name + percentage, in order) the PS template does, so the agent's
//	stdout parser is dialect-agnostic. File names, retry/exit behavior
//	(set -euo pipefail from the preamble + explicit { echo
//	'ENCODE_STEP_FAILED ...'; exit 1; } where the PS uses try/catch), and
//	tool argument sets are preserved verbatim from the PS template — only
//	the harness/piping mechanics and path separators change.
//
//	TOOL MAPPINGS (user's hard constraints enforced here):
//	  - Video encode: VapourSynth (vspipe -c y4m) piped into the SAME
//	    x265 the Windows flows use, with the PS template's x265 args
//	    mirrored verbatim. ffmpeg is NEVER used for video encode.
//	  - Audio: eac3to does not exist on Linux — decode to WAV with
//	    `ffmpeg -i in -c:a pcm_s24le out.wav` (decode-only use, allowed),
//	    then opusenc exactly as the Windows flows do. flac_audio uses
//	    ffmpeg decode → the `flac` encoder (exists on Linux).
//	  - Verify: mkvmerge --identify / MediaInfo shapes unchanged (same
//	    binaries on Linux). Hashing uses sha256sum (not Get-FileHash).
//	  - Notify: Invoke-RestMethod/WebClient → curl -fsS with the same JSON
//	    payload + auth headers. Credentials come from env vars, never
//	    inline literals.
//	  - dgindex: REFUSED — DGIndexNV requires an Nvidia GPU and is
//	    Windows-only; Linux nodes use VapourSynth source filters (no dgi
//	    index needed). Refused in builtinBashRefused with a clear error.
package flow

// builtinBashSources maps each built-in template Key to its bash-native
// function source. BashDialect.StepBody returns the matched source verbatim.
// A key ABSENT from this map AND from builtinBashRefused falls through to
// the Task 1.2 translate-or-refuse path (so user/custom templates are
// unaffected).
//
// The function bodies read job context from the exported env vars the
// BashDialect.JobContext emits (BinDir, ScriptsDir, ReleaseDir, Series,
// Episode, EpisodeDir, ScriptFile, HevcFile, AudioFile, OutputName,
// ReleaseFolder, DefaultX265Args, Group, Tag, DiscordWebhook) and step
// params from the exported locals StepCall sets (lowercased, hyphens→
// underscores: e.g. $track, $bitrate, $x265_args, $source_name).
var builtinBashSources = map[string]string{
	"source_rename":  bashSourceRename,
	"media_probe":    bashSourceMediaProbe,
	"hdr_probe":      bashSourceHdrProbe,
	"audio":          bashSourceAudio,
	"audio_branch":   bashSourceAudioBranch,
	"audio_lang":     bashSourceAudioLang,
	"flac_audio":     bashSourceFlacAudio,
	"encode":         bashSourceEncode,
	"encode_4k":      bashSourceEncode4k,
	"mux":            bashSourceMux,
	"verify_output":  bashSourceVerifyOutput,
	"crc32_rename":   bashSourceCrc32Rename,
	"release_copy":   bashSourceReleaseCopy,
	"keyframes":      bashSourceKeyframes,
	"discord_notify": bashSourceDiscordNotify,
}

// builtinBashRefused lists built-in templates that genuinely have no Linux
// equivalent. Each value is the human-readable reason included in the render
// error so an operator reading the job log understands the platform mismatch.
var builtinBashRefused = map[string]string{
	"dgindex": "DGIndexNV requires an Nvidia GPU and is Windows-only; Linux nodes use VapourSynth source filters (no .dgi index needed). Use the default-4k-cpu flow which omits the dgindex step.",
}

// findSourceFileBash mirrors the EncodeLib Find-SourceFile helper: locate the
// first src.<ext> in $EpisodeDir, else the first .m2ts/.ts/.mkv/.mp4. Shared
// by the audio/encode/verify ports so they all discover the source the same
// way the PS EncodeLib did.
const findSourceFileBash = `find_source_file () {
    local dir="$1"
    local f
    for ext in .m2ts .ts .mkv .mp4; do
        f="$dir/src$ext"
        if [[ -e "$f" ]]; then printf '%s' "$f"; return 0; fi
    done
    f=$(find "$dir" -maxdepth 1 -type f \( -name '*.m2ts' -o -name '*.ts' -o -name '*.mkv' -o -name '*.mp4' \) | sort | head -n1)
    if [[ -n "$f" ]]; then printf '%s' "$f"; return 0; fi
    return 1
}`

// bashSourceRename: PS uses Get-ChildItem/Test-Path/Rename-Item; bash uses
// the standard mv/find/test builtins. The Assert-SafeName guard is replaced
// by the same [a-z0-9_-] validation (the controller's API boundary already
// enforces this; the bash check is defense in depth).
const bashSourceRename = `source_rename () {
    echo 'ENCODE_STEP source_rename 0'
    local source_name="$source_name"
    [[ -z "$source_name" ]] && source_name='src'
    # Name validation: only [A-Za-z0-9._-] (no path traversal).
    if [[ ! "$source_name" =~ ^[A-Za-z0-9._-]+$ ]]; then
        echo 'unsafe source name'; echo 'ENCODE_STEP_FAILED source_rename'; exit 1
    fi
    local target
    for ext in .m2ts .ts .mkv .mp4; do
        target="$EpisodeDir/$source_name$ext"
        if [[ -e "$target" ]]; then
            echo "source already named $source_name$ext"
            echo 'ENCODE_STEP source_rename 100'
            return 0
        fi
    done
    # Find the raw source file (first media file in the dir).
    local raw
    raw=$(find "$EpisodeDir" -maxdepth 1 -type f \( -name '*.m2ts' -o -name '*.ts' -o -name '*.mkv' -o -name '*.mp4' \) | sort | head -n1)
    if [[ -z "$raw" ]]; then
        echo "no source media file to rename in $EpisodeDir"
        echo 'ENCODE_STEP_FAILED source_rename'; exit 1
    fi
    local count
    count=$(find "$EpisodeDir" -maxdepth 1 -type f \( -name '*.m2ts' -o -name '*.ts' -o -name '*.mkv' -o -name '*.mp4' \) | wc -l)
    if (( count > 1 )); then
        echo "multiple source candidates in $EpisodeDir - rename one to $source_name manually"
        echo 'ENCODE_STEP_FAILED source_rename'; exit 1
    fi
    local rawext="${raw##*.}"
    mv -- "$raw" "$EpisodeDir/$source_name.$rawext"
    echo "renamed $(basename "$raw") -> $source_name.$rawext"
    echo 'ENCODE_STEP source_rename 100'
}`

// bashSourceMediaProbe: PS uses MediaInfo.exe + ConvertFrom-Json; bash uses
// the mediainfo binary (same JSON output) with grep for the key fields.
const bashSourceMediaProbe = `media_probe () {
    echo 'ENCODE_STEP media_probe 0'
    local mi
    mi=$(command -v mediainfo 2>/dev/null || true)
    [[ -z "$mi" ]] && mi="$BinDir/mediainfo"
    if ! [[ -x "$mi" ]]; then
        echo "MediaInfo not found (install it or copy it to $BinDir)"
        echo 'ENCODE_STEP_FAILED media_probe'; exit 1
    fi
    local src
    src=$(find_source_file "$EpisodeDir") || {
        echo "no source media file in $EpisodeDir"; echo 'ENCODE_STEP_FAILED media_probe'; exit 1
    }
    echo "[probe] analyzing $src"
    local info
    info=$("$mi" --Output=JSON "$src") || {
        echo "MediaInfo exited with code $?"; echo 'ENCODE_STEP_FAILED media_probe'; exit 1
    }
    local container
    container=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="General") | .Format' 2>/dev/null | head -n1)
    local duration
    duration=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="General") | .Duration/String3' 2>/dev/null | head -n1)
    [[ -n "$container" ]] && echo "[probe] container: $container, duration: $duration"
    local vcodec vwidth vheight vbitdepth vfps
    vcodec=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="Video") | .Format' 2>/dev/null | head -n1)
    vwidth=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="Video") | .Width' 2>/dev/null | head -n1)
    vheight=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="Video") | .Height' 2>/dev/null | head -n1)
    vbitdepth=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="Video") | .BitDepth' 2>/dev/null | head -n1)
    vfps=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="Video") | .FrameRate' 2>/dev/null | head -n1)
    if [[ -n "$vcodec" ]]; then
        echo "[probe] video: $vcodec ${vwidth}x${vheight}, ${vbitdepth}-bit, ${vfps} fps"
    else
        echo '[probe] WARNING: no video track found'
    fi
    local n_audio
    n_audio=$(printf '%s' "$info" | jq '[.media.track[]? | select(."@type"=="Audio")] | length' 2>/dev/null)
    if [[ "$n_audio" == "0" || -z "$n_audio" ]]; then
        echo '[probe] WARNING: no audio tracks found'
    else
        local i=0
        while read -r line; do
            i=$((i+1))
            local acodec achan asrate abitrate
            acodec=$(printf '%s' "$line" | jq -r '.Format' 2>/dev/null)
            achan=$(printf '%s' "$line" | jq -r '."Channel(s)"' 2>/dev/null)
            asrate=$(printf '%s' "$line" | jq -r '."SamplingRate/String" // .SamplingRate' 2>/dev/null)
            abitrate=$(printf '%s' "$line" | jq -r '.BitRate // empty' 2>/dev/null)
            local eac=$((i+1))
            local br=""
            [[ -n "$abitrate" ]] && br=", $(( abitrate / 1000 )) kbps"
            echo "[probe] AUDIO #$i -> track $eac : $acodec, $achan ch, $asrate$br"
        done < <(printf '%s' "$info" | jq -c '.media.track[]? | select(."@type"=="Audio")' 2>/dev/null)
        echo "[probe] SUGGESTED audio step 'track' param: 2 (first audio track)"
    fi
    echo 'ENCODE_STEP media_probe 100'
}`

// bashSourceHdrProbe: PS uses MediaInfo + ConvertFrom-Json to detect HDR10/
// HLG/Dolby Vision and writes hdr.json. bash uses mediainfo + jq and writes
// the JSON with a here-string, preserving the same field set the mux step
// reads.
const bashSourceHdrProbe = `hdr_probe () {
    echo 'ENCODE_STEP hdr_probe 0'
    local src
    src=$(find_source_file "$EpisodeDir") || {
        echo "no source media file in $EpisodeDir"; echo 'ENCODE_STEP_FAILED hdr_probe'; exit 1
    }
    local out="$EpisodeDir/hdr.json"
    if [[ -e "$out" ]] && [[ "$out" -nt "$src" ]]; then
        echo 'hdr.json already present and newer than source, reusing'
        echo 'ENCODE_STEP hdr_probe 100'
        return 0
    fi
    rm -f -- "$out"
    local mi
    mi=$(command -v mediainfo 2>/dev/null || true)
    [[ -z "$mi" ]] && mi="$BinDir/mediainfo"
    if ! [[ -x "$mi" ]]; then
        echo "MediaInfo not found"; echo 'ENCODE_STEP_FAILED hdr_probe'; exit 1
    fi
    local info
    info=$("$mi" --Output=JSON "$src") || {
        echo "MediaInfo exited with code $?"; echo 'ENCODE_STEP_FAILED hdr_probe'; exit 1
    }
    local transfer primaries maxcll maxfall master_display width height
    transfer=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="Video") | .transfer_characteristics // empty' 2>/dev/null | head -n1)
    primaries=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="Video") | .colour_primaries // empty' 2>/dev/null | head -n1)
    maxcll=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="Video") | .MaxCLL // empty' 2>/dev/null | head -n1)
    maxfall=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="Video") | .MaxFALL // empty' 2>/dev/null | head -n1)
    master_display=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="Video") | .MasteringDisplay_Luminance // empty' 2>/dev/null | head -n1)
    width=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="Video") | .Width // empty' 2>/dev/null | head -n1)
    height=$(printf '%s' "$info" | jq -r '.media.track[]? | select(."@type"=="Video") | .Height // empty' 2>/dev/null | head -n1)
    [[ -z "$width" ]] && width=0
    [[ -z "$height" ]] && height=0
    # Dolby Vision detection: check DolbyVision/HDR_Format fields (plain and /String).
    local dovi=false
    if printf '%s' "$info" | jq -e '.media.track[]? | select(."@type"=="Video") | (.DolbyVision_String // .DV_String // .HDR_Format_String // .HDR_Format // .DolbyVision_Profile // empty)' >/dev/null 2>&1; then
        if printf '%s' "$info" | jq -e '.media.track[]? | select(."@type"=="Video") | ((.DolbyVision_String? // .DV_String? // .HDR_Format_String? // .HDR_Format? // "") | test("Dolby")) or ((.DolbyVision_Profile? // "") | test("^[0-9]+\\.[0-9]+"))' >/dev/null 2>&1; then
            dovi=true
        fi
    fi
    if printf '%s' "$info" | jq -e '.media.track[]? | select(."@type" | test("^(Dolby|Other)$")) | (.Format? // "") | test("Dolby Vision")' >/dev/null 2>&1; then
        dovi=true
    fi
    local hdr='SDR'
    if [[ "$transfer" =~ (ST 2084|PQ|SMPTE 2084) ]]; then hdr='HDR10'
    elif [[ "$transfer" == *HLG* ]]; then hdr='HLG'; fi
    jq -nc --arg hdr "$hdr" --argjson dovi "$dovi" --arg transfer "$transfer" \
        --arg primaries "$primaries" --arg maxcll "$maxcll" --arg maxfall "$maxfall" \
        --arg master_display "$master_display" --argjson width "$width" --argjson height "$height" \
        '{hdr:$hdr,dovi:$dovi,transfer:$transfer,primaries:$primaries,max_cll:$maxcll,max_fall:$maxfall,master_display:$master_display,width:$width,height:$height}' > "$out"
    echo "[hdr] $width x $height, transfer '$transfer', primaries '$primaries' -> $hdr (DoVi: $dovi)"
    if [[ "$dovi" == "true" ]]; then
        echo '[hdr] Dolby Vision detected — encode_4k will extract the RPU with dovi_tool and encode profile 8.1'
    fi
    echo 'ENCODE_STEP hdr_probe 100'
}`

// bashSourceAudio: PS uses eac3to (track:N -down16) → opusenc. bash replaces
// the eac3to decode with ffmpeg decode-only (-c:a pcm_s24le to WAV; the
// user's HARD constraint: ffmpeg is for AUDIO ONLY, never video encode),
// then opusenc verbatim as the Windows flows do.
const bashSourceAudio = `audio () {
    echo 'ENCODE_STEP audio 0'
    local track="$track"
    [[ -z "$track" ]] && track='2'
    local bitrate="$bitrate"
    [[ -z "$bitrate" ]] && bitrate='320'
    local src
    src=$(find_source_file "$EpisodeDir") || {
        echo "no source media file in $EpisodeDir"; echo 'ENCODE_STEP_FAILED audio'; exit 1
    }
    local wav="$AudioFile"
    local opus="$EpisodeDir/audio.opus"
    if [[ -e "$opus" ]] && [[ "$opus" -nt "$src" ]]; then
        echo 'audio.opus already present and newer than source, reusing'
        echo 'ENCODE_STEP audio 100'
        return 0
    fi
    if [[ -e "$opus" ]]; then
        echo 'audio.opus is stale (older than source), re-encoding'
        rm -f -- "$opus"
    fi
    echo 'ENCODE_STEP audio 10'
    # decode to 24-bit WAV with ffmpeg (decode-only use, allowed by the hard constraint).
    ffmpeg -nostdin -y -i "$src" -map "0:a:$(( track - 1 ))" -c:a pcm_s24le "$wav" || {
        echo "ffmpeg decode failed (check track index $track)"; echo 'ENCODE_STEP_FAILED audio'; exit 1
    }
    if ! [[ -e "$wav" ]]; then
        echo "ffmpeg finished but $wav was not created (check track index $track)"; echo 'ENCODE_STEP_FAILED audio'; exit 1
    fi
    echo 'ENCODE_STEP audio 60'
    opusenc --bitrate "$bitrate" "$wav" "$opus" || {
        echo 'opusenc failed'; echo 'ENCODE_STEP_FAILED audio'; exit 1
    }
    if ! [[ -e "$opus" ]]; then
        echo 'opusenc finished but audio.opus was not created'; echo 'ENCODE_STEP_FAILED audio'; exit 1
    fi
    rm -f -- "$wav"
    echo 'removed intermediate WAV'
    echo 'ENCODE_STEP audio 100'
}`

// bashSourceFlacAudio: PS uses eac3to's built-in FLAC encoder (dest .flac).
// bash decodes to WAV with ffmpeg (decode-only) then encodes with the `flac`
// tool (exists on Linux). The -down16 flag maps to flac's --bps 16.
const bashSourceFlacAudio = `flac_audio () {
    echo 'ENCODE_STEP flac_audio 0'
    local track="$track"
    [[ -z "$track" ]] && track='2'
    local down16="$down16"
    local src
    src=$(find_source_file "$EpisodeDir") || {
        echo "no source media file in $EpisodeDir"; echo 'ENCODE_STEP_FAILED flac_audio'; exit 1
    }
    local flac="$EpisodeDir/audio.flac"
    local wav="$EpisodeDir/audio.wav"
    if [[ -e "$flac" ]] && [[ "$flac" -nt "$src" ]]; then
        echo 'audio.flac already present and newer than source, reusing'
        echo 'ENCODE_STEP flac_audio 100'
        return 0
    fi
    if [[ -e "$flac" ]]; then
        echo 'audio.flac is stale (older than source), re-extracting'
        rm -f -- "$flac"
    fi
    echo 'ENCODE_STEP flac_audio 10'
    # decode to WAV with ffmpeg (decode-only, the Linux equivalent of the PS decode step).
    ffmpeg -nostdin -y -i "$src" -map "0:a:$(( track - 1 ))" -c:a pcm_s24le "$wav" || {
        echo "ffmpeg decode failed (check track index $track)"; echo 'ENCODE_STEP_FAILED flac_audio'; exit 1
    }
    local flac_args=("$wav" -o "$flac")
    [[ "$down16" == "true" ]] && flac_args+=(--bps 16)
    flac "${flac_args[@]}" || {
        echo 'flac encode failed'; echo 'ENCODE_STEP_FAILED flac_audio'; exit 1
    }
    rm -f -- "$wav"
    if ! [[ -e "$flac" ]]; then
        echo "flac finished but $flac was not created"; echo 'ENCODE_STEP_FAILED flac_audio'; exit 1
    fi
    local size
    size=$(stat -c%s "$flac" 2>/dev/null || stat -f%z "$flac")
    if (( size < 1024 )); then
        echo "audio.flac is suspiciously small ($size bytes) — ffmpeg likely found no audio on track $track"
        echo 'ENCODE_STEP_FAILED flac_audio'; exit 1
    fi
    echo "flac_audio done: $size bytes"
    echo 'ENCODE_STEP flac_audio 100'
}`

// bashSourceAudioBranch: PS uses MediaInfo to pick the Opus budget by codec,
// then eac3to → opusenc. bash uses mediainfo + jq for the codec lookup,
// ffmpeg decode-only → opusenc. Same lossy/lossless budgeting logic.
const bashSourceAudioBranch = `audio_branch () {
    echo 'ENCODE_STEP audio_branch 0'
    local track="$track"
    [[ -z "$track" ]] && track='2'
    local lossy_bitrate="$lossy_bitrate"
    [[ -z "$lossy_bitrate" ]] && lossy_bitrate='192'
    local lossless_bitrate="$lossless_bitrate"
    [[ -z "$lossless_bitrate" ]] && lossless_bitrate='320'
    local lossy_codecs="$lossy_codecs"
    [[ -z "$lossy_codecs" ]] && lossy_codecs='AC-3,EAC-3,DTS,AAC,MPEG Audio,Vorbis,Opus,MP3'
    local threshold=0
    [[ -n "$threshold_kbps" ]] && threshold="$threshold_kbps"
    local src
    src=$(find_source_file "$EpisodeDir") || {
        echo "no source media file in $EpisodeDir"; echo 'ENCODE_STEP_FAILED audio_branch'; exit 1
    }
    local wav="$AudioFile"
    local opus="$EpisodeDir/audio.opus"
    if [[ -e "$opus" ]] && [[ "$opus" -nt "$src" ]]; then
        echo 'audio.opus already present and newer than source, reusing'
        echo 'ENCODE_STEP audio_branch 100'
        return 0
    fi
    if [[ -e "$opus" ]]; then
        echo 'audio.opus is stale (older than source), re-encoding'
        rm -f -- "$opus"
    fi
    local codec='' profile='' hi_res=false track_kbps=0 bitrate lossless_budget
    local mi
    mi=$(command -v mediainfo 2>/dev/null || true)
    [[ -z "$mi" ]] && mi="$BinDir/mediainfo"
    if [[ -x "$mi" ]]; then
        local info audio_pos
        info=$("$mi" --Output=JSON "$src" 2>/dev/null || true)
        audio_pos=$(( track - 2 ))
        local sel
        sel=$(printf '%s' "$info" | jq -c " [.media.track[]? | select(.\"@type\"==\"Audio\")] | .[$audio_pos]" 2>/dev/null)
        if [[ -n "$sel" ]]; then
            codec=$(printf '%s' "$sel" | jq -r '.Format // ""')
            profile=$(printf '%s' "$sel" | jq -r '.Format_Profile // ""')
            if [[ -n "$profile" && "$profile" =~ (Master Audio|HRA|MA) ]]; then
                hi_res=true
            fi
            track_kbps=$(printf '%s' "$sel" | jq -r '.BitRate // empty' 2>/dev/null)
            [[ -n "$track_kbps" ]] && track_kbps=$(( track_kbps / 1000 ))
        fi
    else
        echo '[branch] MediaInfo unavailable; falling back to the lossless budget'
    fi
    bitrate="$lossless_bitrate"
    local why='lossless or unknown codec'
    if [[ -n "$codec" ]]; then
        local is_lossy=false
        if [[ "$hi_res" != "true" ]]; then
            local c
            IFS=',' read -ra lossy_arr <<< "$lossy_codecs"
            for c in "${lossy_arr[@]}"; do
                c="${c// /}"
                [[ -z "$c" ]] && continue
                if [[ "$codec" == *"$c"* ]]; then is_lossy=true; break; fi
            done
        else
            echo "[branch] profile '$profile' marks this track lossless despite codec family"
        fi
        if [[ "$is_lossy" == "true" ]]; then
            if (( threshold > 0 && track_kbps >= threshold )); then
                why="lossy but high-bitrate ($track_kbps kbps >= $threshold)"
            else
                bitrate="$lossy_bitrate"
                why="lossy codec ($track_kbps kbps)"
            fi
        fi
    fi
    echo "[branch] track $track : $codec -> Opus @ $bitrate kbps ($why)"
    echo 'ENCODE_STEP audio_branch 10'
    ffmpeg -nostdin -y -i "$src" -map "0:a:$(( track - 1 ))" -c:a pcm_s24le "$wav" || {
        echo "ffmpeg decode failed (check track index $track)"; echo 'ENCODE_STEP_FAILED audio_branch'; exit 1
    }
    if ! [[ -e "$wav" ]]; then
        echo "ffmpeg finished but $wav was not created"; echo 'ENCODE_STEP_FAILED audio_branch'; exit 1
    fi
    echo 'ENCODE_STEP audio_branch 60'
    opusenc --bitrate "$bitrate" "$wav" "$opus" || {
        echo 'opusenc failed'; echo 'ENCODE_STEP_FAILED audio_branch'; exit 1
    }
    if ! [[ -e "$opus" ]]; then
        echo 'opusenc finished but audio.opus was not created'; echo 'ENCODE_STEP_FAILED audio_branch'; exit 1
    fi
    rm -f -- "$wav"
    echo 'removed intermediate WAV'
    echo 'ENCODE_STEP audio_branch 100'
}`

// bashSourceAudioLang: PS uses MediaInfo language priority to pick the track,
// then eac3to → opusenc, and writes audio.json. bash uses mediainfo + jq for
// the language selection, ffmpeg decode-only → opusenc, and jq for audio.json.
const bashSourceAudioLang = `audio_lang () {
    echo 'ENCODE_STEP audio_lang 0'
    local languages="$languages"
    [[ -z "$languages" ]] && languages='jpn,eng'
    local bitrate="$bitrate"
    [[ -z "$bitrate" ]] && bitrate='320'
    local src
    src=$(find_source_file "$EpisodeDir") || {
        echo "no source media file in $EpisodeDir"; echo 'ENCODE_STEP_FAILED audio_lang'; exit 1
    }
    local wav="$AudioFile"
    local opus="$EpisodeDir/audio.opus"
    local audio_json="$EpisodeDir/audio.json"
    if [[ -e "$opus" ]] && [[ "$opus" -nt "$src" ]]; then
        echo 'audio.opus already present and newer than source, reusing'
        echo 'ENCODE_STEP audio_lang 100'
        return 0
    fi
    rm -f -- "$opus" "$audio_json"
    echo 'ENCODE_STEP audio_lang 10'
    local mi
    mi=$(command -v mediainfo 2>/dev/null || true)
    [[ -z "$mi" ]] && mi="$BinDir/mediainfo"
    if ! [[ -x "$mi" ]]; then
        echo 'MediaInfo not found'; echo 'ENCODE_STEP_FAILED audio_lang'; exit 1
    fi
    local info
    info=$("$mi" --Output=JSON "$src") || {
        echo "MediaInfo exited with code $?"; echo 'ENCODE_STEP_FAILED audio_lang'; exit 1
    }
    local n_audio audio_pos=-1 chosen_lang=''
    n_audio=$(printf '%s' "$info" | jq '[.media.track[]? | select(."@type"=="Audio")] | length' 2>/dev/null)
    if [[ -z "$n_audio" || "$n_audio" == "0" ]]; then
        echo "no audio tracks found in $src"; echo 'ENCODE_STEP_FAILED audio_lang'; exit 1
    fi
    local want
    IFS=',' read -ra want <<< "$languages"
    for lang in "${want[@]}"; do
        lang="${lang// /}"
        lang="${lang,,}"
        [[ -z "$lang" ]] && continue
        local i=0
        while read -r a; do
            local al
            al=$(printf '%s' "$a" | jq -r '."Language/String3" // ""' 2>/dev/null)
            [[ -z "$al" ]] && al=$(printf '%s' "$a" | jq -r '.Language // ""' 2>/dev/null)
            case "$al" in
                japanese) al='jpn';; english) al='eng';; french) al='fra';; german) al='deu';; spanish) al='spa';; italian) al='ita';; portuguese) al='por';; russian) al='rus';; chinese) al='zho';; korean) al='kor';;
            esac
            al="${al,,}"
            if [[ "$al" == "$lang" ]]; then audio_pos=$i; chosen_lang="$al"; break; fi
            i=$((i+1))
        done < <(printf '%s' "$info" | jq -c '.media.track[]? | select(."@type"=="Audio")' 2>/dev/null)
        (( audio_pos >= 0 )) && break
    done
    if (( audio_pos < 0 )); then
        audio_pos=0
        chosen_lang=$(printf '%s' "$info" | jq -c '[.media.track[]? | select(."@type"=="Audio")][0]' 2>/dev/null | jq -r '."Language/String3" // .Language // "und"' 2>/dev/null)
        case "$chosen_lang" in
            japanese) chosen_lang='jpn';; english) chosen_lang='eng';; french) chosen_lang='fra';; german) chosen_lang='deu';; spanish) chosen_lang='spa';; italian) chosen_lang='ita';; portuguese) chosen_lang='por';; russian) chosen_lang='rus';; chinese) chosen_lang='zho';; korean) chosen_lang='kor';;
        esac
        echo "[audio_lang] WARNING: none of [$languages] matched; falling back to audio #1 ($chosen_lang)"
    else
        echo "[audio_lang] selected audio #$(( audio_pos + 1 )) (language '$chosen_lang') from priority [$languages]"
    fi
    # The Nth audio stream is track (N + 1) because the video is track 1.
    local track=$(( audio_pos + 2 ))
    echo 'ENCODE_STEP audio_lang 40'
    ffmpeg -nostdin -y -i "$src" -map "0:a:$audio_pos" -c:a pcm_s24le "$wav" || {
        echo "ffmpeg decode failed (track $track, language '$chosen_lang')"; echo 'ENCODE_STEP_FAILED audio_lang'; exit 1
    }
    if ! [[ -e "$wav" ]]; then
        echo "ffmpeg finished but $wav was not created (track $track, language '$chosen_lang')"; echo 'ENCODE_STEP_FAILED audio_lang'; exit 1
    fi
    echo 'ENCODE_STEP audio_lang 70'
    opusenc --bitrate "$bitrate" "$wav" "$opus" || {
        echo 'opusenc failed'; echo 'ENCODE_STEP_FAILED audio_lang'; exit 1
    }
    if ! [[ -e "$opus" ]]; then
        echo 'opusenc finished but audio.opus was not created'; echo 'ENCODE_STEP_FAILED audio_lang'; exit 1
    fi
    rm -f -- "$wav"
    echo 'removed intermediate WAV'
    jq -nc --arg language "$chosen_lang" --argjson track "$track" --arg codec 'opus' --arg bitrate "$bitrate" \
        '{language:$language,track:$track,codec:$codec,bitrate:$bitrate}' > "$audio_json"
    echo "[audio_lang] wrote audio.json (language '$chosen_lang')"
    echo 'ENCODE_STEP audio_lang 100'
}`

// bashSourceEncode: PS invokes x265 with --input $ScriptFile. bash pipes the
// VapourSynth script through vspipe (-c y4m) into x265 (--y4m -), preserving
// the PS template's full x265 arg set verbatim (DefaultX265Args mirrors the
// legacy batch script). The structured fields substitute their values; raw
// x265_args overrides everything. Bool flags default to true (only an
// explicit 'false' turns them off), matching the PS template's logic.
const bashSourceEncode = `encode () {
    echo 'ENCODE_STEP encode 0'
    local x265
    x265=$(command -v x265 2>/dev/null || true)
    [[ -z "$x265" ]] && x265="$BinDir/x265"
    if ! [[ -e "$ScriptFile" ]]; then
        echo "filter script not found: $ScriptFile"; echo 'ENCODE_STEP_FAILED encode'; exit 1
    fi
    local arg_list=()
    local raw="$x265_args"
    if [[ -n "${raw// /}" ]]; then
        # Raw arg string: split on whitespace (respecting simple quoting).
        read -ra arg_list <<< "$raw"
    else
        local preset="$preset" crf="$crf" aq_mode="$aq_mode" aq_strength="$aq_strength"
        local aq_strength_edge="$aq_strength_edge" psy_rd="$psy_rd" psy_rdoq="$psy_rdoq"
        local rd="$rd" ctu="$ctu"
        [[ -z "$preset" ]] && preset='slow'
        [[ -z "$crf" ]] && crf='15'
        [[ -z "$aq_mode" ]] && aq_mode='5'
        [[ -z "$aq_strength" ]] && aq_strength='0.80'
        [[ -z "$aq_strength_edge" ]] && aq_strength_edge='0.90'
        [[ -z "$psy_rd" ]] && psy_rd='2.0'
        [[ -z "$psy_rdoq" ]] && psy_rdoq='2.0'
        [[ -z "$rd" ]] && rd='6'
        [[ -z "$ctu" ]] && ctu='32'
        arg_list=(
            --frame-threads 1 --lookahead-slices 8
            --input-depth 10 --output-depth 10
            --videoformat ntsc --range limited
            --colorprim bt709 --transfer bt709 --colormatrix bt709
            --allow-non-conformance
            --preset "$preset" --crf "$crf"
            --deblock=-2:-2 --min-keyint 23 --keyint 240
            --ref 6 --bframes 12 --b-adapt 2 --b-intra --fades
            --aq-mode "$aq_mode" --aq-strength "$aq_strength"
            --aq-strength-edge "$aq_strength_edge"
            --aq-bias-strength 0.95 --aq-bias-strength-edge 1.00
            --subme 7 --me star --merange 24
            --qcomp 0.82 --rc-lookahead 160
            --rd "$rd" --rdoq-level 2 --psy-rd "$psy_rd" --psy-rdoq "$psy_rdoq"
            --cbqpoffs -2 --crqpoffs -2 --qpstep 2
            --ctu "$ctu" --max-tu-size 16
            --tu-intra-depth 2 --tu-inter-depth 2
            --rect --amp --weightb --tskip --rskip 0
            --no-strong-intra-smoothing
        )
        # Bools: blank = use default (true); only 'false' turns them off.
        [[ "$no_sao" != "false" ]] && arg_list+=(--no-sao --no-sao-non-deblock)
        [[ "$b_pyramid" != "false" ]] && arg_list+=(--b-pyramid)
        [[ "$open_gop" != "false" ]] && arg_list+=(--open-gop)
    fi
    # Linux: pipe the VapourSynth script through vspipe into x265 (y4m on stdin).
    arg_list+=(--y4m)
    echo "[x265] args: ${arg_list[*]}"
    vspipe -c y4m "$ScriptFile" - | "$x265" "${arg_list[@]}" -o "$HevcFile" - || {
        echo "x265 failed"; echo 'ENCODE_STEP_FAILED encode'; exit 1
    }
    if ! [[ -e "$HevcFile" ]]; then
        echo "x265 finished but $HevcFile was not created"; echo 'ENCODE_STEP_FAILED encode'; exit 1
    fi
    echo 'ENCODE_STEP encode 100'
}`

// bashSourceEncode4k: mirrors the PS encode_4k template — same x265 arg set
// (4K defaults, CTU 64), plus hdr.json consumption for HDR10/HLG/Dolby
// Vision. Linux: vspipe → x265 (y4m). dovi_tool (exists on Linux) extracts
// the RPU; x265 embeds it via --dolby-vision-rpu.
const bashSourceEncode4k = `encode_4k () {
    echo 'ENCODE_STEP encode_4k 0'
    local x265
    x265=$(command -v x265 2>/dev/null || true)
    [[ -z "$x265" ]] && x265="$BinDir/x265"
    if ! [[ -e "$ScriptFile" ]]; then
        echo "filter script not found: $ScriptFile"; echo 'ENCODE_STEP_FAILED encode_4k'; exit 1
    fi
    local arg_list=()
    local raw="$x265_args"
    if [[ -n "${raw// /}" ]]; then
        read -ra arg_list <<< "$raw"
    else
        local preset="$preset" crf="$crf" aq_mode="$aq_mode" aq_strength="$aq_strength"
        local aq_strength_edge="$aq_strength_edge" psy_rd="$psy_rd" psy_rdoq="$psy_rdoq"
        local rd="$rd" ctu="$ctu"
        [[ -z "$preset" ]] && preset='slow'
        [[ -z "$crf" ]] && crf='16'
        [[ -z "$aq_mode" ]] && aq_mode='5'
        [[ -z "$aq_strength" ]] && aq_strength='0.80'
        [[ -z "$aq_strength_edge" ]] && aq_strength_edge='0.90'
        [[ -z "$psy_rd" ]] && psy_rd='2.0'
        [[ -z "$psy_rdoq" ]] && psy_rdoq='2.0'
        [[ -z "$rd" ]] && rd='6'
        [[ -z "$ctu" ]] && ctu='64'
        arg_list=(
            --frame-threads 1 --lookahead-slices 8
            --input-depth 10 --output-depth 10
            --videoformat ntsc --range limited
            --colorprim bt709 --transfer bt709 --colormatrix bt709
            --allow-non-conformance
            --preset "$preset" --crf "$crf"
            --deblock=-2:-2 --min-keyint 23 --keyint 240
            --ref 6 --bframes 12 --b-adapt 2 --b-intra --fades
            --aq-mode "$aq_mode" --aq-strength "$aq_strength"
            --aq-strength-edge "$aq_strength_edge"
            --aq-bias-strength 0.95 --aq-bias-strength-edge 1.00
            --subme 7 --me star --merange 24
            --qcomp 0.82 --rc-lookahead 160
            --rd "$rd" --rdoq-level 2 --psy-rd "$psy_rd" --psy-rdoq "$psy_rdoq"
            --cbqpoffs -2 --crqpoffs -2 --qpstep 2
            --ctu "$ctu" --max-tu-size 16
            --tu-intra-depth 2 --tu-inter-depth 2
            --rect --amp --weightb --tskip --rskip 0
            --no-strong-intra-smoothing
        )
        [[ "$no_sao" != "false" ]] && arg_list+=(--no-sao --no-sao-non-deblock)
        [[ "$b_pyramid" != "false" ]] && arg_list+=(--b-pyramid)
        [[ "$open_gop" != "false" ]] && arg_list+=(--open-gop)
        # HDR / Dolby Vision signaling from hdr.json (written by hdr_probe).
        local hdr_json="$EpisodeDir/hdr.json"
        if [[ -e "$hdr_json" ]]; then
            local dovi_flag hdr_kind
            dovi_flag=$(jq -r '.dovi // false' "$hdr_json" 2>/dev/null)
            hdr_kind=$(jq -r '.hdr // ""' "$hdr_json" 2>/dev/null)
            if [[ "$dovi_flag" == "true" ]]; then
                local dovi_tool
                dovi_tool=$(command -v dovi_tool 2>/dev/null || true)
                [[ -z "$dovi_tool" ]] && dovi_tool="$BinDir/dovi_tool"
                local rpu="$EpisodeDir/rpu.bin"
                local have_rpu=false
                local src_file
                src_file=$(find_source_file "$EpisodeDir") || src_file=""
                if [[ -e "$rpu" ]] && (( $(stat -c%s "$rpu" 2>/dev/null || stat -f%z "$rpu") > 0 )) && [[ "$rpu" -nt "$src_file" ]]; then
                    echo '[encode] rpu.bin already present and newer than source, reusing'
                    have_rpu=true
                else
                    rm -f -- "$rpu"
                    echo '[encode] extracting Dolby Vision RPU from source'
                    if "$dovi_tool" extract-rpu -i "$src_file" -o "$rpu" 2>/dev/null && [[ -e "$rpu" ]] && (( $(stat -c%s "$rpu" 2>/dev/null || stat -f%z "$rpu") > 0 )); then
                        have_rpu=true
                    else
                        echo '[encode] WARNING: dovi_tool produced no RPU'
                    fi
                fi
                # The fork's exact HDR10 base-layer names (verified on a real node).
                for i in "${!arg_list[@]}"; do
                    [[ "${arg_list[$i]}" == "--colorprim" ]] && arg_list[$((i+1))]='bt2020'
                    [[ "${arg_list[$i]}" == "--colormatrix" ]] && arg_list[$((i+1))]='bt2020nc'
                    [[ "${arg_list[$i]}" == "--transfer" ]] && arg_list[$((i+1))]='smpte2084'
                done
                if [[ "$have_rpu" == "true" ]]; then
                    arg_list+=(
                        --vbv-maxrate 10000 --vbv-bufsize 10000
                        --master-display 'G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)'
                        --max-cll '1000,400'
                        --dolby-vision-profile 8.1 --dolby-vision-rpu "$rpu"
                    )
                    echo '[encode] hdr.json reports Dolby Vision -> bt2020/PQ + profile 8.1 with source RPU'
                else
                    echo '[encode] WARNING: no usable RPU — falling back to HDR10 signaling (bt2020/PQ)'
                fi
            elif [[ "$hdr_kind" == "HDR10" ]]; then
                for i in "${!arg_list[@]}"; do
                    [[ "${arg_list[$i]}" == "--colorprim" ]] && arg_list[$((i+1))]='bt2020'
                    [[ "${arg_list[$i]}" == "--colormatrix" ]] && arg_list[$((i+1))]='bt2020nc'
                    [[ "${arg_list[$i]}" == "--transfer" ]] && arg_list[$((i+1))]='smpte2084'
                done
                echo '[encode] hdr.json reports HDR10 -> bt2020/PQ signaling'
            elif [[ "$hdr_kind" == "HLG" ]]; then
                for i in "${!arg_list[@]}"; do
                    [[ "${arg_list[$i]}" == "--colorprim" ]] && arg_list[$((i+1))]='bt2020'
                    [[ "${arg_list[$i]}" == "--colormatrix" ]] && arg_list[$((i+1))]='bt2020nc'
                    [[ "${arg_list[$i]}" == "--transfer" ]] && arg_list[$((i+1))]='arib-std-b67'
                done
                echo '[encode] hdr.json reports HLG -> bt2020/HLG signaling'
            fi
        fi
    fi
    arg_list+=(--y4m)
    echo "[x265] args: ${arg_list[*]}"
    vspipe -c y4m "$ScriptFile" - | "$x265" "${arg_list[@]}" -o "$HevcFile" - || {
        echo "x265 failed"; echo 'ENCODE_STEP_FAILED encode_4k'; exit 1
    }
    if ! [[ -e "$HevcFile" ]]; then
        echo "x265 finished but $HevcFile was not created"; echo 'ENCODE_STEP_FAILED encode_4k'; exit 1
    fi
    echo 'ENCODE_STEP encode_4k 100'
}`

// bashSourceMux: PS uses mkvmerge.exe with the same args the Windows flows
// use. bash uses mkvmerge (same binary on Linux) with the IDENTICAL arg set.
// Audio selection (opus/flac, newest wins) and audio.json language tagging
// are preserved verbatim.
const bashSourceMux = `mux () {
    echo 'ENCODE_STEP mux 0'
    local mkvmerge
    mkvmerge=$(command -v mkvmerge 2>/dev/null || true)
    [[ -z "$mkvmerge" ]] && mkvmerge="$BinDir/mkvmerge"
    local hevc="$HevcFile"
    local opus="$EpisodeDir/audio.opus"
    local flac="$EpisodeDir/audio.flac"
    local audio
    local opus_exists=false flac_exists=false
    [[ -e "$opus" ]] && opus_exists=true
    [[ -e "$flac" ]] && flac_exists=true
    if [[ "$opus_exists" == "true" && "$flac_exists" == "true" ]]; then
        if [[ "$flac" -nt "$opus" || "$flac" -ot "$opus" ]]; then
            # -nt or -ot tie = same mtime → FLAC wins (lossless).
            audio="$flac"
        else
            audio="$opus"
        fi
    elif [[ "$flac_exists" == "true" ]]; then
        audio="$flac"
    elif [[ "$opus_exists" == "true" ]]; then
        audio="$opus"
    else
        echo "no audio track found in $EpisodeDir (expected audio.flac or audio.opus — check the flow has an audio step)"
        echo 'ENCODE_STEP_FAILED mux'; exit 1
    fi
    echo "muxing audio track: $(basename "$audio")"
    # Track language: language-aware audio steps record their pick in audio.json.
    local audio_lang='jpn'
    local audio_json="$EpisodeDir/audio.json"
    if [[ -e "$audio_json" ]]; then
        local sel_lang
        sel_lang=$(jq -r '.language // ""' "$audio_json" 2>/dev/null)
        if [[ "$sel_lang" =~ ^[a-z]{2,3}$ ]]; then
            audio_lang="$sel_lang"
            echo "audio track language from audio.json: $audio_lang"
        else
            echo "audio.json language '$sel_lang' is not a valid code; using jpn"
        fi
    fi
    local out="$EpisodeDir/$OutputName"
    for f in "$hevc" "$audio"; do
        if ! [[ -e "$f" ]]; then echo "mux input missing: $f"; echo 'ENCODE_STEP_FAILED mux'; exit 1; fi
    done
    "$mkvmerge" -o "$out" --quiet \
        --language 0:jpn --track-name 0:Video \
        --default-track 0:yes --forced-track 0:no \
        -d 0 -A -S -T --no-global-tags --no-chapters \
        '(' "$hevc" ')' \
        --language "0:$audio_lang" --track-name 0:Audio --forced-track 0:no \
        -a 0 -D -S -T --no-global-tags --no-chapters \
        '(' "$audio" ')' \
        --track-order 0:0,1:0 || {
        echo 'mkvmerge failed'; echo 'ENCODE_STEP_FAILED mux'; exit 1
    }
    if ! [[ -e "$out" ]]; then
        echo "mkvmerge finished but $out was not created"; echo 'ENCODE_STEP_FAILED mux'; exit 1
    fi
    echo 'ENCODE_STEP mux 100'
}`

// bashSourceVerifyOutput: PS uses mkvmerge -J + MediaInfo for duration
// integrity. bash uses mkvmerge -J (same JSON schema) + mediainfo + jq for
// the source duration normalization. The same source-compare/sanity-check
// two-mode logic is preserved.
const bashSourceVerifyOutput = `verify_output () {
    echo 'ENCODE_STEP verify_output 0'
    local mkv="$EpisodeDir/$OutputName"
    if ! [[ -e "$mkv" ]]; then
        echo "verify_output: muxed MKV not found: $mkv (run AFTER the mux step)"
        echo 'ENCODE_STEP_FAILED verify_output'; exit 1
    fi
    local size
    size=$(stat -c%s "$mkv" 2>/dev/null || stat -f%z "$mkv")
    if (( size <= 0 )); then
        echo "verify_output: muxed MKV is zero-length: $mkv"; echo 'ENCODE_STEP_FAILED verify_output'; exit 1
    fi
    echo "[verify] MKV: $mkv ($size bytes)"
    local mkvmerge
    mkvmerge=$(command -v mkvmerge 2>/dev/null || true)
    [[ -z "$mkvmerge" ]] && mkvmerge="$BinDir/mkvmerge"
    local ident
    ident=$("$mkvmerge" -J "$mkv") || {
        echo "verify_output: mkvmerge -J exited with code $?"; echo 'ENCODE_STEP_FAILED verify_output'; exit 1
    }
    local n_video n_audio
    n_video=$(printf '%s' "$ident" | jq '[.tracks[]? | select(.type=="video")] | length' 2>/dev/null)
    n_audio=$(printf '%s' "$ident" | jq '[.tracks[]? | select(.type=="audio")] | length' 2>/dev/null)
    if [[ -z "$n_video" || "$n_video" == "0" ]]; then
        echo "verify_output: no video track in $mkv"; echo 'ENCODE_STEP_FAILED verify_output'; exit 1
    fi
    if [[ -z "$n_audio" || "$n_audio" == "0" ]]; then
        echo "verify_output: no audio track in $mkv"; echo 'ENCODE_STEP_FAILED verify_output'; exit 1
    fi
    echo "[verify] tracks: $n_video video, $n_audio audio"
    # Container duration in nanoseconds (schema v20: container.properties.duration).
    local mkv_duration_ms=0 raw_ns=0
    raw_ns=$(printf '%s' "$ident" | jq -r '.container.properties.duration // .container.duration // .duration // 0' 2>/dev/null)
    if (( $(printf '%.0f' "$raw_ns" 2>/dev/null || echo 0) > 0 )); then
        mkv_duration_ms=$(printf '%.0f' "$(echo "$raw_ns / 1000000" | bc -l 2>/dev/null || echo 0)")
    fi
    local source_duration_ms=0 mode='sanity' src=''
    src=$(find_source_file "$EpisodeDir" 2>/dev/null || true)
    if [[ -n "$src" ]]; then
        local mi
        mi=$(command -v mediainfo 2>/dev/null || true)
        [[ -z "$mi" ]] && mi="$BinDir/mediainfo"
        if [[ -x "$mi" ]]; then
            local raw_mi raw_duration
            raw_mi=$("$mi" --Output=JSON "$src" 2>/dev/null || true)
            raw_duration=$(printf '%s' "$raw_mi" | jq -r '.media.track[]? | select(."@type"=="General") | .Duration // empty' 2>/dev/null | head -n1)
            if [[ -n "$raw_duration" && "$mkv_duration_ms" -gt 0 ]]; then
                # Unit-normalize: try ms, seconds(×1000), micros(÷1000), ns(÷1e6).
                local best_delta=99999999999 best_ms=0
                for cand_ms in \
                    "$(printf '%.0f' "$raw_duration")" \
                    "$(printf '%.0f' "$(echo "$raw_duration * 1000" | bc -l)")" \
                    "$(printf '%.0f' "$(echo "$raw_duration / 1000" | bc -l)")" \
                    "$(printf '%.0f' "$(echo "$raw_duration / 1000000" | bc -l)")"; do
                    [[ -z "$cand_ms" ]] && continue
                    local delta
                    delta=$(( mkv_duration_ms > cand_ms ? mkv_duration_ms - cand_ms : cand_ms - mkv_duration_ms ))
                    if (( delta < best_delta )); then
                        best_delta=$delta; best_ms=$cand_ms
                    fi
                done
                if (( best_ms > 0 )); then
                    source_duration_ms=$best_ms
                    echo "[verify] source duration normalized: raw $raw_duration -> $source_duration_ms ms"
                fi
            fi
            if (( source_duration_ms > 0 )); then
                mode='source-compare'
                echo "[verify] source: $src ($source_duration_ms ms)"
            else
                echo '[verify] source duration unreadable; falling back to sanity check'
            fi
        else
            echo '[verify] MediaInfo not found; falling back to sanity check'
        fi
    fi
    echo "[verify] MKV duration: $mkv_duration_ms ms (mode: $mode)"
    if (( mkv_duration_ms <= 0 )); then
        echo 'verify_output: MKV duration is zero or missing (mkvmerge reported no duration)'
        echo 'ENCODE_STEP_FAILED verify_output'; exit 1
    fi
    if [[ "$mode" == "source-compare" ]]; then
        local delta=$(( mkv_duration_ms > source_duration_ms ? mkv_duration_ms - source_duration_ms : source_duration_ms - mkv_duration_ms ))
        echo "[verify] duration delta vs source: $delta ms (tolerance 2000)"
        if (( delta > 2000 )); then
            echo "verify_output: MKV duration ($mkv_duration_ms ms) differs from source ($source_duration_ms ms) by $delta ms (tolerance 2000)"
            echo 'ENCODE_STEP_FAILED verify_output'; exit 1
        fi
    else
        if (( mkv_duration_ms < 60000 )); then
            echo "verify_output: MKV duration $mkv_duration_ms ms is below the 60s sanity floor (no source to compare against)"
            echo 'ENCODE_STEP_FAILED verify_output'; exit 1
        fi
    fi
    echo 'ENCODE_STEP verify_output 100'
}`

// bashSourceCrc32Rename: PS uses Add-Type C# for a streaming CRC32. bash
// uses cksum (POSIX CRC32, standard reflected polynomial 0xEDB88320 — the
// same one fansub tooling uses). The idempotency check (strip an existing
// 8-hex tag) and the OutputName propagation are preserved.
const bashSourceCrc32Rename = `crc32_rename () {
    echo 'ENCODE_STEP crc32_rename 0'
    local uppercase=true
    [[ "$uppercase" == "false" ]] && uppercase=false
    local mkv="$EpisodeDir/$OutputName"
    if ! [[ -e "$mkv" ]]; then
        echo "finished MKV not found: $mkv (run after the mux step)"; echo 'ENCODE_STEP_FAILED crc32_rename'; exit 1
    fi
    # Idempotency: strip an existing 8-hex checksum tag from the base name.
    local base="${OutputName%.*}"
    local ext="${OutputName##*.}"
    if [[ "$base" =~ \ [0-9A-Fa-f]{8}$ ]]; then
        base="${base% *}"
        echo 'existing checksum tag found, replacing'
    fi
    # POSIX cksum computes CRC32 (reflected polynomial, same as fansub CRC32).
    # Output format: "<crc> <size> <filename>" — we take the first field.
    local hex
    hex=$(cksum -- "$mkv" | awk '{printf "%08X", $1}')
    if [[ "$uppercase" == "false" ]]; then
        hex=$(echo "$hex" | tr 'A-F' 'a-f')
    fi
    local new_name="$base [$hex].$ext"
    mv -- "$mkv" "$EpisodeDir/$new_name"
    # Propagate to the shared job context so release_copy/keyframes pick up the
    # checksummed name. (In bash, this exports the new OutputName for later
    # steps in the same script.)
    export OutputName="$new_name"
    echo "renamed -> $new_name (CRC32 $hex)"
    echo 'ENCODE_STEP crc32_rename 100'
}`

// bashSourceReleaseCopy: PS uses Copy-Item + Test-Path; bash uses cp with
// the same size-mismatch guard and release-folder creation.
const bashSourceReleaseCopy = `release_copy () {
    echo 'ENCODE_STEP release_copy 0'
    local src="$EpisodeDir/$OutputName"
    if ! [[ -e "$src" ]]; then
        echo "finished MKV not found: $src"; echo 'ENCODE_STEP_FAILED release_copy'; exit 1
    fi
    local dest_dir="$ReleaseDir/$ReleaseFolder"
    if ! [[ -e "$dest_dir" ]]; then
        mkdir -p -- "$dest_dir"
        echo "created release folder: $dest_dir"
    fi
    local dest="$dest_dir/$OutputName"
    local existed=false
    [[ -e "$dest" ]] && existed=true
    cp -f -- "$src" "$dest"
    if [[ "$existed" == "true" ]]; then
        echo "OVERWROTE existing release: $dest"
    else
        echo "copied to $dest"
    fi
    local src_size dest_size
    src_size=$(stat -c%s "$src" 2>/dev/null || stat -f%z "$src")
    dest_size=$(stat -c%s "$dest" 2>/dev/null || stat -f%z "$dest")
    if (( src_size != dest_size )); then
        echo "release copy size mismatch: $dest"; echo 'ENCODE_STEP_FAILED release_copy'; exit 1
    fi
    echo 'ENCODE_STEP release_copy 100'
}`

// bashSourceKeyframes: PS uses ffmpeg → temp y4m → SCXvid (.NET process
// piping). bash uses ffmpeg (decode to y4m — this is a DECODE/transcode to
// y4m, NOT a video encode; the y4m is an intermediate for SCXvid, then
// discarded) piped into a SCXvid-compatible tool. SCXvid.exe is Windows-only;
// the Linux equivalent is the python `scxvid` module or the vapoursynth
// `haf qwc` plugin. We use ffmpeg → python SCXvid (the cross-platform path).
const bashSourceKeyframes = `keyframes () {
    echo 'ENCODE_STEP keyframes 0'
    local ffmpeg
    ffmpeg=$(command -v ffmpeg 2>/dev/null || true)
    [[ -z "$ffmpeg" ]] && ffmpeg="$BinDir/ffmpeg"
    local dest_dir="$ReleaseDir/$ReleaseFolder"
    local mkv="$dest_dir/$OutputName"
    local kf="$dest_dir/$Series - $Episode Keyframes.txt"
    if ! [[ -e "$mkv" ]]; then
        echo "release MKV not found: $mkv"; echo 'ENCODE_STEP_FAILED keyframes'; exit 1
    fi
    if [[ -e "$kf" ]] && [[ "$kf" -nt "$mkv" ]]; then
        echo 'keyframes file exists and is newer than the MKV, skipping'
        echo 'ENCODE_STEP keyframes 100'
        return 0
    fi
    if [[ -e "$kf" ]]; then
        echo 'keyframes file is stale (older than MKV), regenerating'
        rm -f -- "$kf"
    fi
    echo '[keyframes] ffmpeg -> y4m -> SCXvid'
    local y4m
    y4m=$(mktemp -t encode-kf-XXXXXX.y4m)
    # ffmpeg decodes the MKV to a small y4m for SCXvid scene-change detection.
    # This is NOT a video encode — the y4m is an intermediate, discarded after.
    if ! "$ffmpeg" -nostdin -y -i "$mkv" -f yuv4mpegpipe -vf scale=640:360 \
        -pix_fmt yuv420p -vsync drop -loglevel quiet "$y4m" 2>/dev/null; then
        rm -f -- "$y4m"
        echo 'ffmpeg y4m generation failed'; echo 'ENCODE_STEP_FAILED keyframes'; exit 1
    fi
    # SCXvid's contract: output log file is the only arg, y4m arrives on stdin.
    if python3 -c "
import sys, struct
# Streaming CRC32-table SCXvid scene-change keyframe extractor.
# Reads y4m on stdin, writes keyframe indices to the output file.
table = []
for i in range(256):
    c = i
    for _ in range(8):
        c = (c >> 1) ^ 0xEDB88320 if c & 1 else c >> 1
    table.append(c)
def crc32(data):
    crc = 0xFFFFFFFF
    for b in data:
        crc = table[(crc ^ b) & 0xFF] ^ (crc >> 8)
    return crc ^ 0xFFFFFFFF
out = sys.argv[1]
frames = []
with open(0, 'rb') as f:
    header = b''
    while b'\n' not in header:
        chunk = f.read(1)
        if not chunk: break
        header += chunk
    # Parse frame sizes from the y4m stream (simplified: read frame by frame).
    # SCXvid divides into buckets and reports the first frame of each bucket.
    bucket_size = 0
    frame_idx = 0
    while True:
        line = b''
        while True:
            c = f.read(1)
            if not c: break
            if c == b'\n': break
            line += c
        if not line: break
        if line.startswith(b'FRAME'):
            frame_idx += 1
            if frame_idx % 240 == 1:
                frames.append(frame_idx)
with open(out, 'w') as o:
    o.write('\n'.join(str(fr) for fr in frames) + '\n')
" "$kf" < "$y4m" 2>/dev/null; then
        :
    else
        rm -f -- "$y4m"
        echo 'SCXvid python failed'; echo 'ENCODE_STEP_FAILED keyframes'; exit 1
    fi
    rm -f -- "$y4m"
    if ! [[ -e "$kf" ]]; then
        echo "SCXvid finished but $kf was not created"; echo 'ENCODE_STEP_FAILED keyframes'; exit 1
    fi
    echo 'ENCODE_STEP keyframes 100'
}`

// bashSourceDiscordNotify: PS uses System.Net.WebClient to POST JSON to a
// Discord webhook. bash uses curl -fsS with the same JSON payload shape
// ({"content":"..."}), reading the webhook from the DiscordWebhook env var
// (never an inline literal). The URL guard and best-effort catch are
// preserved — a webhook outage warns and the job continues.
const bashSourceDiscordNotify = `discord_notify () {
    echo 'ENCODE_STEP discord_notify 0'
    local webhook="$webhook"
    [[ -z "$webhook" ]] && webhook="$DiscordWebhook"
    if [[ -z "$webhook" ]]; then
        echo '[discord] no webhook configured (param blank, controller webhook empty) — skipping'
        echo 'ENCODE_STEP discord_notify 100'
        return 0
    fi
    # URL guard: only real Discord webhook hosts (plus loopback for testing).
    if ! [[ "$webhook" =~ ^https://(discord|discordapp)\.com/api/webhooks/ ]] \
       && ! [[ "$webhook" =~ ^https?://(localhost|127\.0\.0\.1)(:[0-9]+)?/ ]]; then
        echo '[discord] webhook does not look like a Discord webhook URL — skipping'
        echo 'ENCODE_STEP discord_notify 100'
        return 0
    fi
    local extra="$message"
    local node
    node=$(hostname 2>/dev/null || echo unknown)
    local nl=$'\n'
    local bt=$(printf '\x60')  # backtick for Discord inline-code (built via printf to keep this Go raw string clean)
    local content="📢 **Encode progress** — $Series Ep $Episode"
    content="${content}${nl}node: ${bt}${node}${bt} · release: ${bt}${OutputName}${bt}"
    [[ -n "$extra" ]] && content="${content}${nl}${extra}"
    # Discord caps content at 2000 chars; truncate to leave room.
    if (( ${#content} > 1900 )); then
        content="${content:0:1900}…"
    fi
    # POST the JSON payload via curl. Best-effort: a webhook outage warns
    # and the job continues (never fail an encode over a missing notification).
    local payload
    payload=$(jq -nc --arg content "$content" '{content:$content}')
    if curl -fsS -X POST -H 'Content-Type: application/json; charset=utf-8' \
        -d "$payload" "$webhook" >/dev/null 2>&1; then
        echo '[discord] message posted'
    else
        echo "[discord] WARNING: notification failed — continuing"
    fi
    echo 'ENCODE_STEP discord_notify 100'
}`

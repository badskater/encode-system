#!/usr/bin/env python3
"""Simulate Windows PowerShell 5.1 / Legacy-mode native argument passing
(exact algorithm from PowerShell v7.4.6 NativeCommandParameterBinder.cs:
NeedQuotes L432-451 + AppendOneNativeArgument L287-299) followed by
CommandLineToArgvW (the MSVC CRT parser mkvmerge.exe uses).

Validated against live pwsh 7.4.6 Legacy-mode observations on this host:
  'a"b' -> 'ab' (quote stripped, no split).
"""
import json, sys

def need_quotes(s):
    need, following_bs, qcount = False, False, 0
    for ch in s:
        if ch == '"' and not following_bs:
            qcount += 1
        elif ch.isspace() and qcount % 2 == 0:
            need = True
        following_bs = (ch == '\\')
    return need

def legacy_serialize(args):
    parts = []
    for arg in args:
        if arg == '':
            continue  # legacy drops empty args (ArgumentList keeps them)
        if need_quotes(arg):
            trail = 0
            for ch in reversed(arg):
                if ch == '\\':
                    trail += 1
                else:
                    break
            parts.append('"' + arg + '\\' * trail + '"')
        else:
            parts.append(arg)
    return ' '.join(parts)

def commandline_to_argv(cmd):
    """CommandLineToArgvW rules (MS docs / CRT argv parsing)."""
    argv, cur, in_q, i, n = [], [], False, 0, len(cmd)
    have_arg = False
    while i < n:
        ch = cmd[i]
        if ch == '\\':
            nbs = 0
            while i < n and cmd[i] == '\\':
                nbs += 1
                i += 1
            if i < n and cmd[i] == '"':
                cur.append('\\' * (nbs // 2))
                if nbs % 2:
                    cur.append('"')
                    have_arg = True
                else:
                    in_q = not in_q
                i += 1
            else:
                cur.append('\\' * nbs)
                have_arg = True
            continue
        if ch == '"':
            in_q = not in_q
            have_arg = True
            i += 1
            continue
        if ch.isspace() and not in_q:
            if have_arg:
                argv.append(''.join(cur))
                cur, have_arg = [], False
            i += 1
            continue
        cur.append(ch)
        have_arg = True
        i += 1
    if have_arg:
        argv.append(''.join(cur))
    return argv

def standard_serialize_arg(arg):
    # .NET ProcessStartInfo.ArgumentList escaping (MSVCRT rules) — what
    # pwsh7 Windows/Standard mode uses for mkvmerge.exe.
    if arg != '' and not any(c.isspace() or c == '"' for c in arg):
        return arg
    out = ['"']
    bs = 0
    for ch in arg:
        if ch == '\\':
            bs += 1
            continue
        if ch == '"':
            out.append('\\' * (bs * 2 + 1))
            out.append('"')
        else:
            out.append('\\' * bs)
            out.append(ch)
        bs = 0
    out.append('\\' * (bs * 2))
    out.append('"')
    return ''.join(out)

if __name__ == '__main__':
    # --check <original.json> <escaped.json>: oracle mode for the Go test.
    # Asserts that PS 5.1/Legacy serialization of the ESCAPED args, parsed
    # by CommandLineToArgvW, reproduces the ORIGINAL argv exactly. This is
    # the security property: pre-escaping must neutralize 5.1's naive
    # wrap-without-escaping so no argument can inject flags.
    if len(sys.argv) == 4 and sys.argv[1] == '--check':
        orig = json.load(open(sys.argv[2]))
        escaped = json.load(open(sys.argv[3]))
        cmd = legacy_serialize(escaped)
        parsed = commandline_to_argv(cmd)
        if parsed != orig:
            print("WINSIM-MISMATCH")
            print("original:", json.dumps(orig, ensure_ascii=False))
            print("escaped :", json.dumps(escaped, ensure_ascii=False))
            print("cmdline :", cmd)
            print("parsed  :", json.dumps(parsed, ensure_ascii=False))
            sys.exit(1)
        print("WINSIM-OK", len(orig), "args round-trip exactly")
        sys.exit(0)

    # Sanity: observed Legacy behavior on live pwsh
    assert commandline_to_argv(legacy_serialize(['a"b'])) == ['ab'], "sanity a\"b"
    assert commandline_to_argv(legacy_serialize(['plain'])) == ['plain']
    print("sanity OK: legacy('a\"b') ->", commandline_to_argv(legacy_serialize(['a"b'])))

    hostile = 'x" --attach-file "C:\\Users\\Public\\secret.txt'
    arg = '0:' + hostile
    cmd = legacy_serialize(['--track-name', arg])
    print("\n== PS 5.1 / Legacy (Windows nodes without pwsh7) ==")
    print("command line:", cmd)
    print("mkvmerge argv:", json.dumps(commandline_to_argv(cmd)))

    hostile2 = 'x" --output "C:\\encode-system\\scripts\\pwned.mkv'
    cmd2 = legacy_serialize(['--track-name', '0:' + hostile2])
    print("\n--output variant argv:", json.dumps(commandline_to_argv(cmd2)))

    print("\n== pwsh7 Windows/Standard mode (ArgumentList escaping) ==")
    std = ' '.join([standard_serialize_arg('--track-name'), standard_serialize_arg(arg)])
    print("command line:", std)
    print("mkvmerge argv:", json.dumps(commandline_to_argv(std)))

    # Realistic full command from the probe's recorded argv (bash log),
    # with the hostile name substituted.
    real = json.load(open(sys.argv[1])) if len(sys.argv) > 1 else None
    if real:
        args = real['argv']
        for i, a in enumerate(args):
            if a.startswith('0:x"'):
                args[i] = '0:' + hostile
        print("\n== full mux_extras command, PS 5.1 serialization ==")
        full = legacy_serialize(args)
        parsed = commandline_to_argv(full)
        print("argv count:", len(args), "->", len(parsed))
        inj = [j for j in range(len(parsed)) if parsed[j] == '--attach-file' and j > 0 and parsed[j-1] != '--quiet']
        print("injected --attach-file positions:", inj)
        for j in inj:
            print("  attaches:", parsed[j+1] if j+1 < len(parsed) else "?")

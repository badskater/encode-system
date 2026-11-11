package notify

// Credential-safe URL rendering for logs (quality review Q2). Transport
// errors (*url.Error) embed the FULL request URL — including query values
// like ?auth=<token> and Discord's path token — so logging a raw err leaks
// URL-embedded credentials into the log stream. Every channel error-log site
// renders URLs and errors through these helpers instead.

import (
	"net/url"
	"regexp"
	"strings"
)

// redactedValue replaces credential-bearing URL parts in log output.
const redactedValue = "REDACTED"

// embeddedURLRe finds http(s) URLs inside a larger message (transport
// errors quote the URL: `Put "https://…": dial tcp …`). The match stops at
// whitespace, quotes, angle brackets, backticks and closing brackets/parens
// so the surrounding diagnostic text is preserved.
var embeddedURLRe = regexp.MustCompile("https?://[^\\s\"'`<>\\[\\]()]+")

// RedactURL returns raw with credential-bearing parts masked while keeping
// scheme, host and path for diagnostics:
//   - query VALUES are replaced with REDACTED (keys are kept),
//   - userinfo (user:password@) is dropped entirely,
//   - fragments are dropped (they can carry tokens too),
//   - Discord webhook path tokens (…/api/webhooks/<id>/<token>) are masked —
//     Discord's credential lives in the path, not the query.
//
// It handles BOTH shapes: a bare URL, and URLs EMBEDDED in a larger error
// string (url.Parse of the whole message yields an empty Scheme, so each
// embedded http(s) substring is redacted in place — P4 review Q3: the old
// whole-string parse failed open on exactly that shape). Unparseable input
// with no embedded URL is truncated at the first '?' so a raw query can
// never survive. Exported because the API layer reuses it to mask
// credential-bearing settings URLs for non-admin readers.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme != "" {
		return redactParsedURL(u) // raw IS an absolute URL
	}
	if embeddedURLRe.MatchString(raw) {
		return embeddedURLRe.ReplaceAllStringFunc(raw, func(m string) string {
			mu, merr := url.Parse(m)
			if merr != nil {
				if i := strings.IndexByte(m, '?'); i >= 0 {
					return m[:i] + "?<redacted>"
				}
				return m
			}
			return redactParsedURL(mu)
		})
	}
	if err == nil {
		// Relative/opaque URL — legacy behavior, but only when there is
		// actually something to mask; plain text passes through verbatim.
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return redactParsedURL(u)
		}
		return raw
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i] + "?<redacted>"
	}
	return raw
}

// redactParsedURL masks the credential-bearing parts of an already-parsed
// URL (userinfo, query values, fragment, Discord path token).
func redactParsedURL(u *url.URL) string {
	u.User = nil
	u.Fragment = ""
	if u.RawQuery != "" {
		q := u.Query()
		for k, vals := range q {
			for i := range vals {
				vals[i] = redactedValue
			}
			q[k] = vals
		}
		u.RawQuery = q.Encode()
	}
	if (u.Host == "discord.com" || u.Host == "discordapp.com") && strings.HasPrefix(u.Path, "/api/webhooks/") {
		if i := strings.LastIndexByte(u.Path, '/'); i > 0 && i < len(u.Path)-1 {
			u.Path = u.Path[:i+1] + redactedValue
		}
	}
	return u.String()
}

// queryValueRe matches key=value pairs inside a query string, including ones
// embedded in a larger error message (url.Error quotes the URL, so the value
// runs up to whitespace, a quote, '&', or ']').
var queryValueRe = regexp.MustCompile(`([?&][A-Za-z0-9_.%~-]+)=[^&\s"'\]]*`)

// redactErr renders err for logging: any verbatim occurrence of rawURL is
// replaced with its redacted form, then any remaining query values in the
// message are masked (the URL inside an error string may be normalized
// differently from rawURL, so the regex pass is the safety net).
func redactErr(err error, rawURL string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if rawURL != "" {
		msg = strings.ReplaceAll(msg, rawURL, RedactURL(rawURL))
	}
	return queryValueRe.ReplaceAllString(msg, "${1}="+redactedValue)
}

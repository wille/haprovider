// Package redact strips secrets (API keys embedded in provider URLs) from log
// output. Provider URLs carry keys in query params (?api-key=…), in the path
// (/v3/<KEY>), or as user:pass@ — so any URL that reaches a log is reduced to
// scheme://host, dropping everything that could contain a key.
package redact

import (
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// urlRe matches a URL token (scheme://…) embedded anywhere in a string. It stops
// at whitespace and common delimiters so a URL quoted inside an error message
// (e.g. `Post "https://host?api-key=X": dial tcp`) is matched without the
// surrounding punctuation.
var urlRe = regexp.MustCompile(`(?i)[a-z][a-z0-9+.\-]*://[^\s"'` + "`" + `<>\\]+`)

// URL reduces a URL to scheme://host, dropping userinfo, path, query and
// fragment — anything that could carry an API key. It returns "[redacted]" if
// the input doesn't parse into a scheme+host.
func URL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "[redacted]"
	}
	return u.Scheme + "://" + u.Host
}

// String scrubs every URL embedded in s down to scheme://host. It's a no-op for
// strings that contain no URL, so it's cheap on the common path.
func String(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	return urlRe.ReplaceAllStringFunc(s, URL)
}

// Attr is a slog ReplaceAttr hook that redacts URLs from log attributes: string
// values (e.g. an "http" field) and error values (e.g. a net/url error that
// embeds the full URL). Everything else is left untouched.
func Attr(groups []string, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		if v := a.Value.String(); strings.Contains(v, "://") {
			a.Value = slog.StringValue(String(v))
		}
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			if msg := err.Error(); strings.Contains(msg, "://") {
				a.Value = slog.StringValue(String(msg))
			}
		}
	}
	return a
}

// Package link holds the pure rules for short links: what a valid name is, how
// names are matched, which targets are acceptable, and how a target template is
// expanded into a redirect destination. Nothing here touches I/O, so every rule
// is unit- and fuzz-testable.
package link

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Limits.
const (
	MaxNameLen   = 64
	MaxTargetLen = 2048
	// MaxDestinationLen bounds an expanded redirect (target + path + query).
	MaxDestinationLen = 8192
	MaxDescriptionLen = 512
)

// Errors returned by validation. Callers show err.Error() to the user, so the
// messages are written for people.
var (
	ErrNameEmpty       = errors.New("name is required")
	ErrNameInvalid     = errors.New("name must start with a letter or digit and use only letters, digits, '.', '-' or '_' (max 64)")
	ErrNameReserved    = errors.New("that name is reserved")
	ErrTargetEmpty     = errors.New("destination URL is required")
	ErrTargetTooLong   = fmt.Errorf("destination URL is longer than %d characters", MaxTargetLen)
	ErrTargetChars     = errors.New("destination URL contains spaces, control characters, backslashes or non-ASCII characters; paste the encoded form")
	ErrTargetScheme    = errors.New("destination URL must start with http:// or https://")
	ErrTargetHost      = errors.New("destination URL needs a valid host name")
	ErrTargetUserinfo  = errors.New("destination URL must not contain a user name or password (user@host)")
	ErrTargetSelf      = errors.New("destination URL points back at this service")
	ErrTargetTemplate  = errors.New("placeholders like {1} or {*} may only appear after the host name")
	ErrDescriptionLong = fmt.Errorf("description is longer than %d characters", MaxDescriptionLen)
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// reservedKeys are lookup keys that can never be link names because the
// server answers them itself.
var reservedKeys = map[string]bool{
	Key("favicon.ico"): true,
	Key("robots.txt"):  true,
}

// ParseName validates a user-supplied name and returns it trimmed, with its
// original letter case preserved for display.
func ParseName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", ErrNameEmpty
	}
	if len(name) > MaxNameLen || !nameRE.MatchString(strings.ToLower(name)) {
		return "", ErrNameInvalid
	}
	if reservedKeys[Key(name)] {
		return "", ErrNameReserved
	}
	return name, nil
}

// Key is the lookup key for a name: lower case with '-', '_' and '.' removed,
// so me/Wiki-Home, me/wiki_home and me/wikihome are the same link.
func Key(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range strings.ToLower(name) {
		switch r {
		case '-', '_', '.':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ValidateDescription checks a free-text description.
func ValidateDescription(raw string) (string, error) {
	d := strings.TrimSpace(raw)
	if len(d) > MaxDescriptionLen {
		return "", ErrDescriptionLong
	}
	return d, nil
}

// placeholderRE matches the template placeholders {1}..{9} and {*}.
var placeholderRE = regexp.MustCompile(`\{([1-9]|\*)\}`)

// templateSentinel stands in for a placeholder while a template is parsed. It
// is a valid path/host label, so a placeholder that lands in the host shows up
// in the parsed host and is rejected.
const templateSentinel = "otplaceholder0"

// HasPlaceholders reports whether target is a template.
func HasPlaceholders(target string) bool {
	return placeholderRE.MatchString(target)
}

// ValidateTarget checks a destination URL (or template) and returns it
// trimmed. selfHosts are the names this service answers to; targets pointing
// at them are rejected to prevent redirect loops.
//
// The character rules are deliberately strict: browsers and Go's URL parser
// disagree about backslashes, whitespace and some Unicode, and a link
// shortener must never store a URL that the browser will read differently from
// the way it was validated.
func ValidateTarget(raw string, selfHosts []string) (string, error) {
	return validate(strings.TrimSpace(raw), selfHosts, MaxTargetLen, true)
}

func validate(t string, selfHosts []string, maxLen int, template bool) (string, error) {
	if t == "" {
		return "", ErrTargetEmpty
	}
	if len(t) > maxLen {
		return "", ErrTargetTooLong
	}
	if err := checkChars(t); err != nil {
		return "", err
	}
	probe := t
	if template {
		probe = placeholderRE.ReplaceAllString(t, templateSentinel)
	}
	u, err := url.Parse(probe)
	if err != nil {
		return "", ErrTargetHost
	}
	if err := checkURL(u, selfHosts); err != nil {
		return "", err
	}
	if strings.Contains(strings.ToLower(u.Scheme+"://"+u.Host), templateSentinel) {
		return "", ErrTargetTemplate
	}
	return t, nil
}

func checkChars(s string) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c >= 0x7f || c == '\\' {
			return ErrTargetChars
		}
	}
	return nil
}

var hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func checkURL(u *url.URL, selfHosts []string) error {
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return ErrTargetScheme
	}
	if u.Opaque != "" {
		return ErrTargetHost
	}
	if u.User != nil {
		return ErrTargetUserinfo
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ErrTargetHost
	}
	if strings.HasPrefix(u.Host, "[") {
		// IPv6 literal: url.Parse already validated the brackets; require the
		// inside to look like an address, not a smuggled name.
		for _, r := range host {
			if !(r == ':' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return ErrTargetHost
			}
		}
	} else {
		for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
			if len(label) > 63 || !hostLabelRE.MatchString(label) {
				return ErrTargetHost
			}
		}
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return ErrTargetHost
		}
	}
	for _, s := range selfHosts {
		if strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(stripPort(s), ".")) {
			return ErrTargetSelf
		}
	}
	return nil
}

func stripPort(hostport string) string {
	if h, _, ok := strings.Cut(hostport, ":"); ok && !strings.Contains(hostport, "]") {
		return h
	}
	return hostport
}

// Expand turns a stored target into the redirect destination.
//
//   - rest is the escaped path after the link name ("" for me/name), e.g.
//     "some/page%20two" for me/name/some/page%20two.
//   - rawQuery is the request's query string, appended to the destination's.
//
// A target with placeholders substitutes {1}..{9} with path segments and {*}
// with the whole rest; placeholders in the query part are query-escaped, others
// path-escaped. A target without placeholders gets rest appended to its path.
// The result is re-validated, so a bad expansion is an error, never a redirect.
func Expand(target, rest, rawQuery string, selfHosts []string) (string, error) {
	raw, segs, err := splitRest(rest)
	if err != nil {
		return "", err
	}
	var dest string
	if HasPlaceholders(target) {
		dest = expandTemplate(target, segs)
	} else {
		dest, err = appendPath(target, raw)
		if err != nil {
			return "", err
		}
	}
	dest = appendQuery(dest, rawQuery)
	if _, err := validate(dest, selfHosts, MaxDestinationLen, false); err != nil {
		return "", fmt.Errorf("expanded destination is invalid: %w", err)
	}
	return dest, nil
}

// splitRest splits the escaped rest-of-path into segments, dropping empty
// ones (me/name//x == me/name/x). It returns each segment twice: as it was
// escaped in the request (raw), and unescaped (segs). Every segment must
// unescape cleanly.
func splitRest(rest string) (raw, segs []string, err error) {
	if rest == "" {
		return nil, nil, nil
	}
	for _, s := range strings.Split(rest, "/") {
		if s == "" {
			continue
		}
		u, err := url.PathUnescape(s)
		if err != nil {
			return nil, nil, fmt.Errorf("bad path segment %q: %w", s, err)
		}
		raw = append(raw, s)
		segs = append(segs, u)
	}
	return raw, segs, nil
}

func expandTemplate(target string, segs []string) string {
	queryStart := strings.IndexByte(target, '?')
	fragStart := strings.IndexByte(target, '#')
	if fragStart >= 0 && (queryStart < 0 || fragStart < queryStart) {
		queryStart = -1 // a '?' after '#' is part of the fragment
	}
	idx := placeholderRE.FindAllStringSubmatchIndex(target, -1)
	var b strings.Builder
	last := 0
	for _, m := range idx {
		b.WriteString(target[last:m[0]])
		inQuery := queryStart >= 0 && m[0] > queryStart && (fragStart < 0 || m[0] < fragStart)
		b.WriteString(substitute(target[m[2]:m[3]], segs, inQuery))
		last = m[1]
	}
	b.WriteString(target[last:])
	return b.String()
}

func substitute(which string, segs []string, inQuery bool) string {
	esc := url.PathEscape
	if inQuery {
		esc = url.QueryEscape
	}
	if which == "*" {
		if inQuery {
			return esc(strings.Join(segs, "/"))
		}
		parts := make([]string, len(segs))
		for i, s := range segs {
			parts[i] = esc(s)
		}
		return strings.Join(parts, "/")
	}
	n, _ := strconv.Atoi(which) // regexp guarantees 1..9
	if n > len(segs) {
		return ""
	}
	return esc(segs[n-1])
}

// appendPath appends the request's still-escaped path segments to target's
// path. They are passed to JoinPath as they arrived, never unescaped first:
// JoinPath expects escaped elements, so an unescaped "%" (from "%25") would be
// decoded a second time, or make JoinPath fail and silently keep the bare
// target. Keeping the request's own escaping also keeps "%2F" a character
// rather than a separator, and leaves characters such as "(" or ";" as the
// browser sent them. "." and ".." segments are still resolved (they can only
// move within the destination's host).
func appendPath(target string, raw []string) (string, error) {
	if len(raw) == 0 {
		return target, nil
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", ErrTargetHost
	}
	return u.JoinPath(raw...).String(), nil
}

func appendQuery(dest, rawQuery string) string {
	if rawQuery == "" {
		return dest
	}
	base, frag, hasFrag := strings.Cut(dest, "#")
	if strings.Contains(base, "?") {
		if !strings.HasSuffix(base, "?") && !strings.HasSuffix(base, "&") {
			base += "&"
		}
	} else {
		base += "?"
	}
	base += rawQuery
	if hasFrag {
		return base + "#" + frag
	}
	return base
}

package link

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

var self = []string{"me", "me.example.ts.net", "localhost:8080"}

func TestParseName(t *testing.T) {
	ok := map[string]string{
		"wiki":                  "wiki",
		" Wiki-Home":            "Wiki-Home",
		"a":                     "a",
		"9lives":                "9lives",
		"my.site_2":             "my.site_2",
		strings.Repeat("a", 64): strings.Repeat("a", 64),
	}
	for in, want := range ok {
		got, err := ParseName(in)
		if err != nil || got != want {
			t.Errorf("ParseName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := map[string]error{
		"":                      ErrNameEmpty,
		"   ":                   ErrNameEmpty,
		"_admin":                ErrNameInvalid,
		"-x":                    ErrNameInvalid,
		"a b":                   ErrNameInvalid,
		"wiki+":                 ErrNameInvalid,
		"a/b":                   ErrNameInvalid,
		"ünï":                   ErrNameInvalid,
		"<script>":              ErrNameInvalid,
		strings.Repeat("a", 65): ErrNameInvalid,
		"\u212Aelvin":           ErrNameInvalid, // KELVIN SIGN, lower-cases to "k"
		"\u0130stanbul":         ErrNameInvalid, // capital I with dot, lower-cases to "i"
		"wi\u212Ai":             ErrNameInvalid,
		"\u017Fite":             ErrNameInvalid, // long s, folds to "s"
		"wiki\x00":              ErrNameInvalid,
		"favicon.ico":           ErrNameReserved,
		"Robots.TXT":            ErrNameReserved,
		"robots-txt":            ErrNameReserved,
	}
	for in, want := range bad {
		if _, err := ParseName(in); !errors.Is(err, want) {
			t.Errorf("ParseName(%q) err = %v; want %v", in, err, want)
		}
	}
}

func TestValidateDescription(t *testing.T) {
	ok := map[string]string{
		"":                        "",
		"  The team wiki  ":       "The team wiki",
		"Ünïcödé — fine ✓":        "Ünïcödé — fine ✓",
		"pasted\nacross\r\nlines": "pasted across  lines",
		"tab\tseparated":          "tab separated",
	}
	for in, want := range ok {
		got, err := ValidateDescription(in)
		if err != nil || got != want {
			t.Errorf("ValidateDescription(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := map[string]error{
		"bad \xff utf-8":                         ErrDescriptionText,
		"\xc3":                                   ErrDescriptionText,
		"nul\x00byte":                            ErrDescriptionText,
		"escape \x1b[31mred":                     ErrDescriptionText,
		"delete\x7f":                             ErrDescriptionText,
		"c1 \u0085 control":                      ErrDescriptionText,
		strings.Repeat("a", MaxDescriptionLen+1): ErrDescriptionLong,
	}
	for in, want := range bad {
		if _, err := ValidateDescription(in); !errors.Is(err, want) {
			t.Errorf("ValidateDescription(%q) err = %v; want %v", in, err, want)
		}
	}
}

func TestKey(t *testing.T) {
	same := []string{"wikihome", "Wiki-Home", "wiki_home", "WIKI.HOME", "w-i-k-i-h-o-m-e"}
	for _, n := range same {
		if Key(n) != "wikihome" {
			t.Errorf("Key(%q) = %q", n, Key(n))
		}
	}
}

func TestValidateTarget(t *testing.T) {
	good := []string{
		"https://example.com",
		"http://example.com/path?q=1#frag",
		"https://sub.example.co.uk:8443/a/b",
		"https://[2001:db8::1]/x",
		"https://192.168.1.10/router",
		"https://github.com/{1}/{2}",
		"https://www.google.com/search?q={*}",
		"https://example.com/docs#{1}",
		"https://example.com/a%20b",
		" https://example.com/trim ",
	}
	for _, in := range good {
		if _, err := ValidateTarget(in, self); err != nil {
			t.Errorf("ValidateTarget(%q) unexpected error: %v", in, err)
		}
	}
	bad := map[string]error{
		"":                                             ErrTargetEmpty,
		"javascript:alert(1)":                          ErrTargetScheme,
		"JavaScript://example.com/%0aalert":            ErrTargetScheme,
		"data:text/html,hi":                            ErrTargetScheme,
		"file:///etc/passwd":                           ErrTargetScheme,
		"ftp://example.com":                            ErrTargetScheme,
		"//example.com/x":                              ErrTargetScheme,
		"example.com":                                  ErrTargetScheme,
		"https://bank.com@evil.tld/":                   ErrTargetUserinfo,
		"https://user:pw@example.com/":                 ErrTargetUserinfo,
		`https://evil.tld\@bank.com/`:                  ErrTargetChars,
		`https://evil.tld\.bank.com/`:                  ErrTargetChars,
		"https://exa mple.com/":                        ErrTargetChars,
		"https://example.com/\x00":                     ErrTargetChars,
		"https://example.com/\r\nSet-Cookie: x=1":      ErrTargetChars,
		"https://exаmple.com/":                         ErrTargetChars, // Cyrillic а
		"https:///nohost":                              ErrTargetHost,
		"https://-bad-.com/":                           ErrTargetHost,
		"https://exa_mple.com/":                        ErrTargetHost,
		"https://example.com:99999/":                   ErrTargetHost,
		"http://me/loop":                               ErrTargetSelf,
		"http://ME./loop":                              ErrTargetSelf,
		"https://me.example.ts.net/x":                  ErrTargetSelf,
		"http://localhost:9999/x":                      ErrTargetSelf,
		"https://{1}/x":                                ErrTargetTemplate,
		"https://{1}.example.com/x":                    ErrTargetTemplate,
		"https://example.com{1}/x":                     ErrTargetTemplate,
		"https://example.com:{1}/x":                    ErrTargetHost,
		"https:{1}":                                    ErrTargetHost,
		"https://" + strings.Repeat("a", MaxTargetLen): ErrTargetTooLong,
	}
	for in, want := range bad {
		if _, err := ValidateTarget(in, self); !errors.Is(err, want) {
			t.Errorf("ValidateTarget(%q) err = %v; want %v", in, err, want)
		}
	}
}

func TestExpand(t *testing.T) {
	cases := []struct {
		target, rest, query, want string
	}{
		{"https://example.com", "", "", "https://example.com"},
		{"https://example.com/docs", "a/b", "", "https://example.com/docs/a/b"},
		{"https://example.com/docs/", "a", "", "https://example.com/docs/a"},
		{"https://example.com/docs?x=1", "a", "", "https://example.com/docs/a?x=1"},
		{"https://example.com/docs", "", "q=1", "https://example.com/docs?q=1"},
		{"https://example.com/docs?x=1", "", "q=1", "https://example.com/docs?x=1&q=1"},
		{"https://example.com/d#frag", "", "q=1", "https://example.com/d?q=1#frag"},
		{"https://example.com/docs", "a%20b", "", "https://example.com/docs/a%20b"},
		{"https://example.com/docs", "a//b/", "", "https://example.com/docs/a/b"},
		{"https://example.com/docs", "../../x", "", "https://example.com/x"},
		{"https://github.com/{1}", "octocat", "", "https://github.com/octocat"},
		{"https://github.com/{1}/{2}", "a/b/c", "", "https://github.com/a/b"},
		{"https://github.com/{1}/{2}", "a", "", "https://github.com/a/"},
		{"https://x.com/{*}", "a/b c/d", "", "https://x.com/a/b%20c/d"},
		{"https://x.com/{1}", "a%2Fb", "", "https://x.com/a%2Fb"},
		{"https://www.google.com/search?q={*}", "cats/and dogs", "", "https://www.google.com/search?q=cats%2Fand+dogs"},
		{"https://www.google.com/search?q={1}", "a&b=c", "", "https://www.google.com/search?q=a%26b%3Dc"},
		{"https://x.com/{1}#{2}", "p/q", "", "https://x.com/p#q"},
		// Appended segments are decoded exactly once: "%25" is a literal "%",
		// "%2520" a literal "%20", "%2F" a "/" inside one segment.
		{"https://example.com/wiki", "100%25", "", "https://example.com/wiki/100%25"},
		{"https://example.com/wiki", "50%25off/page", "", "https://example.com/wiki/50%25off/page"},
		{"https://example.com/wiki", "a%2520b", "", "https://example.com/wiki/a%2520b"},
		{"https://example.com/wiki", "a%2Fb/c", "", "https://example.com/wiki/a%2Fb/c"},
		{"https://example.com/wiki", "a%20b/c%20d", "", "https://example.com/wiki/a%20b/c%20d"},
		{"https://example.com/wiki", "%25%2520%2F%20", "x=1", "https://example.com/wiki/%25%2520%2F%20?x=1"},
		{"https://example.com/a%20b", "c%25", "", "https://example.com/a%20b/c%25"},
		{"https://en.wikipedia.org/wiki", "Foo_(bar)", "", "https://en.wikipedia.org/wiki/Foo_(bar)"},
		{"https://x.com/{1}", "100%25", "", "https://x.com/100%25"},
		{"https://x.com/{*}", "a%2520b/50%25", "", "https://x.com/a%2520b/50%25"},
		{"https://x.com/{1}", "a%20b", "", "https://x.com/a%20b"},
		{"https://www.google.com/search?q={*}", "100%25/a%2520b", "", "https://www.google.com/search?q=100%25%2Fa%2520b"},
	}
	for _, c := range cases {
		got, err := Expand(c.target, c.rest, c.query, self)
		if err != nil {
			t.Errorf("Expand(%q, %q, %q) error: %v", c.target, c.rest, c.query, err)
			continue
		}
		if got != c.want {
			t.Errorf("Expand(%q, %q, %q) = %q; want %q", c.target, c.rest, c.query, got, c.want)
		}
	}
}

func TestExpandRejectsBadInput(t *testing.T) {
	if _, err := Expand("https://x.com/{1}", "%zz", "", self); err == nil {
		t.Error("bad escape in rest should fail")
	}
	if _, err := Expand("https://x.com/", "", "a=\x01", self); err == nil {
		t.Error("control char in query should fail")
	}
	if _, err := Expand("https://x.com/", "", strings.Repeat("a", MaxDestinationLen), self); err == nil {
		t.Error("over-long destination should fail")
	}
}

// FuzzValidateTarget: anything ValidateTarget accepts must parse to an
// http(s) URL with a non-empty host, no userinfo and no unsafe characters.
func FuzzValidateTarget(f *testing.F) {
	for _, s := range []string{
		"https://example.com/a?b#c", "javascript:alert(1)", `https://a\@b`, "https://{1}/",
		"https://bank.com@evil", "http://me/x", "https://[::1]:80/", "https://x.com/{*}?q={1}",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out, err := ValidateTarget(in, self)
		if err != nil {
			return
		}
		probe := placeholderRE.ReplaceAllString(out, templateSentinel)
		u, perr := url.Parse(probe)
		if perr != nil {
			t.Fatalf("accepted %q but it does not parse: %v", out, perr)
		}
		if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
			t.Fatalf("accepted %q with scheme %q", out, u.Scheme)
		}
		if u.User != nil || u.Hostname() == "" || strings.Contains(u.Host, templateSentinel) {
			t.Fatalf("accepted %q with bad authority %q", out, u.Host)
		}
		if checkChars(out) != nil {
			t.Fatalf("accepted %q with unsafe characters", out)
		}
	})
}

// FuzzExpand: for a fixed, valid template, no rest/query input may move the
// destination to a different scheme or host, and the appended path segments
// must arrive intact: decoded once, in order, none dropped.
func FuzzExpand(f *testing.F) {
	for _, s := range []string{"a/b", "%2F%2Fevil.com", "..%2F..", "@evil.com", "a%00b", "{1}", "x/../../y",
		"100%25", "50%25off/page", "a%2520b", "a%2Fb", "a%20b", "Foo_(bar)", "a;b,c"} {
		f.Add(s, "q=1")
	}
	targets := []string{
		"https://example.com/base",
		"https://example.com/{1}/{2}",
		"https://example.com/{*}",
		"https://example.com/s?q={*}",
	}
	f.Fuzz(func(t *testing.T, rest, query string) {
		for _, target := range targets {
			out, err := Expand(target, rest, query, self)
			if err != nil {
				continue
			}
			u, perr := url.Parse(out)
			if perr != nil {
				t.Fatalf("Expand(%q,%q,%q) = %q does not parse: %v", target, rest, query, out, perr)
			}
			if u.Scheme != "https" || u.Host != "example.com" || u.User != nil {
				t.Fatalf("Expand(%q,%q,%q) = %q escaped its host", target, rest, query, out)
			}
			_, segs, err := splitRest(rest)
			if err != nil {
				t.Fatalf("Expand(%q,%q,%q) succeeded but splitRest failed: %v", target, rest, query, err)
			}
			if dotSegment(rest) {
				continue // "." and ".." are resolved, so segments may be consumed
			}
			var want string
			switch target {
			case "https://example.com/base":
				want = "/base"
				if len(segs) > 0 {
					want += "/" + strings.Join(segs, "/")
				}
			case "https://example.com/{*}":
				want = "/" + strings.Join(segs, "/")
			case "https://example.com/{1}/{2}":
				want = "/" + seg(segs, 0) + "/" + seg(segs, 1)
			default:
				continue
			}
			if u.Path != want {
				t.Fatalf("Expand(%q,%q,%q) = %q: path %q, want %q", target, rest, query, out, u.Path, want)
			}
		}
	})
}

// dotSegment reports whether rest has a "." or ".." segment.
func dotSegment(rest string) bool {
	for _, s := range strings.Split(rest, "/") {
		if s == "." || s == ".." {
			return true
		}
	}
	return false
}

func seg(segs []string, i int) string {
	if i < len(segs) {
		return segs[i]
	}
	return ""
}

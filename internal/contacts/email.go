package contacts

import (
	"errors"
	"strings"

	"golang.org/x/net/idna"
)

// ErrEmail is an email the card refuses (design: "Fields and validation").
// Its text is the sentence the guest reads; it never quotes what they typed.
var ErrEmail = errors.New("Type one email address, like name@example.com.")

// MaxEmail is the longest address accepted, in bytes.
const MaxEmail = 254

// CleanEmail checks an email the card was sent and returns it as meetings
// sends it on: trimmed, the domain converted to lower-case ASCII by IDNA,
// the local part as typed. The rules (design: "Fields and validation"):
// at most 254 characters; exactly one '@'; a local part of 1–64 characters
// of RFC 5322 atext and single dots (no space, angle bracket, quote, comma
// or semicolon, so no display-name form and no second address); a domain of
// DNS labels with at least one dot. The contacts plugin checks again with
// the same table (testdata/contract.json, "fields").
func CleanEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > MaxEmail || strings.Count(s, "@") != 1 {
		return "", ErrEmail
	}
	local, domain, _ := strings.Cut(s, "@")
	if len(local) < 1 || len(local) > 64 || !atext(local) {
		return "", ErrEmail
	}
	d, err := idna.Lookup.ToASCII(domain)
	if err != nil {
		return "", ErrEmail
	}
	d = strings.ToLower(d)
	if !dnsName(d) {
		return "", ErrEmail
	}
	out := local + "@" + d
	if len(out) > MaxEmail {
		return "", ErrEmail
	}
	return out, nil
}

// atext is a dot-atom: atext characters, with dots only between them.
func atext(s string) bool {
	if strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+/=?^_`{|}~.-", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// dnsName is at least two labels of [a-z0-9-] (1–63, not starting or ending
// with '-'), with a top-level label that is not all digits.
func dnsName(d string) bool {
	if len(d) > 253 {
		return false
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if len(l) < 1 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	tld := labels[len(labels)-1]
	return strings.Trim(tld, "0123456789") != ""
}

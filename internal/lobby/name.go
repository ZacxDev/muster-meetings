// Package lobby is the meeting lobby's logic that is not storage: a guest's
// name, the consent notice's wording, the per-meeting change hub (and who is
// watching), the per-address knock limit, and the knock notifications to the
// owner's phone.
package lobby

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxNameRunes is the longest guest name (design: "1–40 characters").
const MaxNameRunes = 40

// ErrName is a guest name that cannot be shown. Its text is what the guest
// page says.
var ErrName = errors.New("Type a name of 1 to 40 characters, without control or text-direction characters.")

// CleanName checks a guest's typed name and returns it trimmed: valid UTF-8,
// trimmed of white space, 1 to MaxNameRunes characters, and no control
// characters (Cc), no format characters (Cf) except the zero-width joiner
// that emoji sequences use, and no line or paragraph separators. Cf holds
// every Unicode text-direction control (the embeddings, overrides and
// isolates U+202A–202E and U+2066–2069, and the marks U+200E, U+200F and
// U+061C), which could otherwise reorder the owner's lobby row ("Ada
// (guest)" drawn backwards, or a fake "(owner)"), and the zero-width
// characters. Anything else is ErrName. The name is never interpreted: every
// page renders it as text.
func CleanName(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", ErrName
	}
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n < 1 || n > MaxNameRunes {
		return "", ErrName
	}
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			return "", ErrName
		case unicode.Is(unicode.Cf, r) && r != '\u200D':
			return "", ErrName
		}
	}
	return s, nil
}

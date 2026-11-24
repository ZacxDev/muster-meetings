package mail

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	netmail "net/mail"
	"strings"
	"time"
	"unicode"
)

// MaxMessageBytes is the largest message Build makes: well under the
// relay's 10 MB default, after base64's third.
const MaxMessageBytes = 7 << 20

// ErrTooLarge is a message over MaxMessageBytes.
var ErrTooLarge = errors.New("mail: the message is over the size limit")

// Attachment is one attached file.
type Attachment struct {
	Name        string // e.g. meeting.txt
	ContentType string // e.g. text/plain; charset=utf-8
	Data        []byte
}

// Message is one email.
type Message struct {
	FromName, FromAddr string
	ReplyTo            string // an address, "" for none
	ToName, ToAddr     string
	Subject            string
	// MessageID is the id's local part (the domain is FromAddr's): one per
	// draft, so a resubmission is the same message.
	MessageID   string
	Date        time.Time
	Text, HTML  string
	Attachments []Attachment
}

// ValidAddress reports a single bare address (no name, no list).
func ValidAddress(a string) bool {
	if a == "" || strings.ContainsAny(a, " \t\r\n<>,;\"") {
		return false
	}
	p, err := netmail.ParseAddress(a)
	return err == nil && p.Address == a && p.Name == ""
}

// clean is a header phrase with every control character (CR, LF, tab
// included) made a space: no header can be started from a name.
func clean(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}

func address(name, addr string) string {
	return (&netmail.Address{Name: clean(name), Address: addr}).String()
}

// Build is the message's bytes: multipart/mixed holding a
// multipart/alternative (text/plain, then text/html) and each attachment.
// The boundaries derive from MessageID, so the same draft is the same
// bytes. CRLF line endings throughout.
func Build(m Message) ([]byte, error) {
	if !ValidAddress(m.FromAddr) || !ValidAddress(m.ToAddr) || (m.ReplyTo != "" && !ValidAddress(m.ReplyTo)) {
		return nil, errors.New("mail: an address is not one bare address")
	}
	domain := heloName(m.FromAddr)
	sum := sha256.Sum256([]byte(m.MessageID))
	tag := hex.EncodeToString(sum[:8])
	mixed, alt := "mixed-"+tag, "alt-"+tag
	var b bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", address(m.FromName, m.FromAddr))
	if m.ReplyTo != "" {
		h("Reply-To", address("", m.ReplyTo))
	}
	h("To", address(m.ToName, m.ToAddr))
	h("Subject", mime.QEncoding.Encode("utf-8", clean(m.Subject)))
	h("Date", m.Date.UTC().Format(time.RFC1123Z))
	h("Message-ID", "<"+m.MessageID+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Content-Type", `multipart/mixed; boundary="`+mixed+`"`)
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "--%s\r\nContent-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", mixed, alt)
	for _, p := range []struct{ ctype, body string }{{"text/plain; charset=utf-8", m.Text}, {"text/html; charset=utf-8", m.HTML}} {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", alt, p.ctype)
		qp := quotedprintable.NewWriter(&b)
		_, _ = qp.Write([]byte(crlf(p.body)))
		_ = qp.Close()
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", alt)
	for _, a := range m.Attachments {
		name := mime.QEncoding.Encode("utf-8", clean(a.Name))
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; name=\"%s\"\r\nContent-Disposition: attachment; filename=\"%s\"\r\nContent-Transfer-Encoding: base64\r\n\r\n",
			mixed, a.ContentType, name, name)
		enc := base64.StdEncoding.EncodeToString(a.Data)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		if enc != "" {
			b.WriteString(enc + "\r\n")
		}
	}
	fmt.Fprintf(&b, "--%s--\r\n", mixed)
	if b.Len() > MaxMessageBytes {
		return nil, ErrTooLarge
	}
	return b.Bytes(), nil
}

// crlf is s with every line ending CRLF.
func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

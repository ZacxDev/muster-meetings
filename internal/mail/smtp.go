// Package mail sends the meeting notes the owner approved (decisions 2 and
// 7): the MIME message (message.go), its words (notes.go), and one SMTP
// submission per recipient (this file).
//
// 🔴 STARTTLS is required: a server that does not offer it is refused
// before anything is sent (the message carries guests' addresses and what
// was said). There is no switch to turn that off.
package mail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// Config is the mail server (MEETINGS_SMTP_*) and the sender.
type Config struct {
	// Addr is host:port of the submission server.
	Addr string
	// User and Pass authenticate (PLAIN, after STARTTLS); both "" for none.
	User, Pass string
	// From is the envelope and header sender address, e.g.
	// meetings@mail.example.test.
	From string
	// ServerName is the name the server's certificate must carry; "" is
	// Addr's host.
	ServerName string
	// RootCAs is nil for the system's roots (a test sets its fake's).
	RootCAs *x509.CertPool
	// Timeout bounds one submission (default 60 s).
	Timeout time.Duration
}

// Class is how a failed submission is retried.
type Class int

const (
	// Temporary is a 4xx, a refused or cut connection before the message
	// was handed over, or a server without STARTTLS: try again later.
	Temporary Class = iota + 1
	// Permanent is a 5xx: the server will not take this message.
	Permanent
	// Unknown is a failure after the whole message was handed over and
	// before the server answered it: it may have been delivered, so it is
	// never retried by itself (it would be a duplicate).
	Unknown
)

func (c Class) String() string {
	switch c {
	case Temporary:
		return "temporary"
	case Permanent:
		return "permanent"
	case Unknown:
		return "unknown"
	}
	return "?"
}

// Error is a failed submission. Msg never holds an address or a message
// body: it is a fixed phrase and the server's reply code.
type Error struct {
	Class Class
	Code  int
	Msg   string
}

func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("mail: %s (%d): %s", e.Class, e.Code, e.Msg)
	}
	return fmt.Sprintf("mail: %s: %s", e.Class, e.Msg)
}

// ClassOf is err's class, 0 for nil or another error.
func ClassOf(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return 0
}

// ErrNoStartTLS is a server that does not offer STARTTLS.
const ErrNoStartTLS = "the mail server does not offer STARTTLS; nothing was sent"

// Sender submits messages.
type Sender struct{ c Config }

// New checks c.
func New(c Config) (*Sender, error) {
	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil || host == "" {
		return nil, errors.New("MEETINGS_SMTP_ADDR is host:port, e.g. mail.example.test:587")
	}
	if (c.User == "") != (c.Pass == "") {
		return nil, errors.New("MEETINGS_SMTP_USER and MEETINGS_SMTP_PASS go together, or neither")
	}
	if !ValidAddress(c.From) {
		return nil, errors.New("MEETINGS_MAIL_FROM is one address, e.g. meetings@mail.example.test")
	}
	if c.ServerName == "" {
		c.ServerName = host
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	return &Sender{c: c}, nil
}

// From is the sender address.
func (s *Sender) From() string { return s.c.From }

// Host is the server's host:port (for the startup line; no credential).
func (s *Sender) Host() string { return s.c.Addr }

// code is a protocol error's reply code, 0 for another error.
func code(err error) int {
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code
	}
	return 0
}

// classify is a failure before the message was handed over.
func classify(err error, what string) error {
	c := code(err)
	if c >= 500 {
		return &Error{Class: Permanent, Code: c, Msg: what}
	}
	return &Error{Class: Temporary, Code: c, Msg: what}
}

// Send submits msg from the configured sender to one recipient.
func (s *Sender) Send(ctx context.Context, to string, msg []byte) error {
	ctx, cancel := context.WithTimeout(ctx, s.c.Timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", s.c.Addr)
	if err != nil {
		return &Error{Class: Temporary, Msg: "the mail server did not answer"}
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	host, _, _ := net.SplitHostPort(s.c.Addr)
	cl, err := smtp.NewClient(conn, host)
	if err != nil {
		return classify(err, "the mail server's greeting")
	}
	defer cl.Close()
	if err := cl.Hello(heloName(s.c.From)); err != nil {
		return classify(err, "EHLO")
	}
	if ok, _ := cl.Extension("STARTTLS"); !ok {
		return &Error{Class: Temporary, Msg: ErrNoStartTLS}
	}
	if err := cl.StartTLS(&tls.Config{ServerName: s.c.ServerName, RootCAs: s.c.RootCAs, MinVersion: tls.VersionTLS12}); err != nil {
		return classify(err, "STARTTLS")
	}
	if s.c.User != "" {
		if err := cl.Auth(smtp.PlainAuth("", s.c.User, s.c.Pass, s.c.ServerName)); err != nil {
			return classify(err, "AUTH")
		}
	}
	if err := cl.Mail(s.c.From); err != nil {
		return classify(err, "MAIL FROM")
	}
	if err := cl.Rcpt(to); err != nil {
		return classify(err, "RCPT TO")
	}
	wc, err := cl.Data()
	if err != nil {
		return classify(err, "DATA")
	}
	if _, err := wc.Write(msg); err != nil {
		// No closing dot has gone: the server discards a message it never
		// saw end.
		return &Error{Class: Temporary, Msg: "the connection broke while the message was sent"}
	}
	if err := wc.Close(); err != nil {
		if c := code(err); c >= 500 {
			return &Error{Class: Permanent, Code: c, Msg: "the server refused the message"}
		} else if c >= 400 {
			return &Error{Class: Temporary, Code: c, Msg: "the server deferred the message"}
		}
		return &Error{Class: Unknown, Msg: "the server did not answer after the message was sent"}
	}
	_ = cl.Quit()
	return nil
}

// heloName is the sender's domain, the name the client greets with.
func heloName(from string) string {
	if i := strings.LastIndexByte(from, '@'); i >= 0 && i < len(from)-1 {
		return from[i+1:]
	}
	return "localhost"
}

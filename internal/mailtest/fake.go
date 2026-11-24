// Package mailtest is a fake SMTP submission server for the plugin's tests
// and the e2e harness: STARTTLS with a certificate made at start (Pool
// trusts it), AUTH PLAIN, and every message it accepted, as received.
package mailtest

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// Message is one message the fake accepted.
type Message struct {
	From string
	To   []string
	Data []byte
	TLS  bool
	User string
}

// Fake is the fake server.
type Fake struct {
	ln   net.Listener
	cert tls.Certificate
	pool *x509.CertPool
	// NoTLS makes it not offer STARTTLS.
	mu    sync.Mutex
	noTLS bool
	got   []Message
	// replies maps a phase ("RCPT", "END") to the code the fake answers
	// there instead of 250; "END" with code 0 and hang closes the connection
	// without an answer.
	replies map[string]int
	hang    bool
	wait    chan struct{}
}

// New starts a fake on 127.0.0.1.
func New() (*Fake, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &Fake{ln: ln, replies: map[string]int{}, wait: make(chan struct{}, 64)}
	if err := f.mkCert(); err != nil {
		ln.Close()
		return nil, err
	}
	go f.accept()
	return f, nil
}

// Addr is host:port.
func (f *Fake) Addr() string { return f.ln.Addr().String() }

// Pool trusts the fake's certificate (for mail.Config.RootCAs).
func (f *Fake) Pool() *x509.CertPool { return f.pool }

// Close stops it.
func (f *Fake) Close() { f.ln.Close() }

// SetNoTLS makes the fake offer no STARTTLS.
func (f *Fake) SetNoTLS(v bool) { f.mu.Lock(); f.noTLS = v; f.mu.Unlock() }

// Reply makes the fake answer code at phase ("RCPT" or "END", the end of
// DATA); 0 restores 250.
func (f *Fake) Reply(phase string, code int) { f.mu.Lock(); f.replies[phase] = code; f.mu.Unlock() }

// Hang makes the fake close the connection after a message's end, without
// answering it (the message is still recorded: it was delivered).
func (f *Fake) Hang(v bool) { f.mu.Lock(); f.hang = v; f.mu.Unlock() }

// Messages is every message accepted so far.
func (f *Fake) Messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message{}, f.got...)
}

// Wait blocks until a message arrives or timeout passes; false on timeout.
func (f *Fake) Wait(timeout time.Duration) bool {
	select {
	case <-f.wait:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (f *Fake) mkCert() error {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake smtp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		return err
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	f.pool = x509.NewCertPool()
	f.pool.AddCert(c)
	f.cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
	return nil
}

func (f *Fake) accept() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serve(c)
	}
}

func (f *Fake) code(phase string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.replies[phase]; c != 0 {
		return c
	}
	return 250
}

func (f *Fake) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	r, w := bufio.NewReader(c), bufio.NewWriter(c)
	say := func(s string) { fmt.Fprintf(w, "%s\r\n", s); w.Flush() }
	say("220 fake.example.test ESMTP")
	var m Message
	secure := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			f.mu.Lock()
			noTLS := f.noTLS
			f.mu.Unlock()
			w.WriteString("250-fake.example.test\r\n")
			if !secure && !noTLS {
				w.WriteString("250-STARTTLS\r\n")
			}
			if secure {
				w.WriteString("250-AUTH PLAIN\r\n")
			}
			say("250 8BITMIME")
		case up == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{f.cert}})
			if tc.Handshake() != nil {
				return
			}
			c = tc
			r, w = bufio.NewReader(tc), bufio.NewWriter(tc)
			secure = true
		case strings.HasPrefix(up, "AUTH PLAIN"):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(cmd[len("AUTH PLAIN"):]))
			parts := bytes.Split(raw, []byte{0})
			if len(parts) == 3 {
				m.User = string(parts[1])
			}
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			m = Message{From: angle(cmd[len("MAIL FROM:"):]), TLS: secure, User: m.User}
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			if cd := f.code("RCPT"); cd != 250 {
				say(fmt.Sprintf("%d no such user", cd))
				continue
			}
			m.To = append(m.To, angle(cmd[len("RCPT TO:"):]))
			say("250 ok")
		case up == "DATA":
			say("354 end with .")
			var data bytes.Buffer
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(strings.TrimPrefix(l, "."))
			}
			m.Data = data.Bytes()
			f.mu.Lock()
			hang := f.hang
			f.mu.Unlock()
			cd := f.code("END")
			if cd == 250 {
				f.mu.Lock()
				f.got = append(f.got, m)
				f.mu.Unlock()
				select {
				case f.wait <- struct{}{}:
				default:
				}
			}
			if hang {
				return
			}
			say(fmt.Sprintf("%d %s", cd, map[bool]string{true: "queued", false: "refused"}[cd == 250]))
		case up == "QUIT":
			say("221 bye")
			return
		case up == "RSET", up == "NOOP":
			say("250 ok")
		default:
			say("502 unknown")
		}
	}
}

// angle is the address in a MAIL or RCPT argument's <…>.
func angle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '>'); strings.HasPrefix(s, "<") && i > 0 {
		return s[1:i]
	}
	return s
}

// Package egresstest is a fake LiveKit track Egress for tests and the e2e
// harness: a websocket client that dials an ingest URL with NO credential
// but the URL (as Egress does), writes binary frames and never reads, so it
// never answers a ping (as Egress does not, spike S2); and fixtures of
// Egress's PCM (1920-byte messages, 10 ms each, S16LE stereo 48 kHz, the two
// channels identical).
package egresstest

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// MessageBytes is one Egress message: 10 ms of S16LE stereo at 48 kHz.
const MessageBytes = 1920

// Client is one Egress stream.
type Client struct {
	c   net.Conn
	br  *bufio.Reader
	mu  sync.Mutex
	got []byte // every byte the server sent after the 101, if Watch ran
	gmu sync.Mutex
}

// ErrRefused is an upgrade the server answered with a status, not 101.
type ErrRefused struct{ Status int }

func (e *ErrRefused) Error() string {
	return fmt.Sprintf("egresstest: the upgrade was answered %d", e.Status)
}

// Dial opens a websocket to raw (ws://host:port/path). It sends no
// Authorization header, no cookie and no subprotocol: the URL is all.
func Dial(raw string) (*Client, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "ws" {
		return nil, errors.New("egresstest: want a ws:// URL")
	}
	c, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	rand.Read(key)
	req := "GET " + u.RequestURI() + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\nUser-Agent: egresstest\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		c.Close()
		return nil, &ErrRefused{Status: resp.StatusCode}
	}
	return &Client{c: c, br: br}, nil
}

// frame writes one masked client frame.
func (cl *Client) frame(op byte, payload []byte) error {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	hdr := []byte{0x80 | op}
	switch n := len(payload); {
	case n <= 125:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var key [4]byte
	rand.Read(key[:])
	hdr = append(hdr, key[:]...)
	p := make([]byte, len(payload))
	for i := range payload {
		p[i] = payload[i] ^ key[i%4]
	}
	_, err := cl.c.Write(append(hdr, p...))
	return err
}

// Send writes one binary message.
func (cl *Client) Send(msg []byte) error { return cl.frame(0x2, msg) }

// SendText writes one text message (Egress sends none).
func (cl *Client) SendText(s string) error { return cl.frame(0x1, []byte(s)) }

// Stream sends pcm in MessageBytes messages, one every pace (0: as fast as
// the connection takes them). It stops at the first error.
func (cl *Client) Stream(pcm []byte, pace time.Duration) error {
	for i := 0; i < len(pcm); i += MessageBytes {
		end := min(i+MessageBytes, len(pcm))
		if err := cl.Send(pcm[i:end]); err != nil {
			return err
		}
		if pace > 0 {
			time.Sleep(pace)
		}
	}
	return nil
}

// Watch records every byte the server sends from now on (pings, a close),
// in the background. Egress itself never reads: a test that wants to see
// what a server tried to say to such a client calls this.
func (cl *Client) Watch() {
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := cl.br.Read(buf)
			if n > 0 {
				cl.gmu.Lock()
				cl.got = append(cl.got, buf[:n]...)
				cl.gmu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
}

// Received is every byte Watch saw.
func (cl *Client) Received() []byte {
	cl.gmu.Lock()
	defer cl.gmu.Unlock()
	return append([]byte(nil), cl.got...)
}

// Close sends a close frame and closes the connection, as an egress that
// ends does.
func (cl *Client) Close() error {
	_ = cl.frame(0x8, binary.BigEndian.AppendUint16(nil, 1000))
	return cl.c.Close()
}

// Drop closes the TCP connection with no close frame (an egress that dies).
func (cl *Client) Drop() error { return cl.c.Close() }

// ---- fixtures ----

// Tone is ms of a sine at freq Hz and amplitude amp, as Egress sends it:
// S16LE stereo 48 kHz, the channels identical. phase0 is the sample index
// it starts at (so consecutive tones join without a click).
func Tone(ms int, freq float64, amp int16) []byte { return Stereo(Sine(ms, freq, amp)) }

// Silence is ms of digital silence, as Egress sends it.
func Silence(ms int) []byte { return make([]byte, ms*48*4) }

// Sine is ms of a sine at freq Hz and amplitude amp, mono 48 kHz.
func Sine(ms int, freq float64, amp int16) []int16 {
	n := ms * 48
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(float64(amp) * math.Sin(2*math.Pi*freq*float64(i)/48000))
	}
	return out
}

// Stereo interleaves mono 48 kHz samples into Egress's S16LE stereo with
// both channels equal.
func Stereo(mono []int16) []byte {
	b := make([]byte, 0, len(mono)*4)
	for _, s := range mono {
		b = binary.LittleEndian.AppendUint16(b, uint16(s))
		b = binary.LittleEndian.AppendUint16(b, uint16(s))
	}
	return b
}

// StereoLR interleaves two different channels (left, right).
func StereoLR(left, right []int16) []byte {
	b := make([]byte, 0, len(left)*4)
	for i := range left {
		b = binary.LittleEndian.AppendUint16(b, uint16(left[i]))
		b = binary.LittleEndian.AppendUint16(b, uint16(right[i]))
	}
	return b
}

// Speech is a stand-in for speech the plugin's endpointer hears: a 300 Hz
// tone at a speaking level, ms long.
func Speech(ms int) []byte { return Tone(ms, 300, 6000) }

// Script is a fixture of utterances: speech, then silence, for each of
// the given speech lengths (ms), with gap ms of silence after each.
func Script(gap int, speechMS ...int) []byte {
	var b []byte
	for _, s := range speechMS {
		b = append(b, Speech(s)...)
		b = append(b, Silence(gap)...)
	}
	return b
}

// HostOf is the host:port of a ws URL (for a test that wants to dial raw).
func HostOf(raw string) string {
	u, _ := url.Parse(raw)
	return strings.TrimSpace(u.Host)
}

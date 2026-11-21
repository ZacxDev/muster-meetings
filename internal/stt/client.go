package stt

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client calls the STT service muster's dictation uses (muster's
// docs/dictation.md and internal/stt): the OpenAI-style
// `POST /v1/audio/transcriptions`, a multipart body whose `file` part is the
// audio, `language` (en) and `punctuation` (true) fields, a `model` field
// only when one is configured, and bearer authentication; the answer is
// {"text": "…"}. It is muster's client's shape, so stt-cohere (or speaches,
// or a hosted API) answers the plugin exactly as it answers dictation.
//
// 🔴 The token is the plugin's copy of stt-cohere's one bearer (design:
// "Deployment": copied into the plugin's own Secret as MEETINGS_STT_TOKEN).
// No proxy and no redirects: every request carries it.
type Client struct {
	base, token, model string
	http               *http.Client
}

// TranscribePath is the service's endpoint.
const TranscribePath = "/v1/audio/transcriptions"

// The errors Transcribe answers besides a transcript, as muster's client
// names them.
var (
	ErrUndecodable = errors.New("stt: the service could not decode that audio")
	ErrRefused     = errors.New("stt: the service refused the plugin's token")
	ErrTooLong     = errors.New("stt: the audio is longer than the service accepts")
	ErrUnavailable = errors.New("stt: the service could not answer")
)

// NewClient builds a client for base (http(s)://host[:port], no path)
// presenting token; model "" sends no model field.
func NewClient(base, token, model string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("MEETINGS_STT_URL must be http(s)://host[:port] with no path, query, fragment or credentials")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("MEETINGS_STT_TOKEN is not set (the STT service's bearer; its value is not shown)")
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{base: u.Scheme + "://" + u.Host, token: strings.TrimSpace(token), model: strings.TrimSpace(model),
		http: &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, MaxIdleConns: 4, IdleConnTimeout: 90 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Endpoint is the URL Transcribe posts to (it carries no credential).
func (c *Client) Endpoint() string { return c.base + TranscribePath }

// Transcribe sends one utterance (16 kHz mono) as a WAV file and answers
// what the service heard.
func (c *Client) Transcribe(ctx context.Context, pcm []int16) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "utterance.wav")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(WAV(pcm)); err != nil {
		return "", err
	}
	mw.WriteField("language", "en")
	mw.WriteField("punctuation", "true")
	if c.model != "" {
		mw.WriteField("model", c.model)
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+TranscribePath, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrUnavailable, strings.ReplaceAll(err.Error(), c.token, "[redacted]"))
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest:
		return "", ErrUndecodable
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", ErrRefused
	case http.StatusRequestEntityTooLarge:
		return "", ErrTooLong
	default:
		return "", fmt.Errorf("%w (status %d)", ErrUnavailable, resp.StatusCode)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("%w: a 200 whose body is not a transcript", ErrUnavailable)
	}
	return out.Text, nil
}

// WAV is pcm (16 kHz mono, 16-bit) as a RIFF WAVE file.
func WAV(pcm []int16) []byte {
	data := len(pcm) * 2
	b := make([]byte, 0, 44+data)
	b = append(b, "RIFF"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(36+data))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1) // PCM
	b = binary.LittleEndian.AppendUint16(b, 1) // mono
	b = binary.LittleEndian.AppendUint32(b, Rate)
	b = binary.LittleEndian.AppendUint32(b, Rate*2)
	b = binary.LittleEndian.AppendUint16(b, 2)
	b = binary.LittleEndian.AppendUint16(b, 16)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(data))
	for _, s := range pcm {
		b = binary.LittleEndian.AppendUint16(b, uint16(s))
	}
	return b
}

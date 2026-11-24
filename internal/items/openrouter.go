package items

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ChatPath is OpenRouter's chat completions route, under BaseURL.
const ChatPath = "/chat/completions"

// maxAnswer bounds an answer's body.
const maxAnswer = 2 << 20

// client is the one OpenRouter client both adapters and the evaluation use,
// so the evaluation measures production's requests exactly.
type client struct {
	model, key, base string
	prefs            map[string]any
	http             *http.Client
}

func newClient(c Config, hc *http.Client) *client {
	if hc == nil {
		t := c.Timeout
		if t <= 0 {
			t = 30 * time.Second
		}
		hc = &http.Client{Timeout: t}
	}
	// 🔴 No redirects: the bearer goes to BaseURL and nowhere else.
	h := *hc
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return &client{model: c.Model, key: c.APIKey, base: base, prefs: c.Prefs, http: &h}
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// request is the body both adapters send. Field order is the JSON's order,
// and the provider object is a map (encoding/json sorts its keys), so the
// same call is the same bytes.
type request struct {
	Model          string         `json:"model"`
	Messages       []message      `json:"messages"`
	Provider       map[string]any `json:"provider"`
	ResponseFormat any            `json:"response_format,omitempty"`
	Temperature    *float64       `json:"temperature,omitempty"`
	MaxTokens      int            `json:"max_tokens,omitempty"`
	// Usage asks OpenRouter to report the call's cost (usage accounting).
	Usage struct {
		Include bool `json:"include"`
	} `json:"usage"`
}

type answer struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Choices  []struct {
		Message struct {
			Content *string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int      `json:"prompt_tokens"`
		CompletionTokens int      `json:"completion_tokens"`
		Cost             *float64 `json:"cost"`
	} `json:"usage"`
	Error *struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	} `json:"error"`
}

// DetailError carries OpenRouter's own message for a failure. It is kept out
// of Error() (an upstream message may quote the input, and transcript text
// never reaches a log); the evaluation, run by hand on a synthetic corpus,
// prints it.
type DetailError struct {
	Err    *Error
	Detail string
}

func (e *DetailError) Error() string { return e.Err.Error() }
func (e *DetailError) Unwrap() error { return e.Err }

// complete sends body and answers the first choice's content and the
// metadata.
func (c *client) complete(ctx context.Context, body request) (string, Result, error) {
	body.Model = c.model
	body.Usage.Include = true
	b, err := json.Marshal(body)
	if err != nil {
		return "", Result{}, &Error{Class: Fatal, Msg: "encoding the request: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+ChatPath, bytes.NewReader(b))
	if err != nil {
		return "", Result{}, &Error{Class: Fatal, Msg: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// A timeout, a refused or cut connection, or a cancelled context.
		return "", Result{}, &Error{Class: Retry, Msg: "openrouter did not answer: " + redactURL(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return "", Result{}, &Error{Class: Retry, Status: resp.StatusCode, Msg: "reading the answer: " + err.Error()}
	}
	if len(raw) > maxAnswer {
		return "", Result{}, &Error{Class: Schema, Status: resp.StatusCode, Msg: "the answer is over 2 MiB"}
	}
	var a answer
	jerr := json.Unmarshal(raw, &a)
	if resp.StatusCode != http.StatusOK {
		e := &Error{Class: classify(resp.StatusCode), Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
			Msg: "openrouter refused the request"}
		if jerr == nil && a.Error != nil {
			return "", Result{}, &DetailError{e, a.Error.Message}
		}
		return "", Result{}, e
	}
	if jerr != nil {
		return "", Result{}, &Error{Class: Schema, Status: 200, Msg: "the answer is not JSON"}
	}
	// OpenRouter can answer 200 with an error object (an upstream failure
	// after the stream began): classified by the code it carries.
	if a.Error != nil {
		code, _ := strconv.Atoi(strings.Trim(string(a.Error.Code), `"`))
		e := &Error{Class: classify(code), Status: code, Msg: "openrouter answered an error"}
		return "", Result{}, &DetailError{e, a.Error.Message}
	}
	if len(a.Choices) == 0 || a.Choices[0].Message.Content == nil {
		return "", Result{}, &Error{Class: Schema, Status: 200, Msg: "the answer has no message content"}
	}
	res := Result{Provider: a.Provider, Model: a.Model}
	if a.Usage != nil {
		res.Usage = Usage{PromptTokens: a.Usage.PromptTokens, CompletionTokens: a.Usage.CompletionTokens}
		if a.Usage.Cost != nil {
			res.Usage.CostUSD, res.Usage.CostKnown = *a.Usage.Cost, true
		}
	}
	return *a.Choices[0].Message.Content, res, nil
}

// classify is a status's class: 408, 429 and 5xx are retried (and 0, an
// error object with no usable code, is treated as an upstream failure);
// everything else is the configuration's fault.
func classify(status int) Class {
	switch {
	case status == http.StatusTooManyRequests, status == http.StatusRequestTimeout, status >= 500, status == 0:
		return Retry
	}
	return Fatal
}

// retryAfter reads a Retry-After of whole seconds (an HTTP date is ignored).
func retryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// redactURL is a transport error without the URL (which never carries a
// secret here, but a log line need not repeat it).
func redactURL(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if in := ue.Unwrap(); in != nil {
			return in.Error()
		}
	}
	return fmt.Sprint(err)
}

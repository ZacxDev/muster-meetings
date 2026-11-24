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
	// pin is true for a chat_json model: every request must still pin its
	// provider when it is sent.
	pin bool
	// maxTokens is Config.MaxTokens: 0 is the adapter's own default.
	maxTokens int
	http      *http.Client
}

// tokens is the max_tokens a request sends: the configured bound, else def.
func (c *client) tokens(def int) int {
	if c.maxTokens > 0 {
		return c.maxTokens
	}
	return def
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
	return &client{model: c.Model, key: c.APIKey, base: base, prefs: c.Prefs, pin: c.Adapter == AdapterChatJSON, maxTokens: c.MaxTokens, http: &h}
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
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens            int      `json:"prompt_tokens"`
		CompletionTokens        int      `json:"completion_tokens"`
		Cost                    *float64 `json:"cost"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
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

// sendPrefs is a request's own copy of the provider object.
func (c *client) sendPrefs() (map[string]any, error) {
	p, err := copyPrefs(c.prefs)
	if err != nil {
		return nil, &Error{Class: Fatal, Msg: err.Error()}
	}
	return p, nil
}

// complete sends body and answers the first choice's content and the
// metadata.
//
// 🔴 The provider object is checked HERE, on the bytes about to be sent,
// whatever built it: a request without data_collection "deny" (or, for a
// chat_json model, without its provider pin) is refused and never leaves.
// ParsePrefs checked the configuration once; nothing between it and the
// wire may drop either.
func (c *client) complete(ctx context.Context, body request) (string, Result, error) {
	if body.Provider == nil || body.Provider["data_collection"] != "deny" {
		return "", Result{}, &Error{Class: Fatal, Msg: `refused to send: the provider object has no data_collection "deny"`}
	}
	if c.pin && !pinned(body.Provider) {
		return "", Result{}, &Error{Class: Fatal, Msg: "refused to send: a chat_json request must pin its provider"}
	}
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
	// 🔴 What OpenRouter says it ran and charged is kept on EVERY answer from
	// here on, failed or not: a schema failure is still a billed call, and
	// dropping its usage is what read the first run's cost as "unknown".
	res := Result{Provider: a.Provider, Model: a.Model}
	if a.Usage != nil {
		res.Usage = Usage{PromptTokens: a.Usage.PromptTokens, CompletionTokens: a.Usage.CompletionTokens}
		if a.Usage.Cost != nil {
			res.Usage.CostUSD, res.Usage.CostKnown = *a.Usage.Cost, true
		}
		if d := a.Usage.CompletionTokensDetails; d != nil {
			res.Usage.ReasoningTokens = d.ReasoningTokens
		}
	}
	if len(a.Choices) > 0 {
		res.FinishReason = a.Choices[0].FinishReason
	}
	// OpenRouter can answer 200 with an error object (an upstream failure
	// after the stream began): classified by the code it carries.
	if a.Error != nil {
		code, _ := strconv.Atoi(strings.Trim(string(a.Error.Code), `"`))
		e := &Error{Class: classify(code), Status: code, Msg: "openrouter answered an error"}
		return "", res, &DetailError{e, a.Error.Message}
	}
	// An answer cut off at max_tokens is never parsed, whatever its content
	// (empty, when a reasoning model spent the budget thinking; a prefix of
	// the JSON otherwise): it says so, with the tokens, rather than pass as
	// a malformed answer.
	if res.FinishReason == "length" {
		return "", res, &Error{Class: Schema, Status: 200, Msg: fmt.Sprintf("the answer was cut off at max_tokens (finish_reason length: %d completion tokens, %d of them reasoning)",
			res.Usage.CompletionTokens, res.Usage.ReasoningTokens)}
	}
	if len(a.Choices) == 0 || a.Choices[0].Message.Content == nil {
		return "", res, &Error{Class: Schema, Status: 200, Msg: fmt.Sprintf("the answer has no message content (finish_reason %q)", res.FinishReason)}
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

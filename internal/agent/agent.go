// Package agent calls a Model for an agent's reply, streaming it as it is
// generated (docs/adr/0015-agent-profiles-handles-and-agent-chains.md). It
// speaks the Anthropic Messages and OpenAI Chat Completions streaming APIs,
// text only, and retries what a provider says is worth retrying.
package agent

import (
	"bufio"
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

	"defios.com/internal/model"
)

// anthropicVersion is the Messages API version this package speaks.
const anthropicVersion = "2023-06-01"

// maxTokens bounds a reply. The Messages API requires a bound; a reply must
// also fit an app.freeform, so a larger one would be refused at commit.
const maxTokens = 8192

// Request is one reply to generate.
type Request struct {
	Provider string // model.Anthropic or model.OpenAI
	URL      string // the whole endpoint: base URL and API path
	APIKey   string // empty sends none
	Model    string // the provider's model id
	System   string
	Turns    []Turn // the talk so far, alternating and ending with User
}

// Turn is one message of the talk as the Model sees it (ADR 0018): the
// agent's own message is an Assistant turn, everything else User content.
type Turn struct {
	Role string // User or Assistant
	Text string
}

// The roles a Turn takes; both providers name them alike.
const (
	User      = "user"
	Assistant = "assistant"
)

// Events receive a reply as it is generated. Either may be nil.
type Events struct {
	Delta func(text string)
	// Reset means a retry starts the reply over: forget the deltas so far.
	Reset func()
}

// Retries is how many times a retryable failure is tried again (ADR 0015).
const Retries = 3

// maxRetryAfter bounds how long a provider's Retry-After may hold a chain.
const maxRetryAfter = 2 * time.Minute

// Client generates replies.
type Client struct {
	// HTTP is the client to call providers with; nil is a default one.
	HTTP *http.Client
	// Backoff is the wait before the first retry, doubling after each; zero
	// is a second. A provider's Retry-After takes its place.
	Backoff time.Duration
}

// Generate streams req's reply to ev and returns it whole. A rate limit,
// overload, 5xx response or dropped connection is retried up to Retries
// times; a retry after the reply began streaming resets it. Any other
// failure, or the last retryable one, is returned.
func (c Client) Generate(ctx context.Context, req Request, ev Events) (string, error) {
	wait := c.Backoff
	if wait == 0 {
		wait = time.Second
	}
	for n := 0; ; n++ {
		started := false
		text, err := c.attempt(ctx, req, Events{
			Delta: func(s string) {
				started = true
				if ev.Delta != nil {
					ev.Delta(s)
				}
			},
		})
		if err == nil {
			return text, nil
		}
		var retry *retryable
		if ctx.Err() != nil || !errors.As(err, &retry) || n == Retries {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", err
		}
		if started && ev.Reset != nil {
			ev.Reset()
		}
		pause := wait
		if retry.after >= 0 {
			pause = min(retry.after, maxRetryAfter)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pause):
		}
		wait *= 2
	}
}

// retryable is a failure worth trying again: after, when not negative, is
// how long the provider asked to wait.
type retryable struct {
	err   error
	after time.Duration
}

func (e *retryable) Error() string { return e.err.Error() }
func (e *retryable) Unwrap() error { return e.err }

func retry(err error) error { return &retryable{err: err, after: -1} }

// retryStatus is whether a provider's HTTP status is worth retrying: a rate
// limit, Anthropic's overload (529) or any 5xx.
func retryStatus(code int) bool { return code == http.StatusTooManyRequests || code >= 500 }

// retryAfter reads a Retry-After header in seconds; -1 when there is none.
func retryAfter(h http.Header) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || secs < 0 {
		return -1
	}
	return time.Duration(secs) * time.Second
}

// retryEvents are the Anthropic stream error types worth retrying.
var retryEvents = map[string]bool{"overloaded_error": true, "rate_limit_error": true, "api_error": true}

func (c Client) attempt(ctx context.Context, req Request, ev Events) (string, error) {
	body, err := requestBody(req)
	if err != nil {
		return "", err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("agent: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	switch req.Provider {
	case model.Anthropic:
		hreq.Header.Set("anthropic-version", anthropicVersion)
		if req.APIKey != "" {
			hreq.Header.Set("x-api-key", req.APIKey)
		}
	case model.OpenAI:
		if req.APIKey != "" {
			hreq.Header.Set("Authorization", "Bearer "+req.APIKey)
		}
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return "", retry(fmt.Errorf("agent: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		err := fmt.Errorf("agent: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
		if retryStatus(resp.StatusCode) {
			return "", &retryable{err: err, after: retryAfter(resp.Header)}
		}
		return "", err
	}
	var reply strings.Builder
	err = readEvents(resp.Body, func(data string) (bool, error) {
		text, done, err := parseEvent(req.Provider, data)
		if err != nil || done {
			return done, err
		}
		if text != "" {
			reply.WriteString(text)
			if ev.Delta != nil {
				ev.Delta(text)
			}
		}
		return false, nil
	})
	if err != nil {
		return "", err
	}
	return reply.String(), nil
}

func requestBody(req Request) ([]byte, error) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	turns := make([]message, len(req.Turns))
	for i, t := range req.Turns {
		turns[i] = message{t.Role, t.Text}
	}
	switch req.Provider {
	case model.Anthropic:
		return json.Marshal(struct {
			Model     string    `json:"model"`
			MaxTokens int       `json:"max_tokens"`
			System    string    `json:"system,omitempty"`
			Messages  []message `json:"messages"`
			Stream    bool      `json:"stream"`
		}{req.Model, maxTokens, req.System, turns, true})
	case model.OpenAI:
		var msgs []message
		if req.System != "" {
			msgs = append(msgs, message{"system", req.System})
		}
		return json.Marshal(struct {
			Model    string    `json:"model"`
			Messages []message `json:"messages"`
			Stream   bool      `json:"stream"`
		}{req.Model, append(msgs, turns...), true})
	}
	return nil, fmt.Errorf("agent: provider %q must be anthropic or openai", req.Provider)
}

// readEvents hands each server-sent event's data to fn until fn says the
// reply is done. A stream that ends before then was cut off.
func readEvents(r io.Reader, fn func(data string) (done bool, err error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line != "" {
			if v, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(v, " "))
			}
			continue
		}
		if len(data) == 0 {
			continue
		}
		done, err := fn(strings.Join(data, "\n"))
		data = data[:0]
		if err != nil || done {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return retry(fmt.Errorf("agent: reading the reply: %w", err))
	}
	return retry(errors.New("agent: the reply was cut off"))
}

// parseEvent is one event's text, or done at the end of the reply.
func parseEvent(provider, data string) (text string, done bool, err error) {
	switch provider {
	case model.Anthropic:
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return "", false, fmt.Errorf("agent: bad event: %w", err)
		}
		switch ev.Type {
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" {
				return ev.Delta.Text, false, nil
			}
		case "message_stop":
			return "", true, nil
		case "error":
			err := fmt.Errorf("agent: %s: %s", ev.Error.Type, ev.Error.Message)
			if retryEvents[ev.Error.Type] {
				err = retry(err)
			}
			return "", false, err
		}
		return "", false, nil
	}
	if data == "[DONE]" {
		return "", true, nil
	}
	var ev struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return "", false, fmt.Errorf("agent: bad event: %w", err)
	}
	for _, c := range ev.Choices {
		text += c.Delta.Content
	}
	return text, false, nil
}

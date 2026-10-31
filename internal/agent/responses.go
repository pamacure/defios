package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// responsesParser reads a Responses stream (ADR 0019). Each output item is
// a block: a message is text, a reasoning item thinking, a function call a
// tool_use whose ID is its call_id. The item as it came back when done is
// kept on the block, to be replayed on the run's next call.
type responsesParser struct {
	st      *stream
	blocks  []Block
	args    []strings.Builder // function call arguments as they stream, by index
	started []bool            // the item has streamed
	out     call
}

// responsesItem is the part of an output item the reader needs.
type responsesItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

// responsesError is how a Responses stream says it failed.
type responsesError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// err is e as an error, retryable when the API's code says it is worth it.
func (e responsesError) err() error {
	err := fmt.Errorf("agent: %s: %s", e.Code, e.Message)
	if e.Code == "server_error" || e.Code == "rate_limit_exceeded" {
		return retry(err)
	}
	return err
}

func (p *responsesParser) event(data string) (bool, error) {
	var ev struct {
		Type        string          `json:"type"`
		OutputIndex int             `json:"output_index"`
		Item        json.RawMessage `json:"item"`
		Delta       string          `json:"delta"`
		Response    struct {
			Status            string          `json:"status"`
			Usage             map[string]any  `json:"usage"`
			Error             *responsesError `json:"error"`
			IncompleteDetails struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
		responsesError
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return false, fmt.Errorf("agent: bad event: %w", err)
	}
	switch ev.Type {
	case "response.output_item.added":
		var it responsesItem
		if err := json.Unmarshal(ev.Item, &it); err != nil {
			return false, fmt.Errorf("agent: bad event: %w", err)
		}
		b, err := p.added(ev.OutputIndex)
		if err != nil {
			return false, err
		}
		switch it.Type {
		case "message":
			*b = Block{Type: "text"}
		case "reasoning":
			*b = Block{Type: "reasoning"}
		case "function_call":
			*b = Block{Type: "tool_use", ID: it.CallID, Name: it.Name}
			p.started[ev.OutputIndex] = true
			p.st.block(KindTool, it.Name+" ")
		default:
			*b = Block{Type: it.Type}
		}
	case "response.output_text.delta", "response.refusal.delta":
		b, err := p.existing(ev.OutputIndex)
		if err != nil {
			return false, err
		}
		b.Text += ev.Delta
		p.stream(ev.OutputIndex, KindText, ev.Delta)
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		b, err := p.existing(ev.OutputIndex)
		if err != nil {
			return false, err
		}
		b.Thinking += ev.Delta
		p.stream(ev.OutputIndex, KindThinking, ev.Delta)
	case "response.function_call_arguments.delta":
		if _, err := p.existing(ev.OutputIndex); err != nil {
			return false, err
		}
		p.args[ev.OutputIndex].WriteString(ev.Delta)
		p.st.more(KindTool, ev.Delta)
	case "response.output_item.done":
		if err := p.done(ev.OutputIndex, ev.Item); err != nil {
			return false, err
		}
	case "response.completed":
		p.out.usage = ev.Response.Usage
		return true, nil
	case "response.incomplete":
		p.out.usage = ev.Response.Usage
		p.markIncomplete(ev.Response.IncompleteDetails.Reason)
		return true, nil
	case "response.failed":
		if e := ev.Response.Error; e != nil {
			return false, e.err()
		}
		return false, fmt.Errorf("agent: the response failed")
	case "error":
		return false, ev.responsesError.err()
	}
	return false, nil
}

// markIncomplete marks the reply incomplete for reason; at the token bound it
// is also truncated. Either way it is an error.
func (p *responsesParser) markIncomplete(reason string) {
	p.out.stop, p.out.incomplete = reason, true
	p.out.truncated = reason == "max_output_tokens"
}

// added is the block of output item index, made room for if it is new.
func (p *responsesParser) added(index int) (*Block, error) {
	if index < 0 || index > 1024 {
		return nil, fmt.Errorf("agent: bad event: output index %d", index)
	}
	for len(p.blocks) <= index {
		p.blocks = append(p.blocks, Block{})
		p.args = append(p.args, strings.Builder{})
		p.started = append(p.started, false)
	}
	return &p.blocks[index], nil
}

// existing is the block of output item index, which must have been added.
func (p *responsesParser) existing(index int) (*Block, error) {
	if index < 0 || index >= len(p.blocks) {
		return nil, fmt.Errorf("agent: bad event: delta for output item %d before it was added", index)
	}
	return &p.blocks[index], nil
}

// stream streams text of item index, which starts a block the first time.
func (p *responsesParser) stream(index int, kind, text string) {
	if text == "" {
		return
	}
	if p.started[index] {
		p.st.more(kind, text)
		return
	}
	p.started[index] = true
	p.st.block(kind, text)
}

// done takes item index as it came back whole: its fields over what
// streamed, and the item itself to replay.
func (p *responsesParser) done(index int, raw json.RawMessage) error {
	var it responsesItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return fmt.Errorf("agent: bad event: %w", err)
	}
	b, err := p.added(index)
	if err != nil {
		return err
	}
	b.Item = raw
	switch it.Type {
	case "message":
		var text strings.Builder
		for _, c := range it.Content {
			text.WriteString(c.Text + c.Refusal)
		}
		b.Type, b.Text = "text", text.String()
	case "reasoning":
		var thinking []string
		for _, s := range it.Summary {
			thinking = append(thinking, s.Text)
		}
		b.Type = "reasoning"
		if len(thinking) > 0 {
			b.Thinking = strings.Join(thinking, "\n\n")
		}
	case "function_call":
		b.Type, b.ID, b.Name = "tool_use", it.CallID, it.Name
		p.args[index].Reset()
		p.args[index].WriteString(it.Arguments)
	}
	return nil
}

// reply reads a Responses reply that is not streamed: the Response object,
// each output item taken as the stream takes it when done.
func (p *responsesParser) reply(body []byte) error {
	var r struct {
		Status            string            `json:"status"`
		Output            []json.RawMessage `json:"output"`
		Usage             map[string]any    `json:"usage"`
		Error             *responsesError   `json:"error"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("agent: bad reply: %w", err)
	}
	if r.Status == "failed" {
		if r.Error != nil {
			return r.Error.err()
		}
		return fmt.Errorf("agent: the response failed")
	}
	for i, raw := range r.Output {
		if err := p.done(i, raw); err != nil {
			return err
		}
		switch b := p.blocks[i]; b.Type {
		case "text":
			p.stream(i, KindText, b.Text)
		case "reasoning":
			p.stream(i, KindThinking, b.Thinking)
		case "tool_use":
			p.st.block(KindTool, b.Name+" ")
			p.st.more(KindTool, p.args[i].String())
		}
	}
	p.out.usage = r.Usage
	if r.Status == "incomplete" {
		p.markIncomplete(r.IncompleteDetails.Reason)
	}
	return nil
}

func (p *responsesParser) result() call {
	for i := range p.blocks {
		if p.blocks[i].Type == "tool_use" {
			p.blocks[i].Input = toolInput(p.args[i].String())
		}
	}
	p.out.blocks = p.blocks
	return p.out
}

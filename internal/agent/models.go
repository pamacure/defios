package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Model is one model a Provider lists: its Model ID, and the name the list
// gives it, "" when it gives none.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// ModelsPath is where a Provider whose api_path is apiPath lists its
// models: apiPath with s's suffix replaced by /models. Every shape lists
// them there.
func (s Shape) ModelsPath(apiPath string) string {
	return strings.TrimSuffix(apiPath, s.Suffix) + "/models"
}

// maxModelPages bounds how many pages one list may take, so a list that
// always says it has more cannot go on forever.
const maxModelPages = 50

// maxModelList bounds one page of the list.
const maxModelList = 4 << 20

// Models lists the models at url, a Provider's ModelsPath, with key sent as
// s sends it, sorted by ID. A list in pages (Messages') is followed to its
// end. A refusal is an error carrying the API's status and message.
func (c Client) Models(ctx context.Context, s Shape, url, key string) ([]Model, error) {
	var out []Model
	after := ""
	for range maxModelPages {
		page, err := c.modelPage(ctx, s, url, key, after)
		if err != nil {
			return nil, err
		}
		for _, m := range page.Data {
			if m.ID != "" {
				out = append(out, Model{ID: m.ID, Name: m.DisplayName})
			}
		}
		if !page.HasMore || page.LastID == "" || page.LastID == after {
			break
		}
		after = page.LastID
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// modelList is one page of a model list, in either shape's fields.
type modelList struct {
	Data []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	LastID  string `json:"last_id"`
}

func (c Client) modelPage(ctx context.Context, s Shape, at, key, after string) (modelList, error) {
	u, err := url.Parse(at)
	if err != nil {
		return modelList{}, fmt.Errorf("agent: listing models: %w", err)
	}
	if after != "" {
		q := u.Query()
		q.Set("after_id", after)
		u.RawQuery = q.Encode()
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return modelList{}, fmt.Errorf("agent: listing models: %w", err)
	}
	hreq.Header.Set("Accept", "application/json")
	s.auth.set(hreq.Header, key)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return modelList{}, fmt.Errorf("agent: listing models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return modelList{}, fmt.Errorf("agent: listing models: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var page modelList
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelList)).Decode(&page); err != nil {
		return modelList{}, fmt.Errorf("agent: listing models: the reply is no model list: %w", err)
	}
	return page, nil
}

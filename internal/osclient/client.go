// Package osclient is a minimal OpenSearch HTTP client: just the endpoints
// the catalog and the engine need, with no third-party dependencies.
package osclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	URL  string
	HTTP *http.Client
}

func New(baseURL string) *Client {
	return &Client{URL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 120 * time.Second}}
}

// Error carries OpenSearch's error body.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string { return fmt.Sprintf("opensearch %d: %s", e.Status, truncate(e.Body, 400)) }

func (c *Client) Do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return &Error{resp.StatusCode, string(data)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Index is one row of _cat/indices.
type Index struct {
	Name      string `json:"index"`
	DocsCount string `json:"docs.count"`
	StoreSize string `json:"store.size"`
}

func (c *Client) CatIndices(ctx context.Context, pattern string) ([]Index, error) {
	var out []Index
	err := c.Do(ctx, http.MethodGet, "/_cat/indices/"+url.PathEscape(pattern)+"?format=json&h=index,docs.count,store.size&bytes=b&expand_wildcards=open", nil, &out)
	if e, ok := err.(*Error); ok && e.Status == 404 {
		return nil, nil
	}
	return out, err
}

// FieldCap is the per-type capability record returned by _field_caps.
type FieldCap struct {
	Type         string `json:"type"`
	Searchable   bool   `json:"searchable"`
	Aggregatable bool   `json:"aggregatable"`
}

func (c *Client) FieldCaps(ctx context.Context, pattern string) (map[string]map[string]FieldCap, error) {
	var out struct {
		Fields map[string]map[string]FieldCap `json:"fields"`
	}
	err := c.Do(ctx, http.MethodGet, "/"+url.PathEscape(pattern)+"/_field_caps?fields=*", nil, &out)
	return out.Fields, err
}

// SearchResponse is the subset of a _search response we use.
type SearchResponse struct {
	Took int `json:"took"`
	Hits struct {
		Total struct {
			Value    int64  `json:"value"`
			Relation string `json:"relation"`
		} `json:"total"`
		Hits []struct {
			ID     string          `json:"_id"`
			Index  string          `json:"_index"`
			Source json.RawMessage `json:"_source"`
			Sort   []any           `json:"sort"`
		} `json:"hits"`
	} `json:"hits"`
	Aggregations json.RawMessage `json:"aggregations"`
}

func (c *Client) Search(ctx context.Context, indices []string, body any) (*SearchResponse, error) {
	var out SearchResponse
	path := "/" + url.PathEscape(strings.Join(indices, ",")) + "/_search?ignore_unavailable=true&allow_no_indices=true"
	err := c.Do(ctx, http.MethodPost, path, body, &out)
	return &out, err
}

func (c *Client) Count(ctx context.Context, indices []string, query any) (int64, error) {
	var out struct {
		Count int64 `json:"count"`
	}
	body := map[string]any{}
	if query != nil {
		body["query"] = query
	}
	path := "/" + url.PathEscape(strings.Join(indices, ",")) + "/_count?ignore_unavailable=true&allow_no_indices=true"
	err := c.Do(ctx, http.MethodPost, path, body, &out)
	return out.Count, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

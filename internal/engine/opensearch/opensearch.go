// Package opensearch runs slices against OpenSearch daily indices, the
// stand-in for a SIEM's hot tier. Rows page with search_after; grouped
// aggregates page with a composite aggregation.
package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/engine"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/osclient"
	"github.com/rksurwase/fedsearch/internal/planner"
	"github.com/rksurwase/fedsearch/internal/result"
)

type Adapter struct {
	clients map[string]*osclient.Client // by source name
}

func New(cfg *config.Config) *Adapter {
	a := &Adapter{clients: map[string]*osclient.Client{}}
	for _, s := range cfg.Sources {
		if s.Kind == "opensearch" {
			a.clients[s.Name] = osclient.New(s.URL)
		}
	}
	return a
}

func (a *Adapter) Kind() string { return "opensearch" }

func (a *Adapter) client(s *planner.Slice) (*osclient.Client, error) {
	c := a.clients[s.Source]
	if c == nil {
		return nil, fmt.Errorf("no opensearch client for source %s", s.Source)
	}
	return c, nil
}

// Estimate asks the cluster how many documents match (cheap: _count), then
// prices them at the location's average document size.
func (a *Adapter) Estimate(ctx context.Context, s *planner.Slice, q *ir.Query) (planner.Estimate, error) {
	c, err := a.client(s)
	if err != nil {
		return planner.Estimate{}, err
	}
	query := s.DSL.Body["query"]
	n, err := c.Count(ctx, s.Indices, query)
	if err != nil {
		var rows, bytes int64
		for _, p := range s.Parts {
			rows += p.Rows
			bytes += p.Bytes
		}
		return planner.Estimate{Rows: rows, Bytes: bytes, Method: "index sizes (_count failed: " + err.Error() + ")"}, nil
	}
	return planner.Estimate{Rows: n, Bytes: int64(float64(n) * s.Loc.AvgRowSize), Method: fmt.Sprintf("_count = %d docs × %.0f B avg doc", n, s.Loc.AvgRowSize)}, nil
}

func (a *Adapter) Produce(ctx context.Context, s *planner.Slice, q *ir.Query, emit func(engine.Page) error) error {
	c, err := a.client(s)
	if err != nil {
		return err
	}
	if s.DSL == nil {
		return fmt.Errorf("%s: slice not compiled to DSL", s.ID)
	}
	if q.IsAggregate() {
		return a.aggregate(ctx, c, s, q, emit)
	}
	return a.rows(ctx, c, s, emit)
}

func (a *Adapter) rows(ctx context.Context, c *osclient.Client, s *planner.Slice, emit func(engine.Page) error) error {
	body := cloneBody(s.DSL.Body)
	idPhys := s.Loc.Bindings["metadata.uid"].Physical
	fetched := 0
	var matched int64
	for fetched < s.EngineLimit {
		size := min(s.DSL.PageSize, s.EngineLimit-fetched)
		body["size"] = size
		resp, err := c.Search(ctx, s.Indices, body)
		if err != nil {
			return err
		}
		page := engine.Page{}
		var last []any
		for _, h := range resp.Hits.Hits {
			var src map[string]any
			if err := json.Unmarshal(h.Source, &src); err != nil {
				return err
			}
			r := result.Row{Location: s.Location, Slice: s.ID, Fields: map[string]any{}, ID: fmt.Sprint(src[idPhys])}
			r.Time = parseTime(src["time"])
			for _, path := range s.DSL.Columns {
				b := s.Loc.Bindings[path]
				r.Fields[path] = b.FromPhysical(normalize(src[b.Physical]))
			}
			page.Rows = append(page.Rows, r)
			last = h.Sort
		}
		fetched += len(page.Rows)
		matched += int64(len(page.Rows))
		page.Bytes = int64(float64(matched) * s.Loc.AvgRowSize)
		done := len(resp.Hits.Hits) < size || fetched >= s.EngineLimit
		page.Done = done
		if err := emit(page); err != nil {
			return err
		}
		if done {
			return nil
		}
		body["search_after"] = last
	}
	return nil
}

func (a *Adapter) aggregate(ctx context.Context, c *osclient.Client, s *planner.Slice, q *ir.Query, emit func(engine.Page) error) error {
	body := cloneBody(s.DSL.Body)
	page := engine.Page{Done: true}
	if !s.DSL.Composite {
		resp, err := c.Search(ctx, s.Indices, body)
		if err != nil {
			return err
		}
		var aggs map[string]json.RawMessage
		_ = json.Unmarshal(resp.Aggregations, &aggs)
		g := &result.Group{States: states(q, s, aggs, resp.Hits.Total.Value)}
		page.Groups = []*result.Group{g}
		page.Bytes = int64(float64(resp.Hits.Total.Value) * s.Loc.AvgRowSize)
		return emit(page)
	}
	var total int64
	for {
		resp, err := c.Search(ctx, s.Indices, body)
		if err != nil {
			return err
		}
		total = resp.Hits.Total.Value
		var aggs struct {
			Groups struct {
				AfterKey map[string]any               `json:"after_key"`
				Buckets  []map[string]json.RawMessage `json:"buckets"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(resp.Aggregations, &aggs); err != nil {
			return err
		}
		for _, b := range aggs.Groups.Buckets {
			var key map[string]any
			var docCount int64
			_ = json.Unmarshal(b["key"], &key)
			_ = json.Unmarshal(b["doc_count"], &docCount)
			g := &result.Group{}
			for i, path := range s.DSL.GroupCols {
				v := normalize(key[fmt.Sprintf("k%d", i)])
				g.Key = append(g.Key, s.Loc.Bindings[path].FromPhysical(v))
			}
			g.States = states(q, s, b, docCount)
			page.Groups = append(page.Groups, g)
		}
		if len(page.Groups) >= s.EngineLimit {
			page.Capped = true
			page.Groups = page.Groups[:s.EngineLimit]
			break
		}
		if aggs.Groups.AfterKey == nil || len(aggs.Groups.Buckets) == 0 {
			break
		}
		comp := body["aggs"].(map[string]any)["groups"].(map[string]any)["composite"].(map[string]any)
		comp["after"] = aggs.Groups.AfterKey
	}
	page.Bytes = int64(float64(total) * s.Loc.AvgRowSize)
	return emit(page)
}

// states decodes the partial aggregates of one bucket (or the top level).
func states(q *ir.Query, s *planner.Slice, aggs map[string]json.RawMessage, docCount int64) []*result.State {
	val := func(name string) (float64, bool) {
		var v struct{ Value *float64 }
		if raw, ok := aggs[name]; ok && json.Unmarshal(raw, &v) == nil && v.Value != nil {
			return *v.Value, true
		}
		return 0, false
	}
	var out []*result.State
	for i, a := range q.Aggs {
		ac := s.DSL.Aggs[i]
		st := result.NewState(a.Fn)
		if ac.Count == "_doc_count" {
			st.Count = docCount
		} else if ac.Count != "" {
			v, _ := val(ac.Count)
			st.Count = int64(v)
		}
		if ac.Sum != "" {
			st.Sum, _ = val(ac.Sum)
		}
		conv := func(f float64) any {
			if a.Field == "time" {
				return time.UnixMilli(int64(f)).UTC()
			}
			if f == float64(int64(f)) {
				return int64(f)
			}
			return f
		}
		if ac.Min != "" {
			if v, ok := val(ac.Min); ok {
				st.Min = conv(v)
			}
		}
		if ac.Max != "" {
			if v, ok := val(ac.Max); ok {
				st.Max = conv(v)
			}
		}
		if ac.Distinct != "" {
			var t struct {
				Other   int64 `json:"sum_other_doc_count"`
				Buckets []struct {
					Key any `json:"key"`
				} `json:"buckets"`
			}
			if raw, ok := aggs[ac.Distinct]; ok && json.Unmarshal(raw, &t) == nil {
				for _, b := range t.Buckets {
					if len(st.Values) >= result.DistinctCap {
						st.Capped = true
						break
					}
					st.Values[fmt.Sprint(normalize(b.Key))] = struct{}{}
				}
				st.Capped = st.Capped || t.Other > 0
			}
		}
		out = append(out, st)
	}
	return out
}

func parseTime(v any) time.Time {
	switch x := v.(type) {
	case string:
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t.UTC()
		}
	case float64:
		return time.UnixMilli(int64(x)).UTC()
	}
	return time.Time{}
}

func normalize(v any) any {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return int64(f)
	}
	return v
}

func cloneBody(b map[string]any) map[string]any {
	raw, _ := json.Marshal(b)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

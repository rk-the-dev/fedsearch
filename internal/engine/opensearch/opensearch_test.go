package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/compiler"
	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/engine"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/planner"
	"github.com/rksurwase/fedsearch/internal/result"
	"github.com/rksurwase/fedsearch/internal/timex"
)

// fakeOS serves 2,500 documents with search_after paging and one composite
// aggregation with paging via after_key — the two protocols the adapter uses.
func fakeOS(t *testing.T) *httptest.Server {
	base := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "_search") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if aggs, ok := body["aggs"].(map[string]any); ok {
			comp := aggs["groups"].(map[string]any)["composite"].(map[string]any)
			if _, after := comp["after"]; !after {
				fmt.Fprint(w, `{"hits":{"total":{"value":30}},"aggregations":{"groups":{"after_key":{"k0":"failure"},"buckets":[
					{"key":{"k0":"failure"},"doc_count":10,"a1_s":{"value":100}}]}}}`)
				return
			}
			fmt.Fprint(w, `{"hits":{"total":{"value":30}},"aggregations":{"groups":{"buckets":[
				{"key":{"k0":"success"},"doc_count":20,"a1_s":{"value":5}}]}}}`)
			return
		}
		from := 0
		if sa, ok := body["search_after"].([]any); ok {
			from = int(sa[1].(float64)) + 1
		}
		size := int(body["size"].(float64))
		var hits []string
		for i := from; i < from+size && i < 2500; i++ {
			hits = append(hits, fmt.Sprintf(`{"_id":"%d","_source":{"time":"%s","event_id":"%06d","status":"failure","user_name":"u"},"sort":[%d,%d]}`,
				i, base.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), i, base.UnixMilli()+int64(i)*1000, i))
		}
		fmt.Fprintf(w, `{"hits":{"total":{"value":2500},"hits":[%s]}}`, strings.Join(hits, ","))
	}))
}

func slice(t *testing.T, srv *httptest.Server, q *ir.Query, limit int) (*Adapter, *planner.Slice) {
	status := &catalog.FieldBinding{Physical: "status", Enum: map[string]int64{"success": 1, "failure": 2}, Searchable: true, Aggregatable: true}
	loc := &catalog.Location{ID: "hot.authentication", Kind: "opensearch", AvgRowSize: 300,
		Caps: catalog.Capabilities{Dialect: "opensearch_dsl"},
		Bindings: map[string]*catalog.FieldBinding{
			"time": {Physical: "time", Searchable: true, Aggregatable: true}, "metadata.uid": {Physical: "event_id", Searchable: true, Aggregatable: true},
			"status_id": status, "user.name": {Physical: "user_name", Searchable: true, Aggregatable: true},
			"traffic.bytes_out": {Physical: "traffic_bytes_out", PhysType: "long", Searchable: true, Aggregatable: true},
		}}
	tr := timex.New(time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC))
	in := compiler.Input{Query: q, Loc: loc, Time: tr, Columns: []string{"status_id", "user.name"}, Limit: limit, Indices: []string{"ocsf-authentication-2026.09.10"}}
	d, err := compiler.CompileDSL(in)
	if err != nil {
		t.Fatal(err)
	}
	s := &planner.Slice{ID: "s1", Location: loc.ID, Source: "hot", Kind: "opensearch", Time: tr, Indices: in.Indices, DSL: d, Loc: loc, EngineLimit: limit, Columns: in.Columns}
	a := New(&config.Config{Sources: []config.Source{{Name: "hot", Kind: "opensearch", URL: srv.URL}}})
	return a, s
}

func drain(t *testing.T, a *Adapter, s *planner.Slice, q *ir.Query) (rows []result.Row, groups []*result.Group) {
	r := engine.NewRunner(a)
	h, err := r.Submit(context.Background(), s, q)
	if err != nil {
		t.Fatal(err)
	}
	for {
		p, err := r.Fetch(context.Background(), h)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, p.Rows...)
		groups = append(groups, p.Groups...)
		if p.Done {
			return
		}
	}
}

func TestRowsPageWithSearchAfter(t *testing.T) {
	srv := fakeOS(t)
	defer srv.Close()
	q := &ir.Query{Dataset: "authentication", Limit: 2200}
	a, s := slice(t, srv, q, 2200)
	rows, _ := drain(t, a, s, q)
	if len(rows) != 2200 {
		t.Fatalf("rows = %d, want 2200 across 3 pages", len(rows))
	}
	if rows[1500].ID != "001500" || rows[0].Fields["status_id"] != int64(2) {
		t.Fatalf("row mapping: %+v", rows[1500])
	}
}

func TestCompositeAggregatePaging(t *testing.T) {
	srv := fakeOS(t)
	defer srv.Close()
	q := &ir.Query{Dataset: "authentication", GroupBy: []string{"status_id"}, Limit: 10,
		Aggs: []ir.Agg{{Fn: ir.Count, As: "n"}, {Fn: ir.Sum, Field: "traffic.bytes_out", As: "b"}}}
	a, s := slice(t, srv, q, 100)
	_, groups := drain(t, a, s, q)
	if len(groups) != 2 {
		t.Fatalf("groups = %d", len(groups))
	}
	if groups[0].Key[0] != int64(2) || groups[0].States[0].Count != 10 || groups[1].States[1].Sum != 5 {
		t.Fatalf("groups: %+v %+v", groups[0], groups[1])
	}
}

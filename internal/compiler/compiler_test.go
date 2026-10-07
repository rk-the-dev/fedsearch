package compiler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/ir"
	"github.com/rksurwase/fedsearch/internal/timex"
)

func loc(kind, dialect string, unsupported ...string) *catalog.Location {
	b := func(phys, typ string) *catalog.FieldBinding {
		return &catalog.FieldBinding{Physical: phys, PhysType: typ, Searchable: true, Aggregatable: true}
	}
	status := b("status", "keyword")
	status.Enum = map[string]int64{"success": 1, "failure": 2}
	return &catalog.Location{ID: "x." + kind, Kind: kind, Caps: catalog.Capabilities{Dialect: dialect, UnsupportedOp: unsupported},
		Bindings: map[string]*catalog.FieldBinding{
			"time": b("time", "date"), "metadata.uid": b("event_id", "keyword"), "status_id": status,
			"user.name": b("user_name", "keyword"), "src_endpoint.ip": b("src_endpoint_ip", "ip"),
			"traffic.bytes_out": b("traffic_bytes_out", "long"),
		}}
}

var tr = timex.New(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC))

func TestSQLRowQuery(t *testing.T) {
	q := &ir.Query{Dataset: "authentication", Limit: 100, Order: &ir.Order{By: "time"}}
	where := ir.And(ir.C("status_id", ir.Eq, int64(2)), ir.C("user.name", ir.In, []any{"svc_backup", "o'brien"}))
	s, err := CompileSQL(Input{Query: q, Loc: loc("parquet", "duckdb_sql", "cidr"), Time: tr, Pushed: where,
		Columns: []string{"user.name", "src_endpoint.ip"}, Limit: 100, Files: []string{"/d/a.parquet", "/d/b.parquet"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`read_parquet(['/d/a.parquet', '/d/b.parquet'], union_by_name = true)`,
		`"status" = 'failure'`, // enum transform
		`'o''brien'`,           // quote escaping
		`"time" >= TIMESTAMPTZ '2026-09-01 00:00:00.000+00'`,
		`"time" < TIMESTAMPTZ '2026-09-07 00:00:00.000+00'`,
		`ORDER BY __t ASC, __id ASC`, `LIMIT 100`,
	} {
		if !strings.Contains(s.Text, want) {
			t.Errorf("missing %q in:\n%s", want, s.Text)
		}
	}
}

func TestSQLAggregate(t *testing.T) {
	q := &ir.Query{Dataset: "network_activity", GroupBy: []string{"user.name"}, Limit: 10,
		Aggs: []ir.Agg{{Fn: ir.Count, As: "n"}, {Fn: ir.Avg, Field: "traffic.bytes_out", As: "avg"}, {Fn: ir.CountDistinct, Field: "src_endpoint.ip", As: "ips"}}}
	s, err := CompileSQL(Input{Query: q, Loc: loc("parquet", "duckdb_sql"), Time: tr, Limit: GroupCap(10), Files: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"count(*) AS a0_c", `sum("traffic_bytes_out")::DOUBLE AS a1_s`, `count("traffic_bytes_out") AS a1_c`, "list(DISTINCT", "GROUP BY", "LIMIT 100"} {
		if !strings.Contains(s.Text, want) {
			t.Errorf("missing %q in:\n%s", want, s.Text)
		}
	}
}

func TestDSL(t *testing.T) {
	q := &ir.Query{Dataset: "authentication", Limit: 50}
	where := ir.And(ir.C("status_id", ir.Eq, int64(2)), ir.C("src_endpoint.ip", ir.CIDR, "10.20.0.0/16"), ir.Not(ir.C("user.name", ir.Prefix, "svc_")))
	d, err := CompileDSL(Input{Query: q, Loc: loc("opensearch", "opensearch_dsl"), Time: tr, Pushed: where, Columns: []string{"user.name"}, Limit: 50, Indices: []string{"ocsf-authentication-2026.09.01"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d.Body)
	for _, want := range []string{`{"term":{"status":"failure"}}`, `{"term":{"src_endpoint_ip":"10.20.0.0/16"}}`, `"must_not":[{"prefix":{"user_name":"svc_"}}]`, `"gte":1788220800000`, `"size":50`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}

	qa := &ir.Query{Dataset: "authentication", GroupBy: []string{"status_id"}, Aggs: []ir.Agg{{Fn: ir.Count, As: "n"}, {Fn: ir.Sum, Field: "traffic.bytes_out", As: "s"}}, Limit: 10}
	d2, err := CompileDSL(Input{Query: qa, Loc: loc("opensearch", "opensearch_dsl"), Time: tr, Limit: 100, Indices: []string{"i"}})
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(d2.Body)
	if !d2.Composite || !strings.Contains(string(b2), `"composite"`) || d2.Aggs[0].Count != "_doc_count" {
		t.Fatalf("aggregate DSL: %s", b2)
	}
}

func TestPushable(t *testing.T) {
	duck := loc("parquet", "duckdb_sql", "cidr")
	if Pushable(ir.C("src_endpoint.ip", ir.CIDR, "10.0.0.0/8"), duck) {
		t.Error("cidr should not be pushable to duckdb")
	}
	if !Pushable(ir.C("src_endpoint.ip", ir.CIDR, "10.0.0.0/8"), loc("opensearch", "opensearch_dsl")) {
		t.Error("cidr should be pushable to opensearch")
	}
	if Pushable(ir.C("status_id", ir.Gt, int64(1)), duck) {
		t.Error("range on an enum caption column must not be pushed")
	}
	if Pushable(ir.C("status_id", ir.Eq, int64(7)), duck) {
		t.Error("enum value without physical mapping must not be pushed")
	}
}

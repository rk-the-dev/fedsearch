package ir

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rksurwase/fedsearch/internal/schema"
)

type fakeSchema map[string]schema.FieldType

func (f fakeSchema) HasDataset(d string) bool { return d == "authentication" }
func (f fakeSchema) Field(d, p string) (schema.FieldType, schema.Role, bool) {
	t, ok := f[p]
	return t, schema.RoleDimension, ok
}
func (f fakeSchema) Fields(string) []string { return []string{"time", "user.name", "src_endpoint.ip"} }

var sch = fakeSchema{
	"time": schema.Timestamp, "user.name": schema.String, "src_endpoint.ip": schema.IP,
	"status_id": schema.Int, "traffic.bytes_out": schema.Int,
}

func init() { Now = func() time.Time { return time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC) } }

func parse(t *testing.T, s string) *Query {
	t.Helper()
	var q Query
	if err := json.Unmarshal([]byte(s), &q); err != nil {
		t.Fatal(err)
	}
	Normalize(&q, sch)
	return &q
}

func TestRelativeTime(t *testing.T) {
	q := parse(t, `{"dataset":"authentication","time":{"from":"now-30d"}}`)
	if want := Now().Add(-30 * 24 * time.Hour); !q.Time.From.Equal(want) || !q.Time.To.Equal(Now()) {
		t.Fatalf("time = %v", q.Time)
	}
}

func TestValidQuery(t *testing.T) {
	q := parse(t, `{"dataset":"authentication","time":{"from":"now-7d","to":"now"},
		"where":{"and":[{"cmp":{"field":"status_id","op":"eq","value":2}},{"cmp":{"field":"src_endpoint.ip","op":"cidr","value":"185.220.0.0/16"}}]},
		"group_by":["user.name"],"aggs":[{"fn":"count","as":"fails"}],"having":{"cmp":{"field":"fails","op":"gte","value":10}}}`)
	if err := Validate(q, sch); err != nil {
		t.Fatal(err)
	}
	if v := q.Where.And[1].Cmp.Value; v != int64(2) {
		t.Fatalf("status_id not coerced to int64: %#v (order: %+v)", v, q.Where.And)
	}
}

func TestValidationCollectsAllProblems(t *testing.T) {
	q := parse(t, `{"dataset":"authentication","time":{"from":"now","to":"now-1d"},
		"where":{"cmp":{"field":"src_endpoint.ip","op":"prefix","value":"10."}},
		"aggs":[{"fn":"median","field":"traffic.bytes_out","as":"m"}],"limit":5000}`)
	err := Validate(q, sch)
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected validation error, got %v", err)
	}
	joined := strings.Join(ve.Problems, "\n")
	for _, want := range []string{"before", "prefix only applies", "unsupported function", "limit must be"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing problem %q in:\n%s", want, joined)
		}
	}
}

func TestHashIsOrderInsensitive(t *testing.T) {
	a := parse(t, `{"dataset":"authentication","time":{"from":"2026-09-01","to":"2026-09-02"},
		"where":{"and":[{"cmp":{"field":"user.name","op":"eq","value":"x"}},{"cmp":{"field":"status_id","op":"eq","value":2}}]}}`)
	b := parse(t, `{"dataset":"AUTHENTICATION","time":{"from":"2026-09-01","to":"2026-09-02"},
		"where":{"and":[{"cmp":{"field":"status_id","op":"EQ","value":2}},{"and":[{"cmp":{"field":"user.name","op":"eq","value":"x"}}]}]}}`)
	if Hash(a) != Hash(b) {
		t.Fatal("equivalent queries hashed differently")
	}
}

func TestEval(t *testing.T) {
	row := map[string]any{"src_endpoint.ip": "10.20.4.17", "status_id": int64(2), "user.name": "svc_backup"}
	get := func(p string) (any, bool) { v, ok := row[p]; return v, ok }
	cases := map[string]bool{
		`{"cmp":{"field":"src_endpoint.ip","op":"cidr","value":"10.20.0.0/16"}}`:                                                  true,
		`{"cmp":{"field":"src_endpoint.ip","op":"cidr","value":"10.30.0.0/16"}}`:                                                  false,
		`{"and":[{"cmp":{"field":"status_id","op":"gte","value":2}},{"cmp":{"field":"user.name","op":"prefix","value":"svc_"}}]}`: true,
		`{"not":{"cmp":{"field":"user.name","op":"in","value":["a","svc_backup"]}}}`:                                              false,
		`{"cmp":{"field":"missing","op":"exists"}}`:                                                                               false,
	}
	for s, want := range cases {
		var e Expr
		if err := json.Unmarshal([]byte(s), &e); err != nil {
			t.Fatal(err)
		}
		if got := Eval(&e, get); got != want {
			t.Errorf("%s = %v, want %v", s, got, want)
		}
	}
}

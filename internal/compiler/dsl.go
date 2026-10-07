package compiler

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/ir"
)

// DSL is a compiled OpenSearch request.
type DSL struct {
	Indices   []string
	Body      map[string]any
	Columns   []string // OCSF paths of row fields
	Physical  map[string]string
	GroupCols []string
	Aggs      []AggCols
	Composite bool
	PageSize  int
}

// Text renders the request the way a human would paste it into Dev Tools.
func (d *DSL) Text() string {
	b, _ := json.MarshalIndent(d.Body, "", "  ")
	idx := strings.Join(d.Indices, ",")
	if len(d.Indices) > 3 {
		idx = fmt.Sprintf("%s,…(%d indices)…,%s", d.Indices[0], len(d.Indices)-2, d.Indices[len(d.Indices)-1])
	}
	return fmt.Sprintf("POST /%s/_search\n%s", idx, b)
}

const dslPageSize = 1000

// CompileDSL renders a slice as an OpenSearch _search request against the
// pruned daily indices.
func CompileDSL(in Input) (*DSL, error) {
	if len(in.Indices) == 0 {
		return nil, fmt.Errorf("%s: no indices selected for slice", in.Loc.ID)
	}
	filter := []any{map[string]any{"range": map[string]any{"time": map[string]any{
		"gte": in.Time.From.UnixMilli(), "lt": in.Time.To.UnixMilli(), "format": "epoch_millis",
	}}}}
	if in.Pushed != nil {
		q, err := dslExpr(in.Pushed, in.Loc)
		if err != nil {
			return nil, err
		}
		filter = append(filter, q)
	}
	query := map[string]any{"bool": map[string]any{"filter": filter}}
	out := &DSL{Indices: in.Indices, Physical: map[string]string{}}

	if !in.Query.IsAggregate() {
		idB, err := binding(in.Loc, "metadata.uid")
		if err != nil {
			return nil, err
		}
		src := []string{"time", idB.Physical}
		for _, c := range in.Columns {
			if c == "time" || c == "metadata.uid" {
				continue
			}
			if b, err := binding(in.Loc, c); err == nil {
				src = append(src, b.Physical)
				out.Columns = append(out.Columns, c)
				out.Physical[c] = b.Physical
			}
		}
		order := "asc"
		if in.Query.Order != nil && in.Query.Order.Desc {
			order = "desc"
		}
		size := in.Limit
		if size > dslPageSize {
			size = dslPageSize
		}
		out.PageSize = size
		out.Body = map[string]any{
			"size": size, "track_total_hits": false, "_source": src, "query": query,
			"sort": []any{map[string]any{"time": map[string]any{"order": order}}, map[string]any{idB.Physical: map[string]any{"order": "asc"}}},
		}
		return out, nil
	}

	sub := map[string]any{}
	for i, a := range in.Query.Aggs {
		var ac AggCols
		phys := ""
		if a.Field != "" {
			b, err := binding(in.Loc, a.Field)
			if err != nil {
				return nil, err
			}
			if !b.Aggregatable {
				return nil, fmt.Errorf("%s: %s is not aggregatable in %s", a.Fn, a.Field, in.Loc.ID)
			}
			phys = b.Physical
		}
		field := map[string]any{"field": phys}
		switch a.Fn {
		case ir.Count:
			ac.Count = AggAlias(i, "c")
			if phys != "" {
				sub[ac.Count] = map[string]any{"value_count": field}
			} else {
				ac.Count = "_doc_count"
			}
		case ir.Sum:
			ac.Sum = AggAlias(i, "s")
			sub[ac.Sum] = map[string]any{"sum": field}
		case ir.Avg:
			ac.Sum, ac.Count = AggAlias(i, "s"), AggAlias(i, "c")
			sub[ac.Sum] = map[string]any{"sum": field}
			sub[ac.Count] = map[string]any{"value_count": field}
		case ir.Min, ir.Max:
			if t := in.Loc.Bindings[a.Field].PhysType; strings.EqualFold(t, "keyword") || strings.EqualFold(t, "text") {
				return nil, fmt.Errorf("%s on string field %s is not supported by OpenSearch", a.Fn, a.Field)
			}
			if a.Fn == ir.Min {
				ac.Min = AggAlias(i, "mn")
				sub[ac.Min] = map[string]any{"min": field}
			} else {
				ac.Max = AggAlias(i, "mx")
				sub[ac.Max] = map[string]any{"max": field}
			}
		case ir.CountDistinct:
			ac.Distinct = AggAlias(i, "d")
			sub[ac.Distinct] = map[string]any{"terms": map[string]any{"field": phys, "size": 10_000 + 1}}
		}
		out.Aggs = append(out.Aggs, ac)
	}
	body := map[string]any{"size": 0, "track_total_hits": true, "query": query}
	if len(in.Query.GroupBy) == 0 {
		if len(sub) > 0 {
			body["aggs"] = sub
		}
	} else {
		var sources []any
		for i, g := range in.Query.GroupBy {
			b, err := binding(in.Loc, g)
			if err != nil {
				return nil, err
			}
			sources = append(sources, map[string]any{fmt.Sprintf("k%d", i): map[string]any{"terms": map[string]any{"field": b.Physical, "missing_bucket": true}}})
			out.GroupCols = append(out.GroupCols, g)
			out.Physical[g] = b.Physical
		}
		size := in.Limit
		if size > dslPageSize {
			size = dslPageSize
		}
		out.PageSize = size
		g := map[string]any{"composite": map[string]any{"size": size, "sources": sources}}
		if len(sub) > 0 {
			g["aggs"] = sub
		}
		body["aggs"] = map[string]any{"groups": g}
		out.Composite = true
	}
	out.Body = body
	return out, nil
}

func dslExpr(e *ir.Expr, loc *catalog.Location) (map[string]any, error) {
	list := func(kids []*ir.Expr) ([]any, error) {
		out := make([]any, 0, len(kids))
		for _, k := range kids {
			q, err := dslExpr(k, loc)
			if err != nil {
				return nil, err
			}
			out = append(out, q)
		}
		return out, nil
	}
	switch {
	case len(e.And) > 0:
		l, err := list(e.And)
		return map[string]any{"bool": map[string]any{"filter": l}}, err
	case len(e.Or) > 0:
		l, err := list(e.Or)
		return map[string]any{"bool": map[string]any{"should": l, "minimum_should_match": 1}}, err
	case e.Not != nil:
		q, err := dslExpr(e.Not, loc)
		return map[string]any{"bool": map[string]any{"must_not": []any{q}}}, err
	}
	c := e.Cmp
	b, err := binding(loc, c.Field)
	if err != nil {
		return nil, err
	}
	f := b.Physical
	val := func(v any) (any, error) {
		pv, err := physicalLiteral(b, v)
		if t, ok := pv.(time.Time); ok {
			return t.UnixMilli(), err
		}
		return pv, err
	}
	switch c.Op {
	case ir.Exists:
		return map[string]any{"exists": map[string]any{"field": f}}, nil
	case ir.Prefix:
		return map[string]any{"prefix": map[string]any{f: c.Value}}, nil
	case ir.CIDR:
		// ip fields accept CIDR notation in term queries.
		return map[string]any{"term": map[string]any{f: c.Value}}, nil
	case ir.In:
		raw, _ := c.Value.([]any)
		vals := make([]any, 0, len(raw))
		for _, v := range raw {
			pv, err := val(v)
			if err != nil {
				return nil, err
			}
			vals = append(vals, pv)
		}
		return map[string]any{"terms": map[string]any{f: vals}}, nil
	}
	v, err := val(c.Value)
	if err != nil {
		return nil, err
	}
	switch c.Op {
	case ir.Eq:
		return map[string]any{"term": map[string]any{f: v}}, nil
	case ir.Ne:
		return map[string]any{"bool": map[string]any{"must_not": []any{map[string]any{"term": map[string]any{f: v}}}}}, nil
	}
	op := map[ir.Op]string{ir.Lt: "lt", ir.Lte: "lte", ir.Gt: "gt", ir.Gte: "gte"}[c.Op]
	r := map[string]any{op: v}
	if c.Field == "time" {
		r["format"] = "epoch_millis"
	}
	return map[string]any{"range": map[string]any{f: r}}, nil
}

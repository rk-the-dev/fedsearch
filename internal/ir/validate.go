package ir

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/rksurwase/fedsearch/internal/schema"
)

// Limits bound what any caller, including an LLM, can ask for.
const (
	MaxSpan       = 366 * 24 * time.Hour
	MaxRowLimit   = 10_000
	MaxGroupLimit = 1_000
	MaxInValues   = 1_000
	MaxExprDepth  = 8
	MaxExprNodes  = 64
	DefaultRows   = 100
	DefaultGroups = 50
)

// ValidationError lists every problem at once, so an LLM repair loop can fix
// them all in one round.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid query: " + strings.Join(e.Problems, "; ")
}

// Normalize fills defaults, coerces literal types using the schema, and puts
// the query in canonical form. It must run before Validate and Hash.
func Normalize(q *Query, s schema.Lookup) {
	q.V = Version
	q.Dataset = strings.ToLower(strings.TrimSpace(q.Dataset))
	q.Time.From = q.Time.From.UTC().Truncate(time.Millisecond)
	q.Time.To = q.Time.To.UTC().Truncate(time.Millisecond)
	if q.IsAggregate() {
		q.Select = nil
		if q.Limit == 0 {
			q.Limit = DefaultGroups
		}
		if q.Order == nil {
			q.Order = &Order{By: q.Aggs[0].As, Desc: true}
		}
	} else {
		if len(q.Select) == 0 && s != nil {
			q.Select = s.Fields(q.Dataset)
		}
		if q.Limit == 0 {
			q.Limit = DefaultRows
		}
		if q.Order == nil {
			q.Order = &Order{By: "time"}
		}
	}
	for i := range q.Aggs {
		q.Aggs[i].Fn = AggFn(strings.ToLower(string(q.Aggs[i].Fn)))
		if q.Aggs[i].As == "" {
			q.Aggs[i].As = string(q.Aggs[i].Fn)
			if q.Aggs[i].Field != "" {
				q.Aggs[i].As += "_" + strings.ReplaceAll(q.Aggs[i].Field, ".", "_")
			}
		}
	}
	q.Where = normalizeExpr(q.Where, q.Dataset, s)
	q.Having = normalizeExpr(q.Having, "", nil)
}

func normalizeExpr(e *Expr, dataset string, s schema.Lookup) *Expr {
	if e == nil {
		return nil
	}
	switch {
	case e.Cmp != nil:
		c := e.Cmp
		c.Op = Op(strings.ToLower(string(c.Op)))
		if s != nil {
			if t, _, ok := s.Field(dataset, c.Field); ok {
				c.Value = coerce(c.Value, t, c.Op)
			}
		}
		return e
	case e.Not != nil:
		e.Not = normalizeExpr(e.Not, dataset, s)
		return e
	case len(e.And) > 0 || len(e.Or) > 0:
		isAnd := len(e.And) > 0
		kids := e.And
		if !isAnd {
			kids = e.Or
		}
		var flat []*Expr
		for _, k := range kids {
			k = normalizeExpr(k, dataset, s)
			if k == nil {
				continue
			}
			// Flatten nested and(and(..)) / or(or(..)).
			if isAnd && len(k.And) > 0 {
				flat = append(flat, k.And...)
			} else if !isAnd && len(k.Or) > 0 {
				flat = append(flat, k.Or...)
			} else {
				flat = append(flat, k)
			}
		}
		sort.SliceStable(flat, func(i, j int) bool { return exprKey(flat[i]) < exprKey(flat[j]) })
		if len(flat) == 1 {
			return flat[0]
		}
		if isAnd {
			return &Expr{And: flat}
		}
		return &Expr{Or: flat}
	}
	return nil
}

func exprKey(e *Expr) string { return fmt.Sprintf("%v", canon(e)) }

// coerce converts JSON literals (float64, string) into the field's type.
func coerce(v any, t schema.FieldType, op Op) any {
	if op == In {
		if list, ok := v.([]any); ok {
			out := make([]any, len(list))
			for i, x := range list {
				out[i] = coerce(x, t, Eq)
			}
			return out
		}
		return v
	}
	if op == Exists || op == CIDR || op == Prefix {
		return v
	}
	switch t {
	case schema.Int:
		switch x := v.(type) {
		case float64:
			return int64(x)
		case int:
			return int64(x)
		case string:
			var n int64
			if _, err := fmt.Sscan(x, &n); err == nil {
				return n
			}
		}
	case schema.Float:
		switch x := v.(type) {
		case int64:
			return float64(x)
		case int:
			return float64(x)
		}
	case schema.Timestamp:
		if s, ok := v.(string); ok {
			if ts, err := ParseTime(s, Now()); err == nil {
				return ts
			}
		}
	}
	return v
}

// Validate checks the query against the schema and the safety limits.
func Validate(q *Query, s schema.Lookup) error {
	var p []string
	bad := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }

	if q.Dataset == "" || !s.HasDataset(q.Dataset) {
		bad("unknown dataset %q", q.Dataset)
		return &ValidationError{p}
	}
	if q.Time.From.IsZero() || q.Time.To.IsZero() {
		bad("time.from and time.to are required")
	} else if !q.Time.From.Before(q.Time.To) {
		bad("time.from must be before time.to")
	} else if q.Time.Duration() > MaxSpan {
		bad("time span %s exceeds the 366-day limit", q.Time.Duration().Round(time.Hour))
	}

	fieldType := func(path, where string) (schema.FieldType, bool) {
		t, _, ok := s.Field(q.Dataset, path)
		if !ok {
			bad("%s: unknown field %q", where, path)
		}
		return t, ok
	}
	for _, f := range q.Select {
		fieldType(f, "select")
	}
	for _, f := range q.GroupBy {
		fieldType(f, "group_by")
	}
	for _, f := range q.Enrich {
		if t, ok := fieldType(f, "enrich"); ok && t != schema.IP {
			bad("enrich: %q is %s, enrichment needs an ip field", f, t)
		}
	}

	aliases := map[string]bool{}
	for _, a := range q.Aggs {
		switch a.Fn {
		case Count:
			if a.Field != "" {
				fieldType(a.Field, "aggs")
			}
		case Sum, Avg:
			if t, ok := fieldType(a.Field, "aggs"); ok && !t.Numeric() {
				bad("aggs: %s needs a numeric field, %q is %s", a.Fn, a.Field, t)
			}
		case Min, Max:
			if t, ok := fieldType(a.Field, "aggs"); ok && !(t.Ordered() || t == schema.String) {
				bad("aggs: %s not supported on %s", a.Fn, t)
			}
		case CountDistinct:
			fieldType(a.Field, "aggs")
		default:
			bad("aggs: unsupported function %q (allowed: count, sum, min, max, avg, count_distinct)", a.Fn)
		}
		if aliases[a.As] {
			bad("aggs: duplicate alias %q", a.As)
		}
		aliases[a.As] = true
	}
	if len(q.GroupBy) > 0 && !q.IsAggregate() {
		bad("group_by requires at least one aggregate")
	}
	if q.Having != nil && !q.IsAggregate() {
		bad("having requires aggregates")
	}

	if q.IsAggregate() {
		if q.Limit < 1 || q.Limit > MaxGroupLimit {
			bad("limit must be 1..%d for aggregate queries", MaxGroupLimit)
		}
	} else if q.Limit < 1 || q.Limit > MaxRowLimit {
		bad("limit must be 1..%d for row queries", MaxRowLimit)
	}

	if q.Order != nil {
		if q.IsAggregate() {
			ok := aliases[q.Order.By]
			for _, g := range q.GroupBy {
				ok = ok || g == q.Order.By
			}
			if !ok {
				bad("order.by %q must be an aggregate alias or group field", q.Order.By)
			}
		} else if q.Order.By != "time" {
			bad("row queries can only be ordered by time")
		}
	}

	nodes := 0
	var check func(e *Expr, depth int, having bool)
	check = func(e *Expr, depth int, having bool) {
		if e == nil {
			return
		}
		nodes++
		if depth > MaxExprDepth {
			bad("expression deeper than %d", MaxExprDepth)
			return
		}
		set := 0
		for _, b := range []bool{len(e.And) > 0, len(e.Or) > 0, e.Not != nil, e.Cmp != nil} {
			if b {
				set++
			}
		}
		if set != 1 {
			bad("each expression node needs exactly one of and/or/not/cmp")
			return
		}
		for _, k := range append(append([]*Expr{}, e.And...), e.Or...) {
			check(k, depth+1, having)
		}
		check(e.Not, depth+1, having)
		if e.Cmp != nil {
			if having {
				if !aliases[e.Cmp.Field] && !contains(q.GroupBy, e.Cmp.Field) {
					bad("having: %q is not an aggregate alias or group field", e.Cmp.Field)
				}
				return
			}
			if t, ok := fieldType(e.Cmp.Field, "where"); ok {
				checkCmp(e.Cmp, t, bad)
			}
		}
	}
	check(q.Where, 1, false)
	check(q.Having, 1, true)
	if nodes > MaxExprNodes {
		bad("expression has %d nodes, limit is %d", nodes, MaxExprNodes)
	}
	if len(p) > 0 {
		return &ValidationError{p}
	}
	return nil
}

func checkCmp(c *Cmp, t schema.FieldType, bad func(string, ...any)) {
	switch c.Op {
	case Eq, Ne:
		if c.Value == nil {
			bad("where: %s %s needs a value", c.Field, c.Op)
		}
		if t == schema.IP {
			if s, ok := c.Value.(string); !ok || !validIP(s) {
				bad("where: %q is not a valid IP for %s", c.Value, c.Field)
			}
		}
	case Lt, Lte, Gt, Gte:
		if !t.Ordered() {
			bad("where: %s not allowed on %s field %s", c.Op, t, c.Field)
		}
	case In:
		list, ok := c.Value.([]any)
		if !ok || len(list) == 0 {
			bad("where: in on %s needs a non-empty list", c.Field)
		} else if len(list) > MaxInValues {
			bad("where: in list has %d values, limit is %d", len(list), MaxInValues)
		}
	case CIDR:
		s, _ := c.Value.(string)
		if t != schema.IP {
			bad("where: cidr only applies to ip fields, %s is %s", c.Field, t)
		} else if _, err := netip.ParsePrefix(s); err != nil {
			bad("where: %q is not a CIDR block", c.Value)
		}
	case Prefix:
		if t != schema.String {
			bad("where: prefix only applies to string fields, %s is %s", c.Field, t)
		}
		if s, ok := c.Value.(string); !ok || s == "" {
			bad("where: prefix needs a non-empty string")
		}
	case Exists:
	default:
		bad("where: unknown operator %q", c.Op)
	}
}

func validIP(s string) bool { _, err := netip.ParseAddr(s); return err == nil }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Hash returns a stable identifier for a normalized query. It keys result
// caches, the NL fallback cache and audit records.
func Hash(q *Query) string {
	b, _ := json.Marshal(canonQuery(q))
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func canonQuery(q *Query) any {
	return map[string]any{
		"v": q.V, "dataset": q.Dataset, "time": q.Time, "where": canon(q.Where),
		"select": q.Select, "group_by": q.GroupBy, "aggs": q.Aggs, "having": canon(q.Having),
		"order": q.Order, "limit": q.Limit, "enrich": q.Enrich,
	}
}

func canon(e *Expr) any {
	if e == nil {
		return nil
	}
	switch {
	case e.Cmp != nil:
		return []any{e.Cmp.Field, string(e.Cmp.Op), fmt.Sprint(e.Cmp.Value)}
	case e.Not != nil:
		return map[string]any{"not": canon(e.Not)}
	case len(e.And) > 0:
		out := make([]any, len(e.And))
		for i, k := range e.And {
			out[i] = canon(k)
		}
		return map[string]any{"and": out}
	default:
		out := make([]any, len(e.Or))
		for i, k := range e.Or {
			out[i] = canon(k)
		}
		return map[string]any{"or": out}
	}
}

// Getter fetches a field value from a row or group.
type Getter func(path string) (any, bool)

// Eval evaluates an expression in Go. The executor uses it for residual
// predicates an engine cannot evaluate, and the merge layer uses it for HAVING.
func Eval(e *Expr, get Getter) bool {
	if e == nil {
		return true
	}
	switch {
	case len(e.And) > 0:
		for _, k := range e.And {
			if !Eval(k, get) {
				return false
			}
		}
		return true
	case len(e.Or) > 0:
		for _, k := range e.Or {
			if Eval(k, get) {
				return true
			}
		}
		return false
	case e.Not != nil:
		return !Eval(e.Not, get)
	case e.Cmp != nil:
		return evalCmp(e.Cmp, get)
	}
	return true
}

func evalCmp(c *Cmp, get Getter) bool {
	v, ok := get(c.Field)
	if c.Op == Exists {
		return ok && v != nil
	}
	if !ok || v == nil {
		return c.Op == Ne
	}
	switch c.Op {
	case Eq:
		return compare(v, c.Value) == 0
	case Ne:
		return compare(v, c.Value) != 0
	case Lt:
		return compare(v, c.Value) < 0
	case Lte:
		return compare(v, c.Value) <= 0
	case Gt:
		return compare(v, c.Value) > 0
	case Gte:
		return compare(v, c.Value) >= 0
	case In:
		list, _ := c.Value.([]any)
		for _, x := range list {
			if compare(v, x) == 0 {
				return true
			}
		}
		return false
	case Prefix:
		s, _ := c.Value.(string)
		return strings.HasPrefix(fmt.Sprint(v), s)
	case CIDR:
		p, err := netip.ParsePrefix(fmt.Sprint(c.Value))
		if err != nil {
			return false
		}
		a, err := netip.ParseAddr(fmt.Sprint(v))
		return err == nil && p.Contains(a)
	}
	return false
}

// compare orders two values: numbers numerically, times chronologically,
// everything else as strings. Returns -1, 0 or 1.
func compare(a, b any) int {
	if fa, ok := toFloat(a); ok {
		if fb, ok := toFloat(b); ok {
			switch {
			case fa < fb:
				return -1
			case fa > fb:
				return 1
			}
			return 0
		}
	}
	if ta, ok := a.(time.Time); ok {
		if tb, ok := b.(time.Time); ok {
			return ta.Compare(tb)
		}
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// Compare is exported for the merge layer's group ordering.
func Compare(a, b any) int { return compare(a, b) }

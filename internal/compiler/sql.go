package compiler

import (
	"fmt"
	"strings"
	"time"

	"github.com/rksurwase/fedsearch/internal/catalog"
	"github.com/rksurwase/fedsearch/internal/ir"
)

// SQL is a compiled DuckDB query.
type SQL struct {
	Text      string
	Columns   []string  // OCSF paths, in SELECT order after __t and __id (row queries)
	GroupCols []string  // OCSF paths of group keys (aggregate queries)
	Aggs      []AggCols // partial columns per agg
}

// CompileSQL renders a slice as DuckDB SQL over an explicit list of files.
// Listing files (rather than globbing) makes partition pruning explicit: the
// engine can only read what the planner selected.
func CompileSQL(in Input) (*SQL, error) {
	src, err := sqlSource(in)
	if err != nil {
		return nil, err
	}
	tcol := timeExpr(in.Loc)
	where := []string{
		fmt.Sprintf("%s >= %s", tcol, tsLit(in.Time.From)),
		fmt.Sprintf("%s < %s", tcol, tsLit(in.Time.To)),
	}
	if in.Pushed != nil {
		w, err := sqlExpr(in.Pushed, in.Loc)
		if err != nil {
			return nil, err
		}
		where = append(where, w)
	}
	out := &SQL{}
	var sb strings.Builder
	if !in.Query.IsAggregate() {
		idB, err := binding(in.Loc, "metadata.uid")
		if err != nil {
			return nil, err
		}
		sb.WriteString(fmt.Sprintf("SELECT epoch_ms(%s) AS __t, %s AS __id", tcol, qi(idB.Physical)))
		for _, c := range in.Columns {
			if c == "time" || c == "metadata.uid" {
				continue
			}
			b, err := binding(in.Loc, c)
			if err != nil {
				continue // column absent in this location: returned as null
			}
			sb.WriteString(fmt.Sprintf(",\n       %s AS %s", qi(b.Physical), qi(c)))
			out.Columns = append(out.Columns, c)
		}
		dir := "ASC"
		if in.Query.Order != nil && in.Query.Order.Desc {
			dir = "DESC"
		}
		sb.WriteString(fmt.Sprintf("\nFROM %s\nWHERE %s\nORDER BY __t %s, __id ASC\nLIMIT %d", src, strings.Join(where, "\n  AND "), dir, in.Limit))
		out.Text = sb.String()
		return out, nil
	}

	var sel, groupBy []string
	for _, g := range in.Query.GroupBy {
		b, err := binding(in.Loc, g)
		if err != nil {
			return nil, err
		}
		expr := qi(b.Physical)
		if g == "time" {
			expr = fmt.Sprintf("epoch_ms(%s)", tcol)
		}
		sel = append(sel, fmt.Sprintf("%s AS %s", expr, qi(g)))
		groupBy = append(groupBy, qi(g))
		out.GroupCols = append(out.GroupCols, g)
	}
	firstCount := ""
	for i, a := range in.Query.Aggs {
		var ac AggCols
		field := "*"
		if a.Field != "" {
			b, err := binding(in.Loc, a.Field)
			if err != nil {
				return nil, err
			}
			field = qi(b.Physical)
			if a.Field == "time" {
				field = tcol
			}
		}
		wrapTime := func(e string) string {
			if a.Field == "time" {
				return "epoch_ms(" + e + ")"
			}
			return e
		}
		switch a.Fn {
		case ir.Count:
			ac.Count = AggAlias(i, "c")
			sel = append(sel, fmt.Sprintf("count(%s) AS %s", field, ac.Count))
		case ir.Sum:
			ac.Sum = AggAlias(i, "s")
			sel = append(sel, fmt.Sprintf("sum(%s)::DOUBLE AS %s", field, ac.Sum))
		case ir.Avg:
			ac.Sum, ac.Count = AggAlias(i, "s"), AggAlias(i, "c")
			sel = append(sel, fmt.Sprintf("sum(%s)::DOUBLE AS %s", field, ac.Sum), fmt.Sprintf("count(%s) AS %s", field, ac.Count))
		case ir.Min:
			ac.Min = AggAlias(i, "mn")
			sel = append(sel, fmt.Sprintf("%s AS %s", wrapTime("min("+field+")"), ac.Min))
		case ir.Max:
			ac.Max = AggAlias(i, "mx")
			sel = append(sel, fmt.Sprintf("%s AS %s", wrapTime("max("+field+")"), ac.Max))
		case ir.CountDistinct:
			ac.Distinct = AggAlias(i, "d")
			sel = append(sel, fmt.Sprintf("list_slice(list(DISTINCT CAST(%s AS VARCHAR)), 1, %d) AS %s", field, 10_000+1, ac.Distinct))
		}
		if firstCount == "" && ac.Count != "" {
			firstCount = ac.Count
		}
		out.Aggs = append(out.Aggs, ac)
	}
	sb.WriteString("SELECT " + strings.Join(sel, ",\n       "))
	sb.WriteString(fmt.Sprintf("\nFROM %s\nWHERE %s", src, strings.Join(where, "\n  AND ")))
	if len(groupBy) > 0 {
		sb.WriteString("\nGROUP BY " + strings.Join(groupBy, ", "))
		order := "count(*)"
		if firstCount != "" {
			order = firstCount
		}
		sb.WriteString(fmt.Sprintf("\nORDER BY %s DESC\nLIMIT %d", order, in.Limit))
	}
	out.Text = sb.String()
	return out, nil
}

func sqlSource(in Input) (string, error) {
	if len(in.Files) == 0 {
		return "", fmt.Errorf("%s: no files selected for slice", in.Loc.ID)
	}
	files := make([]string, len(in.Files))
	for i, f := range in.Files {
		files[i] = sqlString(f)
	}
	list := "[" + strings.Join(files, ", ") + "]"
	switch in.Loc.Kind {
	case "parquet":
		return fmt.Sprintf("read_parquet(%s, union_by_name = true)", list), nil
	case "ndjson":
		// Explicit columns from the bindings: no type inference drift, and
		// _bulk action lines (no event_id) are filtered out.
		var cols []string
		for _, b := range in.Loc.Bindings {
			cols = append(cols, fmt.Sprintf("%s: '%s'", sqlString(b.Physical), duckType(b.PhysType)))
		}
		sortStrings(cols)
		return fmt.Sprintf("(SELECT * FROM read_json(%s, format = 'newline_delimited', columns = {%s}) WHERE event_id IS NOT NULL)",
			list, strings.Join(cols, ", ")), nil
	}
	return "", fmt.Errorf("sql compiler cannot read %s", in.Loc.Kind)
}

func duckType(phys string) string {
	switch strings.ToUpper(phys) {
	case "BIGINT", "INT64", "INT32", "INTEGER", "LONG":
		return "BIGINT"
	case "DOUBLE", "FLOAT":
		return "DOUBLE"
	case "BOOLEAN":
		return "BOOLEAN"
	}
	return "VARCHAR"
}

func timeExpr(loc *catalog.Location) string {
	if loc.Kind == "ndjson" {
		return `CAST("time" AS TIMESTAMPTZ)`
	}
	return `"time"`
}

func sqlExpr(e *ir.Expr, loc *catalog.Location) (string, error) {
	join := func(kids []*ir.Expr, op string) (string, error) {
		parts := make([]string, 0, len(kids))
		for _, k := range kids {
			s, err := sqlExpr(k, loc)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "(" + strings.Join(parts, " "+op+" ") + ")", nil
	}
	switch {
	case len(e.And) > 0:
		return join(e.And, "AND")
	case len(e.Or) > 0:
		return join(e.Or, "OR")
	case e.Not != nil:
		s, err := sqlExpr(e.Not, loc)
		return "NOT " + s, err
	}
	c := e.Cmp
	b, err := binding(loc, c.Field)
	if err != nil {
		return "", err
	}
	col := qi(b.Physical)
	if c.Field == "time" {
		col = timeExpr(loc)
	}
	lit := func(v any) (string, error) {
		pv, err := physicalLiteral(b, v)
		if err != nil {
			return "", err
		}
		return sqlLiteral(pv), nil
	}
	switch c.Op {
	case ir.Exists:
		return col + " IS NOT NULL", nil
	case ir.Prefix:
		return fmt.Sprintf("starts_with(%s, %s)", col, sqlString(fmt.Sprint(c.Value))), nil
	case ir.In:
		list, _ := c.Value.([]any)
		vals := make([]string, 0, len(list))
		for _, v := range list {
			l, err := lit(v)
			if err != nil {
				return "", err
			}
			vals = append(vals, l)
		}
		return fmt.Sprintf("%s IN (%s)", col, strings.Join(vals, ", ")), nil
	case ir.CIDR:
		return "", fmt.Errorf("cidr is not supported by %s", loc.Caps.Engine)
	}
	l, err := lit(c.Value)
	if err != nil {
		return "", err
	}
	op := map[ir.Op]string{ir.Eq: "=", ir.Ne: "<>", ir.Lt: "<", ir.Lte: "<=", ir.Gt: ">", ir.Gte: ">="}[c.Op]
	if c.Op == ir.Ne {
		// Missing values count as "not equal", matching ir.Eval semantics.
		return fmt.Sprintf("(%s <> %s OR %s IS NULL)", col, l, col), nil
	}
	return fmt.Sprintf("%s %s %s", col, op, l), nil
}

func sqlLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return sqlString(x)
	case time.Time:
		return tsLit(x)
	case int, int32, int64:
		return fmt.Sprint(x)
	case float64:
		return fmt.Sprintf("%g", x)
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	}
	return sqlString(fmt.Sprint(v))
}

func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func qi(id string) string { return `"` + strings.ReplaceAll(id, `"`, `""`) + `"` }

func tsLit(t time.Time) string {
	return "TIMESTAMPTZ '" + t.UTC().Format("2006-01-02 15:04:05.000") + "+00'"
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

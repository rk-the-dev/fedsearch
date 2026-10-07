package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/osclient"
)

// opensearchDiscoverer learns daily indices (<prefix>-YYYY.MM.DD) through the
// cluster's own metadata APIs: _cat/indices for partitions and sizes,
// _field_caps for per-field searchable/aggregatable capabilities, and one
// min/max aggregation for observed time coverage.
type opensearchDiscoverer struct{}

// IndexPattern returns the wildcard pattern for a dataset, e.g. ocsf-authentication-*.
func IndexPattern(src config.Source, dataset string) string {
	return IndexPrefix(src, dataset) + "*"
}

// IndexPrefix returns the daily index prefix for a dataset.
func IndexPrefix(src config.Source, dataset string) string {
	f := src.IndexFmt
	if f == "" {
		f = "ocsf-{dataset}-"
	}
	return strings.ReplaceAll(f, "{dataset}", dataset)
}

func (opensearchDiscoverer) Discover(ctx context.Context, src config.Source, dataset string, prev *Location) (*Location, *LocationReport, error) {
	start := time.Now()
	c := osclient.New(src.URL)
	pattern := IndexPattern(src, dataset)
	loc := &Location{
		ID: src.Name + "." + dataset, Dataset: dataset,
		Layout: Layout{Root: src.URL, Pattern: pattern},
		Caps:   Capabilities{Engine: "opensearch", Dialect: "opensearch_dsl", Aggregations: true},
	}
	rep := &LocationReport{Location: loc.ID}

	indices, err := c.CatIndices(ctx, pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("cat indices %s: %w", pattern, err)
	}
	caps, err := c.FieldCaps(ctx, pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("field caps %s: %w", pattern, err)
	}
	var schemaFields []PhysicalField
	loc.Bindings = map[string]*FieldBinding{} // pre-populated caps, consumed by finish()
	for name, byType := range caps {
		if strings.HasPrefix(name, "_") {
			continue
		}
		for t, fc := range byType {
			schemaFields = append(schemaFields, PhysicalField{Name: name, Type: strings.ToUpper(t)})
			loc.Bindings["_caps:"+name] = &FieldBinding{Physical: name, PhysType: t, Searchable: fc.Searchable, Aggregatable: fc.Aggregatable}
			break
		}
	}
	sortFields(schemaFields)

	for _, ix := range indices {
		span, ok := daySpan(ix.Name)
		if !ok {
			continue
		}
		docs, _ := strconv.ParseInt(ix.DocsCount, 10, 64)
		size, _ := strconv.ParseInt(ix.StoreSize, 10, 64)
		loc.Partitions = append(loc.Partitions, &Partition{Key: ix.Name, Span: span, Rows: docs, Bytes: size, Schema: schemaFields})
	}
	rep.Partitions = len(loc.Partitions)
	rep.PartitionsRead = len(loc.Partitions)

	// Observed time bounds refine the coverage derived from index names.
	if len(loc.Partitions) > 0 {
		resp, err := c.Search(ctx, []string{pattern}, map[string]any{
			"size": 0, "track_total_hits": false,
			"aggs": map[string]any{"tmin": map[string]any{"min": map[string]any{"field": "time"}}, "tmax": map[string]any{"max": map[string]any{"field": "time"}}},
		})
		if err == nil {
			var aggs struct {
				Tmin struct{ Value *float64 } `json:"tmin"`
				Tmax struct{ Value *float64 } `json:"tmax"`
			}
			if json.Unmarshal(resp.Aggregations, &aggs) == nil && aggs.Tmin.Value != nil && aggs.Tmax.Value != nil {
				first, last := loc.Partitions[0], loc.Partitions[len(loc.Partitions)-1]
				first.RowGroups = []RowGroup{{Rows: first.Rows, TimeMin: time.UnixMilli(int64(*aggs.Tmin.Value)).UTC()}}
				last.RowGroups = append(last.RowGroups, RowGroup{TimeMax: time.UnixMilli(int64(*aggs.Tmax.Value)).UTC()})
			}
		}
	}

	drift, err := finish(loc, src, prev, time.Now().UTC())
	if err != nil {
		return nil, nil, err
	}
	rep.Drift = drift
	rep.Duration = time.Since(start)
	return loc, rep, nil
}

func sortFields(f []PhysicalField) {
	for i := 1; i < len(f); i++ {
		for j := i; j > 0 && f[j].Name < f[j-1].Name; j-- {
			f[j], f[j-1] = f[j-1], f[j]
		}
	}
}

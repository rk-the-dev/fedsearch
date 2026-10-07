package catalog

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rksurwase/fedsearch/internal/config"
	"github.com/rksurwase/fedsearch/internal/schema"
	"github.com/rksurwase/fedsearch/internal/timex"
)

const day = 24 * time.Hour

// Discoverer learns one physical location of one dataset from its source.
// prev is the same location from the last refresh (nil on first run); a
// discoverer reuses unchanged partitions from it, keyed by ETag.
type Discoverer interface {
	Discover(ctx context.Context, src config.Source, dataset string, prev *Location) (*Location, *LocationReport, error)
}

// LocationReport is the evidence a refresh leaves behind: how much work it did.
type LocationReport struct {
	Location        string        `json:"location"`
	Partitions      int           `json:"partitions"`
	PartitionsRead  int           `json:"partitions_read"` // metadata actually fetched (cache misses)
	FilesRead       int           `json:"files_read"`
	MetadataBytes   int64         `json:"metadata_bytes"` // bytes fetched to learn schema/stats
	FileBytes       int64         `json:"file_bytes"`     // total size of the files whose metadata was read
	MetadataCalls   int64         `json:"metadata_calls"`
	Duration        time.Duration `json:"duration_ns"`
	Drift           []DriftEvent  `json:"drift,omitempty"`
	SampledFromPart string        `json:"sampled_from,omitempty"`
}

func discovererFor(kind string) (Discoverer, error) {
	switch kind {
	case "parquet":
		return parquetDiscoverer{}, nil
	case "ndjson":
		return ndjsonDiscoverer{}, nil
	case "opensearch":
		return opensearchDiscoverer{}, nil
	}
	return nil, fmt.Errorf("no discoverer for kind %q", kind)
}

var dateInName = regexp.MustCompile(`(\d{4})[-.](\d{2})[-.](\d{2})`)

// daySpan extracts a date from a partition key such as "dt=2026-09-10" or
// "ocsf-authentication-2026.09.10" and returns that UTC day.
func daySpan(key string) (timex.Range, bool) {
	m := dateInName.FindStringSubmatch(key)
	if m == nil {
		return timex.Range{}, false
	}
	t, err := time.Parse("2006-01-02", m[1]+"-"+m[2]+"-"+m[3])
	if err != nil {
		return timex.Range{}, false
	}
	return timex.New(t, t.Add(day)), true
}

// finish turns discovered partitions into a complete location: bindings
// against the declared contract, unmapped columns, coverage, totals, drift.
func finish(loc *Location, src config.Source, prev *Location, now time.Time) ([]DriftEvent, error) {
	sort.Slice(loc.Partitions, func(i, j int) bool { return loc.Partitions[i].Span.From.Before(loc.Partitions[j].Span.From) })
	loc.Tier, loc.Preference, loc.Source, loc.Kind = src.Tier, src.Preference, src.Name, src.Kind
	loc.Cost = CostModel{Kind: src.Cost.Kind, Rate: src.Cost.Rate}
	if loc.Cost.Kind == "" {
		loc.Cost.Kind = "free"
	}

	// Union schema across partitions; record drift where partitions differ.
	union := map[string]string{}
	var order []string
	var drift []DriftEvent
	for _, p := range loc.Partitions {
		for _, f := range p.Schema {
			old, seen := union[f.Name]
			if !seen {
				if len(order) > 0 && p != loc.Partitions[0] {
					drift = append(drift, DriftEvent{loc.ID, p.Key, "new column " + f.Name + " (" + f.Type + ")", now})
				}
				union[f.Name] = f.Type
				order = append(order, f.Name)
			} else if old != f.Type {
				drift = append(drift, DriftEvent{loc.ID, p.Key, fmt.Sprintf("column %s changed type %s -> %s", f.Name, old, f.Type), now})
				union[f.Name] = widen(old, f.Type)
			}
		}
	}

	// Bind declared OCSF fields to physical columns that actually exist.
	pre := loc.Bindings // capability hints a discoverer may have set
	loc.Bindings = map[string]*FieldBinding{}
	declared := map[string]bool{}
	for _, d := range declaredBindings(loc.Dataset) {
		declared[d.physical] = true
		pt, ok := union[d.physical]
		if !ok {
			continue
		}
		b := &FieldBinding{Physical: d.physical, PhysType: pt, Enum: d.enum, Provenance: "declared", Searchable: true, Aggregatable: true}
		if prevB := capsOverride(pre, d.physical); prevB != nil {
			b.Searchable, b.Aggregatable = prevB.Searchable, prevB.Aggregatable
		}
		loc.Bindings[d.path] = b
	}
	loc.Unmapped = nil
	for _, name := range order {
		if !declared[name] {
			loc.Unmapped = append(loc.Unmapped, name)
		}
	}

	// Coverage: partition spans widened by observed min/max event times.
	loc.Rows, loc.Bytes = 0, 0
	var cov timex.Range
	for i, p := range loc.Partitions {
		loc.Rows += p.Rows
		loc.Bytes += p.Bytes
		span := p.Span
		for _, rg := range p.RowGroups {
			if !rg.TimeMin.IsZero() && rg.TimeMin.Before(span.From) {
				span.From = rg.TimeMin
			}
			if !rg.TimeMax.IsZero() && !rg.TimeMax.Before(span.To) {
				span.To = rg.TimeMax.Add(time.Millisecond)
			}
		}
		if i == 0 || span.From.Before(cov.From) {
			cov.From = span.From
		}
		if i == 0 || span.To.After(cov.To) {
			cov.To = span.To
		}
	}
	if loc.Rows > 0 {
		loc.AvgRowSize = float64(loc.Bytes) / float64(loc.Rows)
	}
	loc.Coverage = Coverage{Range: cov, Live: src.Live, Complete: !src.Live, VerifiedAt: now, GranularitySec: int64(day / time.Second)}

	loc.SchemaVer = 1
	if prev != nil {
		loc.SchemaVer = prev.SchemaVer
		if schemaChanged(prev, loc) {
			loc.SchemaVer++
		}
	}
	return drift, nil
}

// capsOverride lets a discoverer pre-populate searchable/aggregatable flags
// (OpenSearch field_caps) before bindings are built.
func capsOverride(hints map[string]*FieldBinding, physical string) *FieldBinding {
	for _, b := range hints {
		if b.Physical == physical {
			return b
		}
	}
	return nil
}

func schemaChanged(prev, cur *Location) bool {
	key := func(l *Location) string {
		var parts []string
		for p, b := range l.Bindings {
			parts = append(parts, p+"="+b.Physical+":"+b.PhysType)
		}
		parts = append(parts, l.Unmapped...)
		sort.Strings(parts)
		return strings.Join(parts, ",")
	}
	return key(prev) != key(cur)
}

// widen picks the more general of two physical types (int -> string, etc.).
func widen(a, b string) string {
	if a == b {
		return a
	}
	num := func(t string) bool {
		t = strings.ToUpper(t)
		return strings.Contains(t, "INT") || strings.Contains(t, "LONG") || strings.Contains(t, "DOUBLE") || strings.Contains(t, "FLOAT")
	}
	if num(a) && num(b) {
		return "DOUBLE"
	}
	return "STRING"
}

// requiredRoles checks that time and identity are bound; without them the
// planner cannot slice and the merge layer cannot dedup.
func requiredRoles(ds *Dataset, loc *Location) error {
	for _, f := range ds.Fields {
		if (f.Role == schema.RoleEventTime || f.Role == schema.RoleEventID) && loc.Bindings[f.Path] == nil {
			return fmt.Errorf("%s: physical data has no column for %s (%s)", loc.ID, f.Path, f.Role)
		}
	}
	return nil
}

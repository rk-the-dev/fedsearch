// Package catalog is the system's map of the world: which logical datasets
// exist, where each physical copy lives, which time range each copy covers,
// how physical fields map to OCSF paths, what each engine can do, and how big
// the data is. Every other layer plans from it.
//
// Central idea: one logical dataset, many physical locations.
package catalog

import (
	"fmt"
	"sort"
	"time"

	"github.com/rksurwase/fedsearch/internal/schema"
	"github.com/rksurwase/fedsearch/internal/timex"
)

type Catalog struct {
	Version     int                 `json:"version"`
	RefreshedAt time.Time           `json:"refreshed_at"`
	Datasets    map[string]*Dataset `json:"datasets"`
	Locations   []*Location         `json:"locations"`
	Drift       []DriftEvent        `json:"drift,omitempty"`
}

// Dataset is a logical OCSF class, independent of where it is stored.
type Dataset struct {
	Name      string   `json:"name"`
	ClassUID  int      `json:"class_uid"`
	ClassName string   `json:"class_name"`
	Fields    []*Field `json:"fields"`
}

type Field struct {
	Path        string           `json:"path"` // OCSF path, e.g. src_endpoint.ip
	Type        schema.FieldType `json:"type"`
	Role        schema.Role      `json:"role"`
	Description string           `json:"description"`
	Enum        map[int64]string `json:"enum,omitempty"` // OCSF id -> caption
	Stats       *FieldStats      `json:"stats,omitempty"`
}

// Location is one physical copy of a dataset in one engine.
type Location struct {
	ID         string                   `json:"id"` // <source>.<dataset>
	Dataset    string                   `json:"dataset"`
	Source     string                   `json:"source"`
	Kind       string                   `json:"kind"` // parquet | ndjson | opensearch
	Tier       string                   `json:"tier"`
	Preference int                      `json:"preference"`
	Layout     Layout                   `json:"layout"`
	Coverage   Coverage                 `json:"coverage"`
	Bindings   map[string]*FieldBinding `json:"bindings"` // OCSF path -> physical
	Unmapped   []string                 `json:"unmapped,omitempty"`
	Caps       Capabilities             `json:"capabilities"`
	Cost       CostModel                `json:"cost"`
	Partitions []*Partition             `json:"partitions"`
	SchemaVer  int                      `json:"schema_version"`
	Rows       int64                    `json:"rows"`
	Bytes      int64                    `json:"bytes"`
	AvgRowSize float64                  `json:"avg_row_bytes"`
}

type Layout struct {
	Root    string `json:"root,omitempty"`    // parquet/ndjson: dir or s3:// URI of this dataset
	Pattern string `json:"pattern,omitempty"` // e.g. dt=YYYY-MM-DD or ocsf-authentication-YYYY.MM.DD
}

type Coverage struct {
	timex.Range
	Live           bool      `json:"live"`     // still ingesting: To tracks now
	Complete       bool      `json:"complete"` // false while the newest partition may still fill
	VerifiedAt     time.Time `json:"verified_at"`
	GranularitySec int64     `json:"granularity_sec"`
}

func (c Coverage) Granularity() time.Duration { return time.Duration(c.GranularitySec) * time.Second }

// Effective returns the coverage the planner should use at time now.
func (c Coverage) Effective(now time.Time) timex.Range {
	r := c.Range
	if c.Live && now.After(r.To) {
		r.To = now
	}
	return r
}

type FieldBinding struct {
	Physical     string           `json:"physical"`
	PhysType     string           `json:"physical_type"`
	Enum         map[string]int64 `json:"enum,omitempty"` // physical value -> OCSF id
	Provenance   string           `json:"provenance"`     // declared | inferred
	Searchable   bool             `json:"searchable"`
	Aggregatable bool             `json:"aggregatable"`
}

// ToPhysical maps an OCSF literal to the physical literal (enum-aware).
func (b *FieldBinding) ToPhysical(v any) (any, bool) {
	if b.Enum == nil {
		return v, true
	}
	var id int64
	switch x := v.(type) {
	case int64:
		id = x
	case int:
		id = int64(x)
	case float64:
		id = int64(x)
	default:
		return nil, false
	}
	for phys, n := range b.Enum {
		if n == id {
			return phys, true
		}
	}
	return nil, false
}

// FromPhysical maps a physical value back to its OCSF value.
func (b *FieldBinding) FromPhysical(v any) any {
	if b.Enum == nil || v == nil {
		return v
	}
	if id, ok := b.Enum[fmt.Sprint(v)]; ok {
		return id
	}
	return int64(99) // OCSF "Other"
}

// Capabilities describe what an engine can evaluate natively.
type Capabilities struct {
	Engine        string   `json:"engine"`
	Dialect       string   `json:"dialect"`        // duckdb_sql | opensearch_dsl
	UnsupportedOp []string `json:"unsupported_op"` // ops the engine cannot push down
	Aggregations  bool     `json:"aggregations"`
}

func (c Capabilities) Supports(op string) bool {
	for _, o := range c.UnsupportedOp {
		if o == op {
			return false
		}
	}
	return true
}

type CostModel struct {
	Kind string  `json:"kind"` // per_tb_scanned | per_gb_scanned | free
	Rate float64 `json:"rate"`
}

// Partition is a unit of storage: a dt= directory/file or a daily index.
type Partition struct {
	Key         string           `json:"key"`
	Span        timex.Range      `json:"span"`
	Objects     []string         `json:"objects,omitempty"`
	Rows        int64            `json:"rows"`
	Bytes       int64            `json:"bytes"`
	ColumnBytes map[string]int64 `json:"column_bytes,omitempty"`
	RowGroups   []RowGroup       `json:"row_groups,omitempty"`
	ETag        string           `json:"etag,omitempty"`
	Schema      []PhysicalField  `json:"schema,omitempty"`
	Stale       bool             `json:"stale,omitempty"` // listed live, not yet in the catalog: stats estimated
}

type RowGroup struct {
	Rows        int64            `json:"rows"`
	TimeMin     time.Time        `json:"time_min"`
	TimeMax     time.Time        `json:"time_max"`
	ColumnBytes map[string]int64 `json:"column_bytes"`
}

type PhysicalField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type FieldStats struct {
	SampledRows int64    `json:"sampled_rows"`
	Distinct    int64    `json:"distinct"`
	TopValues   []string `json:"top_values,omitempty"`
	Examples    []Sample `json:"examples,omitempty"`
	NullFrac    float64  `json:"null_frac"`
}

type Sample struct {
	Value      string `json:"value"`
	Suspicious bool   `json:"suspicious,omitempty"`
}

type DriftEvent struct {
	Location  string    `json:"location"`
	Partition string    `json:"partition"`
	Change    string    `json:"change"`
	At        time.Time `json:"at"`
}

// --- schema.Lookup implementation -----------------------------------------

func (c *Catalog) HasDataset(name string) bool { _, ok := c.Datasets[name]; return ok }

func (c *Catalog) Field(dataset, path string) (schema.FieldType, schema.Role, bool) {
	if f := c.FieldDef(dataset, path); f != nil {
		return f.Type, f.Role, true
	}
	return "", "", false
}

func (c *Catalog) FieldDef(dataset, path string) *Field {
	d := c.Datasets[dataset]
	if d == nil {
		return nil
	}
	for _, f := range d.Fields {
		if f.Path == path {
			return f
		}
	}
	return nil
}

func (c *Catalog) Fields(dataset string) []string {
	d := c.Datasets[dataset]
	if d == nil {
		return nil
	}
	out := make([]string, len(d.Fields))
	for i, f := range d.Fields {
		out[i] = f.Path
	}
	return out
}

// FieldByRole returns the OCSF path playing a role (event_time, event_id).
func (c *Catalog) FieldByRole(dataset string, role schema.Role) string {
	if d := c.Datasets[dataset]; d != nil {
		for _, f := range d.Fields {
			if f.Role == role {
				return f.Path
			}
		}
	}
	return ""
}

// LocationsFor returns a dataset's locations ordered by preference.
func (c *Catalog) LocationsFor(dataset string) []*Location {
	var out []*Location
	for _, l := range c.Locations {
		if l.Dataset == dataset {
			out = append(out, l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Preference < out[j].Preference })
	return out
}

func (c *Catalog) Location(id string) *Location {
	for _, l := range c.Locations {
		if l.ID == id {
			return l
		}
	}
	return nil
}

// Validate checks the invariants the planner relies on.
func (l *Location) Validate(ds *Dataset) error {
	if l.Coverage.IsZero() && len(l.Partitions) > 0 {
		return fmt.Errorf("%s: empty coverage with %d partitions", l.ID, len(l.Partitions))
	}
	for _, f := range ds.Fields {
		if (f.Role == schema.RoleEventTime || f.Role == schema.RoleEventID) && l.Bindings[f.Path] == nil {
			return fmt.Errorf("%s: no binding for %s field %s", l.ID, f.Role, f.Path)
		}
	}
	for i := 1; i < len(l.Partitions); i++ {
		if l.Partitions[i-1].Span.Overlaps(l.Partitions[i].Span) {
			return fmt.Errorf("%s: partitions %s and %s overlap", l.ID, l.Partitions[i-1].Key, l.Partitions[i].Key)
		}
	}
	return nil
}

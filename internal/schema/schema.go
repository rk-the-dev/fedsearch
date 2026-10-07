// Package schema holds the small vocabulary shared by the catalog and the IR:
// logical field types and roles. It is a leaf package so that neither the
// catalog nor the IR has to import the other.
package schema

// FieldType is the logical type of an OCSF field, independent of engine.
type FieldType string

const (
	String    FieldType = "string"
	Int       FieldType = "int"
	Float     FieldType = "float"
	IP        FieldType = "ip"
	Timestamp FieldType = "timestamp"
)

// Ordered reports whether lt/gt comparisons make sense for the type.
func (t FieldType) Ordered() bool { return t == Int || t == Float || t == Timestamp }

// Numeric reports whether sum/avg make sense for the type.
func (t FieldType) Numeric() bool { return t == Int || t == Float }

// Role tells the planner and merge layer what a field means.
type Role string

const (
	RoleEventTime Role = "event_time" // the time column every query filters on
	RoleEventID   Role = "event_id"   // identity used for dedup and citations
	RoleDimension Role = "dimension"  // low/medium cardinality, good for group_by
	RoleMeasure   Role = "measure"    // numeric, good for sum/avg
	RoleFreeText  Role = "free_text"  // attacker-controllable text; never sampled raw
)

// Lookup answers field questions for one dataset. The catalog implements it;
// the IR validator consumes it.
type Lookup interface {
	HasDataset(dataset string) bool
	Field(dataset, path string) (FieldType, Role, bool)
	Fields(dataset string) []string
}

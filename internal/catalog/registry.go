package catalog

import "github.com/rksurwase/fedsearch/internal/schema"

// The registry declares the logical datasets (OCSF classes) and the physical
// layout contract the ingestion pipeline publishes: OCSF paths flattened to
// column names, plus enum transforms where the pipeline kept vendor captions.
// In production this contract comes from the pipeline's mapping metadata;
// discovery then confirms which bindings each physical location actually has.

type fieldDecl struct {
	path, physical string
	typ            schema.FieldType
	role           schema.Role
	desc           string
	enum           map[string]int64 // physical caption -> OCSF id
	captions       map[int64]string
}

type datasetDecl struct {
	name, className string
	classUID        int
	fields          []fieldDecl
}

var (
	statusEnum   = map[string]int64{"success": 1, "failure": 2}
	statusCap    = map[int64]string{0: "Unknown", 1: "Success", 2: "Failure", 99: "Other"}
	actionEnum   = map[string]int64{"allowed": 1, "blocked": 2}
	actionCap    = map[int64]string{0: "Unknown", 1: "Allowed", 2: "Denied", 99: "Other"}
	severityCap  = map[int64]string{0: "Unknown", 1: "Informational", 2: "Low", 3: "Medium", 4: "High", 5: "Critical", 6: "Fatal"}
	commonFields = []fieldDecl{
		{"time", "time", schema.Timestamp, schema.RoleEventTime, "Event time (UTC)", nil, nil},
		{"metadata.uid", "event_id", schema.String, schema.RoleEventID, "Unique event identifier stamped at ingestion", nil, nil},
		{"class_uid", "class_uid", schema.Int, schema.RoleDimension, "OCSF class (3002 Authentication, 4001 Network Activity)", nil, nil},
	}
)

var registry = []datasetDecl{
	{"authentication", "Authentication", 3002, append(append([]fieldDecl{}, commonFields...),
		fieldDecl{"status_id", "status", schema.Int, schema.RoleDimension, "Outcome of the logon attempt", statusEnum, statusCap},
		fieldDecl{"user.name", "user_name", schema.String, schema.RoleDimension, "Account that attempted to authenticate", nil, nil},
		fieldDecl{"src_endpoint.ip", "src_endpoint_ip", schema.IP, schema.RoleDimension, "IP the logon came from", nil, nil},
		fieldDecl{"dst_endpoint.hostname", "dst_endpoint_hostname", schema.String, schema.RoleDimension, "Host or service being logged into", nil, nil},
		fieldDecl{"auth_protocol", "auth_protocol", schema.String, schema.RoleDimension, "Authentication protocol (kerberos, ntlm, ldap, saml, oidc)", nil, nil},
		fieldDecl{"http_request.user_agent", "http_user_agent", schema.String, schema.RoleFreeText, "Client user agent (attacker-controllable)", nil, nil},
		fieldDecl{"severity_id", "severity_id", schema.Int, schema.RoleDimension, "Event severity", nil, severityCap},
	)},
	{"network_activity", "Network Activity", 4001, append(append([]fieldDecl{}, commonFields...),
		fieldDecl{"src_endpoint.ip", "src_endpoint_ip", schema.IP, schema.RoleDimension, "Connection source IP", nil, nil},
		fieldDecl{"dst_endpoint.ip", "dst_endpoint_ip", schema.IP, schema.RoleDimension, "Connection destination IP", nil, nil},
		fieldDecl{"dst_endpoint.port", "dst_endpoint_port", schema.Int, schema.RoleDimension, "Destination port", nil, nil},
		fieldDecl{"connection_info.protocol_name", "connection_protocol", schema.String, schema.RoleDimension, "Transport protocol", nil, nil},
		fieldDecl{"traffic.bytes_out", "traffic_bytes_out", schema.Int, schema.RoleMeasure, "Bytes sent by the source", nil, nil},
		fieldDecl{"traffic.bytes_in", "traffic_bytes_in", schema.Int, schema.RoleMeasure, "Bytes received by the source", nil, nil},
		fieldDecl{"action_id", "action", schema.Int, schema.RoleDimension, "Firewall decision", actionEnum, actionCap},
	)},
}

// NewDatasets returns the logical datasets from the registry.
func NewDatasets() map[string]*Dataset {
	out := map[string]*Dataset{}
	for _, d := range registry {
		ds := &Dataset{Name: d.name, ClassUID: d.classUID, ClassName: d.className}
		for _, f := range d.fields {
			ds.Fields = append(ds.Fields, &Field{Path: f.path, Type: f.typ, Role: f.role, Description: f.desc, Enum: f.captions})
		}
		out[d.name] = ds
	}
	return out
}

// declaredBindings returns the pipeline's physical contract for a dataset.
func declaredBindings(dataset string) []fieldDecl {
	for _, d := range registry {
		if d.name == dataset {
			return d.fields
		}
	}
	return nil
}

// KnownDatasets lists registry dataset names.
func KnownDatasets() []string {
	out := make([]string, len(registry))
	for i, d := range registry {
		out[i] = d.name
	}
	return out
}

# Module 0: OCSF from first principles

OCSF (Open Cybersecurity Schema Framework) is the vendor-neutral schema that DataBahn's pipeline normalizes into. Every later component of this POC (catalog, IR, compilers) speaks OCSF.

## 1. Why it exists

Every vendor logs the same fact differently:

```
sshd:     Failed password for invalid user admin from 185.220.101.47 port 51234 ssh2
Windows:  EventID=4625  IpAddress=185.220.101.47  TargetUserName=admin  LogonType=10
Okta:     {"eventType":"user.session.start","outcome":{"result":"FAILURE"},"client":{"ipAddress":"185.220.101.47"}}
```

OCSF gives each kind of event one canonical shape. AWS Security Lake stores OCSF natively. The current release is 1.9.0 (August 2026), and the schema evolves quickly, which makes version drift a real problem.

## 2. Building blocks (bottom-up)

```
Data types    → string, int, timestamp_t, ip_t, port_t
Attributes    → one global dictionary: ip, port, time, status_id
Objects       → reusable groups: endpoint{ip, port, hostname}, user{name, uid}
Event classes → Authentication, Network Activity, ...
Categories    → IAM, Network Activity, Findings, ...
Profiles      → optional attribute bundles: host, cloud, network_proxy
Extensions    → vendor/org additions without forking
```

Attributes are defined once, globally. `ip` means the same thing inside `src_endpoint`, `dst_endpoint` or `device`.

## 3. Numbering

```
class_uid = category_uid × 1000 + class number
type_uid  = class_uid × 100 + activity_id
```

| Category | Example classes |
|---|---|
| 1 System Activity | File System Activity (1001), Process Activity (1007) |
| 2 Findings | Detection Finding |
| 3 Identity & Access Management | **Authentication (3002)**, Account Change (3001) |
| 4 Network Activity | **Network Activity (4001)**, HTTP Activity (4002), DNS Activity (4003) |
| 5 Discovery | Device Inventory Info |
| 6 Application Activity | API Activity |

A failed logon is Authentication (3002) with Logon (`activity_id` 1), so `type_uid` = 300201. Integer filters are cheap and unambiguous.

## 4. The `_id` + caption convention

| Attribute | Meaning |
|---|---|
| `status_id` | 0 Unknown, 1 Success, 2 Failure, **99 Other** |
| `status` | Caption, e.g. "Failure" |
| `severity_id` | 0 Unknown, 1 Informational, 2 Low, 3 Medium, 4 High, 5 Critical, 6 Fatal, 99 Other |

- Query on `_id`, display the caption.
- `99` (Other) means the vendor value didn't fit. The string sibling then carries the raw value, so nothing is lost.

## 5. Anatomy of one event

```json
{
  "class_uid": 3002, "class_name": "Authentication", "category_uid": 3,
  "activity_id": 1, "activity_name": "Logon", "type_uid": 300201,
  "time": 1759803612000, "severity_id": 3,
  "status_id": 2, "status": "Failure", "status_detail": "Failed password for invalid user",
  "user": { "name": "admin" },
  "src_endpoint": { "ip": "185.220.101.47", "port": 51234 },
  "dst_endpoint": { "hostname": "bastion-01" },
  "auth_protocol": "SSH",
  "metadata": { "version": "1.9.0", "product": { "name": "OpenSSH", "vendor_name": "OpenBSD" },
                "original_time": "Oct  7 02:20:12", "uid": "evt-7f3a…" },
  "observables": [
    { "name": "src_endpoint.ip", "type_id": 2, "value": "185.220.101.47" },
    { "name": "user.name", "type_id": 4, "value": "admin" }
  ],
  "unmapped": { "invalid_user": true },
  "raw_data": "Failed password for invalid user admin from 185.220.101.47 port 51234 ssh2"
}
```

- **metadata**: the producing product, the OCSF version, and event identity (`uid`), which is the dedup key.
- **observables**: a typed, flat index of interesting values across every class. "Find this IP anywhere" can search observables instead of knowing each class's field path.
- **unmapped**: vendor fields with no OCSF home, kept rather than dropped.
- **raw_data**: the original line, kept for forensics and re-parsing.
- Each attribute is *required*, *recommended* or *optional* per class.

## 6. Where OCSF gets hard

1. Field mapping across thousands of vendor formats (DataBahn's Cruz product automates this).
2. The unmapped-field problem: valuable fields stranded where detections never look.
3. Schema drift, from both vendors and OCSF versions.
4. Nested vs flat storage: many systems flatten `src_endpoint.ip` → `src_endpoint_ip`, so the catalog must remember the mapping back.
5. Partial adoption: real environments mix fully normalized, partially normalized and raw sources.

## 7. How it connects to federated search

```
"failed logins from 185.220.101.47"
      │
IR:   class_uid=3002 AND status_id=2 AND src_endpoint.ip='185.220.101.47'
      ├─► Cold Parquet (flat):    WHERE class_uid=3002 AND status_id=2 AND src_endpoint_ip=…
      ├─► OpenSearch (nested):    bool.filter: term src_endpoint.ip …
      └─► Raw BYO bucket:         catalog says IpAddress → src_endpoint.ip,
                                  EventID 4625 → class 3002 + status 2
```

OCSF is the lingua franca of the IR. The catalog holds each source's mapping to OCSF, and the compilers translate from it.

## Exercises

**A. Read the source.** Clone `github.com/ocsf/ocsf-schema` and read:

- `dictionary.json`
- `events/iam/authentication.json`, `events/network/network_activity.json`
- `objects/network_endpoint.json`, `objects/user.json`, `objects/metadata.json`

**B. Answer:**

1. Authentication's `activity_id` values, and what `logon_type_id` is for.
2. Network Activity's `activity_id` values. Which fits a single firewall "allowed" flow record?
3. Which base-event attributes are required for every class?
4. How does a class inherit from `base_event`, and how does a profile add attributes?

**C. Hand-map to OCSF JSON:**

```
Windows: EventID=4624 TimeCreated=2026-09-10T02:31:00Z TargetUserName=svc_backup
         IpAddress=10.20.4.17 WorkstationName=LT-ANKIT-042 LogonType=3
         AuthenticationPackageName=NTLM TargetServerName=db-prod-01

Firewall CSV: 2026-09-27T03:05:00Z,allow,tcp,10.20.6.88,49812,185.220.101.47,443,bytes_sent=134217728,bytes_recv=20480
```

Pay attention to what goes in `observables`, what goes in `unmapped`, and how `LogonType=3` maps.

## References

- [OCSF schema releases](https://github.com/ocsf/ocsf-schema/releases)
- [What's new in OCSF 1.4.0 (Query.ai)](https://www.query.ai/resources/blogs/whats-new-ocsf-1_4_0/)
- [Tenzir OCSF reference](https://docs.tenzir.com/reference/ocsf)

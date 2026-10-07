-- Reef stand-in: a temporal context store.
-- The key idea: an IP→host fact is only true during a time interval, and the
-- database itself guarantees an IP never belongs to two hosts at once.

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE hosts (
    hostname    text PRIMARY KEY,
    owner       text NOT NULL,
    department  text NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('laptop', 'server')),
    criticality text NOT NULL CHECK (criticality IN ('low', 'medium', 'high'))
);

CREATE TABLE ip_assignments (
    ip         inet        NOT NULL,
    hostname   text        NOT NULL REFERENCES hosts,
    valid_from timestamptz NOT NULL,
    valid_to   timestamptz,               -- NULL = still assigned
    validity   tstzrange GENERATED ALWAYS AS
               (tstzrange(valid_from, coalesce(valid_to, 'infinity'), '[)')) STORED,
    -- No IP may have two overlapping assignments.
    EXCLUDE USING gist (ip WITH =, validity WITH &&)
);

COPY hosts FROM '/seed/hosts.csv' WITH (FORMAT csv, HEADER true);
COPY ip_assignments (ip, hostname, valid_from, valid_to)
    FROM '/seed/ip_assignments.csv' WITH (FORMAT csv, HEADER true);

-- As-of lookup: who held this IP at event time? (The correct join.)
CREATE FUNCTION host_at(p_ip inet, p_at timestamptz)
RETURNS TABLE (hostname text, owner text, department text, criticality text)
LANGUAGE sql STABLE AS $$
    SELECT h.hostname, h.owner, h.department, h.criticality
    FROM ip_assignments a JOIN hosts h USING (hostname)
    WHERE a.ip = p_ip AND a.validity @> p_at
$$;

-- Current owner view: what a naive enrichment uses. Kept to demonstrate the trap.
CREATE VIEW current_ip_owner AS
SELECT a.ip, h.hostname, h.owner
FROM ip_assignments a JOIN hosts h USING (hostname)
WHERE a.valid_to IS NULL;

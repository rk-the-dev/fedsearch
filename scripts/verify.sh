#!/bin/sh
# Day 1 acceptance checks against the running stack. Every number printed
# here should match out/ground_truth.json.
set -eu
DC="docker compose -f deploy/docker-compose.yml"
OS=http://localhost:9200

echo "== hot tier (OpenSearch) =="
for ds in authentication network_activity; do
  printf '%-17s docs: ' "$ds"; curl -fsS "$OS/_cat/count/ocsf-$ds-*?h=count"
done
printf 'exfil flows to attacker (expect 30): '
curl -fsS "$OS/ocsf-network_activity-*/_count" -H 'Content-Type: application/json' \
  -d '{"query":{"term":{"dst_endpoint_ip":"185.220.101.47"}}}' | sed 's/.*"count":\([0-9]*\).*/\1/'; echo

echo "== cold tier (MinIO) =="
$DC run --rm -T --entrypoint /bin/sh minio-init -c '
  mc alias set s3 http://minio:9000 fedsearch fedsearch-secret >/dev/null
  for ds in authentication network_activity; do
    echo "$ds parquet files: $(mc ls --recursive s3/telemetry-archive/ocsf/$ds/ | wc -l)"
  done'

echo "== context (Postgres) =="
$DC exec -T postgres psql -U fedsearch -d reef -c \
  "SELECT 'as-of (correct)' AS lookup, * FROM host_at('10.20.4.17', now() - interval '27 days')
   UNION ALL
   SELECT 'current (trap)', hostname, owner, NULL, NULL FROM current_ip_owner WHERE ip = '10.20.4.17';"

echo "== expected =="
grep -A6 '"counts"' out/ground_truth.json

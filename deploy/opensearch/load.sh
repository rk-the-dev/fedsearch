#!/bin/sh
# Loads the hot tier into OpenSearch: one daily index per dataset, like a SIEM.
set -eu
OS="${OS_URL:-http://opensearch:9200}"

echo "installing index template"
curl -fsS -X PUT "$OS/_index_template/ocsf" -H 'Content-Type: application/json' \
  --data-binary @/init/template.json >/dev/null

failed=0
for f in /seed/*/bulk-*.ndjson; do
  resp=$(curl -fsS -X POST "$OS/_bulk" -H 'Content-Type: application/x-ndjson' --data-binary "@$f")
  case "$resp" in
    *'"errors":false'*) echo "loaded $(basename "$(dirname "$f")")/$(basename "$f")" ;;
    *) echo "ERRORS in $f: $(echo "$resp" | head -c 400)"; failed=1 ;;
  esac
done

curl -fsS -X POST "$OS/ocsf-*/_refresh" >/dev/null
echo "document counts:"
curl -fsS "$OS/_cat/count/ocsf-authentication-*?v"
curl -fsS "$OS/_cat/count/ocsf-network_activity-*?v"
exit $failed

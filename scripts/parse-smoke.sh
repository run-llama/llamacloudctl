#!/usr/bin/env bash
# parse-smoke.sh — end-to-end parse smoke test against a running LlamaCloud install.
#
# It uploads a document, polls the parse job to completion, and reads back the
# markdown. This exercises the whole path a health check does not: object storage
# upload (the s3proxy shim), the parse pipeline, Temporal, and the database. A green
# health check with broken object storage still fails here.
#
# Usage:
#   BASE_URL=http://localhost:8000 LLAMA_CLOUD_API_KEY=llx-... scripts/parse-smoke.sh [file]
#
#   BASE_URL              API base URL of the install (port-forward the API service,
#                         or the ingress host). Required.
#   LLAMA_CLOUD_API_KEY   A project API key. Create one in the UI (Settings > API
#                         Keys) after logging in. Required.
#   file                  Optional path to a document. Defaults to the bundled
#                         single-page test PDF.
#
# Tunables (env): TIER (default fast, the rule-based tier that needs no LLM key),
# TIMEOUT (default 300s), POLL_INTERVAL (default 5s).
set -euo pipefail

BASE_URL="${BASE_URL:?set BASE_URL to the API base URL of the install}"
API_KEY="${LLAMA_CLOUD_API_KEY:?set LLAMA_CLOUD_API_KEY to a project API key}"
BASE_URL="${BASE_URL%/}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FILE="${1:-$SCRIPT_DIR/testdata/parse-smoke.pdf}"
TIER="${TIER:-fast}"
TIMEOUT="${TIMEOUT:-300}"
POLL_INTERVAL="${POLL_INTERVAL:-5}"

[ -f "$FILE" ] || { echo "file not found: $FILE" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

auth=(-H "Authorization: Bearer $API_KEY")
api="$BASE_URL/api/v2/parse"

# The v2 upload requires a configuration with a tier and a concrete version. Pull the
# latest published version for the tier from the server so this does not go stale.
echo "==> resolving latest $TIER parse version"
version=$(curl -fsS "$api/versions" "${auth[@]}" | jq -r --arg t "$TIER" '.[$t][0] // empty')
[ -n "$version" ] || { echo "no versions available for tier $TIER" >&2; exit 1; }
config=$(jq -nc --arg t "$TIER" --arg v "$version" '{tier:$t, version:$v}')
echo "    tier=$TIER version=$version"

echo "==> uploading $(basename "$FILE") to $BASE_URL"
upload=$(curl -fsS -X POST "$api/upload" "${auth[@]}" \
  -F "configuration=$config" -F "file=@$FILE" ) || { echo "upload failed" >&2; exit 1; }

job_id=$(echo "$upload" | jq -r '.id // empty')
[ -n "$job_id" ] || { echo "no job id in upload response: $upload" >&2; exit 1; }
echo "    job: $job_id"

echo "==> polling for up to ${TIMEOUT}s"
deadline=$(( $(date +%s) + TIMEOUT ))
status=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  status=$(curl -fsS "$api/$job_id" "${auth[@]}" | jq -r '.job.status // empty')
  case "$status" in
    SUCCESS|PARTIAL_SUCCESS|COMPLETED) echo "    status: $status"; break ;;
    ERROR|CANCELLED|FAILED)            echo "    status: $status" >&2; exit 1 ;;
    *)                                 printf '    %s ...\n' "${status:-<no status>}"; sleep "$POLL_INTERVAL" ;;
  esac
done

case "$status" in
  SUCCESS|PARTIAL_SUCCESS|COMPLETED) ;;
  *) echo "job did not reach a terminal success state within ${TIMEOUT}s (last: ${status:-none})" >&2; exit 1 ;;
esac

echo "==> fetching markdown result"
# The v2 result nests markdown per page: .markdown.pages[].markdown. Join the pages.
markdown=$(curl -fsS "$api/$job_id?expand=markdown" "${auth[@]}" \
  | jq -r '[.markdown.pages[]?.markdown] | join("\n\n")')

if [ -z "$markdown" ] || [ "${#markdown}" -lt 5 ]; then
  echo "FAIL: the result markdown is empty — the parse ran but produced nothing, which is what a broken object-storage backend looks like" >&2
  exit 1
fi

echo "--- markdown (first 5 lines) ---"
echo "$markdown" | head -5
echo "--------------------------------"
echo "PASS: parsed $(basename "$FILE") and got ${#markdown} characters of markdown."

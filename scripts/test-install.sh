#!/usr/bin/env bash
#
# test-install.sh — interactive driver for `llamacloudctl install`.
#
# Everything that stays the same between test runs is embedded below as
# CONSTANTS (DB endpoints, cloud/region/release, install flags). Each run only
# prompts for the values you are actually testing:
#
#     • Namespace          (-n, the thing you vary most)
#     • BYOC license key    -> license.key
#     • OpenAI API key      -> config.llms.openAi.apiKey
#     • OIDC discovery URL  -> config.authentication.oidc.discoveryUrl
#     • OIDC client ID      -> config.authentication.oidc.clientId
#     • OIDC client secret  -> config.authentication.oidc.clientSecret   (the "token")
#
# It writes a complete values file from CONSTANTS + your answers, then runs:
#
#     llamacloudctl install -n <namespace> -f <generated-values> --non-interactive --yes [flags]
#
# Your last answers are remembered and offered as defaults next time, so a
# repeat run into a new namespace is usually just: type namespace, Enter, Enter…
#
# Usage:
#     scripts/test-install.sh                 # prompt for the variables
#     scripts/test-install.sh -n my-ns        # pre-seed the namespace, still prompts
#     scripts/test-install.sh --defaults      # reuse every saved answer, no prompts
#     scripts/test-install.sh --defaults -n x # same secrets, new namespace (fast loop)
#     scripts/test-install.sh --reset         # forget saved answers
#
# Compatible with bash 3.2 (macOS default) — no associative arrays.
#
set -euo pipefail

# ============================================================================
#  CONSTANTS  — edit these once; they are the same for every test run.
# ============================================================================

# --- install command knobs --------------------------------------------------
BIN=""                                       # resolved below: built from source unless LLAMACLOUDCTL_BIN is set
NAMESPACE_DEFAULT="llamacloud"              # default namespace when you don't pass -n
CLOUD="aws"                                  # --cloud aws|azure|gcp
REGION="us-east-1"                          # --region
RELEASE="llamacloud"                        # --release (Helm release name)
CHART_VERSION=""                             # --chart-version ("" = latest)
STORAGE_CLASS=""                             # --storage-class ("" = cluster default; e.g. gp2)
KUBE_CONTEXT=""                              # --context ("" = current kube context)
DRY_RUN="${DRY_RUN:-n}"                       # y = add --dry-run (render only, no install); env-overridable
SKIP_PERM_CHECK="y"                          # y = add --skip-permission-check

# --- constant values-file fields (NOT the secrets you test) -----------------
# In-cluster / external data services. Adjust to match your test cluster.
PG_HOST="pg.example.com"; PG_PORT="5432"; PG_DB="llamacloud"; PG_USER="llama"; PG_PASSWORD="x"
REDIS_HOST="redis.example.com"; REDIS_PORT="6379"; REDIS_DB="0"; REDIS_PASSWORD="x"
# Temporal is a REQUIRED service in the chart. This points at an external endpoint;
# to deploy Temporal in-cluster instead, drop these and pass --provision-in-cluster.
TEMPORAL_HOST="temporal.example.com"; TEMPORAL_PORT="7233"
STORAGE_PROVIDER="aws"                        # config.storageBuckets.provider

# ============================================================================
#  End of constants — the rest of the script prompts for the variables.
# ============================================================================

CONFIG_FILE="${LLAMACLOUDCTL_TEST_CONFIG:-$HOME/.llamacloudctl-test.env}"
USE_DEFAULTS=0
SEED_NAMESPACE=""

# ---- arg parsing -----------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    -n|--namespace) SEED_NAMESPACE="${2:-}"; shift 2 ;;
    --defaults)     USE_DEFAULTS=1; shift ;;
    --reset)        rm -f "$CONFIG_FILE"; echo "Cleared $CONFIG_FILE"; shift ;;
    -h|--help)
      grep '^#' "$0" | sed 's/^# \{0,1\}//' | sed '/^!/d'
      exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done

# ---- build + resolve the binary --------------------------------------------
# Always build from source and run the freshly-built artifact, so a test run
# never silently exercises a stale llamacloudctl on PATH. Export LLAMACLOUDCTL_BIN
# to skip the build and point at a specific binary instead.
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
if [[ -n "${LLAMACLOUDCTL_BIN:-}" ]]; then
  BIN="$LLAMACLOUDCTL_BIN"
  echo "Using LLAMACLOUDCTL_BIN=$BIN (build skipped)."
else
  echo "Building llamacloudctl from source ($REPO_ROOT)..."
  ( cd "$REPO_ROOT" && make build ) || { echo "error: build failed." >&2; exit 1; }
  BIN="$REPO_ROOT/bin/llamacloudctl"
fi
if [[ ! -x "$BIN" ]]; then
  echo "error: binary '$BIN' not found or not executable." >&2
  exit 1
fi

# ---- load previously saved answers (only the variables) --------------------
if [[ -f "$CONFIG_FILE" ]]; then
  while IFS='=' read -r k v; do
    [[ -z "$k" || "$k" == \#* ]] && continue
    v="${v%\"}"; v="${v#\"}"
    printf -v "SAVED_$k" '%s' "$v"
  done < "$CONFIG_FILE"
fi
def() { local var="SAVED_$1"; if [[ -n "${!var:-}" ]]; then printf '%s' "${!var}"; else printf '%s' "${2:-}"; fi; }

# ---- prompt helper ---------------------------------------------------------
# ask <KEY> "Question" "<default>"  -> sets variable KEY
ask() {
  local key="$1" prompt="$2" default="$3" reply
  if [[ "$USE_DEFAULTS" == "1" ]]; then printf -v "$key" '%s' "$default"; return; fi
  if [[ -n "$default" ]]; then
    read -r -p "$prompt [$default]: " reply
    printf -v "$key" '%s' "${reply:-$default}"
  else
    read -r -p "$prompt: " reply
    printf -v "$key" '%s' "$reply"
  fi
}

echo "=================================================="
echo " llamacloudctl install — variable test run"
echo "=================================================="
echo "Binary : $BIN"
echo "Config : $CONFIG_FILE   (remembers your last answers)"
echo "Constants: cloud=$CLOUD region=$REGION release=$RELEASE dry-run=$DRY_RUN"
echo

# ---- prompt only for the variables -----------------------------------------
NS_DEFAULT="$(def NAMESPACE "$NAMESPACE_DEFAULT")"
[[ -n "$SEED_NAMESPACE" ]] && NS_DEFAULT="$SEED_NAMESPACE"
ask NAMESPACE           "Namespace (-n)"          "$NS_DEFAULT"
ask BYOC_LICENSE        "BYOC license key"        "$(def BYOC_LICENSE '')"
ask OPENAI_API_KEY      "OpenAI API key"          "$(def OPENAI_API_KEY '')"
ask OIDC_DISCOVERY_URL  "OIDC discovery URL"      "$(def OIDC_DISCOVERY_URL '')"
ask OIDC_CLIENT_ID      "OIDC client ID"          "$(def OIDC_CLIENT_ID '')"
ask OIDC_CLIENT_SECRET  "OIDC client secret/token" "$(def OIDC_CLIENT_SECRET '')"

# ---- persist the variable answers ------------------------------------------
{
  echo "# llamacloudctl test-install variables (last run)"
  for k in NAMESPACE BYOC_LICENSE OPENAI_API_KEY OIDC_DISCOVERY_URL OIDC_CLIENT_ID OIDC_CLIENT_SECRET; do
    printf '%s="%s"\n' "$k" "${!k}"
  done
} > "$CONFIG_FILE"
chmod 600 "$CONFIG_FILE" 2>/dev/null || true

# ---- generate the values file ----------------------------------------------
# Constants + the prompted secrets. OIDC is enabled here (exactly one auth
# method may be enabled), so basicAuth is intentionally left off.
VALUES_FILE="${LLAMACLOUDCTL_TEST_VALUES:-$PWD/${NAMESPACE}-testvalues.yaml}"
cat > "$VALUES_FILE" <<YAML
# Generated by scripts/test-install.sh — do not hand-edit; re-run the script.
license:
  key: "${BYOC_LICENSE}"
postgresql:
  { host: ${PG_HOST}, port: "${PG_PORT}", database: ${PG_DB}, username: ${PG_USER}, password: ${PG_PASSWORD} }
redis:
  { host: ${REDIS_HOST}, port: "${REDIS_PORT}", db: ${REDIS_DB}, password: ${REDIS_PASSWORD} }
temporal:
  { deploy: false, host: ${TEMPORAL_HOST}, port: ${TEMPORAL_PORT} }
config:
  llms:
    openAi:
      apiKey: "${OPENAI_API_KEY}"
  authentication:
    oidc:
      enabled: true
      discoveryUrl: "${OIDC_DISCOVERY_URL}"
      clientId: "${OIDC_CLIENT_ID}"
      clientSecret: "${OIDC_CLIENT_SECRET}"
  storageBuckets:
    provider: ${STORAGE_PROVIDER}
YAML

# ---- build the command -----------------------------------------------------
CMD=("$BIN" install -n "$NAMESPACE" -f "$VALUES_FILE" --non-interactive --yes)
add_str()  { [[ -n "$2" ]] && CMD+=("$1" "$2"); return 0; }
add_bool() { [[ "$2" == "y" ]] && CMD+=("$1"); return 0; }
add_str  --cloud          "$CLOUD"
add_str  --region         "$REGION"
add_str  --release        "$RELEASE"
add_str  --chart-version  "$CHART_VERSION"
add_str  --storage-class  "$STORAGE_CLASS"
add_str  --context        "$KUBE_CONTEXT"
add_bool --dry-run              "$DRY_RUN"
add_bool --skip-permission-check "$SKIP_PERM_CHECK"

# ---- preview + confirm -----------------------------------------------------
echo
echo "Values file: $VALUES_FILE"
echo "About to run:"
printf '  '; printf '%q ' "${CMD[@]}"; echo; echo

if [[ "$USE_DEFAULTS" != "1" ]]; then
  read -r -p "Proceed? (y/n) [y]: " go
  case "${go:-y}" in y|Y|yes|YES) ;; *) echo "Aborted."; exit 0 ;; esac
fi

# ---- run -------------------------------------------------------------------
echo "--------------------------------------------------"
set +e
"${CMD[@]}"
rc=$?
set -e
echo "--------------------------------------------------"
echo "Exit code: $rc  (namespace: $NAMESPACE)"
exit "$rc"

#!/usr/bin/env bash
#
# ecs-metering-snapshot — read-only check of how fresh Dell ECS namespace
# metering is, and what it reports per bucket.
#
# It answers one question before you tune ECS_POLL_INTERVAL or trust the
# derived Activity/Busy columns: does ECS actually republish metering between
# two polls? Run it twice about one poll interval apart and diff:
#
#   ./hack/ecs-metering-snapshot.sh > /tmp/ecs-t1.txt
#   sleep 1800
#   ./hack/ecs-metering-snapshot.sh > /tmp/ecs-t2.txt
#   diff /tmp/ecs-t1.txt /tmp/ecs-t2.txt
#
# Reading the diff:
#   uptodate_till advanced + counters moved -> metering is fresh; polling
#       faster buys real resolution.
#   uptodate_till identical                -> ECS has not republished; polling
#       faster is wasted load and Busy's denominator stays low.
#   uptodate_till jumps in ~1h steps       -> activity resolution is hourly.
#
# stdout is stable and diffable (no wall clock, no token, no password); the
# timestamp goes to stderr. Requires: curl, jq. Written for bash 3.2 (macOS).
#
# Environment (same names the extension itself uses):
#   ECS_MGMT_URL     required   https://ecs.example.com:4443, no trailing slash
#   ECS_NAMESPACE    required
#   ECS_USERNAME     required   management user, e.g. ecs-usage
#   ECS_PASSWORD     prompted with `read -rs` when unset; never logged
#   ECS_SIZEUNIT     optional   query param, default KB
#   ECS_TLS_CA_FILE  optional   path to a PEM bundle
#   ECS_TLS_INSECURE optional   "true" skips verification (lab only)
#   ECS_MAX_PAGES    optional   billing pagination guard, default 100

set -eu

usage() {
  awk 'NR==1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  "") ;;
  *) echo "ecs-metering-snapshot: unexpected argument '$1' (try --help)" >&2; exit 2 ;;
esac

for tool in curl jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "ecs-metering-snapshot: $tool is required" >&2; exit 1; }
done

: "${ECS_MGMT_URL:?set ECS_MGMT_URL, e.g. https://ecs.example.com:4443}"
: "${ECS_NAMESPACE:?set ECS_NAMESPACE}"
: "${ECS_USERNAME:?set ECS_USERNAME (management user, e.g. ecs-usage)}"
ECS_SIZEUNIT="${ECS_SIZEUNIT:-KB}"
ECS_MAX_PAGES="${ECS_MAX_PAGES:-100}"
ECS_MGMT_URL="${ECS_MGMT_URL%/}"

if [ -z "${ECS_PASSWORD:-}" ]; then
  if [ -t 0 ]; then
    printf 'ECS_PASSWORD for %s: ' "$ECS_USERNAME" >&2
    read -rs ECS_PASSWORD
    printf '\n' >&2
  else
    echo "ecs-metering-snapshot: ECS_PASSWORD is unset and stdin is not a terminal" >&2
    exit 2
  fi
fi
[ -n "$ECS_PASSWORD" ] || { echo "ecs-metering-snapshot: ECS_PASSWORD is empty" >&2; exit 2; }

# curl reads credentials from a 0600 config file instead of argv, so neither
# the password nor the token shows up in `ps` output or shell history.
case "$ECS_USERNAME$ECS_PASSWORD" in
  *'"'*) echo 'ecs-metering-snapshot: a credential containing " cannot be passed via curl -K' >&2; exit 2 ;;
esac
CFG="$(mktemp "${TMPDIR:-/tmp}/ecs-snapshot.XXXXXX")" || exit 1
chmod 600 "$CFG"
trap 'rm -f "$CFG"' EXIT
trap 'rm -f "$CFG"; trap - INT; kill -INT $$' INT
trap 'rm -f "$CFG"; trap - TERM; kill -TERM $$' TERM

# write_cfg <credential-line...> rebuilds the curl config: TLS options plus one
# credential line (basic auth for /login, the token header afterwards).
write_cfg() {
  : > "$CFG"
  if [ -n "${ECS_TLS_CA_FILE:-}" ]; then
    [ -r "$ECS_TLS_CA_FILE" ] || { echo "ecs-metering-snapshot: cannot read ECS_TLS_CA_FILE=$ECS_TLS_CA_FILE" >&2; exit 2; }
    printf 'cacert = "%s"\n' "$ECS_TLS_CA_FILE" >> "$CFG"
  fi
  if [ "${ECS_TLS_INSECURE:-false}" = "true" ]; then
    printf 'insecure\n' >> "$CFG"
  fi
  printf '%s\n' "$1" >> "$CFG"
}

# fetch <url> -> body on stdout; non-200 exits with the status and a snippet.
fetch() {
  resp="$(curl -sS -K "$CFG" -H 'Accept: application/json' -w '\n%{http_code}' "$1")" || {
    echo "ecs-metering-snapshot: request failed: $1" >&2; exit 1; }
  status="${resp##*$'\n'}"
  body="${resp%$'\n'*}"
  if [ "$status" != "200" ]; then
    echo "ecs-metering-snapshot: HTTP $status from $1" >&2
    printf '%s\n' "$body" | head -c 300 >&2; printf '\n' >&2
    exit 1
  fi
  # Some ECS builds answer an Accept: application/json request with HTML or
  # HTTP 406 unless the *path* carries a .json suffix (the Go client does the
  # same fallback once). The suffix goes before the query string.
  case "$body" in
    \{*) printf '%s' "$body" ;;
    *)
      if json_suffix_tried "$1"; then
        echo "ecs-metering-snapshot: response is not JSON (HTTP $status)" >&2
        printf '%s\n' "$body" | head -c 200 >&2; printf '\n' >&2
        exit 1
      fi
      fetch "$(add_json_suffix "$1")"
      ;;
  esac
}

# path_part <url> strips the query string.
path_part() { printf '%s' "${1%%\?*}"; }

# json_suffix_tried reports whether the .json fallback was already attempted,
# so a non-JSON answer can never recurse.
json_suffix_tried() {
  case "$(path_part "$1")" in
    *.json) return 0 ;;
    *) return 1 ;;
  esac
}

# add_json_suffix inserts .json between the path and the query string.
add_json_suffix() {
  case "$1" in
    *\?*) printf '%s.json?%s' "$(path_part "$1")" "${1#*\?}" ;;
    *)     printf '%s.json' "$1" ;;
  esac
}

# --- login: the token comes back in a response header, not the body ---
write_cfg "$(printf 'user = "%s:%s"' "$ECS_USERNAME" "$ECS_PASSWORD")"
headers="$(curl -sS -K "$CFG" -D - -o /dev/null "$ECS_MGMT_URL/login")" || {
  echo "ecs-metering-snapshot: login request failed (is ECS_MGMT_URL reachable?)" >&2; exit 1; }
if printf '%s' "$headers" | grep -qiE '^HTTP/[0-9.]+ (401|403)'; then
  echo "ecs-metering-snapshot: login rejected (401/403) — check ECS_USERNAME/ECS_PASSWORD" >&2
  exit 1
fi
TOKEN="$(printf '%s' "$headers" | tr -d '\r' \
  | awk 'tolower($1)=="x-sds-auth-token:" { sub(/^[^:]*:[ \t]*/, ""); print; exit }')"
[ -n "$TOKEN" ] || {
  echo "ecs-metering-snapshot: login response had no X-SDS-AUTH-TOKEN header" >&2
  printf '%s\n' "$headers" | head -1 >&2   # status line only, never other headers
  exit 1; }
write_cfg "$(printf 'header = "X-SDS-AUTH-TOKEN: %s"' "$TOKEN")"

echo "# ecs-metering-snapshot $(date -u '+%Y-%m-%dT%H:%M:%SZ') ns=$ECS_NAMESPACE url=$ECS_MGMT_URL" >&2

# --- billing pages, following next_marker like the extension does ---
marker=""
page=0
while :; do
  page=$((page + 1))
  if [ "$page" -gt "$ECS_MAX_PAGES" ]; then
    echo "ecs-metering-snapshot: exceeded $ECS_MAX_PAGES billing pages" >&2; exit 1
  fi
  url="$ECS_MGMT_URL/object/billing/namespace/$ECS_NAMESPACE/info?include_bucket_detail=true&sizeunit=$ECS_SIZEUNIT"
  [ -z "$marker" ] || url="$url&marker=$marker"
  body="$(fetch "$url")"
  printf '%s' "$body" | jq -e . >/dev/null 2>&1 || {
    echo "ecs-metering-snapshot: page $page is not valid JSON" >&2; exit 1; }
  if [ "$page" -eq 1 ]; then
    printf '%s' "$body" | jq -r '"namespace_uptodate_till: \(.uptodate_till // "-")"'
  fi
  printf '%s' "$body" | jq -r '
    (.bucket_billing_info // [])
    | sort_by(.name)[]
    | [.name, (.total_objects|tostring), (.total_size|tostring),
       (.total_mpu_size|tostring), (.total_mpu_parts|tostring),
       (.uptodate_till // "-")]
    | @tsv'
  marker="$(printf '%s' "$body" | jq -r '.next_marker // ""')"
  [ -z "$marker" ] && break
done

#!/usr/bin/env bash
# Security round: black-box abuse of the API must yield governed 4xx errors,
# never cross-tenant data, never host paths, never leaked secrets.
set -euo pipefail

BASE_URL="${SBX_URL:-http://127.0.0.1:18083}"
DATA_DIR="/tmp/sandboxd-sec"
CONF="/tmp/sandboxd-sec.conf"
LOG="/tmp/sandboxd-sec.log"
ERR="/tmp/sandboxd-sec.err"

cat > "$CONF" <<EOF
listen = "127.0.0.1:18083"
data_dir = "$DATA_DIR"
bootstrap.image = "sandboxd-test-base:alpine322"
pool.enabled = false
EOF

pkill -9 -f "sandboxd -config $CONF" 2>/dev/null || true; sleep 1
rm -rf "$DATA_DIR"
./bin/sandboxd -config "$CONF" > "$LOG" 2>"$ERR" &
DAEMON_PID=$!
trap 'kill -9 $DAEMON_PID 2>/dev/null || true' EXIT
sleep 2
ADMIN_KEY=$(grep -oE "sk_[A-Za-z0-9_-]+" "$LOG" | tail -1)

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ✓ $1"; }
bad() { fail=$((fail+1)); echo "  ✗ $1"; }
code() { curl -s -o /tmp/sec-body.json -w "%{http_code}" "$@"; }
jsonf() { python3 -c "import json,sys;d=json.load(sys.stdin);print(d$1)"; }

TENANT=$(curl -s -H "Authorization: Bearer $ADMIN_KEY" -X POST "$BASE_URL/v1/tenants" -d '{"name":"sec-a"}' | jsonf "['id']")
TENANT_B=$(curl -s -H "Authorization: Bearer $ADMIN_KEY" -X POST "$BASE_URL/v1/tenants" -d '{"name":"sec-b"}' | jsonf "['id']")
KEY_A=$(curl -s -H "Authorization: Bearer $ADMIN_KEY" -X POST "$BASE_URL/v1/tenants/$TENANT/keys" -d '{"name":"a"}' | jsonf "['secret']")
IMAGE=sandboxd-test-base:alpine322
A="Authorization: Bearer $KEY_A"

SBX=$(curl -s -X POST "$BASE_URL/v1/sandboxes?wait=true" -H "$A" -d "{\"base_image\":\"$IMAGE\"}" | jsonf "['id']")

echo "== authentication =="
[ "$(code "$BASE_URL/v1/sandboxes")" = "401" ] && ok "no credentials → 401" || bad "unauthenticated request not 401"
[ "$(code "$BASE_URL/v1/sandboxes" -H "Authorization: Bearer sk_forgedvalue000000000")" = "401" ] && ok "forged key → 401" || bad "forged key accepted"
# tampered key with valid prefix of a real key
PFX="${KEY_A:0:16}"
TAMPERED="${KEY_A:0:16}AAAAAAAAAAAAAAAAAAAAAAA"
[ "$(code "$BASE_URL/v1/sandboxes" -H "Authorization: Bearer $TAMPERED")" = "401" ] && ok "tampered key (real prefix) → 401" || bad "prefix confusion possible"

echo "== tenant isolation (IDOR) =="
KEY_B=$(curl -s -H "Authorization: Bearer $ADMIN_KEY" -X POST "$BASE_URL/v1/tenants/$TENANT_B/keys" -d '{"name":"b"}' | jsonf "['secret']")
[ "$(code "$BASE_URL/v1/sandboxes/$SBX" -H "Authorization: Bearer $KEY_B")" = "403" ] && ok "cross-tenant sandbox read → 403" || bad "IDOR on sandbox read"
[ "$(code -X POST "$BASE_URL/v1/sandboxes/$SBX/exec" -H "Authorization: Bearer $KEY_B" -d '{"command":"id"}')" = "403" ] && ok "cross-tenant exec → 403" || bad "IDOR on exec"
[ "$(code -X DELETE "$BASE_URL/v1/sandboxes/$SBX" -H "Authorization: Bearer $KEY_B")" = "403" ] && ok "cross-tenant delete → 403" || bad "IDOR on delete"
SNAP=$(curl -s -X POST "$BASE_URL/v1/snapshots" -H "$A" -d "{\"sandbox_id\":\"$SBX\",\"name\":\"sec-snap\"}" | jsonf "['id']")
[ "$(code "$BASE_URL/v1/snapshots/$SNAP" -H "Authorization: Bearer $KEY_B")" = "403" ] && ok "cross-tenant snapshot read → 403" || bad "IDOR on snapshot"
# tenant B may see its OWN events, but every event must belong to B
FOREIGN=$(curl -s "$BASE_URL/v1/events" -H "Authorization: Bearer $KEY_B" | python3 -c "
import json,sys
events = json.load(sys.stdin)['events']
foreign = [e for e in events if e.get('tenant_id') != '$TENANT_B']
print(len(foreign))")
[ "$FOREIGN" = "0" ] && ok "no cross-tenant events visible" || bad "cross-tenant event leak ($FOREIGN foreign)"

echo "== admin boundaries =="
[ "$(code -X POST "$BASE_URL/v1/tenants" -H "$A" -d '{"name":"x"}')" = "403" ] && ok "tenant key cannot create tenants" || bad "tenant key has admin powers"
[ "$(code "$BASE_URL/v1/tenants" -H "$A")" = "403" ] && ok "tenant key cannot list tenants" || bad "tenant key lists tenants"
[ "$(code -X POST "$BASE_URL/v1/sandboxes?wait=false" -H "Authorization: Bearer $ADMIN_KEY" -d "{\"base_image\":\"$IMAGE\"}")" = "403" ] && ok "admin key without X-Tenant-Id cannot create" || bad "admin creates without tenant context"

echo "== path & payload abuse =="
[ "$(code -X PUT "$BASE_URL/v1/sandboxes/$SBX/files?path=/safe/../../etc/escape" -H "$A" --data-binary x)" = "400" ] && ok "path traversal rejected" || bad "traversal accepted"
[ "$(code -X PUT "$BASE_URL/v1/sandboxes/$SBX/files?path=relative" -H "$A" --data-binary x)" = "400" ] && ok "relative path rejected" || bad "relative accepted"
[ "$(code -X POST "$BASE_URL/v1/sandboxes/$SBX/exec" -H "$A" -d '{"argv":[]}')" = "400" ] && ok "empty argv rejected" || bad "empty argv accepted"
python3 -c "print('{\"x\":1}' * 300000)" > /tmp/sec-big.json
B=$(code -X POST "$BASE_URL/v1/sandboxes?wait=false" -H "$A" -H "Content-Type: application/json" --data-binary @/tmp/sec-big.json)
case "$B" in 400|413) ok "oversized body rejected ($B)";; *) bad "oversized body → $B";; esac
B=$(code -X POST "$BASE_URL/v1/sandboxes?wait=false" -H "$A" -d 'not json at all')
[ "$B" = "400" ] && ok "malformed JSON → 400" || bad "malformed JSON → $B"
# 1000-file env flood
python3 -c "
import json
env = {f'K{i}': 'v'*100 for i in range(1000)}
print(json.dumps({'base_image': '$IMAGE', 'spec': {'env': env, 'cpu_millicores': 500, 'memory_mib': 128, 'timeout_seconds': 600}}))" > /tmp/sec-env.json
B=$(code -X POST "$BASE_URL/v1/sandboxes?wait=false" -H "$A" -H "Content-Type: application/json" --data-binary @/tmp/sec-env.json)
[ "$B" = "400" ] && ok "env flood rejected" || bad "env flood → $B"

echo "== honest refusals =="
sleep 6  # let the create-group token bucket refill so the gate under test is admission, not rate limiting
B=$(code -X POST "$BASE_URL/v1/sandboxes?wait=false" -H "$A" -d "{\"base_image\":\"$IMAGE\",\"spec\":{\"egress\":{\"mode\":\"allowlist\",\"allow\":[\"api.github.com\"]},\"cpu_millicores\":500,\"memory_mib\":128,\"timeout_seconds\":600}}")
[ "$B" = "501" ] && ok "egress allowlist refused (501), not faked" || bad "egress allowlist → $B"
B=$(curl -s -X POST "$BASE_URL/v1/sandboxes?wait=false" -H "$A" -d "{\"base_image\":\"$IMAGE\",\"spec\":{\"disk_mib\":1024,\"cpu_millicores\":500,\"memory_mib\":128,\"timeout_seconds\":600}}" | jsonf "['error']['code']")
[ "$B" = "unimplemented" ] && ok "unenforceable disk quota refused" || bad "disk quota → $B"

echo "== secret hygiene =="
if grep -q "$KEY_A" "$ERR" 2>/dev/null; then bad "tenant key leaked into logs"; else ok "no key material in logs"; fi
if grep -q "$KEY_A" /tmp/sec-body.json 2>/dev/null; then bad "key echoed in responses"; else ok "no key material in error bodies"; fi

echo "== cleanup =="
curl -s -X DELETE "$BASE_URL/v1/sandboxes/$SBX?force=true" -H "$A" -o /dev/null
echo
echo "security results: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]

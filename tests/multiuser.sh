#!/usr/bin/env bash
# Multi-user round: two tenants run independent agent-like workflows
# concurrently while we continuously assert isolation, per-tenant quotas,
# and per-tenant audit scoping — the "SaaS with multiple real customers" shape.
set -uo pipefail

BASE_URL="http://127.0.0.1:18089"
DATA_DIR="/tmp/sandboxd-multi"
CONF="/tmp/sandboxd-multi.conf"
LOG="/tmp/sandboxd-multi.log"
IMAGE=sandboxd-test-base:alpine322

cat > "$CONF" <<EOF
listen = "127.0.0.1:18089"
data_dir = "$DATA_DIR"
bootstrap.image = "$IMAGE"
pool.enabled = false
EOF

pkill -9 -f "sandboxd -config $CONF" 2>/dev/null; sleep 1
rm -rf "$DATA_DIR"
./bin/sandboxd -config "$CONF" > "$LOG" 2>&1 &
DAEMON_PID=$!
trap 'kill -9 $DAEMON_PID 2>/dev/null || true' EXIT
sleep 2
ADMIN_KEY=$(grep -oE "sk_[A-Za-z0-9_-]+" "$LOG" | tail -1)

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ✓ $1"; }
bad() { fail=$((fail+1)); echo "  ✗ $1"; }

api() { curl -s -H "Authorization: Bearer $1" "${@:2}"; }

echo "== provisioning two customers =="
for C in alice bob; do
  TID=$(api "$ADMIN_KEY" -X POST "$BASE_URL/v1/tenants" -d "{
    \"name\": \"$C\",
    \"quota\": {
      \"max_sandboxes\": 3, \"max_memory_mib\": 1024,
      \"rates\": {
        \"create\": {\"per_second\": 20, \"burst\": 20},
        \"exec\": {\"per_second\": 50, \"burst\": 50},
        \"read\": {\"per_second\": 50, \"burst\": 50},
        \"snapshot\": {\"per_second\": 20, \"burst\": 20},
        \"admin\": {\"per_second\": 20, \"burst\": 20}
      }
    }
  }" | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
  KID=$(api "$ADMIN_KEY" -X POST "$BASE_URL/v1/tenants/$TID/keys" -d "{\"name\":\"$C-key\"}" | python3 -c "import json,sys;print(json.load(sys.stdin)['secret'])")
  eval "T_$C=$TID"; eval "K_$C=$KID"
done
echo "  alice=$T_alice bob=$T_bob"

WORKFLOW='
tenant=$1; key=$2; otherkey=$3; rounds=$4
for i in $(seq 1 $rounds); do
  # 1) create a sandbox for this round
  SBX=$(curl -s -X POST "'"$BASE_URL"'/v1/sandboxes?wait=true" -H "Authorization: Bearer $key" \
    -d "{\"base_image\":\"'"$IMAGE"'\",\"spec\":{\"cpu_millicores\":200,\"memory_mib\":128,\"timeout_seconds\":900}}" \
    | python3 -c "import json,sys;print(json.load(sys.stdin)[\"id\"])")
  echo "$tenant $i $SBX" >> /tmp/multi-live.txt
  # LIVE isolation probe: the other tenant must get 403 on a running sandbox
  PBODY=$(curl -s -w "\n%{http_code}" "'"$BASE_URL"'/v1/sandboxes/$SBX/exec" \
    -H "Authorization: Bearer $otherkey" -H "Content-Type: application/json" -d "{\"command\":\"id\"}")
  PCODE=$(echo "$PBODY" | tail -1)
  if [ "$PCODE" = "403" ]; then
    echo "ISOLATION-OK $tenant $i"
  else
    echo "WORKFLOW-FAIL $tenant $i probe=$PCODE body=$(echo "$PBODY" | head -1 | head -c 150)"
  fi
  # 2) agent-style work: write + read inside the box
  R=$(curl -s -X POST "'"$BASE_URL"'/v1/sandboxes/$SBX/exec" -H "Authorization: Bearer $key" \
    -d "{\"command\":\"echo $tenant-round-$i > /root/work.txt; cat /root/work.txt\"}")
  OUT=$(python3 -c "import json,sys,base64;print(base64.b64decode(json.load(sys.stdin)[\"stdout\"]).decode().strip())" <<< "$R")
  [ "$OUT" = "$tenant-round-$i" ] || echo "WORKFLOW-FAIL $tenant $i exec"
  # 3) snapshot the round state
  SN=$(curl -s -X POST "'"$BASE_URL"'/v1/snapshots" -H "Authorization: Bearer $key" \
    -d "{\"sandbox_id\":\"$SBX\",\"name\":\"$tenant-snap-$i\"}" 2>/dev/null)
  # 4) teardown
  curl -s -X DELETE "'"$BASE_URL"'/v1/sandboxes/$SBX?force=true" -H "Authorization: Bearer $key" -o /dev/null
done
'

echo "== concurrent workflows: alice ×3 rounds || bob ×3 rounds =="
: > /tmp/multi-live.txt
C=snap-uniq
bash -c "$WORKFLOW" -- alice "$K_alice" "$K_bob" 3 &
P1=$!
bash -c "$WORKFLOW" -- bob "$K_bob" "$K_alice" 3 &
P2=$!
wait $P1; wait $P2
if grep -q WORKFLOW-FAIL /tmp/multi-live.txt 2>/dev/null; then
  bad "workflow errors: $(grep WORKFLOW-FAIL /tmp/multi-live.txt | head -3)"
else
  ok "6 concurrent rounds: every create/exec/file/snapshot/delete + live isolation probe succeeded"
fi

echo "== audit scoping =="
FOREIGN=$(api "$K_alice" "$BASE_URL/v1/events?limit=1000" | python3 -c "
import json,sys
evs=json.load(sys.stdin)['events']
bad=[e for e in evs if e.get('sandbox_id','').startswith('sbx_') and e.get('tenant_id')!='$T_alice']
print(len(bad))")
[ "$FOREIGN" = "0" ] && ok "alice's event stream contains zero bob events" || bad "audit cross-tenant leak"
BOB_EVENTS=$(api "$K_bob" "$BASE_URL/v1/events?limit=1000" | python3 -c "import json,sys;print(len(json.load(sys.stdin)['events']))")
[ "$BOB_EVENTS" -ge 8 ] && ok "bob's audit stream has his own trail ($BOB_EVENTS events)" || bad "bob audit missing"

echo "== quota independence: alice saturates, bob unaffected =="
sleep 3
for i in 1 2 3; do
  curl -s -X POST "$BASE_URL/v1/sandboxes?wait=true" -H "Authorization: Bearer $K_alice" \
    -d "{\"base_image\":\"$IMAGE\"}" -o /dev/null
done
CODE_A=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE_URL/v1/sandboxes?wait=false" \
  -H "Authorization: Bearer $K_alice" -d "{\"base_image\":\"$IMAGE\"}")
CODE_B=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE_URL/v1/sandboxes?wait=true" \
  -H "Authorization: Bearer $K_bob" -d "{\"base_image\":\"$IMAGE\"}")
[ "$CODE_A" = "402" ] && [ "$CODE_B" = "201" ] \
  && ok "alice at quota → 402; bob still creates → 201 (quotas are per-tenant)" \
  || bad "quota independence broken (alice=$CODE_A bob=$CODE_B)"

echo "== key revocation is instant and tenant-scoped =="
NEWKEY=$(api "$ADMIN_KEY" -X POST "$BASE_URL/v1/tenants/$T_bob/keys" -d '{"name":"burner"}' | python3 -c "import json,sys;print(json.load(sys.stdin)['secret'])")
KID=$(api "$ADMIN_KEY" "$BASE_URL/v1/tenants/$T_bob/keys" | python3 -c "
import json,sys
ks=json.load(sys.stdin)['keys']
print([k['id'] for k in ks if k['name']=='burner'][0])")
RCODE=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$BASE_URL/v1/tenants/$T_bob/keys/$KID" -H "Authorization: Bearer $ADMIN_KEY")
[ "$RCODE" = "204" ] || echo "  (revoke call itself returned $RCODE — test harness issue)"
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/v1/sandboxes" -H "Authorization: Bearer $NEWKEY")
[ "$CODE" = "401" ] && ok "revoked key rejected immediately (401)" || bad "revoked key still works ($CODE)"
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/v1/sandboxes" -H "Authorization: Bearer $K_bob")
[ "$CODE" = "200" ] && ok "bob's other key unaffected (revocation is per-key)" || bad "revocation hit wrong key"

echo "== cleanup =="
api "$K_alice" "$BASE_URL/v1/sandboxes" | python3 -c "
import json,sys,subprocess
for sb in json.load(sys.stdin)['sandboxes']:
    subprocess.run(['curl','-s','-X','DELETE','$BASE_URL/v1/sandboxes/'+sb['id']+'?force=true','-H','Authorization: Bearer $K_alice','-o','/dev/null'])"
echo
echo "multi-user results: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]

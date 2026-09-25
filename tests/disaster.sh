#!/usr/bin/env bash
# Disaster round: two genuine disasters beyond process death.
#   A) data-loss recovery: restore the data dir from a backup taken earlier
#      (point-in-time recovery); state created AFTER the backup must be gone.
#   B) infrastructure loss: the container VM itself reboots under a live
#      daemon — sandboxes die with the machine, records must converge via
#      reconciliation, and the platform must admit new work after recovery.
set -uo pipefail

BASE_URL="http://127.0.0.1:18091"
DATA_DIR="/tmp/sandboxd-dr"
CONF="/tmp/sandboxd-dr.conf"
LOG="/tmp/sandboxd-dr.log"
IMAGE=sandboxd-test-base:alpine322

cat > "$CONF" <<EOF
listen = "127.0.0.1:18091"
data_dir = "$DATA_DIR"
bootstrap.image = "$IMAGE"
pool.enabled = false
sweep_every = "5s"
reconcile_every = "5s"
EOF

pkill -9 -f "sandboxd -config $CONF" 2>/dev/null; sleep 1
rm -rf "$DATA_DIR" "$DATA_DIR.bak"

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ✓ $1"; }
bad() { fail=$((fail+1)); echo "  ✗ $1"; }
start_daemon() { ./bin/sandboxd -config "$CONF" >> "$LOG" 2>&1 & DAEMON_PID=$!; sleep 2; }
stop_daemon()  { kill "$DAEMON_PID" 2>/dev/null; wait "$DAEMON_PID" 2>/dev/null; sleep 1; }

echo "== A) backup / point-in-time recovery =="
start_daemon
ADMIN_KEY=$(grep -oE "sk_[A-Za-z0-9_-]+" "$LOG" | tail -1)
TENANT=$(curl -s -H "Authorization: Bearer $ADMIN_KEY" -X POST "$BASE_URL/v1/tenants" -d '{"name":"original"}' | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
TKEY=$(curl -s -H "Authorization: Bearer $ADMIN_KEY" -X POST "$BASE_URL/v1/tenants/$TENANT/keys" -d '{"name":"dr"}' | python3 -c "import json,sys;print(json.load(sys.stdin)['secret'])")
SBX=$(curl -s -X POST "$BASE_URL/v1/sandboxes?wait=true" -H "Authorization: Bearer $TKEY" -d "{\"base_image\":\"$IMAGE\"}" | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
curl -s -X PUT "$BASE_URL/v1/sandboxes/$SBX/files?path=/root/state.txt" -H "Authorization: Bearer $TKEY" --data-binary "pre-backup-state" -o /dev/null
SNAP=$(curl -s -X POST "$BASE_URL/v1/snapshots" -H "Authorization: Bearer $TKEY" -d "{\"sandbox_id\":\"$SBX\",\"name\":\"dr-snap\"}" | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))")

stop_daemon
cp -a "$DATA_DIR" "$DATA_DIR.bak"          # the operator's backup

# after the backup, business continues and creates NEW state
start_daemon
curl -s -H "Authorization: Bearer $ADMIN_KEY" -X POST "$BASE_URL/v1/tenants" -d '{"name":"created-after-backup"}' -o /dev/null
stop_daemon

# disaster: the live data dir is lost; restore from backup
rm -rf "$DATA_DIR" && cp -a "$DATA_DIR.bak" "$DATA_DIR"
start_daemon

CODE_T=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/v1/tenants/$TENANT" -H "Authorization: Bearer $ADMIN_KEY")
CODE_OLD=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE_URL/v1/tenants" -H "Authorization: Bearer $ADMIN_KEY" -d '{"name":"created-after-backup-dup"}')
[ "$CODE_T" = "200" ] && ok "original tenant recovered from backup" || bad "original tenant lost ($CODE_T)"
api_count=$(curl -s "$BASE_URL/v1/tenants" -H "Authorization: Bearer $ADMIN_KEY" | python3 -c "
import json,sys
names=[t['name'] for t in json.load(sys.stdin)['tenants']]
print(len([n for n in names if n=='created-after-backup']))")
[ "$api_count" = "0" ] && ok "post-backup state correctly absent (true point-in-time)" || bad "post-backup state leaked into restore"
CODE_K=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/v1/sandboxes" -H "Authorization: Bearer $TKEY")
[ "$CODE_K" = "200" ] && ok "tenant API key still valid after restore" || bad "tenant key lost ($CODE_K)"
curl -s "$BASE_URL/v1/snapshots" -H "Authorization: Bearer $TKEY" | grep -q '"dr-snap"' \
  && ok "snapshot metadata recovered" || bad "snapshot lost"
stop_daemon

echo "== B) infrastructure disaster: container VM reboots under a live daemon =="
start_daemon
ADMIN_KEY=$(grep -oE "sk_[A-Za-z0-9_-]+" "$LOG" | tail -1)
TENANT=$(curl -s -H "Authorization: Bearer $ADMIN_KEY" -X POST "$BASE_URL/v1/tenants" -d '{"name":"vmloss"}' | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
TKEY=$(curl -s -H "Authorization: Bearer $ADMIN_KEY" -X POST "$BASE_URL/v1/tenants/$TENANT/keys" -d '{"name":"vmloss"}' | python3 -c "import json,sys;print(json.load(sys.stdin)['secret'])")
SBX=$(curl -s -X POST "$BASE_URL/v1/sandboxes?wait=true" -H "Authorization: Bearer $TKEY" -d "{\"base_image\":\"$IMAGE\"}" | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
STATE_BEFORE=$(curl -s "$BASE_URL/v1/sandboxes/$SBX" -H "Authorization: Bearer $TKEY" | python3 -c "import json,sys;print(json.load(sys.stdin)['state'])")
echo "  live sandbox: $SBX state=$STATE_BEFORE; now the VM dies..."
colima stop >/dev/null 2>&1
DURING=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/readyz")
echo "  during outage: readyz=$DURING (daemon up, backend gone)"
[ "$DURING" = "503" ] && ok "backend loss reported honestly (503, not silent)" || bad "readyz during outage = $DURING"
colima start >/dev/null 2>&1
sleep 3
# reconciler (5s tick) must converge the records with backend truth
sleep 12
STATE_AFTER=$(curl -s "$BASE_URL/v1/sandboxes/$SBX" -H "Authorization: Bearer $TKEY" | python3 -c "import json,sys;print(json.load(sys.stdin)['state'])")
case "$STATE_AFTER" in
  stopped|failed) ok "sandbox record converged to '$STATE_AFTER' (truthfully dead with the VM)" ;;
  running) bad "record still claims running after VM death" ;;
  *) bad "record in unexpected state: $STATE_AFTER" ;;
esac
EV=$(curl -s "$BASE_URL/v1/events?action=reconcile.fix&limit=5" -H "Authorization: Bearer $TKEY" | python3 -c "import json,sys;print(len(json.load(sys.stdin)['events']))")
[ "$EV" -ge 1 ] && ok "reconciliation audited ($EV reconcile.fix events)" || bad "no reconciliation audit"
NEW=$(curl -s -X POST "$BASE_URL/v1/sandboxes?wait=true" -H "Authorization: Bearer $TKEY" -d "{\"base_image\":\"$IMAGE\"}" | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
[ -n "$NEW" ] && ok "platform admits new sandboxes after infra recovery ($NEW)" || bad "platform broken after infra recovery"

echo "== cleanup =="
curl -s -X DELETE "$BASE_URL/v1/sandboxes/$NEW?force=true" -H "Authorization: Bearer $TKEY" -o /dev/null
stop_daemon
echo
echo "disaster results: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]

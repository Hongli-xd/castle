#!/usr/bin/env bash
# Chaos round: the daemon must converge to backend truth after SIGKILL at
# arbitrary points, and never leave a "running" record for a dead container.
set -euo pipefail

BASE_URL="${SBX_URL:-http://127.0.0.1:18081}"
DATA_DIR="/tmp/sandboxd-chaos"
CONF="/tmp/sandboxd-chaos.conf"
LOG="/tmp/sandboxd-chaos.log"

cat > "$CONF" <<EOF
listen = "127.0.0.1:18081"
data_dir = "$DATA_DIR"
bootstrap.image = "sandboxd-test-base:alpine322"
pool.enabled = false
sweep_every = "5s"
reconcile_every = "5s"
EOF

pass=0; fail=0
ok()   { pass=$((pass+1)); echo "  ✓ $1"; }
bad()  { fail=$((fail+1)); echo "  ✗ $1"; }

pkill -9 -f "sandboxd -config $CONF" 2>/dev/null || true
sleep 1
rm -rf "$DATA_DIR"
./bin/sandboxd -config "$CONF" > "$LOG" 2>&1 &
DAEMON_PID=$!
sleep 2
ADMIN_KEY=$(grep -oE "sk_[A-Za-z0-9_-]+" "$LOG" | tail -1)

api() { curl -s -H "Authorization: Bearer $ADMIN_KEY" "$@"; }
# tenant-scoped call: exactly one Authorization header
tapi() { curl -s -H "Authorization: Bearer $TKEY" "$@"; }

# tenant + key for chaos ops
TENANT=$(api -X POST "$BASE_URL/v1/tenants" -d '{"name":"chaos"}' | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
TKEY=$(api -X POST "$BASE_URL/v1/tenants/$TENANT/keys" -d '{"name":"chaos"}' | python3 -c "import json,sys;print(json.load(sys.stdin)['secret'])")
A="Authorization: Bearer $TKEY"

echo "== chaos 1: SIGKILL while a create is in flight =="
SBX=$(curl -s -X POST "$BASE_URL/v1/sandboxes?wait=false" -H "$A" -d '{"base_image":"sandboxd-test-base:alpine322"}' | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
kill -9 $DAEMON_PID 2>/dev/null || true
sleep 0.3
./bin/sandboxd -config "$CONF" >> "$LOG" 2>&1 &
DAEMON_PID=$!
sleep 8   # allow reconcile (5s tick)
STATE=$(tapi "$BASE_URL/v1/sandboxes/$SBX" | python3 -c "import json,sys;print(json.load(sys.stdin)['state'])")
case "$STATE" in
  running|failed|stopped) ok "create converged to '$STATE' after mid-flight kill";;
  *) bad "create stuck in '$STATE'";;
esac

echo "== chaos 2: SIGKILL with live sandbox; sandbox must survive and stay consistent =="
SBX2=$(curl -s -X POST "$BASE_URL/v1/sandboxes?wait=true" -H "$A" -d '{"base_image":"sandboxd-test-base:alpine322"}' | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
kill -9 $DAEMON_PID 2>/dev/null || true
sleep 0.3
./bin/sandboxd -config "$CONF" >> "$LOG" 2>&1 &
DAEMON_PID=$!
sleep 8
STATE2=$(tapi "$BASE_URL/v1/sandboxes/$SBX2" | python3 -c "import json,sys;print(json.load(sys.stdin)['state'])")
if [ "$STATE2" = "running" ]; then ok "live sandbox survived daemon SIGKILL (state=running)"; else bad "live sandbox became $STATE2"; fi
EXEC=$(curl -s -X POST "$BASE_URL/v1/sandboxes/$SBX2/exec" -H "$A" -d '{"command":"echo alive-after-chaos"}' | python3 -c "import json,sys,base64;print(base64.b64decode(json.load(sys.stdin)['stdout']).decode())")
[ "$EXEC" = "alive-after-chaos" ] && ok "exec works after crash-recovery" || bad "exec broken after recovery: $EXEC"

echo "== chaos 3: store survives repeated SIGKILL (WAL integrity) =="
for i in 1 2 3; do
  kill -9 $DAEMON_PID 2>/dev/null || true
  sleep 0.2
  ./bin/sandboxd -config "$CONF" >> "$LOG" 2>&1 &
  DAEMON_PID=$!
  sleep 3
done
COUNT=$(tapi "$BASE_URL/v1/sandboxes" | python3 -c "import json,sys;print(json.load(sys.stdin)['count'])")
if [ "$COUNT" -ge 2 ]; then ok "records intact after 3 SIGKILLs (count=$COUNT)"; else bad "records lost after SIGKILLs (count=$COUNT)"; fi
STATE2B=$(tapi "$BASE_URL/v1/sandboxes/$SBX2" | python3 -c "import json,sys;print(json.load(sys.stdin)['state'])")
[ "$STATE2B" = "running" ] && ok "sandbox still running after 3 crash cycles" || bad "sandbox state drifted: $STATE2B"

echo "== cleanup =="
kill -9 $DAEMON_PID 2>/dev/null || true
echo
echo "chaos results: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]

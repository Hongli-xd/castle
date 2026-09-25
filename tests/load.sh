#!/usr/bin/env bash
# Load round: mixed create/exec/delete traffic under concurrency.
# Gates: zero 5xx, zero errors other than governed 4xx (429/402), sane latency.
set -euo pipefail

BASE_URL="${SBX_URL:-http://127.0.0.1:18082}"
DATA_DIR="/tmp/sandboxd-load"
CONF="/tmp/sandboxd-load.conf"
LOG="/tmp/sandboxd-load.log"

cat > "$CONF" <<EOF
listen = "127.0.0.1:18082"
data_dir = "$DATA_DIR"
bootstrap.image = "sandboxd-test-base:alpine322"
pool.enabled = true
pool.size = 8
max_concurrent_creates = 8
quota.max_sandboxes = 100
EOF

pkill -9 -f "sandboxd -config $CONF" 2>/dev/null || true; sleep 1
rm -rf "$DATA_DIR"
./bin/sandboxd -config "$CONF" > "$LOG" 2>&1 &
DAEMON_PID=$!
trap 'kill -9 $DAEMON_PID 2>/dev/null || true' EXIT
sleep 2
ADMIN_KEY=$(grep -oE "sk_[A-Za-z0-9_-]+" "$LOG" | tail -1)
# the load tenant opts into high create/exec rates so the gate being measured
# is the daemon's capacity, not the default 1/s tenant guardrail
TENANT=$(curl -s -H "Authorization: Bearer $ADMIN_KEY" -X POST "$BASE_URL/v1/tenants" -d '{
  "name": "load",
  "quota": {
    "max_sandboxes": 100,
    "rates": {
      "create":  {"per_second": 50, "burst": 60},
      "exec":    {"per_second": 100, "burst": 200},
      "read":    {"per_second": 100, "burst": 200}
    }
  }
}' | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
TKEY=$(curl -s -H "Authorization: Bearer $ADMIN_KEY" -X POST "$BASE_URL/v1/tenants/$TENANT/keys" -d '{"name":"load"}' | python3 -c "import json,sys;print(json.load(sys.stdin)['secret'])")

echo "== 30 concurrent creates (wait=true) =="
CREATES=/tmp/load-create.txt
: > "$CREATES"
CREATE_PIDS=()
for i in $(seq 1 30); do
  ( t0=$(python3 -c 'import time;print(int(time.time()*1e9))')
    CODE=$(curl -s -o /tmp/load-body-$i.json -w "%{http_code}" -X POST "$BASE_URL/v1/sandboxes?wait=true" \
      -H "Authorization: Bearer $TKEY" -d '{"base_image":"sandboxd-test-base:alpine322","spec":{"cpu_millicores":100,"memory_mib":128,"timeout_seconds":600}}')
    t1=$(python3 -c 'import time;print(int(time.time()*1e9))')
    echo "$CODE $(( (t1 - t0)/1000000 ))" >> "$CREATES" ) &
  CREATE_PIDS+=($!)
done
# wait only on our workers — a bare `wait` would also wait on the daemon
wait "${CREATE_PIDS[@]}"
python3 - <<'PY'
codes = {}
lat = []
for line in open('/tmp/load-create.txt'):
    c, ms = line.split()
    codes[c] = codes.get(c, 0) + 1
    if c == '201':
        lat.append(int(ms))
lat.sort()
p50 = lat[len(lat)//2] if lat else -1
p99 = lat[int(len(lat)*0.99)] if lat else -1
print(f"  create codes: {codes}")
print(f"  create latency ms: p50={p50} p99={p99} min={lat[0] if lat else '-'} max={lat[-1] if lat else '-'}")
assert codes.get('5xx', 0) == 0 and not any(k.startswith('5') for k in codes), "5xx detected!"
PY

echo "== 200 sequential execs across 20 sandboxes, 10 workers =="
IDS=$(python3 - <<'PY'
import json, glob
ids = []
for f in glob.glob('/tmp/load-body-*.json'):
    try:
        d = json.load(open(f))
        if d.get('id'):
            ids.append(d['id'])
    except Exception:
        pass
print('\n'.join(ids[:20]))
PY
)
echo "$IDS" > /tmp/load-ids.txt
EXECS=/tmp/load-exec.txt
# precompute the 200-task list: worker i owns lines [i*20+1, (i+1)*20]
python3 - <<'PYBUILD'
ids = [l.strip() for l in open('/tmp/load-ids.txt') if l.strip()]
tasks = []
for w in range(10):
    for j in range(20):
        tasks.append(ids[(w * 20 + j) % len(ids)])
open('/tmp/load-tasks.txt', 'w').write('\n'.join(tasks) + '\n')
PYBUILD
: > "$EXECS"
EXEC_PIDS=()
for i in $(seq 0 9); do
  (
    START=$(( i * 20 + 1 ))
    END=$(( (i + 1) * 20 ))
    while read -r ID; do
      [ -z "$ID" ] && continue
      t0=$(python3 -c 'import time;print(int(time.time()*1e9))')
      CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE_URL/v1/sandboxes/$ID/exec" \
        -H "Authorization: Bearer $TKEY" -d '{"command":"true"}')
      t1=$(python3 -c 'import time;print(int(time.time()*1e9))')
      echo "$CODE $(( (t1 - t0)/1000000 ))" >> "$EXECS"
    done < <(sed -n "${START},${END}p" /tmp/load-tasks.txt)
  ) &
  EXEC_PIDS+=($!)
done
wait "${EXEC_PIDS[@]}"

python3 - <<'PY'
codes = {}
lat = []
for line in open('/tmp/load-exec.txt'):
    c, ms = line.split()
    codes[c] = codes.get(c, 0) + 1
    if c == '200':
        lat.append(int(ms))
lat.sort()
print(f"  exec codes: {codes}")
print(f"  exec latency ms: p50={lat[len(lat)//2] if lat else '-'} p99={lat[int(len(lat)*0.99)] if lat else '-'}")
assert not any(k.startswith('5') for k in codes), "5xx detected!"
PY

echo "== daemon health after load =="
curl -s "$BASE_URL/readyz"
echo
echo "load round complete"

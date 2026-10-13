#!/usr/bin/env bash
# PoC-6：容器后端共享宿主内核 —— 沙箱内能看见什么、看不见什么
#
# 目标：把"共享内核"这个抽象说法变成一张可核对的清单：
#   · 被真正隔离的（命名空间生效）         → 项目的隔离参数确实有效
#   · 仍然泄漏给任意租户的（未命名空间化） → 多租户下的真实信息面
#
# 关键判据：沙箱的 memory 上限被设为 128 MiB；如果 /proc/meminfo 报告的
# 总量远大于 128 MiB，就证明 /proc/meminfo 反映的是【宿主总量】而非容器上限，
# 即该字段未被命名空间化 —— 任意租户都能读到平台宿主的真实硬件规格。
set -uo pipefail
cd "$(dirname "$0")/.."

PORT=18124
BASE="http://127.0.0.1:$PORT"
DIR=/tmp/poc-iso-data
CONF=/tmp/poc-iso.conf
LOG=/tmp/poc-iso.log
IMAGE=sandboxd-test-base:alpine322
PY=/usr/bin/python3

c() { curl -s --noproxy '*' "$@"; }
jget() { $PY -c "import json,sys;print(json.load(sys.stdin)$1)"; }

cat > "$CONF" <<EOF
listen = "127.0.0.1:$PORT"
data_dir = "$DIR"
bootstrap.image = "$IMAGE"
pool.enabled = false
EOF

pkill -9 -f "sandboxd -config $CONF" 2>/dev/null; sleep 1
rm -rf "$DIR"
./bin/sandboxd -config "$CONF" > "$LOG" 2>&1 &
PID=$!
trap 'kill -9 $PID 2>/dev/null' EXIT
for _ in $(seq 1 40); do
  [ -n "$(grep -oE 'sk_[A-Za-z0-9_-]+' "$LOG" 2>/dev/null | tail -1)" ] && break
  sleep 0.5
done
sleep 2
ADMIN=$(grep -oE "sk_[A-Za-z0-9_-]+" "$LOG" | tail -1)

TEN=$(c -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
      -X POST "$BASE/v1/tenants" -d '{"name":"iso"}' | jget "['id']")
KEY=$(c -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
      -X POST "$BASE/v1/tenants/$TEN/keys" -d '{"name":"iso"}' | jget "['secret']")
A="Authorization: Bearer $KEY"

MEM_LIMIT=128
SBX=$(c -X POST "$BASE/v1/sandboxes?wait=true" -H "$A" -H "Content-Type: application/json" \
  -d "{\"base_image\":\"$IMAGE\",\"spec\":{\"memory_mib\":$MEM_LIMIT,\"cpu_millicores\":500,\"timeout_seconds\":3600}}" \
  | jget "['id']")
echo "沙箱 $SBX   声明内存上限 = ${MEM_LIMIT} MiB"
echo

run() {  # run <标签> <shell 命令>
  local out
  out=$(c -X POST "$BASE/v1/sandboxes/$SBX/exec" -H "$A" -H "Content-Type: application/json" \
        -d "$($PY -c 'import json,sys;print(json.dumps({"command":sys.argv[1],"timeout_ms":20000,"max_output":65536}))' "$2")" \
        | $PY -c "import json,sys,base64;d=json.load(sys.stdin);print(base64.b64decode(d.get('stdout') or b'').decode('utf-8','replace').strip())")
  printf '%s\n' "$out" | sed "s/^/   /"
}

echo "【A】被真正隔离的部分（命名空间生效，隔离参数有效）"
echo "-- PID 命名空间：能看到几个进程？"
run "pidns" 'ls -d /proc/[0-9]* 2>/dev/null | wc -l'
echo "-- 能否看到别的容器/宿主进程？"
run "other" 'ls /proc/[0-9]* -d 2>/dev/null | head -5; echo "---"; cat /proc/1/comm 2>/dev/null'
echo "-- 网络：net=none 是否真的没有网卡（除 lo）？"
run "net" 'ip -o link 2>/dev/null || cat /proc/net/dev'
echo "-- 能力集：CapDrop ALL 后还剩什么？"
run "caps" 'grep -E "^(CapInh|CapPrm|CapEff|CapBnd)" /proc/self/status'
echo "-- 有没有挂到宿主目录？"
run "mounts" 'grep -vE "^(proc|sysfs|tmpfs|devpts|mqueue|shm|cgroup|overlay|none)" /proc/mounts | head -8'

echo
echo "【B】仍然泄漏给任意租户的宿主信息（未命名空间化的字段）"
echo "-- 宿主内核版本（→ 可据此挑选匹配的内核提权漏洞）"
run "kernel" 'uname -r; uname -a'
echo "-- 宿主 CPU 型号与核数"
run "cpu" 'grep -m1 "model name" /proc/cpuinfo; grep -c ^processor /proc/cpuinfo'
echo "-- 宿主物理内存总量（本沙箱上限仅 '"$MEM_LIMIT"' MiB，看它报多少）"
run "mem" 'grep -E "^(MemTotal|MemFree|MemAvailable|SwapTotal)" /proc/meminfo'
echo "-- 宿主实时负载与开机时长（→ 可探测平台繁忙度/邻居压力）"
run "load" 'cat /proc/loadavg; cat /proc/uptime'
echo "-- 宿主硬件厂商/型号（/sys 以只读方式来自宿主）"
run "dmi" 'for f in sys_vendor product_name product_uuid bios_version; do printf "%s=" "$f"; cat /sys/class/dmi/id/$f 2>/dev/null || echo "(n/a)"; done'

echo
echo "【C】判定"
MEMTOTAL=$(run "memnum" 'awk "/MemTotal/{print \$2}" /proc/meminfo')
MEM_MIB=$($PY -c "print(int(${MEMTOTAL:-0}//1024))")
echo "   沙箱声明内存上限: ${MEM_LIMIT} MiB"
echo "   /proc/meminfo 报告: ${MEM_MIB} MiB"
if [ "${MEM_MIB:-0}" -gt $((MEM_LIMIT * 2)) ]; then
  echo "   [已复现] /proc/meminfo 反映宿主总量（${MEM_MIB} MiB），未按容器上限过滤"
  echo "            → 任意租户可读到平台宿主的真实内存规格"
else
  echo "   /proc/meminfo 未泄漏宿主总量"
fi

echo
echo "-- 附加：宿主内核版本指纹"
KV=$(run "kv" 'uname -r')
echo "   沙箱内 uname -r = $KV"

c -X DELETE "$BASE/v1/sandboxes/$SBX?force=true" -H "$A" -o /dev/null
echo
echo "结论：命名空间层面（PID/网络/能力/挂载）隔离确实生效；"
echo "      但共享内核导致的信息面（内核版本、CPU 型号、宿主内存、实时负载、DMI）"
echo "      对任意租户完全可见 —— 这是容器后端与 microVM 后端不可弥合的差距。"

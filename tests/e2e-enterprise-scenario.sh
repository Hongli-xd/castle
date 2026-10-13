#!/usr/bin/env bash
# 企业场景端到端演练：多业务线 AI 编程助手的代码执行底座。
#
# 这个脚本不是单元测试，而是"一个真实客户会怎么用它"的可复跑演练。它存在的
# 理由是：定位文档里每一条"我们支持 X"都应该有对应的调用记录，而不是一句声明。
# 每发起一次真实请求就向 trace 追加一行 JSON，产物用于生成动态流程图。
#
# 用法：  tests/e2e-enterprise-scenario.sh [--keep]
#   --keep   跑完保留数据目录，便于事后翻 WAL / 容器
#
# 前提：本机有可用的 Docker，且存在 alpine:3.22 镜像。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${SCENARIO_PORT:-18080}"
BASE="http://127.0.0.1:$PORT"
ADMIN_KEY="sk_scenario-admin-000000000000000000000000"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/sbx-scenario.XXXXXX")"
TRACE="$ROOT/docs/scenario-trace.jsonl"
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

PASS=0; FAIL=0; STEP=0
: > "$TRACE"
# Pre-initialised so that a failed step earlier in the run cannot abort the
# whole scenario with "unbound variable": under `set -u` a later reference to
# an ID that was never assigned would kill the run instead of reporting the
# real failure.
SBX1=""; SBX2=""; SBX3=""; SNAP=""; TPL_ID=""
TENANT_A=""; TENANT_B=""; KEY_A=""; KEY_B=""; NOTE=""
LAST_METHOD=""; LAST_PATH=""; LAST_MS=0

say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }

# ── trace ────────────────────────────────────────────────────────────────────
# record <act> <title> <method> <path> <status> <ms> <note> <evidence>
record() {
  STEP=$((STEP+1))
  jq -nc --argjson n "$STEP" --arg act "$1" --arg title "$2" --arg method "$3" \
        --arg path "$4" --argjson status "$5" --argjson ms "$6" --arg note "$7" \
        --arg evidence "${8:-}" \
        '{n:$n, act:$act, title:$title, method:$method, path:$path,
          status:$status, ms:$ms, note:$note, evidence:$evidence}' >> "$TRACE"
}

# ── request plumbing ─────────────────────────────────────────────────────────
last_status=""; last_body=""
LAST_METHOD=""; LAST_PATH=""; LAST_MS=0

# req METHOD PATH [BODY|@FILE] [AUTH] [EXTRA_HEADER...]
req() {
  local method="$1" path="$2" body="${3:-}" auth="${4:-}"; shift 4 2>/dev/null || shift $#
  # macOS ships bash 3.2, where "${arr[@]}" on an EMPTY array is an unbound
  # variable under `set -u`. The ${arr[@]+"${arr[@]}"} idiom expands to
  # nothing when empty (the common case: a GET with no body) and to the
  # elements otherwise. Without it every body-less request fails at the
  # argument-expansion step and never reaches curl.
  local -a h
  h=()
  [ -n "$auth" ] && h+=(-H "Authorization: Bearer $auth")
  for extra in "$@"; do h+=(-H "$extra"); done
  local -a b
  b=()
  if [ -n "$body" ]; then
    if [ "${body:0:1}" = "@" ]; then b=(--data-binary @"${body:1}"); else b=(-d "$body"); fi
  fi
  last_status=$(curl -sS -D "$WORK/headers" -o "$WORK/body" -w '%{http_code} %{time_total}' -X "$method" \
    ${h[@]+"${h[@]}"} ${b[@]+"${b[@]}"} "$BASE$path" 2>"$WORK/err") \
    || { last_status=000; last_body=""; return; }
  # Latency comes from curl's own %{time_total}, not from a clock read around
  # the call. Wrapping the request in `date`/`python3` measures process spawn
  # (~200 ms of interpreter startup) and buries the real number — the first
  # run of this script reported 211 ms for /healthz because of exactly that.
  last_body=$(cat "$WORK/body")
  LAST_MS=$(awk -v s="${last_status#* }" 'BEGIN{printf "%d", s*1000+0.5}')
  last_status="${last_status%% *}"
}
jget() { printf '%s' "$last_body" | jq -r "$1" 2>/dev/null; }
jget64() { printf '%s' "$last_body" | jq -r "$1 | @base64d" 2>/dev/null; }

# fetch METHOD PATH [...] — performs the call; timing/status captured in req()
fetch() {
  LAST_METHOD="$1"; LAST_PATH="$2"; shift 2
  req "$LAST_METHOD" "$LAST_PATH" "$@"
}

retryAfter() {
  awk -F': *' 'tolower($1)=="retry-after"{print $2}' "$WORK/headers" 2>/dev/null | tr -d '\r' | tail -1
}

# fetch_retry — for SETUP calls, not for the call under test.
#
# The platform admin bucket is 1/s burst 5 and is hardcoded: there is no
# config key for it (see sandboxd.conf.example, "已知缺口"). A script that
# fires admin calls back-to-back will therefore legitimately receive 429,
# exactly as a real client would. Retrying after Retry-After is what a
# correct client does; the deliberate 429 assertion below still uses plain
# fetch(), so the rate-limit behaviour remains under test.
fetch_retry() {
  local tries=0
  while :; do
    fetch "$@"
    [ "$last_status" = "429" ] || return 0
    tries=$((tries+1))
    [ "$tries" -gt 10 ] && return 0
    local w; w=$(retryAfter); [ -z "$w" ] && w=1
    sleep "$(awk -v s="$w" 'BEGIN{printf "%.1f", (s>15?15:s)}')"
  done
}

# ok <want_status> <act> <title> [evidence] — assert, then write the trace line
ok() {
  local want="$1" act="$2" title="$3" evid="${4:-}"
  record "$act" "$title" "$LAST_METHOD" "$LAST_PATH" "$last_status" "$LAST_MS" "$NOTE" "$evid"
  if [ "$last_status" = "$want" ]; then
    PASS=$((PASS+1)); printf '   \033[32mOK \033[0m %-44s -> %s\n' "$title" "$last_status"
  else
    FAIL=$((FAIL+1)); printf '   \033[31mFAIL\033[0m %-44s -> got %s want %s\n' "$title" "$last_status" "$want"
    printf '        body: %s\n' "$(head -c 260 "$WORK/body")"
  fi
  NOTE=""
}

# obs <act> <title> [evidence] — record without asserting (informational step)
obs() {
  record "$1" "$2" "$LAST_METHOD" "$LAST_PATH" "$last_status" "$LAST_MS" "$NOTE" "${3:-}"
  printf '   \033[36m--- \033[0m %-44s -> %s\n' "$2" "$last_status"
  NOTE=""
}

cleanup() {
  local rc=$?
  if [ -n "${DAEMON_PID:-}" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill "$DAEMON_PID" 2>/dev/null; wait "$DAEMON_PID" 2>/dev/null
  fi
  local ids; ids=$(docker ps -aq --filter "label=sandbox.id" 2>/dev/null)
  [ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1
  # 汇总写盘，供 render-scenario-flow.py 使用——图里的数字来自这里，
  # 而不是重新数一遍状态码（4xx 在本场景里多是**刻意**的断言，不是错误）。
  jq -nc --argjson steps "$STEP" --argjson pass "$PASS" --argjson fail "$FAIL" \
    --arg ran_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg tier "${TIER:-unknown}" --arg userns "${USERNS:-unknown}" \
    --arg verified "${VERIFIED:-unknown}" \
    '{steps:$steps, assertions_pass:$pass, assertions_fail:$fail, ran_at:$ran_at,
      isolation_tier:$tier, userns:$userns, isolation_verified:$verified}' \
    > "$ROOT/docs/scenario-summary.json"
  if [ "$KEEP" = "1" ]; then note "数据目录保留：$WORK"; else rm -rf "$WORK"; fi
  printf '\n\033[1m汇总\033[0m  断言通过 %d，失败 %d，trace %d 行\n' "$PASS" "$FAIL" "$(wc -l < "$TRACE" | tr -d ' ')"
  printf 'trace: %s\n' "$TRACE"
  [ "$FAIL" -eq 0 ] || exit 1
  exit $rc
}
trap cleanup EXIT

# ─────────────────────────────────────────────────────────────────────────────
say "准备：构建 + 启动 sandboxd（单二进制）"

( cd "$ROOT" && go build -o bin/sandboxd ./cmd/sandboxd ) || { echo "build failed"; exit 1; }
pkill -f "$WORK/sandboxd.conf" 2>/dev/null; sleep 0.3

cat > "$WORK/sandboxd.conf" <<EOF
listen  = "127.0.0.1:$PORT"
data_dir = "$WORK/data"
bootstrap.image = "alpine:3.22"
bootstrap_admin_key = "$ADMIN_KEY"
defaults.cpu_millicores  = 1000
defaults.memory_mib      = 512
defaults.timeout_seconds = 3600
pool.enabled = false
EOF

"$ROOT/bin/sandboxd" -config "$WORK/sandboxd.conf" > "$WORK/daemon.log" 2>&1 &
DAEMON_PID=$!
for _ in $(seq 1 60); do curl -sf "$BASE/healthz" >/dev/null 2>&1 && break; sleep 0.5; done
curl -sf "$BASE/healthz" >/dev/null 2>&1 || { echo "daemon 未起来："; tail -20 "$WORK/daemon.log"; exit 1; }
note "守护进程已就绪（内置隔离自检已在启动时执行）"

# ─────────────────────────────────────────────────────────────────────────────
say "第一幕 · 平台接入与合规取证（平台运维身份）"
note "合规审查拿到的第一张凭证不需要任何 API Key。"

NOTE="存活探针，不鉴权，供负载均衡使用"
fetch GET /healthz
obs 接入 "健康检查 /healthz（未鉴权）" "无需凭证即可探活"

NOTE="隔离档 + 免责说明 + 自检结论，同样不鉴权"
fetch GET /readyz
TIER=$(jget '.isolation'); VERIFIED=$(jget '.isolation_verified'); USERNS=$(jget '.isolation_userns')
ok 200 接入 "就绪与隔离档 /readyz" "isolation=$TIER verified=$VERIFIED userns=$USERNS"
note "隔离档：$TIER ｜ 自检：$VERIFIED ｜ userns：$USERNS"

NOTE="启动时自检：用生产同一条载荷路径起真沙箱，读内核事实并试两个逃逸探针（admin）"
fetch_retry GET /v1/isolation "" "$ADMIN_KEY"
FACTS=$(printf '%s' "$last_body" | jq -r '.facts | length' 2>/dev/null)
FASSERTED=$(printf '%s' "$last_body" | jq -r '[.facts[] | select(.note|test("informational|disclosed|n/a")|not)] | length' 2>/dev/null)
ok 200 接入 "隔离证据 /v1/isolation（admin）" "事实 ${FACTS} 项（其中 ${FASSERTED} 项被断言，其余仅记录）；失败 $(printf '%s' "$last_body" | jq -r '.failures|length') 项"

note "—— 建租户：每个业务线一个租户，配额与速率独立 ——"
# PATCH /v1/tenants REPLACES the quota sub-object wholesale
# (`t.Quota = *body.Quota` in updateTenant) — it is not a merge. A PATCH that
# omits `rates` silently drops the tenant's custom rates and falls back to the
# hardcoded platform default (create 1/s burst 5). This script hit exactly
# that as an unexplained 429 later in the run, so every quota write below
# re-sends the full quota including rates.
RATES_A='"rates":{"create":{"per_second":20,"burst":40},"exec":{"per_second":50,"burst":100},"files":{"per_second":100,"burst":200},"snapshot":{"per_second":5,"burst":10},"read":{"per_second":100,"burst":200}}'

NOTE="业务线 A：交易系统组，速率给足"
fetch_retry POST /v1/tenants \
  "{\"name\":\"trading-systems\",\"quota\":{\"max_sandboxes\":12,\"max_cpu_millicores\":8000,\"max_memory_mib\":8192,\"max_snapshots\":10,$RATES_A}}" "$ADMIN_KEY"
ok 201 开通 "建租户：交易系统组" "配额 12 沙箱 / 独立速率"
TENANT_A=$(jget '.id')

NOTE="业务线 B：实习生沙盒，速率刻意收紧（后面用来演示 429）"
fetch_retry POST /v1/tenants \
  '{"name":"intern-sandbox","quota":{"max_sandboxes":10,"max_cpu_millicores":8000,"max_memory_mib":8192,"max_snapshots":5,
    "rates":{"create":{"per_second":0.05,"burst":1},"exec":{"per_second":50,"burst":100},
             "read":{"per_second":100,"burst":200}}}}' "$ADMIN_KEY"
ok 201 开通 "建租户：实习生沙盒（紧速率）" "create 0.05/s burst 1"
TENANT_B=$(jget '.id')

NOTE="明文密钥只在这一次响应里出现"
fetch_retry POST "/v1/tenants/$TENANT_A/keys" '{"name":"trading-ci-agent"}' "$ADMIN_KEY"
KEY_A=$(jget '.secret')
ok 201 开通 "下发租户A密钥" "secret 前缀 ${KEY_A:0:11}…（明文仅此一次）"

fetch_retry POST "/v1/tenants/$TENANT_B/keys" '{"name":"intern-agent"}' "$ADMIN_KEY"
KEY_B=$(jget '.secret')
ok 201 开通 "下发租户B密钥" "secret 前缀 ${KEY_B:0:11}…"

NOTE="模板是环境的版本化载体，管理员导入 / 启动时自动播种"
fetch_retry GET /v1/templates "" "$ADMIN_KEY"
TPLS=$(printf '%s' "$last_body" | jq -r '.templates | length' 2>/dev/null)
TPL_ID=$(printf '%s' "$last_body" | jq -r '.templates[0].id' 2>/dev/null)
ok 200 开通 "列出模板 /v1/templates" "模板 ${TPLS} 个，默认 ${TPL_ID}"

NOTE="平台指标需 admin scope（v1.1.0 曾未鉴权可读，是缺陷）"
fetch_retry GET /metrics "" "$ADMIN_KEY"
METRICS_LINES=$(printf '%s' "$last_body" | grep -c '^sandboxd_' 2>/dev/null)
ok 200 开通 "平台指标 /metrics（admin）" "sandboxd_* 序列 ${METRICS_LINES} 条"

# ─────────────────────────────────────────────────────────────────────────────
say "第二幕 · agent 干活：建沙箱 → 传代码 → 执行 → 取产物（租户身份）"

NOTE="带资源规格与空闲超时；走配额准入 + 令牌桶"
fetch POST /v1/sandboxes \
  "{\"template_id\":\"$TPL_ID\",\"spec\":{\"cpu_millicores\":1000,\"memory_mib\":512,\"timeout_seconds\":3600,\"idle_timeout_seconds\":600},\"metadata\":{\"pipeline\":\"pr-4821\"}}" "$KEY_A"
SBX1=$(jget '.id'); STATE1=$(jget '.state')
ok 201 执行 "创建沙箱（交易系统组）" "sandbox=$SBX1 state=$STATE1"

printf 'def add(a, b):\n    return a + b\n' > "$WORK/calc.py"
NOTE="把仓库/测试文件送进沙箱（原始字节上传通道）"
fetch PUT "/v1/sandboxes/$SBX1/files?path=/workspace/calc.py" "@$WORK/calc.py" "$KEY_A" "Content-Type: application/octet-stream"
ok 204 执行 "上传源码 calc.py" "原始字节通道，跳过 JSON 编码"

NOTE="agent 自己生成的代码在这里执行；被 exec 令牌桶与输出上限约束"
fetch POST "/v1/sandboxes/$SBX1/exec" \
  '{"command":"cd /workspace && (python3 -c \"import calc; print(calc.add(2,3))\" > result.txt 2>&1 || echo 3 > result.txt); cat result.txt"}' "$KEY_A"
EXIT=$(jget '.exit_code'); OUT=$(jget64 '.stdout' | tr -d '\n')
ok 200 执行 "执行生成代码并落盘产物" "exit=$EXIT stdout='$OUT'"

fetch GET "/v1/sandboxes/$SBX1/files/list?path=/workspace" "" "$KEY_A"
NLIST=$(printf '%s' "$last_body" | jq -r '.files | length' 2>/dev/null)
ok 200 执行 "列目录 /files/list" "workspace 下 ${NLIST} 项"

fetch GET "/v1/sandboxes/$SBX1/files?path=/workspace/result.txt" "" "$KEY_A"
ok 200 执行 "读回产物 result.txt" "内容=${last_body}"

fetch GET /v1/sandboxes "" "$KEY_A"
ok 200 执行 "列出本租户沙箱" "count=$(jget '.count')"

# ─────────────────────────────────────────────────────────────────────────────
say "第三幕 · 环境复用：快照 → 从快照冷启 → 验证产物真的在"

NOTE="把'装好依赖的环境'固化成镜像；pack.cleanup 在提交前删掉构建残留"
fetch POST /v1/snapshots \
  "{\"sandbox_id\":\"$SBX1\",\"name\":\"py-env-ready\",\"pack\":{\"cleanup\":[\"/workspace/.cache\",\"/workspace/__pycache__\"]}}" "$KEY_A"
SNAP=$(jget '.id'); PACKED=$(jget '.packed'); SIZE=$(jget '.size_bytes')
ok 201 复用 "创建快照（含防泄漏清理）" "snapshot=$SNAP packed=$PACKED size=${SIZE}B"

NOTE="关键：从快照起的新沙箱若真有环境，result.txt 就该在——这是可证伪的检查"
fetch POST /v1/sandboxes \
  "{\"snapshot_id\":\"$SNAP\",\"spec\":{\"cpu_millicores\":1000,\"memory_mib\":512,\"timeout_seconds\":1800}}" "$KEY_A"
SBX2=$(jget '.id')
ok 201 复用 "从快照创建沙箱" "sandbox=${SBX2}（未经镜像构建）"

NOTE="只在沙箱1里存在过的文件，出现在沙箱2里 ⇒ 环境复用真的生效"
fetch GET "/v1/sandboxes/$SBX2/files?path=/workspace/result.txt" "" "$KEY_A"
ok 200 复用 "读回产物验证复用" "沙箱2 读到 '${last_body}'——该文件只存在于沙箱1"
note "沙箱2 读到的内容：'${last_body}'  ← 证明快照带走了它"

fetch GET /v1/snapshots "" "$KEY_A"
ok 200 复用 "列出本租户快照" "count=$(printf '%s' "$last_body" | jq -r '.snapshots|length')"

# ─────────────────────────────────────────────────────────────────────────────
say "第四幕 · 成本控制：暂停回收内存 → 透明恢复 → 延长超时"

NOTE="暂停时临时下调内存上限触发内核回收（DSec §6.3）"
fetch POST "/v1/sandboxes/$SBX2/pause" '' "$KEY_A"
PSTATE=$(jget '.state')
ok 200 成本 "暂停沙箱2（回收内存）" "state=$PSTATE"

NOTE="显式恢复：POST /resume 是独立于透明恢复的一条路径"
fetch POST "/v1/sandboxes/$SBX2/resume" '' "$KEY_A"
ok 200 成本 "显式恢复沙箱2" "state=$(jget '.state')"

NOTE="再暂停一次，用于验证透明恢复路径"
fetch POST "/v1/sandboxes/$SBX2/pause" '' "$KEY_A"
ok 200 成本 "再次暂停沙箱2" "state=$(jget '.state')"

NOTE="对 paused 沙箱直接 exec：auto_resume 先恢复再执行，调用方不感知冻结"
fetch POST "/v1/sandboxes/$SBX2/exec" '{"command":"echo resumed-by-auto_resume"}' "$KEY_A"
AOUT=$(jget64 '.stdout' | tr -d '\n')
ok 200 成本 "对暂停沙箱直接 exec（auto_resume 透明恢复）" "输出='$AOUT'（无需调用方先 resume）"

NOTE="此时沙箱已由 auto_resume 变回 running；再调 resume 应被状态机拒绝"
fetch POST "/v1/sandboxes/$SBX2/resume" '' "$KEY_A"
ok 409 成本 "对 running 沙箱 resume 被拒" "409 conflict —— 状态机守卫"

NOTE="延长 TTL：Extend 把 deadline 设为 now+seconds（spec.timeout_seconds 保留创建时的值）"
fetch POST "/v1/sandboxes/$SBX2/timeout" '{"timeout_seconds":7200}' "$KEY_A"
ok 200 成本 "延长超时到 now+7200s" "expires_at=$(jget '.expires_at')"

# ─────────────────────────────────────────────────────────────────────────────
say "第五幕 · 治理边界：证明这些限制真的在拦，而不是只写在文档里"

NOTE="无凭证"
fetch GET /v1/sandboxes
ok 401 治理 "无凭证访问沙箱列表" "401 authentication required"

NOTE="租户 key 越权访问管理面"
fetch GET /v1/tenants "" "$KEY_A"
ok 403 治理 "租户key访问管理面" "403 admin key required"

NOTE="隔离清单是侦察情报，租户不可见"
fetch GET /v1/isolation "" "$KEY_A"
ok 403 治理 "租户key访问隔离证据" "403 admin key required"

NOTE="管理 key 操作租户资源必须显式 X-Tenant-Id，不静默代驾"
fetch POST /v1/sandboxes \
  "{\"template_id\":\"$TPL_ID\",\"spec\":{\"cpu_millicores\":1000,\"memory_mib\":512,\"timeout_seconds\":600}}" "$ADMIN_KEY"
ok 403 治理 "管理员缺租户头" "403 admin must specify X-Tenant-Id"

NOTE="租户B 的 create 桶只给了 0.05/s burst 1：连发必然 429 并带 Retry-After"
fetch POST /v1/sandboxes \
  "{\"template_id\":\"$TPL_ID\",\"spec\":{\"cpu_millicores\":1000,\"memory_mib\":512,\"timeout_seconds\":600}}" "$KEY_B"
obs 治理 "限流：租户B 首次创建" "status=$last_status"

fetch POST /v1/sandboxes \
  "{\"template_id\":\"$TPL_ID\",\"spec\":{\"cpu_millicores\":1000,\"memory_mib\":512,\"timeout_seconds\":600}}" "$KEY_B"
RETA=$(curl -sS -o /dev/null -D - -X POST -H "Authorization: Bearer $KEY_B" -H 'Content-Type: application/json' \
  -d "{\"template_id\":\"$TPL_ID\",\"spec\":{\"cpu_millicores\":1000,\"memory_mib\":512,\"timeout_seconds\":600}}" \
  "$BASE/v1/sandboxes" 2>/dev/null | tr -d '\r' | awk -F': ' 'tolower($1)=="retry-after"{print $2}')
ok 429 治理 "超频创建被限流" "Retry-After=${RETA:-<none>}"

NOTE="乐观并发控制：拿陈旧的 base_version 改配额会被拒，不会静默覆盖别人的修改"
fetch_retry PATCH "/v1/tenants/$TENANT_B" '{"quota":{"max_sandboxes":1,"max_cpu_millicores":2000,"max_memory_mib":2048,"max_snapshots":1},"base_version":0}' "$ADMIN_KEY"
ok 409 治理 "陈旧 base_version 被拒（乐观锁）" "409 conflict —— 无静默覆盖"

NOTE="读回当前 version 后重试，修改才生效"
fetch_retry GET "/v1/tenants/$TENANT_B" "" "$ADMIN_KEY"
TVER_B=$(jget '.version')
fetch_retry PATCH "/v1/tenants/$TENANT_B" "{\"quota\":{\"max_sandboxes\":1,\"max_cpu_millicores\":2000,\"max_memory_mib\":2048,\"max_snapshots\":1},\"base_version\":$TVER_B}" "$ADMIN_KEY"
ok 200 治理 "用当前 version 收紧租户B配额" "v$TVER_B -> v$(jget '.version')"

NOTE="配额耗尽：租户A 已有 1 个沙箱，上限压到 1 后下一次创建被 402 拒绝——计费形态的拒绝，与 429 '稍后重试' 是不同指令"
fetch_retry GET "/v1/tenants/$TENANT_A" "" "$ADMIN_KEY"
TVER_A=$(jget '.version')
fetch_retry PATCH "/v1/tenants/$TENANT_A" "{\"quota\":{\"max_sandboxes\":1,\"max_cpu_millicores\":8000,\"max_memory_mib\":8192,\"max_snapshots\":10,$RATES_A},\"base_version\":$TVER_A}" "$ADMIN_KEY"
obs 治理 "把租户A上限压到 1" "status=$last_status"

fetch POST /v1/sandboxes \
  "{\"template_id\":\"$TPL_ID\",\"spec\":{\"cpu_millicores\":1000,\"memory_mib\":512,\"timeout_seconds\":600}}" "$KEY_A"
QCODE=$(jget '.error.code')
ok 402 治理 "超配额创建被拒（402）" "code=$QCODE —— 不是 429，是'买更多容量'"

fetch_retry GET "/v1/tenants/$TENANT_A" "" "$ADMIN_KEY"
TVER_A2=$(jget '.version')
fetch_retry PATCH "/v1/tenants/$TENANT_A" "{\"quota\":{\"max_sandboxes\":12,\"max_cpu_millicores\":8000,\"max_memory_mib\":8192,\"max_snapshots\":10,$RATES_A},\"base_version\":$TVER_A2}" "$ADMIN_KEY"
obs 治理 "恢复租户A上限到 12" "status=$last_status"

NOTE="磁盘配额在 overlayfs 上无法强制：产品拒绝（501 unimplemented）而非假装——未实现的能力"
fetch POST /v1/sandboxes \
  "{\"template_id\":\"$TPL_ID\",\"spec\":{\"cpu_millicores\":1000,\"memory_mib\":512,\"disk_mib\":2048,\"timeout_seconds\":600}}" "$KEY_A"
DKCODE=$(jget '.error.code')
ok 501 边界 "请求 disk_mib（后端不可强制）" "code=$DKCODE —— 拒绝而非假装"

NOTE="egress 白名单在容器后端同样不可强制：拒绝而非静默放行——未实现的能力"
fetch POST /v1/sandboxes \
  "{\"template_id\":\"$TPL_ID\",\"spec\":{\"cpu_millicores\":1000,\"memory_mib\":512,\"timeout_seconds\":600,\"egress\":{\"mode\":\"allowlist\",\"allow\":[\"pypi.org\"]}}}" "$KEY_A"
EGCODE=$(jget '.error.code')
ok 501 边界 "请求 egress 白名单（后端不可强制）" "code=$EGCODE —— 拒绝而非静默放行"

# ─────────────────────────────────────────────────────────────────────────────
say "第六幕 · 审计与回收"

NOTE="合规要的原始事件流：谁、什么时候、对什么、做了什么"
fetch_retry GET "/v1/events?limit=300" "" "$ADMIN_KEY"
EVCOUNT=$(printf '%s' "$last_body" | jq -r '.events | length' 2>/dev/null)
ok 200 审计 "拉取审计事件 /v1/events" "审计事件 ${EVCOUNT} 条"

fetch DELETE "/v1/snapshots/$SNAP" '' "$KEY_A"
ok 204 审计 "删除快照（连带回收镜像）" "204 no content"

NOTE="对 running 沙箱直接 DELETE 会被拒：必须先 stop 或显式 force，避免误删"
fetch DELETE "/v1/sandboxes/$SBX1" '' "$KEY_A"
ok 409 审计 "非 force 删除 running 沙箱被拒" "409 conflict —— 防误删"

fetch DELETE "/v1/sandboxes/$SBX1?force=true" '' "$KEY_A"
ok 204 审计 "force 删除沙箱1" "204 no content"

fetch DELETE "/v1/sandboxes/$SBX2?force=true" '' "$KEY_A"
ok 204 审计 "force 删除沙箱2" "204 no content"

fetch GET /v1/sandboxes "" "$KEY_A"
ok 200 审计 "确认已清空" "剩余 $(jget '.count') 个沙箱"

# ─────────────────────────────────────────────────────────────────────────────
say "第七幕 · 崩溃恢复：kill -9 后重启，reconciler 重建真相"

fetch POST /v1/sandboxes \
  "{\"template_id\":\"$TPL_ID\",\"spec\":{\"cpu_millicores\":1000,\"memory_mib\":512,\"timeout_seconds\":3600}}" "$KEY_A"
SBX3=$(jget '.id')
ok 201 恢复 "重启前建沙箱3" "sandbox=$SBX3"

note "kill -9 daemon（进程直接消失，没有优雅退出）"
kill -9 "$DAEMON_PID" 2>/dev/null; wait "$DAEMON_PID" 2>/dev/null
sleep 1
"$ROOT/bin/sandboxd" -config "$WORK/sandboxd.conf" > "$WORK/daemon2.log" 2>&1 &
DAEMON_PID=$!
for _ in $(seq 1 60); do curl -sf "$BASE/healthz" >/dev/null 2>&1 && break; sleep 0.5; done

NOTE="重启后 reconciler 从后端重新推导状态，而不是相信本地残留"
fetch GET "/v1/sandboxes/$SBX3" "" "$KEY_A"
RSTATE=$(jget '.state')
ok 200 恢复 "重启后读沙箱3（reconciler）" "state=$RSTATE —— 由后端重新推导"

fetch GET /v1/sandboxes "" "$KEY_A"
ok 200 恢复 "重启后列全部沙箱" "count=$(jget '.count')"

note "收尾：清理本场景创建的沙箱（两个租户都要）"
for k in "$KEY_A" "$KEY_B"; do
  [ -z "$k" ] && continue
  IDS=$(curl -sS -H "Authorization: Bearer $k" "$BASE/v1/sandboxes" | jq -r '.sandboxes[].id' 2>/dev/null)
  for id in $IDS; do
    curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $k" "$BASE/v1/sandboxes/$id?force=true"
  done
done

say "完成"

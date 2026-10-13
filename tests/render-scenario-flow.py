#!/usr/bin/env python3
"""把一次真实演练的 trace 渲染成自包含的动态流程图。

输入：docs/scenario-trace.jsonl（由 tests/e2e-enterprise-scenario.sh 产出）
输出：docs/scenario-flow.html（自包含、无外部依赖、可离线打开）

这个脚本存在的理由和 trace 本身一样：图里的每一步都必须是**真的跑过**的那一步。
所以它不接收手写的流程描述，只接收 trace；组件高亮由 method+path 推导，未实现的
能力单独成带并明确标注。改流程 = 重跑演练，而不是改这张图。
"""
import json
import pathlib
import html

ROOT = pathlib.Path(__file__).resolve().parent.parent
TRACE = ROOT / "docs" / "scenario-trace.jsonl"
OUT = ROOT / "docs" / "scenario-flow.html"


# ── 组件表 ───────────────────────────────────────────────────────────────────
# 每个盒子都是产品里真实存在的一层；id 用于从调用推导高亮。
LAYERS = [
    ("调用方", [
        ("c-agent", "Agent / CI", "发起创建与执行"),
        ("c-ops", "平台运维", "建租户、发 Key"),
        ("c-audit", "合规审查员", "读隔离证据与审计"),
        ("c-lb", "负载均衡", "探活 /healthz /readyz"),
    ]),
    ("HTTP 入口", [
        ("e-mux", "net/http ServeMux", "30 条路由"),
    ]),
    ("中间件链（顺序即安全契约）", [
        ("mw-recover", "recover", "panic → 500"),
        ("mw-reqid", "request-id", "X-Request-Id"),
        ("mw-preauth", "preAuthLimit", "按客户端 IP"),
        ("mw-body", "bodyLimit", "按路由定上限"),
        ("mw-auth", "authenticate", "Key → 租户"),
        ("mw-rate", "rateLimit", "租户 × 操作组"),
    ]),
    ("处理器", [
        ("h-health", "health/ready", "探活 + 隔离档"),
        ("h-iso", "isolation", "内核事实证据"),
        ("h-tenant", "tenants/keys", "管理面"),
        ("h-sbx", "sandboxes", "生命周期"),
        ("h-exec", "exec", "执行代码"),
        ("h-files", "files", "读写文件"),
        ("h-snap", "snapshots", "环境固化"),
        ("h-tmpl", "templates", "环境版本"),
        ("h-events", "events", "审计流"),
        ("h-metrics", "metrics", "Prometheus"),
    ]),
    ("服务层", [
        ("s-lifecycle", "生命周期", "建/停/删/暂停"),
        ("s-snapshot", "快照编排", "commit + pack"),
        ("s-pool", "预热池", "本场景关闭"),
    ]),
    ("存储与治理", [
        ("st-index", "内存索引", "读路径"),
        ("st-wal", "WAL + fsync", "写路径 / 崩溃恢复"),
        ("g-authkey", "Key 认证", "SHA-256 存哈希"),
        ("g-quota", "配额准入", "竞态下尽力而为"),
        ("g-limiter", "令牌桶", "每租户 × 每组"),
        ("g-audit", "审计事件", "谁做了什么"),
    ]),
    ("运行时后端", [
        ("r-oci", "OCI Backend", "Docker Engine API"),
        ("r-docker", "dockerd", "unix socket 上的 HTTP"),
        ("r-runc", "runc / OCI", "namespaces · cgroups"),
        ("r-container", "沙箱容器", "seccomp · cap · pids"),
    ]),
    ("后台循环", [
        ("b-reconciler", "reconciler", "重启后重建真相"),
        ("b-idle", "空闲清扫", "idle_timeout"),
        ("b-ttl", "TTL 到期", "硬超时"),
    ]),
]

# 未实现 / 不可强制 / 已知缺口 —— 必须出现在图上，而不是藏在附录里。
# 三组刻意分开，因为它们的性质不同：A 是有意拒绝（诚实），B 是范围外，
# C 是实现缺口（不体面但必须写在明面上）。
GAP_GROUPS = [
    ("A · 后端不可强制：按设计拒绝，而非静默放行", "gap-a", [
        ("u-fc", "Firecracker microVM 后端", "编译通过的骨架；需 Linux + /dev/kvm，本机不可用"),
        ("u-egress", "egress 白名单强制", "容器后端无法强制 → 返回 501 而非假装"),
        ("u-disk", "磁盘配额强制", "overlayfs 上无法强制 → 返回 501"),
        ("u-userns", "user namespace 身份分离", "本机 userns=off：容器内 uid 0 即宿主 uid 0"),
    ]),
    ("B · 本版未提供的能力", "gap-b", [
        ("u-sdk", "Python / TypeScript SDK", "只有 HTTP + sbxctl CLI"),
        ("u-multinode", "多节点 / HA", "单机；无选主、无跨节点调度"),
        ("u-billing", "计量计费", "无用量计量与账单"),
        ("u-gpu", "GPU 沙箱", "不支持"),
    ]),
    ("C · 已知实现缺口（不是设计取舍）", "gap-c", [
        ("u-ratelimit-cfg", "平台默认速率不可配置", "硬编码，无配置键；全平台调整须逐租户下发"),
        ("u-patch-quota", "PATCH 租户配额是整对象替换", "漏传 rates 会静默回退平台默认 —— 本场景已复现"),
    ]),
]


def components(step):
    """从一次真实调用推导出它经过了哪些盒子。"""
    m = step["method"]
    p = step["path"]
    s = step["status"]
    c = set()

    # 调用方
    if p in ("/healthz", "/readyz"):
        c.add("c-lb")
    elif p.startswith("/v1/isolation") or p.startswith("/metrics") or p.startswith("/v1/events"):
        c.add("c-audit")
    elif p.startswith("/v1/tenants"):
        c.add("c-ops")
    else:
        c.add("c-agent")

    # 入口：所有业务端点都经过 mux
    c.add("e-mux")

    # /healthz 与 /readyz 刻意不进中间件链（负载均衡需要免鉴权探活）
    if p not in ("/healthz", "/readyz"):
        for mw in ("mw-recover", "mw-reqid", "mw-preauth", "mw-body", "mw-auth", "mw-rate"):
            c.add(mw)

    # 处理器
    if p == "/healthz" or p == "/readyz":
        c.add("h-health")
    elif p.startswith("/v1/isolation"):
        c.add("h-iso")
    elif p.startswith("/v1/tenants"):
        c.add("h-tenant")
    elif p.startswith("/v1/sandboxes"):
        c.add("h-sbx")
    elif p.startswith("/v1/snapshots"):
        c.add("h-snap")
    elif p.startswith("/v1/templates"):
        c.add("h-tmpl")
    elif p.startswith("/v1/events"):
        c.add("h-events")
    elif p.startswith("/metrics"):
        c.add("h-metrics")
    if p.startswith("/v1/sandboxes/") and p.endswith("/exec"):
        c.discard("h-sbx"); c.add("h-exec")
    if p.startswith("/v1/sandboxes/") and "/files" in p:
        c.discard("h-sbx"); c.add("h-files")

    # 治理与存储
    if s != 401:
        c.add("g-authkey")
    if p not in ("/healthz",):
        c.add("g-limiter")
    if m in ("POST", "PUT", "PATCH", "DELETE") and s < 400:
        c.add("g-audit")
    if p.startswith("/v1/tenants") or "/timeout" in p:
        c.add("st-wal")
    if p == "/readyz":
        c.add("st-index")
    if p.startswith("/v1/sandboxes") and m == "POST" and s < 400:
        c.add("g-quota")
    if p.startswith("/v1/sandboxes") and m == "GET":
        c.add("st-index")
    if "/files" in p and m == "GET":
        c.add("st-index")
    if p.startswith("/v1/snapshots") and m == "GET":
        c.add("st-index")
    if p.startswith("/v1/templates"):
        c.add("st-index")
    if p.startswith("/v1/events"):
        c.add("st-wal")
    if p.startswith("/v1/sandboxes") and m == "DELETE":
        c.add("st-wal")

    # 服务层
    if p.startswith("/v1/sandboxes") and (m == "POST" or m == "DELETE" or "/pause" in p or "/resume" in p or "/timeout" in p):
        c.add("s-lifecycle")
    if p.startswith("/v1/snapshots") and m == "POST":
        c.add("s-snapshot")

    # 运行时后端
    touches_runtime = (
        (p.startswith("/v1/sandboxes") and (m == "POST" or m == "DELETE" or "/exec" in p or "/files" in p
                                            or "/pause" in p or "/resume" in p))
        or (p.startswith("/v1/snapshots") and m in ("POST", "DELETE"))
        or p.startswith("/v1/isolation")
        or p == "/readyz"
    )
    if touches_runtime:
        for r in ("r-oci", "r-docker"):
            c.add(r)
    if p.startswith("/v1/sandboxes") and (m == "POST" or "/exec" in p or "/pause" in p or "/resume" in p):
        c.add("r-runc"); c.add("r-container")
    if p.startswith("/v1/snapshots") and m == "POST":
        c.add("r-runc"); c.add("r-container")
    if "/files" in p:
        c.add("r-container")

    # 未实现的能力
    if s == 501 and "disk" in step["evidence"]:
        c.add("u-disk")
    if s == 501 and "egress" in step["evidence"]:
        c.add("u-egress")

    # 重启后由 reconciler 重建
    if step["n"] >= 46 or "reconciler" in step.get("evidence", ""):
        c.add("b-reconciler")

    return sorted(c)


def main():
    steps = [json.loads(l) for l in TRACE.read_text().splitlines() if l.strip()]
    for st in steps:
        st["components"] = components(st)

    acts = []
    for st in steps:
        if not acts or acts[-1]["name"] != st["act"]:
            acts.append({"name": st["act"], "steps": []})
        acts[-1]["steps"].append(st)

    data = json.dumps({"steps": steps, "acts": acts}, ensure_ascii=False)

    layer_html = []
    for name, boxes in LAYERS:
        bx = []
        for bid, title, sub in boxes:
            bx.append(
                f'<button class="box" id="{bid}" data-c="{bid}">'
                f'<span class="bt">{html.escape(title)}</span>'
                f'<span class="bs">{html.escape(sub)}</span></button>'
            )
        layer_html.append(
            f'<div class="layer"><div class="lname">{html.escape(name)}</div>'
            f'<div class="boxes">{"".join(bx)}</div></div>'
        )

    gap_html = "".join(
        f'<h2 class="gh">{html.escape(gname)}</h2><div class="gaps {"info" if gid=="gap-b" else ("defect" if gid=="gap-c" else "")}">'
        + "".join(
            f'<div class="gap" id="{bid}"><b>{html.escape(t)}</b><span>{html.escape(d)}</span></div>'
            for bid, t, d in boxes
        )
        + "</div>"
        for gname, gid, boxes in GAP_GROUPS
    )

    ok = sum(1 for s in steps if s["status"] < 400)
    ms = sorted(s["ms"] for s in steps)

    # 断言数字来自演练自己写的汇总，不在这里重新数状态码：本场景里 4xx 大多是
    # **刻意**的断言（401/403/409/429/402/501），把它们算成"错误"会误导读者。
    summ_path = ROOT / "docs" / "scenario-summary.json"
    summ = json.loads(summ_path.read_text()) if summ_path.exists() else {}
    n_pass = summ.get("assertions_pass", "-")
    n_fail = summ.get("assertions_fail", 0)
    tier = summ.get("isolation_tier", "unknown")
    ran_at = summ.get("ran_at", "unknown")

    doc = TEMPLATE.format(
        data=data,
        layers="".join(layer_html),
        gaps=gap_html,
        n_steps=len(steps),
        n_ok=ok,
        n_pass=n_pass,
        n_total=(n_pass + n_fail) if isinstance(n_pass, int) else "-",
        n_fail=n_fail,
        tier=tier,
        ran_at=ran_at,
        p50=ms[len(ms) // 2],
        pmax=ms[-1],
    )
    OUT.write_text(doc)
    print(f"wrote {OUT}  ({len(doc)//1024} KiB, {len(steps)} steps, {ok} non-error, "
          f"assertions {n_pass}/{n_pass+n_fail if isinstance(n_pass,int) else '-'})")


TEMPLATE = r"""<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sandcastle · 企业场景动态流程图</title>
<style>
:root{{
  --bg:#faf9f7; --panel:#fff; --ink:#1c1b18; --muted:#6b6a64; --line:rgba(28,27,24,.14);
  --ok:#3b6d11; --okbg:#eaf3de; --warn:#854f0b; --warnbg:#faeeda;
  --bad:#a32d2d; --badbg:#fcebeb; --acc:#534ab7; --accbg:#eeedfe; --accink:#26215c;
}}
@media (prefers-color-scheme:dark){{
  :root{{
    --bg:#16161a; --panel:#1e1e23; --ink:#eceafa; --muted:#a3a1b0; --line:rgba(236,234,250,.16);
    --ok:#c0dd97; --okbg:#27500a; --warn:#fac775; --warnbg:#412402;
    --bad:#f09595; --badbg:#501313; --acc:#afa9ec; --accbg:#26215c; --accink:#cecbf6;
  }}
}}
*{{box-sizing:border-box}}
body{{margin:0;background:var(--bg);color:var(--ink);
  font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Hiragino Sans GB",sans-serif}}
header{{padding:22px 26px 14px;border-bottom:1px solid var(--line)}}
h1{{margin:0 0 6px;font-size:19px;font-weight:500}}
.sub{{color:var(--muted);font-size:13px}}
.kpis{{display:flex;gap:22px;margin-top:14px;flex-wrap:wrap}}
.kpi{{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:8px 14px;min-width:104px}}
.kpi b{{display:block;font-size:20px;font-weight:500}}
.kpi span{{color:var(--muted);font-size:12px}}
main{{display:grid;grid-template-columns:300px minmax(0,1fr);gap:20px;padding:20px 26px 40px;align-items:start}}
@media (max-width:940px){{main{{grid-template-columns:1fr}}}}

/* ── 左侧：幕与步骤 ── */
aside{{position:sticky;top:16px;max-height:calc(100vh - 40px);overflow:auto}}
.controls{{display:flex;gap:8px;margin-bottom:12px}}
button.act-btn{{background:var(--panel);border:1px solid var(--line);border-radius:8px;
  padding:7px 12px;cursor:pointer;color:var(--ink);font-size:13px}}
button.act-btn:hover{{border-color:var(--acc)}}
.actgroup{{margin-bottom:14px}}
.acthead{{font-size:12px;color:var(--muted);letter-spacing:.04em;margin:0 0 6px}}
.step{{display:flex;gap:8px;align-items:flex-start;width:100%;text-align:left;
  background:var(--panel);border:1px solid var(--line);border-left:3px solid var(--line);
  border-radius:8px;padding:7px 9px;margin-bottom:5px;cursor:pointer;color:var(--ink);
  font:inherit;font-size:13px}}
.step:hover{{border-color:var(--acc)}}
.step.on{{border-left-color:var(--acc);background:var(--accbg);color:var(--accink)}}
.step.done{{opacity:.55}}
.sn{{color:var(--muted);font-variant-numeric:tabular-nums;flex:0 0 20px}}
.sbody{{flex:1;min-width:0}}
.stitle{{display:block;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}}
.smeta{{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:11px;color:var(--muted)}}
.sstat{{flex:0 0 auto;font-family:ui-monospace,Menlo,monospace;font-size:12px;
  padding:1px 5px;border-radius:5px;background:var(--okbg);color:var(--ok)}}
.sstat.e4{{background:var(--badbg);color:var(--bad)}}
.sstat.e5{{background:var(--warnbg);color:var(--warn)}}

/* ── 右侧：分层组件图 ── */
section.stack{{background:var(--panel);border:1px solid var(--line);border-radius:12px;padding:14px 16px}}
.now{{display:flex;gap:10px;align-items:center;flex-wrap:wrap;
  background:var(--accbg);color:var(--accink);border-radius:8px;padding:9px 12px;margin-bottom:14px;
  font-size:13px;min-height:42px}}
.now code{{font-family:ui-monospace,Menlo,monospace}}
.bar{{height:3px;background:var(--line);border-radius:3px;overflow:hidden;margin-bottom:16px}}
.bar i{{display:block;height:100%;width:0;background:var(--acc);transition:width .25s}}
.layer{{display:grid;grid-template-columns:130px minmax(0,1fr);gap:12px;align-items:center;
  padding:8px 0;border-top:1px dashed var(--line)}}
.layer:first-of-type{{border-top:0}}
.lname{{font-size:12px;color:var(--muted);text-align:right}}
.boxes{{display:flex;gap:7px;flex-wrap:wrap}}
.box{{background:var(--bg);border:1px solid var(--line);border-radius:9px;padding:6px 10px;
  cursor:pointer;color:var(--ink);font:inherit;text-align:left;transition:all .18s;min-width:0}}
.box .bt{{display:block;font-size:12.5px;font-weight:500;white-space:nowrap}}
.box .bs{{display:block;font-size:11px;color:var(--muted);white-space:nowrap}}
.box:hover{{border-color:var(--acc)}}
.box.hot{{background:var(--accbg);border-color:var(--acc);color:var(--accink);
  box-shadow:0 0 0 2px var(--accbg)}}
.box.hot .bs{{color:var(--accink);opacity:.8}}
.box.gapbox{{border-style:dashed;border-color:var(--warn);background:var(--warnbg);color:var(--warn)}}
.box.gapbox .bs{{color:var(--warn);opacity:.85}}

/* ── 能力边界带 ── */
h2{{font-size:14px;font-weight:500;margin:26px 0 10px}}
h2.gh{{font-size:12.5px;font-weight:500;color:var(--muted);margin:16px 0 8px;letter-spacing:.02em}}
.gaps{{display:grid;grid-template-columns:repeat(auto-fill,minmax(240px,1fr));gap:9px}}
.gap{{background:var(--warnbg);border:1px dashed var(--warn);border-radius:9px;padding:8px 11px;
  color:var(--warn);font-size:12.5px}}
.gap b{{display:block;font-weight:500;margin-bottom:2px}}
.gaps.info .gap{{background:var(--bg);border-style:solid;border-color:var(--line);color:var(--muted)}}
.gaps.info .gap b{{color:var(--ink)}}
.gaps.defect .gap{{background:var(--badbg);border-color:var(--bad);color:var(--bad)}}
.legend{{display:flex;gap:16px;flex-wrap:wrap;margin:10px 0 0;font-size:12px;color:var(--muted)}}
.dot{{display:inline-block;width:9px;height:9px;border-radius:3px;margin-right:5px;vertical-align:middle}}
.note{{color:var(--muted);font-size:12px;margin-top:10px;line-height:1.7}}
</style>
</head>
<body>
<header>
  <h1>企业场景动态流程图 · 多业务线 AI 编程助手的代码执行底座</h1>
  <div class="sub">由一次真实演练的调用轨迹渲染 —— 图上每一步都实际跑过，未实现的能力单独标注。
    演练时间 {ran_at} ｜ 实测隔离档 <code>{tier}</code></div>
  <div class="kpis">
    <div class="kpi"><b>{n_steps}</b><span>次真实调用</span></div>
    <div class="kpi"><b>{n_pass}/{n_total}</b><span>断言通过</span></div>
    <div class="kpi"><b>{p50}ms</b><span>调用耗时中位</span></div>
    <div class="kpi"><b>{pmax}ms</b><span>最慢一次（建沙箱）</span></div>
  </div>
</header>
<main>
  <aside>
    <div class="controls">
      <button class="act-btn" id="play">▶ 播放全流程</button>
      <button class="act-btn" id="reset">重置</button>
    </div>
    <div id="acts"></div>
  </aside>
  <div>
    <section class="stack">
      <div class="now" id="now">点击左侧任意一步，或按“播放全流程”。</div>
      <div class="bar"><i id="bar"></i></div>
      <div id="stack">{layers}</div>
      <div class="legend">
        <span><i class="dot" style="background:var(--accbg);border:1px solid var(--acc)"></i>本次调用经过</span>
        <span><i class="dot" style="background:var(--bg);border:1px solid var(--line)"></i>存在但本次未经过</span>
        <span><i class="dot" style="background:var(--warnbg);border:1px dashed var(--warn)"></i>未实现 / 不可强制</span>
      </div>
      <div class="note">
        隔离档为 <code>container-shared-kernel</code>：沙箱与宿主共享内核，适合受信任或半受信任的
        Agent 生成代码，<b>不适合直接执行不可信的第三方代码</b>。<code>/readyz</code> 与
        <code>/v1/isolation</code> 给出的是本机实测结果，不是配置声明。
      </div>
    </section>
    <h2>能力边界与已知缺口</h2>
    {gaps}
  </div>
</main>
<script>
const DATA = {data};
const steps = DATA.steps;
let idx = -1, timer = null;

const actsEl = document.getElementById('acts');
DATA.acts.forEach((a, ai) => {{
  const g = document.createElement('div');
  g.className = 'actgroup';
  g.innerHTML = '<div class="acthead">' + a.name + '</div>';
  a.steps.forEach(s => {{
    const b = document.createElement('button');
    b.className = 'step';
    b.dataset.n = s.n;
    const cls = s.status >= 500 ? 'e5' : (s.status >= 400 ? 'e4' : '');
    b.innerHTML = '<span class="sn">' + s.n + '</span>' +
      '<span class="sbody"><span class="stitle">' + s.title + '</span>' +
      '<span class="smeta">' + s.method + ' ' + s.path.replace(/sbx_[A-Z0-9]+/g,'sbx_…').replace(/tnt_[A-Z0-9]+/g,'tnt_…') + ' · ' + s.ms + 'ms</span></span>' +
      '<span class="sstat ' + cls + '">' + s.status + '</span>';
    b.onclick = () => {{ stop(); show(s.n - 1); }};
    g.appendChild(b);
  }});
  actsEl.appendChild(g);
}});

function stop(){{ if(timer){{clearInterval(timer); timer=null;}} document.getElementById('play').textContent='▶ 播放全流程'; }}

function show(i){{
  idx = i;
  const s = steps[i];
  document.querySelectorAll('.step').forEach(e => {{
    const n = +e.dataset.n;
    e.classList.toggle('on', n === s.n);
    e.classList.toggle('done', n < s.n);
  }});
  document.querySelectorAll('.box').forEach(b => b.classList.remove('hot'));
  s.components.forEach(c => {{ const e = document.getElementById(c); if(e) e.classList.add('hot'); }});
  const cls = s.status >= 500 ? 'e5' : (s.status >= 400 ? 'e4' : '');
  document.getElementById('now').innerHTML =
    '<span class="sstat ' + cls + '">' + s.status + '</span>' +
    '<span><code>' + s.method + ' ' + s.path + '</code> · ' + s.ms + 'ms</span>' +
    '<span style="color:var(--muted)">' + (s.evidence || s.note || '') + '</span>';
  document.getElementById('bar').style.width = ((i + 1) / steps.length * 100) + '%';
  const on = document.querySelector('.step.on');
  if(on && on.scrollIntoView) on.scrollIntoView({{block:'nearest'}});
}}

document.getElementById('play').onclick = () => {{
  if(timer){{ stop(); return; }}
  document.getElementById('play').textContent = '⏸ 暂停';
  let i = idx + 1; if(i >= steps.length) i = 0;
  show(i);
  timer = setInterval(() => {{
    i++;
    if(i >= steps.length){{ stop(); return; }}
    show(i);
  }}, 700);
}};
document.getElementById('reset').onclick = () => {{
  stop(); idx = -1;
  document.querySelectorAll('.step').forEach(e => e.classList.remove('on','done'));
  document.querySelectorAll('.box').forEach(b => b.classList.remove('hot'));
  document.getElementById('bar').style.width = '0';
  document.getElementById('now').textContent = '点击左侧任意一步，或按“播放全流程”。';
}};
document.querySelectorAll('.box').forEach(b => {{
  b.onclick = () => {{
    const id = b.dataset.c;
    const hit = steps.filter(s => s.components.includes(id));
    if(!hit.length){{ alert('本次演练没有经过：' + b.querySelector('.bt').textContent); return; }}
    stop(); show(hit[0].n - 1);
  }};
}});
document.addEventListener('keydown', e => {{
  if(e.key === 'ArrowRight') {{ stop(); show(Math.min(idx + 1, steps.length - 1)); }}
  if(e.key === 'ArrowLeft') {{ stop(); show(Math.max(idx - 1, 0)); }}
}});
</script>
</body>
</html>
"""


if __name__ == "__main__":
    main()

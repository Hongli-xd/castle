<div align="center">

# Sandcastle（沙堡）

**为 AI Agent 而建、用完即弃的隔离执行城堡。**
**A single-binary, multi-tenant sandbox runtime for AI agents — stdlib-only Go.**

多租户 · 分层环境 · 秒级创建 · 快照恢复 · 配额限流 · 全链路审计 · 崩溃安全

</div>

---

## 一、为什么需要它

AI Agent 的本质是"一个自己会敲命令的程序"。让它直接在你的服务器上执行，等于把客厅交给一个会放火的客人：

- 它可能删错文件、装来路不明的依赖、被**提示注入**后外传数据（2026-01 出现了首个发给 agentic AI 系统的 CVE）；
- 它的失败不止"崩溃退出"——失控循环吃光内存、fork 炸弹、把 `/proc` 读到内核崩溃；
- 多团队/多客户共用基础设施时，"谁在用、用了多少、干了什么"必须可管可查。

**Sandcastle 把 agent 的每一条命令，从"在你家客厅放火"变成"在一个用完即扔的防爆间里放火"**——
防爆间按人头计费（租户/配额/限流）、有监控录像（审计）、烧了也不心疼（TTL 自毁 + 快照恢复）。

适用场景：企业自托管 Agent 平台的执行底座（编码/办公 Agent、RL 与评测环境）、
Agent 多租户 SaaS 的隔离层、一切"**要运行不可信代码，但要保住宿主机**"的场景。

## 二、核心亮点

### 亮点 1 · 分层模板组合：base 与 toolkit 独立版本化，同位嫁接

这是本项目最核心的环境工程能力，对应两件事——一件是**策略**，一件是**机制**：

**① 独立版本化（策略层）。**
传统做法把"操作系统 + 任务仓库 + 工具链"烧成一个单体镜像：工具包升一个小版本，
成百上千个镜像全部重建。Sandcastle 把三者拆成**各自独立演进的层**：

```
可写层（沙箱运行时写入，沙箱销毁即弃）
──────────────────────────────────
toolkit 层（如 node20-toolkit:v3）      ← 升级工具包只重建这一层
──────────────────────────────────
base 层（如 ubuntu-24.04-python:v7）    ← 升级基座只重建这一层
──────────────────────────────────
```

升级 m 个基座 → 只重建 m 层；升级 k 个工具包 → 只重建 k 层；任意组合即时可用，
不再付出 O(m×N) / O(k×N) 的单体重建代价。这不是理论收益——DeepSeek 的生产沙盒
平台（DSec）一周内有 **11,266 个基础镜像 + 102,171 个工作目录**在同时活跃，
低复用率镜像语料正是单体镜像模式先崩掉的地方。

**② 同位嫁接（机制层）。**
组合语义只有一条规则，可预测、可背诵：**层镜像里的 `/opt/toolkit` 是什么，
组合进沙箱后 `/opt/toolkit` 就是什么**：

```bash
POST /v1/templates/compose
{
  "name":  "swe-node20",
  "base":  "ubuntu-24.04-python:v7",
  "layers": [ { "image": "node20-toolkit:v3", "path": "/opt/toolkit" } ],
  "public": true
}
```

基座容器保持原样，toolkit 层的目录树经 container-to-container 拷贝原位嫁接进来，
最终 commit 为一个可创建沙箱的模板；组合链（谁 + 哪些层）记录在
`Template.Composition` 供审计与重建。基座与每一层的镜像**互不耦合**——
不需要修改 dockerd，不需要发明新格式。

### 亮点 2 · 快照即创建：毫秒级交付

按模板预热**已启动的纯净容器**（纯净 = 从未执行过任何工作负载，交给任何租户都不泄漏）。
创建命中池时毫秒级交付；未命中走冷路径。暂停、快照、模板三种产物形状对齐，
从快照重建新沙箱与冷创建走同一条治理与供给管线。

### 亮点 3 · 崩溃可判定：意图先行 + 单一真相源 + reconciler

沙箱是长命有状态对象，守护进程随时可能被 `kill -9`。Sandcastle **先落盘意图再触碰后端**：
create 把 `provisioning` 意图写进 WAL（每事务一帧 `[len][crc32][json]` + fsync，撕裂尾部
自动截断）后才启动容器。凡处于过渡态（provisioning/pausing/resuming/stopping）的记录，
重启后一律交给 reconciler 向后端重新探测真相并收敛——本地状态只是假设，后端才是事实。
灾备演练验证：连续三次 `kill -9` 状态零漂移；容器 VM 整机重启后，死者如实收敛为
stopped 并入审计，平台照常接纳新沙箱。

### 亮点 4 · 诚实语义：拒绝而非假装

后端无法真实强制的能力，**拒绝创建而不是静默降级**：容器后端做不了内核级出口白名单
→ `egress.mode=allowlist` 返回 501；overlayfs 强制不了单沙箱磁盘配额 → `disk_mib`
返回 501（除非运维显式接受风险）。一个"看起来受限实际没有"的沙箱比没有更危险——
这条原则有专门测试锁死，不做例外。

### 亮点 5 · 为 agent 的失败模式逐项设防

真实 agent 会做什么，恶意负载轮就测什么（沙箱内真实执行）：
`rm -rf /`（爆炸半径=沙箱本身，宿主机金丝雀文件与 `/etc` 哈希前后一致）、
fork 炸弹（pids-limit 吸收，进程表耗尽时返回可操作的 409 而非 OCI 内部报文）、
`apk add` 装包与 wget 外传（egress=none 下死在 DNS/路由）、内存炸弹
（64MB 上限 exit 137）、CPU 烧灼（配额隔离，健康检查 0.05s 不受影响）。
另有 **pack 防泄漏打包**：快照提交前按 `pack.cleanup` 清除构建残留与参考答案，
系统根目录拒绝删除——环境可以由 agent 构建，但泄漏必须由平台拦住。

### 亮点 6 · 体验细节也是系统设计

- **透明恢复**：对 paused 沙箱的请求自动先恢复再执行，调用方永远不需要管理冻结状态；
- **pause 主动内存回收**：冻结期临时降限触发内核驱逐文件页/换出匿名页，恢复前**必须**
  先还原原始上限（带重试；还原失败宁可保持冻结也不带病解冻）；
- **双维度回收**：硬 TTL 防泄漏 + 空闲超时防驻留（agent 交互间隙 CPU 稀疏但内存长期占着，
  这才是密度的隐形杀手），分别审计为 `sandbox.expire` / `sandbox.idle_expire`；
- **治理一等公民**：API Key 仅存哈希（常数时间比对、明文只出现一次）、租户级配额
  （并发/CPU/内存/快照数/字节）、按租户×操作组令牌桶限流（429 + Retry-After）、
  不可变审计流。

### 亮点 7 · 零第三方依赖（stdlib-only Go）

供应链攻击面为零、单文件分发、可气隙构建。WAL 存储、Prometheus 指标、令牌桶、
Docker API 客户端全部手写——每一行都在你自己的 code review 范围内。

## 三、架构

```
pi agent / SDK / CLI / 任意 HTTP 客户端
        │ REST + Bearer sk_...（TLS 可选）
        ▼
┌──────────────────────── sandboxd（单二进制）────────────────────────┐
│ httpapi   recover→requestID→accessLog→bodyLimit→authenticate→rateLimit│
│           （中间件顺序即安全契约：限流必须在认证内侧才有租户身份）        │
│ service   生命周期状态机（4 个显式过渡态）· 预热池 · 快照/pack ·        │
│           模板组合 · TTL/空闲清扫 · reconciler                         │
│ store     崩溃安全 WAL：每事务一帧[len][crc32][json]+fsync            │
│           撕裂尾部自动截断 · 周期压实 · flock 防双进程 · CAS 乐观锁     │
│ gov       Key 认证（哈希落库/常数时间比对）· 令牌桶限流 · 配额 · 审计   │
│ runtime   Backend 接口 ── oci（Docker 容器）▸ firecracker（骨架）      │
└──────────────────────────────────────────────────────────────────┘
```

**一次创建的完整旅程**（`sbxctl create --template tpl_x`）：

```
CLI ──▶ 中间件链（认证→限流）──▶ 治理准入（模板解析/规格校验/egress检查/配额）
    ──▶ 意图落盘（provisioning，WAL+fsync）        ← 从此任何时刻断电都可判定
    ──▶ 预热池命中？──是──▶ 直接采用（毫秒级，状态=running）
                    └─否──▶ 异步供给：OCI Create（net=none·CapDrop ALL·
                            no-new-privileges·禁swap内存硬限）→ Start →
                            轮询确认真实相位 → 状态机收敛 running
    ──▶ waitForState 轮询 store ──▶ 201 返回
此后：TTL/空闲清扫器回收僵尸 · reconciler 周期对账 · 每步动作入审计流
```

沙箱状态机（客户端走合法边；reconciler 直接写真相；过渡态让崩溃可判定）：

```
provisioning ─▶ running ⇄ pausing/paused ⇄ resuming ─▶ stopping ─▶ stopped(终态)
      └─▶ failed ─▶ stopping            任意存活态 ─▶ stopping
```

## 四、功能全景

| 域 | 能力 |
|---|---|
| 隔离 | 容器后端：net=none · CapDrop ALL · no-new-privileges · pids-limit · 禁 swap 内存硬限 · CPU 配额；Firecracker 骨架（无 KVM 诚实拒绝） |
| 生命周期 | 同步创建 · 暂停/透明恢复/停止/强杀 · pause 内存回收 · 硬 TTL + 空闲超时 |
| 执行与文件 | 同步 exec（超时硬截止/输出截断/退出码透传）、文件四操作（路径三重校验） |
| 快照与环境 | 快照与恢复 · pack 防泄漏 · 分层模板组合 · 离线 rootfs 导入（气隙） |
| 治理 | 租户/Key/即时吊销 · 配额 · 按组限流 · 追加式审计 |
| 运维 | healthz/readyz/metrics · JSON 日志（request_id）· 优雅停机 · WAL 压实 · 一致性备份 |

**Agent 集成**：[pi coding agent 官方扩展](integrations/pi/pi-sbx/README.md)——pi 与 LLM 凭据留在宿主机，
bash 工具经 REST 路由进沙箱；已用 **Z.ai GLM-5.3-flash** 完成真实端到端验证
（GLM 驱动 pi 在沙箱内写文件，宿主机确认该文件不存在——隔离边界成立）。

## 五、质量：七轮商用验收，71 项端到端断言

`make test-e2e` 一键复跑。另有 7 个包的单测/集成测试（`-race` 全绿），
含真实容器集成：OOM 真实击杀、超时真实生效、快照往返、离线 rootfs、层嫁接。

| 轮次 | 内容 | 结果 |
|---|---|---|
| chaos | `kill -9` 三场景：在途创建收敛 / 存活沙箱穿越 3 次连续崩溃 / WAL 完整性 | 5/5 |
| load | 30 并发创建 + 200 次混合 exec（开发虚机实测：创建 P99≈2s，exec P99≈0.2s） | 零 5xx |
| security | 伪造/篡改 Key、跨租户 IDOR、路径穿越、体积洪水、密钥卫生 | 21/21 |
| malicious | 沙箱内真实执行 `rm -rf /`、fork 炸弹、装包、外传、内存炸弹、CPU 烧灼、写宿主 `/etc` | 12/12 |
| multiuser | 双租户并发工作流 + 运行中实时越权探针 + 配额独立性 + 吊销即时性 | 6/6 |
| disaster | 备份时点恢复 + 容器 VM 整机重启下的对账收敛 | 8/8 |
| pi-sbx | 真实 pi bash 工具契约驱动扩展（含隔离取证） | 7/7 |

**测试抓出并修复了 9 个真实缺陷**——包括纯单测永远发现不了的：
限流中间件顺序反了导致限流被静默跳过；Key 更新双索引不同步导致已吊销 Key 仍可用；
fork 炸弹把 exec 启动失败伪装成 exit 128 正常输出。完整复盘见 [CHANGELOG](CHANGELOG.md)。

## 六、定位：与同类项目的关系

| | Sandcastle | E2B | Daytona | DSec (DeepSeek) |
|---|---|---|---|---|
| 形态 | **单二进制，自托管** | 云服务 + 大型开源 runtime | **2026-06 起闭源** | 生产训练平台，未开源 |
| 依赖 | 零第三方（stdlib-only） | Consul/Nomad 等全家桶 | — | 3FS/EROFS/自研调度 |
| 治理 | 内建（Key/配额/限流/审计） | 内建 | 内建 | 内建 |
| 适用 | 读得懂全部代码的自托管执行底座 | 云端/企业部署 | 已停止开源维护 | RL 训练基础设施（16 万核级） |

Sandcastle 不复刻 DSec 的 16 万核调度；它把其中**在单机同样成立的设计**
（分层环境、透明恢复、pause 回收、空闲超时、pack 防泄漏——v1.1 已全部吸收）
带给自托管场景，其余部分（eBPF 网络白名单、EROFS 按需镜像等）写成明确的路线图。

## 七、快速开始

要求：任何能跑 Docker 的环境（Linux 生产 / macOS + colima 或 Docker Desktop 开发）。

```bash
git clone https://github.com/Hongli-xd/castle.git && cd castle
make build                                    # bin/sandboxd + bin/sbxctl

cat > sandboxd.conf <<'EOF'
listen  = "127.0.0.1:8080"
data_dir = "./data"
bootstrap.image = "alpine:3.22"               # 本机已有该镜像则自动播种默认模板
EOF
./bin/sandboxd -config sandboxd.conf
# 首次启动打印一次 ADMIN API KEY —— 立即保存

export SBX_URL=http://127.0.0.1:8080
export SBX_API_KEY=<管理员Key>
./bin/sbxctl tenant-create demo quota.max_sandboxes=10
./bin/sbxctl key-create <tnt_id> demo-key     # 租户 Key 明文仅此一次
```

日常使用：

```bash
export SBX_API_KEY=<租户Key>
./bin/sbxctl create --template default        # {"id":"sbx_...","state":"running"}
./bin/sbxctl sh <sbx_id> -- "echo hello && uname -m"
./bin/sbxctl cp <sbx_id> ./data.csv /root/
./bin/sbxctl snapshot-create <sbx_id> checkpoint-1
./bin/sbxctl create --snapshot <snp_id>       # 从快照恢复新沙箱
```

## 八、API 一览（REST，统一错误包络）

```
POST   /v1/sandboxes?wait=true|false   创建（template_id | snapshot_id | base_image 三选一）
GET    /v1/sandboxes                   列表（state 过滤）
POST   /v1/sandboxes/{id}/pause|resume|stop|timeout
POST   /v1/sandboxes/{id}/exec         {command | argv, timeout_ms, max_output}
PUT/GET/DELETE /v1/sandboxes/{id}/files?path=
POST   /v1/snapshots                   {sandbox_id, name, pack:{cleanup:[...]}}
POST   /v1/templates/compose           {name, base, layers:[{image,path}...]}  (admin)
GET    /v1/events                      审计事件流（租户自动限定）
POST   /v1/tenants …/keys              租户与 Key 管理 (admin)
GET    /healthz /readyz /metrics
```

错误以稳定 code 分支（`quota_exceeded` / `rate_limited` / `unimplemented` / …），
HTTP 状态码不参与重试判定。完整语义见 [docs/API.md](docs/API.md)。

## 九、文档地图

| 文档 | 内容 |
|---|---|
| [docs/DESIGN.md](docs/DESIGN.md) | 领域模型、状态机、可靠性设计、DSec 吸收路线图 |
| [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md) | 威胁主体、容器/microVM 隔离分级、诚实拒绝清单 |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | 生产部署、气隙安装、备份恢复、运行手册 |
| [docs/RESEARCH.md](docs/RESEARCH.md) | 赛道调研（E2B/Daytona 闭源/DSec/pi）与决策依据 |
| [docs/API.md](docs/API.md) | 完整 API 参考 |
| [CHANGELOG.md](CHANGELOG.md) | 版本历史（含测试抓出的 9 个缺陷复盘） |

## 十、路线图

- **中期**：eBPF 按沙箱网络白名单（egress 从诚实拒绝升级为内核级强制）· AppArmor 加固 · exec 流式输出与 PTY
- **远期**：Firecracker microVM 后端（接口已锁定）· EROFS+分布式 FS 按需镜像 · 多节点放置（power-of-k）· Postgres 存储后端 · 事件 Webhook

## 许可证

Apache-2.0。欢迎 issue / PR。

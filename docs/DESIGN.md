# sandboxd 产品设计文档（商用 v1.0）

## 0. 一句话定位

**sandboxd** 是一个单二进制、零第三方依赖、可私有部署的**多租户 Agent 沙箱运行时**：
为 AI Agent 提供隔离的执行环境（进程/文件/网络/资源受控），支持秒级创建、暂停恢复、
快照与预热池加速，内置租户、API Key、配额、限流与审计——对标 E2B/Daytona 的能力面，
面向"企业自托管 Agent 基础设施"这一真实商用场景（腾讯 AgentRuntime JD、ima/WorkBuddy
类产品的底层诉求）。

**业务场景**：企业内部 Agent 平台（编码 Agent、办公 Agent、数据分析 Agent）需要给
每个任务/每个用户分配一次性隔离执行环境；平台方需要按租户计量、限额、审计、隔离，
且必须能私有化部署（数据不出域）。sandboxd 就是这个"执行底座"。

## 1. 总体架构

单二进制 `sandboxd`（控制面 + 单节点数据面一体，内部分层对齐 E2B 的控制面/数据面分离），
CLI `sbxctl`，pi 扩展 `pi-sbx`。部署形态：Linux 生产服务器（rootfs 离线导入可气隙部署）；
macOS/任何有 Docker 的环境可开发自测。

```
pi agent / SDK / CLI / 任意 HTTP 客户端
        │ REST + X-API-Key (TLS 可选)
        ▼
┌────────────────────── sandboxd ──────────────────────┐
│ httpapi   认证→限流→配额→审计→路由（中间件链）           │
│ service   生命周期状态机 / reconciler / 预热池 / 快照    │
│ store     WAL 持久层（crc32 帧 + fsync；接口可换 PG）    │
│ runtime   Backend 接口：oci(Docker) ▸ firecracker(骨架) │
└──────────────────────────────────────────────────────┘
```

分层规则（抄自 E2B 并本地化）：
- httpapi 只做"认证了谁、限没限住、记没记账"，不碰后端；
- service 拥有全部状态机与补偿逻辑，唯一写状态的地方；
- runtime Backend 只回答"这台机器上真实存在什么"，不持久化。

## 2. 领域模型

| 实体 | ID 前缀 | 说明 |
|---|---|---|
| Tenant | `tnt_` | 租户=计费/配额/审计边界。含 name、quota、status |
| APIKey | `ak_` | 仅落库 SHA-256 哈希；含 prefix(前 12 字符用于查找)、scope(admin/tenant)、过期时间；可吊销 |
| Sandbox | `sbx_` | 核心对象，含 Spec（cpu/mem/disk/timeout/egress/env/metadata）、state、backend ref。**没有 node 字段**——本版本是单节点进程，多节点调度需要先改数据模型 |
| Snapshot | `snp_` | 由沙箱 commit 而来：rootfs 差分层 + 元数据；可派生新沙箱 |
| Template | `tpl_` | 模板=镜像产物引用 + 默认 Spec；预热池按键 (tenant,template) 维护 |
| AuditEvent | `evt_` | 追加式审计/生命周期事件；租户只能读自己的，admin 读全部 |

**沙箱状态机**（显式过渡态，崩溃可判定）：

```
provisioning → running → pausing → paused → resuming → running
                   │                                 │
                   │                                 └→ stopping → stopped → removing(隐式) → 终态移除
                   └→ failed            任意态 ─→ stopping ─→ stopped
```

崩溃恢复不变式：凡处于过渡态（provisioning/pausing/resuming/stopping）的记录，
守护进程重启时一律视为"被打断的操作"，由 reconciler 向后端重新探测真相位并收敛
（后端说是什么就是什么），不信任本地残留状态。

**乐观锁**：领域对象带 `BaseVersion`（读取时基线），存储层 CAS 比对 `BaseVersion` 而
非 `Version+1`——允许"内存三次变更、一次落盘"，仍能精确检出并发写。

## 3. 商用功能面（v1 范围）

### 3.1 多租户治理
- **认证**：`Authorization: Bearer ak_...` / `X-API-Key`。哈希落库、常数时间比较、
  prefix 索引查找、过期与吊销、scope 区分 admin/tenant。启动时从配置引导 admin Key。
- **限流**：每租户令牌桶，按操作组（create/exec/files/snapshot/admin/read）独立预算；
  429 + `Retry-After` + `X-RateLimit-Limit/Remaining/Reset`。内存实现（单机精确），
  接口化以便未来换 Redis 集群版。
- **配额**：租户级并发沙箱数、vCPU 总量、内存总量、快照数、快照存储字节；超配额拒绝
  创建（402 quota_exceeded），删除即返还。
- **审计**：全部变更操作与生命周期事件落 `evt_` 流；支持按租户/沙箱/动作/时间过滤分页。

### 3.2 沙箱生命周期
- 创建（同步等待进入 running）、列表（游标分页 + 状态过滤）、查询、暂停、恢复、
  停止/强杀、删除、TTL 续期（timeout extend）。
- **TTL 与空闲回收**：创建必须带 `timeout_seconds`（上限可配，默认 3600，防泄漏）；
  到期自动停止；后台 sweeper 每 30s 扫描。
- **预热池**：按 (template, tenant) 维护 N 个预热沙箱；创建命中池时 <100ms 交付；
  池由后台维护器补货。指标暴露命中/未命中。
- **reconciler**：启动即全量对账；运行期每 60s 增量对账 + 状态修复事件入审计。

### 3.3 执行与文件
- `POST /v1/sandboxes/{id}/exec`：同步执行（argv 或 shell 串），可设 cwd/env/user/
  timeout_ms；返回 exit_code/stdout/stderr/耗时/是否截断。`max_output` 是**上限**：
  默认 1MiB，平台硬顶 `max_exec_output_bytes`（默认 8MiB），超出在准入处即被拒绝
  （控制面把这些字节缓冲在自己的堆上，无上限的请求字段等于让人远程决定我们的内存占用）。
- 文件 API：上传（base64 或原始体）、下载、列表、删除；路径校验拒绝越界（`..`、绝对
  路径逃逸）；单文件默认 100MiB 上限。

### 3.4 快照与模板
- `POST /v1/snapshots {sandbox_id, name}`：commit 文件系统差分层 + 冻结元数据；
- 从快照/模板创建沙箱走同一恢复路径；快照受租户配额（个数/字节）约束。
- 模板：`POST /v1/templates` 由管理员从 rootfs 产物注册；`GET /v1/templates` 公开列表。

### 3.5 出口网络策略（诚实语义）
- `egress.mode=none`（默认）：沙箱无网络，容器后端真实强制；
- `egress.mode=allowlist`：**容器后端无法内核级强制 → 创建被 501 拒绝**，
  绝不产出"看起来受限实际没有"的沙箱；内核级白名单（nftables + SNI/Host 检查）
  属于 firecracker 后端路线图；
- 未知模式：创建被拒绝（invalid_request），绝不静默降级。

### 3.6 隔离等级（必须显式声明，且不可静默降级）

隔离强度**不是**一个部署细节，而是产品对客户的核心承诺，因此它是一个一等公民配置项：

| 等级 | 值 | 内核 | 适用对象 |
|---|---|---|---|
| 容器（当前唯一可用） | `container-shared-kernel` | **与宿主共享** | 可信 / 半可信租户 |
| microVM（路线图，本版本不可用） | `microvm` | 每沙箱独立 guest kernel | 任意不可信代码 |

- `isolation = auto`（默认）：使用本机真正能跑的最强后端，并把**实际**等级写进启动日志；
- `isolation = container`：明确接受共享内核这一档；
- `isolation = microvm`：**要求**强隔离。本机不具备（无 `/dev/kvm`）或驱动仍是骨架时，
  **拒绝启动**，而不是记一条 warning 然后继续用容器跑。这是本设计的硬约束：
  一个"以为自己在跑 microVM、实际在跑共享内核容器"的部署，比明说"我只有容器"危险得多。

运行中的等级是**机器可读**的：`GET /readyz` 返回 `.isolation` 与 `.isolation_note`，
无需认证（唯一的例外，因为客户最需要能独立核对的就是这一项）。

`internal/runtime.Backend` 接口带 `IsolationTier()` 方法，所以新增后端**必须**声明自己的等级——
这条信息不可能再被漏掉。

#### 3.6.1 容器级隔离契约：字段钉死 + userns + 自检

等级说清了"这一档有多强"，这一节说清"这一档具体做了什么"。

**（一）安全契约写在创建请求里，不写在宿主 `daemon.json` 里。**
凡是没有显式写入 `HostConfig` 的字段，都由宿主的 `daemon.json` 决定 —— 那是一个可变的文件，
换一个引擎版本或换一个运维就能改，而这份代码不会有任何变化。因此每一个沙箱依赖的字段都被显式
写出，**即使它的值恰好等于今天的引擎默认值**：

| 字段 | 钉死为 | 若不钉死会怎样 |
|---|---|---|
| `NetworkMode` | `none` | 沙箱获得网络 |
| `IpcMode` | `private` | `default-ipc-mode` 默认 `shareable`，别的容器可加入本沙箱的 IPC 命名空间 |
| `CgroupnsMode` | `private` | `default-cgroupns-mode` 在 cgroup v1 上默认 `host` |
| `UsernsMode` | `private` 或 `host`（**永不省略**） | 见下（二） |
| `Privileged` | `false` | — |
| `CapDrop` / `CapAdd` | `ALL` / `[]` | 空 `CapAdd` 也显式写出：未设置的字段是引擎可以填的字段 |
| `SecurityOpt` | `no-new-privileges` + `apparmor=docker-default` | `seccomp-profile` 可被 daemon 级替换 |
| `MaskedPaths` / `ReadonlyPaths` | 引擎默认集合的显式副本 | 屏蔽集合可随引擎版本变化 |
| `Ulimits` | `core=0/0`、`nofile=1024/65536` | 见下（四） |
| `ReadonlyRootfs` | `false`（**刻意**，非遗漏） | 见下（四） |
| `Resources` | CPU/内存/关闭 swap/PidsLimit | 单沙箱耗尽宿主机资源 |

`apparmor=` 只在 `/info` 报告存在 AppArmor 时才写：在没有 AppArmor 的宿主上指定 profile 是错误。

**（二）user namespace 是一个会静默失效的设置。**
实测（Docker 29.5.2，未配置 `userns-remap`）：把 `HostConfig.UsernsMode` 设为 `private`，

```
GET /containers/<id>/json → HostConfig.UsernsMode = "private"   ← 引擎原样回读
/proc/self/uid_map        → 0 0 4294967295                      ← 什么都没发生
```

引擎接受了它无法兑现的字段并回读为"已应用"。因此**读配置（哪怕是从引擎回读的配置）都不是证据**，
只有内核报告的 uid 映射才是。据此：

- `userns = auto`（默认）：从 `/info` 的 `SecurityOptions` 判断本 daemon 是否真的具备 remap 能力，
  具备则用 `private`，否则运行在 `host` 并把这次降级**写进启动日志与 `/readyz`**；
- `userns = require`：不具备时**拒绝启动**，报错信息点名 `userns-remap` / `daemon.json` / `userns=off`
  三条出路；
- `userns = off`：显式接受共享身份这一档（启动打印 WARNING）。

`UsernsMode` 永不省略。留空不是中性的：配了 `userns-remap` 的引擎会把新容器默认设为 `private`，
于是同一个"未设置"在某些宿主上是 `private`、在另一些上是 `host` —— 那样 `userns=off` 就无法真正关闭它。

没有用户命名空间时，沙箱内的 uid 0 **就是**宿主的 uid 0：一个内核漏洞直接到达宿主 root，
不需要第二次提权，上面那些控制（drop capabilities、seccomp、LSM）在被越过的那一刻就全部失效。
这一层是"压缩逃逸收益"，与其余几层"缩小入口"不同。

**（三）隔离必须可被证明，而不是被声明。**
`internal/runtime.Backend` 带 `VerifyIsolation()`：用**生产同一条载荷路径**起一个真沙箱，
在里面读内核事实（`/proc/self/uid_map`、`Seccomp`、`NoNewPrivs`、`CapEff/CapBnd`、
`/proc/self/attr/current`、`ulimit -c`），并尝试两个逃逸探针（`mount`、读 `/proc/kcore`）。
因为 `IsolationTier()` 只声明等级，`VerifyIsolation()` 才证明等级。

结果有三种，且**不允许互相冒充**：

| 结果 | 含义 |
|---|---|
| 全项通过 | 逐项列出实测值 |
| 有项失败 | 逐项列出 `observed` 与 `want` |
| **无法测量** | 探针没跑起来 / 探针与解析器对不上键 → 返回错误 |

第三种尤其重要：一个测不出结果的检查，既不能报"通过"，也不能报"发现漏洞"。
启动时执行一次，结果缓存在 `GET /readyz`（摘要，不鉴权）与 `GET /v1/isolation`（完整证据，需 admin）。
`/readyz` 在自检从未运行时返回 `isolation_verified: false` 并说明"这是配置，不是已验证的事实"，**
绝不会把"未验证"渲染成成功**。

**（四）两处与快照语义耦合的决定。**
- `core=0`：本产品会把沙箱文件系统 `Commit()` 成快照，因此一次 core dump 会把进程内存
  ——包括它持有的凭据——写进一个会被存储并分发给其他沙箱的产物里。这是数据外泄控制，不是整洁性设置。
- `ReadonlyRootfs=false`：只读根目录是通常的下一步加固，但它与快照模型不兼容 ——
  `Commit()` 捕获的是容器的可写层，只读根会产出空快照。要与"可写 workspace overlay"一起重新设计，
  在此之前不假装拥有它。

### 3.7 运维面
- `GET /healthz`（存活）/ `GET /readyz`（就绪：store+backend 探测，并回报隔离等级与自检摘要）/
  `GET /v1/isolation`（完整隔离自检证据，需 admin）/ `GET /metrics`（Prometheus 文本格式，stdlib 手写）。
  `/healthz` 与 `/readyz` 不鉴权（负载均衡需要）；`/metrics` 与 `/v1/isolation` 需要 **admin scope**——
  它们承载全租户的运维数据与当前生效的控制清单，后者对正在策划逃逸的人就是侦察情报。
- 结构化 JSON 日志（request_id 贯穿）、优雅停机、`SIGHUP` 日志级别热调。
- 配置：TOML-lite（自解析 key=value）文件 + 环境变量覆盖 + 命令行。
  数值型环境变量解析失败即启动失败，**不**静默回退默认值。
- 请求体上限 1MiB（文件上传除外）、ReadHeaderTimeout 防 slowloris、并发连接上限。
- exec 输出上限 `max_exec_output_bytes`（默认 8MiB）。请求里的 `max_output` 是**上限**
  而非偏好：控制面把这些字节缓冲在自己的堆上，0 表示后端默认（1MiB），**不**表示"无限"。
- 未认证流量按客户端 IP 设防洪阈值（`preauth.*`）。它位于认证**之前**，因为未认证的
  调用者还谈不上租户配额；且刻意不看 `X-Forwarded-For`（该头可伪造）。
- 认证失败审计按 IP 分窗聚合（首次全量记录 + 每 100 次记一次计数），
  所以未认证洪泛不会变成"每请求一次 fsync"。指标 `sandboxd_auth_failures_total`
  **不做采样**，真实总量始终可见。

### 3.8 WAL 损坏恢复（区分"崩溃残骸"与"已确认数据"）

| 损坏位置 | 判定 | 默认动作 |
|---|---|---|
| 日志尾部（写入中途掉电） | 该帧本就不是已提交事务 | 自动截断，记 `RepairReport` |
| 日志中段（其后仍有完好帧） | 那些帧已 fsync 并被确认 | **拒绝启动**，要求人工决定 |

`wal_repair = salvage` 是在首个坏帧处截断的显式逃生舱；`strict` 则连尾部撕裂也不自动修。
三种策略都会产出量化的 `RepairReport`（损坏类型/偏移/保留与丢弃的字节数），
经启动日志暴露——**不存在静默丢数据的路径**。

## 4. API 面（REST，`/v1`）

```
POST   /v1/sandboxes                     创建（image=tpl_ | snapshot=snp_ | base_image=oci 引用）
GET    /v1/sandboxes?state=&limit=&cursor=
GET    /v1/sandboxes/{id}
POST   /v1/sandboxes/{id}/pause | /resume | /stop | /timeout
DELETE /v1/sandboxes/{id}?force=
POST   /v1/sandboxes/{id}/exec
PUT    /v1/sandboxes/{id}/files?path=    （raw body）
GET    /v1/sandboxes/{id}/files?path=    /  /files/list?path=  /  DELETE .../files?path=
POST   /v1/snapshots                     GET /v1/snapshots  GET/DELETE /v1/snapshots/{id}
GET    /v1/templates
GET    /v1/events?tenant=&sandbox=&action=
POST   /v1/tenants (admin)               GET/PATCH /v1/tenants/{id}
POST   /v1/tenants/{id}/keys (admin)     DELETE /v1/tenants/{id}/keys/{key_id}
GET    /healthz /readyz /metrics
```

错误包络（全 API 统一）：`{"error":{"code","message","retryable","request_id"}}`，
错误码稳定（`auth_failed/quota_exceeded/rate_limited/not_found/conflict/invalid_request/
backend_unavailable/unimplemented/internal`），HTTP 状态码不参与重试判定
（显式 retryable 字段）。

## 5. 安全与威胁模型（摘要，全文见 THREAT-MODEL.md）

威胁主体：**运行在沙箱里的不可信代码**（可能是被提示注入的 Agent 产物，参考
CVE-2026-25253）+ **持有租户 Key 的调用方**。

- 容器后端强制：net=none（默认）、drop ALL capabilities、no-new-privileges、
  pids-limit、memory 上限且 swappiness=0、只读 cap 提权不可用；
- **磁盘配额无法在 overlayfs/ext4 上真实强制 → 默认拒绝启动**，运维必须显式
  `enforce_disk_quota=false` 接受风险（风险写在配置注释与文档）；
- API Key 哈希存储/常数时间比较；租户数据全部按 tenant_id 服务端过滤（IDOR 防线在
  service 层而非路由层）；文件路径穿越校验；exec 输出/文件大小/请求体三重上限；
- 审计不可变（只追加）；日志与错误消息永不携带 Key 明文与文件内容。

## 6. 可靠性

- **WAL 存储层**：每事务一帧 `[len][crc32][payload]`，写后 fsync；重启重放；尾部残帧
  CRC 检出即截断（崩溃原子性）；周期压实（全量重写+截断日志）；数据目录丢失=不可恢复
  （有文档的备份指引：copy 数据目录即可）。
- **崩溃恢复**：过渡态对账（见 §2）；后端容器孤儿回收（带产品标签的容器按标签收割）；
  **孤儿镜像回收**——快照/模板镜像带标签（`sandboxd-snap-<id>`），因此**不是**悬空镜像，
  `docker image prune` 永远不会删它们。数据目录丢失、从提交前的备份恢复、或节点重建时，
  镜像会没有记录指向它并永久留在盘上。reconciler 的镜像遍历是唯一会删它们的东西，
  它保护任何仍被记录引用的镜像（含**从快照创建的活沙箱**所用的镜像）。见
  `TestOrphanedSnapshotImagesAreReaped`。
- **背压**：创建并发闸（全局信号量）+ 预热池吸收突发。

## 7. 测试与验收（商用门槛）

全部端到端断言位于 `tests/` 下的 Go 套件，共用一个 harness（daemon 生命周期、带类型客户端与
沙箱夹具）。harness 复用生产侧的 `internal/domain` 类型，字段改名会在编译期报错而不是在运行时
静默漏测；断言辅助函数把结果分成"通过 / 失败 / 测不出来"三种，且不允许互相冒充。

1. 单元：存储崩溃语义（撕裂帧/并发 CAS/压实）、状态机全部非法转换、配额/限流边界、
   路径穿越、Key 哈希；`-race` 全绿。
2. 集成（真实 Docker）：创建→exec→文件→快照→从快照重建→暂停→恢复→停止全链路；
   资源限制真实生效验证；离线 rootfs 导入（不依赖 registry）。
3. API 黑盒：未认证/错租户访问/越权 IDOR/超配额/超限流 429/畸形请求——全部得到预期
   错误码且无信息泄漏；错误包络的 `code` 是客户端唯一应分支的字段。
4. 隔离契约：22 项从**运行中沙箱内部**读取的内核事实（seccomp / NoNewPrivs / 能力集 /
   `/proc/self/uid_map` / ulimit / 设备 / 套接字 / 路由），并断言 daemon 自己的启动自检
   与本套件结论一致。
5. 遏制：OOM 上限、fork 炸弹、CPU 独占、`rm -rf /` 爆炸半径、沙箱内写"宿主路径"——
   每一项都带**正向对照**（先证明恶意载荷真的执行并生效），否则"没出事"可能只是"没发生"。
6. 混沌与灾难：daemon 执行中 `kill -9` → 重启对账收敛；重复崩溃的 WAL 完整性；
   备份/时点恢复；孤儿后端对象与孤儿快照镜像的回收。
7. 并发与多租户：并发创建的归属、并发 exec 的输出不串、租户间文件系统不共享、
   配额在并发下的真实语义（收敛而非瞬时精确，见 SECURITY-CONTRACT.md §5）。
8. 负载：并发创建 + 200 次混合 exec，零 5xx，P50/P99 实测记录在测试日志中。
9. Agent 实战：pi 扩展端到端——pi 的 `bash` 工具实际落在沙箱内执行；read/write/edit 仍在宿主（本产品是**执行隔离**而非 **Agent 隔离**，边界见 integrations/pi/pi-sbx/README.md 范围声明）。

验收门槛：以上全部通过，且 `SBX_REQUIRE_DOCKER=1` 下**不允许出现静默跳过**——
一个静默跳过的套件与一个通过的套件在报告上无法区分。

客户可核对的逐层承诺与**明确不承诺**的边界见 [SECURITY-CONTRACT.md](SECURITY-CONTRACT.md)。

## 7.5 v1.1 路线图（吸收 DSec，arXiv:2609.22978）

DSec（DeepSeek 生产沙盒平台，日产 ~300 万沙箱）验证了本设计的状态单一真相源、
预热池、Backend 分级与诚实安全语义，并提供如下演进地图：

已落地（v1.1）：透明恢复（auto_resume）、pause 内存回收、空闲超时、
分层模板组合（base+toolkit 同位嫁接）、pack 防泄漏打包。

中期（需新基础设施）：
- eBPF 按沙箱网络白名单（按域名/端口/协议过滤，动态更新）——容器后端的
  egress allowlist 从"诚实拒绝"升级为内核级强制；macOS 开发环境保持拒绝；
- AppArmor 文件/套接字控制（约束沙箱内 root 对平台日志/内部套接字的访问），
  攻防样本（XFS_IOC_SWAPEXT 绕过）纳入威胁模型演练。

远期（多节点/训练基础设施规模时）：EROFS+分布式 FS 按需镜像、virtio-pmem DAX
页缓存共享、DAMON+气球页回收、power-of-k 放置与边缘准入、BGP/ECMP 服务发现、
GPU FnCall（MIG 分区 + 常驻进程池）。

## 8. 里程碑映射（本次交付）

M1 地基(apperr/id/domain/store) → M2 runtime(OCI+离线rootfs) → M3 治理(认证/配额/限流/
审计) → M4 编排(状态机/快照/池/reconciler/httpapi) → M5 CLI+pi 扩展 → M6 六轮测试 →
M7 文档与交付。

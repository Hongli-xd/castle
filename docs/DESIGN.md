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
| Sandbox | `sbx_` | 核心对象，含 Spec（cpu/mem/disk/timeout/egress/env/metadata）、state、node 信息 |
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
  timeout_ms；返回 exit_code/stdout/stderr/耗时/是否截断；输出默认 1MiB 截断（防 OOM）。
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

### 3.6 运维面
- `GET /healthz`（存活）/ `GET /readyz`（就绪：store+backend 探测）/ `GET /metrics`
  （Prometheus 文本格式，stdlib 手写）。
- 结构化 JSON 日志（request_id 贯穿）、优雅停机、`SIGHUP` 日志级别热调。
- 配置：TOML-lite（自解析 key=value）文件 + 环境变量覆盖 + 命令行。
- 请求体上限 1MiB（文件上传除外）、ReadHeaderTimeout 防 slowloris、并发连接上限。

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
- **崩溃恢复**：过渡态对账（见 §2）；后端容器孤儿回收（带产品标签的容器按标签收割）。
- **背压**：创建并发闸（全局信号量）+ 预热池吸收突发。

## 7. 测试与验收（商用门槛）

1. 单元：存储崩溃语义（撕裂帧/并发 CAS/压实）、状态机全部非法转换、配额/限流边界、
   路径穿越、Key 哈希；`-race` 全绿。
2. 集成（真实 Docker）：创建→exec→文件→快照→从快照重建→暂停→恢复→停止全链路；
   资源限制真实生效验证；离线 rootfs 导入（不依赖 registry）。
3. API 黑盒：未认证/错租户访问/越权 IDOR/超配额/超限流 429/畸形请求——全部得到预期
   错误码且无信息泄漏。
4. 混沌：daemon 执行中 `kill -9` → 重启对账收敛；存储目录权限损坏时的失败模式；
   后端不可达时的降级表现。
5. 负载：50 并发创建/exec 混合负载，P99 与错误率达标（0 5xx、限流器工作正常）。
6. Agent 实战：pi 扩展端到端——pi 的 bash/read/write 工具实际落在沙箱内执行。

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

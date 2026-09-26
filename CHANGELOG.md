# 变更说明

## v1.1.0（2026-09-26）— 吸收 DSec（DeepSeek Elastic Compute, arXiv:2609.22978）设计

对照 DeepSeek 生产沙盒平台论文（支撑 V3.2→V4.1 agentic RL 训练，日产 ~300 万沙箱）
的五项近期可落地设计，全部实现并补齐回归测试：

1. **透明恢复（auto_resume）**：对 paused 沙箱的 exec/文件请求自动先恢复再执行，
   调用方不再感知冻结状态（DSec §6.3 "transparently resumes it"）。配置
   `auto_resume`（默认开）可关闭回到 409 语义。
2. **pause 时主动内存回收**：暂停后临时下调容器内存上限（≤64MiB）触发内核回收
   文件页并换出匿名页，恢复前**必须**先还原原始上限（重试至多 5 次，失败则保持
   冻结并报错——带病解冻的 OOM 循环更糟）（DSec §6.3）。配置 `pause_reclaim`。
3. **空闲超时**：`spec.idle_timeout_seconds`（≥60s，0 继承平台默认/0=关闭）+
   `LastActivityAt` 活动时间戳（每沙箱每分钟至多落盘一次），清扫器按
   `sandbox.idle_expire` 独立审计。DSec §1(3)：agent 交互间隙 CPU 稀疏，
   内存/页缓存长期驻留是密度上限的主要约束。
4. **分层模板组合**：`POST /v1/templates/compose` —— base + toolkit 层镜像按路径
   同位嫁接（container-to-container archive 拷贝），基座与每层独立版本化，
   组合链记入 `Template.Composition`；组合残留容器由 reconciler 按名回收
   （DSec §5.1，容器后端实现无需修改 dockerd）。
5. **pack 防泄漏打包**：`POST /v1/snapshots` 支持 `pack.cleanup`（≤16 条路径，
   拒绝系统根目录），提交前删除构建残留/参考答案；快照带 `packed` 标记与
   独立审计事件（DSec §6.1 "build environments of Agents, by Agents, for Agents"
   的防泄漏语义）。

同时新增 `POST /v1/templates/compose`（admin）端点与 `CopyDir` 后端原语
（接口 + OCI 实现 + Firecracker 骨架）。


## v1.0.0（2026-09-25）— 首个商用版本

本次交付在本仓库从零构建了完整的 sandboxd 运行时。调研与设计依据见
docs/RESEARCH.md 与 docs/DESIGN.md；所有新增代码均为本仓库原创
（stdlib-only，未引入任何第三方库）。

### 新增：地基

- `internal/apperr`：稳定错误码 + 显式 Retryable（不从 HTTP 状态推导）。
- `internal/idgen`：带前缀 Crockford base32 ID（tnt_/ak_/sbx_/snp_/tpl_/evt_），
  Parse 在边界校验类型，杜绝"快照 ID 当沙箱 ID"的深度腐坏。
- `internal/domain`：沙箱状态机（含 provisioning/pausing/resuming/stopping 四个
  显式过渡态，崩溃可判定）、Spec 校验、租户/配额/Key/快照/模板/审计事件。
- `internal/store`：崩溃安全 WAL（每事务一帧 `[len][crc32][payload]` + fsync；
  撕裂尾部自动截断；周期压实；flock 防双进程；BaseVersion CAS 乐观锁——
  允许"内存多次变更一次落盘"，同时精确检出并发写）。

### 新增：运行时

- `internal/runtime`：Backend 抽象（生命周期/exec/文件/快照/孤儿对账）。
- `internal/runtime/oci`：Docker Engine API 客户端（unix socket、多路流解复用）。
  安全基线：net=none、CapDrop ALL、no-new-privileges、PidsLimit、禁 swap 的
  内存硬限、CPU 配额。Start 校验真实相位（防"假运行"），OOM(exit 137) 显式命名。
- `internal/runtime/oci/rootfs.go`：离线 rootfs 导入（气隙一等公民），
  自动处理 merged-/usr 布局与动态链接器归一化。
- `internal/runtime/fc`：Firecracker microVM 后端骨架——接口编译期锁定；
  无 KVM 环境下所有操作诚实返回 unimplemented。

### 新增：治理

- `internal/gov`：API Key 认证（SHA-256 落库/常数时间比较/prefix 索引）、
  租户×操作组令牌桶限流（429 + Retry-After + X-RateLimit-*）、配额记账
  （并发沙箱/CPU/内存/快照数/快照字节）、追加式审计。

### 新增：编排与服务

- `internal/service`：意图先行持久化 + 异步供给；暂停/恢复/停止的"记录→后端→
  记录"三段式与失败回退；快照创建超配额自动回滚镜像；预热池（命中毫秒级交付、
  崩溃残留按名收割、采用对象受保护）；reconciler（周期对账 + 启动对账两种模式，
  启动时无在途宽限期）；TTL 清扫器；WAL 压实循环。
- `internal/httpapi`：REST 控制面（recover→requestID→accessLog→bodyLimit→
  authenticate→rateLimit 顺序加载）；统一错误包络；LastUsedAt 节流落盘。
- `internal/config`、`internal/metrics`：key=value 配置 + SBXD_* 环境覆盖；
  stdlib 手写 Prometheus 文本指标。
- `cmd/sandboxd`：单二进制 daemon（admin key 一次性引导、默认模板播种、
  优雅停机）；`cmd/sbxctl`：全功能 CLI（exec 透传远端退出码）。

### 新增：pi agent 集成

- `integrations/pi/pi-sbx`：官方扩展（Gondolin 同款工具路由模式）——pi 与
  LLM 凭据留在宿主机，bash 工具经 REST 路由进沙箱执行；SBX_KEEP/SBX_SANDBOX_ID
  等运维开关。零运行时依赖（仅 peer 依赖 pi-coding-agent）。

### 新增：商用测试套件

- `tests/chaos.sh`：kill -9 混沌（在途创建收敛/存活沙箱保持/连续三次崩溃 WAL 完整）。
- `tests/load.sh`：30 并发创建 + 200 混合 exec（零 5xx，P99 统计）。
- `tests/security.sh`：21 项安全黑盒（认证/IDOR/路径穿越/体积/诚实拒绝/密钥卫生）。
- `tests/pi-sbx-harness.mjs`：真实加载扩展 + 真实 pi bash 工具契约驱动（7 项）。

### 测试过程中发现并修复的真实缺陷（节选）

1. **限流中间件顺序反了**：rateLimit 在 authenticate 外层，认证前 key 为空导致
   限流被静默跳过——HTTP 级限流测试（而非仅 Limiter 单测）抓出。
2. **双索引不同步**：UpdateKey 只更新主 map，prefix 索引仍指旧对象，吊销后的
   Key 依旧可用——治理层测试抓出。
3. **exec 超时不生效**：流读阻塞在无数据帧上，deadline 检查永不执行——
   改为专用短超时 context 取消底层读。
4. **归档 PUT 不建父目录**：Docker archive API 语义；上传前显式 mkdir -p。
5. **镜像名大小写**：Docker 仓库名必须小写，而产品 ID 是大写 base32——快照
   commit 时 toLowerCase。
6. **busybox ash 命令替换落盘**：内存 OOM 探针本身无效（替换输出进临时文件），
   造成"64MB 限制下 200MB 分配存活"的假象——换堆增长探针验证限制真实生效。
7. **merged-/usr 布局**：rootfs 构建若在根下建实体 bin/lib 目录则镜像不可启动；
   归一化为 usr/* 真身 + 顶层符号链接。

### 后续加固测试（恶意负载/多用户/灾备三轮）追补修复

8. **进程表耗尽的可观测性**：fork 炸弹占满 pids 上限后，Docker 把 exec 启动失败以
   exit 128 + stdout 内嵌 "OCI runtime exec failed ... setns" 回传。现在识别并映射为
   可操作的 409（"进程表已耗尽，等待退出或删除重建"），不再泄漏 OCI 内部报文。
   注意：检测必须在权威 exit code 回填之后执行（首次实现即犯此顺序错误，测试抓出）。
9. **卡死容器的强删可靠性**：pids 耗尽的容器删除时 Docker 可能持续返回 409
   （removal in progress）。Remove 现在对 409 重试最多 8 秒，force-delete 总能回收。

### 已知限制（如实声明）

- 容器后端：共享内核隔离强度；egress 白名单与磁盘配额不可强制（拒绝而非假装）。
- exec 为同步模型（流式/PTY 在路线图）。
- 事件查询为单机内存集（默认保留 10 万条；WAL 中有全量）。

# 变更说明

## v1.1.8（2026-10-12）— exec 审计落盘与 Docker 往返并行化（端到端 −18% / p99 −36%）

起因是「性能还有优化的点吗」的一次测量驱动排查。结论是：exec 热路径上，sandboxd
自己唯一没摊掉的成本是审计事件那次 **WAL fsync**，其余全部已摊掉（HTTP 连接复用、
`touchActivity` 每分钟节流、meter 内存增量后台落盘）。

### 测量（证据，非承诺）

同机同沙箱，warm 后各 120 次，`argv=["true"]`，macOS / Docker 29.5.2：

| 指标 | 值 |
|---|---|
| 纯 Docker API exec（3 往返，无审计）p50 | 21.8 ms |
| sandboxd exec 端到端 p50（优化前） | 24.5 ms |
| 其中 `duration_ns`（backend 往返） | 19.4 ms（79%） |
| 其中 self（审计+HTTP+认证+meter） | **5.0 ms（20%）** |
| `GET /v1/sandboxes/{id}`（读，无 exec/审计） | 0.18 ms |
| `AppendEvent` 单次（fsync on / off） | **4.2 ms / 13 µs** |
| `AppendEvent` 8 并发（fsync on / off） | 33.3 ms / 0.28 ms（严格串行，全局写锁） |

self 的 5.0 ms 里，减去读路径 0.18 ms 后约 4.8 ms 就是审计 fsync——与 `AppendEvent`
实测的 4.2 ms 吻合。**审计 fsync 是 sandboxd 自身开销的 ~96%，占端到端 ~20%；Docker
3 往返（19.4 ms）是外部依赖，无法优化。**

### 做法：并行，不是异步

文档立场（`LEARNING-GUIDE.md` §11.10、`THREAT-MODEL.md`）早已写死：**把刷盘改成异步
改的是持久化语义本身，本项目选择少写**。所以本版**不**把审计改成异步/批量丢盘，而是
把那次 fsync 从「backend 往返**之前**串行」改成「在独立 goroutine 里与 backend 往返
**重叠**」，并在 exec 返回前 `WaitGroup.Wait()` 等它落盘。

- 持久化契约不变：**客户端收到 exec 响应 ⇒ 这条 `sandbox.exec` 已 fsync**，
  「崩溃不丢已确认事件」成立。
- 唯一变化在请求**内部**：事件由「命令开始前已落盘」变为「响应发出前已落盘」。
  极窄窗口（命令已开始、审计尚未落盘）内崩溃，可能丢一条**其响应从未到达客户端**
  的 exec 审计。这是审计完整性的一次刻意、显式、有注释的取舍，不是异步。

### 效果（实测）

| 指标 | 优化前 | 优化后 |
|---|---|---|
| exec 端到端 p50 | 24.5 ms | 20.0 ms（−18%） |
| exec 端到端 p99 | 58.7 ms | 37.8 ms（−36%） |
| self（审计+HTTP+认证+meter）p50 | 5.0 ms | 0.85 ms（−83%） |

self 从 5.0 ms 降到 0.85 ms：审计 fsync 的串行开销与抖动被 Docker 往返掩盖。

### 验收

- `internal/service`、`internal/gov` 单测全绿；`TestAuditTrail`（端到端）验证
  exec 返回后 `sandbox.exec` 事件仍立即可查（`WaitGroup.Wait` 保证落盘先于返回）。
- 注释在 `lifecycle.go` 的 exec 闭包处写清了语义边界，与 THREAT-MODEL 对齐。

## v1.1.7（2026-10-12）— 修复挂载：只读宿主目录挂载（opt-in、allowlist 门禁）

起因是 pi 接入实测暴露的可用性缺陷：pi-sbx 只把 `bash` 路由进沙箱后，`bash`
**看不到项目文件**（宿主目录未挂载），跑不了 `python test.py`。此前的结论是
"不挂任何宿主目录"是最硬边界，于是文件要么靠 heredoc 传、要么靠文件工具绕过沙箱。
本版在**不放开写边界**的前提下补上这一缺：

### 只读宿主目录挂载（`spec.mounts`）

- **只读，不可协商**：`runtime.Mount` 类型里根本没有"可写"字段——可写宿主挂载 =
  宿主写逃逸，所以它在类型层面被消灭，而不是靠一条能被删掉的校验。OCI 后端把它
  转成 `ReadOnly=true` 的 bind mount + `rprivate` propagation。
- **默认关闭**：`mount.allow` 白名单为空时，任何挂载都被 403 拒绝，隔离契约的
  `no_host_directory_is_mounted` 金丝雀**保持默认通过**（`tests/isolation_test.go`）。
- **allowlist 门禁**：`source` 必须是 `mount.allow` 列出的目录或其子目录（路径段
  对齐，`/srv/projects` 不会误放 `/srv/project`），否则 403——防止租户把 `/etc`、
  `/root/.ssh` 挂进沙箱读全盘。这是多租户下挂载功能不变成"整机读"的唯一防线。
- **走完整链路并持久化**：`spec.mounts` 存进 `SandboxSpec`（崩溃恢复从存储的
  spec 重建挂载），经 `mergeSpec` → `provision` → `CreateRequest.Mounts` →
  OCI `HostConfig.Mounts`。
- **修了一个本版几乎又犯的 bug**：`mergeSpec` 起初没透传 `req.Mounts`，导致请求的
  挂载被模板默认（空）静默丢弃——与 v1.1.5 `idle_timeout_seconds` 同类的"静默丢弃"
  缺陷。已加 `if len(req.Mounts) > 0` 覆盖分支，并有 `TestCreatePassesMountsToBackend`
  回归守护。

### pi-sbx 接入

- 扩展默认把 `process.cwd()`（或 `SBX_WORKDIR`）以只读方式挂到 `/workspace`，
  并把 bash 工具的 cwd 设为 `/workspace`、把 cwd 透传给 exec 的 `workdir`——这样
  `python test.py` 的相对路径真在项目目录里解析。
- `SBX_NO_MOUNT=1` 回到旧行为；`SBX_MOUNT_TARGET` 改挂载点。daemon 未配
  `mount.allow` 时创建被**诚实拒绝**（403），不是"文件静默不存在"。
- `tests/pi-sbx-harness.mjs` 从"断言 /workspace 未挂载"改为"断言挂载可读 + 写入
  被阻 + 宿主仓库未被触碰"（9/9）。

### 证据

- 端到端 `tests/mount_test.go`：只读可读、相对路径（workdir=/workspace）解析、
  写入失败、宿主不变、白名单外 403、默认关闭 403。
- 单元：`oci.TestMountsAreAlwaysReadOnly`/`TestMountsSurviveJSONEncoding`、
  `domain` 挂载形状校验、`service.TestMountsAreAllowlistGated`/`TestMountAllowlistPrefixBoundary`。
- 一个方法论教训落到测试里：**`test -w` 对 root + 只读挂载会误报"可写"**（mode-bit
  回声），真实写入才返回 EROFS。断言必须用真实写入，不用 `test -w`。

## v1.1.6（2026-10-11）— 补齐选型表上三个「一眼淘汰」的空白：计量、基准、幂等

起因是一份横向厂商调研（`~/Desktop/Agent Runtime 厂商调研报告.md`），它的结论是
**Runtime 已经商品化**，差异化发生在 Harness；而 MCP 变为无状态协议、计费单位统一为
资源秒这两件事，把三个缺口从"最好有"升级成了**硬要求**。本版只做这三件，不碰 Harness
（战略取舍见 `docs/ROADMAP.md` §0）。差距分析与证据链在 `docs/RESEARCH.md` §4。

三个 P0 都对应"客户在选型表上第一眼就能淘汰你"的空白，且都带验收测试、都能一键复现：

### P0-1 ✅ 用量计量 API（`GET /v1/metering`）

调研 §3.3/§3.5 的计费单位是 vCPU 小时 / GiB 小时 / 会话小时，且"内存照计、I/O 视状态"。
本版实现：

- **资源秒 = 采样积分**（采样器对"并发分配 × 墙钟时间"积分），**不是**请求数 × 固定费率。
  沙箱挂一小时、零 API 流量也能被正确计量。
- **CPU 与内存分开**：CPU 只在可调度状态（running/provisioning/resuming）累计；
  **内存在任何非终态都累计**（暂停的沙箱仍占工作集）——即"内存照计"。
- **exec 计数走请求路径的内存增量**，与资源秒同频（`meter.every`，默认 30s）落盘，
  忙碌租户不会为每个 exec 付一次 fsync。读接口永远返回"已落盘 + 未落盘尾"。
- 崩溃至多丢一个采样区间（计量本质上是采样器而非同步台账）。每租户一条 `Usage`
  记录进 WAL，跨重启 + 压实不丢。
- 租户 Key 见本租户；admin Key 用 `?tenant=` 或看全舰队。

新增：`internal/domain/usage.go`、`internal/service/meter.go`、store 的 usage 实体
与 `fkUsage` 帧。验收：`internal/service/meter_test.go`、`internal/store/usage_test.go`、
`tests/metering_test.go`。

### P0-2 ✅ 冷启动/吞吐基准（`make bench`）

调研 §7.1 选型六问之首就是"冷启动多快、吞吐多高"，而本项目此前"未测量、未公开"。

- `make bench` → `tests/bench_test.go`，产出 `docs/bench-summary.json`。
- 测：冷启动到 `/readyz`、exec p50/p99、建沙箱 p50/p99、80 并发建沙箱。
- **方法论**（继承 `tests/commercial_test.go`）：**只测量、不对绝对毫秒数设硬断言**——
  绝对预算是机器的属性，会在一台忙的笔记本上失败然后被静音。可依赖的是形状。
- 参考数字（2026-10-11，macOS / Docker 29.5.2，随机器负载浮动，**非承诺**）：

```
冷启动到 /readyz   7.699s   （被隔离自检这个 I/O 密集步骤主导，见下）
exec p50           24.611ms | exec p99 35.509ms
建沙箱 p50         184ms    | 建沙箱 p99 196ms
80 并发建沙箱      4.586s，全部准入（80/80）
```

口径说明：竞品宣称的 25ms/90ms/150ms 是厂商口径，第三方实测在 700–2800ms 区间；
本项目的数字是 `make bench` 一键复现的实测，冷启动被诚实写成范围而非最好值。
冷启动被隔离自检主导这件事本身是**信息**：自检用与生产完全相同的载荷路径起真沙箱、
读内核事实，是 I/O 密集的一步，但它把"隔离档"从形容词变成了可核对的事实。

### P0-3 ✅ exec 幂等键（对齐 Stripe 语义）

调研 §5.1：MCP 无状态化后，客户端收不到响应就必须重试，运行时必须保证重试不把命令
跑两遍——对 agent 意味着不会写两份文件、发两封邮件、扣两次款。

- 请求带可选 `idempotency_key`（≤128 字节、非空、无 NUL）；首次执行并记住结果；
  同键重试返回缓存结果 + `replayed: true`，**不再执行**；同键并发重试**单飞**成一次；
  同键配**不同参数**是客户端 bug，返回 409 而非静默返回旧结果。
- 作用域：键按（租户, 沙箱, 幂等键）隔离，跨沙箱/跨租户不串味。
- **边界（诚实声明）**：缓存是内存 + 大小上限（4096）+ TTL（10 分钟）。幂等键恢复的
  故障是"响应在传输中丢失"，几秒内重发；把最大 8 MiB 的 exec 输出写进 WAL 会把一次重试
  模式变成 fsync 放大器。daemon 重启清缓存，沙箱本身由 reconciler 存活——重启后的重试
  是一次新执行。
- 幂等重放**不计费**：命中缓存时 `exec_count` 不增加。

新增：`internal/service/idempotency.go`，`runtime.ExecResult` 加 `Replayed`。
验收：`internal/service/idempotency_test.go`（重放/单飞/参数冲突/键校验/有界+TTL 淘汰）、
`tests/idempotency_test.go`（HTTP 层 + 跨沙箱隔离）。

### 文档

- 新增 `docs/ROADMAP.md`：P0 已交付的验收标准、P1（凭据 vault / OTel 导出 / 检查点 /
  多租户形态）与 P2 方向、以及"刻意不做"清单（Harness / 模型路由 / eval / GPU）。
- `docs/RESEARCH.md` 新增 §4：三条行业事实 + 差距表 + 战略取舍。
- `docs/API.md` 新增计量端点小节、exec 幂等字段说明。
- `docs/POSITIONING.md`：劣势表由"无计量计费 / 无冷启动数字"改为已交付，
  §8 补充 `make bench` 可复现数字。
- `sandboxd.conf.example` 新增 `meter.every` 说明。

### 回归

`go build ./...`、`go vet ./...`、`go test -race ./internal/...` 与 `tests/` 全量通过；
新增单元测试（meter / idempotency / usage 持久化）与 3 个 HTTP 级 e2e 套件
（idempotency / metering / bench）。`make bench` 多次复跑一致。

### 修复：删除沙箱在并发簿记写入下返回假 409（产品缺陷）

全量回归（`tests/` 的 `TestNoBackendLeaks`）抓出一个**删除路径的 CAS 竞态**：

- `DeleteSandbox` 先 `Backend.Remove`（删容器）再 `St.DeleteSandbox(sb.ID, sb.Version)`
  （CAS 删记录）。删除是终态幂等操作，但记录删除却用了乐观锁。
- 而 `touchActivity` / reconciler / sweeper 这些簿记写入**即使传 `baseVersion=-1` 也会
  `Version+1`**（`-1` 只跳过 CAS 校验，不跳过版本自增）。只要其中任何一次落在
  "删容器"和"删记录"之间，CAS 就报 `409 "modified concurrently"` —— 此时容器**已经没了**，
  记录却卡住，客户端收到一个凭空出现的冲突。
- 症状正是本版回归中 `TestNoBackendLeaks` 的一次偶发失败：`DELETE ...?force=true` 返回
  409，而容器已被删除、只剩一条要等 reconciler 收敛的记录。

修复：记录删除改为 **CAS-free**（`baseVersion=-1`），并视并发删除造成的 `NotFound` 为
成功（两者都是"这个沙箱没了"这一目标的达成，不是冲突）。新增两个确定性回归测试
`TestDeleteSurvivesConcurrentVersionBump`（用 `fakeBackend` 的 `onRemove` 钩子在删除
进行中 bump 版本，复现原竞态窗口）与 `TestDeleteIsIdempotentAcrossConcurrentDeletes`。
`TestNoBackendLeaks` 连跑 5 次全过。

### 文档诚实修正：执行隔离 ≠ Agent 隔离

pi 集成实测（真 glm-5.3-flash 跑 pi-sbx 扩展）确认一个此前被文档含糊掉的边界：
`bash` 走沙箱，`read/write/edit/ls/grep/find` **仍在宿主机**。但三处文档把它写成了
"七种工具都路由 / read-write 也落在沙箱"，即**断言了没发生的事**：

- `docs/RESEARCH.md` §1 表格、§2.5：把"Gondolin 模式路由七工具"误并成"本产品同款"；
- `docs/DESIGN.md` §7 第 9 条：写"bash/read/write 工具实际落在沙箱内执行"（实测 write 落在宿主）。

修正：三处改成"v1 只路由 `bash`"；`integrations/pi/pi-sbx/README.md` 范围声明补正面结论
（这是**执行隔离**不是 **Agent 隔离**，且"路由文件工具"补不上 Agent 隔离、还会削弱"无宿主
目录挂载"这条最硬边界）；`docs/POSITIONING.md` §3 增补第 4 条边界。原则不变：**文档只断言
真实发生的事**。

## v1.1.5（2026-10-09）— 商用五维测试，以及一份能让新人独立应对技术面的学习文档

v1.1.4 回答了"它能干什么、不能干什么"。本版回答两个更硬的问题：

1. **按买方的验收口径独立测一遍** —— 高并发、安全、可用性、弹性、启动实效与
   响应性（其中延迟被明确要求"很重要"）。
2. **一个零基础的人怎么在有限时间里把这套东西学到能过面试** ——
   `docs/LEARNING-GUIDE.md`。

### 新增：商用五维套件 `tests/commercial_test.go`（8 个端到端场景）

安全不在这份文件里，因为它已经有三个专属套件（`contract_test.go`、
`containment_test.go`、`isolation_test.go`），复述一遍只会得到一个"绿但不新"的
测试。这份文件覆盖的是**此前有明确空洞**的四个轴：

| 空洞 | 原来是什么样 | 本版怎么做 |
|---|---|---|
| 启动实效 | 启动**只是一个 90 秒超时**（`Harness.WaitReady`）。套件能知道 daemon 起来了，永远不知道用了多久 | 新增 `Options.NoStart` + `Harness.StartAndTime`，把启动变成**被测量的量** |
| 响应性 | `tests/load_test.go` 文件头写明"延迟只报告不断言，因为那是机器的属性" | 那个立场对"绝对毫秒阈值"是正确的，对产品主张是错的。**把主张写成比值**：分母是同机同次运行里最便宜的请求（`GET /healthz`） |
| 弹性 | 空闲回收与 TTL 回收**连一个端到端测试都没有**，只有单元测试（`internal/service/service_test.go`） | 两个真跑过期流程的场景，其中一个顺带抓到一个产品缺陷（见下） |
| 可用性 | 只在负载**停止之后**检查健康（`load_test.go` 的 `the_daemon_is_still_healthy_after_load`） | 负载**进行中**持续采样 `/readyz`，并断言延迟不随时间漂移 |

延迟断言的具体形式（这是本版方法论的核心）：

- `p50(401) ≤ 2 × p50(/healthz) + 10ms` —— 拒绝由中间件决定，必须在任何后端工作之前；
- `p50(读) ≤ 10 × p50(/healthz) + 25ms` —— 已认证读只多一次存储查找；
- `p50(读, fleet=12) ≤ 3 × p50(读, fleet=1) + 20ms` —— 热路径不能是 O(沙箱数) 扫描；
- `p50(后半段) ≤ 3 × p50(前半段) + 30ms` —— 持续负载下延迟不能漂移。

比值不是取巧：分母是**同一台机器、同一次运行、同一个 daemon** 上最便宜的那个请求，
它捕获机器的噪声与调度，**不捕获产品的任何工作量**。另外每个端点都先 warm 再测，
否则排在最前面的那次测量会吸收所有一次性成本。

### 修复：`spec.idle_timeout_seconds` 被静默丢弃（**产品缺陷**）

`internal/service/service.go` 的 `mergeSpec` 有该字段的**默认回填**，却没有对应的
**请求覆盖分支** —— 它是唯一一个这样的字段。后果：

- 校验照常通过（`domain/sandbox.go:178` 检查了 60..MaxTimeoutSeconds）；
- 创建照常返回 **201**，不报错、不打日志；
- `docs/API.md:49` 把它文档化为可逐沙箱设置的旋钮；
- 而存下来的 spec **永远携带平台默认值**（默认 0 = 关闭）。

也就是说：**逐沙箱空闲回收在修复前不可达**。失败模式是"最坏的一种"——无错误、
无日志、有 201，客户端以为自己设置了回收。

发现路径（值得记录）：新的空闲回收场景在预算 60s、`sweep_every=1s` 下，
**95 秒都没回收**。先怀疑测试，逐项确认 `sweep_every` 生效、沙箱是 `running`、
`IdleExpired` 判定用的是 `LastActivityAt`/`CreatedAt` + 预算 —— 都正常。
剩下的唯一可能就是预算根本没写进沙箱，于是读到 `mergeSpec`。

修法 3 行，并把**这个场景本身**变成回归测试：它断言的是**回收真的发生了**，
而不是"响应体里有那个字段"。

### 修复：`WaitForState` 在 `limit <= 0` 时空指针 panic（**脚手架缺陷**）

一次误写 `ttl+40`（无类型常量而目标类型是 `time.Duration`，于是成了**纳秒**）
让轮询循环一次都没执行，`last` 为 nil，然后在**失败信息里**解引用。
修法：加正数校验 + nil 检查。

这条值得单独记下来，因为它违反了一条 fixture 的底线：**fixture 不该"爆炸"，
它应该"报告"**。用纳秒截止时间导致的真实问题是"没有任何东西被观测到"，
而报出来的是一段栈回溯。

### 实测数字（macOS / Docker 29.5.2 / Go 1.26.5）

```
启动  冷启动到 /readyz 200 = 727ms ｜ ready 后首个建沙箱请求 = 188ms
      带 9 个存活沙箱重启到 ready = 166ms，9 个全部仍 running，后端对象数不变

延迟  /healthz(地板) p50 73µs ｜ 401 伪造 Key p50 96µs
      读沙箱 p50 81µs ｜ exec p50 26.885ms
      → 控制面自身在 exec 上只增加约 10µs，其余全是容器运行时往返
规模  fleet 1→12：读 p50 82µs→77µs（×0.9）｜ exec p50 24.364ms→27.268ms（×1.1）
      诚实标注：exec p99 由 28.9ms 升到 155ms，这是引擎调度长尾，只打印不设界

并发  80 个同时建沙箱（限速 10/s burst 10）421ms 内完成
      → 201×10 / 429×70，全部 429 带 Retry-After
      → 租户记录数 = 10，容器数 = 基线+10（零幽灵记录、零泄漏）

可用  12 秒持续负载 2,968 次请求全 200（0 个 5xx / 0 个 429 / 0 个无响应）
      /readyz 采样 47/47 成功 ｜ p50 前半 41.0ms → 后半 41.4ms（×1.01，无漂移）

弹性  空闲预算 60s + 1s 扫描 → 1 分 6 秒回收；同期 twin 承受 13 次 exec 而存活
      TTL 35s → 36 秒回收，且配额=1 被真正归还（随后新建成功）

租户  安静租户 p50 79µs → 邻居风暴中 229µs（×2.91）→ 风暴后 85µs
      风暴期间安静租户零拒绝 ｜ 吵闹租户 200×8 / 429×4，零 5xx

全量  go test ./tests/ 228.5s，34 个顶层用例 0 失败（1 个刻意 skip 的破坏性用例）
```

### 新增：`docs/LEARNING-GUIDE.md` —— 从零基础到能接住面试

面向"计算机基础扎实、此前完全没接触这个项目、时间有限"的读者。组织原则是
**每个技术点都要能指向真实代码或真实跑出来的数字**，别人没用的技术不写，
写了的技术都给 `文件:行号`。含：Go（只讲本项目用到的）、Shell（只讲本项目用到的）、
容器与隔离、持久化与崩溃恢复、治理、HTTP 契约、测试方法论、运维，
以及 **68 道面试题（含 10 道压力题/陷阱题）** 和一份自测清单。

压力题部分刻意不美化：例如被问"你的沙箱绝对安全吗"，答案是
**"不，容器档共享内核，没有安全边界只有成本边界"**，并给出可随时核对的
`/v1/isolation`。**面试时最有价值的不是把产品说强，而是知道它在哪失效。**

### 工具

- `make test-commercial` —— 只跑这五个轴，约 4 分钟。慢是诚实的代价：
  空闲与 TTL 回收有 60s 下限（`domain.SandboxSpec.Validate`），
  要按 API 口径测过期，就得等过期。
- `Harness.StartAndTime` / `Options.NoStart` / `withIdleTimeout` 三个 fixture 补充。

### 一处需要说明的环境观察

本版第一次跑全量 `go test ./tests/` 时，进程**远超自身的 `-timeout 40m` 仍未结束**，
且因为输出被 `| grep | tail` 管道缓冲，看不到任何中间状态，无法定位何处停住。
杀掉改用落盘日志重跑后，**228.5 秒干净通过**。根因未定位，故如实记录，不冒认结论。
附带的教训与 v1.1.4 那条同源：**一个把证据藏起来的采集方式，
会让"没有证据"和"没有故障"变得不可区分。**

## v1.1.4（2026-10-09）— 把定位落到一个跑得通的企业场景上，并把边界画进图里

v1.1.3 把验收从 shell 搬进了 Go 测试。本版回答另一个问题：**"所以它到底能干什么、
不能干什么"** —— 用一个真实的客户场景走一遍入口到结束，然后把结果变成一张
由调用轨迹渲染的图，而不是一段形容词。

### 新增：企业场景端到端演练 `tests/e2e-enterprise-scenario.sh`

场景：**多业务线 AI 编程助手的代码执行底座**。七个幕，47 次真实调用，43 项断言：

1. 平台接入与合规取证（无凭证读隔离档 → admin 读内核事实 → 建租户 → 发 Key → 模板）
2. Agent 干活（建沙箱 → 传源码 → 执行生成代码并落盘 → 读回产物）
3. 环境复用（快照 → 从快照冷启 → **读回只在沙箱 1 存在过的文件** → 证明复用成立）
4. 成本控制（暂停回收内存 → 显式恢复 → 透明恢复 → 状态机拒绝非法 resume → 延长 TTL）
5. 治理边界（401/403/429/**402**/乐观锁 409/501）
6. 审计与回收
7. 崩溃恢复（`kill -9` → 重启 → reconciler 从后端重建真相）

每发起一次请求就向 `docs/scenario-trace.jsonl` 追加一行 JSON，包含方法、路径、
状态码与**由 curl 自身测量的耗时**。这只脚本不是 Go 套件的替代品——它是面向买方的
演练，也是流程图的唯一数据来源。

### 新增：`docs/POSITIONING.md` —— 细化后的产品定位

卖给谁 / 不卖给谁 / 威胁模型边界 / 与 Docker 的分界 / 为什么用 Docker Engine /
竞争格局 / 胜负清单 / 本场景证明与**未证明**的东西。所有数字来自上面的演练。

### 新增：动态流程图 `docs/scenario-flow.html`（由轨迹渲染）

自包含、无外部依赖、支持浅色/深色。左侧列出 47 次调用，右侧是分层组件图；
点击任一步（或按播放）会点亮该次调用**实际经过**的组件，并显示状态码与实际证据。
未实现 / 不可强制 / 已知缺口分成三组单独成带。

图的生成器是 `tests/render-scenario-flow.py`：它只接收 trace，不接收手写的流程描述。
**改流程必须重跑演练**，而不是改那张图。

### 实测数字

```
真实调用 47 次   断言 43/43 通过
响应分布 200×23 201×9 204×4 401×1 402×1 403×3 409×3 429×1 501×2
隔离档   container-shared-kernel（自检已执行，userns=off）
隔离证据 10 项事实，其中 9 项被断言，失败 0 项
耗时     中位 7ms ｜ 建沙箱 163ms ｜ 创建快照 137ms ｜ 从快照建沙箱 160ms ｜ exec 26ms
审计     24 条事件
```

### 演练确认/复现的三个边界与缺口

1. **`disk_mib` 与 egress 白名单返回 501（unimplemented），不是 400。** 语义是
   "本节点不具备该能力"，客户端应据此区分"改请求"与"换部署"。
2. **平台默认速率硬编码，本配置文件无对应键（已知缺口）。** 管理面 bucket 是
   `1/s burst 5`，演练中连续的管理面调用**确实收到过 429**。全平台调整只能逐租户
   下发 `quota.rates`。
3. **`PATCH /v1/tenants` 是整对象替换，不是合并（已知缺口）。** `updateTenant`
   执行 `t.Quota = *body.Quota`，因此只带 `max_sandboxes` 的 PATCH 会把该租户的
   自定义 `rates` 一并抹掉、静默回退到平台默认。**本次演练已复现**：一次遗漏
   `rates` 的配额修改导致后续创建意外 429。演练脚本已改为每次写全 `rates`，
   但语义本身值得商榷。

### 文档订正

- README 的"该项目累计 12 个产品缺陷"删除。那个整数**无法从历史记录客观重建**
  （v1.0.0 记 9 条、v1.1.1 记 9 条、v1.1.3 记 3 条，按任何口径都不是 12），
  而一个重建不出来的数字不该出现在给客户看的文档里。改为指向逐版清单。
- README 第六节的 Daytona 一行由"2026-06 起闭源"更新为"2026-06 闭源、
  2026-10-03 仓库归档、控制面在厂商侧"，并补上其退场理由对本项目定位的含义。

### 工具

- `Makefile` 新增 `scenario`（跑演练）与 `scenario-flow`（跑演练并重新渲染流程图）。
- 演练脚本本身在 macOS 的 bash 3.2 上可用：空数组展开用 `${arr[@]+"${arr[@]}"}`
  （`set -u` 下空数组是 unbound），管理面调用尊重 `Retry-After` 重试，
  多字节字符紧跟变量处一律加花括号（`${VAR}（` 会被 bash 误读为变量名的一部分）。

## v1.1.3（2026-10-09）— 端到端验收改成"可复用的证据"，并修掉它抓出的缺陷

v1.1.2 把隔离契约钉死在创建请求里。本版处理**怎么证明它**：把六份各自重复三十行样板
（写配置 → `pkill` → `rm -rf` → 启动 → `sleep 2` → 从日志里 grep 出 admin key → 三个
curl/python 辅助函数 → 两个计数器）的 shell 脚本，重写成**一个 Go 测试包 + 一个共享 harness**。
822 行 bash 退役，换成 26 个套件、83 个子场景。

### 为什么改：那些样板本身就是故障源

原脚本的每一步都有失败模式，而没有一步是被测对象：`sleep 2` 在慢机器上不够；从日志里
grep admin key 在 key 格式变化时静默失败；每个脚本自行 `pkill -9 -f` 别人的进程。
新 harness 用**固定的** bootstrap key（daemon 每次启动都 upsert 它，所以夹具在进程输出
任何一个字节之前就知道凭据），并复用生产侧的 `internal/domain` 类型——字段改名变成**编译错误**，
而不是运行时静默漏测。

### 新增：容器级安全契约文档 `docs/SECURITY-CONTRACT.md`

对客户的可核对承诺：22 项从**运行中沙箱内部**读取的内核事实、遏制能力的实测数字、
以及一节**明确不承诺**的边界（共享内核 / 磁盘配额不可强制 / egress 白名单不可强制 /
userns 未配置时身份不分离 / 非只读根 / 配额非精确记账）。每一条都写明对应测试名与
自行核对命令。`docs/THREAT-MODEL.md` 的验证矩阵改为引用真实测试名。

### 产品缺陷（由新套件抓出并修复）

1. **畸形 ID 返回 500 且 `retryable: true`。** `idgenCheck` 直接返回 `idgen.Parse` 的错误，
   `apperr.From` 把它包成 *internal*，而 internal 被标记为可重试。于是一次**拼写错误**
   生成无限重试风暴，并被报告成服务端的错。改为 400 `invalid_request`、`retryable: false`；
   形状正确但不存在的 ID 仍是 404。
2. **运维在配置里提供的 bootstrap admin key 仍被打印到 stdout/journal。** 该文件是配置，
   stdout 进 journal。改为只有本进程**生成**的 key 才回显，配置提供的只记一行
   "not printed"。
3. **孤儿快照镜像无限增长，且代码注释声称不存在 GC 在管。** 快照/模板镜像带标签
   （`sandboxd-snap-<id>`），因此**不是**悬空镜像，`docker image prune` 永远不删它们；
   数据目录丢失、从提交前的备份恢复、节点重建，都会让镜像永久留在盘上。
   `snapshot.go` 的注释写着"node GC 会回收未打标签镜像"——两头都错。新增
   `runtime.Backend.ManagedImages`、`store.ForEachSnapshot/ForEachTemplate`，由 reconciler
   回收无人引用的镜像，并保护任何仍被记录引用的镜像（含**从快照创建的活沙箱**所用的镜像）。
   实测：本机遗留 **89** 个 → 回收后 **0** 个。见 `TestOrphanedSnapshotImagesAreReaped`。

### 夹具缺陷（同样修掉，因为它们会伪造结论）

4. **`Resp.Err()` 永远匹配不到任何东西。** 夹具把响应体解成扁平结构，而服务端把错误包在
   `"error"` 之下。十几个断言因此声称"响应体里没有错误包络"，而包络就在消息里。
5. **夹具里的清理被管理面静默拒绝。** 管理 key 操作租户资源必须带 `X-Tenant-Id`，
   否则 403；而 `Drain()` 不带，于是它一直是**静默空操作**。改为跟踪沙箱归属、带租户头删除。
6. **一个断言与自身比较。** 曾用 `cat /proc/1/comm` 的输出与同一命令的输出比较——
   在包括彻底损坏的宿主上都能通过。改为写入标记文件再经 `/proc/1/root` 读回。
7. **限流场景实为计时场景。** 令牌桶连续补充（`tokens += elapsed*perSec`），
   所以等容器启动的 ~1.2s 会退还一个整令牌，"1/s" 的限流永远测不出来。改为 0.01/s
   并只钉住 `create` 一个桶（钉全部桶会让该场景自己的 DELETE 也被限流，沙箱被滞留）。

### 断言收紧：把"产品不承诺的事"从测试里拿掉

8. **配额测试曾断言"恰好 5 个"。** `gov.Quotas.CheckCreate` 的注释明确写了竞态下的尽力而为语义。
   实测：20 路并发创建对上限 5，**20 个全部放行**（越限 15），突发后下一次创建被拒绝。
   测试改为断言真实成立的三条——不**低**准入、无记账漂移、突发后必然收敛——
   并把越限数字打进测试日志，让这个数字留在记录里而不是被抹平。
   （顺带确认 `quota_exceeded` 映射为 **402** 而非 429 是**刻意的**：`apperr` 把它定义为
   "计费形态的拒绝"，与"稍后重试"是不同的指令。）
9. **"创建无法完成"的断言指向了错的失败路径。** 断言的是后端启动失败（409 + `state: failed`），
   实测走的却是准入前的 404"镜像不在本节点"。现在两条路径各有专门场景：新增一个
   **真实存在但启动即死**的镜像（基于基础镜像删掉 `/bin/sleep`，于是 PID 1 以 127 退出），
   以及一个未导入镜像的准入前 404。
10. **遏制场景之前只存在于 shell 里，且没有正向对照。** `rm -rf /`、fork 炸弹、内存炸弹、
    CPU 独占、沙箱内写宿主路径——全部移植为 Go 测试，并补上**正向对照**：先证明恶意载荷
    真的执行并生效，否则"宿主没出事"可能只是"载荷没跑"。例如宿主路径写入场景会先确认
    沙箱内那次写入**确实成功**（`rc1=0 rc2=0`），再断言宿主文件摘要未变。

### 实测数字（测试日志原话）

```
内存炸弹（64 MiB 上限）        exit=137（SIGKILL）；邻居沙箱继续正常 exec
CPU 独占期间的控制面           5 次健康检查 1 ms；无关沙箱 exec 36 ms
fork 炸弹                     timed_out=true；daemon 全程可响应；沙箱仍可回收
20 路并发创建对上限 5          20 全部放行（越限 15）；下一次创建 402
24 并发创建 / 200 次 exec      零 5xx；创建 p50=836ms p99=1.03s；exec p50=62ms p99=121ms
孤儿快照镜像                   89 → 0
```

### 已知缺口（本版未改，写在文档里而不是藏着）

- 平台默认租户速率**硬编码**，配置文件无对应键；全平台调整只能逐租户下发。
  `create_queue_timeout` 同样不可配置。
- `DELETE /v1/sandboxes/{id}` 与创建共用租户 `create` 令牌桶：租户打满 create 预算期间
  也无法删除。默认 1/s burst 5 下影响有限，但属于设计取舍而非疏漏。

### 其他

- 新增 `Makefile` 目标 `test-e2e-race`：用 `-race` 构建**被测 daemon**，
  让竞态检测器覆盖 daemon 自己的 goroutine，而不只是 harness 的。
- `internal/runtime.Backend` 新增 `ManagedImages(ctx)`（fc 骨架返回 unimpl，
  而不是空列表——空列表会被读成"没有需要回收的东西"）。
- 退役 `tests/{chaos,disaster,load,malicious,multiuser,security}.sh`；
  保留 `tests/pi-sbx-harness.mjs`（它验证的是 `integrations/pi` 这一独立层）。

## v1.1.2（2026-09-27）— 把容器档的隔离契约钉死，并让"隔离"变成可证明的

v1.1.1 让隔离**等级**不可静默降级。本版处理等级内部的那一层：**同一等级里具体做了什么**，
以及如何确认它真的生效。起因是一次实测——把 `HostConfig.UsernsMode` 设为 `private` 后，
引擎接受了它、从 `/json` 回读为 `"private"`，而沙箱仍然拿着 identity uid 映射。
**读配置不是证据，只有内核报告的事实才是。**

### 新增：安全契约从宿主 `daemon.json` 收回创建请求

`internal/runtime/oci` 的 `hostConfig` 只显式写了 6 个字段，其余全部由宿主 `daemon.json`
决定——一个换引擎版本或换运维就能改、而这份代码毫无变化的文件。现在每个沙箱依赖的字段都显式写出，
**即使值与今天的引擎默认相同**：`IpcMode`（`default-ipc-mode` 默认 `shareable`，
别的容器可加入本沙箱的 IPC 命名空间）、`CgroupnsMode`（cgroup v1 上默认 `host`）、
`Privileged`、`CapAdd`（空列表也写出：未设置的字段是引擎可以填的字段）、
`MaskedPaths`/`ReadonlyPaths`（引擎默认集合的显式副本）、`UsernsMode`（永不省略）。
`apparmor=docker-default` 只在 `/info` 报告存在 AppArmor 时才写。

### 新增：user namespace 三档，且拒绝"设了但没生效"

`userns = auto | require | off`。判断依据是 daemon 自己的 `/info`
（`SecurityOptions` 是否含 `name=userns`），**不是**请求它然后假定生效。
`require` 在不具备时拒绝启动，报错点名 `userns-remap`/`daemon.json`/`userns=off` 三条出路；
`auto` 降级到 `host` 时把降级写进启动日志与 `/readyz`。
`UsernsMode` 永不省略：留空不中性——配了 remap 的引擎会把新容器默认设为 `private`，
于是"未设置"在不同宿主上含义不同，`userns=off` 也就无法真正关闭它。
未探测过的后端一律发 `host`，即**不声明自己没验证过的东西**。

### 新增：隔离自检 `VerifyIsolation()`（成为 `Backend` 接口的一部分）

用与生产完全相同的载荷路径起一个真沙箱，在里面读内核事实——`/proc/self/uid_map`、`Seccomp`、
`NoNewPrivs`、`CapEff`/`CapBnd`、`/proc/self/attr/current`、`ulimit -c`——并尝试两个逃逸探针
（`mount`、读 `/proc/kcore`）。结果有三种且**不允许互相冒充**：全项通过、有项失败、
**无法测量**（探针没跑起来，或探针与解析器对不上键、需要改代码）。第三种返回错误而不是结果，
因为一个测不出东西的检查，既不能报"通过"也不能报"发现漏洞"。
启动时执行一次；摘要进 `GET /readyz`（自检从未运行时报 `isolation_verified: false`
并说明"这是配置，不是已验证的事实"），完整证据进 `GET /v1/isolation`（需 admin——
当前生效的控制清单对正在策划逃逸的人就是侦察情报）。
`fc` 骨架的 `VerifyIsolation` 返回 unimpl，而不是一个空报告——空报告会被读成"没问题"。

### 新增：两处此前未覆盖的资源控制

- **`core=0`**：本产品会把沙箱文件系统 `Commit()` 成快照，一次 core dump 会把进程内存
  （含其持有的凭据）写进一个会被存储并分发给其他沙箱的产物里。是数据外泄控制，不是整洁性设置。
- **`nofile=1024/65536`**：fd 是宿主全局资源，没有按沙箱上限时一个租户可以耗尽宿主表并饿死其他租户。

`ReadonlyRootfs` 显式钉为 `false`（**刻意**，非遗漏）：只读根与快照模型不兼容——
`Commit()` 捕获可写层，只读根会产出空快照。要与"可写 workspace overlay"一起重新设计。

### 修复：自检自身发现的两个 bug（都是本版新增代码的）

1. **解析器与探针的键名漂移**：断言用 `no_new_privs` 而探针输出 `nonewprivs`，
   导致 7 个事实在拿空字符串与期望值比较，其中 `uid_map` 走的是"仅记录"分支，
   于是**以空观测报了成功**。修法不是补齐字符串，而是改成键只出现一次的结构
   （`addEq`/`addPred` 自行从 `obs[key]` 取值和判定），并加 `probeKeys` 守卫：
   探针少输出任何一个键就返回错误，而不是产出结果。
2. **`uid_map` 的空格对齐**：`/proc/self/uid_map` 的真实字节是
   `"         0          0 4294967295"`，与单空格常量比较会**认不出 identity 映射**，
   从而报告一个不存在的 user namespace。改为折叠空白后比较。
   回归用例直接使用实测到的带空格原串。

### 测试

- `internal/runtime/oci/isolation_test.go`（新增）：载荷层 6 个用例（不需要 daemon），
  含"每个钉死字段都必须出现在 **JSON 字节**里"——Go 值对而线上载荷更弱正是原始缺陷的形态；
  以及 `uidMapIsRemapped`/`allZeroHex`/`missingProbeKeys` 的边界用例。
  需要 daemon 的 5 个：自检必须全绿、自检不留残余容器、探针跑不起来必须报错而非通过、
  `userns=require` 在不具备的 daemon 上必须失败且消息可执行、`auto` 必须披露降级。
- `internal/config/config_test.go`：`userns` 三档校验（含 `"true"`/`"false"` 必须被拒——
  这个姿态不是布尔值，"关"与"没要求"不是一回事）、文件与环境变量两条路径、默认值必须是 `auto`。

## v1.1.1（2026-09-27）— 修复会丢数据/被放大的缺陷，并让隔离等级不可静默降级

一次针对"能否对外商用"的独立审查发现了 4 个缺陷与 3 个可被程序化验证的诚实性缺口。
本版全部修复，并为每一个补了**能在修复前失败**的回归测试。

### 修复：会丢已确认数据的两个 P0

1. **WAL 追加曾发生在写锁之外**（`internal/store/store.go`）。`PutTenant/PutKey/
   PutSandbox/PutSnapshot/PutTemplate/AppendEvent` 先 `append`（写盘 + fsync）
   再取锁改内存 map，而 `Update*/Delete*` 是锁内完成——两套约定并存。
   后果：`Compact()` 可在"帧已落盘、内存未更新"的窗口抢到锁，把该记录连同 WAL 一起抹掉，
   调用方却已收到成功。实测 2880 次写入全部返回成功、重启后丢 120 条（4.2%，120/120 轮复现）。
   现在所有变更都在同一把写锁内完成 append + 发布。
   回归守卫：`TestAppendAlwaysUnderWriteLock` 用 AST 扫描断言 12 个追加点全部持锁。

2. **撕裂写的"回滚"是空操作**（`internal/store/wal.go`）。回滚调用
   `Truncate(walSize())`，而 `walSize()` 返回的是**写入之后**的文件大小，截断到当前大小即空操作。
   更糟的是 fsync 失败时返回错误却把帧留在盘上——重启后会被重放，成为"被告知失败却生效"的幽灵写。
   现在：写入前记录偏移，失败（含 fsync 失败）回滚到该偏移并 fsync；回滚本身失败则
   **毒化存储**，后续写入快速失败、`/readyz` 明确报因，而不是在状态未知的日志上继续跑。

3. **重放曾在首个坏帧处静默截断其后全部内容**。现在先**分类**再处置：
   尾部撕裂是常规崩溃残骸（自动修掉）；日志**中段**损坏意味着其后是已 fsync 且已确认的数据，
   默认**拒绝启动**并给出可执行指引，`wal_repair = salvage` 是显式逃生舱，
   `strict` 则连尾部撕裂也要人来看。三种策略都产出量化的 `RepairReport`。

4. **flock 句柄是包级全局变量**，导致打开第二个 Store 会覆盖第一个的句柄：
   关闭任意一个都会释放另一个的排他锁并泄漏一个 fd。现在由 Store 实例持有；
   `Open` 在中途失败时会释放锁，`Close` 幂等。

### 修复：两个可被单请求利用的 P1

5. **exec 的 `max_output` 没有上限**。控制面把输出缓冲在自己的堆上，因此该字段是"上限"
   而非"偏好"，但代码把它当偏好：单请求 `max_output=2000000000` 被接受，实测令守护进程
   材料化 254 MiB。现在新增 `max_exec_output_bytes`（默认 8 MiB），超限在准入处返回
   400；负数同样被拒（HTTP 字段是 int，静默当默认值会掩盖客户端 bug）。
   `runtime.ExecRequest` 的字段注释由"偏好"改为"上限"，后端另设硬顶做纵深防御。

6. **未认证流量完全不受限流，且每次失败一次 fsync**。中间件把限流放在 `authenticate`
   **内层**（因为需要已认证租户），于是认证失败的请求根本不经过限流，却仍写一条审计事件
   = 一次 fsync。实测 400 个未认证请求产生 400 个 WAL 帧与 400 次 fsync，零 429——
   一个不需要任何凭证的 I/O 放大器。现在分两级限流：`preauth.*` 按客户端 IP 设防洪阈值
   （位于认证**之前**，刻意不读可伪造的 `X-Forwarded-For`），
   租户配额仍在认证后生效；认证失败审计按 IP 分窗聚合（首次全量 + 每 100 次带计数），
   `sandboxd_auth_failures_total` **不做采样**。实测 400 次拒绝现在只落 1 个 WAL 帧。

### 修复：运维可观测性

7. **指标标签值全部丢失**。三处调用点传的是 `[]string{"status"}`（只有标签名没有值），
   而 registry 按名值对遍历后整体丢弃，于是每条序列都渲染成无标签、维度凭空消失。
   修好调用点传真实值；registry 对奇数长度标签集改为**可见地**渲染（`name="<missing>"`）
   并计入自检指标，而不是静默丢弃；`key()` 改用规范化渲染消除标签集碰撞；
   exposition 输出转义标签值，避免调用方可控的字符串破坏整个抓取。
8. **`/metrics` 未认证可读**。它由 `mux.HandleFunc` 直挂，绕过了整条中间件链。
   现在需要 **admin scope**。`/healthz` 与 `/readyz` 保持不鉴权（负载均衡需要）。
9. **限流器有两个缺陷**：`sweeper` 没有停止通道（每个 Service 永久泄漏一个 goroutine）；
   桶键用租户 ID，而平台管理员 Key 的租户 ID 为空，导致全舰队所有管理员凭证共用一个桶、
   最忙的那个饿死其余。现在有 `Stop()`（并入 `Service.Stop`）、身份化桶键（带长度前缀，
   任何输入都无法互相碰撞）、桶表上限与最旧淘汰。

### 变更：隔离等级成为一等公民，且不可静默降级

10. **`Backend` 接口新增 `IsolationTier()`**，`oci` 返回 `container-shared-kernel`，
    新建后端**必须**声明自己的等级。
11. **新增 `isolation` 配置**（`auto`/`container`/`microvm`）。要求 `microvm` 而本机
    不具备时**拒绝启动**；v1.1.0 只记一条 warning 然后继续用容器跑，且没有任何选择路径——
    强隔离后端在运行期根本不可达。
12. **`GET /readyz` 公开 `.isolation` 与 `.isolation_note`**（不鉴权）。客户可以程序化核对
    自己买到的到底是什么档，而不必信任 README。
13. 修正 `docs/DESIGN.md` 中"Sandbox 含 node 信息"（实际没有 node 字段）与 README 中
    "适用于运行不可信代码"的表述，改为明确写出当前只有容器档及其后果。

### 测试

新增 `internal/store/regression_test.go`、`internal/config/config_test.go`、
`internal/metrics/metrics_test.go`、`internal/gov/limiter_regression_test.go`、
`internal/service/exec_ceiling_test.go`、`internal/httpapi/regression_test.go`。
`poc/tornframe` 由"缺陷复现"改写为覆盖 3 种恢复策略、5 个场景的验证 harness。

全部 71 项端到端验收断言与 `go test -race ./...` 保持通过。

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

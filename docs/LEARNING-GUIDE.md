# Sandcastle 全栈学习文档 —— 从零基础到能讲清楚每一个设计决策

> 读者假设：计算机基础扎实的研究生，**此前完全没接触过这个项目**，也没系统学过 Go 与 Linux 内核隔离。
> 目标假设：读完能**讲清原理与取舍**，不是记住结论。

## 怎么用这份文档

这份文档是**教科书结构**，不是速查表。每一节都按同一个顺序展开：

1. **问题** —— 没有它会怎样（先给失败场景，而不是先介绍概念）
2. **原理** —— 它到底怎么工作，必要时给最小可运行例子
3. **本项目怎么做** —— 真实代码摘录 + `文件:行` + 逐行解释
4. **替代方案** —— 至少一种别的做法，说清为什么没选它
5. **怎么验证** —— 一条可执行命令或一个测试，说明预期看到什么

每章末尾有 `### 本章检验`：几个必须能答的问题 + 一个动手任务。**答不出来就回头重读那一节**，不要跳过去。

| 剩余时间 | 读法 |
|---|---|
| 半天 | §1 全读 → §5 → §6 → §9 → §10（主干：定位、隔离、持久化、一致性） |
| 两三天 | 按顺序全读，每章做完检验题 |
| 要讲给别人听 | 重点看每节的「替代方案」段——那里才有能被人追问的东西 |

**记住这一句，它是全项目的骨架**：

> **配置是意图，内核事实才是证据。**
> 容器引擎会接受并原样回读它根本无法兑现的安全设置。只有从**运行中的沙箱内部**读到的 `/proc/self/*` 才算证据。

## 全篇组织

| 篇章 | 标题 | 解决什么问题 |
|---|---|---|
| §1 | 项目全景：它是什么、为什么存在、长什么样 | 建立地图，知道零件在哪 |
| §2 | Go 语言基础：从这个项目反推你需要的那部分 | 读得懂代码 |
| §3 | Go 并发与 context | 读得懂后台循环与停机 |
| §4 | Go 标准库实战：JSON、HTTP 服务端与测试 | 读得懂接口层与测试 |
| §5 | Linux 隔离基础：威胁模型、namespace 与 cgroup | 隔离的底层机制 |
| §6 | Linux 加固与逃逸面：能力、seccomp、LSM 与 user namespace | 权限与逃逸面 |
| §7 | 文件系统、镜像与容器运行时 | 容器运行时与镜像 |
| §8 | 沙箱生命周期：暂停、恢复、快照、模板与预热池 | 生命周期的产品语义 |
| §9 | 持久化：预写日志、崩溃原子性与备份 | 数据不丢 |
| §10 | 崩溃恢复与一致性：意图先行、对账与乐观锁 | 状态最终正确 |
| §11 | 多租户治理：认证、限流、配额、审计 | 多租户怎么管 |
| §12 | HTTP API 与契约设计 | 对外契约 |
| §13 | 可观测性与运维 | 上线之后怎么活 |
| §14 | 测试方法论 | 怎么证明它真的成立 |
| §15 | Agent 集成与平台对接 | 与 Agent 怎么接 |
| §16 | 工程决策复盘与延伸 | 决策复盘与边界 |
| §17 | 附录：速查卡与阅读路径 | 快速检索与自测 |

本文档只写这个项目**真实用到**的技术；每个技术点都标了 `文件:行`，可以照着去读源码。
数字只来自仓库文档、代码、测试日志或本机实测；找不到出处的地方会明说"文档未给出"，不编。

---


# §1 项目全景：它是什么、为什么存在、长什么样

## 1.1 一句话定位：它是什么，卖给谁

### 问题

把一个项目讲清楚，最难的不是列出它有什么，而是说清它不接什么。一份只写能力的介绍会让每个听众都觉得"这能解决我的问题"，其中一部分人要在上线后才发现自己的问题属于被放弃的那一类。**Sandcastle（二进制名 `sandboxd`）是一个单机、自托管、容器档隔离、多租户的 Agent 代码执行底座**，这四个限定词就是用来提前劝退那些人的。

### 原理

限定词比形容词有信息量，因为它可证伪。"面向企业的沙箱平台"没有反例——任何场景都能被说成企业场景；"单机"有明确反例：需要跨节点调度的场景。**只有能被具体场景反驳的句子才具备约束力。**

### 本项目怎么做

这句话里四个限定词都不是装饰，每一个都在**排除一类场景**。先把它们摆清楚，后面所有设计才有着落：

| 限定词 | 排除掉的场景 | 如果你属于被排除的那类，本项目不适合你 |
|---|---|---|
| 单机 | 需要跨节点调度、异地多活 | 要弹性伸缩到几千并发 → 去找 E2B 这类云服务 |
| 自托管 | 想直接用云 API、不想运维 | 不想装 Docker、不想管数据目录 → 同上 |
| 容器档 | 直接执行不可信第三方代码 | 做在线判题、SaaS 代码解释器 → 你需要 microVM 档 |
| 多租户 | 单用户本地开发工具 | 只是自己一个人跑 → 裸 `docker run` 更省事 |

这个"四限定"的写法来自 `docs/POSITIONING.md:15-19`（一句话版本在 `:11`）。同一份文档在 `:22` 翻译成对外的价值陈述：隔离档位的真实边界要放在能被程序读到的地方，而不是放在 README 的形容词里。

那么它到底给谁用？`docs/DESIGN.md:11` 给出的场景是"企业内部 Agent 平台"：编码 Agent、办公 Agent、数据分析 Agent，需要给每个任务或每个用户分配一个一次性的隔离执行环境，而平台方需要按租户计量、限额、审计、隔离，并且必须能私有化部署（数据不出域）。`docs/POSITIONING.md:31-45` 把它翻成两张清单：该买的（企业内 AI 编程助手、数据主权受限、多业务线共享平台）与不该买的（执行外部用户提交的代码、需要 GPU、需要弹性到数千并发、需要官方 SDK、需要按量出账）。

这里有一个关键判断：**为什么企业要自己搭，而不是买个云沙箱**。答案在 `docs/POSITIONING.md:33-34`——数据主权与合规受限时，代码与产物不能出机房，而同时又不值得为几十个开发者维护一套 Kubernetes。这正是本项目存在的缝隙：**比裸容器多出治理能力，比 Kubernetes 少掉运维复杂度**。

### 替代方案

形容词式定位（"面向企业的安全沙箱平台"）不排除任何场景，于是销售阶段会承诺实现阶段做不到的事。能力清单（"我们支持快照、预热池、配额……"）会过期，且回答不了"清单外的需求怎么办"。把实现手段当定位写同样不稳：手段会换，理由不会——手段应写成定位的**理由**。

### 怎么验证

```bash
cd castle && go run ./cmd/sandboxd -version
# sandboxd 1.1.5
```

版本号来自 `cmd/sandboxd/main.go:34`。拿到它之后，对着四个限定词各想一个"同时满足该买与不该买"的场景；想得出来，说明定位有漏洞。

## 1.2 为什么不是 `docker run`：问题域的六个缺口

### 问题

三个人共用一台 8 核机器，各自写了一句 `docker run --rm -d alpine:3.22 sleep 3600`。两周之后这台机器的状态是什么？看引擎的答案：

```bash
docker run --rm -d --name sbx-demo alpine:3.22 sleep 600
docker inspect sbx-demo --format 'id={{.Id}}  labels={{.Config.Labels}}  net={{.HostConfig.NetworkMode}}'
# id=080ce8fb2a879ae447a73aa23e81aea4e043a2ba3ced0e4b738ccdff67d4bba4  labels=map[]  net=bridge
docker rm -f sbx-demo
```

`labels=map[]` 说明系统不记录这次执行归谁，`net=bridge` 说明它默认带着网络出去，而 `docker ps` 那一行既不写什么时候该回收，也不留下"谁在什么时候对它做过什么"。两周后你面对的是几十个不知道属于谁、不知道该不该删、删了也无法交代的容器。

### 原理

先拆掉一个误解：**本项目的价值不在隔离**。隔离是内核给的——namespace、cgroup、capabilities 都是 Linux 自己的东西，Docker 只是把它们打包成命令行。缺的是围绕一批隔离实例的生命周期治理，可以拆成四件具体的事：**归属**（这次执行属于哪个主体，凭据怎么验、怎么吊销）、**计量**（此刻占了多少并发、CPU、内存）、**拒绝**（下一个请求该不该放行，且"稍后重试"与"买更多容量"必须能区分）、**追溯**（事后能否重建谁对哪个对象做了什么）。

四件事的共同点是：它们都是**关于隔离实例的元数据**，不是隔离本身。内核与 `docker run` 都不提供。

### 本项目怎么做

如果你只需要"跑一段不可信代码"，`docker run --rm` 就够了。所以本项目必须回答一个问题：**裸容器到底缺什么？** 逐个看：

**缺口一：没有租户概念。** 裸 Docker 里所有容器是平的。你无法回答"这是谁的容器""他用了多少配额""超了要怎么拒"。本项目引入 Tenant 实体与 API Key，并把速率桶按租户×操作组切分（算法见 §11）。

**缺口二：没有生命周期治理。** 容器跑起来之后没人管它。忘了删就永久占着内存和 CPU。本项目给每个沙箱加 TTL 与空闲超时，由后台清扫器回收（机制见 §8）。

**缺口三：进程一挂状态全丢。** 这是最要命的一条。你的控制面进程持有"我创建了哪些沙箱"的内存状态；进程被 `kill -9` 之后，内存没了，但**磁盘上那些容器还在跑**。重启后面对的是几十个不知道属于谁的运行中容器。本项目的答案是意图先行 + 预写日志 + 收敛器（§9、§10）。

**缺口四：没有审计。** "谁在什么时候对哪个沙箱做了什么"没有任何记录。出了事无法回溯。本项目把每一次状态变更写成一条不可变的审计事件（§11）。

**缺口五：环境无法秒级复用。** 装好依赖的环境（比如带 Python 数据栈的镜像）每次都要重新起容器、重新装包。本项目用快照与预热池解决：快照把已装好依赖的文件系统固化成镜像，预热池事先起好容器等着被领走（§8）。

**缺口六："隔离"只是声称。** 你在 `docker run` 里写了 `--cap-drop ALL`、`--security-opt no-new-privileges`，然后你**怎么知道它生效了**？如果宿主机的 `daemon.json` 覆盖了某项，或者这个字段在引擎内部被接受但没落实到内核，你的配置读出来是"正确"的，而沙箱实际是敞开的。本项目为此做了两件在同规模项目里很少见的事：一是**启动时真的起一个沙箱、在里面读内核事实**并逐项核对；二是把"配置不是证据"作为贯穿全项目的方法论（§6）。

把六个缺口合起来看，本项目的定位就清楚了：**它不是"更好的 Docker"，它是"给一大堆容器加上租户、配额、审计、自愈和可核对的安全承诺"的那一层**。

**一个反例：不治理会怎样。** `internal/httpapi/server.go:110` 的注释记着项目自己踩过的一个真坑（详细复盘见 §12）：

```go
// Order is load-bearing, and v1.1.0 got it wrong:
//	recover → request-id → pre-auth limit → body-limit → authenticate
//	        → per-tenant limit → handler
```

v1.1.0 把限流器放在了认证**内部**，理由是"它需要已认证的租户"。后果是：**一个认证失败的请求永远到不了限流器，却仍然花费一次审计写入（也就是一次 fsync）**。400 个未认证请求 → 400 次 fsync、0 个 429。注释里给这个现象起了个准确的名字：**一个不需要任何凭据的 I/O 放大器**。

这个反例说明什么？说明"治理"不是加几个中间件就完了——**中间件的相对位置本身就是设计**。而这类错误只有在有人真的去想"未认证流量走了哪条路径"时才会被发现。

### 替代方案

最直觉的做法是在 `docker run` 外面包一层脚本，起容器时打标签、跑 cron 清过期容器。它能补缺口二，补不了缺口三：脚本没有持久化意图，重启后它内存里的"我建了 3 个"和引擎里实际存在的 5 个开始漂移，且没有机制判定谁对。搬进 Kubernetes 是第二种：命名空间、ResourceQuota、控制器自愈都有，代价是客户要先把"租户"映射成一整套 K8s 对象并长期维护它（§16.5）。

### 怎么验证

上面那条 `docker inspect` 就是验证：预期 `labels=map[]`（没有归属）与 `net=bridge`（默认带网）。这四件事在本项目里分别由 `gov`、`store`、`httpapi` 接管。

## 1.3 架构分层：控制面与数据面为什么要分开

### 问题

为什么不能把所有逻辑写在一个大 `main` 里？考虑一个具体场景：要加一个"按租户查沙箱列表"的接口。你会很容易写出这样的代码——HTTP handler 里直接去调 Docker API 拿容器列表，然后过滤出属于这个租户的。这段代码能跑，但它埋了三颗雷：

1. **越权风险**：过滤逻辑写在 handler 里，那么下一个新写的 handler 忘了过滤，就是一次 IDOR（越权访问）。
2. **无法替换后端**：Docker API 调用散落在各处，想换 Firecracker 就要改几十个文件。
3. **测试困难**：想测"没有容器运行时时会怎样"，得真的把 Docker 停掉。

### 原理

分层的价值是把这三颗雷逐个拆掉，而拆的手段不是"分目录"，是**限制依赖方向**。依赖方向决定了错误的种类：如果 `runtime` 可以 import `service`，后端实现就能绕过配额改状态，这类错误只在运行时暴露；单向依赖把它变成编译错误。**把一类运行时漏洞降级成编译不过，是这个分层最主要的收益。**

### 本项目怎么做

本项目照 E2B 的做法分四层（`docs/DESIGN.md:33-36`），规则是单向依赖：

```text
httpapi   只回答"认证了谁、限没限住、记没记账"，不碰后端
  ↓
service   拥有全部状态机与补偿逻辑，是唯一写状态的地方
  ↓
store     预写日志持久层（接口可换 PG）
  ↓
runtime   Backend 接口：oci(Docker) ▸ firecracker(骨架)
```

**httpapi 不许碰后端。** 这一条直接消灭了越权风险的一半：handler 拿不到后端句柄，它只能调用 `service`，而 `service` 的每个方法签名都要求传租户。忘传租户是**编译错误**，不是运行时漏洞。

**service 是唯一写状态的地方。** 这是"单一写者"原则。为什么重要？因为状态机有合法性约束（1.4 会讲），如果多个层都能改状态，你就无法保证每个改动都经过合法性检查。这条规则在代码里有落点：`internal/service/lifecycle.go:19` 的 `loadOwned` 是每一个按 ID 访问沙箱的动作都要过的关卡，它在返回对象之前先比对 `sb.TenantID != tenantID`。

**runtime 不持久化。** 这条规则解决的是"真相归谁"：`runtime` 层只回答"这台机器上**真实存在**什么"，它不知道也不关心数据库里记了什么。这个分工是崩溃恢复能成立的前提（§10）：重启后，`service` 问 `runtime` "这个 ID 的容器还在吗"，然后以后端的回答为准。

**依赖方向严格单向。** 意味着 `runtime` 不能 import `service`，`store` 不能 import `httpapi`。这带来一个具体收益：**加节点、换后端不动业务逻辑**。`internal/runtime/runtime.go:152` 的 `Backend` 接口就是那个替换缝，它有 24 个方法，覆盖创建、启停、暂停恢复、执行、文件、快照、孤儿枚举（§7 会讲这个接口里两个"故意放进去的"方法）。

### 替代方案

一个大 `main` 的代价上面说过。另一种是把四层拆成四个进程用 RPC 通信：能获得独立伸缩与部署，代价是把进程内一次函数调用换成网络调用，而本项目要解决的崩溃一致性反而更难（跨进程没有共享的 fsync 语义）。在"单机"这个限定词下，进程内分层是净收益。

### 怎么验证

```bash
cd castle
go list -f '{{.ImportPath}} {{join .Imports ","}}' ./cmd/... ./internal/... \
  | sed 's|sandcastle.dev/sandcastle/||g' \
  | while read p imports; do echo "$p : $(echo "$imports" | tr ',' '\n' | grep '^internal/' | tr '\n' ' ')"; done
```

预期看到 `internal/store` 与 `internal/runtime` 都只依赖 `apperr` 和 `domain`，而 `internal/service` 依赖除 `httpapi` 外的所有包。任何一条从下往上的箭头都是分层被破坏的证据。

## 1.4 领域模型：六个实体与一条状态机

### 问题

有了分层，接下来定"系统里到底有哪些东西"。一个偷懒的做法是不要领域模型，直接把引擎返回的容器 JSON 当模型用——字段现成，序列化也现成。它会立刻在三处出问题：容器 JSON 里没有"这个容器属于谁"，没有"它该什么时候被回收"，也没有"它的规格请求是什么"。

### 原理

领域模型的职责是定义**我们向客户承诺的东西**，而不是后端恰好有的东西。判据是：一个字段若只在本层内部有用，它是实现细节；若它决定了对外承诺（配额怎么算、何时回收、能不能被别人访问），就必须进领域模型并可以被序列化——因为进程随时会死，跨重启还需要的字段不能丢。状态机是同一件事的另一面：它定义**合法变化的集合**，而不是当前值。

### 本项目怎么做

`docs/DESIGN.md:40-47` 列出全部领域对象。**注意它们之间的关系，而不只是名称**：

| 实体 | ID 前缀 | 它为什么存在（不只是"是什么"） |
|---|---|---|
| Tenant | `tnt_` | 配额与审计的边界。没有它，"限额"无处挂靠 |
| APIKey | `ak_` | 身份。**只落库 SHA-256 哈希**，因为数据库被读走时明文 Key 就是全部租户的通行证 |
| Sandbox | `sbx_` | 核心对象。注意它**没有 node 字段**——这是单机的诚实体现，加多节点要先改数据模型 |
| Snapshot | `snp_` | 由沙箱 commit 而来，用来"复制一个装好的环境" |
| Template | `tpl_` | 镜像引用 + 默认规格，预热池按 (租户,模板) 维护 |
| AuditEvent | `evt_` | 追加式。租户只能读自己的，admin 读全部 |

ID 前缀（`sbx_`、`tnt_`）不是装饰：它让**一眼看出这是哪类对象**，也让日志里 grep 一个 ID 就能定位类型。前缀常量在 `internal/idgen/idgen.go:16-22`，生成规则见 §2。

`internal/domain/sandbox.go:24` 定义了 8 个状态。**这里最重要的问题不是有哪几个状态，而是为什么其中四个是"过渡态"**：

```go
StateProvisioning SandboxState = "provisioning"
StateRunning      SandboxState = "running"
StatePausing      SandboxState = "pausing"
StatePaused       SandboxState = "paused"
StateResuming     SandboxState = "resuming"
StateStopping     SandboxState = "stopping"
StateStopped      SandboxState = "stopped"
StateFailed       SandboxState = "failed"
```

代码注释（`internal/domain/sandbox.go:20-23`）把设计意图写明了：过渡态的存在，是为了让**"守护进程在操作中途崩溃"与"沙箱健康"这两种情况可以被区分**。重启时，凡是处于过渡态的沙箱，一律交给收敛器，由它向后端重新推导真相。判据函数 `Transitional()` 在 `internal/domain/sandbox.go:55`。

如果没有过渡态会怎样？假设只有"创建中"和"运行中"两个状态。进程在"已经向 Docker 发了创建请求，但还没收到响应"时被 `kill -9`。重启后你看到的是什么？要么是"创建中"——但你不知道 Docker 那边到底建没建成；要么你乐观地标记成"运行中"——但容器可能根本不存在。**两种选择都在猜**。而有了 `provisioning` 这个明确的过渡态，系统的态度是：**不猜，去问后端**（收敛的推导见 §10，状态集合的判据见 §10.5）。

很多项目的状态机写成一把 `if/switch`。本项目把它写成一张**表**（`internal/domain/sandbox.go:37`）：

```go
var transitions = map[SandboxState][]SandboxState{
	StateProvisioning: {StateRunning, StateFailed, StateStopping},
	StateRunning:      {StatePausing, StateStopping},
	StatePausing:      {StatePaused, StateRunning, StateStopping},
	StatePaused:       {StateResuming, StateStopping},
	StateResuming:     {StateRunning, StatePaused, StateStopping},
	StateStopping:     {StateStopped, StateFailed},
	StateStopped:      {}, // terminal
	StateFailed:       {StateStopping},
}
```

这张表值得逐行读，因为每一行都在表达一个判断：

- `StatePausing → {StatePaused, StateRunning, StateStopping}`：暂停**失败**了可以退回运行态，这很现实；同时运行中也可能直接走向停止。**如果只写 `→ StatePaused`，一次暂停失败就会把沙箱卡死在 `pausing`**。
- `StateResuming → {StateRunning, StatePaused, StateStopping}`：恢复失败退回已暂停，而不是丢弃状态。
- `StateStopped: {}` 是终态，注释明写了 `// terminal`。
- `StateFailed → {StateStopping}`：失败的沙箱只剩"走向停止"一条路，避免失败实例被无限留在系统里。

而校验函数只有五行（`internal/domain/sandbox.go:69`），遍历这张表判断目标状态是否在白名单里。**把规则做成数据的好处**：新增一个状态时，你被迫填写它能从哪来、能到哪去，而不是在十几个 `if` 里漏掉一个分支。

非法转换的返回值也值得注意（`internal/domain/sandbox.go:80`）：返回 `apperr.ConflictErr`（映射为 HTTP 409），错误消息里**带上当前状态**——因为客户端看到 409 时第一个问题必然是"那它现在是什么状态"。

最后一条乐观锁（`docs/DESIGN.md:62`）：领域对象带一个 `BaseVersion`（读取时的基线），存储层比对的是 `BaseVersion` 而不是 `Version+1`。差别在哪？用 `Version+1` 的写法要求"内存改一次就必须立刻落盘一次"。用 `BaseVersion` 就允许"**内存里改三次、磁盘上写一次**"，同时仍能精确检出并发写：如果期间别人改过，`BaseVersion` 就对不上，返回 409。存储层的实现见 §10。

### 替代方案

`if/switch` 最常见，它的失效方式很具体：8 个状态两两组合 64 条边，用分支写出来之后，**"忘了写一条边"和"判定这条边非法"在代码里长得一模一样**——都是走到 `default` 返回 false。表驱动让"没写"变成"表里没有这一项"，可枚举、可被测试穷举。

### 怎么验证

```bash
cd castle && go test ./internal/domain/ -run 'TestStateMachine' -v
```

`TestStateMachineLegalTransitions`（`internal/domain/domain_test.go:19`）遍历转移表断言每条边都通过，`TestStateMachineIllegalTransitions`（`:42`）钉住复活与跳过过渡态这两类非法边。手工验证：往表里加一行，看这两个测试里是否恰好有一个开始失败。

## 1.5 一次创建的完整旅程

### 问题

前面讲的都是零件，各自只回答"某一个切面长什么样"。看代码时最容易犯的错是逐包读完却拼不出一条请求路径——因为你不知道谁先调用谁。

### 原理

一条创建请求值得单独走一遍，因为它是唯一一个**同时穿过全部四层**的动作：过认证与限流、落盘意图、判定配额与规格、真正起容器，还要在返回前等状态收敛。

### 本项目怎么做

这条路径来自 `README.md:111-119`，它同时是一份"读代码的路线图"：

```text
CLI ──▶ 中间件链（认证→限流）──▶ 治理准入（模板解析/规格校验/egress 检查/配额）
    ──▶ 意图落盘（provisioning，WAL+fsync）          ← 从此任何时刻断电都可判定
    ──▶ 预热池命中？──是──▶ 直接采用（毫秒级，状态=running）
                    └─否──▶ 异步供给：OCI Create（net=none · CapDrop ALL ·
                            no-new-privileges · 禁 swap 内存硬限）→ Start →
                            轮询确认真实相位 → 状态机收敛 running
    ──▶ waitForState 轮询 store ──▶ 201 返回
此后：TTL/空闲清扫器回收僵尸 · reconciler 周期对账 · 每步动作入审计流
```

逐步解释**为什么是这个顺序**：

1. **中间件链先跑**，因为认证失败就不该占用任何稀缺资源（这正是 1.2 结尾那个 I/O 放大器要修的问题）。
2. **治理准入在落盘之前**。配额不够、规格非法、egress 模式无法强制——这些都在**写盘之前**拦掉（判定集中在 `internal/service/service.go:96-120`，全部排在 `PutSandbox` 之前）。为什么强调这点？因为一旦落了盘，预写日志里就有了一条记录，你还得再写一条"失败"记录去抵消它，等于用两次写入换一次拒绝。**能在准入处拒的，绝不留到状态机里拒。**
3. **意图先落盘（provisioning + fsync）**，然后才碰后端（`internal/service/service.go:147`）。这一条是整个可靠性设计的地基：**从 fsync 返回的那一刻起，这次创建在任何时刻断电都是"可判定"的**——要么日志里有一条 `provisioning` 记录等着被对账，要么什么都没有。绝不会出现"容器建好了但系统不知道"。
4. **预热池命中就直接用**（`internal/service/service.go:137`），跳过创建。这是把"秒级创建"变成"毫秒交付"的手段。
5. **未命中则异步供给**（`internal/service/service.go:151`），且**轮询确认真实相位**——不是发完请求就算成功，而是回头去问后端"它真的在跑吗"。
6. **`waitForState` 轮询 store**，把"等待"这件事也建在持久层的状态上，而不是建在一个内存里的 channel 上。

这条路径里藏着本项目的一个核心态度：**每一步都在问"如果我现在断电，重启后会怎样"**。

### 替代方案

把供给做成同步（在请求 goroutine 里一路建完再返回）最简单，代价是 HTTP 请求超时与容器创建时长绑死，而容器创建正是整条链路里最慢的一步。拆成"落盘 + 异步供给 + 轮询等待"之后，慢的那一步不再持有请求上下文。

### 怎么验证

```bash
cd castle && go test ./tests/ -run 'TestSandboxLifecycle|TestCrashRecovery' -count=1 -v
```

`TestSandboxLifecycle` 覆盖创建到停止的合法边，`TestCrashRecovery` 覆盖"落盘之后、供给完成之前进程被杀"那一段。

## 1.6 代码地图与阅读顺序

### 问题

一个 18,872 行的 Go 仓库，按目录名随机翻开，读三天也形不成结构感。需要的是"先看哪个、再看哪个"的顺序，而顺序不能随意排。

### 原理

阅读顺序应该与依赖方向同向：先读不依赖别人的包，再读依赖它的包。这样每读一个新包，它引用的东西你都见过，认知负担是累加而不是递归的。反过来先读 `cmd/sandboxd`，第 23-31 行的 import 块里就是九个没见过的包（`internal/config` 到 `internal/store`）。

### 本项目怎么做

`internal/` 下的包按依赖方向从外到内排列。**建议的阅读顺序就是这张表从上到下**，因为每一层只依赖它下面的层：

| 路径 | 一句话职责 | 从哪里开始读 |
|---|---|---|
| `cmd/sandboxd/` | 服务入口。**启动顺序本身就是设计**（§13） | `main.go:56` 的 `run()` |
| `cmd/sbxctl/` | 运维 CLI，把 HTTP API 包成命令行 | `main.go` |
| `internal/apperr/` | 稳定错误模型：`Code` + `Retryable` | `apperr.go:15`（`Code`）、`apperr.go:61`（`Retryable`） |
| `internal/config/` | key=value 配置 + `SBXD_*` 环境变量；坏值拒绝启动 | `config.go` |
| `internal/domain/` | 领域模型 + 状态机 | `sandbox.go:24`（状态）、`sandbox.go:37`（合法边） |
| `internal/gov/` | 治理：令牌桶、配额准入、审计、Key 哈希 | `gov.go`、`quota.go`、`audit.go` |
| `internal/httpapi/` | 路由、中间件链、请求/响应契约 | `server.go:54`（路由）、`server.go:108`（链的顺序） |
| `internal/idgen/` | ID 生成 | `idgen.go` |
| `internal/metrics/` | 指标注册表 | `metrics.go` |
| `internal/runtime/` | **后端接口定义（替换缝）** | `runtime.go:152`（`Backend`） |
| `internal/runtime/oci/` | 容器后端：Docker API 客户端、创建载荷、隔离自检、文件传输 | `client.go`、`backend.go`、`isolation.go` |
| `internal/runtime/fc/` | Firecracker 骨架：每个操作都返回"未实现" | `fc.go` |
| `internal/service/` | 业务编排：创建、生命周期、快照、池、收敛器、后台循环 | `service.go`、`lifecycle.go`、`reconciler.go` |
| `internal/store/` | 内存索引 + 预写日志 + 崩溃修复 + 备份 | `store.go`、`wal.go` |
| `tests/` | 端到端套件（Go 测试 + harness） | `harness_test.go` |
| `poc/` | 可复现的攻击/缺陷验证程序 | `walrace`、`tornframe`、`lockleak` |

每个包的**包注释**是这份地图里性价比最高的部分，它们写的不是"这个包有什么"，而是"这个包踩过什么坑"——例如 `internal/store/store.go:20-36` 直接列出五条让存储层崩溃正确的不变式，并注明每条由哪个测试守着。

`internal/runtime/runtime.go:152` 那个 `Backend` 接口值得单独看一眼，因为它是全项目最重要的一个文件：24 个方法定义了"一个后端必须能做什么"，其中 `IsolationTier()`（第 163 行）和 `VerifyIsolation()`（第 183 行）是**故意放进去的**——设计文档（§7 会详述）说得很直接：**新增后端"必须"声明自己的隔离档位，这条信息不可能再被漏掉**。

### 替代方案

按字母顺序读（或按包大小读）看起来更公平，但它把 `domain` 里的 `SandboxState` 放在读者还不知道沙箱为何物的时候。

### 怎么验证

```bash
cd castle
for d in $(find . -name '*.go' -not -path './bin/*' -exec dirname {} \; | sort -u); do
  echo "$(find "$d" -maxdepth 1 -name '*.go' | xargs wc -l | tail -1 | awk '{print $1}') $d"
done | sort -rn | head -8
```

实测前几名是 `tests` 4,152、`internal/runtime/oci` 2,851、`internal/service` 2,805、`internal/store` 2,390、`internal/httpapi` 1,726。这个分布本身就是信息：引擎适配层与业务编排层体量相当，存储层排第三，说明"持久化"在这类系统里不是配角。

## 1.7 规模与实测数字总表

### 问题

技术评审时，报不出数字的项目和只会报数字的项目同样危险：前者无法被质疑，后者经不起第一句追问。

### 原理

一张数字表的价值取决于每个数字能不能追溯到一次具体运行。凡是靠"大约""应该在……量级"写出来的数字，都不该出现在给客户看的文档里——第一次复现失败时，损失的不止这一个数字。本项目的做法是把数字与推导分开：本章只做索引，推导归各自的归属章节。

### 本项目怎么做

这张表是**索引**，不是论据。每个数字的来历、为什么这么设计、能被怎么质疑，都在它所属的章节里展开。

| 项 | 数值 | 出处 / 展开章节 |
|---|---|---|
| Go 模块 | `sandcastle.dev/sandcastle`，`go 1.26` | `go.mod:1,3` |
| 第三方依赖 | **0 个**（`go.mod` 无 `require` 行） | §16 讲代价与收益 |
| Go 总行数 | 18,872（非测试 10,269 / 测试 8,603，比值 0.84） | §14.9 |
| 最大的三个包 | `tests` 4,152 ｜ `internal/runtime/oci` 2,851 ｜ `internal/service` 2,805 | §14.9 |
| 端到端套件 | 34 个顶层用例、83 个子场景，全量 228.5 秒 | §14.9 |
| 沙箱状态数 | 8（其中 4 个是过渡态） | §1.4、§10.5 |
| 后端接口方法数 | 24 | `internal/runtime/runtime.go:152` |
| 隔离契约断言 | 22 项，全部从**运行中沙箱内部**读内核事实 | §6、§14 |
| 遏制场景 | 5 项，各带正向对照 | §5、§14 |
| 冷启动到 `/readyz` 200 | 727 ms | §14 |
| ready 后首个建沙箱请求 | 188 ms | §14 |
| 带 9 个存活沙箱重启到 ready | 166 ms，9 个沙箱全部仍 `running` | §10、§14 |
| 80 并发创建（限速 10/s burst 10） | 201×10 / 429×70，记录数与容器数零幽灵 | §11、§14 |
| 12 秒负载 | 2,968 次请求全 200，`/readyz` 采样 47/47 | §14 |
| 空闲预算 60 s | 实测 1 分 6 秒回收，且配额真的被归还 | §8、§11 |
| `/healthz` 地板延迟 | p50 73 µs | §13 |
| exec p50 | 26.885 ms（其中控制面自身约 10 µs） | §13 |

**怎么用这张表**：面试或技术评审时，被问到的任何一项，你都要能立刻回答三件事——这个数字是怎么测出来的、它为什么是这个量级、它在什么条件下会变差。只会报数字而不懂这三件事，比不报数字更危险。

### 替代方案

把每个数字连同推导一起写在本章，读者不用跳转。代价是同一个数字在两处出现后，其中一处迟早过期而另一处不会——所以宁可只放索引。

### 怎么验证

```bash
cd castle
find . -name '*.go' -not -path './bin/*' | xargs wc -l | tail -1                          # 18872
find . -name '*.go' -not -name '*_test.go' -not -path './bin/*' | xargs wc -l | tail -1   # 10269
find . -name '*_test.go' -not -path './bin/*' | xargs wc -l | tail -1                     # 8603
grep -n '228.5' CHANGELOG.md
```

最后一条应命中 `CHANGELOG.md:92`。行数三项应自洽：10,269 + 8,603 = 18,872。任何一处对不上，先怀疑自己的统计口径（是否排除了 `bin/` 与 `integrations/`），再怀疑文档。

## 1.8 控制面与数据面：一个二进制里怎么把两者分开

### 问题

E2B 那套控制面 / 数据面分离，在它的原生语境里是两台机器：控制面决定"在哪建、记录建了没"，每节点的 orchestrator 拥有"怎么建"。本项目只有一个进程、一台机器，这套划分是不是退化成了比喻？

### 原理

不是比喻，因为两者的区别不在物理位置，而在**权力**：控制面掌握决策与记录（能不能建、建在哪、建了有没有记下来），数据面掌握执行与真相（用什么载荷起、起没起成、此刻真实相位是什么）。关键是：数据面的回答是**真相**，控制面的记录只是**假设**。这个不对称是崩溃恢复的全部前提（§10）——两边不一致时，改记录去匹配后端，而不是反过来。

### 本项目怎么做

**第一，API 层只做决策，不做执行。** `internal/httpapi/server.go:54` 的 `routes` 里每个端点先过 `s.chain(...)`，链上完成认证、防洪、限流；handler 唯一能碰的业务入口是 `s.Svc`。整个 `internal/httpapi` 包共有 47 处 `s.Svc.` 调用，没有一处直接构造后端请求。

**第二，执行权收敛在 service，真相权收敛在 runtime。** `internal/service/service.go:1-5` 的包注释说得很直接：它是编排层，也是**唯一**被允许改变领域状态的组件。

**第三，单向依赖让分工可被机器检查。** 1.3 那条 `go list` 就是检查手段。但这里必须诚实清点例外：分层靠约定与评审维持，不是靠 Go 的可见性强制——`Service` 结构体的 `St` 字段是大写的（`internal/service/service.go:21`），拿到 `svc` 的包都能绕过 service 读写存储层。实际越层访问共五处：

- **引用 `runtime` 的类型**：`internal/httpapi` 确实 import 了 `internal/runtime`，但全包只有三处引用且都是类型——`runtime.TierMicroVM`（`internal/httpapi/handlers.go:101`）、`runtime.ExecRequest`（`:333`）、`runtime.FileInfo`（`:423`）。它拿不到 `Backend` 实例，也就无法发起任何后端动作。**引用一个包的类型与持有那个包的句柄是两件事。**
- **直接读 store 四处**：`internal/httpapi/handlers.go:42` 在 `/readyz` 里问 `St.Healthy()`，是有意留的——就绪判定要的是存储层自己的信号，若经 service 包装一次，"存储坏了"和"业务暂时不可用"会被渲染成同一件事；`handlers.go:129`、`:135` 与 `internal/httpapi/server.go:383` 是认证链上按 ID 取 `Tenant`。
- **直接写 store 一处**：`internal/httpapi/server.go:242` 在认证中间件里调用 `St.UpdateKey` 刷新 Key 的 `LastUsedAt`。可以接受它的理由是：这个字段不参与配额、限流、审计中的任何一项判定，冲突也无害——代码里连乐观锁都关掉了，传给 `baseVersion` 的是 `-1`，而 `internal/store/store.go:347` 的判据是 `baseVersion >= 0 && cur.Version != baseVersion`，负数即"不做并发比较"。

把这五处列出来不是给它们开脱，而是让"单一写者"有可核对的边界：**一个声称 service 是唯一写者的项目，应该能数出自己违反过几次。**

`internal/gov` 也不在 service 之下，而是与 service 并列被编排：`internal/service/service.go:41` 的 `New()` 把四个治理件装配进 `Service`，治理因此不是可以绕过的一层。

### 替代方案

一个明显的替代是把 `Backend` 实例直接挂到 `Server` 上，让 handler 在读路径上直接 `Inspect`。它少一次函数调用，但把"谁有权问后端"从一个包扩散到所有包——1.3 里那颗 IDOR 的雷就是这么埋下的。另一个是把控制面拆成独立进程（守护进程 + 每节点 agent），好处是加第二台机器不用改架构，代价 1.3 已算过：跨进程没有共享的 fsync 语义。

### 怎么验证

```bash
cd castle
grep -o 's\.Svc\.' internal/httpapi/handlers.go internal/httpapi/admin.go internal/httpapi/server.go | wc -l
grep -n 'runtime\.' internal/httpapi/handlers.go
grep -n 'Svc\.St\.' internal/httpapi/handlers.go internal/httpapi/admin.go internal/httpapi/server.go
```

第一条给出 httpapi 走 service 的调用点总数（实测 47）；第二条预期恰好三行且都是类型名；第三条预期恰好五行，其中只有 `internal/httpapi/server.go:242` 带 `UpdateKey` 是写操作。

## 1.9 仓库全景与三条阅读路线

### 问题

1.6 的地图只覆盖 `internal/`，仓库里还有 `cmd/`、`tests/`、`poc/`、`integrations/`、`deploy/`、`docs/`。它们的共同点是**都不属于运行时，却决定了你怎么验证它、部署它、向别人解释它**。

### 原理

要区分两类产物：**产品代码**（会被编译进 `bin/sandboxd`）与**证据与通道**（测试、复现程序、集成适配器、部署单元、文档）。区别在于失败方式——产品代码错了会在运行时出错，证据错了会在你以为安全的时候出错。

### 本项目怎么做

| 路径 | 负责什么 | 一句话说明 |
|---|---|---|
| `cmd/` | 两个可执行入口 | `sandboxd` 是守护进程，`sbxctl` 是运维 CLI（`cmd/sbxctl/main.go:1-4` 自称"a thin HTTP client"） |
| `tests/` | 端到端套件 + 企业场景演练 | Go 断言在 `*_test.go`，演练在 `e2e-enterprise-scenario.sh`，图由 `render-scenario-flow.py` 从 trace 渲染 |
| `poc/` | 缺陷复现程序 | 7 个 PoC，只读代码或只走 REST，不改产品逻辑（`poc/README.md:4`） |
| `integrations/` | 外部 Agent 的适配层 | `pi-sbx` 把 pi 的 bash 工具经 REST 路由进沙箱（§15） |
| `deploy/` | 部署单元 | `sandboxd.service`，含 `After=docker.service` 与 `StateDirectory=sandboxd` |
| `docs/` | 设计、定位、威胁模型、API、部署 | 其中 `scenario-flow.html` 是由演练生成的派生件 |
| `Makefile` | 所有可复跑命令的入口 | `build` / `test` / `test-e2e` / `test-commercial` / `scenario-flow` |

三条阅读路线，按目的选一条：

- **路线 A（要改代码）**：`cmd/sandboxd/main.go:56` 的 `run()` → `internal/service/service.go:96` 的 `CreateSandbox` → `internal/service/reconciler.go:125` 的 `reconcile`。
- **路线 B（要评审安全性）**：`docs/THREAT-MODEL.md` → `internal/runtime/runtime.go:152` 的 `Backend` → `internal/runtime/oci/isolation.go` → `tests/isolation_test.go`。
- **路线 C（要判断能不能买）**：`docs/POSITIONING.md` → `docs/SECURITY-CONTRACT.md` → `make scenario-flow` 跑出的图。这条不需要读 Go 代码。

### 替代方案

最常见的组织方式是把测试、脚本、部署、文档摊在仓库根目录。代价是新来的人靠文件名猜，而 `tests/` 这个目录名本身就携带一条信息：这里的东西是断言，不是可选项。`poc/` 单独成目录同理——它告诉读者"这些程序是给审查者复现缺陷用的，不是产品的一部分"（`poc/README.md:3`）。

### 怎么验证

```bash
cd castle
make build          # bin/sandboxd + bin/sbxctl
make test           # go test -race -count=1 ./internal/...
ls tests poc deploy integrations/pi
```

`Makefile:8-10` 的 `build` 只编译 `cmd/` 下两个入口；`Makefile:18-19` 的 `test` 只对 `internal/...` 跑竞态检测。端到端套件与 PoC 都不在默认构建与默认测试之内，这是有意的：它们需要真实的容器运行时（准入条件见 §14.9）。

---

### 本章检验

1. 客户说"我要执行外部用户提交的代码"，而文档第一句写着"容器档"。请不用"不行"两个字，而是从四个限定词里挑出相关的那个，说明它被排除的机制原因（提示：内核、身份、逃逸收益），并给出你建议的隔离档位。
2. 转移表里 `StatePausing` 有两个正常出口。请构造一个真实故障（写出哪一步的哪个调用失败、返回什么），说明去掉 `StateRunning` 后沙箱会停在什么状态、随后造成什么后果（提示：`CountableResources` 在 `internal/domain/sandbox.go:279` 怎么算）。
3. 1.8 承认 httpapi 确实 import 了 `internal/runtime`，也承认了五处越过 service 访问 store。为什么"引用一个包的类型"不算破坏分层，而"持有那个包的实例"就算？`internal/httpapi/server.go:242` 那次越层写凭什么可以接受？
4. 1.5 的创建路径把"治理准入"放在"意图落盘"之前。请把顺序倒过来，推演一次超配额的创建：日志里多出几条记录、配额账目在哪个时刻是错的、客户端拿到什么错误码。为什么"多写一条失败记录去抵消"不是可接受的修法？
5. 1.7 里测试代码 8,603 行、实现代码 10,269 行。这个比值不能证明任何质量结论——请说出两种能让比值好看但测试毫无用处的写法，并说明本项目用哪条规则挡住了它们（提示：见 §14.9 关于"静默跳过"的准入条件）。
6. **动手任务**：按 1.9 的路线 A 走一遍。在 `cmd/sandboxd/main.go:56` 的 `run()` 里把每一行按"它依赖什么、它准备什么"标注出来，然后回答：`isolationSelfCheck`（第 98 行）为什么排在 `svc.StartLoops()`（第 102 行）和构造 HTTP 服务端（第 108 行起）之前？把这三处顺序任意交换，会有什么东西在还没被证明之前就被放进生产路径？把答案写成 300 字，读 §13 时回来核对。


# §2 Go 语言基础：从这个项目反推你需要的那部分

## 2.1 三条设计哲学：显式、组合、错误即值

**问题。** 读 Go 代码的人常问：为什么每处调用后面都跟着 `if err != nil`？为什么全项目没有一个 `class`，也没有继承？为什么 `Retryable` 要单独占一个结构体字段，而不是从 HTTP 状态码推出来？这三件事背后是三条设计取向，是本章后面所有细节的地基。

**原理。** 第一条，**显式优于隐式**：语言和库都不替使用者做推测。Go 没有异常（异常的传播路径在栈上隐式，读代码看不见控制流），没有隐式类型转换，不自动调用构造；代价是代码更长，收益是任意一行的后果都落在看得见的范围里。第二条，**组合优于继承**：类型之间只有组合和接口两种关系，没有继承链，就没有"改基类导致子类行为意外变化"这一整类问题。第三条，**错误即值**：错误是普通返回值，本身是接口值，可被传递、包装、比较、放进结构体；没有 `throw`，没有 `finally`。

**本项目怎么做。** 关于显式：`internal/apperr/apperr.go:58` 的 `type E struct` 把"能不能重试"写成独立布尔字段 `Retryable`（为什么必须这样见 §2.8）；`internal/domain/sandbox.go:37` 的 `transitions` 把状态机写成一张显式的表（状态机本身见 §1.4）。关于组合：`internal/service/service.go:20` 的 `Service` 把 `*store.Store`、`runtime.Backend`、`*gov.Authenticator`、`*gov.Quotas` 当字段装进来（`:21`–`:28`）；`internal/httpapi/server.go:188` 把 `*statusWriter` 包在真正的 `ResponseWriter` 外层，而不是写一个子类。关于错误即值：`internal/runtime/runtime.go:150` 的注释把契约写死——`Backend` 的所有方法都必须返回 `*apperr.E`；`cmd/sandboxd/main.go:50` 的 `if err := run(*configPath); err != nil` 把错误的产生与处置放在同一屏内。

**替代方案。** 另一条路是"异常 + 继承"：想加一层缓冲，Java 得把流套进 `BufferedInputStream`，因为行为挂在继承树上；Go 的 `io.Reader` 接口谁都能实现，不需要公共祖先。异常方案真正的问题是**不可见**——签名里看不出会不会抛，编译器也不强制处理；Go 把它搬到 `(T, error)` 签名上，编译期就强迫调用方直面它。

**怎么验证。** 跑 `go build ./...`，实测返回 0。再做反证：删掉一处 `if err != nil` 检查、让一个局部变量悬空，编译会报 `declared and not used`。"显式"是编译期能抓住的机制，不是风格建议。

## 2.2 go.mod 只有三行：零第三方依赖是什么样子

**问题。** 项目最容易失控的地方之一是依赖。你今天引一个 JSON 库，它明天带进来十二个间接依赖，其中一个是两个月没更新的；做供应链审计时你甚至说不清最终锁死的是哪几份代码。"零第三方依赖"要么能在 `go.mod` 里被看见，要么就只是一句营销词。

**原理。** `go.mod` 声明三件事：模块路径、语言版本、依赖清单（`require` 块）。前两行回答"这个模块是谁""按哪个语言版本编译"，`require` 块回答"要拉谁"。**没有 `require` 块就等于没有任何模块依赖，编译只需要标准库。**

**本项目怎么做。** 全文就在 `castle/go.mod`，去掉结尾空行后一共三行：

```text
module sandcastle.dev/sandcastle

go 1.26
```

**第 1 行 `module sandcastle.dev/sandcastle`** 是模块路径：它同时是模块的唯一身份和本模块内所有 import 的前缀，所以你会看到 `sandcastle.dev/sandcastle/internal/domain` 这样的导入（`internal/domain/sandbox.go:16`）。它只是命名空间，**不要求** `sandcastle.dev` 能解析成网页。**第 3 行 `go 1.26`** 是语言版本指令：它告诉编译器本模块的语言语义以 1.26 为准，决定泛型、`min`/`max` 内建函数、循环变量作用域等特性按哪个版本解释（泛型见 §2.7）。本机实测 `go version` 是 `go1.26.5 darwin/arm64`。

**没有 `require` 块**，且仓库根目录**没有 `go.sum`、没有 `vendor/`**——这两样是依赖存在的物证（前者记录依赖哈希，后者存放源码），都不存在说明确实只依赖标准库。我另外扫描了 `cmd/` 与 `internal/` 下所有 import：除本模块自身外全部是标准库（`context`、`crypto/sha256`、`net/http` 之类）。唯一的例外 `"docker.io/library/alpine"`（`internal/runtime/oci/oci_test.go:49`）是 OCI 镜像引用，不是 import。

**这种写法对三件事的后果。** 构建上：只需要标准库，离线可构建，耗时不受上游发版影响，产物是单个二进制。供应链上：没有第三方代码就没有需要审计的传递依赖。分发上：客户可以在气隙环境从源码构建。

**替代方案。** 常见做法是用现成库替掉手写代码（第三方路由库、JSON 库、Docker 官方 Go SDK）；代价是本该由库提供的要自己写——Prometheus 文本格式（见 §13）、key=value 配置解析（`internal/config/config.go:253`）都是手写的。这组取舍的完整复盘见 §16。

**怎么验证。** 跑 `cd castle && go list -m all`，实测只输出一行 `sandcastle.dev/sandcastle`。再跑 `go build ./...`，零依赖前提下它不会因为缺 `go.sum` 而失败。

## 2.3 包组织：cmd/ 与 internal/，以及编译器强制的边界

**问题。** 假设你把错误类型和领域模型放在模块根目录 `apperr/`、`domain/`（也就是公开目录）。半年后有个外部项目 import 了你的 `apperr.E`，然后你把 `Message` 字段改名——对方的构建挂了。你只是改了自己的内部字段，却破坏了别人的编译，而且从此被迫永远保持向后兼容。

**原理。** Go 里目录就是包。两个目录名带特殊含义。`cmd/`：**没有编译器语义**，纯约定，社区惯例表示"这里放 main 包"；`cmd/sandboxd/`（守护进程）与 `cmd/sbxctl/`（运维 CLI）各是一个 `package main`。`internal/`：**有编译器强制语义，这是本章第一个必须记牢的点**。Go 规定任何 import 路径中带 `internal` 段的包，只能被"以 `internal` 的父目录为根的子树"里的代码导入。本项目的 `internal` 在模块根下，父目录就是 `sandcastle.dev/sandcastle`，于是 `internal/...` 只能被本模块自己的代码导入。关键是——**这是编译错误，不是 lint 警告，也不是 review 约定。**

**本项目怎么做。** 所有内部包都写在 `internal/` 下：`internal/apperr`、`internal/domain`、`internal/runtime`、`internal/store`……（清单见 §1.6）。为什么把 `apperr`、`domain` 也放进去？因为它们是**实现内部契约**：对外契约是 HTTP/JSON（见 §12），客户端看到的是 `{"error":{"code":...}}`，而不是 Go 的 `*apperr.E`。把 Go 类型关进 `internal`，等于宣布"这些类型我可以随时重构"。

**替代方案。** 另一种做法是把 `apperr`、`domain` 做成公开目录当 Go SDK 给别人 import。没选它是因为：一旦公开就需要版本语义、弃用政策、兼容性测试这一整套负担；而本项目（见 §1.1）的集成面是 HTTP 与 CLI，不是 Go 库。

**怎么验证。** 这是本章最值得亲手做的一次实验。我在 `/tmp` 建了模块 `probe.example/outside`，用 `replace` 指向本仓库，再 import `sandcastle.dev/sandcastle/internal/domain`，`go build` 的真实输出是：

```text
package probe.example/outside
	main.go:3:8: use of internal package sandcastle.dev/sandcastle/internal/domain not allowed
```

`not allowed` 是编译器说的话。把同一个 import 写进 `castle/` 自己内部的任意包就合法通过——所以 `internal` 是"用目录结构表达的访问控制"。

## 2.4 可见性：首字母大写这一个规则

**问题。** 上一节讲了包这一级怎么用目录划边界，包**内部**的边界怎么划？如果每个字段外部都能改，"状态只能由 service 改"（见 §1.3）就只能靠自觉，而自觉拦不住下一个新写的 handler。

**原理。** Go 的访问控制只有一条规则：**标识符首字母大写 = 导出（exported），小写 = 未导出（unexported）**。没有 `public`/`private`/`protected` 关键字（`internal` 是目录语义，不是关键字）。作用域单位是**包**：未导出标识符只能被同一个包的代码访问，导出标识符任何 import 了它的包都能访问。

**本项目怎么做。** 跨包要用的必须大写：`domain.Sandbox` 的字段（`internal/domain/sandbox.go:214`）全部大写，因为 service 要构造、store 要序列化它们。只在包内用的必须小写：`internal/store/store.go:64` 的 `Store`，`mu`、`dir`、`wal`、`tenants`、`sandboxes` 全是小写，外部只能走导出的 `GetSandbox` 之类方法（`internal/store/store.go:397`）。方法同理：`internal/domain/sandbox.go:80` 的 `(sb *Sandbox) Transition` 导出，是受控写入口；而 `:37` 的 `transitions` 小写，外部改不到它，也就无法绕过 `CanTransition` 的校验。

**代价是什么——这一节必须回答的问题。** 粒度太粗：可见性是**包**级，不是类型级、字段级；你没办法让某字段"对 A 包可见、对 B 包不可见"，也没有 `protected`。后果有二。其一，**未导出字段外部连读都不行**，所以本项目要在 store 边界做克隆（`internal/store/store.go:27` 的不变式 2：每个边界都克隆）——否则把内部 map 的指针交出去，外部就能在不写 WAL 的情况下改掉对象。其二，**导出字段是可直改的**，Go 没有属性或 setter；`domain.Sandbox.State` 谁都能写，约束它靠的是"service 是唯一写者"这条分层纪律（见 §1.3），不是语法。

**替代方案。** Java/C++ 有四级访问控制。Go 故意只留一级：规则少到不必查文档、编译器和读者都容易判断，代价是表达能力弱。目录级的 `internal` 能补"包之间"的边界，补不了"字段之间"的。

**怎么验证。** 跑 `go doc sandcastle.dev/sandcastle/internal/domain.Sandbox`，实测输出只列大写字段（`ID`、`TenantID`、`Spec`、`State`、`Version`……），包内小写的 `transitions` 不会出现在导出面里。反证更直接：在别的包里写 `domain.Sandbox{}.state`，`go build` 会报 `unknown field`。

## 2.5 struct、嵌入与方法集：接收者怎么选

**问题。** 有了包边界和字段可见性，下一个问题是"数据和行为怎么摆"。Go 没有 class，字段和方法分开定义，方法通过**接收者**绑定到类型上。接收者选错，会出现"我明明在方法里改了字段，调用方却看不到改动"这种只在运行时才炸的问题。

**原理。** struct 只是字段的集合，本身没有方法；方法是绑定到某个类型上的函数，写成 `func (r T) M()`。**嵌入（embedding）** 是在 struct 里写一个只有类型名、没有字段名的字段：被嵌入类型的方法和字段会被**提升（promote）**到外层类型上，外层的方法集里就有了它们——这不是继承，外层只是"拥有一套"内层的能力。**接收者**有两种：`func (t T)`（值接收者）拿到副本，改它不影响原值；`func (t *T)`（指针接收者）拿到地址，可以改。它还决定**方法集**：类型 `T` 的方法集只含值接收者方法，`*T` 含两者；所以只有 `*T` 才能满足"接口要求了指针接收者方法"的接口。

**本项目怎么做。** 先说字段组织。`internal/domain/sandbox.go:214` 的 `Sandbox` 字段按"身份 → 归属 → 引用 → 规格 → 状态 → 时间 → 版本"排：`ID`，`TenantID`，`TemplateID`/`SnapshotID`/`BaseImage`，`Spec`/`State`/`Backend`/`Metadata`/`Reason`，最后是各时间戳与 `Version`/`BaseVersion`。`internal/domain/tenant.go:77` 的 `Tenant` 是同样的骨架；两者共享的尾部字段（`CreatedAt`、`UpdatedAt`、`Version`、`BaseVersion`）就是本项目对"一个可持久化实体长什么样"的约定（`Version`/`BaseVersion` 的语义见 §10）。注意 `Sandbox` 里**没有节点字段**——这是单机的诚实体现，见 §1.4。

再说嵌入。`internal/httpapi/server.go:199`：

```go
type statusWriter struct {
	http.ResponseWriter
	status int
}
```

这里 `http.ResponseWriter` 是**嵌入的接口**：`statusWriter` 自动拥有 `Header()` 与 `Write()`（从被嵌入接口提升而来），只把 `WriteHeader` 换成自己的版本（`internal/httpapi/server.go:204`）。使用处是 `internal/httpapi/server.go:188` 的 `sw := &statusWriter{ResponseWriter: w, status: 200}`——嵌入字段的名字就是类型名 `ResponseWriter`，所以能这样初始化。另一处嵌入是 `internal/httpapi/admin.go:22` 的 `tenantResponse`，嵌入 `*domain.Tenant`——它当前没有被任何处理器引用（Go 不禁止未使用的类型），但展示了"把 `Tenant` 的字段与方法一并提升"的写法。**net/http 本身的机制见 §4。**

最后是接收者怎么选。会改状态、或代表身份的类型一律用指针——`Transition`（`:80`）会写 `sb.State`、`Extend`（`:269`）会写 `sb.ExpiresAt`、`String`（`:287`）；连只读的 `IdleDeadline`（`:236`）与 `CountableResources`（`:279`）也统一用 `*Sandbox`。而纯值、语义不可变的类型用值接收者——`SandboxState` 的 `Terminal`（`:49`）、`Transitional`（`:55`）、`CanTransition`（`:69`），以及 `EgressPolicy.Validate`（`:104`）、`SandboxSpec.Validate`（`:164`）、`TenantQuota.Validate`（`internal/domain/tenant.go:47`）；它们是值类型或纯输入 DTO，不该有身份。概括成一句：**实体（有身份、会被改）用 `*T`；值类型（无身份、不可变）用 `T`**；且同一类型内部不混用——只要有一个方法必须用指针，其余全部用指针。

**替代方案。** 常见错误是"只读方法用值接收者、写方法用指针接收者"混着来。它能编译，但方法集不一致会让"这个值能不能赋给某接口"难以预测。所以社区惯例（本项目照做）是：一旦某个方法必须用指针，整个类型统一用指针。

**怎么验证。** `go vet ./...` 不会报接收者问题——它不是错误。跑 `go doc sandcastle.dev/sandcastle/internal/domain.Sandbox`：实测列出的 8 个方法全部带 `*Sandbox` 接收者，一个值接收者都没有。

## 2.6 接口：隐式实现、Backend 接口与 nil 接口陷阱

**问题。** 前面几节在讲"怎么把代码关起来"，这一节讲"怎么把代码解耦开"。本项目最要紧的一个问题是：业务逻辑写完后一旦绑死在 Docker 上，想接 Firecracker 就得改几十处调用。如果每次换后端都要动业务代码，那"支持多后端"就是假的。

**原理：隐式实现。** Go 的接口有个关键性质：实现者**不需要声明**自己实现了谁。只要一个类型的方法集**恰好包含**接口要求的全部方法，它就自动满足该接口。没有 `implements` 关键字。15 行以内的最小可运行例子：

```go
package main

import "fmt"

type Speaker interface{ Speak() string }

type dog struct{}

func (dog) Speak() string { return "woof" }

func main() {
	var s Speaker = dog{} // dog 从未声明它实现 Speaker
	fmt.Println(s.Speak())
}
```

预期输出一行 `woof`。`dog` 的定义里没有任何地方提到 `Speaker`——满足关系是编译器推导出来的：接口由使用方定义，实现方不需要知道它的存在。

**本项目怎么做（一）：Backend 接口。** `internal/runtime/runtime.go:152` 定义了 `Backend`，共 24 个方法，分六组：身份（`Name()`，`:155`；`IsolationTier()`，`:163`）、证明（`VerifyIsolation()`，`:183`；`Ping()`，`:186`）、生命周期、执行与文件、快照与镜像、对账（后三组覆盖 `:189`–`:233`）。

**为什么 `IsolationTier()` 必须是接口的一部分（`:163`）。** 理由是三个"否则会怎样"。第一，**没有它，新后端可以不声明等级就编译通过**：等级只写在文档里的话，作者漏掉它没有任何机制提醒；放进接口后，少实现这个方法就是编译失败。第二，**守护进程无法在启动时拒绝档位不匹配**：`cmd/sandboxd/main.go:157` 的 `buildBackend` 拿 `cfg.Isolation` 和本机真实能力比对，要求 `microvm` 但没有 `/dev/kvm`、或驱动仍是骨架时直接拒绝启动（`main.go:159`–`:174`），而不是降级成容器静默跑。第三，**运行中的等级无法机器可读**：`cmd/sandboxd/main.go:230` 会检查 `b.IsolationTier() != runtime.TierMicroVM` 并打 WARNING，`/readyz` 也把它暴露给客户（见 §12）。设计文档把这条写死了（`docs/DESIGN.md:124`）："新增后端必须声明自己的等级"。

**为什么 `VerifyIsolation()` 也必须在接口里（`:183`）。** 它守的失败更尖锐。设计文档 §3.6（`docs/DESIGN.md:180`）说："`IsolationTier()` 只声明等级，`VerifyIsolation()` 才证明等级。"原因在一段实测：把 `HostConfig.UsernsMode` 设成 `private`，容器引擎会**原样回读** "private"，而沙箱里的 `/proc/self/uid_map` 仍然是 `0 0 4294967295`——**什么都没发生**（`docs/DESIGN.md:153`–`:158`）。`internal/runtime/oci/isolation.go:70` 把这个值定义成常量 `identityUIDMap`，注明这正是"引擎接受了字段、回读了它、却一项都没落实"的特征。于是结论是：**读配置（哪怕是从引擎回读的配置）都不是证据，只有内核从沙箱内部报告出来的才是**（内核机制见 §5、§6）。因此 `VerifyIsolation` 用**和生产同一条载荷路径**起一个真沙箱，读内核事实（`internal/runtime/oci/isolation.go:35` 的 `probeScript`），并尝试两个逃逸探针（`mount`、读 `/proc/kcore`）。还有一点：**"测不出来"必须和"测出来且通过"分开**——`internal/runtime/oci/isolation.go:59` 的 `probeKeys` 列全了脚本必须产出的每个键，少一个就拒绝出报告（`docs/DESIGN.md:188` 把"无法测量"单列一行）。把"未验证"渲染成"成功"，比不检查更危险。

**本项目怎么做（二）：接口解耦意味着什么。** 因为 `runtime.Backend` 是接口，`Service` 只依赖接口：`internal/service/service.go:22` 的字段就是 `Backend runtime.Backend`，构造函数 `New(...)`（`:41`）同样只要求 `runtime.Backend`。于是业务逻辑——状态机、快照、预热池、收敛器——一行都不用为换后端而改。要验证接口够不够用，最硬的办法是写第二个实现：`internal/runtime/fc/fc.go:34` 就是，`type Backend struct{}` 配 24 个方法，几乎全部返回 `apperr.Unimplemented`（如 `:40`、`:53`、`:118`）。一句话：**这个接口把"后端能做什么"和"业务想要什么"之间的耦合，压缩成了一次编译期检查**。

**本项目怎么做（三）：`any`、类型断言与 `, ok`。** `any` 是 `interface{}` 的别名，两者完全等价，1.18 之后官方推荐写 `any`；本项目**通篇用 `any`，没有一处 `interface{}`**（在 `cmd/`、`internal/` 下 grep `interface{}` 字面量命中 0 次）。例子有 `internal/apperr/apperr.go:65` 的 `Details map[string]any`、`:81` 的 `Invalid(format string, args ...any)`、`internal/httpapi/server.go:448` 的 `writeJSON(... v any)`。类型断言用来把接口值取回具体类型，`, ok` 形式能避免 panic，`internal/runtime/oci/errors.go:8` 是范例：

```go
func asAppErr(err error) (*apperr.E, bool) {
	if err == nil {
		return nil, false
	}
	if ae, ok := err.(*apperr.E); ok {
		return ae, true
	}
	return nil, false
}
```

`err.(*apperr.E)` 的单值形式在类型不符时会**直接 panic**；加 `, ok` 后，失败返回零值 `nil` 和 `false`。这个 helper 之所以安全，正因为同时处理了"err 本身为 nil"和"err 是别的类型"两种情况。调用点如 `internal/runtime/oci/backend.go:495`：`if ae, ok := asAppErr(err); ok && ae.Code == apperr.NotFound { return nil }`——把"容器已经不存在"翻译成幂等成功。

**nil 接口陷阱——本节最容易在线上爆炸的一点。** 规则是：接口值由"动态类型"和"值"两部分组成，它等于 `nil` **当且仅当两部分都为 nil**；如果类型部分有值、值部分是指针 nil，这个接口值**不等于 nil**。最小例子：

```go
type E struct{ Code string }

func (e *E) Error() string { return e.Code }

func boom() error {
	var e *E // e == nil
	return e // 返回的 error 非 nil！
}

func main() {
	err := boom()
	fmt.Println(err == nil) // 输出 false
	if err != nil {
		fmt.Println(err.Error()) // panic: nil pointer dereference
	}
}
```

预期先输出一行 `false`，随后 panic：`err == nil` 为假，所以走进 `if err != nil` 分支，而 `err.Error()` 里访问 `e.Code` 时 `e` 是 nil。

**本项目怎么避开它。** `internal/apperr/apperr.go:125` 的 `From` 返回类型是**具体指针 `*E`**，不是 `error`，这本身就少了一层陷阱（返回 nil `*E` 时 `== nil` 成立）。但陷阱的形状确实存在于"把 `*E` 当 `error` 返回"的地方：`internal/service/errors.go:29` 的 `errFromCtx` 返回类型是 `error`，实现是 `return apperr.From(ctx.Err())`。`From` 里的 `if err == nil { return nil }`（`internal/apperr/apperr.go:126`）正是为了在**入参**是 nil 时不返回带类型的 nil；而所有把 `apperr.From(ctx.Err())` 当 `error` 返回的调用点，都放在 `case <-ctx.Done():` 分支里（`internal/service/service.go:378`、`internal/runtime/oci/backend.go:408`、`:477`），此时 `ctx.Err()` 必然非 nil。也就是说：**这个项目没踩这个坑，是靠每个可能踩坑的位置都有一个"入参必定非 nil"的保证。**排查线上"错误处理看着对、行为却怪"的 bug 时，先打印 `fmt.Printf("%T %v", err, err)`：若打出 `*apperr.E <nil>`，就踩了这个陷阱。

**替代方案。** 隐式实现之外也可以要求显式 `implements`（像 Java）。Go 没选，因为隐式实现允许"接口由使用方定义"：`internal/service/errors.go:25` 的 `errCreateTimeout(d interface{ String() string })` 与 `:29` 的 `errFromCtx(ctx interface{ Err() error })` 就是**匿名接口**参数，意思是"我要的只是这个能力"，好处是该文件不必 import `time` 和 `context`。代价是"谁实现了谁"不好 grep。

**怎么验证。** 三条：`go build ./...` 通过，说明现有两个后端都满足 `Backend`；`go doc sandcastle.dev/sandcastle/internal/runtime.Backend` 打印接口全文（实测 24 个方法都在）；把上面那段 nil 接口最小例子跑一遍，观察 `false` 与随后的 panic。

## 2.7 泛型：本项目生产代码为何一处不用

**问题。** Go 1.18 引入泛型。这个项目用了吗？没用的话为什么？这不能靠印象回答，得去仓库里数。

**原理。** 泛型（类型参数）让你写出对多种类型都成立的函数或类型，同时在编译期保留类型安全。最划算的三类场景是容器类型、通用算法、以及测试断言辅助函数——最后一类恰好是本项目用了泛型的唯一地方。

**本项目怎么做（如实结论）。** 我 grep 了整个仓库（`func Name[T ...]` 与 `type Name[T ...]`），结论分两半。**生产代码（`cmd/`、`internal/`）里泛型出现 0 次**；泛型只出现在**测试辅助函数**里，共 3 处：`tests/harness_test.go:971` 的 `func eq[T comparable](t *testing.T, got, want T, what string)`、同文件 `:978` 的 `neq[T comparable](...)`、以及 `tests/client_test.go:344` 的 `func decode[T any](t *testing.T, r *Resp) T`。

这 3 处正好命中泛型最划算的场景。`eq` 要能比较字符串、整数、甚至 `domain.SandboxState`；若用 `any` 写，调用方就丢掉类型检查，写错 `eq(t, got, "running")`（把 `SandboxState` 和 `string` 混比）要到运行期才失败；`[T comparable]` 把"必须能用 `!=` 比较"这条约束写进签名。`decode[T any]` 同理，靠类型参数在**每个调用点**推断返回类型（注释 `tests/client_test.go:342` 还提示"传入指针类型参数就得到指针结果"）。

**生产代码为什么不用泛型。** 可观察的原因有二。其一，生产代码里**没有需要泛型的抽象**：泛型收益来自"同一套逻辑服务多种类型"，而本项目的通用容器是 `map[string]*domain.Sandbox` 这类**具体类型**的 map（`internal/store/store.go:83`–`:86`）；要抽象的维度是"实体种类"，但每种实体的存储、CAS、WAL 帧、审计都不同。其二，Go 没有泛型方法、也没有泛型字段（这是对语言能力的陈述），很多"看起来适合泛型"的地方——比如 `Store` 里每个实体的 `Get`/`Put`——用泛型并不能自然表达。至于"本项目风格保守所以不用"这一条属于**推断**：设计文档里没有关于泛型的专门决策记录，不做过度归因。

**如果要用会用在哪儿（假设分析，非现状）。** 一个 `putEntity[T any]` 式的抽象在原理上能替 `internal/store/store.go` 里那些相似的"查表 / 克隆 / 写 WAL / CAS"逻辑减少重复；但本项目**没有**这么做，理由如上。真正需要泛型的场合是"纯算法"——例如对 `[]*Sandbox` 和 `[]*Snapshot` 做同一个分页切片，本项目目前也没有。判据：**当"类型"是你唯一的差异变量时，用泛型；当"类型"还牵动行为分支时，泛型只会把 `if` 换成类型开关，更糟。**

**替代方案。** 没有泛型时用 `any` 加类型断言（本项目的 `writeJSON(v any)`，`internal/httpapi/server.go:448`）或 `reflect`。`any` 的代价是丢掉编译期类型检查（就是上面 `eq` 的反例）；`reflect` 则既丢类型检查又更慢更难读——本项目生产代码没有用 `reflect` 做通用序列化。

**怎么验证。** 跑 `grep -rnE 'func [A-Za-z]+\[[A-Z]' castle --include='*.go'`，实测只命中那 3 处测试辅助函数，`castle/internal` 与 `castle/cmd` 下命中数为 0。

## 2.8 错误处理：从 error 接口到 apperr.Code

**问题。** 在多租户服务里，"错误"不只是"出事了"。调用方要能回答：该不该重试？是我的输入错还是服务端错？是暂时不可用还是永久拒绝？如果这些只能靠猜 HTTP 状态码，客户端就会写出脆弱的逻辑。更糟的是状态码并不总可信——它会经过负载均衡、重试代理、网关，可能被改写。

**原理：`error` 是接口。** Go 里错误是内建接口 `interface{ Error() string }`，任何有 `Error() string` 方法的类型都是错误。把它做成接口而不是具体类型（也不是异常）的原因有三：错误可以携带**任意结构化信息**（能往里放错误码、可重试标志、建议重试秒数）；错误是**值**，可以返回、传递、比较、放进字段；包装靠约定而非语言特性——`fmt.Errorf("...: %w", err)` 用 `%w` 把原错误包进去，`errors.Unwrap` 能剥出来，于是形成一条**包装链**。

**原理：哨兵错误、`errors.Is` / `errors.As` / `%w`。** 哨兵错误是包级导出的一个特定错误值，例如标准库的 `http.ErrServerClosed`；判断"是不是它"用 `errors.Is(err, target)`，它会沿包装链逐个比较，被 `%w` 包了三层也认得出。当错误**携带自定义类型**（要取它的字段）时用 `errors.As(err, &target)`，同样穿透包装链，把链条中第一个类型匹配的错误赋给 `target`。`%w` 与 `%v` 的区别是关键：`%w` 建立可解开的链接（`errors.Is`/`As` 能用），`%v` 只是拼字符串（链接断掉）。所以**包链只该在真正想保留"可判定性"的地方建立**。

**本项目怎么做：`Code` 与 `Retryable` 解决了什么。** 读 `internal/apperr/apperr.go`。`type Code string`（`:15`）配一组常量（`:17`–`:28`，如 `invalid_request`、`conflict`、`quota_exceeded`、`backend_unavailable`、`unimplemented`、`internal`）。这些是对外的稳定契约——包注释（`internal/apperr/apperr.go:3`）明说："clients branch on Code, never on HTTP status or message text"。`type E struct`（`:58`）把这些信息装起来：`Code`、`Message`、`Retryable`、`RetryAfter`（限流用）、`Details`（结构化上下文）。各工厂函数各自钉死了 `Code` 与 `Retryable`：`Invalid` 是 `invalid_request` 且不可重试（`:81`），`Unavailable` 是 `backend_unavailable` 且可重试（`:97`），`Unimpl` 是 `unimplemented`（`:101`，拒绝而不是静默降级）。**这就是"错误即值"落到产品契约上的样子**。

**为什么"状态码不参与重试判定"很重要。** 设计文档（`docs/DESIGN.md:251`）写着："HTTP 状态码不参与重试判定（显式 retryable 字段）"。`Retryable` 是 `E` 上的显式字段，而不是从状态码推出来的，理由在于**同一个状态码，重试语义可能相反**。看 `Code → HTTPStatus` 的映射（`internal/apperr/apperr.go:32`）：`Conflict` 映射到 409；而 409 在本项目至少两种来源——`internal/domain/sandbox.go:85` 用它表达"非法状态转换"（**不该重试**），`internal/store/store.go:241` 用它表达乐观锁并发冲突（**该重试**，机制见 §10）。两者都是 409，靠状态码无法区分，包注释把这点写在文件顶上（`internal/apperr/apperr.go:4`）：`HTTP 409 can be retryable (optimistic-lock conflict) or not (illegal state transition)`。

**一个状态码会被中间设备改写的具体失败场景。** 设想客户端前面有一个网关，配置了"对 5xx 自动重试一次"。你的服务对某个失败返回 500，网关把它改写成 502（很多网关按自己的错误页模板重写上游 5xx）；客户端看到 502，"猜"成可重试，于是重发。如果这个请求是"创建沙箱"、而创建本身没有幂等保证，就变成重复创建。**只要判定依据是状态码，中间环节的每一次改写都是一次语义丢失**；换成响应体里的 `retryable` 字段后，状态码被改写也不影响判断。

一个可以自己核对的细节：`ConflictErr`（`internal/apperr/apperr.go:89`）没有显式设置 `Retryable`，于是取零值 `false`——当前版本把**所有** 409 都报成不可重试，包括乐观锁冲突。我不把它说成 `retryable:true`，因为代码不是这么写的；这恰恰是"字段显式"的意义：将来要单独标注某一类 409 可重试，只需改一个工厂函数。

**错误处理分层：service 层怎么转换与包装。** 读 `internal/service/errors.go`，它的角色是**把底层的、带实现细节的错误，翻译成站得住脚的对外语义**：`errSuspended()`（`:13`）返回 `forbidden`，`errForbidden()`（`:17`）返回"资源属于别的租户"的 `forbidden`，`errMissing()`（`:21`）返回 `not_found`——越权与不存在都被显式命名，避免泄露信息（IDOR 防线见 §12）。有一段真实教训写在 `idgenCheck`（`:53`）的注释里（`:44`–`:52`）：它过去直接返回 `idgen.Parse` 的裸错误，而 `apperr.From`（`internal/apperr/apperr.go:125`）会把未知错误包成 **`internal` 且可重试**（`:133`）；后果是**一个永远不可能成功的畸形 ID 请求，被报成"服务端内部错误，请重试"**，客户端于是重试，形成由客户端输入错误引发的重试风暴。修法是 `idgenCheck` 把解析失败转成 `apperr.Invalid`（`:55`）；注释最后点题：这是端到端套件**断言了错误码**、而非只断言"请求失败"才抓到的。

另一个手法是自定义错误类型：`SandboxFailedError`（`internal/service/errors.go:36`）带一个 `reason`，在 HTTP 层用 `errors.As` 被识别（`internal/httpapi/handlers.go:176`–`:177`），从而能返回"404/500 之外"的响应：HTTP 409 加**完整的沙箱记录**让客户端事后分析（`handlers.go:178`）——这正是 `errors.As` 相比字符串匹配的价值，它能取回**类型**并拿到额外数据。整条链的终点是 `internal/httpapi/server.go:429` 的 `writeError`：`e := apperr.From(err)`（`:430`），随后写出统一包络 `{code, message, retryable, request_id}`（`:438`–`:445`）（包络契约见 §12）。

**替代方案。** 一是只用哨兵错误（`var ErrNotFound = errors.New(...)`）。本项目**没有**在生产代码里定义任何哨兵（在 `cmd/`、`internal/` 下 grep `errors.New` 命中 0 次）：哨兵把"错误种类"压成一个值，无法携带 `Retryable`、`RetryAfter` 这类每实例不同的字段。二是靠字符串匹配错误消息——最脆弱，消息一改就全崩；`Code` 作为稳定对外契约（`internal/apperr/apperr.go:3`）就是为了替代它。

**怎么验证。** 两条：`go doc sandcastle.dev/sandcastle/internal/apperr` 会列出 `Code` 全部常量与 `E` 字段；更说明问题的是跑 `TestHTTPContract/a_malformed_id_is_refused_as_client_input`（`tests/contract_test.go:166`），它拿四个畸形 ID 请求，逐个断言 `MustStatus(http.StatusBadRequest)` 加 `MustErr(apperr.InvalidRequest)`，并检查 `r.Err().Retryable == false`（`tests/contract_test.go:179`）。**这条用例断言的正是"错误码 + 可重试标志"，而不是"请求失败"**——`idgenCheck` 的重试风暴缺陷就是被它抓住的。

## 2.9 命名与惯用法

**问题。** 命名不是审美问题。团队里若有人写 `sandboxId`、有人写 `sb`，读代码的人就要为每个变量重新猜一次含义。惯用法也不只是好看：Go 的"提前返回"和"短变量名"背后有实际的阅读成本考量。

**原理。** 四条被广泛接受的惯例：变量名长度与作用域成反比（作用域越小越短，`i`、`err`、`s` 就够）；错误检查后**尽早返回**，让正常路径保持在最左列；接收者命名要短且一致，通常取类型首字母（`s` for `Server`、`sb` for `Sandbox`）；构造函数按 `NewXxx` 命名，返回已初始化好的对象。

**本项目怎么做。** 逐条举一个真实片段。**短变量名**：`internal/store/store.go:90` 的 `func Open(dir string, opts Options) (*Store, error)` 用 `s` 指代 store；`internal/domain/sandbox.go:236` 的 `func (sb *Sandbox) IdleDeadline()` 用 `sb`。**尽早返回**：`internal/service/service.go:96` 的 `CreateSandbox` 开头是一连串"检查失败即 `return nil, err`"——`:98`（`errSuspended`）、`:101`（`resolveImage`）、`:106`（`spec.Validate`）、`:109`（`validateMetadata`）、`:113`（`diskQuotaRefused`）、`:115`（`runtime.CheckEgress`）、`:118`（`s.Quotas.CheckCreate`），七个检查排成一列，正常路径没有被推进缩进深处。**接收者命名**：`internal/` 下 `*Backend` 用 `b`、`*Service`/`*Store`/`*Server` 用 `s`、`*Sandbox` 用 `sb`，全是类型首字母且同类型内一致。**构造函数**：`internal/service/service.go:41` 的 `New(...)`、`internal/domain/sandbox.go:252` 的 `NewSandbox`、`internal/gov/gov.go:35` 的 `NewAuthenticator`——都返回初始化好的对象，把字段填充收在构造函数里（如 `NewSandbox` 里 `State: StateProvisioning`）。**用构造函数守住不变式**：`NewSandbox`（`:252`）强制新对象从 `StateProvisioning` 开始，`NewTenant`（`internal/domain/tenant.go:88`）强制初始状态是 `TenantActive`，于是"新对象一定处于合法初始状态"成了不需要检查的前提。

**替代方案。** 另一种风格是长变量名加 setter/getter（Java 常见）：`sandboxIdentifier`、`getState()`。Go 没选它：长名字在短作用域里只是噪音，而导出字段可直接读写（见 §2.4）。"尽早返回"的替代写法是层层嵌套的 `if … else …`，本项目一处也没有。

**怎么验证。** `gofmt -l .` 与 `go vet ./...` 是基础门槛，两者都应无输出（本项目实测 `go build ./...` 为 0）。命名一致性没有自动检查，但有一条便宜的读法：`grep -rhoE 'func \([a-z]+ \*?[A-Za-z]+\)' internal/ | sort | uniq -c`，会看到每个类型的接收者名唯一。

## 2.10 defer：顺序、返回值与循环里的坑

**问题。** 资源清理（关文件、解锁、取消 context、回收临时对象）如果写成"每个返回点都手写一遍"，那么一旦新增一个返回点就漏一次——这类泄漏在审查时几乎看不出来。`defer` 就是为此存在的，但它有几条反直觉的规则，用错就在线上出事。

**原理。** `defer f()` 把 `f` 登记到当前函数的延迟调用栈上，函数返回时按**后进先出**顺序执行。第二条反直觉规则：**defer 在登记时就求值它的实参**（不是执行时），所以 `defer fmt.Println(x)` 打印的是登记那一刻的 `x`。第三条：如果函数的返回参数**有名字**，defer 里的闭包可以修改它，因为命名返回值是函数作用域内的变量。第四条坑：在循环里 `defer` 不会在每轮结束时执行，而是攒到函数返回时一次性执行——循环里开资源、循环里 defer 关闭，可能攒下成千上万个句柄。

**本项目怎么做。** 用法可归成三类。第一类**释放资源**：`internal/config/config.go:258` 在 `os.Open` 成功后 `defer f.Close()`，紧跟错误检查之后（`:254`–`:258`），保证后面无论从哪个分支返回文件都会关。第二类**取消 context**：`cmd/sandboxd/main.go:141` 的 `shutdownCtx, cancel := context.WithTimeout(...)` 紧跟 `defer cancel()`（`:142`）；`internal/runtime/oci/backend.go:586`–`:588` 有 `defer cancelExec()` 与 `defer cancel()` 两条连用。第三类**配对清理**：`internal/runtime/oci/isolation.go:100` 的 `defer b.reapProbe(ref)`——`VerifyIsolation` 先创建一个临时沙箱（`:91`），登记一条 defer 保证无论后面哪一步失败（`:102` 启动、`:106` 执行）都会把它回收掉。

**defer 与命名返回值的关系，本项目有一个可读的例子**：`internal/runtime/runtime.go:189` 的接口方法 `Create(...)` 用了命名返回值 `(ref string, err error)`——意义之一是让 defer 里的清理逻辑能观察或调整返回值（例如把 panic 恢复成 error 的惯用法）。它在 `fc` 里的实现（`internal/runtime/fc/fc.go:53`）没有用命名返回值：接口声明处保留名字，是为了把返回值的含义写进签名。

至于**循环里的坑**，本项目没有踩：需要"循环内即时释放"的地方用显式配对调用而不是 defer。以 `internal/runtime/oci/backend.go:472` 附近的重试循环为例，它在每轮里**直接赋值** `lastErr` 并在循环外统一判断（`:482`）——那正是"在循环里 defer"的经典误用场景。结论：**defer 适合"函数级"的清理，不适合"循环迭代级"的清理**。

**替代方案。** 一种是每个返回点手写清理，返回点一多就漏；另一种是 C++ 的 RAII 析构，更细，但要求类型有析构语义。Go 选了 defer 这种"函数级、显式登记"的折中。

**怎么验证。** 一个 10 行实验就能看到顺序与参数求值：写一个函数，依次 `defer fmt.Println("a", i)` 与 `defer fmt.Println("b")`，其中 `i` 在后面被改；预期输出是后登记的 `b` 先打，而 `a` 打印的是登记时的旧值。至于本项目，直接读 `cmd/sandboxd/main.go:70`–`:77`：`store.Open` 成功后 `defer st.Close()`（`:77`）落在同一函数 `run` 的作用域里，没有任何中间返回点会跳过它。

---

### 本章检验

1. `internal/` 的边界是编译器强制的，`cmd/` 的边界只是约定。假设你被要求"把 `internal/domain` 改成外部可 import 的公开包"，请列出这次改动会带来哪三类具体后果（至少一类要涉及"以后能不能改字段名"），并说明为什么本项目当初把它放进 `internal`。

2. `IsolationTier()` 与 `VerifyIsolation()` 各紧跟着一段长注释解释"为什么它在接口里"。请分别说出这两个方法防止的具体失败；然后回答：如果只保留 `IsolationTier()`、把 `VerifyIsolation()` 去掉，会重新打开哪一个已经关闭的漏洞？（提示：`identityUIDMap` 与"配置不是证据"。）

3. 本章给了 `Retryable` 必须显式的理由，也如实说明了 `ConflictErr` 当前把 409 报成不可重试。请构造一个"状态码被中间设备改写导致客户端误判重试"的完整故事（请求、中间设备的行为、客户端的错误决策、最终后果），并说明为什么把判定依据换成响应体里的 `retryable` 字段就能避免它。

4. 泛型这一节的结论是"生产代码 0 处使用，测试辅助函数 3 处"。请论证：为什么 `eq[T comparable]` 用泛型是正确的选择，而把 `internal/store/store.go` 里的各实体 `Get`/`Put` 改成 `Store[T]` 反而更糟？两者差别的判据是什么？

5. **动手任务**：写一个最小程序，制造并观察 nil 接口陷阱。(a) 定义一个用指针接收者实现 `error` 的具名类型；(b) 写一个返回类型是 `error` 的函数，让它返回那个类型的 nil 指针；(c) 在 `main` 里打印 `err == nil` 并尝试调用 `err.Error()`，记录输出与是否 panic。然后把 (b) 的返回类型从 `error` 改成那个具体指针类型，重跑，观察 `err == nil` 变成了什么——用它解释 `internal/apperr/apperr.go:125` 的 `From` 为什么把返回类型写成 `*E` 而不是 `error`。


# §3 Go 并发与 context

## 3.1 一个进程里同时有哪几件事在跑

**问题**：先不谈语法。`sandboxd` 启动后，同一进程里至少有四类"同时进行的事"：处理请求的 goroutine、后台的回收与对账循环、被派发给后端的创建工人、库内清扫协程。任意两类同时读写同一块内存，或其中一类永不退出，就会产生下面三类故障，每个都给出可复现时序。

**故障一：共享 map 并发写，进程被运行时错误打死。** 设 `store` 的内存索引是一张裸 map（没有锁）。两个租户同时发创建请求，各自在自己的请求 goroutine 里走到"把新沙箱写进索引"：

```text
时刻 t0   G1 进入 PutSandbox(A)：s.sandboxes["sbx_A"] = A
时刻 t1   G2 进入 PutSandbox(B)：s.sandboxes["sbx_B"] = B   ← 与 G1 同时写同一张 map
```

Go 运行时检测到并发写 map 时抛 `fatal error: concurrent map writes`。注意措辞是 `fatal error` 而不是 `panic`：这是运行时直接终止进程，**`recover()` 抓不住**。所以 `internal/httpapi/server.go:157` 的 `recover` 中间件在这里完全无效——它只救 handler 里普通 `panic`，救不了 runtime 的致命错误。一个正常请求就能把整个控制面打死。

**故障二：goroutine 泄漏，内存只涨不落。** goroutine 一旦 park 在永远不会就绪的操作上（典型是往没人接收的 channel 发消息），它和它引用的整条对象图就一直活着。这不是抽象风险：`Limiter` 的清扫协程在 v1.1.0 里**根本没有停止通道**，每个 `Service` 创建时泄漏一个（`CHANGELOG.md:417`）。时序是：

```text
时刻 t0   测试用例 1 调用 New(service) → NewLimiter() → go l.sweeper()
时刻 t1   用例 1 结束，Service、Limiter、桶表全部被丢弃
时刻 t2   sweeper 仍持有那张桶表，每 10 分钟醒来扫一次
...        用例 2..N 各泄漏一个，N 个协程 + N 张桶表常驻
```

`internal/gov/limiter_regression_test.go:148` 的 `TestStopTerminatesTheSweeper` 用 `runtime.NumGoroutine()` 把这件事变成可断言的量：创建 300 个 `Limiter`，`Stop()` 之后协程数必须回落。**"会不会泄漏"必须被度量，而不是靠读代码相信。**

**故障三：后台循环停不掉，优雅停机挂住。** 停机路径在 `cmd/sandboxd/main.go:132-144`：收到 `SIGTERM` 后先用 20 秒预算的 `shutdownCtx` 调 `srv.Shutdown`（停止接受新连接、等在途请求做完），再调 `svc.Stop()`（关掉后台循环）。若某个循环没有 `select` 到停止信号，`svc.Stop()` 只是把一个 channel 关掉而没人理它。更隐蔽的是顺序：**先 `Shutdown` 再 `Stop` 是有意的**——在途请求依赖对账循环和供给工人继续工作，先停循环会让它们卡半路，20 秒预算耗尽后 `Shutdown` 返回错误。

**原理**：并发不是"同时跑很多代码"，而是"多件事之间没有固定的先后顺序"。上面三个故障都源于"顺序没被约定"。本章后面的每个工具都在回答同一个问题——**用什么机制把并发的无序约束成设计者能保证的顺序**：锁约束内存访问，channel 约束通信，`stopCh` 约束退出，`context` 约束取消。

**本项目怎么做**：四类并发者各有归属文件。请求 goroutine 由 `net/http` 提供（每请求一个，不在本仓库代码里）；后台循环有四个，全部在 `internal/service/service.go:60` 的 `StartLoops` 里启动；异步工人只有一处，`internal/service/service.go:151` 的 `go s.provision`；库内协程只有一处，`internal/gov/gov.go:133` 的 `go l.sweeper`；另有 `cmd/sandboxd/main.go:120` 一个跑监听器的协程。数清"有几个 goroutine、各自怎么退出"，是读并发代码的第一步。

**为什么不是别的做法**：最彻底的替代是彻底不要并发——单线程顺序处理每个请求，三个故障都不存在。但原因很硬：一次创建要轮询容器相位，慢时是秒级；一次 `exec` 可能跑 30 分钟。顺序处理意味着一个慢请求阻塞所有租户，多租户的公平性直接崩掉。所以本项目选择并发加明确的退出约定。

**怎么验证**：`go test -race ./internal/...`（等价于 `make test`）。`-race` 在运行期检查内存访问的先后顺序，把故障一那类问题在测试阶段就报出来。端到端层面，`tests/concurrency_test.go:7` 的注释给出另一条路径：用 `SBX_IT_RACE=1` 构建被测守护进程，让竞态检测器覆盖**守护进程自己的** goroutine，而不只是 harness 的。

```bash
$ SBX_IT_RACE=1 SBX_REQUIRE_DOCKER=1 go test -count=1 -timeout 90m ./tests/
```

预期：全部通过，守护进程日志里不出现 `WARNING: DATA RACE`；若出现，输出会给出两个冲突访问各自的 `文件:行` 与 goroutine 创建栈。

## 3.2 goroutine：它轻在哪，又泄漏在哪

**问题**：为什么这个项目敢为每个 `Limiter` 起一个协程、为每次创建起一个工人协程？若 goroutine 和 OS 线程一样贵，这些设计全都不成立。反过来，既然它便宜，为什么还要在意"它有没有退出"？两个问题必须一起答。

**原理**：goroutine 是运行时的**用户态**调度单位，不是内核线程。它初始只约 2 KiB 栈，会随递归深度自动增长、也会收缩；一个 OS 线程的栈通常固定 1–8 MiB。这个量级差决定了并发数：几十万 goroutine 是常规操作，几十万线程会先把内存吃光。

调度模型记住三样即可：**G**（goroutine，要跑的代码）、**M**（machine，真正执行的操作系统线程）、**P**（processor，可调度上下文，同时决定并行度）。G 排队，由某个 M 领一个 P 去执行。goroutine 之所以轻，关键有两点：阻塞在 channel 或锁上的 G 只是被换下 P，**M 不被阻塞**，可以立刻去跑别的 G；只有真正发起阻塞系统调用（比如读写文件）时 M 才可能被拖住，此时运行时会再开一个 M 顶上。栈能增长加阻塞不占线程，合起来就是"轻"。

"轻"的另一面正是泄漏的代价：**goroutine 没有父子的自动回收**。`go` 启动之后，没有任何机制替你结束它，它只能自己 `return`。所以它的所有出口条件必须显式写出。泄漏有两种典型形态：**阻塞在 channel 上**（`ch <- v` 没有接收者，G 永久 park，不占 CPU，只看到内存不降）；**循环没有退出条件**（`for { ... }` 里没有 `case <-stopCh`，可能空转烧 CPU，也可能每个 tick 做一次工作）。这两种形态在本仓库历史里各出现过一次。

**本项目怎么做**：把每个 goroutine 的启动点和退出条件列全。

第一，监听器协程，`cmd/sandboxd/main.go:119-128`：

```go
	errCh := make(chan error, 1)
	go func() {
		logf("info", "http listening on %s (tls=%v)", cfg.Listen, cfg.TLSCertFile != "")
		if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			errCh <- srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
			return
		}
		errCh <- srv.ListenAndServe()
	}()
```

退出条件是 `ListenAndServe` 返回（正常关闭返回 `http.ErrServerClosed`）。细节：`errCh` 的**缓冲是 1**。若它无缓冲，而主协程已走到 `svc.Stop()` 不再读 `errCh`，这个 goroutine 就会永久阻塞在发送上——正是"阻塞在 channel 上"那种泄漏。缓冲 1 保证它总有地方放下结果。

第二，四个后台循环，`internal/service/service.go:60-67`：

```go
func (s *Service) StartLoops() {
	go s.sweepLoop()
	go s.reconcileLoop()
	go s.compactLoop()
	if s.Cfg.PoolEnabled && s.Cfg.PoolSize > 0 {
		go s.poolLoop()
	}
}
```

它们靠什么退出？看一个就够，`internal/service/reconciler.go:18-31`：

```go
func (s *Service) reconcileLoop() {
	t := time.NewTicker(s.Cfg.ReconcileEvery)
	for {
		select {
		case <-s.stopCh:
			t.Stop()
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			s.Reconcile(ctx)
			cancel()
		}
	}
}
```

**每个循环的 `select` 里都必须有 `<-s.stopCh` 这一分支**，这就是退出条件；`sweepLoop`（`internal/service/reconciler.go:33`）、`compactLoop`（`internal/service/reconciler.go:46`）、`poolLoop`（`internal/service/pool.go:70`）结构一致。每个 tick 里都新起一个带超时的 `ctx`——一次对账不能因为后端卡住就永远占着循环。

第三，创建工人，`internal/service/service.go:151` 的 `go s.provision(...)`。这是**唯一一处 fire-and-forget**：它不由 `stopCh` 控制，退出条件是"函数自然返回"。它靠两层约束保证一定返回，`internal/service/service.go:282-292`：

```go
func (s *Service) provision(sbID, imageRef string) {
	select {
	case s.createSem <- struct{}{}:
	case <-time.After(s.Cfg.CreateQueueTimeout):
		s.failSandbox(sbID, "create queue timeout — node is saturated")
		return
	}
	defer func() { <-s.createSem }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
```

第一层是信号量 `createSem`（容量 `MaxConcurrentCreates`）：拿不到名额就在 `CreateQueueTimeout` 后放弃并把沙箱标记为失败，绝不无限等待。第二层是 5 分钟的 `ctx`：一旦开始干活，所有后端调用都受它约束，最坏 5 分钟也返回。**这就是"不要让 goroutine 的寿命无界"的具体写法**——用超时把"多久结束"变成一个可计算的数。

第四，限流器清扫协程，`internal/gov/gov.go:252-263`，退出条件是 `<-l.stopCh`，由 `internal/gov/gov.go:141` 的 `Stop()` 关闭。它被接进 `Service.Stop`，见 `internal/service/service.go:69-76`：注释写明——停掉 service 自己的循环而不管限流器的清扫协程，等于让它继续对着一张已被丢弃的桶表跑。

**为什么不是别的做法**：这四处本可以用 `errgroup` 或 `sync.WaitGroup` 统一管理"起一批、一起停"。本项目没有：零依赖约束（见 §16）排除了 `golang.org/x/sync` 的 `errgroup`；而需求是"发一个广播、各自看到后退出"，正是 channel 关闭天然表达的语义，用 `WaitGroup` 反而要把广播翻译成逐个通知。

**怎么验证**：`go test -race -run TestStopTerminatesTheSweeper ./internal/gov/`。它先创建 300 个 `Limiter` 等它们全部 park，逐个 `Stop()`，再断言 `runtime.NumGoroutine()` 回落（`internal/gov/limiter_regression_test.go:148`）。预期：通过。若给 `Limiter` 加了新协程却忘了在 `Stop` 里停它，这个测试会以"协程没有退出"失败。

## 3.3 channel：语义、关闭、与三种固定模式

**问题**：有了 goroutine，第一个问题是它们怎么通信。直接办法是共享变量加锁；另一个是把值从一个 goroutine **传给**另一个。Go 偏好后者，因为传值可以携带明确的先后顺序：接收者看到值，就说明发送者此前的写都已完成。这一节说清 channel 的语义边界与三个模式。

**原理**：先看最小例子，无缓冲与有缓冲的差别：

```go
func main() {
	unbuffered := make(chan int)      // 容量 0
	buffered := make(chan int, 1)     // 容量 1

	go func() { unbuffered <- 1 }()   // 发送方会阻塞，直到有人接收
	fmt.Println(<-unbuffered)         // 输出 1

	buffered <- 2                     // 不阻塞：缓冲区放得下
	fmt.Println(<-buffered)           // 输出 2
}
```

预期输出两行 `1`、`2`。差别在**同步点**：无缓冲 channel 的发送必须等到接收方就绪，`unbuffered <- 1` 在那一刻才返回——所以它是一次"会合"，天然携带"我的事你做之前就完成了"的顺序保证。有缓冲的发送只要缓冲区没满就立刻返回，两者之间**没有**这次会合，先后关系不由这一句保证。这是最容易混淆的地方：缓冲不是为了性能随手加的，它放松了同步。

`close` 的语义：关闭后接收端立刻不再阻塞，得到该类型的零值；`v, ok := <-ch` 判断时 `ok` 为 `false`。`close` 只能由**发送方**调用——若接收方关了 channel，发送方再发就 `panic: send on closed channel`。规矩是：**谁发送，谁关闭；永远不要由接收方关闭。** 若有多个发送方，就需要额外约定，或干脆不关，改用单独的 `done` channel 发信号。

`for range` 是"一直收到关闭为止"的简写：`for v := range ch { ... }` 在 channel 关闭且数据取完后自动结束。`select` 是"多路等待"，会随机选一个就绪分支；加 `default` 就变成非阻塞——**没有分支就绪时走 `default`，立刻返回**。

在此之上，"取消"这个需求催生了一批固定模式。最通用的是超时模式与 done 模式：

```go
func worker(done <-chan struct{}, out chan<- int) {
	for i := 0; ; i++ {
		select {
		case <-done:      // 收到取消，干净退出
			return
		case out <- i:    // 有接收者才发
		}
	}
}
```

`done` 是一个只用来"关闭"的 channel：`close(done)` 立刻唤醒所有在 `<-done` 上等待的 goroutine，这是**一对多广播**；`select` 里的 `case <-ctx.Done()` 就是它的库化版本（下一节展开）。超时模式则是 `select` 配 `time.After`。

**本项目怎么做**：仓库里的 channel 用法全落在这几个模式上。

信号量用带缓冲 channel，`internal/service/service.go:51`：`createSem: make(chan struct{}, cfg.MaxConcurrentCreates)`。`struct{}` 零字节，表示"只传递名额、不传递数据"；放入一个就占一个名额（`internal/service/service.go:284`），取出一个就释放（`internal/service/service.go:289`）。这里**用 channel 表达容量，而不是用计数器加锁**，好处是等待和超时能直接写进 `select`。

广播用 `close(channel)`：`internal/service/service.go:53` 定义 `stopCh: make(chan struct{})`，`internal/service/service.go:71` 用 `close(s.stopCh)` 一次唤醒所有后台循环，`internal/service/service.go:70` 用 `sync.Once` 保证只关一次（关两次会 panic）。每个循环的 `case <-s.stopCh` 都是这条广播的接收端。

"谁关闭"的规则在 `cmd/sandboxd/main.go:119` 体现得很清楚：`errCh` 由监听器协程**发送**，但它并不关闭它——主协程只在 `select` 里收一次就继续往下走。

**为什么不是别的做法**：`waitForState` 是一个反例性的取舍，`internal/service/service.go:374-386` 用 `select` 同时等三件事——超时、`ctx.Done()`、以及一个 150 毫秒的 `tick.C`，每次 tick 去 store 读一次状态：

```go
	after := time.After(deadline)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-after:
			return errCreateTimeout(deadline)
		case <-ctx.Done():
			return errFromCtx(ctx)
		case <-tick.C:
			sb, ok := s.St.GetSandbox(id)
			if ok && targetSet[sb.State] {
				return nil
			}
		}
	}
```

它没有用"发布订阅"（状态变更方主动往 channel 推事件）。为什么？函数上方 `internal/service/service.go:356-358` 的注释给了理由：**store 是唯一真相源，轮询不可能漏掉一次状态变更**。改成事件推送就得处理"事件发出时还没有订阅者""订阅者列表要加锁"这类问题；轮询把顺序问题换成一次多余但无害的读。这是"传所有权"与"读共享状态"之间的取舍：前者低延迟，后者难出错，这里选后者。

**怎么验证**：`go test -race -run TestConcurrentExec ./tests/`。它在同一沙箱里并发跑 20 条 `exec`，每条输出一个独有标记，断言**每个调用者拿到的都是自己那条命令的输出**（`tests/concurrency_test.go:81-115`）。预期：20 个状态全 200，输出逐条对应。若某环节把不同请求的输出串了通道，这里会立刻暴露——输出串了就等于 agent 拿到了别人的命令结果。

## 3.4 sync 家族：锁、Once、WaitGroup、atomic 各自解决什么

**问题**：channel 适合传值，但有些共享状态天生不适合"传"——比如一张被所有请求读、偶尔被写的表。这类状态需要的是"同一时刻只允许一个写者"或"读可以并行"。要选对工具，必须先看清每个工具的语义边界。

**原理**：逐个说清它保证什么、不保证什么。

`sync.Mutex` 是互斥锁：`Lock`/`Unlock` 之间构成临界区，同一时刻至多一个 goroutine 进入。它建立 happens-before：**上一个持有者在 `Unlock` 之前做的所有写，下一个持有者在 `Lock` 之后一定看得到**。这比"互斥"本身更重要——它意味着临界区里可以放心读写普通变量，不需要额外的内存屏障。

`sync.RWMutex` 区分读写锁：多个 `RLock` 可并存，`Lock` 独占。问题是"读多写少真的更快吗"。答案是**要看临界区里做了什么**。读写锁内部要维护读者计数、在写者到来时阻塞新读者以防写者饿死，每次 `RLock`/`RUnlock` 都要原子操作改计数；若临界区只是"从 map 取一个指针"，这段计数的开销可能**比临界区本身还大**，此时它比普通锁更慢。它真正占优的条件是：读远多于写，**且读临界区足够长**。所以"读多写少就用 RWMutex"是不完整的，条件要加上"读操作本身够重"。

`sync.Once` 保证一段初始化**只执行一次**，且所有调用者都会等它执行完。`sync.WaitGroup` 是"等一组任务全部完成"：`Add(n)` 计数，`Done()` 减一，`Wait()` 阻塞到归零——它关联的是**优雅停机**。`sync/atomic` 提供对单个变量的原子操作，最容易被误用，因为"我用 atomic 读了所以安全"是错的：它只保证那**一个**变量的读写原子，不保证围绕它的多步逻辑原子。它真正不可替代的场景是作为**同步原语的构件**（实现 `Once`、用 CAS 做无锁计数）。

**本项目怎么做**：用 Grep 搜 `sync.` 得到的真实使用点，逐个归类。

`sync.Mutex`（生产代码）：`internal/service/service.go:31` 的 `poolMu`、`internal/service/service.go:33` 的 `activityMu`、`internal/service/service.go:35` 的 `isoMu`；`internal/gov/audit.go:17` 的 `mu`；`internal/gov/gov.go:119` 的 `mu`；`internal/httpapi/server.go:300` 的 `mu`（认证失败聚合器）；`internal/httpapi/handlers.go:25` 的包级 `lastUsedMu`；`internal/metrics/metrics.go:50` 的 `mu`。

`sync.RWMutex`：只有一处，`internal/store/store.go:65` 的 `mu`。这很说明问题——**全项目唯一需要读写并行的地方就是 store 的内存索引**，因为它是被读得最多、写得相对最少的那张表。其他地方要么访问频率低（审计、指标），要么临界区极短（`isoMu` 只保护一个指针），普通 `Mutex` 就够。读写锁不是"读多写少"的同义词，store 用它是因为读路径足够频繁，值得那点额外开销。

`sync.Once`：`internal/service/service.go:38` 的 `stopOnce`、`internal/gov/gov.go:123` 的 `stopOnce`，用法一致，都是"关闭一个 channel 这个动作只能发生一次"。

`sync.WaitGroup`：值得单独指出——**生产代码里一处都没有**，只出现在测试与 PoC（`tests/concurrency_test.go:47`、`tests/concurrency_test.go:90`、`internal/store/store_test.go:307`、`poc/walrace/main.go:56` 等）。这解释了上一节那个取舍：生产代码的停机用"关闭 `stopCh` 广播"，而 `WaitGroup` 在测试里用来"等一批并发请求全部返回后再断言"。

`atomic`：同样只在测试与 PoC 里出现，`tests/commercial_test.go:201` 的 `atomic.AddInt64(&s.ok, 1)`、`internal/store/store_test.go:306` 的 `atomic.Int32`（统计 CAS 竞争的胜者数）。生产代码不用它保护业务状态——因为业务状态的更新总是多字段的，必须整体受锁保护，单变量原子操作表达不了。

**为什么不是别的做法**：一个自然的替代是"全用 `Mutex`，不用 `RWMutex`"。没这么做是因为 store 读路径密：`/v1/sandboxes` 列表、对账循环的 `ForEachSandbox`、`/readyz` 的 `Healthy` 都在读，串行会明显压低多租户读吞吐。反过来，把所有 `Mutex` 换成 `atomic` 是错误的：`Service.pool` 是一张 map（`internal/service/service.go:32`），对它的"检查再修改"是多步操作，`atomic` 无法让整段逻辑原子化。

**怎么验证**：`go test -race -run TestConcurrentCASExactlyOneWinner ./internal/store/`。它起 16 个 goroutine 用同一版本号并发 `UpdateSandbox`，用 `atomic.Int32` 统计成功数，断言**恰好一个**成功（`internal/store/store_test.go:300-324`）。预期：通过，`winners = 1`。它同时验证两件事：锁把并发 CAS 串行化，且版本比较语义是"只有一个赢家"（版本比较本身的含义见 §10）。

## 3.5 context：取消信号如何从 HTTP 请求传到最底层

**问题**：客户端发起一个 `exec` 请求后中途断开。此时控制面正在跑一个可能持续几十分钟的后端调用。若没有机制把"客户端走了"传下去，这条请求会一直占着资源跑完——对多租户平台，这是一条免费的资源消耗路径。`context` 要解决的正是：**一个取消信号，从请求入口一路传到最底层的阻塞操作**。

**原理**：`context.Context` 是一棵由父到子的树，每个节点可以携带一个取消信号（`Done()` 返回一个 channel）、一个取消原因（`Err()`）、一个截止时间。四个构造函数对应四种来源：`WithCancel(parent)` 手动取消；`WithTimeout(parent, d)` 在 `d` 后自动取消；`WithDeadline(parent, t)` 到某绝对时刻取消；`WithValue(parent, key, val)` 挂一个键值对。前三个都返回 `cancel`，**必须调用**（通常 `defer cancel()`），否则父 ctx 活着时子 ctx 的资源不会被释放。取消的语义是**广播**：任一节点被取消，它的所有子孙的 `Done()` 都会关闭——这是上节 `close` 模式的库化。

搭配写法是固定的：等待方用 `select` 同时监听业务 channel 和 `ctx.Done()`，收到后不要立刻返回"未知错误"，而是调 `ctx.Err()` 分辨原因——它返回 `context.Canceled`（有人主动取消）或 `context.DeadlineExceeded`（超时到了）。两者意义不同，必须区分。本项目的 `waitForState` 就写成 `case <-ctx.Done(): return errFromCtx(ctx)`（`internal/service/service.go:378-379`），由 `errFromCtx` 去读 `ctx.Err()` 决定返回哪种错误。

传播规则有三条，都要理解"为什么"：

1. **只能向下传，不能向上传。** 子 ctx 由父 ctx 派生；父被取消，子必然被取消；反之不成立。所以一个函数只能把取消权交给调用者，不能反过来控制调用者。这保证取消只向一个方向收敛。
2. **不应当存业务数据。** `WithValue` 的键必须是自定义类型（本项目用未导出的 `ctxKey`，`internal/httpapi/server.go:28`），避免和别的包撞键。但更重要的规矩是"不要拿它当参数包"：context 会传得很远，一旦承载业务参数，任何一处改动都变成隐式契约。它的正当用途只有横切关注点——请求 ID、认证身份、追踪信息。
3. **取消是广播，不是单播。** 这决定了"取消一个请求"会同时终结它派生出的所有子任务，这是想要的：客户端走了，为它做的一切都该停。

`http.Request.Context()` 的关系是：`net/http` 为每个请求预置了一个 ctx，当客户端断开或连接超时，它替你把该 ctx 取消。所以 handler 里拿到的 `r.Context()` 已经是"带取消信号"的——你要做的是**接着往下传**，而不是另起一个。

**本项目怎么做**：跟着一次 `exec` 请求走一遍。

入口在 `internal/httpapi/handlers.go:341`：`s.Svc.Exec(r.Context(), tn.ID, key.ID, r.PathValue("id"), req)`。**注意传的是 `r.Context()`，不是 `context.Background()`**——这一句是整个取消链的起点。service 层收下它并在最底层用掉，`internal/service/lifecycle.go:251`：`return s.Backend.Exec(ctx, sb.Backend.Ref, req)`。到了后端，它被用作派生的父节点。

后端如何处理超时，是这一节最需要讲清的地方，`internal/runtime/oci/backend.go:578-588`：

```go
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultExecTO
	}
	// Outer ctx covers create+inspect calls; the exec stream itself runs on
	// execCtx so its read loop unblocks exactly at the deadline instead of
	// hanging on a silent stream until the outer context expires.
	execCtx, cancelExec := context.WithTimeout(ctx, timeout)
	defer cancelExec()
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
```

这里有两个源头，必须分清。**第一是请求级超时**：`req.Timeout` 来自客户端 JSON 的 `timeout_ms`（`internal/httpapi/handlers.go:338`），在 service 层被夹紧——超过平台上限直接拒绝，小于等于零则取 60 秒默认（`internal/service/lifecycle.go:229-234`），上限默认 30 分钟（`internal/config/config.go:216`）。**第二是后端自己的硬截止**：后端不假设 service 设过超时，若 `req.Timeout <= 0`，它自己退回到 `defaultExecTO`（5 分钟，`internal/runtime/oci/client.go:30`）。两者是**层层设防**：service 是权威，把客户端的随意值夹进平台契约；后端是最后一道，防"上游忘了设"。

那两行 `WithTimeout` 写成 `timeout` 与 `timeout+30s` 两个值，是流式接口才有的细节。`execCtx`（`timeout`）专门给那条 exec 数据流用——到点就取消，让 `resp.Body` 上的读立刻解冻，而不是卡在一条不再吐数据的流上。外层 `ctx`（`timeout+30s`）覆盖"创建 exec 对象"和"读取最终退出码"，这两个必须在流超时**之后**还能跑，因为退出码要等流结束才查得到。若用同一个值，要么流不会准时解冻，要么查退出码时 ctx 已经死了。`internal/runtime/oci/backend.go:677` 还有一道保险：读循环每轮检查是否越过 `started.Add(timeout)`，覆盖"流上一直没数据、ctx 取消没能触发读"的快路径。

超时最终怎么"落到容器里"？答案在 `internal/runtime/oci/backend.go:642-645`：判定超时或出错后，控制面 best-effort 地向 Docker 发 `DELETE /exec/{id}`，杀掉那个还在跑的进程，避免它继续占着沙箱的 CPU 配额。

**为什么不是别的做法**：一个常见错误替代是"每种超时各写一个普通参数，不引入 context"。代价是取消信号无法穿透中间层——每层都得显式传参，新增一层就要改一遍签名。context 的价值不在"能带超时值"，而在"让取消这个横切关注点可以隐式穿透"。反过来，全项目只用 `WithValue` 放了一个值：认证后的 API key（`internal/httpapi/server.go:421` 的 `withKey`），恢复时用自定义键读出（`internal/httpapi/server.go:425` 的 `keyFrom`）；请求 ID 走响应头（`internal/httpapi/server.go:176`），没进 context。这是有意的收敛。

**怎么验证**：`go test -run TestExecRejectsMaxOutputAboveThePlatformCeiling ./internal/service/` 验证准入侧边界（`internal/service/exec_ceiling_test.go:18`）。要观察取消传播本身，看 `internal/service/lifecycle.go` 的 `requireRunning`：`ctx` 已取消时它不把请求变成"未知错误"，而是通过 `errFromCtx` 返回带原因的错误（`internal/service/service.go:379`）。判据是：客户端断开时应返回"取消/超时"类错误，而非内部 500。

## 3.6 并发安全的共享状态：锁还是传所有权

**问题**：`store` 的内存索引同时被请求路径、对账循环、TTL 清扫器访问，是全项目竞争最激烈的数据结构。怎么保证它不出错？有两条思路：用锁把访问串行化，或把数据的**所有权**在 goroutine 之间传递，保证同一时刻只有一个 goroutine 能碰它。本项目两处各用了其一。

**原理**：锁的思路是"数据共享，但访问排队"，前提是**同一块数据被多方长期持有**。传所有权的思路来自"不要通过共享内存来通信，而要通过通信来共享内存"，前提是数据能在一段时间内**只属于一个 goroutine**——channel 就是它的载体：把指针发到 channel 上，语义即"我交给你了，我不再碰"。

取舍很清楚。锁对"多方长期读写同一张表"是唯一可行方案，因为 store 的索引每个请求都要读，无法被独占。传所有权在任务流水线里更好，因为它从根本上消除了忘记加锁的可能。

**本项目怎么做**：先看 store 的锁（`internal/store/store.go:64-87`）：一把 `sync.RWMutex`（`internal/store/store.go:65`），配六张 map（`internal/store/store.go:80-86`）加一个切片。并发安全由三条约定构成：

第一，**读走读锁、写走写锁，且写必须在同一把锁里完成"落盘 + 发布"**。以 `PutTenant` 为例，`internal/store/store.go:207-212`：

```go
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked(fkTenant, "put", t.ID, t); err != nil {
		return err
	}
	s.tenants[t.ID] = cloneTenant(t)
```

`appendLocked`（`internal/store/wal.go:36`）的名字里就带着约束——它**必须**在写锁内调用。注释 `internal/store/wal.go:26-32` 说明：磁盘帧顺序与内存发布顺序必须落在同一临界区，否则两者可交错，崩溃时会重放出一段进程从未服务过的历史。这段历史在 v1.1.0 造成过一次真实的确认写丢失，`poc/walrace` 就是复现它写的（帧格式与 fsync 见 §9）。**这里要记住的并发不变量是"落盘与发布同锁"。**

第二，**进出 store 的每个对象都要克隆**。`GetTenant`（`internal/store/store.go:216`）在读锁内取到指针后返回 `cloneTenant(t)`；`UpdateSandbox`（`internal/store/store.go:407`）在写锁内先 `cloneSandbox(cur)`，把副本交给回调改，改完再发布。为什么必须克隆？若 store 把手中指针直接交给调用者，调用者随时能改字段，等于绕过锁、也绕过落盘。注释 `internal/store/store.go:26-29` 把这条列为不变量："store 既不保留调用方的指针，也不把自己的指针交出去"。锁管访问权，克隆管副本安全。

第三，**遍历时不许保留指针**。`ForEachSandbox`（`internal/store/store.go:466`）在读锁内逐个回调，注释要求 `fn` 不得保留传入的指针：读锁一释放，那个对象就可能被写者替换。清扫器和收敛器（`internal/service/reconciler.go:72`、`internal/service/reconciler.go:130`）都把值收集进本地切片，出锁后再处理。

再看"传所有权"的那一处，很小但很清楚：`createSem`（`internal/service/service.go:51`）不为保护数据，只为限制**同时在跑的创建数**。`provision` 先往 channel 放一个记号才获得"干活的权利"，干完 `<-s.createSem` 还回去（`internal/service/service.go:283-289`）。这里没有锁去数"当前有几个创建在跑"——**计数器的所有权在 channel 手里**：channel 本身成为那个受约束的资源。令牌桶算法见 §11。

**为什么不是别的做法**：一个替代是换成 `sync.Map`。它只优化"键稳定、读远多于写"这一种场景，而 store 写路径同样密集，且每次写要和落盘绑定在同一临界区里——这是 `sync.Map` 表达不了的，它不给你"把 map 写入和一串操作锁在一起"的能力。另一个替代是"不要内存索引，每次查询都读磁盘"，那会把对账循环变成磁盘扫描。

并发写冲突如何检出：`UpdateSandbox` 在写锁内比较调用方读到的版本与当前版本，不一致即返回冲突（`internal/store/store.go:418-420`），对外映射为 409。CAS 的存储层语义（`BaseVersion`、409 判定边界）属 §10，这里只交代"并发写由锁串行、由版本比较识别冲突"。

**怎么验证**：`go test -race -count=1 ./internal/store/`。其中的 `TestAcknowledgedWritesSurviveRestartUnderConcurrentCompaction`（`internal/store/regression_test.go:163`）让若干 `PutTenant` 与一次 `Compact()` 并发，再关闭、重开 store，断言**所有返回成功的写入在重启后仍在**。预期：通过，零丢失。对照：`CHANGELOG.md:373` 记录修复前实测 2880 次写入全部返回成功、重启后丢 120 条（4.2%）。现在还有一道静态守卫 `TestAppendAlwaysUnderWriteLock`（`internal/store/regression_test.go:106`），用 AST 扫描断言每个 `appendLocked` 调用点都在写锁内。

## 3.7 竞态检测：`-race` 能抓什么，抓不到什么

**问题**：一个竞态是"两个 goroutine 无同步地访问同一内存，且至少一个是写"。它有两条恼人的性质：大部分时候"看起来正常"，只在特定调度下出错；出错表现可能是数据错、可能崩，也可能什么都不发生。所以靠"多跑几遍没出事"来判断有没有竞态是不可靠的。`-race` 就是为这个不确定性准备的。

**原理**：`go test -race` 打开 Go 的竞态检测器。它在编译时给每次内存访问插桩，运行期维护"哪个 goroutine、在哪个同步关系下访问了这个地址"的元数据。发现两个访问之间**没有任何同步关系**（没经过同一把锁、同一个 channel）且至少一个是写，它就报 `DATA RACE`，并打印两个访问各自的调用栈与 goroutine 创建栈——**报出的是"这里可能出错"，即使这次运行恰好没出错**。

它抓不到的东西同样重要：只检查**这次运行真正发生过的访问**，没跑到的路径看不到；对"逻辑竞态"无能为力——两个操作都正确加锁、但顺序错导致业务结果错，这要靠断言而不是探针；它也无法证明"没有竞态"，只能证明"这次没发现"。所以 `-race` 必要但不充分，必须配针对性的并发断言。

**本项目怎么做**：仓库里有一个专门的 PoC，值得单独读，`poc/walrace/main.go`。它复现的正是 3.6 节那个历史缺陷：`Put*` 序列在锁外先写盘、再锁内改内存，于是 `Compact()` 可以在"帧已落盘、内存还没更新"的窗口里抢到锁，把这条记录连同 WAL 一起清掉，调用方却已拿到成功返回。程序结构是"一轮 `Compact` 与 24 次 `PutTenant` 并发，然后关闭重开、检查哪些曾返回成功的记录不见了"（`poc/walrace/main.go:56-88`），循环 120 轮，最后打印丢失率。

这个 PoC 为什么值得单独写一个程序，而不是写成一个 test？因为它的价值在于**能被人独立运行、独立观察**。`poc/walrace/main.go:16-21` 的注释写明："因为只有一次 `Compact`、且它是最后一次落盘动作，所以不可能被后续压实治愈"——这是在设计一个**不可自愈的复现窗口**。能复现，缺陷才是事实而不是推断；能被别人复现，这个事实才能被核对。这正是全项目"可核对性"方法论的一个样本（见 §16）。

还要区分两件常被混淆的事：`poc/walrace` 复现的是**丢失已确认写入**这个业务后果，`-race` 报的是**无同步访问**这个机制问题。前者可能不触发 `-race`（若两个访问恰好都被锁保护，只是保护范围错了），后者也不必然产生业务错误。

**为什么不是别的做法**：一个替代是只依赖 `-race`，把它当唯一防线。它抓不到逻辑错误，也抓不到"结构性约定被违反、却恰好没被并发访问到"的情况，所以本项目保留 `poc/walrace` 这类端到端复现与 AST 静态扫描作为补充——**三种手段覆盖三个不同的盲区**：运行期探针抓并发访问，静态扫描抓"结构性约定被违反"，端到端断言抓业务后果。

**怎么验证**：`go run ./poc/walrace`。预期它打印总写入数、重启后丢失数、丢失率与复现轮次。在修复后的代码上，丢失数应为 0，程序以"本轮未复现"结束。全量竞态检查用 `make test`（即 `go test -race -count=1 ./internal/...`）。测试方法论见 §14。

## 3.8 反例：看起来对、实际有窗口的并发代码

**问题**：下面两段代码都常见，也都能通过"跑一遍看看"的检验，但各自有一个窗口。

反例一，锁外读值：

```go
type Cache struct {
	mu sync.Mutex
	m  map[string]string
}

func (c *Cache) Get(k string) string {
	if v, ok := c.m[k]; ok { // 错误：锁外读 map
		return v
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}
```

反例二，没有内存屏障的双重检查：

```go
var (
	mu  sync.Mutex
	cfg *Config
)

func Get() *Config {
	if cfg == nil { // 错误：无同步地读指针
		mu.Lock()
		defer mu.Unlock()
		if cfg == nil {
			cfg = load()
		}
	}
	return cfg
}
```

**原理**：反例一的错在"优化"——想用一次锁外读避开加锁，但这个读本身与别的 goroutine 的写构成竞态。map 在并发读写时不只可能读到旧值，而是可能触发运行时直接中止进程（3.1 节的 `fatal error`）。正确的直觉是：**只要一个地址可能被别的 goroutine 写，读它就必须处于同一套同步关系下**，没有"轻量地先看一眼"这回事。

反例二更隐蔽，它看起来有锁、还做了双重检查。问题在第一次 `if cfg == nil`：它在锁外无同步地读了 `cfg`。即使用锁保证了只有一个 goroutine 执行 `load()`，**其他 goroutine 读到的可能是"已被赋为非 nil、但指针指向的字段还没初始化完"的状态**——因为缺少同步时，编译器和 CPU 都允许重排：先让 `cfg` 变成非 nil，再填它指向的内容。要修就用 `sync.Once`（内部自带同步）或 `atomic.Pointer[Config]` 的 `Load`/`Store`（atomics 建立顺序）。**双重检查锁在没有原子变量或同步原语时是坏的。**

**本项目怎么做**：正确写法就在仓库里。`internal/httpapi/server.go:236-249` 的 `lastUsed` 刷新就是标准形态——先在锁内查 `lastUsedSeen`，命中就解锁返回，未命中才在解锁后去写库：

```go
		lastUsedMu.Lock()
		last, seen := lastUsedSeen[key.ID]
		now := time.Now()
		if !seen || now.Sub(last) > time.Minute {
			lastUsedSeen[key.ID] = now
			lastUsedMu.Unlock()
			_, _ = s.Svc.St.UpdateKey(key.ID, -1, func(k *domain.APIKey) error {
				ts := now.UTC()
				k.LastUsedAt = &ts
				return nil
			})
		} else {
			lastUsedMu.Unlock()
		}
```

注意结构：**判断和写 map 都在锁内，只有耗时的落库操作在锁外**——既保住正确性，又没把慢操作关进临界区。

二是"只初始化一次"用 `sync.Once`，不手写双重检查。`internal/service/service.go:69-76` 的 `Stop`：

```go
func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		// The limiter owns a sweeper goroutine; stopping only the service's
		// own loops would leave it running against a discarded bucket table.
		s.Limiter.Stop()
	})
}
```

用 `Once` 而不是 `if !closed { close(stopCh) }`：后者读 `closed` 是竞态，且 `close` 已关闭的 channel 会 panic，`Once` 一次解决两者。

**为什么不是别的做法**：有人会说"这段初始化在启动早期、还没有并发，所以不用同步"。这在"该函数只被启动路径调用一次"的假设下成立，但假设会失效——只要第二个调用点出现在并发热路径上，缺陷立刻成立，且没有编译期信号。**用 `Once` 的成本几乎为零，消除的却是一整类"将来有人多调了一次"的风险**，所以只要语义是"恰好一次"，就写 `Once`。

**怎么验证**：`go test -race -run TestStopIsIdempotent ./internal/gov/`。`internal/gov/limiter_regression_test.go:179-187` 连着调两次 `Stop()`，断言不 panic，且停止后的限流器仍正常限流。预期：通过。反例里那两段代码若放进 `-race` 环境跑并发测试，会被直接报出 `DATA RACE`。

---

### 本章检验

1. `internal/service/service.go:151` 的 `go s.provision(...)` 是全项目唯一的 fire-and-forget goroutine，它不接受 `stopCh` 广播。请说明：这个设计为什么可接受？如果它的两层约束（信号量超时、5 分钟 `ctx`）任意去掉一层，会出现什么样的"停不掉的 goroutine"？
2. 设 `store` 的内存索引是一张裸 map，两个写请求并发到达。请解释为什么 `internal/httpapi/server.go:157` 的 `recover` 中间件救不了它，并说明这与普通 `panic` 在运行时的处理上有何本质区别。
3. `waitForState`（`internal/service/service.go:359`）选择了"轮询 store"，而不是"订阅状态变更事件"。请从"顺序由谁保证"的角度对比两种方案各自把哪一类正确性责任放在哪里，并说明为什么本项目选了前者。
4. 反例二的 `Get()` 里明明有锁，为什么仍然是错的？请具体指出"非 nil 但字段未初始化"的可见性是如何产生的，以及 `sync.Once` 和 `atomic.Pointer` 各自靠什么机制消除了它。
5. **动手任务**：打开 `internal/runtime/oci/backend.go`，把 `Exec` 函数（第 569 行起）里出现的每一个 `ctx` 是"由谁派生、受谁的取消约束"列成一张小表——区分清楚哪些调用用 `execCtx`、哪些用 `ctx`（`timeout+30s`），并回答：如果把第 619 行的 `b.execStart(execCtx, ...)` 改成传入 `ctx`，会出现什么行为变化？把答案写成一段话。


# §4 Go 标准库实战：JSON、HTTP 服务端与测试

## 4.1 struct tag 与两个方向：字段名是契约的一部分

**问题。** 一个结构体字段叫 `TimeoutSeconds`，没有 tag。服务端编码出 `"TimeoutSeconds"`，客户端按 snake_case 读 `timeout_seconds`，读到零值后按默认 3600 秒继续跑——沙箱存活时间从来没按客户端要求设置过，双方却都认为事情办成了。反向也成立：有人把字段改名成 `Timeout`，编译与单测都通过，线上协议被一次纯内部重命名改掉了。

**原理。** `encoding/json` 用反射遍历结构体字段：只有**导出**字段参与；JSON 里的名字先取 tag 中逗号前的部分，没有 tag 就用 Go 字段名本身。这份映射**编码与解码共用**，所以 tag 既是输出契约也是输入契约；解码还有一处宽容行为：没有精确匹配时退化为**大小写不敏感**匹配。下面这段程序一次说清三件事（`json:"-"` 两个方向都丢、未导出字段被跳过、大写键仍能命中）：

```go
type A struct {
	TimeoutSeconds int    `json:"timeout_seconds"`
	Secret         string `json:"-"`
	hidden         string
	Note           string
}
blob, _ := json.Marshal(A{TimeoutSeconds: 5, Secret: "s", hidden: "h", Note: "n"})
fmt.Println(string(blob))
var back A
_ = json.Unmarshal([]byte(`{"TIMEOUT_SECONDS":9,"secret":"x","hidden":"y"}`), &back)
fmt.Printf("%+v\n", back)
```

预期输出：

```text
{"timeout_seconds":5,"Note":"n"}
{TimeoutSeconds:9 Secret: hidden: Note:}
```

`Note` 没有 tag，线上键就是 `Note`；`Secret` 的 `-` 让它连解码入口都没有，输入的 `"secret"` 被丢弃；`hidden` 首字母小写，两个方向都不存在；`TIMEOUT_SECONDS` 靠大小写不敏感匹配填中了 `TimeoutSeconds`。

写这个项目的人迟早会碰到一件事：你在 Go 结构体里把一个字段从 `Timeout` 改名成 `TimeoutSeconds`，什么都没坏，测试全绿——但线上客户端的解析逻辑全断了。原因不在 Go，在于**结构体字段名和线上 JSON 字段名本来是两回事，是 tag 把它们绑在一起**。

`encoding/json` 有两个对称的入口：`Marshal` 把 Go 值写成 JSON 字节，`Unmarshal` 把 JSON 字节读回 Go 值。没有 tag 时，字段名原样出现，首字母大写（`TimeoutSeconds` → `"TimeoutSeconds"`）。tag 让你把线上名字和 Go 名字解耦。语法是 `json:"名字,选项1,选项2"`，三个选项本章必须讲清：`-` 表示**两个方向都忽略该字段**；`omitempty` 在值为"空"时省略字段；`,string` 让一个数值字段以 JSON 字符串形式编解码（用于需要避免大整数精度损失、或对端只接受字符串的场合）。本项目没有使用 `,string`，理由在 4.3。

**本项目怎么做。** 所有对外结构体的 tag 都写在 `internal/domain/` 的类型定义上。看请求体与响应体各一例。HTTP 层为创建请求单独定义了一个入参结构（`internal/httpapi/handlers.go:144`）：

```go
type createSandboxBody struct {
	TemplateID string             `json:"template_id,omitempty"`
	SnapshotID string             `json:"snapshot_id,omitempty"`
	BaseImage  string             `json:"base_image,omitempty"`
	Spec       domain.SandboxSpec `json:"spec"`
	Metadata   map[string]string  `json:"metadata,omitempty"`
}
```

它把线上的 `template_id` 映射到 Go 的 `TemplateID`，于是 Go 的命名惯例和 API 惯例可以各走各的。响应方向则直接复用领域类型（`internal/domain/sandbox.go:214`），客户端看到的 `tenant_id`、`base_version` 全由这些 tag 决定。

最值得注意的是 `-` 这个选项，以及项目**刻意不用它**。`internal/domain/sandbox.go:6` 的包注释记着一笔旧账：

```go
// previous iteration used json:"-" on crash-critical fields and silently
// lost them on restart; that bug class is designed out here.
```

上一版有人为了"这个字段不想暴露给客户端"给它加上了 `json:"-"`，结果它同时被排除出了持久化——重启后字段归零，而且**没有任何报错**（Unmarshal 见不到这个字段，自然也不会报缺字段）。项目的应对是把"不想暴露的东西"从根上隔离：API Key 的明文从来不进入 `domain` 包，库里只有哈希（`internal/domain/tenant.go:128`），所以不需要靠 `-` 去遮谁。

**替代方案**：如果不用 JSON 而用 `gob` 之类的格式，字段名就不再是对外契约，跨语言、跨版本就无法协商。JSON tag 的"烦人"恰恰是它最有价值的地方——它把字段名变成一份显式、可评审、改名就会炸的契约。

**怎么验证**：`go test ./internal/store/ -run TestReopenRecoversState`（`internal/store/store_test.go:41`）。这个用例写入一个沙箱、关掉存储、重新打开，断言 `Spec.MemoryMiB` 仍然是 256。它之所以能通过，前提就是每个字段的 tag 都让它在 JSON 里真实往返了一次。把 `MemoryMiB` 的 tag 改成 `json:"-"`，这个测试会立刻红。

## 4.2 `omitempty` 的坑：零值、指针与 `time.Time`

**问题。** 设置页上有个"允许出口网络"的开关。服务端建模成 `EgressAllowed bool` 并加了 `omitempty`，于是用户取消勾选时 `false` 被省略，请求体里没有这个键；服务端按"键不存在则继承模板默认"处理，而默认恰好是允许——用户保存后刷新，开关又变回勾选，而且**永远关不掉**。`false` 与"没这个键"在 JSON 上长得一模一样。

**原理。** `omitempty` 的判定是 `encoding/json` 里的一个固定函数：对 `bool`、数值、字符串判零值，对指针与接口判 `nil`，对 array/slice/map 判长度是否为 0，**其余类型一律判为"非空"，永不省略**。它只作用于**编码**方向；解码时"键缺失"与"键存在但为零值"都留下一个零值，Go 无从区分。实测：

```go
type B struct {
	Flag   bool          `json:"flag,omitempty"`
	Count  int           `json:"count,omitempty"`
	When   time.Time     `json:"when,omitempty"`
	Ptr    *int          `json:"ptr,omitempty"`
	Nested struct{ X int } `json:"nested,omitempty"`
}
blob, _ := json.Marshal(B{})
fmt.Println(string(blob))
```

预期输出：

```text
{"when":"0001-01-01T00:00:00Z","nested":{"X":0}}
```

`flag`、`count`、`ptr` 消失（false、0、nil），`when` 与 `nested` 不属于"可判空"的类型，被原样输出。**所以"能不能省略"由字段类型决定，不是由意图决定**；表达"有/无"只能改用指针，或让调用方把零值解释成"未设置"。

先问一个具体问题：一个沙箱的 `disk_mib` 是多少？如果客户端收到 `{}`，它无法回答。`omitempty` 的语义是"值为空就不输出"，而"空"对 Go 来说是一个固定的短名单：`false`、数值 `0`、`nil` 指针、`nil` 接口、长度为 0 的字符串/数组/切片/映射。**一个字段被省略，和它的值是零，在 JSON 上长得一模一样**——客户端因此分不清"服务端没有这个概念"和"服务端认为它是 0（或空串、或 false）"。

这不是理论问题。设想一个响应里有 `"retryable": false`：如果 tag 是 `json:"retryable,omitempty"`，那么"不可重试"和"没这个字段"就都消失成 `{}`，而调用方的重试决策正是看这个字段。**能省略的只有那些"零值本身就是合法、且与缺失等价"的字段。**

**本项目怎么做。** 取舍写在 spec 的 tag 上（`internal/domain/sandbox.go:128`）：

```go
type SandboxSpec struct {
	CPUMillicores  int `json:"cpu_millicores"`
	MemoryMiB      int `json:"memory_mib"`
	DiskMiB        int `json:"disk_mib,omitempty"`
	TimeoutSeconds int `json:"timeout_seconds"`
	// ...
	IdleTimeoutSeconds int               `json:"idle_timeout_seconds,omitempty"`
	Egress             EgressPolicy      `json:"egress"`
	Env                map[string]string `json:"env,omitempty"`
	User               string            `json:"user,omitempty"`
	WorkDir            string            `json:"workdir,omitempty"`
}
```

分两类看。`cpu_millicores`、`memory_mib`、`timeout_seconds` **不加** `omitempty`：它们的零值是非法值（`Validate` 会拒绝小于 100 的 CPU、小于 64 的内存、小于 30 的 TTL），而且响应里必须永远可见，客户端才能据它计算配额。`disk_mib`、`idle_timeout_seconds`、`env`、`user`、`workdir` **加** `omitempty`：它们的零值就是"未指定，走默认"，省略掉语义不丢。

那"存在性有语义"的字段怎么办？答案是**指针**。`omitempty` 对指针只在 `nil` 时生效，所以 `*time.Time` 能同时表达"没有这个值"和"这个值恰好是零时刻"（`internal/domain/tenant.go:130`）：

```go
ExpiresAt   *time.Time `json:"expires_at,omitempty"`
RevokedAt   *time.Time `json:"revoked_at,omitempty"`
LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
```

`nil` 表示"永不过期 / 未吊销 / 从未使用"，非 `nil` 一定被序列化。这就补上了 `omitempty` 会丢掉的信息。

最后是一个必须知道的陷阱，它就长在本项目里：`time.Time` 是一个 struct，而 `omitempty` 对 struct 不生效（struct 永远不算"空"）。看 `internal/domain/sandbox.go:230`：

```go
LastActivityAt time.Time `json:"last_activity_at,omitempty"`
```

这个 `omitempty` 是**死代码**：`LastActivityAt` 为零时刻时，字段照样输出成 `"last_activity_at":"0001-01-01T00:00:00Z"`。注释里写明了设计意图——"zero means no activity recorded since creation，清扫器改用 `CreatedAt`"——所以正确性不依赖省略，而是依赖客户端把零时刻当"无记录"。**教训是：在 `time.Time` 上写 `omitempty` 表达不了"可选时间"，要么改用 `*time.Time`，要么让调用方自己解释零值。**

**替代方案**：把每个字段都做成指针（`*int`、`*string`），存在性表达最精确，但一个 50 多字段的结构体会立刻变得难用，而且每处取值都要判空。本项目的选择是**只在存在性真正有语义的字段上用指针**，其余靠"零值即默认"的约定。

**怎么验证**：`go test ./tests/ -run 'TestHTTPContract/unknown_json_fields_are_refused'` 走一遍编解码路径；再 `curl -s .../v1/sandboxes/<id> | jq '.spec | keys'`，确认 `disk_mib` 在未指定时缺席、而 `cpu_millicores` 与 `memory_mib` 始终出现。

## 4.3 数字精度、未知字段与流式解码

**问题。** 客户端把创建沙箱的请求拼错了一个键——`timeout_second`，少一个 `s`。服务端不认识这个键也不报错，`TimeoutSeconds` 取到默认 3600 秒，客户端收到 201，以为自己要求的 30 秒生效了。直到某天沙箱早该被回收却还活着、占着租户配额，才有人去逐字节对请求体。

**原理。** `Unmarshal` 是**按目标结构体的字段驱动**的：它遍历输入的键，在结构体里找同名（或大小写不敏感）字段，找不到的键跳过、不算错误——"读别人写的 JSON"时多一个键本就不该致命。`DisallowUnknownFields` 是 `json.Decoder` 上的开关，打开后解码器记下"哪些输入键没命中任何字段"并在结束时报错；**包级函数 `json.Unmarshal` 没有这个开关**，这就是本项目必须自己写 `decode()` 的原因。实测差别：

```go
var c struct{ A int `json:"a"` }
dec := json.NewDecoder(strings.NewReader(`{"a":1,"timeout_second":30}`))
dec.DisallowUnknownFields()
fmt.Println(dec.Decode(&c))
fmt.Println(json.Unmarshal([]byte(`{"a":1,"timeout_second":30}`), &c))
```

预期输出：

```text
json: unknown field "timeout_second"
<nil>
```

精度那一半的机制在**目标类型**上：

```go
var m map[string]any
_ = json.Unmarshal([]byte(`{"id":9007199254740993}`), &m)
fmt.Printf("%v (%T)\n", m["id"], m["id"])
var t struct{ ID int64 `json:"id"` }
_ = json.Unmarshal([]byte(`{"id":9007199254740993}`), &t)
fmt.Printf("%d\n", t.ID)
```

预期输出：

```text
9.007199254740992e+15 (float64)
9007199254740993
```

三个坑，都是"能跑但会错"的类型。

**坑一：JSON number 与 `float64` 失真。** JSON 不区分整数和浮点，解析器默认把所有 number 读成 `float64`，而 `float64` 只有 53 位尾数能精确表示整数，超过约 9×10^15 的大整数（雪花 ID、纳秒时间戳、字节计数）就会悄悄变值。破解办法有两个：把目标字段声明成 `int64`（`encoding/json` 会按目标类型精确解析），或者用 `json.Decoder.UseNumber()`，让数字保持为字符串形态的 `json.Number`，由你自己决定何时转成整型。

本项目**没有**用 `UseNumber`，因为它解码时几乎总是给出带类型的结构体。凡是会参与比较、求和、CAS 的数字都是 `int64`，不是浮点数：`Version`、`BaseVersion`、`SizeBytes`、`MaxSnapshotBytes`（`internal/domain/`）。唯一用 `float64` 的是速率，`internal/domain/tenant.go:19`：

```go
type Rate struct {
	PerSecond float64 `json:"per_second"`
	Burst     int     `json:"burst"`
}
```

这是**故意的**：令牌桶（算法见 §11）的补充速率本来就允许是小数（快照组默认 0.5/s），而速率不需要"精确到最后一个比特"，浮点的误差无关紧要。**规则是：需要精确的用 `int64`，允许近似的用 `float64`，绝不让一个整型语义的值落到 `any` 里。** 反过来，如果你把响应解成 `map[string]any`，所有数字都会变成 `float64`——CLI 里对 `map[string]any` 的用法只碰字符串字段（`cmd/sbxctl/main.go:821` 的事件输出），碰数字的地方一律用带类型 struct（`cmd/sbxctl/main.go:333` 的沙箱列表），这就是为避开这个坑。

**坑二：未知字段默认被静默忽略。** `Unmarshal` 遇到结构体里没有的 JSON 键，默认丢掉不报错。对配置文件这种"多写一个键无害"的场景可以接受；对 API 请求体则是灾难——客户端把 `timeout_second`（少个 s）发过来，服务端默默用了默认值，客户端以为设置生效了。所以本项目的解码器**显式开启严格模式**（`internal/httpapi/server.go:454`）：

```go
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apperr.Invalid("invalid JSON body: %v", err)
	}
	return nil
}
```

`DisallowUnknownFields` 让拼错的键变成一个明确的 `invalid_request`（400）。**什么时候该开**：所有面向客户的请求体都该开；只有"向前兼容的读"——旧客户端读新服务端响应、或读第三方数据——才该关，否则对方新增一个字段你就会解析失败。

**坑三：一次性 `ReadAll` 会把整个请求读进内存再解析。** 流式的写法是 `json.NewDecoder(r.Body)` 直接从请求流增量解码，不需要先把 body 全部缓冲；输出方向同理，`json.NewEncoder(w).Encode(v)` 把结果直接编进 `ResponseWriter`，不需要先 `Marshal` 成一整块字节。本项目的读写两条路径都是流式的（`internal/httpapi/server.go:448` 的 `writeJSON` 用 `json.NewEncoder(w).Encode(v)`，`438` 的错误响应也用它，`454` 的解码器直接吃 `r.Body`）。**这不只是省内存**：它让"请求体上限"能真正生效——解码器读到超限处就被打断，而不是先把 2 GB 读进堆里再说（见 4.4）。

**本项目怎么做。** `internal/httpapi/server.go:454` 的 `decode` 是全站唯一的解码入口，所有读 body 的 handler 都调它，所以"严格模式"开一次就覆盖全部端点；`internal/httpapi/handlers.go:159` 的 `if r.ContentLength != 0` 让"客户端没发 body"的创建请求走默认 spec，而不是报一个 JSON 语法错误——`ContentLength` 为 0 时谈不上解码。

唯一没有走流式的是文件上传的原始分支（`internal/httpapi/handlers.go:381`）：

```go
data, err := io.ReadAll(r.Body)
if err != nil {
	s.writeError(w, r, apperr.Invalid("read upload: %v", err))
	return
}
content = data
```

它把整个 body 读进内存，看似正是坑三警告的写法，但成立的**前提**是 `bodyLimit` 中间件已把 `r.Body` 换成带上限的读端（见 4.4）：`ReadAll` 能拿到的字节数在到达这里之前就被截断，超限只得到一个 error 并被转成 `invalid_request`。输出方向统一走 `internal/httpapi/server.go:448` 的 `writeJSON`，直接编进 `ResponseWriter`。

**替代方案**：可以给每个请求强制 `io.LimitReader` 再 `ReadAll` 成一个 `[]byte`，然后 `Unmarshal`。问题是它把"限制"和"解码"分成了两步，中间那步就是内存放大的窗口；而且多了一次全量拷贝。流式解码把两者合并成一个动作。

**怎么验证**：`go test ./tests/ -run 'TestHTTPContract/malformed_json_is_a_governed_400'`（`tests/contract_test.go:135`）确认坏 JSON 得到的是受治理的 400，而不是 panic 或 500。

## 4.4 反序列化攻击面：准入处把内存放大砍掉

**问题。** 一条 40 字节的请求体：`{"argv":["yes"],"max_output":2000000000}`。服务端按这个数字去准备 stdout 与 stderr 的捕获缓冲，几十字节的请求换走控制面几百 MiB 的堆。**分配量由请求内容决定，这就是内存放大**——攻击者不需要漏洞，只需要一个被你当成"偏好"来读的数字。

**原理。** `http.MaxBytesReader(w, r.Body, limit)` 在内部累计已读字节数，超过 limit 时返回 `http: request body too large`；同时——这一条容易忽略——它通过传入的 `ResponseWriter` 把响应标记成"回复后关闭连接"（标准库 `response.requestTooLarge()` 会在头未写出时把 `Connection` 置为 `close`）。也就是说超限**不是一个能优雅重试的错误码，它连连接一起结束**。实测：服务端 `io.ReadAll` 读到 16 字节后报错，handler 仍能写 400，但连接随后被关闭。

反序列化不是"纯计算"，它会分配内存，而**分配多少，在很多时候由请求自己决定**。攻击者不需要漏洞，只需要一个字段：你写了 `max_output int`，他发 `max_output: 2000000000`，你的控制面就老老实实准备 2 GB 的缓冲。这叫内存放大——一次几十字节的请求换你几百 MiB 的堆。

**本项目在准入处设了两道闸。**

第一道是**请求体字节上限**。`internal/httpapi/server.go:209` 的中间件把每个请求的 body 换成一个受约束的读端：

```go
func (s *Server) bodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := s.Cfg.MaxRequestBody
		if strings.HasSuffix(r.URL.Path, "/files") && r.Method == http.MethodPut {
			limit = s.Cfg.MaxFileBytes + 4096 // raw upload path allows large bodies
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}
```

`http.MaxBytesReader(w, r.Body, limit)` 把原始的 body 包成一个"读到第 limit+1 字节就报错"的读端，默认上限是 1 MiB（`internal/config/config.go:214`），只有原始文件上传路由例外。注意它装在**认证之前**（`internal/httpapi/server.go:127` 的 `chain`），所以一个还没通过认证的请求也无法用超大 body 撑爆内存；这条链为什么这么排见 §12，本节只讲机制：**上限必须装在读到 body 之前，所以它天然是一个中间件。**

第二道是**请求字段的数值上限**，专门堵那个"数字字段决定分配量"的口子。exec 请求里有个 `max_output`（`internal/httpapi/handlers.go:315`），它被当成**上限**而非偏好来校验（`internal/service/lifecycle.go:235`）：

```go
// max_output is a CEILING, not a preference. The control plane buffers the
// whole stdout+stderr in its own heap before trimming, so without this
// check one request could ask for 2 GB and make the daemon allocate
// hundreds of MiB of shared memory (poc/probe_live.sh, PoC-4). A negative
// value is rejected rather than treated as "default": the JSON field is
// int, so a client error would otherwise silently become a large unsigned
// cast downstream.
switch {
case req.MaxOutput < 0:
	return nil, apperr.Invalid("exec max_output must not be negative")
case int64(req.MaxOutput) > s.Cfg.MaxExecOutput:
	return nil, apperr.Invalid("exec max_output exceeds the platform maximum of %d bytes (0 selects the %s backend default of %d bytes)",
		s.Cfg.MaxExecOutput, s.Backend.Name(), runtime.DefaultOutputCap)
}
```

语义在配置注释里写得很直白（`sandboxd.conf.example:74`）；“单次 exec 捕获的 stdout+stderr 字节数硬上限……控制面会把这些字节缓冲在自己的内存里，所以超过此值的请求在准入处直接被拒。0 表示后端默认（1 MiB），它**不**表示‘无限’。”设计文档 `docs/DESIGN.md:90`（§3.3）补了同一句话的另一半：无上限的请求字段等于"让人远程决定我们的内存占用"。注意这里还有一条细节——`max_output` 是 `int`，负值被**显式拒绝**而不是当成"用默认值"，因为不这样做的话，一个客户端错误会在下游被静默强转成一个巨大的无符号数。

**本项目怎么做。** 文件上传这条路径把这个机制用得很细。`internal/httpapi/server.go:212` 给原始上传路由单独放宽上限：

```go
if strings.HasSuffix(r.URL.Path, "/files") && r.Method == http.MethodPut {
	limit = s.Cfg.MaxFileBytes + 4096 // raw upload path allows large bodies
}
```

多出的 4096 是请求体开销余量，目的是让**恰好等于 `MaxFileBytes` 的合法文件不要撞上 `MaxBytesReader`**（撞上就是连接被关、客户端拿不到解释），而是完整地穿到 service 层，由这句判断裁决（`internal/service/lifecycle.go:266`）：

```go
if int64(len(content)) > s.Cfg.MaxFileBytes {
	return apperr.Quota("file exceeds the %d byte upload cap", s.Cfg.MaxFileBytes)
}
```

于是"文件太大"在两层后果不同：中间件层超限，连接被关、客户端只看到传输错误；service 层超限，是带错误码的 `Quota` 响应。顺序也不能倒——service 层检查时 `content` 已在堆里，只能把不该发生的请求变成清晰错误，真正拦住内存占用的是装在读之前的中间件（默认 1 MiB 见 `internal/config/config.go:214`，文件上限 100 MiB 见 `internal/config/config.go:215`）。

**替代方案**：可以在真正分配之前不做检查，靠 `runtime.MaxBackendOutput`（1 GiB，`internal/runtime/runtime.go:72`）在后端兜底。项目确实留了这道底，但注释写明它是"用来抓一个忘了执行 `config.MaxExecOutput` 的 service 层"——**兜底是给编程错误用的，不是给请求用的**。正确的顺序是：准入处用配置上限拒掉请求，后端用硬上限抓实现 bug。

**怎么验证**：`go test ./internal/service/ -run TestExecRejectsMaxOutputAboveThePlatformCeiling`（`internal/service/exec_ceiling_test.go:18`）。它断言恰好等于上限的请求被接受、上限加一被拒绝、负值被拒绝，并且用一个很高的临时上限去证明 `runtime.MaxBackendOutput` 这道底也真的会触发。

## 4.5 `Handler` 与 `ResponseWriter`：三条隐含规则

**问题。** 一个 handler 先 `fmt.Fprint(w, "body")`，然后才 `w.Header().Set("Content-Type", "application/json")`，最后 `w.WriteHeader(201)`。客户端收到状态码 200，`Content-Type` 被嗅探成 `text/plain; charset=utf-8`，服务端日志多一行 `http: superfluous response.WriteHeader call`。三行都"执行成功"，响应却不是你要的那个。

**原理。** `ResponseWriter` 不直接往 socket 写，它把响应头攒在一个 map 里；第一次 `Write` 会触发一次**隐式 `WriteHeader(200)`**——因为 HTTP 要求状态行必须先于 body 到达。那一刻 header map 被快照并刷出去，此后的 `Header().Set` 改的是一份没人再看的 map，此后的 `WriteHeader` 被丢弃并打日志。**所以那三条规则不是书写习惯，而是"一旦写出就不可逆"的推论。** 实测（必须用真服务器，recorder 的 `Header()` 是活 map）：

```go
srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "body-first")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
}))
resp, _ := http.Get(srv.URL)
fmt.Println(resp.StatusCode, resp.Header.Get("Content-Type"))
```

预期输出：

```text
200 text/plain; charset=utf-8
```

`net/http` 的服务端抽象很小：一个 `Handler` 是"能处理一次请求"的东西，接口只有一个方法 `ServeHTTP(w ResponseWriter, r *Request)`；`HandlerFunc` 是一个函数适配器，让普通函数也能当 `Handler`。整套东西只有三条规则，但**每一条违反都不会编译报错，只会让响应变得诡异**。

规则一：**先设 Header，再 `WriteHeader`，最后写 body。** `Header()` 返回的 map 必须在你调用 `WriteHeader`（或第一次 `Write`）之前改完，因为一旦状态行和响应头被写出，后面的 Header 修改就无效了。

规则二：**`WriteHeader` 只能生效一次。** 第二次调用会被忽略并打一条日志——响应码以第一次为准。

规则三：**不调 `WriteHeader` 直接 `Write`，隐含状态码是 200。** 很多人忘了这条，于是写出"我明明想返回 201，结果返回了 200"的 bug。

**本项目怎么做。** 错误响应函数把前两条规则写对了顺序（`internal/httpapi/server.go:429`）：

```go
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	// ...
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":       e.Code,
			"message":    e.Message,
			"retryable":  e.Retryable,
			"request_id": rid,
		},
	})
}
```

`Set` 在 `WriteHeader` 之前，`Encode` 在最后。顺序颠倒的话，`Content-Type` 会消失、状态码会从 `code` 变成 200。

规则三在本项目里有一个很有说服力的体现：访问日志中间件要记录真实状态码，于是它包了一个 `ResponseWriter` 记录状态（`internal/httpapi/server.go:199`）：

```go
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
```

初始化时**默认 `status: 200`**（`internal/httpapi/server.go:188` 的 `&statusWriter{ResponseWriter: w, status: 200}`）——这正是因为规则三：如果某个 handler 只 `Write` 而不 `WriteHeader`，真实状态就是 200，包装器必须复刻这个隐含行为，否则日志会把所有"只写不设码"的请求记成 0。

`Request` 侧常用的字段，本项目几乎每个都用了：`Method` 与 `URL`（访问日志）、`Header`（取 `Authorization` / `X-API-Key` / `X-Request-Id`）、`Body`（解码）、`Context()`（承载认证结果与取消信号）、`PathValue("id")`（路由参数，见 4.6）、`ContentLength`（`internal/httpapi/handlers.go:159` 用它判断"请求体是否为空，空则不解析"）、以及 `RemoteAddr`——注意 `clientIP`（`internal/httpapi/server.go:263`）**只读 `RemoteAddr`**，不读 `X-Forwarded-For`，因为后者在默认部署下是攻击者可以随便填的。

**替代方案**：很多框架提供 `Context` 对象的 `JSON(statusCode, obj)` 之类封装，把三条规则藏起来。好处是不容易写错，代价是你看不到 `WriteHeader` 到底被调了几次。本项目零依赖，直接用标准库，规则必须自己守住。

**怎么验证**：`go test ./internal/httpapi/ -run TestMetricsExposeRealLabelValues`（`internal/httpapi/regression_test.go:202`）。它先发一个 200 请求、再发一个 401 请求，然后断言 `/metrics` 里同时存在 `status="200"` 和 `status="401"` 两条计数——只有当 `statusWriter` 正确复刻了规则二和规则三，这两个标签才会出现。

## 4.6 路由：Go 1.22 的 `ServeMux` 怎么匹配

**问题。** Go 1.22 之前 `ServeMux` 只做前缀匹配，路径参数只能在 handler 里自己切。于是吊销密钥的端点变成 `parts := strings.Split(r.URL.Path, "/")` 再用 `parts[3]`、`parts[5]` 取 id：请求 `DELETE /v1/tenants` 时 `parts[5]` 越界 panic；请求 `DELETE /v1/tenants/tnt_1` 时取到 `tnt_1` 却被当 key id 用——**把租户 id 当密钥 id 解析，报错看起来还完全正常**。

**原理。** Go 1.22 的模式形如 `[METHOD ][HOST]/[PATH]`，路径里的 `{name}` 匹配**恰好一个**路径段（`{name...}` 只在末尾出现，吃掉剩余所有段），匹配到的文本由 `ServeMux` 塞进请求，handler 用 `r.PathValue("name")` 取回。三个细节：方法不匹配由 `ServeMux` 自己处理（路径命中但方法没注册时返回 405 并带 `Allow` 头）；优先级由模式的**具体程度**决定而非注册顺序；`{name}` 不吃斜杠，多一段就 404。实测：

```go
mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "id=%s", r.PathValue("id"))
})
mux.HandleFunc("GET /v1/sandboxes/{id}/files/list", func(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "list")
})
```

```text
GET  /v1/sandboxes/sbx_1            -> 200 "id=sbx_1"
GET  /v1/sandboxes/sbx_1/files/list -> 200 "list"
GET  /v1/sandboxes/a/b              -> 404 "404 page not found"
POST /v1/sandboxes/sbx_1            -> 405, Allow: GET, HEAD
```

第三行是 `{id}` 只吃一段的证据，第四行的 `Allow` 是 `ServeMux` 自己写的。

Go 1.22 之前，标准库的 `ServeMux` 只会按路径前缀匹配，方法和路径参数都得在 handler 里手动判断。1.22 起模式可以带**方法和通配段**：`"GET /v1/sandboxes/{id}"` 里的 `GET` 限定方法，`{id}` 匹配一个路径段并把值交给 `r.PathValue("id")`。

本项目的整张路由表就是这一节最好的教材（`internal/httpapi/server.go:54` 起）：

```go
mux.HandleFunc("GET /healthz", s.health)
mux.HandleFunc("GET /readyz", s.ready)
mux.Handle("GET /metrics", s.chainAdmin(s.handleMetrics))
mux.Handle("GET /v1/isolation", s.chainAdmin(s.isolationReport))
mux.Handle("POST /v1/sandboxes", s.chain(domain.GroupCreate, s.createSandbox))
mux.Handle("GET /v1/sandboxes", s.chain(domain.GroupRead, s.listSandboxes))
mux.Handle("GET /v1/sandboxes/{id}", s.chain(domain.GroupRead, s.getSandbox))
mux.Handle("GET /v1/sandboxes/{id}/files", s.chain(domain.GroupFiles, s.getFile))
mux.Handle("GET /v1/sandboxes/{id}/files/list", s.chain(domain.GroupFiles, s.listFiles))
mux.Handle("DELETE /v1/tenants/{id}/keys/{key_id}", s.chain(domain.GroupAdmin, s.revokeKey))
```

逐条说匹配规则。第一，`GET /healthz` 这样的模式**只**匹配 GET；一个 POST 到 `/healthz` 不会落到它身上，而是得到 405，这不是中间件做的，是 `ServeMux` 按已注册方法自动给的。第二，`{id}` 只匹配**一个**路径段，不吃斜杠，所以 `/v1/sandboxes/sbx_abc` 匹配、`/v1/sandboxes/sbx_abc/files` 不匹配前者而匹配后者的注册项。第三，模式越长越具体者优先：`/v1/sandboxes/{id}/files/list` 比 `/v1/sandboxes/{id}/files` 更具体，所以带 `/list` 的请求不会掉进 `getFile`。第四，参数可跨段多个：`/v1/tenants/{id}/keys/{key_id}` 里 `r.PathValue("id")` 和 `r.PathValue("key_id")` 各取一段。第五，没有方法限定的模式（本项目没用）会对所有方法生效。

**本项目怎么做**：每个受治理的端点被包成 `s.chain(操作组, handler)`，操作组（`domain.GroupCreate` 等）决定这个端点用哪个令牌桶（见 §11）；两个健康检查端点走裸 `HandleFunc`，因为它们不鉴权也不限流；`/metrics` 和 `/v1/isolation` 走 `s.chainAdmin`，被要求 admin 凭据（`internal/httpapi/server.go:64` 的注释解释了为什么——它们在 v1.1.0 曾用裸 `HandleFunc` 注册，绕过了整条中间件链，把全租户运维数据 200 给任何人）。

**替代方案**：可以用第三方路由（`chi`、`gorilla/mux`）拿到更复杂的匹配能力，代价是多一个依赖，而这个项目把"零第三方依赖"当成供应链承诺（见 §16）。对这张表里的模式，标准库 1.22 的能力已经够用。

**怎么验证**：`go test ./tests/ -run 'TestHTTPContract/wrong_methods_are_405'` 与 `'TestHTTPContract/unknown_routes_are_404'`（`tests/contract_test.go:112`、`:117`），一个证明方法限定生效，一个证明未知路由不会掉进某个通配模式。

## 4.7 中间件怎么写：闭包链

**问题。** 鉴权、限流、访问日志必须发生在**每一个**端点上，而业务 handler 对它们一无所知。只要"套上链"依赖程序员记得，就一定会漏——本项目漏过一次：`/metrics` 在 v1.1.0 里用裸 `HandleFunc` 注册，绕过整条链，把全租户运维数据 200 给了任何人（注释见 `internal/httpapi/server.go:60`）。**漏一个端点，中间件写得再对也没用。**

**原理。** 中间件是一个**接收 Handler、返回 Handler 的函数**，用闭包就能把"下一个"捕获进去。`http.HandlerFunc` 之所以能当 Handler 用，是因为它是在函数类型上定义了 `ServeHTTP` 方法的适配器——一个 `func(w, r)` 经一次类型转换就满足单方法接口，这就是"为什么中间件能写成普通函数而非结构体"。嵌套顺序即执行顺序：外层的"前"先跑，内层的"后"先跑完。

```go
func wrap(name string, next func()) func() {
	return func() {
		fmt.Println("before", name)
		next()
		fmt.Println("after", name)
	}
}
wrap("a", wrap("b", func() { fmt.Println("handler") }))()
```

预期输出：

```text
before a
before b
handler
after b
after a
```

中间件的本质是**函数式装饰器**：它接收一个 `http.Handler`，返回一个新的 `http.Handler`，新 handler 在调用原 handler 前后插入自己的逻辑。就这么简单，用闭包就能写。

一个最小的记日志中间件，不到 15 行：

```go
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r) // 先让内层处理
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
```

用法是把它包在目标外面：`http.ListenAndServe(":8080", logRequests(mux))`。如果把多个中间件串起来，就是 `a(b(c(handler)))`——**嵌套顺序决定执行顺序**，外层的"前置逻辑"先进、内层的"后置逻辑"后出。

**本项目怎么做。** 项目的 `chain` 就是把这个嵌套写出来的地方（`internal/httpapi/server.go:127`）：

```go
func (s *Server) chain(group string, h http.HandlerFunc) http.Handler {
	return s.accessLog(s.recover(s.requestID(
		s.preAuthLimit(
			s.bodyLimit(
				s.authenticate(
					s.rateLimit(group, h),
				),
			),
		),
	)))
}
```

看一个具体中间件的实现，`requestID`（`internal/httpapi/server.go:168`）：

```go
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" || len(id) > 64 {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r)
	})
}
```

它体现了两个模式：**签名固定**——永远 `(http.Handler) http.Handler`，所以任意两个中间件可以任意嵌套，不需要知道对方内部；**上下文用闭包携带**——`next` 是捕获进闭包的，不需要显式传参。`recover` 中间件（`internal/httpapi/server.go:157`）再补一个要点：它用 `defer recover()` 兜住内层 panic，把它转成 500，这也是中间件能做的、handler 自己做不了的全局事情。

**注意**：这些中间件**为什么按这个顺序排**（为什么 pre-auth 限流必须在认证外面、body-limit 在认证里还是外），是安全契约的一部分，归属 §12，本节只讲"怎么用闭包写出一个中间件"这个机制。

**替代方案**：可以定义一个中间件切片 `[]func(http.Handler) http.Handler`，循环反向包裹。好处是顺序变成数据、可以动态调整；代价是编译器不再保证类型，顺序错了连看代码都难发现。本项目选择显式嵌套，因为这句话是有意写给人看的。

**怎么验证**：`go test ./internal/httpapi/ -run TestRateLimitOverHTTP`（`internal/httpapi/httpapi_test.go:22`）。它通过 `srv.Handler().ServeHTTP` 打真实路由路径上的 50 个并发请求，断言其中出现 429——单个限流器单测覆盖不到"中间件被正确串进链路"，这个用例补的就是那段接线。

## 4.8 超时字段与请求体上限

**问题。** 攻击者开 2000 个连接，每个发完 `GET / HTTP/1.1\r\nHost: x\r\n` 就停住，每 30 秒补一个字节。服务端为每个连接起一个 goroutine 等它把头读完；头永远读不完，连接与 goroutine 就永不释放。几分钟后 `/healthz` 也连不上，负载均衡器判定实例死亡并摘掉它，下一个实例同样死法——整场攻击只花几 KB 流量。

**原理。** `ReadHeaderTimeout` 是在**读请求头之前**给连接设的一次读 deadline；头一读完它就被清掉，所以对"body 发得慢"完全不生效——这正是要它的理由。对比 `ReadTimeout`：它是从连接建立到读完整请求（含 body）的总时限，会把合法的大文件上传一起掐死。**头慢从来不合法，body 慢有时候合法**，所以只给前者设上限，用 body 字节上限管后者（见 4.4）：慢换不到大。实测（服务端 `ReadHeaderTimeout` 500ms，客户端只发半个头）：

```go
conn, _ := net.Dial("tcp", addr)
_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n"))
n, err := conn.Read(buf) // 期待约 500ms 后拿到 EOF
```

预期输出：

```text
read n=0 err=EOF
```

`http.Server` 有四个超时字段，名字很像，防的却是完全不同的事。分不清它们的代价是：要么把正常的长请求掐死，要么给攻击者留一条慢慢滴水把连接占满的路。

- `ReadHeaderTimeout`：从连接建立到**读完请求头**的时间上限。它专防 **slowloris**——攻击者开大量连接、每次只发几个字节的头、永远不发完，让服务端把连接和 goroutine 全占住。防它必须掐在"读头"这一步。
- `ReadTimeout`：从连接建立到**读完整个请求**（含 body）的上限。
- `WriteTimeout`：**写响应**的上限（对 HTTP/1.1，从读完请求头算起）。
- `IdleTimeout`：keep-alive 连接在两次请求之间的空闲上限，超时就关掉。它就是 keep-alive 的寿命。
- `MaxHeaderBytes`：请求头总字节上限，防止一个超大 header 直接吃掉内存。

**本项目怎么做。** 构造函数在 `cmd/sandboxd/main.go:109`：

```go
srv := &http.Server{
	Addr:              cfg.Listen,
	Handler:           api.Handler(),
	ReadHeaderTimeout: 10 * time.Second,
	// exec streams are long-lived; per-request contexts govern them
	WriteTimeout:   0,
	IdleTimeout:    120 * time.Second,
	MaxHeaderBytes: 1 << 20,
}
```

三处选择都值得读。设置 `ReadHeaderTimeout = 10s`：slowloris 被挡在读头这一步。设置 `WriteTimeout = 0`（不设）：这是**故意的**，注释说明了理由——exec 可能跑几十秒，一个固定的写超时会把合法的长输出直接砍断；替代手段是**每个请求自己的 context** 在更内层设时限，所以这里不能一刀切。`IdleTimeout = 120s` 让 keep-alive 连接有明确寿命，空闲连接不会永远挂着。注意本项目**没有**设 `ReadTimeout`：这意味着一个连上后慢慢发 body 的客户端不受这个字段约束，真正兜住它的是下面这句话——**body 有硬上限**，读超就报错，所以"慢"换不到"大"。

请求体上限由 `http.MaxBytesReader` 落在那条中间件里（4.4 已引，`internal/httpapi/server.go:215`），默认 1 MiB、文件上传路由放宽。

**替代方案**：可以在反向代理（nginx）层设超时，让 Go 服务端保持"无限"。问题是这样一来，超时行为就不在你自己的代码里、也不在你的测试里，换一个部署环境就变了。本项目把这几个字段钉在 `http.Server` 构造里，让它们随二进制一起被评审和测试。

**怎么验证**：`grep -n 'ReadHeaderTimeout\|IdleTimeout\|WriteTimeout' cmd/sandboxd/main.go` 核对生效值；行为侧则发一个超过限额的请求体，确认它在 `decode`（`internal/httpapi/server.go:454`）读 body 时被中断、映射成 `invalid_request`，而不是被完整读进内存。

## 4.9 优雅停机：`Shutdown` 与 `Close` 的区别

**问题。** systemd 重启服务时先发 `SIGTERM`。一个正在执行 30 秒长命令的 exec 请求被当场掐断，客户端拿到连接重置，唯一合理的动作就是重试——同一条命令被执行两次。另一头更麻烦：沙箱已在后端建好，但创建响应没发出去，客户端重试又建出第二个，第一个从此没人知道，成了孤儿（回收见 §10）。

**原理。** `Shutdown(ctx)` 做三步：先关掉所有监听器；再**循环轮询**已登记的连接，空闲的立即关、活跃的等着；直到活跃连接归零或 `ctx` 到期——到期时关掉剩余连接并返回 `ctx.Err()`。它**不**等 hijack 出去的连接。`Close()` 没有中间那一步，监听器和连接当场关。实测（handler 睡 300ms，停机时请求已在途）：

```text
9 shutdown err: <nil> after 300ms
9 client saw: body=done
```

`Shutdown` 阻塞 300 毫秒把在途请求等完，`ListenAndServe` 随后返回 `http.ErrServerClosed`——这个返回值必须被显式当成功处理，否则每次正常停机都会被记成致命错误。**为什么是轮询**：服务端只知道"活跃连接数"，无法知道某个 handler 还要跑多久，预算（本项目 20 秒）只能由人给定。

进程收到 `SIGTERM` 时，如果直接退出，正在处理的请求会被硬生生掐断，客户端看到连接重置——对一个控制面来说，这可能意味着"沙箱已经建好了，但创建响应没发出去"，客户端的重试就会建出第二个。

**`http.Server.Shutdown(ctx)` 做三件事**：停止接受新连接（监听器关掉）、等待**在途请求**处理完、等所有连接空闲或 `ctx` 到期后返回。`ctx` 到期时它返回错误，未完成的请求随之被放弃。`Close()` 完全不同：它**立即**关闭所有监听器和连接，在途请求当场断掉。

**本项目怎么做**（`cmd/sandboxd/main.go:141`）：

```go
shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
defer cancel()
_ = srv.Shutdown(shutdownCtx)
svc.Stop()
```

给它 20 秒的预算，然后才停后台循环（`svc.Stop()`）。监听服务本身是另起一个 goroutine 跑的（`cmd/sandboxd/main.go:120`），主 goroutine 在 `select` 里同时等信号和监听器错误——如果是正常关闭，`ListenAndServe` 会返回 `http.ErrServerClosed`，代码显式把它当成功处理（`cmd/sandboxd/main.go:136`），否则任何一次正常停机都会被记成致命错误。

**替代方案**：可以做**零停机重启**——在 `Shutdown` 之前先启动新进程并做连接交接，或者用 `Server.RegisterOnShutdown` 挂钩子。本项目是单机、单进程、systemd 托管，`Shutdown` + 20 秒预算已经足够，更复杂的交接留给多节点场景。这里只讲 `Shutdown` 这个 API 怎么用，它的运维含义（排空窗口多长合适、和 systemd 的配合）见 §13。

**怎么验证**：`go test ./tests/ -run 'TestCrashRecovery/a_graceful_restart_keeps_serving'`（`tests/resilience_test.go:68`），它让守护进程走一次优雅停机再在同一数据目录上重启，断言服务没断。

## 4.10 `httptest`：不监听端口的 handler 测试

**问题。** 起真服务器测 handler 会带来三件事。端口是全局资源：`go test ./...` 并发跑多个包时两个测试抢同一端口，一个随机失败、重跑又好了——直到某天 CI 红了没人能复现。其次要自己处理"监听还没起来"的等待，测试里于是出现 `time.Sleep`。最后，监听会穿过本机的代理与防火墙设置。

**原理。** `httptest.NewRequest` 直接构造 `*http.Request`（不走网络、不做 DNS），`httptest.NewRecorder` 返回一个实现了 `ResponseWriter` 的结构体，body 接到 `*bytes.Buffer` 上，结束后从 `rec.Code` 与 `rec.Body` 读结果。够用的原因是 `ServeHTTP` 的契约**只依赖这两个接口**，中间有没有 socket 对 handler 是透明的。但默认值不是零值，写断言时必须知道（实测）：

```text
RemoteAddr="192.0.2.1:1234" Host="example.com" ContentLength=0 TLS=<nil>
default recorder code: 200
```

`RemoteAddr` 是 `192.0.2.1:1234` 而不是空串——按 IP 分桶的限流器在 `httptest` 下只会分出一个桶，你以为在测"500 个不同来源"，其实只有一个。recorder 默认 200 也复刻了 4.5 的规则三。

要测一个 handler，最直接的办法是起一个真服务器、发真请求。问题是它慢、占端口、还和并发测试抢资源。`net/http/httptest` 解决的就是这个：`httptest.NewRequest` 造一个 `*http.Request`，`httptest.NewRecorder` 造一个假的 `ResponseWriter`，直接调用 `ServeHTTP`，全程不碰网络。

**本项目怎么做。** 内部测试用的正是这套（`internal/httpapi/regression_test.go:78`）：

```go
func (h *apiHarness) do(method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}
```

`h.srv.Handler()`（`internal/httpapi/server.go:52` 返回 `s.mux`）拿到的是**完整的中间件后的栈**，所以这一个调用同时跑通了路由、链路和 handler。`NewRecorder` 既是 `ResponseWriter`，又在结束后暴露 `rec.Code` 和 `rec.Body`，断言可以直接读。测试用它连发 400 个未认证请求，统计 429/401 的分布（`internal/httpapi/regression_test.go:96`）。

**替代方案**：真正需要跨进程行为（信号、端口、TLS、超时）时，`httptest` 就不够了，必须起真进程——`tests/` 包的 `Harness` 就是干这个的（`tests/harness_test.go:276`），它编译真二进制、分配空闲端口、等待 `/readyz`。两套并存是有意的：**能在进程内测的放 `internal/`，必须跨进程的放 `tests/`。**

**怎么验证**：`go test ./internal/httpapi/ -run TestUnauthenticatedFloodIsRateLimited`（`internal/httpapi/regression_test.go:96`），它整个跑在 `httptest` 上，不占端口、不依赖 Docker。

## 4.11 `testing`：表驱动与子测试

**问题。** 校验一份 spec 要覆盖 CPU 上下界、内存下界、TTL 下界、非法 env 键、带 `..` 的 workdir、以 `-` 开头的 user。写成十个 `TestSpecValidationCPULow` 这样的函数后，改一条断言要改十处；失败信息也只说"某个函数红了"，你得逐个读一遍才知道哪个输入越界。

**原理。** 表驱动把**数据**与**断言**分开：每行是"名字 + 输入 + 期望"，循环体只写一份。`t.Run` 再加两件东西：每个用例拿到自己的 `*testing.T`，失败互不牵连，配合 `t.Errorf`（而非 `t.Fatalf`）让坏行一次报全；名字参与 `-run` 匹配，匹配**按 `/` 分层**，名字里的空格还会被改写成下划线（实测：`-run 'TestTable/cpu_too_low'` 命中 `cpu too low`）：

```go
for _, tc := range []struct {
	name string
	v    int
}{{"cpu too low", -1}, {"ok", 5}} {
	t.Run(tc.name, func(t *testing.T) {
		if tc.v <= 0 {
			t.Errorf("%s: want positive", tc.name)
		}
	})
}
```

```bash
go test -run 'TestTable/cpu_too_low' -v
```

预期只有 `=== RUN TestTable/cpu_too_low` 被运行。

一组输入对应一组期望输出，是测试里最常见的形状。如果写成十来个几乎一样、只差数据的用例，改一个断言要改十处——于是有了**表驱动**：把用例写成一张表，循环遍历。

**本项目怎么做。** `internal/domain/domain_test.go:85` 的 `TestSpecValidation` 是一张典型的表：

```go
cases := []struct {
	name string
	spec SandboxSpec
	ok   bool
}{
	{"ok", SandboxSpec{CPUMillicores: 500, MemoryMiB: 256, TimeoutSeconds: 3600}, true},
	{"cpu too low", SandboxSpec{CPUMillicores: 50, MemoryMiB: 256, TimeoutSeconds: 3600}, false},
	{"cpu too high", SandboxSpec{CPUMillicores: 64000, MemoryMiB: 256, TimeoutSeconds: 3600}, false},
	{"mem too low", SandboxSpec{CPUMillicores: 500, MemoryMiB: 16, TimeoutSeconds: 3600}, false},
	// ...
}
for _, tc := range cases {
	err := tc.spec.Validate(testLimits, 0)
	if tc.ok != (err == nil) {
		t.Errorf("%s: err=%v want ok=%v", tc.name, err, tc.ok)
	}
}
```

每一行的 `name` 是失败的诊断线索，`spec` 是输入，`ok` 是期望。断言用 `t.Errorf` 而不是 `t.Fatalf`：**一个用例失败不该终止整张表**，让所有坏行一次报全。这条表覆盖了 CPU 上下界、内存下界、TTL 下界、非法 env 键、带 `..` 的 workdir、以 `-` 开头的 user——每一个都是曾经真实会导致越权或资源失控的输入。

**子测试**是表驱动的升级：把每个用例包进 `t.Run(tc.name, ...)`。好处有三个——失败信息自动带上用例名、`go test -run 'TestX/absurd'` 能**只跑一个用例**、子测试可以各自 `t.Parallel()`。`internal/config/config_test.go:33` 就是这么写的：

```go
for _, tc := range []struct {
	name string
	v    int64
	ok   bool
}{
	// ... (zero / negative / absurd / tiny / reasonable 五个取值)
} {
	t.Run(tc.name, func(t *testing.T) {
		c := Default()
		c.MaxExecOutput = tc.v
		err := c.validate()
		// ...
	})
}
```

端到端套件里也用同一手法给每个场景取名（`tests/governance_test.go:73`），于是 `-run 'TestQuotaAndAdmission/specs_outside_platform_limits_are_rejected_with_a_reason/cpu_below_the_floor'` 能精确命中一行。

**替代方案**：也可以用 `t.Run` 之外的方式给用例加标签再过滤，但那要自己实现过滤逻辑；`t.Run` 的命名 + `-run` 是标准库白送的。

**怎么验证**：`go test ./internal/config/ -run 'TestMaxExecOutputHasUsableBounds/absurd' -v`，你会看到只有一个子测试跑起来并通过——这就是子测试的价值。

## 4.12 `testing`：`t.Helper`、`t.Cleanup`、`t.TempDir`、`t.Parallel`

**问题。** 把断言抽进辅助函数 `mustStatus(t, rec, 201)` 之后，失败信息报的是辅助函数自己那一行（`mustStatus` 内部的第 11 行），而不是两百个调用点里出错的那一个。这张表有两百个调用点，你得逐个猜是谁传错了期望值。**抽函数省了重复，却把"哪里错了"一起抽掉了。**

**原理。** `t.Helper()` 只做一件事：调用它的那一帧的**程序计数器**被记进该测试的"helper 帧"集合（通过 `runtime.Callers` 取 PC），格式化失败信息时 testing 从栈顶往下找第一个不在集合里的帧作为报告位置。所以它的语义是"**这一帧对读者没有信息量**"，只对直接调用它的那一帧生效。实测（调用点第 27 行，断言在辅助函数第 11 行）：

```text
x_test.go:27: want positive, got 0
```

`t.Cleanup` 与 `defer` 的差别同样是**作用域**：`defer` 绑在函数上，`t.Cleanup` 绑在测试上，子测试结束时先跑自己的回调（实测顺序：`cleanup inner` → 父测试体最后一行 → `cleanup outer`）。跨 helper 传递资源时只有后者是对的。

**`t.Helper()` 解决的是"报错行号指错地方"。** 你把一堆断言抽进一个辅助函数，如果辅助函数里不调 `t.Helper()`，那么断言失败时报的行号是**辅助函数内部那一行**，而不是调用者那一行——读的人要跳进辅助函数再回头找是谁调的。加上它，Go 就把这一帧从栈里隐去，行号落在真正的调用点。本项目几乎所有辅助函数第一行就是它（`tests/harness_test.go:77`、`tests/client_test.go:167`、`internal/store/store_test.go:15`）。项目里那些断言辅助函数（`internal/httpapi` 的 `MustStatus`、`MustErr`）全部带它，就是为了让失败信息直指数据。

**`t.Cleanup` 与 `defer` 的区别，本项目给了一个教科书级的例子**（`internal/store/store_test.go:14`）：

```go
func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), Options{DisableSync: false})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
```

这里**不能**用 `defer s.Close()`。`defer` 在它所在的那个函数（`newStore`）返回时执行——也就是说存储会在用例刚拿到它、还没开始用的时候就被关掉。`t.Cleanup` 注册的回调归属于**测试**，在测试（或子测试）结束时按注册逆序执行，所以它等的是测试体的结束。此外 `t.Cleanup` 在 `t.Fatalf` 触发的提前终止里也会跑，`defer` 只要还是同一个 goroutine 也会跑，但跨 helper 传递资源时只有 `t.Cleanup` 的"作用域 = 测试"是对的。`tests/harness_test.go:296` 用同一个模式管理一个真进程：`t.Cleanup(func(){ h.Drain(); h.Kill(); h.Reap() })`，测试无论成功失败都会把守护进程和容器清干净。

**`t.TempDir()`** 给每个测试一个唯一、自动删除的临时目录，且和 `t.Parallel()` 安全共存（不会像写死 `/tmp/x` 那样在并发下撞车）。项目里凡是要落盘或要写配置文件的测试都用它（`internal/store/store_test.go:16` 给存储、`tests/harness_test.go:287` 给守护进程的数据目录和配置文件、`internal/config/config_test.go:59` 给要加载的配置文件）。

**`t.Parallel()`** 让多个子测试并行跑，把墙钟时间压下来。本项目**没有用它**，这是一个有意的取舍：端到端套件共享一个 Docker 守护进程、会分配端口、还会数全局的容器数量（`tests/harness_test.go:207` 的 `managedContainers`），并行会把这些全局断言变脆；而纯粹的内存单测本来就快，并行收益有限。**结论是：并行只有在测试之间真的没有共享状态时才安全**，本项目大部分测试不满足这个前提。

**本项目怎么做。** 把"作用域 = 测试"这一点用足的例子，是端到端套件的夹具（`tests/harness_test.go:287`）：

```go
root := t.TempDir()
h.DataDir = filepath.Join(root, "data")
h.ConfPath = filepath.Join(root, "sandboxd.conf")
h.LogPath = filepath.Join(root, "sandboxd.log")
// ...
t.Cleanup(func() {
	h.Drain()
	h.Kill()
	h.Reap()
})
```

`root := t.TempDir()` 给这次测试一个唯一目录，数据目录、配置文件、日志全部长在它下面，两个用例不会写到同一路径，目录也在测试结束时被自动删除。`t.Cleanup` 里注册的是"排空、杀进程、回收容器"，它归属于测试而非 `New`——写成 `defer`，守护进程会在夹具刚构造完、测试还没跑时就被杀掉。helper 开头的 `t.Helper()`（`tests/harness_test.go:277`）补另一半：夹具自己失败时行号落在调用它的那一行。

**替代方案**：临时目录可以自己 `os.MkdirTemp` + `defer os.RemoveAll`，但那样每次都要手写清理、还容易在 `Fatalf` 时漏掉；`t.TempDir()` 是标准库把"唯一性 + 清理 + 并行安全"打包好的版本。

**怎么验证**：`go test ./internal/store/ -run TestConcurrentCASExactlyOneWinner -race`（`internal/store/store_test.go:300`）。这个用例起 16 个 goroutine 抢同一个基线版本，断言恰好一个赢——`-race` 在这里不是装饰，它让并发语义也一起被检查。

## 4.13 `testing`：benchmark、命令行 flag 与测试替身

**问题。** 你写了个 benchmark 测一段 JSON 编码，报告 `0.3 ns/op`。这不可能对——结果没被使用，编译器把整段计算优化没了，你测的是空循环。**benchmark 的第一个坑不是"怎么跑"，而是"你测的东西还在不在"。** 另一件常踩的事：改了代码再跑测试，输出和上次一模一样，其实是结果缓存。

**原理。** `go test` 把包编译成测试二进制再运行它，所有 `-test.*` flag 由该二进制自己解析（`go test` 只负责转发），所以 `go test -run X` 与 `./pkg.test -test.run=X` 等价。三个机制：`-run` 是**按 `/` 分层的正则**（见 4.11）；`-count` 与结果缓存——成功的测试会被缓存，第二次直接打印 `(cached)` 而不真跑（实测 `ok e (cached)`），抓 flaky 要写 `-count=20`，**`-count=1` 是绕过缓存的写法**；`-race` 是编译期插桩，只对**真正发生过的**交错报警，绿不代表无竞态、红一定是真实竞争。

`Benchmark` 的 `b.N` 由框架从 1 开始放大直到测量稳定，报告 `ns/op = 总时间 / b.N`：不能只测一次，因为计时本身有开销与抖动，这也是"结果没被使用就被优化掉"能骗过你的原因。

**`Benchmark`** 是形如 `func BenchmarkX(b *testing.B)` 的函数，`go test -bench .` 会把它跑很多轮并报告每次操作耗时；加 `-benchmem` 还会报告每次操作的分配次数和字节数。它和 `Test` 的区别是 `b.N` 由框架调整、目标是稳定测量而不是断言。**`Example`** 函数则是可执行文档：`func ExampleFoo()` 把期望输出写在 `// Output:` 注释里，`go test` 会真的运行并比对，文档一旦过时测试就红。

必须诚实地说：**本项目没有 `Benchmark`，也没有 `Example`。** 它选择用端到端套件加实测数字来回答"快不快"（`/healthz` 的地板延迟、冷启动到 ready 的毫秒数等都记在测试日志里，展开见 §14），而不是靠微基准。对绝大多数热点不存在、复杂度又高的服务端代码，微基准的收益常常小于它带来的维护负担；但这不是"benchmark 没用"，而是"这个项目的瓶颈在别处"。

**命令行 flag** 里，测试者最常用的几个：

- `-run <正则>`：按名字挑测试（含子测试路径，见 4.11）。
- `-v`：打印每个测试的名字和 `t.Logf` 的输出。项目里大量 `t.Logf("status distribution over %d ...")`（`internal/httpapi/regression_test.go:118`）就是给 `-v` 看的。
- `-count N`：每个测试重复跑 N 次，用来抓 flaky。`internal/store/store_test.go:261` 特意在循环里 `time.Sleep(time.Millisecond)`，就是为了给 50 条事件造出**不同**的时间戳——如果时间戳相同，去重和排序断言就会变成一次随机通过。
- `-timeout`：整体超时，默认 10 分钟。集成套件会自己设更长的等待（`tests/harness_test.go:368` 等 `/readyz` 最多 90 秒）并在超时时打印配置和日志。
- `-race`：打开竞态检测器。代价是每个内存访问都被插桩，程序明显变慢、内存占用上升，而且它只能报**真正发生过的**竞态交错——所以它绿不代表无竞态，但红了就是一个真实的数据竞争（虽然未必每次都能重现）。项目把它做成可选：`SBX_IT_RACE=1` 时连被测的守护进程二进制都用 `-race` 编译（`tests/harness_test.go:86`），因为守护进程自己的并发才是要检查的对象，只给测试代码加 `-race` 没有意义。

**测试替身：fake / stub / spy。** 三者常被混用，但职责不同：**stub** 返回事先写死的答案（"给我一个 `true`"）；**spy** 除了应答还**记录**被怎么调用，供事后断言；**fake** 是一份能真正工作的简化实现。本项目需要一个不依赖 Docker 的 `runtime.Backend`，用的是 **fake**（`internal/service/service_test.go:39`）：

```go
type fakeBackend struct {
	mu      sync.Mutex
	objects map[string]*fakeObj
	images  map[string]*fakeImage
	seq     int

	failCreate error
	failStart  error
}
```

它不是"永远返回成功"的空壳，而是维护了 `objects`、`images` 两张表，能真的承接"创建 → 写文件 → commit → 从镜像重建"这条链，还预留了 `failCreate`/`failStart` 两个注入点，用来触发失败补偿路径。**能替换的原因是一个接口**：`service.New(st *store.Store, backend runtime.Backend, ...)`（`internal/service/service.go:41`）只认 `runtime.Backend` 接口（`internal/runtime/runtime.go:152`），所以测试把 `newFakeBackend()` 传进去，生产代码传 OCI 后端，二者对 service 完全等价。HTTP 层的测试换了个更轻的替身（`internal/httpapi/httpapi_test.go:73` 的 `fakeBackendForAPI`，每个方法几乎都返回零值），因为它只关心"请求有没有被路由和限流正确对待"，不关心后端状态。

**本项目怎么做。** 替换发生在构造那一行（`internal/service/service_test.go:388`）：

```go
fb := newFakeBackend()
svc := New(st, fb, cfg, metrics.New())
```

第二行的签名是 `func New(st *store.Store, backend runtime.Backend, cfg *config.Config, reg *metrics.Registry) *Service`（`internal/service/service.go:41`），第二个参数类型是**接口** `runtime.Backend`（`internal/runtime/runtime.go:152`）。于是测试传 `newFakeBackend()`、生产代码在 `cmd/sandboxd/main.go:89` 传 `buildBackend` 解析出的真实后端，两处对 service 层等价，service 层没有任何一行知道自己正被测试。这不是"为可测而加的抽象"：`Backend` 本就要支持多种隔离后端实现（取舍见 §16），可测性是顺带的结果。

`tests/` 包里还有一种偏 spy 的用法：`Client` 把**传输失败**记在响应对象上而不是当场抛错（`tests/client_test.go:250`），让调用方决定"没拿到响应"到底是场景要测的边界，还是测试自己坏了。

**替代方案**：可以用 `gomock` 之类的框架生成 mock。代价是多一个依赖和一层生成代码，而这个项目的核心承诺就是零第三方依赖（见 §16）；手写 fake 还顺便强制你实现整个接口，漏一个方法会编译报错。

**怎么验证**：`go test ./internal/service/ -v`（内部测试全跑在 `fakeBackend` 上，无需 Docker）；`SBX_IT_RACE=1 go test ./tests/ -run TestSandboxLifecycle` 则用带竞态检测的真二进制跑一遍完整生命周期。

---

### 本章检验

1. `internal/domain/sandbox.go:230` 用了 `LastActivityAt time.Time \`json:"last_activity_at,omitempty"\``。请解释为什么这个 `omitempty` 不起作用，并给出两种能让"可选时间"语义正确的改法，各说一个代价。
2. 一个字段的零值恰好是合法值、且与"字段缺失"含义不同（例如 `retryable bool`）。用本章讲的两个机制分别说明怎么保住这个区别，并说明各自适用于什么场景。
3. 有人把 `chain()`（`internal/httpapi/server.go:127`）里的 `s.bodyLimit` 挪到了 `s.authenticate` 的内层。请从"请求体上限在哪一刻生效"的角度说明这会造成什么后果，并指出本章没有重讲、而应由哪一章负责的那部分推理。
4. `newStore`（`internal/store/store_test.go:14`）如果改成 `defer s.Close()` 而不是 `t.Cleanup(func(){ s.Close() })`，测试会以什么方式失败？失败现象和"存储真的坏了"相比，为什么更容易被误判？
5. **动手任务**：在本机仓库里跑 `go test ./internal/httpapi/ -run 'TestMetricsExposeRealLabelValues' -v`，读输出里的 `/metrics` 正文，找出 `status="200"` 与 `status="401"` 两条计数各自由哪一次请求产生；然后回到 `internal/httpapi/server.go` 的 `accessLog` 中间件与 `statusWriter`，确认这两条计数只有在"先 Header 再 WriteHeader 再 Write"三条规则被正确复现时才会出现。把你的推理写成一段话。


# §5 Linux 隔离基础：威胁模型、namespace 与 cgroup

## 5.1 威胁模型：先回答"谁会攻击、攻击什么"

**问题**
如果不先写清威胁主体，"隔离"就会退化成一种感觉：见到容器就以为安全了，而没有人能回答"防的是谁、防到什么程度、哪些攻击根本不在这个档位的射程内"。没有威胁模型的直接后果是两类错误同时发生：对真正的攻击面（沙箱里那段代码）估计不足，同时对做不到的事（内核级隔离）做出承诺。

**原理**
`docs/THREAT-MODEL.md:5` 把威胁主体列成一张"主体/能力/动机"的表，这个三分法本身就是设计：

- **沙箱内代码**：能力是"沙箱内任意计算"，动机是逃逸、横向移动、耗尽宿主资源。这条最重，因为它就是被隔离对象本身。
- **持租户 Key 的调用方**：能力是"合法 API 面内的一切"，动机是越权访问他租户资源、超配额占用平台。它不需要逃逸，因为它本来就在门内。
- **网络攻击者**：能力是对 API 的未认证访问，动机是绕过认证、注入、DoS。

这里最值得单独说的是第一条里的括注：沙箱内的代码"可能是被提示注入的 Agent 产物"，依据是 CVE-2026-25253，`docs/THREAT-MODEL.md:7` 与 `docs/DESIGN.md:256` 都称它为**首个发给 agentic AI 系统的 CVE**。它被当论据不是因为有多严重，而是因为它把时间问题变成了结构问题：过去说"代码总有一天会被攻击"，前提是有人主动提交恶意代码；而 Agent 平台的工作负载由大模型生成，一次提示注入就能让**正常输入**产出敌意产物，运维什么都没做错。威胁因此从"未来可能"变成"输入即攻击面"。

第二件必须分清的事：**拒绝服务与逃逸是两类威胁**，缓解机制完全不同。
- 逃逸关心**边界**：代码要穿出沙箱、到宿主或别的租户那里去。对策是命名空间、能力裁剪、seccomp（能力与 seccomp 归 §6）。
- 拒绝服务关心**配额**：代码不出去，就在沙箱里把共享资源吃光。对策是 cgroup 的 CPU/内存/进程数上限与 ulimit。

混为一谈会出具体事故：加了 cgroup 限额就以为挡住了逃逸；裁掉全部能力就以为死循环会被拦住——能力不足不消耗 CPU 时间片，死循环会。

"没有隔离会发生什么"给三个具体场景：
1. **提示注入产出 `rm -rf /`**：若代码直接跑在控制面进程里，被删的是宿主的根文件系统，平台自己的数据目录、预写日志、镜像一起没了。
2. **内存/进程炸弹**：没有 cgroup，宿主内存与进程表被占满，OOM killer 开始杀"分数最高的进程"，可能正好是控制面 `sandboxd` 本身；一个租户的失控拖垮整台机器上所有租户。
3. **外泄**：有了网络，被注入的产物把上下文或文件 `wget` 出去，"数据不出域"的合规承诺当场失效。

**本项目怎么做**
威胁主体与边界写死在 `docs/THREAT-MODEL.md`，并且最重要的一节是**明确不在模型内的三件事**（`docs/THREAT-MODEL.md:11-12`）：宿主机 root 已被攻破、内存级侧信道、网络洪泛型 DoS（由部署侧负载均衡与防火墙承担）。把"不负责什么"写明，才能把边界变成可讨论、可计价的东西。

每一条威胁对应一条强制，且分属两类：不让出去（`NetworkMode=none` 默认零网络、能力全部裁剪加禁止提权）与不让你吃光（`PidsLimit=512`、内存硬上限且禁 swap、`NanoCPUs` 配额、重启策略 no 加 init 进程收僵尸），见 `docs/THREAT-MODEL.md:18-24`。调用方这一类由认证、租户过滤、配额、限流处理（`docs/THREAT-MODEL.md:43-56`），其中越权（IDOR）防线刻意放在服务层而不是路由层。

**替代方案**
一个诱人的替代是"只防逃逸，资源靠监控报警"。为什么不选：报警是事后的，OOM kill 是瞬时的，而且受害者未必是肇事者。另一个极端是"只限资源、不做边界"（所有代码共用一个账户跑），那等于没有沙箱；一个进程能读另一个租户的文件，限额再精确也拦不住。两类威胁机制不同，必须都有。

**怎么验证**
威胁模型是否有牙齿，看它有没有从内部核对。`TestIsolationContract`（`tests/isolation_test.go:27`）逐条从运行中的沙箱**内部**读内核事实，而不是读配置；DoS 这一类由遏制套件验证（见 §5.10）。

## 5.2 隔离的三个目标与容器档的边界

**问题**
"隔离"是个含糊的词。不拆开，团队会各说各话：有人以为隔离是"进程互相看不见"，有人以为是"资源不会互相饿死"，两者用的机制完全不同。拆成三个可执行的目标，每一个设计动作才有归属，取舍也才谈得下去。

**原理**
把全部隔离手段归到三个目标：

1. **缩小入口**：让逃逸的第一步根本没得走。
2. **压缩收益**：承认边界可能被击穿，但让击穿之后拿到的东西尽量少。
3. **限制影响**：即使被击穿或只是失控，损失被限在一个沙箱内。

三者的关键差异在**它们对"边界是否成立"的假设不同**：目标一假设边界成立，所以减少攻击面；目标二假设边界**已经**不成立，所以在边界内先把身份削掉；目标三假设最坏已发生，所以限制后果。这个差异决定了失效模式：边界一旦被击穿，所有"缩小入口"的措施同时作废，而"压缩收益"要在这个时候**仍然**起作用。

**本项目怎么做**
- 缩小入口：文件系统不可见（mnt 命名空间）、没有出口路径（零网络）、看不到控制面（pid 命名空间）、主机名独立（uts）、IPC 不互通（ipc）；再加上能力全部裁剪、seccomp 过滤器、禁止提权、屏蔽与只读路径。后四项属 §6。
- 压缩收益：user namespace，让沙箱内的 root 映射到宿主的一个非特权身份。`docs/DESIGN.md:174` 的措辞很精确：这一层是"压缩逃逸收益"，与其余几层"缩小入口"的机制不同。
- 限制影响：cgroup 的 CPU、内存、进程数上限，加上 `core=0` 与文件描述符两个 ulimit（`internal/runtime/oci/backend.go:238-252`）。失控代码无论怎么闹，消耗有上限。

**容器档在"压缩收益"上的局限**
这是本章必须说清、不能含糊的一点。默认配置下，宿主若没有配置 `userns-remap`，本项目的 `userns=auto` 会降级到 `host`，并把这次降级写进启动日志与 `/readyz`（`docs/DESIGN.md:163-166`）。此时沙箱内的 uid 0 **就是**宿主的 uid 0：一个内核漏洞直达宿主 root，不需要第二次提权，上面那些"缩小入口"的控制在被越过的那一刻就全部失效（`docs/DESIGN.md:172-173`）。换句话说，**在当前默认部署下，目标二是缺失的**。这必须照 `docs/POSITIONING.md:55` 如实说。

结论也要照 `docs/POSITIONING.md` 第 3 节的口径给：容器档共享内核，"逃逸需要**同时击穿每一层**"，正确的说法不是"无法逃逸"（`docs/POSITIONING.md:51-54`）。两个方向都不能省：既不能说"很安全"，也不能因为共享内核就说"隔离没用"。

**替代方案**
把目标二做实，最直接的两条路是配 `userns-remap`，或部署 microVM 后端；后者的档位对比见 §16。前者是运维一行配置的事，但会带来文件属主映射的连锁影响，所以项目选择"检测能力 + 如实披露"。另一个替代是默认就用 `userns=require`：不具备能力就拒绝启动，承诺更纯。为什么不选：它会让大多数默认宿主直接起不来，用"能跑"换"更纯的承诺"。项目把选择权交给运维（`userns=require` 可用，细节见 §6）。

**怎么验证**
`GET /readyz` 未鉴权返回 `.isolation`、`.isolation_note`、`.isolation_verified`（`docs/SECURITY-CONTRACT.md:17`）。读 `.isolation_note`，它应当写着"与宿主共享内核"这类边界描述，而不是一句"安全"。断言在 `tests/isolation_test.go:202-209`。

## 5.3 namespace 总览与三种观察手法

**问题**
两个进程看到同一份 `/`、同一张进程表、同一张网卡，绝大多数时候是好事；但当其中一个是不可信的时候，"能看到"就等于"能影响"。一个被注入的产物若能看见其他租户的进程、能改宿主路由，那么前面所有"看起来隔离了"的说法都不成立。

**原理**
namespace 是内核给**全局资源**加的一层"每进程视图"：同一类资源（挂载表、进程 ID 空间、网络栈、IPC 对象、主机名、uid 空间）被复制成多份，每个进程只看自己那一份。`clone()` 的 `CLONE_NEW*` 标志、以及 `unshare`/`setns` 是创建与进入这层视图的接口；容器运行时的核心动作，就是为新进程一次性带上这些标志。

必须强调：namespace 隔离的是**可见性**，不是**资源占用**。一个进程在自己的 net namespace 里，照样能烧掉宿主的整个 CPU —— 那是 cgroup 的事（见 §5.8）。

**本项目怎么做**
容器后端把六种 namespace 一次性配好，逐字段**显式**写出（`internal/runtime/oci/backend.go:183-207`），而不依赖宿主 `daemon.json` 的默认值。至于"HostConfig 每个字段为什么都要钉死"这套方法论，是 §7 的主题；本节只取与 namespace、cgroup 直接相关的那几个字段。

**三种观察手法（本章最该记住的实验）**
在沙箱内任何进程里都能用：

```bash
readlink /proc/self/ns/mnt   # 例：mnt:[4026532845]
ls -l /proc/self/ns/         # 一次列全，每项带命名空间编号
lsns                         # 表格：NS TYPE / NPROCS / PID / USER / COMMAND
```

`/proc/self/ns/<类型>` 是符号链接，`readlink` 结果形如 `类型:[编号]`。**编号是关键**：属于同一个 namespace 的进程，编号相同；不同 namespace 编号不同。于是判断"两个进程是否共享某层隔离"，只要比较方括号里的数字：

```bash
readlink /proc/self/ns/pid   # 在沙箱内
readlink /proc/self/ns/pid   # 在宿主上：编号不同 → pid 命名空间是独立的
```

`lsns` 把可见的 namespace 列成表，同一行的 `NPROCS` 表示属于该 namespace 的进程数——一个孤立的空闲沙箱，`pid` 行的进程数应当很小，而不是宿主上的几百。

**替代方案**
用"约定"代替 namespace（约定代码不去看 `/proc`）毫无强制力。另一个替代是 chroot：它只改根目录，进程仍看得见宿主进程表、网络与主机名，逃逸材料丰富。namespace 的价值正是把"看不见"从口头约定变成内核强制。

**怎么验证**
最小实验：沙箱内 `ls -l /proc/self/ns/`，与宿主上同一条命令对比，`mnt`、`pid`、`net`、`ipc`、`uts` 五项编号应全部不同。这一手法是后面每一节的公共工具。

## 5.4 mnt 与 uts：文件系统与主机名

**问题**
假设没有 mnt 命名空间，沙箱里的 `/` 就是宿主的 `/`，那么 `rm -rf /` 删的是宿主的根文件系统——平台自己的数据目录、预写日志、镜像全在里面。若主机名（uts）不隔离，沙箱能改宿主 hostname，影响日志、TLS，以及任何拿主机名做身份判断的东西。

**原理**
mnt 命名空间决定进程看到哪一棵挂载树。容器做的事是把宿主的一棵树在别处挂载（overlayfs 的物理机制见 §7），再让新进程以它为根、并带 `CLONE_NEWNS` 起一个独立挂载视图；此后 `mount`/`umount` 只在本视图内生效。uts 命名空间隔离 `hostname` 与 `domainname` 两个值，即 `uname()` 返回的那部分。

**本项目怎么做**
mnt 由引擎默认给每个容器独立的一份，项目本身不为 mnt 命名空间做额外设置；真正让 mnt 有价值的是"不往里塞东西"——本项目一个宿主目录都不挂，沙箱的挂载视图里只有它自己的根与少量运行时必需的挂载。与 mnt 相邻的"屏蔽路径、只读路径"集合属于 §6，本节不展开。uts 同样不额外设置，靠引擎默认。

还有一条与文件系统姿态相关、但原因不在 mnt 的决定：根文件系统被**刻意**保持可写（`ReadonlyRootfs=false`，`internal/runtime/oci/backend.go:284`）。它与本项目的快照模型耦合，原因见 §7、§8。

**替代方案**
把宿主目录 bind mount 进沙箱是常见的性能手段，也是常见的逃逸面。本项目一个宿主目录都不挂：`TestIsolationContract/no_host_directory_is_mounted`（`tests/isolation_test.go:111-123`）先在宿主上种一个金丝雀文件，再断言沙箱既看不到它、挂载表里也没有它——将来任何加进 `backend.go` 的 bind mount 都会立刻打破这条断言。

**怎么验证**
沙箱内 `readlink /proc/self/ns/mnt` 与宿主对比编号不同；`cat /proc/self/mounts` 只看到容器自己的挂载；`hostname` 与宿主不同（`tests/isolation_test.go:151-157`）。

## 5.5 pid 与 PID 1：进程视角与信号的特权

**问题**
没有 pid 命名空间，沙箱能看到宿主整张进程表——包括控制面 `sandboxd` 本身和其他租户的工作负载。看到之后，`kill` 只是一个系统调用。此外，没有 pid 命名空间就没有"进程 1"这层边界：容器里试图杀"进程 1"，打到的会是宿主进程。

**原理**
pid 命名空间给每个沙箱一张独立的进程 ID 表：沙箱内从 1 开始编号，宿主上这些进程是另一组数字。它同时决定 `/proc` 能看到谁。pid 命名空间还有一个独有性质：它有**父子层级**，子命名空间里的进程对父命名空间可见，反之不可见——所以宿主看得见沙箱的进程，沙箱看不见宿主的。

PID 1 有两条特殊语义值得记牢：
1. **信号**：namespace 里的 PID 1 不享受"默认动作"的保护——内核不会对一个没安装处理函数的 PID 1 施加"默认终止"类信号。也就是说，给 PID 1 发 SIGTERM，而它没注册处理函数时，**信号会被忽略**。这解释了为什么"优雅停止"要靠窗口期加兜底强杀。
2. **收尸**：在 pid 命名空间里，孤儿进程会被重新挂到 PID 1 上，由它 `wait`。PID 1 若不 `wait`，僵尸进程会堆积，最终耗尽进程表。

**本项目怎么做**
主进程的构造在 `internal/runtime/oci/backend.go:264-296`：

```go
Cmd: []string{"/bin/sh", "-c", "exec sleep infinity"}, // stable PID 1; workloads run via exec
// ...
Init: boolPtr(true), // reap zombies from workload execs
```

这段是三条设计叠加：
- `exec sleep infinity`：`exec` 让 shell 被 `sleep` **替换**（不是 fork 出子进程），得到一个稳定、几乎不耗资源的主进程；它不退出，容器就不会因"主进程结束"而结束。
- 工作负载全部走 `exec`，而不替换主进程。这样一次 exec 崩溃或超时不会影响容器存活；也解释了 pids 上限被撞满时连 `docker exec` 都会失败——代码把这种失败翻译成可行动的 409，而不是泄漏 OCI 内部错误（`internal/runtime/oci/backend.go:600-608`）。
- `Init: true`：让引擎在 PID 1 位置插入一个小 init，它负责收尸，并在收到信号时转发给工作负载——这是对上面两条语义的直接回应：既补上"PID 1 必须 wait"，也补上"实际干活的进程要能收到信号"。

关于停止：如果让一个不处理信号的 `sleep` 直接当 PID 1，那么发给 PID 1 的 SIGTERM 会被忽略，窗口用完才靠 SIGKILL 收场。`Init: true` 正是为了不让平台停在这个位置上。即便如此，`Stop` 也不把"停得掉"押在工作负载或 init 的信号行为上：非强制时走 `?t=10` 的优雅窗口，超时后强制杀（`internal/runtime/oci/backend.go:492-508`），强杀是兜底。

**替代方案**
一个常见做法是让工作负载本身当 PID 1（`docker run image my-app`）。为什么不选：那样应用一退出容器就结束，且应用必须自己正确处理全部信号与孤儿进程——把通用执行平台绑死在"应用要写对 init 逻辑"上，是不可接受的耦合。

**怎么验证**
沙箱内 `cat /proc/1/comm` 不是 `sandboxd`（`tests/isolation_test.go:143`）；`grep -l sandboxd /proc/[0-9]*/comm | wc -l` 为 0（`tests/isolation_test.go:141`）；空闲沙箱里 `ls -d /proc/[0-9]* | wc -l` 小于 10（`tests/isolation_test.go:147`）——共享宿主 pid 命名空间时这里会是几百。

## 5.6 net 与"零网络"：没有出口路径，而不是有防火墙

**问题**
沙箱若有网络，被注入的产物就能把数据 `wget` 出去，或连回攻击者，直接违反"数据不出域"。更常见的是"看起来关了、其实没关"：依赖某条防火墙规则，而规则运行在沙箱自己的 namespace 里——工作负载能触及的地方。

**原理**
net 命名空间给进程独立的网络栈：网卡、路由表、ARP 表、iptables 规则、端口空间。容器默认做法是新 namespace 里只留一个 loopback，再按需接入虚拟网卡。

`NetworkMode=none` 的语义正是"什么都不接"：沙箱得到一个只有 loopback 的网络命名空间，**没有** `eth0` 之类的出口接口，因此路由表为空，也就没有任何一条路径离开这个盒子。它与"有接口但防火墙拦住"是本质不同的两种姿态：前者的攻击面是"没有路"，后者的攻击面是"有一条路，靠规则拦"，而规则可能被改、可能漏。

loopback 要谨慎表述：`none` 模式下沙箱只有 `lo` 一个接口，而 `lo` 是否处于 up 状态不构成承诺。可核对的承诺是两条（`docs/SECURITY-CONTRACT.md:55-57`）：沙箱内**没有出口路由**，且**loopback 是沙箱自己的**，不是通回控制面的路。

**本项目怎么做**
`NetworkMode: "none"` 写死在 `createPayload`（`internal/runtime/oci/backend.go:278`）。而依赖它的出口策略里，只有 `none` 一种可强制：请求 `egress.mode=allowlist` 的创建被 **501 拒绝**，绝不产出"看起来受限实际没有"的沙箱（`docs/DESIGN.md:99-104`）。对应断言 `TestIsolationContract/unenforceable_limits_are_refused_not_silently_ignored`（`tests/isolation_test.go:229-244`）。

**替代方案**
在沙箱内用 iptables/nftables 做出口白名单，是很多平台的选择。为什么不选：规则运行在沙箱能触及的地方，容器后端无法保证内核级强制；与其造一个可被绕过的"白名单"，不如诚实拒绝。内核级白名单属于 microVM 后端路线图，档位对比见 §16。

**怎么验证**
沙箱内四条命令：

```bash
ip route                                     # 预期：无输出（没有路由表项）
ls /sys/class/net                            # 预期：只有 lo，没有 eth0
wget -T 3 -q -O- http://1.1.1.1/ ; echo $?   # 预期：非 0（连不出去）
nslookup example.com ; echo $?               # 预期：非 0（DNS 也出不去）
```

对应断言在 `tests/isolation_test.go:161-181`。

## 5.7 ipc 与 cgroup 命名空间：为什么也设 private

**问题**
两个不同沙箱若共享 IPC 命名空间，就能通过 POSIX 消息队列、共享内存、信号量互相通信，这是横向移动的一条路。cgroup 命名空间则决定进程看到的 `/proc/self/cgroup` 与 `/sys/fs/cgroup` 是哪一层的视图；不隔离，沙箱就能读到整台机器的 cgroup 树——谁在跑、限额多少——这本身就是侦察情报。

**原理**
ipc 命名空间隔离 System V IPC 对象与 POSIX 消息队列，`private` 意味着每个容器独享一份。cgroup 命名空间让进程看到"以自己所在 cgroup 为根"的 cgroup 树；设为 `private` 后，沙箱内 `cat /proc/self/cgroup` 显示的是自己，而不是宿主的完整层级。

这两个字段都有"引擎级默认值可能反水"的性质，这是必须钉死的原因：`IpcMode` 的默认受宿主 `default-ipc-mode` 控制，`CgroupnsMode` 的默认受 `default-cgroupns-mode` 控制（`internal/runtime/oci/backend.go:172-182` 的注释点名了这两个字段；字段钉死的整体方法论见 §7）。`docs/DESIGN.md:139-140` 给出具体的坑：`default-ipc-mode` 默认为 `shareable`，别的容器可以加入本容器的 IPC 命名空间；`default-cgroupns-mode` 在 **cgroup v1 上默认是 `host`**。v1 与 v2 的差异见 §5.8。结论是：不显式设 `private`，沙箱的 IPC 与 cgroup 视图就取决于宿主装的是哪种 cgroup、以及运维怎么配。

**本项目怎么做**
两个字段都写死为 `private`（`internal/runtime/oci/backend.go:279`、`internal/runtime/oci/backend.go:282`）。`PidMode` 显式写成空字符串（`internal/runtime/oci/backend.go:280`）是同一逻辑的另一种形式：留空是"交给引擎/宿主决定"，显式空才表示"用默认的容器私有 pid 命名空间"。

**替代方案**
`IpcMode=host` 在某些高性能场景（共享大页内存）确有动机，代价是让沙箱加入宿主的 IPC 命名空间，本项目的多租户模型不接受这个代价——为隔离而放弃的这点性能，正是产品要卖的东西。身份隔离（user namespace）是另一类取舍，本章不复述，见 §6。

**怎么验证**
沙箱内 `readlink /proc/self/ns/ipc` 与宿主对比编号不同；`ls -l /proc/self/ns/` 中应有一项 `cgroup`，且与宿主不同。cgroup 的具体数值观察见 §5.8。

## 5.8 cgroup v2：把"资源占用"变成可强制的事实

**问题**
namespace 让沙箱看不见宿主，但看不见不等于拿不走。一个 fork 炸弹能占满宿主的进程表，一个堆炸弹能把宿主内存吃光，一个死循环能把 CPU 烧满——这三件事在"一个租户的失控不能拖垮别人"的多租户场景里都是致命的，而且 OOM killer 可能杀掉控制面本身。所以隔离必须包含配额，而配额是与 namespace 完全不同的机制。

**原理：层次与控制器**
cgroup 是一棵**树**，每个节点约束挂在它下面的进程。cgroup v2 的关键设计是**单层次**：所有控制器共用同一棵树。

- `cgroup.controllers`：本层**可用**的控制器列表，文件系统根上通常列出 `cpu`、`memory`、`pids`、`io` 等。
- `cgroup.subtree_control`：本层**向子层下发**哪些控制器。父层不在 `subtree_control` 里打开 `memory`，子层就看不到 `memory.max`。这就是"启用传播"：控制器的可用性沿树向下传递，而非全局存在。
- v2 还有一条"无内部进程"规则（根除外）：一个 cgroup 要么挂控制器、要么直接挂着进程，不能两者兼得。它强制树"分组在上、进程在下"，让记账语义清晰。

**v1 与 v2 的关键差别**：v1 是**多层次**——每个控制器一棵独立的树（cpu、memory、pids、blkio 各自挂载、各自分组），一个进程在不同控制器树里可以属于不同的组。后果有两件：记账要在多棵树之间拼凑，"这个进程属于谁"没有唯一答案。v2 统一成一棵树，语义才可组合。这也直接解释了 §5.7 那个默认值差异：v1 的多层次让"cgroup 命名空间"要隔离的对象不清晰，所以 `default-cgroupns-mode` 在 v1 上默认 `host`；到了 v2 的单一层次，设成 `private` 才有清楚的含义——沙箱看到自己子树的根。

**原理：本项目用到的控制器**
- `cpu.max`：写作 `"quota period"`，单位都是微秒。period 默认 100000（100 ms），quota 是这个周期内允许消耗的 CPU 时间。`"50000 100000"` 即每 100 ms 最多用 50 ms，也就是半个核。**毫核换算**：本项目把请求的毫核乘 10^6 得到 `NanoCPUs`（`internal/runtime/oci/backend.go:273`），引擎再把它落成 `cpu.max` 的 quota。于是 1000 毫核（一个整核）对应 `"100000 100000"`，500 毫核对应 `"50000 100000"`，100 毫核对应 `"10000 100000"`。
- `memory.max`：cgroup 的内存硬上限（字节）。超限先回收（page cache、可回收匿名页），回收不动就 OOM。
- `memory.swap.max`：swap 上限，设 0 表示该 cgroup 禁用 swap。
- `pids.max`：该 cgroup 内**任务**数上限（进程与线程都计）。
- `io.max`：按设备的读写带宽/IOPS 上限，形如 `"8:0 rbps=1048576 wbps=1048576"`。

**OOM 怎么发生**
当 `memory.max` 被突破、内核回收不出足够内存时，OOM killer 在**这个 cgroup 内**挑受害者并发送 SIGKILL。三点要记牢：
1. **谁被杀**：受害者落在越限的那个 cgroup 内，也就是沙箱自己的进程；宿主的其他进程（含控制面）不在候选集里。上限画在沙箱的 cgroup 上，OOM 就是"内爆"而不是"外溢"。组内通常是内存占用最大者被选中；v2 还有 `memory.oom.group`（置 1 时整组一起杀）。
2. **退出码 137**：进程收到 SIGKILL 而死，等待者看到 `128 + 9 = 137`。这是 shell 的约定：被信号 n 杀死时退出码是 128+n，SIGKILL 的编号是 9。所以 137 不是某个"内存错误码"，它就是"被 9 号信号杀了"。
3. **外显**：Docker 把这次死亡标成 `OOMKilled`，本项目的 `inspect` 读这个字段（`internal/runtime/oci/backend.go:314`）；`Start` 在容器刚起就 OOM 时直接返回"被内存限制杀掉，请调大 memory_mib"（`internal/runtime/oci/backend.go:392-393`），退出码 137 也会被翻译成同一句可行动提示（`internal/runtime/oci/backend.go:397-398`）。

**`memory.swap.max=0` 为什么必须显式设**
`memory.max` 约束的是 cgroup 的内存占用，但如果允许 swap，内核可以把一部分匿名页换出到 swap，再让进程继续分配——沙箱的**总提交量**就变成 `memory.max` 加 swap，而 swap 是宿主全局的共享资源。结果是：内存上限被 swap 绕过，压力从内存变成宿主磁盘压力，OOM 也可能拖慢整台机器。设 `swap.max=0` 让 `memory.max` 成为真正的总上限，OOM 也变成确定性的。本项目的做法是让 `MemorySwap` 等于 `Memory`（`internal/runtime/oci/backend.go:275`，注释为 `swap off: an OOM must hit this sandbox only, and visibly`）。

**`pids.max` 与 fork 炸弹**
fork 炸弹靠指数级 `fork` 占满进程表，使宿主无法再创建进程。`pids.max` 直接封死这条路：cgroup 内任务数到达上限后，`fork`/`clone` 返回失败，炸弹无法再增长。好处是**上限画在沙箱自己的 cgroup 上**，其他租户完全不受影响；本项目里 pids 到达上限还会让 `docker exec` 失败，代码把这种失败翻译成"进程表已耗尽，进程退出后自愈或删除重建"的可行动 409（`internal/runtime/oci/backend.go:600-608`）。

`pids.max` **防不住什么**，必须说清：防不住一个进程烧 CPU（那是 `cpu.max`）；防不住一个进程吃内存（那是 `memory.max`）；防不住一个进程耗尽文件描述符（文件描述符是宿主全局资源，靠 `nofile` ulimit 管，`internal/runtime/oci/backend.go:246-252`）；也防不住"在 512 个以内"的失控——上限的含义是"不至于拖垮宿主"，不是"一个进程都不能多"。

**本项目怎么做**
逐字段对应见 §5.9。这里先指出一处诚实边界：本项目的 `resources` 结构只有 CPU、内存、swap、pids 四项（`internal/runtime/oci/backend.go:215-220`），**没有设 `io.max`**。磁盘配额在 overlayfs/ext4 上无法可靠强制，请求 `disk_mib` 会被 501 拒绝（`docs/THREAT-MODEL.md:30-32`）。控制器存在，不等于本档位选择用它。

**替代方案**
只用 ulimit 而不用 cgroup 不够：ulimit 是**每进程**限制，一个进程分叉出十个进程就各有各的 ulimit，总量没有上限；cgroup 才是"整组"的账本。

**怎么验证**
在沙箱内直接读自己的 cgroup 数值：

```bash
cat /proc/self/cgroup                # v2 统一层次：0::/  ← 沙箱是自己 cgroup 树的根
cat /sys/fs/cgroup/memory.max        # 例：536870912（= 512 MiB）
cat /sys/fs/cgroup/memory.swap.max   # 预期：0（禁 swap）
cat /sys/fs/cgroup/pids.max          # 预期：512
cat /sys/fs/cgroup/cpu.max           # 例：50000 100000
```

`cat /proc/self/cgroup` 那一行的形态，也是判断宿主是 v1 还是 v2 最快的办法：v2 只有一行 `0::/...`，v1 是多行、每行一个控制器。宿主机侧用 `systemd-cgtop` 可以按 cgroup 看实时的 CPU/内存占用排行，用来观察哪个沙箱在吃资源。

## 5.9 本项目怎么把请求变成 cgroup 数值

**问题**
上一节讲的是内核接口，这一节回答"数值从哪来"。不讲清这条链，读者会以为 pids 上限或内存上限是凭空出现的，也就无法判断哪些能调、哪些调不动。

**原理**
沙箱的规格先在服务层被"模板默认、配置默认、请求覆盖"三层合并，再被校验，然后才变成后端的 `CreateRequest`（`CPUMillicores`/`MemoryMiB`，`internal/runtime/runtime.go:29-30`），由容器后端换算成 `HostConfig.Resources`。

**本项目怎么做**
换算就三行（`internal/runtime/oci/backend.go:272-277`）：

```go
Resources: resources{
    NanoCPUs:   int64(req.CPUMillicores) * 1_000_000,   // → cpu.max
    Memory:     int64(req.MemoryMiB) << 20,             // → memory.max
    MemorySwap: int64(req.MemoryMiB) << 20,             // → memory.swap.max = 0
    PidsLimit:  &pids,                                  // → pids.max
},
```

逐字段对应：
- `NanoCPUs` ← 毫核乘 10^6 → `cpu.max`。10^9 NanoCPUs 是一个整核，即 `"100000 100000"`。
- `Memory` ← MiB 左移 20 位（乘 2^20）得字节 → `memory.max`。
- `MemorySwap` ← 与 `Memory` **同值** → 引擎解读为"内存加 swap 的总量等于内存上限"，等价于 `memory.swap.max=0`。
- `PidsLimit` ← 常量 `pids := int64(512)`（`internal/runtime/oci/backend.go:255`）。注意它**不来自请求也不来自配置**——一个租户无法把 512 调高。

数值来源链（以默认部署为例）：
1. 请求里的 `cpu_millicores`/`memory_mib`（可省略）；
2. 省略时落到模板默认，再落到配置默认 `DefaultCPUMillicores=1000`、`DefaultMemoryMiB=512`（`internal/config/config.go:192-193`）；
3. 合并函数 `mergeSpec` 负责"零值继承"（`internal/service/service.go:224-243`）；
4. 校验闸门：CPU 落在 100 到 8000、内存落在 64 到 16384（`internal/domain/sandbox.go:165-169`、`internal/config/config.go:183-184`）。

所以一个"什么都没写"的创建请求，最终在沙箱里看到的会是 `memory.max` 为 512 MiB、`cpu.max` 为 `"100000 100000"`、`pids.max` 为 512、`memory.swap.max` 为 0。

**替代方案**
让后端自己决定默认资源（"不填就给个合理值"）看似方便，但那样"一个沙箱拿了多少"就散落在后端实现里，配额没法在准入处准确计算。本项目把默认值上移到配置层，后端只做机械换算——这样 `gov` 层算配额和 `oci` 层落 cgroup 用的是同一个数字来源。

**怎么验证**
按 §5.8 的命令在沙箱内读 `memory.max`，与创建请求的 `memory_mib` 对照；省略请求字段时应当看到 512 MiB。

## 5.10 自检与遏制测试：怎么证明隔离真的在跑

**问题**
"我配了 `memory.max`"和"`memory.max` 真的生效了"是两回事。配置读出来正确、内核却收到别的值的失败模式，在容器引擎层是真实存在的（见 §6 的身份映射实测）。所以必须有从沙箱**内部**读事实的证据链，以及让人相信"攻击真的发生过却被挡住"的对照。

**原理**
隔离声明有两种出错方式：配置写对了，内核却没照做；以及载荷根本没执行，于是"什么都没发生"。前者要求证据必须来自沙箱**内部**读到的内核事实，后者要求每个遏制场景先证明载荷生效。这两条原则在本章只落到具体机制上，方法论整体见 §14。

**本项目怎么做**
本项目用两条互补的证据链。

一是 **daemon 自检**：`VerifyIsolation`（`internal/runtime/oci/isolation.go:75-137`）用**生产同一条创建路径**起一个真沙箱（`internal/runtime/oci/isolation.go:91-99`），在里面跑一段只读探针 `probeScript`（`internal/runtime/oci/isolation.go:35-52`），把内核报告的事实逐条对照契约。探针中与 namespace/资源相关的项有 `uid_map`（身份映射，归 §6 讲）与 `can_mount`（能否新建挂载，即 mnt 视角的边界，`internal/runtime/oci/isolation.go:44-50`）；其余是 seccomp、禁止提权、能力集、core dump、能否读 `/proc/kcore`。自检机制的整体设计属 §6，这里只指出：探针沙箱本身走同一份 `Resources` 路径创建（`internal/runtime/oci/isolation.go:94-95` 用 100 毫核、64 MiB），所以它读的是"一个真沙箱的内核事实"，不是配置。

二是 **端到端遏制套件**：`tests/containment_test.go` 用五个真实载荷验证资源与边界。cgroup 数值被证明生效，正是在这里——daemon 自检那份契约主要覆盖 seccomp、能力与 namespace，**不覆盖**资源上限，两个集合互补。

**五个遏制场景各验证哪一层**

| 载荷 | 验证的机制 | 关键断言 |
|---|---|---|
| `rm -rf /` | mnt 命名空间：破坏留在沙箱自己的挂载视图 | daemon `/healthz` 200、同租户旁观沙箱仍工作、宿主金丝雀与 `/etc/hosts` 摘要不变、残骸仍可删除（`tests/containment_test.go:49-118`） |
| fork 炸弹（2000 个 `sleep 30 &`） | `pids.max` | 不产生 5xx，daemon 仍响应；再 exec 要么成功要么给可行动 409（`tests/containment_test.go:122-160`） |
| 64 MiB 内存炸弹 | `memory.max` 与 OOM | 退出码非 0（实测 137），邻居沙箱正常，daemon 正常（`tests/containment_test.go:166-186`） |
| CPU 独占（500 毫核加死循环） | `cpu.max` | 烧满时 5 次健康检查小于 3 秒、无关沙箱 exec 小于 10 秒（`tests/containment_test.go:191-238`） |
| 沙箱内写 `/etc/hosts`、`/etc/passwd` | mnt 命名空间：同一路径在不同侧是不同文件 | 沙箱内写入必须**成功**（回读 1/1），宿主同名文件摘要不变（`tests/containment_test.go:248-274`） |

**为什么每项都带正向对照**
`tests/containment_test.go:6-12` 的注释把理由写死了：旧的 shell 版本只断言"宿主没事"，却从不证明载荷真的执行过——而一个静默失败的探针，和一个被完整挡住的攻击，会通过**同一条断言**。"没出事"只有在先证明"出过事"之后才有意义。所以每个场景都先给出载荷在可及范围内**确实生效**的证据：`rm -rf /destroy-me` 真的删掉了目录（`tests/containment_test.go:62-71`），沙箱内对 `/etc/hosts` 的写入真的返回 `rc=0` 并回读得到（`tests/containment_test.go:260-270`）。测试方法论整体见 §14。

**替代方案**
只跑单元测试（构造一个假的 cgroup 接口，断言写入了预期数值）也能"证明"配置正确，但这恰好是最不可靠的一层：它证明的是代码写对了，不是内核照做了。本项目的取舍是把最关键的事实放到"真沙箱、真载荷"上验证，代价是慢、且依赖容器运行时。

**怎么验证**
```bash
SBX_REQUIRE_DOCKER=1 go test ./tests/ -run 'TestBlastRadius|TestForkBomb|TestMemoryBomb|TestCPUBurn|TestInSandboxHostPath' -v -count=1
```
`SBX_REQUIRE_DOCKER=1` 不能省：没有它，缺运行时的机器会静默跳过整套测试，而"跳过"与"通过"在报告上长得一样（`docs/SECURITY-CONTRACT.md:150-151`）。

## 5.11 有界结论：容器档防住什么、防不住什么

按 `docs/POSITIONING.md` 第 3 节的口径，结论必须明确且有界。

**防得住（默认部署下成立的边界）**
- 沙箱看不到宿主的文件系统（mnt）、看不到控制面进程（pid）、没有出口网络（net）、主机名独立（uts）、IPC 不互通（ipc）、cgroup 视图是自己的（cgroupns）。
- 失控的资源占用被限制在沙箱自己的 cgroup 内：内存爆炸只杀本沙箱，fork 炸弹撞上 pids 上限，CPU 独占不饿死控制面。
- 破坏的爆炸半径是沙箱本身：`rm -rf /` 之后 daemon 存活、宿主逐字节未变、邻居与新沙箱正常。

**防不住（三条边界，都是产品设计的一部分，不是缺陷）**
1. **不承诺内核级隔离**：沙箱与宿主共享内核，一个内核漏洞构成理论逃逸面（`docs/POSITIONING.md:51-54`）。正确措辞是"逃逸需要**同时击穿每一层**"，不是"无法逃逸"。
2. **默认不承诺身份分离**：`userns=off` 时沙箱内 uid 0 就是宿主 uid 0，一次内核提权不需要第二次提权（`docs/POSITIONING.md:55`）。`userns=require` 能在不具备条件时拒绝启动，而不是静默降级。
3. **两条控制被拒绝而非假装**：磁盘配额与 egress 白名单的请求都返回 501（`docs/POSITIONING.md:58-60`），并给出可执行的替代说明。

隔离档位的强度排序（Firecracker/Kata 不低于 gVisor，gVisor 高于普通容器）见 §16。

---

### 本章检验

1. 为什么"拒绝服务"和"逃逸"必须分成两类威胁？各举出本项目的一个措施，并说明如果拿错措施去防另一类威胁会发生什么。
2. `memory.swap.max` 不显式设 0 时，`memory.max` 为什么不再是一个可信的上限？请从"总提交量"与"共享资源"两个角度说清。
3. 如果没有 init 收在 PID 1 位置、PID 1 就是一个不处理信号的 `sleep`，`docker stop` 的 SIGTERM 会怎样？这如何解释本项目要用 `Init: true`，并在 `Stop` 里保留 `?t=10` 加强杀兜底？
4. 五个遏制场景里，`rm -rf /` 与"沙箱内写 `/etc/hosts`"都在验证 mnt 命名空间，它们的区别是什么？为什么后者必须要求沙箱内写入**成功**？
5. **动手任务**：在沙箱内依次运行 `readlink /proc/self/ns/mnt`、`cat /proc/self/cgroup`、`cat /sys/fs/cgroup/pids.max`，再在宿主上运行同样三条。把两边的差异列成一张表，并解释：`cat /proc/self/cgroup` 的输出形态为什么能告诉你宿主用的是 cgroup v1 还是 v2。


# §6 Linux 加固与逃逸面：能力、seccomp、LSM 与 user namespace

## 6.1 capabilities：为什么 root 被拆成几十个能力

**问题**

假设内核里没有能力这个概念，进程就只有普通用户和 root 两种身份。现在沙箱里的工作负载要做一件需要特权的小事——绑定 80 端口、改一个不属于自己的文件。你只能给它 root。而 root 不只是"能做那件小事"：它同时能读任何文件、加载内核模块、挂载文件系统。**你为了授予一项权力，被迫授予了全部权力。** 这就是超级用户悖论，最小权限原则在它面前失效。

在沙箱里这个悖论更尖锐：沙箱内的进程为了跑常见工具链习惯以 uid 0 运行；如果 uid 0 真意味着全部权力，那"沙箱里的 root"就是"宿主上的 root"。

**原理**

从 Linux 2.2 起，内核用**能力（capability）**替代单一的 `uid == 0` 检查：每次原本"要求 root"的操作改成一个具体能力位，内核不再问"你是不是 root"，而问"有效能力集里有没有这一位"。这样的位有四十几项。危险且需记住的几个：

- `CAP_SYS_ADMIN`（第 21 位）：挂载、`unshare`、`setns`、`pivot_root` 与大量 `ioctl`。它被塞进太多互不相关的操作，被称为"新的 root"。
- `CAP_SYS_MODULE`（16）：加载内核模块，等于把任意代码放进内核。
- `CAP_SYS_PTRACE`（19）：跟踪任意进程，读写别人的内存。
- `CAP_SYS_RAWIO`（17）：裸 I/O。
- `CAP_DAC_OVERRIDE`（1）、`CAP_DAC_READ_SEARCH`（2）：绕过文件权限检查。
- `CAP_NET_ADMIN`（12）：配置网络。

每个进程有五个能力集，读 `/proc` 前必须分清：`CapPrm`（允许持有的上限）、`CapEff`（**真正被检查的集合**）、`CapInh`（可被 exec 继承的部分）、`CapBnd`（**天花板**，某位一旦从 bounding 去掉，任何 `execve` 都加不回来）、`CapAmb`（为无特权程序保留）。在任意进程里：

```bash
grep -E '^Cap' /proc/self/status
```

掩码读法直接：**最低位是第 0 位，往左每一位对应一个能力编号。** 用一个真实数值练一遍——Docker 默认给出的有效能力集在 `/proc/self/status` 里是 `00000000a80425fb`：

```text
a80425fb
  |||||||└─ b = 1011 → 位 0,1,3   CAP_CHOWN, CAP_DAC_OVERRIDE, CAP_FOWNER
  ||||||└── f = 1111 → 位 4..7    CAP_FSETID, CAP_KILL, CAP_SETGID, CAP_SETUID
  |||||└─── 5 = 0101 → 位 8,10    CAP_SETPCAP, CAP_NET_BIND_SERVICE
  ||||└──── 2 = 0010 → 位 13      CAP_NET_RAW
  |||└───── 4 = 0100 → 位 18      CAP_SYS_CHROOT
  ||└────── 0 = 0000 → 位 20..23  无
  |└─────── 8 = 1000 → 位 27      CAP_MKNOD
  └──────── a = 1010 → 位 29,31   CAP_AUDIT_WRITE, CAP_SETFCAP
前八位 00000000 → 位 32..63 全为 0
```

这份默认集合**有** `CAP_SETUID`/`CAP_SETGID`（能改身份）、**没有** `CAP_SYS_ADMIN`（跑不了 `mount`）。可见"默认并不安全"：它只拿掉了最危险的一批，剩下的仍能改文件属主、绕过权限、创建设备节点。`capsh --print` 会直接把同一份掩码翻成能力名清单：

```bash
capsh --print
getpcaps $$        # 只看某个 pid
```

**本项目怎么做**

创建载荷把能力整体清空（`internal/runtime/oci/backend.go:285-286`）：

```go
CapDrop:        []string{"ALL"},
CapAdd:         []string{},
```

`CapDrop: ALL` 从 bounding 集把所有能力去掉，因此 `CapPrm`/`CapEff` 无法再有内容，此后任何 `execve` 也带不回来。`CapAdd: []string{}` 是**显式写出的空集合**——未设置的字段是引擎可以替你填的字段，写空数组才表示"一个都不要加"（字段钉死方法论见 §7）。

**为什么 `CapDrop: ALL` 比只 drop 几个更好**，这是"默认拒绝"与"默认允许"的分野。只 drop 一张危险名单做的是**枚举式禁止**：名单之外默认"可以有"，安全性取决于名单有多全，而名单永远不全——内核每新增一个能力位，它立刻是被允许的，你的代码一字未改，沙箱却静默放宽。`CapDrop: ALL` 做的是**枚举式允许**：默认全禁、按需加白名单；新能力位默认落在"禁止"一侧，出错方向是安全的那一侧。本项目连白名单都是空的。

还要说清：**裁剪能力没有去掉 uid 0**，沙箱内 `id -u` 仍返回 0。保留它是因为大量工具链假定自己在 root 下运行（写 `/etc`、装包、改属主）；而 uid 0 的危害来自它**绑定**的那些能力，能力一空，它就只剩一个名字。这就是契约里"uid 是 root 但没有权力"（`docs/SECURITY-CONTRACT.md:43-44`）的来历。

**替代方案**

"只 drop 最危险的几个"没选：它是枚举式禁止，需要与内核能力表保持同步，而内核不会通知你。"让工作负载以非 root uid 运行"没选：uid 不是权限的载体、能力才是——非 root 进程照样可以持有文件能力位或处在自己是 root 的命名空间里，还会破坏依赖 root 的工具链。

**怎么验证**

```bash
grep -E '^Cap' /proc/self/status   # 预期：五个集合全为 0000000000000000
capsh --print                      # 预期：Current 一行为空
id -u                              # 预期：0
```

`TestIsolationContract/capability_sets_are_empty`（`tests/isolation_test.go:51-55`）要求五个集合全零；`uid_is_root_but_powerless`（`tests/isolation_test.go:57-63`）在断言 uid 为 0 的同时要求有效能力集为空。辅助函数 `mustBeAllZeroHex`（`tests/isolation_test.go:251-260`）读到空字符串时**直接判失败**并给出理由"内核什么都没报，这是没测到而不是通过"——三态纪律在本章第一次出现。

## 6.2 seccomp：把系统调用表变成一道过滤器

**问题**

能力清空后进程没有任何特权位，但仍能调用内核暴露的**整张系统调用表**。其中一批工作负载一辈子用不到，却是内核漏洞利用的垫脚石：`ptrace`、`kexec_load`、`userfaultfd`、`keyctl`、`bpf`、带危险标志的 `clone`/`unshare`。系统调用面是所有攻击面里最大的一个，一次内核本地提权几乎总从某个系统调用开始。

**原理**

seccomp 是内核按进程生效的**系统调用过滤器**。`/proc/self/status` 的 `Seccomp:` 有三个取值：

- `0`：未启用。
- `1`：`SECCOMP_MODE_STRICT`，只允许 `read`/`write`/`_exit`/`sigreturn`，其余杀进程。
- `2`：`SECCOMP_MODE_FILTER`，挂一段 BPF，**每次系统调用进入内核时**运行，按调用号与参数决定放行、返回错误或杀进程。容器用模式 2。

模式 2 的力量来自位置：过滤器在内核的**系统调用入口**执行，先于用户代码，用户态的 bug 绕不过它——被拦的调用根本没机会进入后续内核路径。

这解释了它为什么比"检查文件名"更根本。一个按名字校验的封装：先判断路径不以 `/workspace` 开头就拒绝，再打开。检查与打开是两次操作，攻击者能在中间把路径换成指向 `/etc/shadow` 的符号链接——这就是 TOCTOU。任何按名字检查的逻辑都有这个窗口，因为它检查的是**用户态的一个字符串**，内核执行的却是**那一刻真正的对象**。seccomp 过滤系统调用本身，字符串怎么变都骗不过。

**本项目怎么做**

载荷把 `no-new-privileges` 显式写进 `SecurityOpt`（`internal/runtime/oci/backend.go:257`），apparmor 视宿主能力追加（见 §6.3）；**seccomp 用引擎自带默认 profile，本项目没有钉死它**。这是一处诚实边界：`docs/DESIGN.md:144` 的字段表写明 `SecurityOpt` 的 seccomp 项"可被 daemon 级替换"——沙箱确实运行在模式 2 下，但那份允许/拒绝清单由宿主引擎决定。

正因为清单不在自己手里，本项目只对**结果**下断言：自检要求 `Seccomp` 等于 `2`（`internal/runtime/oci/isolation.go:221-222`，注释是"系统调用过滤器是内核漏洞必须先穿过的那一层"）；契约测试 `seccomp_filter_is_in_force`（`tests/isolation_test.go:38-41`）同样断言模式为 2。这是"配置不是证据"在 seccomp 上的落点：我们无法核对 profile 内容，但有办法核对"过滤器到底挂上没挂上"。

Docker 默认 profile 是**黑名单**：列一批已知危险系统调用去禁止，其余放行。局限与能力位一样——它随内核与攻击手法演进，而更新由引擎版本决定；两次更新之间，新出现的危险调用是放行的。

**能防住什么、防不住什么**最易误解。它能防住黑名单上那些调用被调用；**防不住对合法调用的滥用**。`write` 永远在白名单上，沙箱内代码可以一直写直到磁盘写满——seccomp 无能为力；磁盘上限在文件系统层面无法可靠强制，所以本项目对 `disk_mib` 请求**直接 501 拒绝**（`docs/THREAT-MODEL.md:30-32`）。同理 `fork`/`clone` 是合法调用，fork 炸弹由 `pids.max` 接住（见 §5.8）。一句话：**seccomp 管"你用了哪个系统调用"，不管"你把一个合法调用用得多狠"。**

**替代方案**

自定义**允许清单** profile（只放行某运行时需要的几十个调用）更强但脆：Go、Python、shell 各需一份，写少一个就让正常任务以像 bug 的方式失败。本项目选引擎默认 profile，把"过滤器是否在跑"当可核对承诺，把"过滤器内容"如实说成不可控。用 gVisor 在用户态重实现系统调用是另一档，见 §16。

**怎么验证**

```bash
grep '^Seccomp' /proc/self/status    # 预期：Seccomp: 2
keyctl list @s 2>&1 | head -1        # 默认 profile 下被拦的调用会失败
```

契约测试 `TestIsolationContract/seccomp_filter_is_in_force`（`tests/isolation_test.go:38-41`）断言模式等于 2；daemon 侧 `internal/runtime/oci/isolation.go:221-222` 做同一条断言。

## 6.3 LSM 与 AppArmor：第二道独立于 root 的门

**问题**

能力与 seccomp 管的都是"凭**特权**能做什么"。但沙箱里跑的是 uid 0，仍可能触及一些按路径而非按权限判定的操作。更要警惕的是另一个层面：容器引擎靠 LSM 给容器挂策略；如果宿主有 AppArmor、而这个策略**没挂上**，整道第二层防线就是缺席的。反过来，如果宿主**没有** AppArmor，你却硬指定一个 profile，那是一次会让容器根本起不来的错误。

**原理**

LSM（Linux Security Modules）是内核里的**一组钩子**：内核在一批安全操作点上布钩子——文件打开、套接字连接、挂载、`ptrace`——模块注册回调，返回允许或拒绝。AppArmor 是其中一个模块，SELinux 是另一个。AppArmor 的模型是**按路径挂 profile**：profile 属于某个进程，列出路径模式与允许的访问，加上能力规则；`enforce` 模式违规即拒，`complain` 只记日志。当前进程挂哪个 profile，读 `/proc/self/attr/current` 就知道，输出形如 `docker-default (enforce)`。

**本项目怎么做：为什么只在 `/info` 报告存在 AppArmor 时才指定 profile**

初始化时读 `/info`，记下宿主是否报告 AppArmor（`internal/runtime/oci/backend.go:125`）：`b.apparmorHost = info.hasSecurityOption("apparmor")`。只有它为真才追加 profile（`internal/runtime/oci/backend.go:258-263`）：

```go
secOpt := []string{"no-new-privileges"}
if b.apparmorHost {
	// naming a profile on a host without AppArmor is an error.
	secOpt = append(secOpt, "apparmor=docker-default")
}
```

道理是 AppArmor 不普遍。它在 Debian/Ubuntu 系默认启用，在很多 RHEL 系宿主上被 SELinux 取代，在内核未选它时则完全没有。在**内核没有加载 AppArmor 的宿主上指定 profile 是错误**——运行时会因为无法把 profile 应用到进程而让容器启动失败。所以无条件写 `apparmor=docker-default` 不是"更安全"，而是"在某些宿主上直接跑不起来"。本项目把它变成**有条件**动作，并如实反映到自检：宿主没有 AppArmor 时，自检对 `apparmor` 只**记录**不断言，值为 `n/a`，附注"SELinux 宿主会不一样"（`internal/runtime/oci/isolation.go:236-243`）。

**替代方案**

无条件指定 `apparmor=docker-default` 没选：会在没有 AppArmor 的宿主上让沙箱起不来，用"能跑"换一个在那些宿主上根本不存在的姿态。改用 SELinux 标签（`label=...`）没选：那是另一套基于标签、需要宿主侧策略的模型，给容器文件系统打标签还有自己的属主连锁影响。本项目不试图同时支持两套 LSM，而是**检测宿主有什么、据此决定说什么**；真正的 LSM 级文件/套接字控制在路线图里（`docs/DESIGN.md:320-321`）。

**怎么验证**

```bash
cat /proc/self/attr/current     # 预期（AppArmor 宿主）：docker-default (enforce)
sudo aa-status                  # 宿主上：列出已加载 profile 与其模式
```

自检在 AppArmor 宿主上断言字符串含 `(enforce)`（`internal/runtime/oci/isolation.go:236-239`）。

## 6.4 no-new-privileges：封住重新获得特权的两条路

**问题**

创建时把能力丢掉，不等于进程永远没能力。两条经典路径能让它**重新获得**特权：执行一个 setuid-root 可执行文件（`sudo`、`passwd`），`execve` 会把有效 uid 变成文件属主并可能给能力集加内容；执行一个带**文件能力位**的二进制（`setcap cap_net_raw+eip /path/bin`），`execve` 会把文件声明的那几个能力授予进程。两条路不封，那么"我丢了能力"只是"我此刻没有能力"。

**原理**

`no-new-privileges` 对应内核的 `PR_SET_NO_NEW_PRIVS` 一个位。置上后，内核在 `execve` 时**跳过 setuid/setgid 的提权处理与文件能力位的授予**：setuid 位被忽略，进程不会因执行某个二进制而变成 root；文件能力位同样被忽略。它把"提权只能靠已有能力"变成硬规则。

**为什么它和 drop capabilities 互补而非重复**：drop capabilities 处理进程**此刻握着什么**，把当前能力集清空；`no-new-privileges` 处理进程**将来能否通过 exec 拿回什么**，堵住内核在 `execve` 时主动授予新特权的机制。前者管"存量"，后者管"增量"。没有后者，一个 bounding 集非空的环境里，攻击者只要让沙箱执行镜像里某个带文件能力的二进制就能拿回能力；只有后者而没有 drop capabilities，进程当下握着的位一个没少。两者必须同时有。

**本项目怎么做**

无条件写进 `SecurityOpt`（`internal/runtime/oci/backend.go:257`）：`secOpt := []string{"no-new-privileges"}`。这与 apparmor 的条件写入形成对照：`no-new-privileges` 是内核普遍支持的位，任何宿主上写都安全，所以无条件钉死。自检断言 `NoNewPrivs` 等于 `1`，附注"阻止通过 setuid/setgid 二进制获得特权"（`internal/runtime/oci/isolation.go:223-224`）。

**替代方案**

只靠 drop capabilities 没选：它不阻止 `execve` 改变有效 uid（setuid 位仍生效），且留了文件能力的口子。以 `nobody`（65534）运行没选：既不移除当前能力、也管不住文件能力位，还改变工作负载对文件属主的假定。

**怎么验证**

```bash
grep '^NoNewPrivs' /proc/self/status    # 预期：NoNewPrivs: 1
```

`TestIsolationContract/privilege_cannot_be_regained`（`tests/isolation_test.go:44-48`）断言该位为 1；daemon 自检见 `internal/runtime/oci/isolation.go:223-224`。

## 6.5 masked paths / readonly paths：挡掉少数几个危险文件

**问题**

沙箱里有一个 `/proc` 和一个 `/sys`（它们属于容器的视图，由引擎挂载）。大部分无害，但少数文件直接暴露宿主层面：`/proc/kcore` 是宿主内核地址空间的一份 ELF 核心映像，读到它就是一次内核内存泄漏，能打掉 KASLR；`/proc/sysrq-trigger` 一写就触发内核动作；`/proc/timer_list`、`/proc/sched_debug`、`/proc/keys`、`/proc/acpi`、`/proc/scsi` 泄漏内核指针或设备信息；`/sys/firmware`、`/sys/devices/virtual/powercap` 泄漏宿主固件与功耗状态；`/proc/sys` 下的内核可调参数里，最经典的是 `kernel.core_pattern`——若它可写，攻击者可以把它设成管道命令，于是宿主上任何进程下一次 core dump 时内核会以 root 执行那条命令，这是一条不需要内核漏洞的直通宿主 root 的路。

**原理**

引擎用两种动作，机制不同、效果要分清：

- **屏蔽（masking）**：在沙箱挂载命名空间里把 `/dev/null` 绑定挂到该路径之上，读它得到空、写它无效——把文件"藏起来"。
- **只读（read-only）**：把该路径以只读方式绑定挂载，可读不可写。

两者都是**寄生在挂载命名空间里的绑定挂载**，由内核挂载层强制，而非靠文件权限位——所以对沙箱内的 root 一样有效。

**为什么"屏蔽集合会随引擎版本变化"所以要显式复制**

屏蔽哪些、只读哪些，是**引擎版本的属性**，不是内核的、也不是本项目代码的；Docker 跨版本增删过这张清单。不写 `MaskedPaths`，你继承的就是这台机器上装的引擎默认清单——引擎升级一次，沙箱实际姿态可能悄悄变了，而你的代码一字未改。因此本项目把集合**抄进代码**（`internal/runtime/oci/backend.go:222-236`）：

```go
pinnedMaskedPaths = []string{
	"/proc/asound", "/proc/acpi", "/proc/kcore", "/proc/keys",
	"/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats",
	"/proc/sched_debug", "/proc/scsi", "/sys/firmware",
	"/sys/devices/virtual/powercap",
}
pinnedReadonlyPaths = []string{
	"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
}
```

复制过来之后契约在**一个地方**可审查，引擎升级也不改变它。字段钉死的整体方法论属于 §7。

**本项目怎么做**

两个集合在 `createPayload` 里传给引擎（`internal/runtime/oci/backend.go:288-289`）。两处"结果"被自检独立核对：沙箱内读 `/proc/kcore` 必须失败（`internal/runtime/oci/isolation.go:233-234`、`tests/isolation_test.go:81-83`），`/proc/sysrq-trigger` 不可写（`tests/isolation_test.go:85-88`）。

**替代方案**

依赖引擎默认值没选：它可变、随版本漂移，属于"静默放宽"的典型。把整个 `/proc` 设只读或挂新 procfs 没选：`/proc` 必须动态反映进程状态，引擎本已给了独立 procfs，问题只出在少数几个文件上。

**怎么验证**

```bash
head -c 1 /proc/kcore ; echo $?           # 预期：非 0
echo 1 > /proc/sysrq-trigger ; echo $?    # 预期：Permission denied / Read-only file system
cat /proc/self/mountinfo | grep -E 'kcore|sysrq'   # 预期：能看到 /dev/null 与 ro 绑定
```

最后一条最直接：它显示的是**内核挂载表里的实际挂载**，不是配置。

## 6.6 user namespace 与 uid 映射：唯一一层"假设边界已经破了"的控制

**问题**

以上一切作用都在沙箱**内部**。但如果沙箱内的 uid 0 **就是**宿主的 uid 0，那么任何一层被内核漏洞击穿的那一刻，攻击者已经是宿主 root——没有第二道墙，因为身份本身没有下沉。这就是 §5.2 里"压缩收益"的缺口：沙箱缺一个身份地板。

**原理**

user namespace 把**命名空间内的 uid** 映射到**命名空间外的 uid**，映射写在 `/proc/<pid>/uid_map`，每行三个数：`内核内 uid  宿主 uid  数量`。一件必须讲清的事：内核里记录的、用来判断的**始终是宿主侧 uid**，命名空间内的 uid 只是给进程看的一个视图。所以：

- 沙箱内 uid 0 若映射到宿主 100000，内核检查文件属主、检查能否给某进程发信号时用的是 100000。
- 沙箱内创建的文件在宿主上属主是 100000，不是 0。
- 沙箱内发出的信号"来自" uid 100000，打不到宿主的 root 进程。
- 一次内核漏洞即使把沙箱内提权成"root"，提的也是**这个命名空间的 root**，即宿主上的 100000——一个无特权用户。

于是私有 user namespace 里 `/proc/self/uid_map` 会显示类似：

```bash
cat /proc/self/uid_map
#          0     100000      65536
```

读作"内核内 uid 0 起、共 65536 个，映射到宿主 uid 100000 起"。

**本项目怎么做。** 这条原则在代码里落成下面四个可核对的决定。第一个是一行必须能一眼认出的映射——本项目把它写成常量 `identityUIDMap`（`internal/runtime/oci/isolation.go:65-70`）：`0 0 4294967295`。有一行必须能一眼认出——本项目把它写成常量 `identityUIDMap`（`internal/runtime/oci/isolation.go:65-70`）：`0 0 4294967295`。读作"内核内 uid 0 起、共 4294967295 个（2³²−1，覆盖整个 32 位 uid 空间）映射到宿主 uid 0 起"。映射范围是整个空间且起点相同，所以对每个 uid 都有"内 == 外"——这个命名空间**什么都没映射**：形式上存在，效果上等于不存在。一行字就把"私有用户命名空间"这个说法证伪，这就是它值得做常量的原因。

**为什么 userns 属于"压缩收益"而不是"缩小入口"**

§5.2 的三类目标区别在**对"边界是否已破"的假设不同**。"缩小入口"削减攻击面（可见命名空间更少、没网络、没能力），假设边界还成立。user namespace 不减少攻击面（沙箱照样有 uid 0、照样共享内核），它改变的是**逃逸成功之后落到哪儿**。`docs/DESIGN.md:172-174` 把它与其余层并列时用的措辞正是"压缩逃逸收益"。这个归属不是文字游戏：它决定失效模式——缩小入口的措施在内核被击穿那一刻全部作废，而压缩收益要在这时候仍然起作用。

**那个实测**

这是本章、也是全项目最值得记住的例子。在一台**没有配置 `userns-remap`** 的宿主上把 `HostConfig.UsernsMode` 设为 `private`：

```text
GET /containers/<id>/json → HostConfig.UsernsMode = "private"   ← 引擎原样回读
/proc/self/uid_map        → 0 0 4294967295                      ← 什么都没发生
```

引擎**接受了它无法兑现的字段**，存下来，并从 inspect 接口**原样回读为已应用**，而沙箱仍跑在 identity map 上（记录在 `docs/DESIGN.md:152-160`，同样的说明在 `internal/runtime/oci/client.go:118-133`）。

为什么它值得记住：它把"配置不是证据"从口号变成一次**可复现的实测**。人们会以为"从引擎读回来的值总该是真的吧"；这个现象证明不是——引擎回读的是它**存下的请求**，不是它**落实的事实**。而且这个谎言不可见：除非走进一个真沙箱读**内核报告的** uid 映射，你永远只看到 `"private"`。整个 `VerifyIsolation` 机制、`UsernsMode` 从布尔改成三态、`/readyz` 的 `isolation_verified`，全都从这里长出来。

**auto / require / off 各适合什么场景**

- `auto`（默认）：从 `/info` 的 `SecurityOptions` 判断 daemon 是否真具备 remap 能力（`internal/runtime/oci/client.go:118-133` 的 `hasUserns`，逐字注释写着 `UsernsMode="private"` 不是能被按需兑现的请求）；具备用 `private`，否则退回 `host` 并把降级**写进启动日志与 `/readyz`**（`internal/runtime/oci/backend.go:145-153`）。适合最常见的情况：一台可能配了也可能没配 remap 的宿主。
- `require`：不具备时**拒绝启动**，报错点名三条出路——`daemon.json` 加 `userns-remap`、或设 `userns=off` 显式接受（`internal/runtime/oci/backend.go:129-139`）。适合已决定"身份分离不可谈判"的部署。
- `off`：显式接受共享身份，启动打印 WARNING（`internal/runtime/oci/backend.go:142-144`）。适合开发机，或因文件属主映射连锁影响而不能开 remap 的宿主。

**为什么 `UsernsMode` 永不省略**

留空**不是中性的**：配了 `userns-remap` 的引擎把新容器默认设成 `private`，没配的默认设成 `host`。同一个"未设置"因此在某些宿主上是 `private`、在另一些上是 `host`。后果有二：一是**你无法真正关闭它**——运维设 `userns=off` 想显式接受共享身份，而在一台配了 remap 的宿主上留空会让沙箱悄悄变成 private，`off` 说了等于没说；二是它违反字段钉死规则——未设置的字段是引擎可以替你决定的字段（`docs/DESIGN.md:169-170`）。所以本项目总是**显式写出** `UsernsMode`：`Init` 探测出结论之前保守地写 `host`（`internal/runtime/oci/backend.go:76-84`，注释说"未验证的后端绝不能宣称一个它没确认过的命名空间"），`Init` 之后写经过验证的姿态。

**替代方案**

无条件写 `UsernsMode = "private"` 没选：就是上面那个实测——在没 remap 的 daemon 上什么都不做，却让控制面**宣称**一个不存在的命名空间，严格地比不写更糟。让沙箱以非 root uid 运行没选：不做 remap，进程在宿主上仍是那个 uid，文件属主仍宿主可读。用 rootless 引擎（podman）是另一条路，见 §16。

**怎么验证**

```bash
cat /proc/self/uid_map    # userns=off     预期：0 0 4294967295
cat /proc/self/uid_map    # userns=private 预期：0 100000 65536（宿主 uid 因配置而异）
```

`user_namespace_posture_matches_the_kernel`（`tests/isolation_test.go:185-200`）的逻辑值得逐句读：它从 `/readyz` 读 `isolation_userns`，然后**与 `/proc/self/uid_map` 对照**——报 `off` 时要求映射恰是 `0 0 4294967295`，报 `private` 时要求它**不是**这一行。配置值只被用来选择"该期望哪一行内核事实"，从不被当作事实本身。

还有一处解析细节：`uidMapIsRemapped`（`internal/runtime/oci/isolation.go:290-303`）比较前先把空白折叠，因为内核为对齐会给列补空格，真实字节是 `"         0          0 4294967295"`。直接拿单空格常量比原始行就认不出 identity map，从而**报告一个并不存在的用户命名空间**——一个假阴性，正是这段逻辑要防的错误。

## 6.7 逃逸探针：不只读状态，还亲手试一试

**问题**

到此为止的契约本质上是关于进程**状态**的一组断言（`Seccomp=2`、`CapEff=0`、`NoNewPrivs=1`、uid 映射、`core=0`）。状态全对却仍有一种失败模式：某机制静默失效，而状态字段恰好没反映出来。所以自检不能只读，还要**亲手做一次逃逸的第一步并要求它失败**。

**原理**

自检跑两个探针，选它们的理由是它们是容器逃逸最经典的头两步：

1. **`mount -t tmpfs none /tmp/...`**：挂载是"把宿主根 bind mount 进来"这类经典逃逸的第一步，需要 `CAP_SYS_ADMIN`。一个沙箱若能创建新挂载，它的挂载命名空间就不是墙。
2. **`head -c 1 /proc/kcore`**：读内核内存是一次直接的信息泄漏原语。

两个探针各验证上面讲过的一层：`can_mount=no` 同时验证**能力层与挂载命名空间**——它比单读 `CapEff` 更强，因为证明的是"没有那个能力"**产生了预期效果**；`can_read_kcore=no` 验证**屏蔽路径层**，证明 `/dev/null` 真的挂到了 `/proc/kcore` 上（见 §6.5）。两个探针在 `probeScript`（`internal/runtime/oci/isolation.go:44-51`），写成 shell 串有一个刻意前提：**只读**，唯一写入是一眼看得见的 `/tmp/.sandboxd-probe-$$` 目录、建完立刻删——"自检本身不能造成事故"（`internal/runtime/oci/isolation.go:22-24`）。

**结果三态，且不允许互相冒充**

探针返回值不是布尔的。`can_mount` 可能打印 `YES`、`no`、`UNKNOWN` 之一。`UNKNOWN` 出现在 `mkdir -p "$d"` 本身失败时——探针自己的前置条件都不满足，于是它**明确报"没测到"而不是"没有"**。这就是第三种结果：**无法测量**。自检的契约是三种结果不许互相冒充：**全项通过**列实测值；**有项失败**列 `observed` 与 `want`；**无法测量**（探针没跑起来，或探针与解析器对不上键）返回错误，既不报通过也不报发现漏洞。

第三种有两处守卫。`VerifyIsolation` 在观测键少于 6 个时直接返回错误（`internal/runtime/oci/isolation.go:120-126`），理由直白——"一个没跑起来的检查，不能被报告成一个通过的检查"，最可能原因是镜像里没有 `/bin/sh`。`missingProbeKeys`（`internal/runtime/oci/isolation.go:129-135`）检查报告需要的每个键是否都被探针输出过，有缺就返回 `apperr.InternalErr`，而不是拿空观测去跟期望值比较。这条守卫的来历是一起真实事故：开发期间线上探针键与事实名悄悄分叉，七个事实在拿 `""` 比期望值，其中一个（uid 映射，走不断言的分支）**在一次空观测上报告了成功**（`internal/runtime/oci/isolation.go:26-34`）。三态方法论的整体展开属于 §14。

**本项目怎么做**

探针在 `internal/runtime/oci/isolation.go:35-52`，需要的键清单在 `internal/runtime/oci/isolation.go:59-63`，两条断言在 `internal/runtime/oci/isolation.go:231-234`：

```go
addEq("can_mount", "no",
	"mounting is the conventional first step of a container escape")
addEq("can_read_kcore", "no",
	"/proc/kcore is host kernel memory")
```

探针沙箱走**生产同一条载荷路径**（`internal/runtime/oci/isolation.go:91-99`），用 100 毫核、64 MiB 创建，因此读到的是"一个真沙箱的内核事实"，不是特殊插桩的容器。探针标签刻意不带 `sandbox.id`，好让收敛器把它当不可归属对象留给运维而非回收（`internal/runtime/oci/isolation.go:87-90`）；跑完由 `reapProbe` 用新 context 拆除（`internal/runtime/oci/isolation.go:150-158`），因为调用方 context 此时通常已逼近期限。自检不留东西有独立测试：`TestSelfCheckLeavesNothingBehind`（`internal/runtime/oci/isolation_test.go:286-313`）断言跑一次前后孤儿数不变。

**替代方案**

用读配置（`docker inspect`）代替探针，是本章从第一行就在反对的做法——引擎会回读它没兑现的字段。把检查压成一个"总体结论"也没选：三态一旦压成布尔，就再也说不出**是哪一层**出了问题。

**怎么验证**

```bash
getpcaps $$
mount -t tmpfs none /tmp/m ; echo "mount exit=$?"   # 预期：非 0
head -c 1 /proc/kcore ; echo "kcore exit=$?"         # 预期：非 0
```

契约测试 `mounting_is_denied`（`tests/isolation_test.go:74-79`）与 `kernel_memory_is_unreadable`（`tests/isolation_test.go:81-83`）；daemon 侧 `TestIsolationSelfCheckPassesOnThisHost`（`internal/runtime/oci/isolation_test.go:238-282`）单独断言这两项与 seccomp、no-new-privs、core、`cap_effective`。

## 6.8 "配置不是证据"：全项目的方法论归属地

**问题**

控制面写下去的每一样东西——`CapDrop: ALL`、`SecurityOpt: no-new-privileges`、`UsernsMode: private`、`MaskedPaths: [...]`——都是对一个**外部程序**（容器引擎）发出的**请求**。引擎有自己的版本、`daemon.json`、bug。它可以接受一个兑现不了的请求，存起来，再报告成"已应用"。因此一个通过读回自己写的请求（甚至读回引擎回声）来"验证"的控制面，验证的是零。

**原理。**

本节要明确写出这条贯穿全项目的原则：

> **引擎回读的配置不是证据，只有内核报告的事实才是证据。**

引擎是意图与落实之间的那一层：它的回读是关于**意图**的陈述，不是关于**落实**的陈述。落实发生在某个正在运行的沙箱进程上，由内核记录，也只有从那个沙箱**内部**才读得到。`internal/runtime/runtime.go:134-137` 对 `IsolationReport` 的注释把它写成一句"evidence, not intent"。

**"配置被接受但不生效"的三种具体成因**

1. **引擎接受无法兑现的字段**：`UsernsMode="private"` 落在没有 `userns-remap` 的 daemon 上，被存下、被回读、被忽略（见 §6.6）。特征是：请求**语法合法**、引擎**不报错**、回读**显示已应用**，但内核事实不变。
2. **宿主 `daemon.json` 覆盖**：凡没显式写进 `HostConfig` 的字段都由宿主的 `daemon.json` 决定——一个可变的文件，换引擎版本或换运维就能改，而这份代码不会变。具体的坑例如 `default-ipc-mode` 默认 `shareable`、`default-cgroupns-mode` 在 cgroup v1 上默认 `host`（`docs/DESIGN.md:139-140`；机制见 §5.7）。对策是把每个依赖的字段显式写出，即使它的值恰好等于今天的引擎默认值（完整清单属于 §7）。
3. **内核版本不支持**：请求词汇表里的某个特性在**正在运行的内核**上根本不存在。例如在没有加载 AppArmor 的宿主上指定 `apparmor=docker-default`（见 §6.3）。JSON 合法、引擎收下，但内核无处落实。

三种成因的共同点是：**从配置侧看一切正确。** 只有从内核侧看才能发现差异。

**本项目怎么做：据此做的三件事**

1. **启动自检**：`isolationSelfCheck`（`cmd/sandboxd/main.go:248-281`）在启动时跑一次 `VerifyIsolation`，用**生产同一条载荷路径**起真沙箱、读内核事实，并**逐条**记日志——包括通过的那些，理由是"一个只在出错时才出声的报告，与一个根本没跑过的报告无法区分"（`cmd/sandboxd/main.go:241-247`）。
2. **`/readyz` 的 `isolation_verified`**：`internal/httpapi/handlers.go:67-80`。报告存在时写 `isolation_verified = rep.OK()`，外加 `isolation_checked_at`、`isolation_userns`，失败时附 `isolation_failures`；报告为 `nil` 时写 `false` 并附 note："self-check has not run: the tier and note above are configuration, not verified evidence"。`rep.OK()` 定义为失败项为空（`internal/runtime/runtime.go:148`）。
3. **`/v1/isolation` 的完整证据**：`internal/httpapi/handlers.go:87-98`，需 admin scope，返回完整 `IsolationReport`（每项带 `observed`/`want`/`note` 与时间戳）。要 admin 是因为一份准确的"当前生效控制清单"对策划逃逸的人就是侦察情报（`internal/httpapi/handlers.go:84-86`）。时间戳要跟着走：自检在启动算一次就缓存，因为跑一次要花一个容器，而"过期的、带标签的观测比没有观测更有用，前提是时间戳跟着它"（`internal/service/isolation.go:5-9`）。

**为什么自检未跑起来时报 `false` 而不是报通过**

因为"未验证"与"验证过且良好"是**两个不同状态**。如果自检没跑（比如 `bootstrap.image` 指向的镜像没有 shell）而 `/readyz` 却报 `isolation_verified: true`，这个字段就失去意义——它会在一台隔离已破损的宿主上同样报 `true`。报 `false` 并附说明，读的人才分得清"我们查过，是好的"与"我们还没查过"。所以 `nil` 的情形在 `/readyz` 报 `false`，在 `/v1/isolation` 返回 HTTP 503、`status: not_run`，措辞是"这不是一个通过的结果"（`internal/httpapi/handlers.go:89-96`）；`service.IsolationReport()` 的注释也写明 `nil` 报告"绝不能渲染成一个通过的检查"（`internal/service/isolation.go:18-21`）。

一处工程细节：自检失败**默认不阻止** daemon 启动，只记 warning；但在 `userns = require` 下，"自检跑不起来"与"自检有项失败"都升级成启动错误（`cmd/sandboxd/main.go:254-261`、`cmd/sandboxd/main.go:273-279`）——因为要求 userns 就等于要求对它的证明。

**替代方案**

信任配置、或信任 `docker inspect` 没选：上面那个实测。持续跑自检而不缓存没选：每次都要花一个容器；启动跑一次、带上 `CheckedAt` 是它选的折中。

**怎么验证**

```bash
curl -s $BASE/readyz | jq '{isolation, isolation_note, isolation_verified, isolation_userns}'
curl -s -H "Authorization: Bearer $ADMIN_KEY" $BASE/v1/isolation | jq
```

再跑两条独立测量的互证：`TestIsolationContract/the_self_check_inventory_agrees_with_this_suite`（`tests/isolation_test.go:211-225`）把 daemon 自己启动时的自检结论与独立测试套件的结论对比，两者必须一致，注释写着"两种对同一批事实的独立测量；一致就是证据，不一致意味着其中一个错了、而两者都不可信"；以及 `TestUnrunnableProbeIsAnErrorNotAPass`（`internal/runtime/oci/isolation_test.go:318-329`）——对一个跑不起来的镜像，`VerifyIsolation` 必须返回错误**且不返回报告**。

## 6.9 `ulimit core=0` 与快照的耦合

**问题**

如果进程崩溃时内核写下 core dump，那份 dump 是崩溃时刻进程内存的一份拷贝——**包括它当时握着的任何凭据**。而这个产品的沙箱文件系统会被 `Commit()` 成快照（机制见 §7），快照会被存储并分发给其他沙箱（产品语义见 §8）。于是 core dump 不是"`/tmp` 里的一个大文件"，而是一条**把凭据写进一个可分发产物的外泄通道**。这是数据外泄控制，不是整洁性设置。

**原理**

`RLIMIT_CORE`（用 `ulimit -c` 读）是 core 文件的大小上限，0 表示不写。内核在进程崩溃时查它。与之相邻的是 `kernel.core_pattern`——`/proc/sys` 下的内核可调参数（就在 §6.5 的只读路径集合里）；若它能被写，攻击者可以把它指向管道命令，于是宿主上任何进程下一次崩溃都会让内核以 root 执行那条命令，这是一条不需要内核漏洞的宿主提权路径。`core=0` 从源头把 dump 去掉。

**本项目怎么做**

两个 ulimit 在 `internal/runtime/oci/backend.go:238-252` 钉死：

```go
pinnedUlimits = []ulimit{
	{Name: "core", Soft: 0, Hard: 0},
	{Name: "nofile", Soft: 1024, Hard: 65536},
}
```

`core=0` 与快照的耦合写在它上方的注释里（`internal/runtime/oci/backend.go:241-244`）：本产品把沙箱文件系统提交成快照，因此一次 core dump 会把进程内存——包括它持有的凭据——写进一个会被存储并交给其他沙箱的产物里。注意软、硬上限**都设 0**：只设软上限的话，沙箱内的进程可以自己把它调回硬上限；两个都设 0，就毫无余地。`nofile` 是同一段里的另一条，动机完全不同：文件描述符是宿主全局资源，没有单沙箱上限，一个租户就能耗尽宿主表、饿死其他租户——那是一条跨租户可用性控制（`internal/runtime/oci/backend.go:246-248`）。两者并列正好说明 ulimit 管的是 cgroup 不覆盖的两类资源，一条关乎外泄、一条关乎可用性。自检断言 `core_dump = 0`，附注点明"core dump 写进程内存，其中含凭据，而那份文件系统会被做成快照"（`internal/runtime/oci/isolation.go:229-230`）。

**替代方案**

让 core dump 正常发生、在提交快照前清掉没选：这样你必须在**每一次 commit 之前**可靠地找出并删除每一份 dump——一个可以忘的清理动作，换掉一个忘不了的源头预防。把 core 设成较小的非零值没选：被截断的 dump 仍包含内存的开头若干字节，而密钥很可能就在前几页。只有 0 才什么都不泄漏。

**怎么验证**

```bash
ulimit -c              # 预期：0
ulimit -n              # 预期：1024
```

契约测试 `core_dumps_are_disabled`（`tests/isolation_test.go:66-70`）；daemon 自检见 `internal/runtime/oci/isolation.go:229-230`。再补一条来自 §6.5 的证据：`/proc/sysrq-trigger` 在沙箱内不可写，说明"改 core_pattern"那条路同样被只读挂载挡住。

---

### 本章检验

1. `CapDrop: ALL` 为什么比"只 drop 几个危险能力"更安全？请从"枚举式允许 vs 枚举式禁止"讲清两者的默认方向，并说明当内核未来新增一个能力位时，两种做法各自会发生什么。
2. 一个沙箱的 `/proc/self/status` 显示 `Seccomp: 2`、`CapEff: 0000000000000000`，但它仍然能用 `write` 把磁盘写满、用 `fork` 制造炸弹。这说明 seccomp 与 capabilities 各自"防住的"是什么？本项目分别用哪两个机制去接住这两个具体行为（或如实拒绝）？
3. `HostConfig.UsernsMode` 被引擎回读为 `"private"`，而沙箱内 `/proc/self/uid_map` 仍是 `0 0 4294967295`。请逐字段解释这行 `uid_map` 为什么等于"什么都没映射"，以及这个现象为什么足以推翻"读引擎回读的配置就算验证"这一做法。
4. `/readyz` 在自检从未运行时返回 `isolation_verified: false` 并附一段 note，而不是返回 `true`。如果它返回 `true`，会具体坏在哪里？请构造一个宿主场景，说明这个 `true` 会误导谁、误导到什么后果。
5. **动手任务**：在一台有 Docker 的机器上运行
   `docker run --rm -it --cap-drop ALL --security-opt no-new-privileges --userns=private <image> sh`，
   在容器内依次执行：`grep -E 'Seccomp|NoNewPrivs|Cap' /proc/self/status`、`capsh --print`、`cat /proc/self/uid_map`、`head -c 1 /proc/kcore; echo $?`、`ulimit -c`。然后把 `CapEff` 的十六进制值按 §6.1 的方法逐位还原成能力名；再运行一次**去掉** `--cap-drop ALL` 的同一条命令，观察 `CapEff` 变成什么，并解释这个新值为什么是"默认并不安全"。最后对比两次 `cat /proc/self/uid_map` 的输出，并回答：仅凭 `docker inspect` 的回读，你能否区分"真的映射了"和"什么都没映射"？


# §7 文件系统、镜像与容器运行时

## 7.1 为什么走 Docker Engine HTTP API，而不是 `exec.Command("docker", ...)`

**问题**
用命令行拼串驱动引擎，第一次就会撞上三件事。宿主上没装 `docker` 二进制，创建全部失败，而报错是"executable file not found"——它和"镜像不存在"共用同一个失败出口。要判断容器是否已存在，只能抓回 stderr 去匹配 `already in use`；引擎一升级改了措辞，幂等判断就静默失效。要把 `docker inspect -f '{{.State.OOMKilled}}'` 解析成布尔值，模板语法一改就析出空串，"这次是 OOM"被误判成"没 OOM"。三者的共同点：**把一个结构化的机器接口，降级成了给人看的文本接口**。

**原理**
Docker Engine 是常驻宿主的 HTTP 服务，监听 Unix socket（默认 `/var/run/docker.sock`）；`docker` CLI 只是它的一个客户端，把子命令翻译成 HTTP 请求、再把 JSON 渲染成表格。所以"用 CLI"和"用 API"不是两条路线，而是同一协议的两层封装——API 是里层，多做一层封装只是多一层会犯错的手工翻译。逐条对比：

**每次 exec 起一个进程。** `docker` 是几十 MB 的静态二进制，每次调用都要 fork/exec、加载、解析、连 socket、格式化输出、退出。API 客户端在进程内长期持有一条连接，一次调用只是往已有连接写一帧。并发创建时这不是"快一点"，而是"每次调用多一个进程生命周期"——而进程表正是 §5 里被 pids 上限保护的那类宿主全局资源。

**解析人可读输出做错误处理。** CLI 失败只给你一个退出码和一段 stderr。`docker create` 返回 1 时，可能是镜像不存在、镜像名拼错、容器同名、socket 权限不足、daemon 没起、磁盘满、某字段非法。**退出码 1 可能是任何一种失败。** 唯一能区分它们的办法是匹配 stderr 文本，而那段文本给人读、没有兼容性承诺、随版本变。Engine API 返回的是 HTTP 状态码加 JSON 错误体 `{"message": "..."}`：`404` 是对象不存在、`409` 是状态冲突、`400` 是请求被拒。状态码是协议的一部分，文本只是描述。

**并发下的稳定性。** CLI 模式下要同时跑 80 个创建，就得起 80 个子进程各自处理管道；某个子进程因编码或引号问题挂住，你就会漏掉一个已真实创建的容器——在 §10 的对账里，它表现为"后端有对象、本地无记录"的孤儿，而这本不该产生。API 模式下并发就是多个 goroutine 共用一个 `http.Client`，连接池本身并发安全（§3 的原语在这里被标准库封装好了）。

**Unix socket 的权限模型。** 走 API 时，客户端只是 dial 一个 socket 文件，谁能连它由该文件的属主、属组、mode 位决定。走 CLI 时，你把"访问 socket"转包给了一个可执行文件：要么让 `sandboxd` 以能读该 socket 的身份运行，要么把它加进 `docker` 组、即获得 fork 任意子进程的能力。后者在一个"给不可信代码提供执行环境"的产品里是白送的权限面；只要涉及拼串，就还多一条命令注入路径。`docs/POSITIONING.md:92-95` 把这条写成明确决策：生产代码从不 `exec.Command("docker", ...)`，在 `cmd/` 与 `internal/` 下 grep 该模式零命中。

**流式输出。** 一次 exec 是两条独立、可能持续很久的字节流。Engine API 的 `/exec/{id}/start` 返回**多路复用流**：每段前有 8 字节头，第一个字节标流类型（1 是 stdout、2 是 stderr），后四字节是大端长度。按头切段，天然分开两条流。走 CLI 则只能把输出当单一文本读回，分不清来源。本项目在 `internal/runtime/oci/backend.go:692-713` 正是按这 8 字节头解复用：`header[0]` 判流、后四字节拼长度、`streamType == 2` 归到 stderr。

**最后一条，视角最高：容器运行时不一定是 `docker` CLI。** 生产镜像里不该有 `docker` 二进制；而"CLI 在不在"和"引擎能不能被访问"是两件事。API 客户端把"怎么访问引擎"与"用哪个工具访问"解耦：换一个 socket（colima、Docker Desktop、远程 TCP）、换一台机器，代码一行不改。这也是零第三方依赖的路径——客户端用标准库 `net/http` 手写。

**本项目怎么做**

`NewClient` 做三件事：找端点、建客户端、探活。端点是一个有界的候选列表（`internal/runtime/oci/client.go:41-55`），顺序即优先级：显式配置 `cfg.DockerHost`（来自 `sandboxd.conf` 的 `docker_host` 键，`internal/config/config.go:322-323`）、环境变量 `DOCKER_HOST`、开发机常见位置（colima、Docker Desktop）、最后是 Linux 标准路径 `/var/run/docker.sock`。为什么要猜多个？因为本项目要能在 macOS 开发机上自测（`docs/DESIGN.md:18-19`），而开发机的 socket 路径各不相同。但猜必须有界：列表走完还没探通就返回不可用错误，而不是无限盲试。

HTTP 客户端的构造值得逐字读（`internal/runtime/oci/client.go:62-72`）：

```go
c := &Client{base: "http://" + pseudoHost}
c.hc = &http.Client{
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
		DisableKeepAlives: false,
	},
	Timeout: 10 * time.Minute, // execs may legitimately run long; per-call ctx deadlines do the real work
}
```

四点。**`base` 是假主机名** `http://sandboxd-docker`（`client.go:28`），永不解析；真正决定连到哪的是 `DialContext`，它把 `network`/`addr` 直接交给 `net.Dialer`，绕过 DNS，让 URL 里的主机名只作占位符。**`DisableKeepAlives: false`** 是连接复用，它是一个功能依赖而非调优：所有调用共享同一条到 daemon 的连接池。**`Timeout: 10 * time.Minute`** 只是兜底，注释说明真正的超时在每次调用的 `context` 上（`client.go:213` 用 `http.NewRequestWithContext`）——只有"每调用一个 ctx 截止时间"，才能让"查询接口 3 秒超时"与"exec 5 分钟超时"共存。**Dial 的 5 秒独立超时**保证找一个不可达的 socket 不会拖满整个调用预算。

响应侧有两条约束。`do`（`client.go:188-202`）在 2xx 时用 `io.LimitReader(resp.Body, respLimit)` 截断，`respLimit` 是 8 MiB（`client.go:29`），防止畸形响应把控制面堆爆。非 2xx 交给 `statusErr`（`client.go:228-248`）：

```go
switch status {
case http.StatusNotFound:
	return apperr.New(apperr.NotFound, fmt.Sprintf("docker: not found on %s: %s", path, msg))
case http.StatusConflict:
	return apperr.ConflictErr("docker conflict on %s: %s", path, msg)
case http.StatusBadRequest:
	return apperr.Invalid("docker rejected %s: %s", path, msg)
default:
	return apperr.Unavailable("docker error %d on %s: %s", status, path, msg)
}
```

**这就是"能把引擎错误体解析成结构化错误"的机制。** `statusErr` 先尽力取 JSON 里的 `message` 当人类可读描述（取不到就退回原始 body），但**分支依据是 HTTP 状态码**，映射到本项目稳定的 `apperr` 码。为什么这个映射是设计而非装饰？因为服务层要据此做决定：`Create` 把 `NotFound` 翻译成"基础镜像不在本机，请导入 rootfs 或换一个可达镜像"（`internal/runtime/oci/backend.go:348-353`）；`Start` 把 `Conflict` 当作"已经在跑"，幂等返回成功（`backend.go:378-381`）。这些判断全部建立在错误码上，没有一处去读 message 文本——文本只给人看。这就是"退出码 1 可能是任何一种失败"的正解：**把失败分类做成协议的一部分，而不是靠猜一段描述。**

**替代方案**

生产代码继续用 CLI：另有隐藏代价——它把"引擎可达"和"镜像里有 docker 二进制"绑定，而本项目有一个气隙最小化镜像的部署形态（§7.6），往里塞 docker 客户端毫无道理。用官方 Docker SDK for Go：错误也结构化，但违背零依赖硬约束（`go.mod` 只有 `module` 与 `go 1.26` 两行，`docs/POSITIONING.md:133`），会带来依赖树、版本漂移与供应链面；而本项目需要的 API 面很小，手写更可控。直接用 containerd 或 runc：那等于自己实现镜像分发、快照管理与网络装配的大半工作，是几万行已调好的代码（`docs/POSITIONING.md:88-89`），而本项目卖的是引擎**之上**的治理层。

但有一处必须点明的对照，否则结论会被读成"CLI 一定错"：**测试清理确实用了 `exec.Command("docker", ...)`**（`tests/harness_test.go:568-586`）：

```go
out, err := exec.Command("docker", "ps", "-aq", "--filter", "label="+labelSandbox+"="+ref.id).Output()
```

理由写在注释里：这段代码跑在 `t.Cleanup` 里，**必须在 daemon 已经死掉的情况下也能清理容器**——而被测对象正是 daemon 本身，它不能被 daemon 的 API 依赖住。同一个项目两种选择并存不是自相矛盾：**生产用 API 是因为要结构化错误与长期连接；测试清理用 CLI 是因为要一个不依赖被测进程的兜底。** 目标不同，工具不同。

**怎么验证**

```bash
grep -rn 'exec.Command("docker"' cmd/ internal/   # 预期：无输出
curl -s --unix-socket /var/run/docker.sock http://localhost/_ping   # 预期：OK
```

第一条证明生产代码零 CLI 调用（同样的 grep 在 `tests/` 会命中 `harness_test.go`，正好对上上一段的对照）。第二条越过 CLI 直接对引擎发协议层请求，`curl` 就是一个与本项目同构的 HTTP 客户端——本项目探活打的就是同一个端点（`client.go:94-97` 的 `ping`）。

## 7.2 OCI 三规范与运行时栈：为什么"容器"不是内核概念

**问题**
若以为"容器"是内核里的一个对象，你会连问题都问错：你会问"怎么向内核查询这个容器有多强"——而内核对"容器"这个词一无所知。你也会以为"换成更安全的容器类型"是一次内核开关，从而低估从容器档升到 microVM 档要重写什么，并看不懂为什么同一个"容器"概念在 Docker、containerd、runc、Kubernetes 文档里含义不同。

**原理**
**"容器"是一组用户态工具对内核特性的约定，不是内核概念。** 内核只知道：命名空间、cgroup（见 §5）、挂载树、以及 capability、seccomp、LSM 标签（见 §6）。把这些按某种顺序配好、再用一个进程带起来，就得到一个容器——所以"建容器"在内核层面是一串系统调用的组合，而不是一个"创建容器"的系统调用。

既然内核不管，就必须有**文件格式约定**规定"这堆配置长什么样、谁能读懂"。这就是 OCI 三份规范：**runtime-spec** 管怎么运行一个容器，定义了 `config.json`（跑什么进程、用哪些命名空间、挂哪些目录、cgroup 限多少、capability 与 seccomp 怎么设），runc 读的就是它；**image-spec** 管镜像怎么表示（manifest 列出各层摘要与媒体类型、config 记环境变量与入口、再加一串层，层用内容摘要标识）；**distribution-spec** 管镜像怎么在 registry 上推拉（HTTP API、分块上传、按摘要校验）。

一句话串起来：**image-spec 说镜像长什么样，distribution-spec 说怎么把它搬到本机，runtime-spec 说怎么把本机这个镜像变成一组被隔离的进程。** 少了任何一份，不同厂商的工具就没法互通。

有了规范，运行时栈这条链就清楚了：

```
dockerd    ── 对外的 HTTP API 服务；镜像、网络、卷、编排（§7.1 直连的就是这层）
containerd ── 镜像分发与快照管理、容器任务的真实生命周期
runc       ── 按 runtime-spec 组装 namespace/cgroup/挂载，exec 出容器进程
Linux 内核 ── namespace + cgroup + 挂载 + capability/seccomp/LSM
```

每层只做自己那一段。`dockerd` 是面向用户的编排面（也是替换缝所在，`docs/POSITIONING.md:96-99`）；`containerd` 是镜像与任务的持久管理层；`runc` 最薄，把容器进程拉起来之后就退出——因为"容器"没有常驻守护进程，被隔离的进程就是普通进程。这正是上一段的体现：**栈底没有"容器对象"，只有一组配好内核特性的进程。**

**本项目怎么做**

本项目只消费 API，不实现规范。它把版本号钉在一个常量上（`internal/runtime/oci/client.go:26-31`）：

```go
const (
	apiVersion    = "v1.44"
	pseudoHost    = "sandboxd-docker"
	respLimit     = 8 << 20 // cap JSON response reads
	defaultExecTO = 5 * time.Minute
)
```

`v1.44` 是 **Docker Engine API 的版本**，不是 OCI 规范的版本——两者常被混为一谈。Engine API 版本决定请求路径（`/v1.44/containers/create`）；OCI 规范版本决定镜像与 config 的格式。本项目只关心前者，后三层的规范细节全由引擎处理掉了。

本项目的"我知道自己处在哪一档"表达在 `IsolationTier()` 上（`internal/runtime/oci/backend.go:96-98`）：

```go
// IsolationTier is honest about what a container gives you: a shared kernel.
// There is no configuration of the Docker Engine API that changes this.
func (b *Backend) IsolationTier() string { return runtime.TierContainer }
```

`internal/runtime/runtime.go:113-121` 定义了两档：`container-shared-kernel` 与 `microvm`。为什么做成接口方法而非一行注释？`runtime.go:155-163` 写明：这样新增后端**必须**声明等级，且 daemon 能在运维要求一个宿主给不出的档位时**拒绝启动**而不是静默降级。这正建立在"容器是用户态约定"这个认识上：既然每层都可被替换，任何一层的强度声明就必须显式。

**替代方案**

直接调 `runc`（自己填 `config.json`）：那样就绕过了 `containerd`，得自己接手镜像拉取、快照管理、层复用——正是 §7.3 与 §7.6 要讲的整套机制，等于把"复用生态"（`docs/POSITIONING.md:90`）换成"自建半个引擎"。另一个方向是**换掉 `runc` 这一层**：既然 runtime-spec 把"怎么跑一个容器"标准化了，就能换一个行为兼容、隔离更强的实现，gVisor 与 Kata 走的正是这条路，本项目的替换缝也留在这里（`docs/POSITIONING.md:96-99`）。档位强度排序与取舍见 §16。

**怎么验证**

```bash
docker version
docker info --format '{{.Runtimes}} {{.DefaultRuntime}}'
```

第一条把 Client、Server、containerd、runc 四段版本**分开**打印——四段分离正是运行时栈分层的外在证据。第二条打印可用 runtime 与默认 runtime（例如 `map[runc:{...}] runc`），它对应上一段的替换缝：把 `default-runtime` 改成 `kata-runtime`，本项目代码一行不改。

## 7.3 镜像与层：overlayfs、COW 与 `commit` 的物理机制

**问题**
三个看似孤立的问题其实是一件事：为什么 `docker run` 毫秒级返回而开虚拟机要等秒级？为什么改镜像里一个字节不会复制整个镜像？`docker commit` 到底做了什么、为什么比 `docker run` 贵得多？不搞清这三个，就无法理解 §7.6 在构造什么，也无法理解 §8 的快照为什么与 `commit` 这个动作耦合。

**原理**
先立思想：**多个只读层 + 一个可写层，叠成统一的目录视图。** 这叫 unionfs（联合文件系统）。Linux 上的现代实现是 **overlayfs**，一次挂载要喂四个目录：

- **`lowerdir`**：一个或多个**只读**层，按顺序叠放，写在最前的优先级最高。
- **`upperdir`**：唯一的**可写**层，所有改动落到这里。
- **`workdir`**：overlayfs 自用的暂存目录，必须与 `upperdir` 同一文件系统，用于原子地准备 rename/副本，不出现在合并视图里。
- **`merged`**：合并后的视图，即容器进程看到的根目录树。

**读**：从最上层往下找，命中即停；文件只在 `lowerdir` 就直接读、不复制。**写**：要改的文件在 `lowerdir` 时，先把整个文件**复制**到 `upperdir`，再在副本上改——这就是 **copy-on-write（COW，写时复制）**；此后 `merged` 里这个名字指向 `upperdir` 那份，`lowerdir` 原件不动。**删**：不能真删只读层（它可能被别的容器共享），于是在 `upperdir` 放一个 whiteout 标记，让合并视图"看不见"它。

三个问题一次回答：**建容器廉价**，因为只需准备一个空 `upperdir`、把镜像现有只读层挂成 `lowerdir`、再挂载合并——没有内核启动，也没有根文件系统拷贝。**改一个文件不复制整个镜像**，因为 COW 只复制被动到的那一个文件。**`commit` 昂贵**，因为它要把 `upperdir` 的内容打包成一份新的、不可变的只读层——这是真在搬数据。

**层的复用与去重**是第二半。每层用**内容摘要**（对内容做哈希）标识：两个镜像若共享同一段历史（都从 `alpine` 开始），前几层摘要相同，磁盘上只有一份。`docker pull` 第二个基于 alpine 的镜像时 "already exists" 说的就是这件事。所以镜像共享的底层不是文件，是**按摘要索引的只读层**。层的语义是"相对上一层的一组文件变更"，`docker history` 逐行展示的就是这条变更链。

一个最小例子，overlayfs 可手工挂起来观察，不需要 Docker：

```bash
mkdir -p lower upper work merged
echo original > lower/a.txt ; echo readonly-b > lower/b.txt
mount -t overlay overlay -o lowerdir=lower,upperdir=upper,workdir=work merged
cat merged/a.txt          # 预期：original
echo modified > merged/a.txt   # 触发 COW：a.txt 被复制到 upper 再改
cat upper/a.txt           # 预期：modified   ← 内容已复制到可写层
cat lower/a.txt           # 预期：original   ← 只读层原件未动
ls upper                  # 预期：a.txt       ← 只有写过的文件出现在 upper
```

`upper` 里只有 `a.txt`、没有 `b.txt`，这就是 COW 的证据：没动过的文件不复制。

**本项目怎么做**

`commit` 的实现只有十行（`internal/runtime/oci/backend.go:766-776`）：

```go
func (b *Backend) Commit(ctx context.Context, ref, tag string) (string, int64, error) {
	if _, err := b.c.do(ctx, "POST",
		"/commit?container="+urlQueryEscape(ref)+"&repo="+urlQueryEscape(tag), nil, nil); err != nil {
		return "", 0, err
	}
	size, err := b.imageSize(ctx, tag)
	if err != nil {
		return tag, 0, err
	}
	return tag, size, nil
}
```

`POST /commit?container=<ref>&repo=<tag>` 让引擎把该容器的**可写层**打包成新镜像并打上 `tag`；返回后用 `imageSize`（`backend.go:797-809`）读回字节数，作为"这次快照占了多大"的计量依据。所以 `Commit()` 的产物是**一份新的只读层**，记录这个容器在它的镜像之上**改动了什么**。语义边界要说清：`Commit()` 只负责产生这份层和它的 tag；"快照"这个对象、引用计数、配额与回收是产品语义，归 §8，§7 只讲这个动作在文件系统层面做了什么。快照路径上 tag 的构造在服务层（`internal/service/snapshot.go:62-63`）：

```go
// image repository names must be lowercase; sandbox IDs are uppercase
tag := strings.ToLower("sandboxd-snap-" + sn.ID)
imageRef, size, err := s.Backend.Commit(ctx, sb.Backend.Ref, tag)
```

那行注释是一个真实约束：镜像仓库名只能小写，本项目 ID 是大写，必须显式 `ToLower`。`size` 读回后立刻用于配额检查（`snapshot.go:67-70`），不通过就把刚落盘的镜像**删掉**，不留"告诉了调用方失败、东西还在盘上"的中间态。

还有一处与 `commit` 耦合的决定必须在这里点明：根文件系统被**刻意保持可写**（`ReadonlyRootfs=false`，`backend.go:284`）。`backend.go:191-196` 的注释说清了理由：

```go
// ReadonlyRootfs is pinned false on purpose, not by omission. A read-only
// root is the usual next hardening step, but it is incompatible with this
// product's snapshot model: Commit() captures the container's writable
// layer, so a read-only root would produce empty snapshots. Revisit only
// together with a writable-workspace overlay design.
```

只读根这个通常的加固动作会直接**让快照变成空的**——它关掉了 `upperdir`，而 `Commit()` 捕获的正是 `upperdir`。这是一个"看起来更安全、实际破坏另一项能力"的反例，所以它被写成"刻意 false 而非遗漏"。

**替代方案**

换存储驱动：除 overlayfs 外还有 `devicemapper`（每层一个块设备快照，开销大）和 `vfs`（每层完整目录拷贝，最简单也最慢，启动一次拷一遍根文件系统）。不选它们，是因为 overlayfs 在"层复用 + 启动开销 + 内存占用"三者间最平衡、且是当前主流默认。不用 `commit` 而用 `docker export/import` 整盘打包：区别在粒度——`commit` 只打包**差分层**，产物小、能继续共享底层；`export` 把整个根文件系统打成扁平 tar，产物大、丢失层复用。§7.6 的离线导入确实构造扁平 tar，那是因为它要从**零**造基础镜像、没有可共享的底层；而"从装好依赖的沙箱产出一份可复用环境"这个场景里，差分层才是对的选择，所以快照走 `commit`。

**怎么验证**

```bash
docker image ls                                   # 每个镜像的总大小
docker history <image>                            # 逐层：创建命令、大小、层 ID
docker inspect <image> --format '{{json .RootFS.Layers}}'
docker inspect <container> --format '{{json .GraphDriver.Data}}'
```

`docker history` 逐行列出层，每层有独立 `SIZE`——这是"层是增量变更"的直接证据，也解释了为什么多个镜像共享底层时 `docker image ls` 的 SIZE 加总会大于磁盘实际占用。最后一条预期返回含 `LowerDir`/`UpperDir`/`MergedDir`/`WorkDir` 四个键的 JSON，正好对应本节四个目录；`LowerDir` 可能是**多个**冒号分隔的目录，说明底层镜像有多层。更直观的是进沙箱看 `cat /proc/mounts | grep overlay` 的 `lowerdir=` 那一串。

## 7.4 HostConfig 字段钉死方法论

**问题**
这是整章最重要的一节，因为它的失效方式最隐蔽。你在请求里写了 `NetworkMode=none`、`CapDrop=ALL`，于是以为"没写的就是安全的"。但 Docker 有一整套**引擎级默认值**，写在宿主的 `daemon.json` 里。**凡是没有显式写入 `HostConfig` 的字段，就由 `daemon.json` 决定——而它是一个可变的文件，换一个引擎版本、换一个运维就能改，代码却没有任何变化。** 更糟的是有些字段的引擎默认值本身就比能接受的要弱：你读代码觉得自己没暴露它、所以安全，而真实生效的默认可能是"允许别的容器加入本沙箱的 IPC 命名空间"。

**原理**
先看清一条缝：**"生成请求"和"引擎决定默认值"是两个时刻、两个地方。** 控制面在自己进程里拼一个 `HostConfig`；引擎收到后，对**你没提的字段**填入自己的默认值。最终生效的配置 = 你显式写的字段 ∪ 引擎默认填的字段。只要后一半存在，你写在代码里的安全契约就是**不完整的**——完整的那份散落在宿主配置、引擎版本和运维历史里。

由此得到方法论：**沙箱依赖的每个字段都必须显式写出，即使它的值恰好等于今天的引擎默认值。** 因为"今天恰好相等"不是保证，是一个关于宿主当前状态的偶然事实。钉死的目的是把契约从三个地方（代码、daemon.json、引擎源码）收回到**一个地方**（创建请求），让它可审查、可 diff、可被一条测试守住。

**本项目怎么做**

`hostConfig` 结构体的注释就是这条方法论的原文（`internal/runtime/oci/backend.go:172-182`）：

```go
// hostConfig is the security contract. Every field a sandbox depends on is
// written here explicitly, even where the value matches today's engine
// default, because anything omitted is delegated to the host's daemon.json —
// a mutable file that a different engine version or a different operator can
// change without this code changing. The fields that genuinely have a
// daemon-level default capable of weakening a sandbox are IpcMode
// (default-ipc-mode), CgroupnsMode (default-cgroupns-mode), UsernsMode
// (userns-remap), Ulimits (default-ulimits), SecurityOpt's seccomp/apparmor
// entries (seccomp-profile) and Runtime (default-runtime, ...). The rest are
// pinned so that the contract can be reviewed in one place instead of
// reconstructed from the engine's source.
```

逐字段看 `createPayload`（`backend.go:254-297`）里每一项钉死的原因与"若不钉死会怎样"。这不是一张要背的表，而是一条链：每个字段背后都有一个"被引擎默认值覆盖后沙箱会变弱成什么样"的具体后果。

`NetworkMode` 钉为 `none`（`backend.go:278`）。若不钉死，引擎默认接一个 bridge 网络，沙箱立刻获得带 NAT 的出口接口，"数据不出域"当场失效。各模式创建什么见 §7.5。

`IpcMode` 钉为 `private`（`backend.go:279`）。**第一个重点坑。** 默认值受宿主 `default-ipc-mode` 控制，而那个默认是 `shareable`（`docs/DESIGN.md:139`）。`shareable` 的含义不是"每个容器有私有 IPC"，而是"**允许别人加入**这个容器的 IPC 命名空间"。不钉死，两个本不该互通的沙箱就能通过 POSIX 消息队列、共享内存、信号量互相说话——多租户下的横向移动路径。关键在：`shareable` 恰好不是一个"看起来不安全"的名字，读起来像"可共享是好事"，极易漏掉。

`CgroupnsMode` 钉为 `private`（`backend.go:282`）。**第二个重点坑，而且它依赖宿主的内核。** 默认受 `default-cgroupns-mode` 控制，而那个默认**在 cgroup v1 上是 `host`**（`docs/DESIGN.md:140`）。也就是说：同一份代码跑在 cgroup v1 宿主上，沙箱会看到**宿主的完整 cgroup 树**——谁在跑、限额多少，全是侦察情报；跑在 v2 宿主上默认可能变成 `private`，沙箱看到的是自己。**同一个"不写"，行为随宿主改变，正是钉死要消灭的不确定性。** cgroup v1 与 v2 的差别见 §5.8。

`PidMode` 显式写成**空字符串**（`backend.go:280`）。这一项易被误读为"没写"：省略字段是"交给引擎/宿主决定"，显式空才表示"用默认的容器私有 pid 命名空间"。它是同一逻辑的另一种形式——把"我要默认行为"也做成显式的选择，而不是一个碰巧。

`UsernsMode` 钉为 `private` 或 `host`，**永不省略**（`backend.go:281`）。为什么永不省略？因为留空在这里**不是中性的**：配了 `userns-remap` 的引擎会把新容器默认设成 `private`，于是同一个"未设置"在某些宿主上是 `private`、另一些上是 `host`，而 `userns=off` 的本意是"显式接受共享身份这一档"——字段留空，它就无法真正关闭。用户命名空间的机制与那个"配置被接受但不生效"的实测归 §6，本节只到"为什么这个字段不能省"。

`Privileged` 钉 `false`（`backend.go:283`）；`ReadonlyRootfs` 钉 `false`（`backend.go:284`，理由见 §7.3）。`CapDrop`/`CapAdd` 钉 `ALL`/`[]`（`backend.go:285-286`），注意 `CapAdd` 是**显式的空切片**而非 nil：nil 会被 `omitempty` 丢掉，等于把这个字段交给引擎默认值；空切片序列化成 `[]`，是明确的"什么都不要加"。capability 裁剪挡住什么归 §6。

`SecurityOpt` 钉 `no-new-privileges`，再按宿主是否报告 AppArmor 追加 `apparmor=docker-default`（`backend.go:257-263`）。不钉死则 seccomp 与 AppArmor profile 会受宿主级 `seccomp-profile` 影响。AppArmor 有一层保护：只在 `/info` 报告宿主有 AppArmor 时才写 `apparmor=`，因为在一个没有 AppArmor 的宿主上指定 profile 会**报错**（`docs/DESIGN.md:150`）——这正是本项目别处反复出现的"先探测能力、再决定要不要声明"。

`MaskedPaths`/`ReadonlyPaths` 钉成引擎默认集合的**显式副本**（`backend.go:226-236`）。理由与别的不同：它的值通常**等于**默认值，钉死不是为改行为，而是**防版本漂移**。屏蔽集合（哪些 `/proc`、`/sys` 路径不可见/只读）没有兼容性承诺，未来引擎版本可悄悄改它。`backend.go:222-225` 点明后果："一个能读 `/proc/kcore` 或能写 `/proc/sys` 的沙箱，是实质不同的沙箱。"

`Ulimits` 钉 `core=0/0`、`nofile=1024/65536`（`backend.go:238-252`）。不钉死则默认受宿主 `default-ulimits` 影响。`core=0` 不是整洁性设置而是**数据外泄控制**——本产品会把沙箱文件系统 `Commit()` 成快照（§7.3），一次 core dump 会把进程内存（含凭据）写进一个会被存储、还会分发给别的沙箱的产物。`nofile` 是**跨租户可用性控制**——文件描述符是宿主全局资源，没有每沙箱上限，一个租户就能耗尽宿主文件表、饿死其他租户。

`Resources` 钉 CPU、内存、关闭 swap、pids 上限（`backend.go:272-277`），不钉死则单个沙箱能耗尽宿主资源；四个字段怎么换算成 cgroup 数值归 §5.9。`Init` 与 `RestartPolicy` 也在同一载荷里（`backend.go:291-294`）：`Init: true` 让引擎在 PID 1 位置插一个小 init 收僵尸，`RestartPolicy` 钉 `no` 保证沙箱崩溃后引擎不会自动拉起（自动重启会与状态机对"是否还活着"的判断打架），与进程管理的关系见 §5.5。

收起来看：钉死的价值不在于"每个字段都挡住一次攻击"，而在于**把契约的可核对面从"宿主 + 引擎版本 + 运维历史"收缩到"这一个结构体"**。`internal/runtime/oci/isolation_test.go:15-82` 的 `TestCreatePayloadPinsEveryIsolationField` 就是守卫：它逐项断言上面这些值——因为删掉一个字段不会让别的测试失败，沙箱照常启动、只是变弱了，所以**载荷本身需要一个测试，而不只是端到端测试**。`isolation_test.go:84-113` 的 `TestPinnedFieldsSurviveJSONEncoding` 更进一步，在**编码后的字节**上断言，因为"Go 值正确、wire 载荷却因一个 struct tag 漏掉字段"是这类错误的真实形态。

**替代方案**

一个自然的替代是：**不钉每个字段，只维护一份推荐的 `daemon.json` 发给运维**。不选的理由：这份文件在宿主文件系统上，不在代码库里，改没改、部署的是哪版，控制面看不见也无法核对。另一个替代是"只钉那几个已知危险字段、其余交给默认"——这正是本项目**没有**做的：注释确实点名了六个"真有引擎默认值能削弱沙箱"的字段（`backend.go:177-182`），但项目选择连"值恰好等于默认值"的字段也一并钉死，理由是版本漂移。代价是结构体较长、需要人维护；收益是安全契约能被一条测试守住、能在一个文件里读完。

**怎么验证**

```bash
docker inspect <container> --format '{{json .HostConfig}}' | python3 -m json.tool
```

预期：`NetworkMode` 为 `none`、`IpcMode` 与 `CgroupnsMode` 为 `private`、`CapDrop` 为 `["ALL"]`、`CapAdd` 为 `[]`、`Privileged` 与 `ReadonlyRootfs` 为 `false`、`UsernsMode` 为 `host` 或 `private`（**绝不为空**）。提醒一个贯穿全项目的立场：`docker inspect` 读到的是**你请求了什么**，证明请求被接受了，**不证明内核照做了**——那要从沙箱内部读内核事实（见 §6）。§7.8 会把这两侧放在一起对照。

## 7.5 网络模式与 egress 策略：`none` / `bridge` / `host` 的物理差别

**问题**
一个多租户沙箱平台，默认不给沙箱网络，被注入的产物就没法把上下文或文件 `wget` 出去。但"不给网络"有多种实现，安全性差得很远。最危险的是"看起来关了、实际没关"：容器里跑一套 iptables 白名单，你以为挡住了，可规则运行在**沙箱自己触手可及的地方**，工作负载能改它。要用对策略，先看清引擎三种网络模式各自创建了什么、连接了什么。

**原理**
Docker 的 `NetworkMode` 有三种基础形态（net 命名空间的隔离机制见 §5.6，本节只讲引擎侧装配）：

**`none`**：为一个新容器创建一个空的网络命名空间，里面只有 loopback 接口 `lo`，**不接任何东西**。没有 `eth0`、没有 veth pair，路由表为空。语义是"有个独立网络栈，但不与外界相连"。攻击面是"**没有路**"。

**`bridge`**：引擎在宿主维护一个虚拟网桥（默认 `docker0`），每起一个容器就创建一对 veth（虚拟网线），一头放进容器的网络命名空间、成为容器里的 `eth0`，另一头接在网桥上；宿主 iptables 还要为容器流量做 masquerade（NAT）。**语义是"有一条路，靠规则管出口"**——`bridge` 本身恰恰是为让容器能出网而建的。要在这模式上做限制，就得叠加出口规则，而那些规则运行在沙箱能触及的地方，就可能被改、可能漏。

**`host`**：容器**不**创建新的网络命名空间，直接加入宿主的——容器的 `eth0` 就是宿主网卡、路由表就是宿主路由表、监听的端口就是宿主端口。语义是"**没有任何网络边界**"，性能最好（省掉网桥与 NAT），隔离为零。

`docs/DESIGN.md:139-140` 点出 `IpcMode`/`CgroupnsMode` 的默认坑，而 `NetworkMode` 的坑在于：它的引擎默认是 `bridge`，也就是**默认是一个有出口的网络**。

**本项目怎么做**

`NetworkMode` 写死为 `none`（`internal/runtime/oci/backend.go:278`）。`none` 与"有接口但防火墙拦住"是**本质不同的两种姿态**：前者攻击面是"没有路"，后者是"有一条路，靠规则拦"。本项目选前者，因为它是可被内核事实证明的承诺，而非依赖规则正确性的承诺。§5.6 里沙箱内的四条观察命令（`ip route` 空、`ls /sys/class/net` 只有 `lo`、`wget`/`nslookup` 失败）证明的正是"没有路"这个物理事实。

出口策略这一层，本项目做了一件看着不够"功能丰富"、但符合其诚实语义的事。强制执行入口是一个纯函数（`internal/runtime/runtime.go:239-250`）：

```go
// CheckEgress enforces the honest-refusal contract: a backend that cannot
// truly enforce a policy must refuse the create.
func CheckEgress(b Backend, policy domain.EgressPolicy) error {
	switch policy.Mode {
	case "", domain.EgressNone:
		return nil
	case domain.EgressAllowlist:
		return apperr.Unimpl("backend %q cannot enforce egress allowlists; use egress=none or deploy the firecracker backend (roadmap)", b.Name())
	default:
		return apperr.Invalid("unknown egress mode %q", policy.Mode)
	}
}
```

`none`（也是默认）放行；`allowlist` 返回 `apperr.Unimpl`；未知模式返回 `apperr.Invalid`。`apperr.Unimpl` 映射到 HTTP **501**（`internal/apperr/apperr.go:26,49`，注释写着"501, refused rather than silently degraded"）。这个调用发生在**治理准入阶段、落盘之前**（`internal/service/service.go:115-117`），与磁盘配额拒绝同一位置（`service.go:112-114`），也就是 §1.5 里"能在准入处拒的，绝不留到状态机里拒"。

**为什么 `allowlist` 在容器后端只能返回 501，而不能"尽力而为地配一下"？** 因为容器后端无法做**内核级**强制。所谓内核级强制，指外发流量必须经过一个**沙箱改不到**的决策点（宿主侧 nftables、或专用出口网关），白名单在那里生效。容器后端能实现的只有"在沙箱自己的网络栈里加规则"，而那个位置沙箱本身能触及，因此是可被绕过的白名单。本项目的立场写在 `docs/DESIGN.md:99-104`：绝不产出"看起来受限实际没有"的沙箱——宁可明确告诉调用方"本节点做不到"，也不制造一个调用方以为受限、实际敞开的执行环境。这不是能力缺失，是产品设计（`docs/POSITIONING.md:58-60` 把它列为"买之前必须接受"的一条）。

**替代方案**

替代一，**在容器里用 iptables/nftables 做出口白名单**，很多平台这么选，理由是"总比没有强"。不选，理由如上：规则运行在沙箱能触及的地方，无法保证内核级强制；与其造一个可被绕过的白名单，不如诚实拒绝——诚实拒绝的代价是"没有这个功能"，而绕过它的收益是"调用方以为自己安全"。替代二，**自建一个强制出口代理网关**，让沙箱只能连网关、由网关做域名白名单。这是真实路线，但它引入一个平台必须运维、承载全部租户出口流量的组件，还要处理 TLS 的 SNI/Host 检查才能按域名而非按 IP 判断。本项目把它放在路线图里：中期规划用 eBPF 按沙箱做网络白名单（按域名/端口/协议过滤、动态更新），届时容器后端的 egress allowlist 从"诚实拒绝"升级为内核级强制，而 macOS 开发环境**保持拒绝**（`docs/DESIGN.md:317-321`）。

**怎么验证**

```bash
docker inspect <container> --format '{{json .HostConfig.NetworkMode}} {{json .NetworkSettings.Networks}}'
# 预期： "none" {}
SBX_REQUIRE_DOCKER=1 go test ./tests/ -run 'TestIsolationContract/unenforceable_limits_are_refused_not_silently_ignored' -v -count=1
```

第一条预期是 `"none" {}`——模式为 `none`、且 `Networks` 为空对象（没接入任何网络）。对比一个默认容器，它的 `NetworkMode` 是 `bridge`、`Networks` 里有 `bridge` 条目，这就是"引擎默认有出口"的直观证据。第二条断言请求 `allowlist` 出口策略会得到 `apperr.Unimplemented`（即 501），并**同时**验证 501 是那个 code 的传输状态（`tests/isolation_test.go:229-244`）；注释写明了它防的失效模式："调用方以为受限、而实际不受限的沙箱，是最坏的一种。"

## 7.6 离线 rootfs 导入：不依赖 registry 造一个镜像

**问题**
企业内网和数据主权场景里，目标节点经常**连不上任何 registry**——这就是气隙（air-gapped）部署。而"起一个沙箱"至少需要一个基础镜像。如果唯一的造镜像方式是"让引擎去 pull 一个"，那么在最需要这个产品的环境里，它连一个沙箱都建不起来。所以"从本地目录构造镜像"必须是**一等能力**，而不是变通手段。

**原理**
Docker 的导入接口是 `POST /images/create`，带 `fromSrc=-` 与 `repo=<tag>`，请求体是一个 tar。这个 tar 不是"镜像 tar"（那是有 manifest、有层的复合结构），而是**扁平的 rootfs 归档**：里面就是文件系统内容，路径直接是 `usr/bin/...` 这样的相对路径。引擎把它当**一个单层**，生成新镜像并打上 `repo`。所以离线导入的产物是"只有一层"的镜像——它没有可共享的底层，因为它是从零造的基础镜像。

构造这个 tar 有一个与层无关、纯粹属于**用户态布局**的坑。Ubuntu 24.04、Arch 采用 **merged-/usr** 布局：`/bin`、`/lib`、`/lib64`、`/sbin` 不是真实目录，而是指向 `usr/bin`、`usr/lib` 等的**符号链接**，真正的二进制都在 `usr/` 下。一个朴素打包器若看到源目录里 `/bin` 存在（哪怕它是个符号链接）就照着创建真实目录，二进制会被放进 `usr/bin`、而 `/bin` 是个空目录，镜像里连 `/bin/sh` 都找不到，无法启动。

**本项目怎么做**

`internal/runtime/oci/rootfs.go` 的包注释把两个踩过的坑写成了设计前提（`rootfs.go:3-13`）：

```go
// Offline rootfs import: registries are often unreachable in enterprise and
// air-gapped networks, so building a base image from a local rootfs directory
// is a first-class product capability, not a workaround.
//
// ... Naively creating real directories there puts binaries in /usr/bin
// while /bin stays empty and the image cannot boot; blindly "de-duplicating"
// symlinks with os.SameFile also collapses /usr/bin/sh into /usr/bin/dash and
// deletes the one link that makes the image usable. Both failure modes shipped
// in an earlier iteration; the normalization below is the fix.
```

两个失败模式都值得记：一是**把符号链接当目录重建**（`/bin` 空）；二是**用 `os.SameFile` 盲目去重符号链接**——因为 `/usr/bin/sh` 与 `/usr/bin/dash` 指向同一目标，去重会删掉那个让镜像可用的链接。后者的教训是：符号链接的"相同"在文件系统层面是"指向同一目标"，在**语义层面**它们是不同的名字，不能合并。

构造因此分两步。第一步是顶层重写（`rootfs.go:51-58`）：

```go
rewrite := map[string]string{
	"bin":   "usr/bin",
	"lib":   "usr/lib",
	"lib64": "usr/lib64",
	"sbin":  "usr/sbin",
}
```

`filepath.Walk` 遍历时，落在 `bin`/`lib`/`lib64`/`sbin` 顶层下的实体文件，路径前缀改写成 `usr/*`；顶层目录本身被**跳过**（不写成真实目录）。第二步是补符号链接（`rootfs.go:126-143`）：若某个顶层名字在遍历中未出现（说明源目录里那本就只是符号链接、被第一步跳过了），就显式写一条 `bin → usr/bin` 这样的符号链接头进 tar。于是无论源目录是哪种布局，产物都是同一套正确的 merged-/usr 结构。

导入本身是两个函数。`ImportRootfs`（`rootfs.go:150-167`）从本地目录构造并导入：

```go
// internal/runtime/oci/rootfs.go:151-166
func (b *Backend) ImportRootfs(ctx context.Context, rootfsDir, tag string) (string, int64, error) {
	tarball, err := BuildRootfsTar(rootfsDir)
	if err != nil {
		return "", 0, err
	}
	body, err := b.c.do(ctx, "POST",
		"/images/create", map[string]string{"fromSrc": "-", "repo": tag}, bytes.NewReader(tarball))
	if err != nil {
		return "", 0, err
	}
	_ = body
	size, err := b.imageSize(ctx, tag)
	if err != nil {
		return tag, 0, err
	}
	return tag, size, nil
}
```

`fromSrc: "-"` 表示"镜像源是请求体本身，不是 URL 或 registry 引用"；`repo: tag` 是产物要打的 tag。`ImportRootfsTar`（`rootfs.go:169-194`）是它的兄弟，接收一个**预先构造好的** tar 字节（可选 gzip），服务"运维把 rootfs 产物随二进制一起分发"这个场景。服务层把它接成模板（`internal/service/snapshot.go:189-211`）：`ImportRootfsTemplate` 用 `sandboxd-tpl-<name>` 作 tag 调 `ImportRootfs`，再注册模板对象。HTTP 端点在 `/v1/templates/import`（`internal/httpapi/server.go:104`），处理器在 `internal/httpapi/admin.go:187-212`，需要 admin scope。运维流程写在 `docs/DEPLOYMENT.md:66-78`：联网机器准备"二进制 + rootfs tarball"，目标节点无需 registry，用 admin Key 调导入端点，要求是"完整的用户态（含 `/usr/bin`、`/bin/sh` 及动态链接器）"。

**为什么企业内网需要这个能力**，归结成一句：**它把"造一个可运行的基础镜像"从"依赖外网"解耦成"依赖一个本地目录或 tar 文件"。** 这是"自托管、数据不出域"定位（§1.1）能否成立的前置条件——如果拉镜像必须出网，那"数据不出域"就只在运行期成立、在部署期不成立。

**替代方案**

替代一，在联网机器 `docker pull` 再 `docker save` 导出、目标节点 `docker load`。它也能离线，但要求那台机器能访问 registry、且镜像是**别的环境**构建好的；而 rootfs 导入产物更小、布局更可控（本项目的归一化保证 merged-/usr 正确），适合"运维手上就有一份发行版 rootfs"的情形。替代二，在目标节点起一个内网 registry 仍走标准 pull，这引入一个要运维、要保活、要配证书的组件，与"单二进制、零外部依赖"冲突（§16 会讨论零依赖的代价与收益）。

**怎么验证**

```bash
SBX_REQUIRE_DOCKER=1 go test ./internal/runtime/oci/ -run TestOfflineRootfsImport -v -count=1
```

预期通过。这条用例（`internal/runtime/oci/oci_test.go:295-429`）特意构造一个 **merged-/usr 布局**的 rootfs（`/bin`、`/lib` 都是符号链接，`usr/bin` 里放 busybox 与它的动态链接器），导入后从该镜像起沙箱、执行 `/bin/echo offline-ok` 并断言输出为 `offline-ok`。用例注释点出失败的样子：busybox 动态链接着 musl，"没有加载器时镜像构建成功，但每次 exec 都以 127 失败"（`oci_test.go:319-321`）——这正是 §7.4 里 `Start` 把退出码 127 翻译成"镜像缺解释器或链接器"的来源。

## 7.7 本项目的容器组织方式：命名与标签

**问题**
一台宿主上可能同时有几十个沙箱、若干预热容器、一些模板合成的临时容器混在一起。崩溃恢复（§10）要问的第一件事是"**哪些对象是我们的**"，第二件是"**它属于哪个沙箱/租户**"。没有可被机器查询的归属信息，重启后的控制面要么漏掉真实存在的沙箱，要么误删不属于自己的容器。归属必须**写在对象本身里**，而不是记在控制面内存里——因为进程会死，对象不会。

**原理**
有两条表达归属的通道：**名称**与**标签**，性质不同、用途不同。名称是一个唯一字符串，但它**可以被重命名**——本项目的预热池正是靠重命名把一个预热容器交接给租户（`Rename`，`internal/runtime/runtime.go:201-203`）。标签是一组键值对，容器创建后**不能被修改**（引擎不提供改标签的接口）。所以：名称适合"会变化的事务性身份"，标签适合"不变的归属事实"。一个可靠的组织方式需要**两条都用，各司其职**。标签还有一个现实优势：可被引擎**过滤查询**，让"枚举我们所有的对象"变成一次引擎端查询，而不是把宿主所有容器拉回来在控制面里过滤。

**本项目怎么做**

命名。用户沙箱叫 `sandboxd-<沙箱ID>`（`internal/service/service.go:298`）：

```go
name := "sandboxd-" + sbID
```

预热池对象叫 `sandboxd-warm-<随机片段>`（`internal/service/pool.go:112-118`），模板合成的临时容器叫 `sandcastle-compose-<随机片段>`（`internal/service/compose.go:117-124`）。`sandboxd-warm-` 这个前缀本身就被代码依赖：收敛器用**名称前缀**判断"这是池对象"，`isWarmName` 的实现是 `ref[:14] == "sandboxd-warm-"`（`internal/service/reconciler.go:298-302`），注释点明原因——"因为在容器上标签不能被更新，名称才是池对象的稳定身份"。

标签。后端定义了产品自己的键名常量（`internal/runtime/oci/backend.go:16-24`）：

```go
const (
	labelProduct = "product"
	labelSandbox = "sandbox.id"
	labelTenant  = "sandbox.tenant"
	prodValue    = "sandboxd"
	// ...
)
```

标签值由服务层在创建时写入（`internal/service/service.go:307-310`）：

```go
Labels: map[string]string{
	"sandbox.id":     sbID,
	"sandbox.tenant": cur.TenantID,
},
```

无论来源，每个容器最后都会经 `withProduct`（`backend.go:363-369`）补上 `product=sandboxd`。所以一个普通沙箱容器带三个标签：`product=sandboxd`、`sandbox.id=sbx_...`、`sandbox.tenant=tnt_...`；预热池对象带 `warm=1` 与 `template.id`（`pool.go:117`），合成临时容器带 `compose=1`（`compose.go:123`）。

这里要澄清一处措辞精度：**`sandboxd-` 这个前缀出现在容器的"名称"和镜像的"tag"上，而不是标签键上**。标签的键是 `product`、`sandbox.id`、`sandbox.tenant`；值是 `sandboxd`（product 值）。镜像 tag 才带 `sandboxd-` 前缀，有两种：快照镜像 `sandboxd-snap-<快照ID>`（`internal/service/snapshot.go:62`）、模板镜像 `sandboxd-tpl-<名字>`（`snapshot.go:192`）。后端把它们的前缀钉在一个变量里（`internal/runtime/oci/backend.go:846-850`）：

```go
var managedImagePrefixes = []string{"sandboxd-snap-", "sandboxd-tpl-"}
```

**标签就是后续孤儿回收的依据。** 崩溃后的收敛器枚举后端对象（`Orphans`，`backend.go:811-844`，用 `filters={"label":["product=sandboxd"]}` 只取带产品标签的），对每个读 `sandbox.id` 标签（`reconciler.go:210`）：有标签值、但存储里查不到对应记录，说明这是"删除操作删掉了记录、还没删掉容器"的崩溃残骸，回收；连 `sandbox.id` 都没有的对象（比如自检容器），是"带产品标签但无法归属"，**不碰**，留给运维（`reconciler.go:211-213`）。最后这条规则解释了那个看似别扭的设计：隔离自检起的探针容器**故意不带 `sandbox.id`**（`internal/runtime/oci/isolation.go:87-90`），否则收敛器会把它当崩溃残骸回收，自检就会"被从自己脚下抽走"。这条性质有专门的测试守着：`internal/runtime/oci/isolation_test.go:213-231` 的 `TestProbeContainerIsNotRegisteredAsASandbox` 断言探针容器没有 `sandbox.id`、但**仍有** `product=sandboxd`——既要能被识别为"我们的"，又要能被识别为"不是注册过的沙箱"。本节任务到此为止：**标签本身是什么、为什么这么打。** 收敛器根据标签做什么决策归 §10。

**替代方案**

替代一，**只用名称前缀判断归属**。不选：名称会被 `Rename` 改（池交接、用户重命名），一旦改名，"通过前缀认出这是谁的"就不成立；而标签不可改，是更可靠的归属。这也解释了为什么池对象**反过来**用名称当稳定身份——因为它故意要能被改名交接，需要"名字"这个会变的东西做事务标识，而不是"不变"的标签。替代二，**只把归属记在控制面存储里**、容器上不打标签。不选：控制面崩溃后，存储里可能有"创建意图但没写回 ref"的记录（§9 的意图先行），此时磁盘上的容器是"光秃秃"的，没办法把它和某条记录或某个租户联系起来——既不能被安全回收，也不能被安全采纳。归属必须冗余地写进对象本身，这就是"标签是孤儿回收依据"的全部意义。

**怎么验证**

```bash
docker ps -a --filter label=product=sandboxd \
  --format '{{.Names}}\t{{.Label "sandbox.id"}}\t{{.Label "sandbox.tenant"}}'
docker image ls --filter reference='sandboxd-*'
```

第一条预期：每行是一个 `sandboxd-<ID>`、`sandboxd-warm-<片段>` 或 `sandcastle-compose-<片段>` 的容器，后面跟着它的 `sandbox.id` 与 `sandbox.tenant` 标签值；预热与合成容器的后两列会为空（它们不打这两个标签），这本身就是"不同来源带不同标签集"的证据。第二条预期：只有 `sandboxd-snap-*` 与 `sandboxd-tpl-*` 两类——它们正是 `managedImagePrefixes` 声明的、由本项目 `commit`/导入产出、归本项目回收的那批镜像。

## 7.8 把这一章读回来：请求侧与内核侧的两侧对照

**问题**
前面每节都给了自己的验证命令，但零散各处。更麻烦的是，这些命令读到的多是**请求侧**——你要求引擎做什么。而本项目的核心方法论是"配置不是证据"（§6），所以必须把"请求侧"和"内核侧"放在一起看，才知道该信哪边、以及为什么。

**原理**
`docker inspect` 返回一份分段 JSON，每段对应一类真相：**`Config`** 是被声明的部分（环境变量、入口命令、归属标签 `Config.Labels`）；**`HostConfig`** 是**请求侧**的安全契约（就是 §7.4 钉死的那些字段）；**`State`** 是运行时状态（`Running`、`Paused`、`ExitCode`、`OOMKilled`、`Status`），本项目的 `inspectBody` 只解析这一段（`internal/runtime/oci/backend.go:299-333`），因为状态机要知道的只是"它现在到底在不在跑"；**`GraphDriver.Data`** 是文件系统层（`LowerDir`/`UpperDir`/`MergedDir`/`WorkDir`，即 §7.3 的四个目录）。

关键在于：`HostConfig` 是**请求侧**证据，`State` 与 `GraphDriver` 是引擎对请求的执行结果，而**内核事实**（uid 映射、seccomp 模式、能力集、core ulimit、路由表）都不在这份 JSON 里——只能从**沙箱内部**读。§6 那个"`UsernsMode` 被原样回读为 `private`、而 `/proc/self/uid_map` 什么都没发生"的实测，说明的正是这个差距：`HostConfig` 会回读你请求了什么，`/proc` 不会。

**本项目怎么做。**

服务层只读 `inspectBody` 里它需要的那一段（`internal/runtime/oci/backend.go:299-333`），所以核对也必须按同一分段来做。按"请求侧 → 引擎侧 → 内核侧"排列，逐层核对。

```bash
C=sandboxd-<沙箱ID>

# 请求侧：我要了什么（§7.4 / §7.5 / §7.7）
docker inspect "$C" --format '{{json .HostConfig}}' | python3 -m json.tool
docker inspect "$C" --format '{{json .Config.Labels}}' | python3 -m json.tool

# 引擎侧：引擎做了什么
docker inspect "$C" --format '{{.State.Status}} {{.State.Running}} {{.State.Paused}} {{.State.OOMKilled}} {{.State.ExitCode}}'
docker inspect "$C" --format '{{json .GraphDriver.Data}}' | python3 -m json.tool

# 内核侧：内核给了什么（不在 inspect 里，必须进沙箱）
docker exec "$C" /bin/sh -c 'readlink /proc/self/ns/mnt; cat /proc/self/ns/ipc; cat /sys/fs/cgroup/pids.max; ip route'
```

**怎么验证。**

逐段预期：`HostConfig` 里 `NetworkMode` 应为 `none`、`IpcMode` 与 `CgroupnsMode` 应为 `private`、`CapDrop` 为 `["ALL"]`、`CapAdd` 为 `[]`、`Privileged` 与 `ReadonlyRootfs` 为 `false`、`UsernsMode` 绝不为空。`Config.Labels` 应有 `product=sandboxd` 与 `sandbox.id`、`sandbox.tenant`，且 `sandbox.id` 的值与容器名 `sandboxd-` 后面的部分一致。`State` 那行健康沙箱打印 `running true false false 0`，这五个字段正是 `inspectBody.toStatus()`（`backend.go:321-333`）在命令行上的等价物，也是状态机与收敛器（§10）判断真值所读的字段。`GraphDriver.Data` 会返回四个键，`LowerDir` 里有几个冒号分隔的目录，镜像就有几层只读层，`UpperDir` 是这一个容器独有的可写层——`Commit()` 捕获的就是它（§7.3、§8）。内核侧：`mnt`/`ipc` 编号与宿主不同（§5.3）、`pids.max` 为 `512`（§5.8）、`ip route` 无输出（§5.6）。

这三层连起来就是本章的用法：`HostConfig` 说"我们要了什么"，`State`/`GraphDriver` 说"引擎据此做了什么"，`/proc` 说"内核最终给了什么"。三者一致，契约成立；不一致时——最典型的就是 §6 那个 userns 静默失效——**只有第三层算证据**。

**替代方案**

只信 `/v1/isolation`（内核事实、权威）、不看 `inspect`。不选：`inspect` 展示**请求侧**，恰好能回答"我的代码有没有把该钉的字段钉上"——这是内核事实看不到的。内核事实回答"隔离生效了没有"，`inspect` 回答"我的契约完整不完整"。一个字段在 `inspect` 里就是空的，说明它根本没被请求，那是 §7.4 里的 bug，不是隔离的问题。两侧互补，缺一不可。

---

### 本章检验

1. `internal/runtime/oci/backend.go` 的 `hostConfig` 注释点名了六个"真有引擎默认值能削弱沙箱"的字段，但项目最终钉死了远不止六个。请解释这个"多钉"的取舍：多钉的代价是什么、收益是什么？并指出不做这个"多钉"时，哪一种失败是端到端测试**抓不到**的。

2. `IpcMode` 与 `CgroupnsMode` 都被钉为 `private`，但两个坑的形状不同：前者默认值在**所有宿主**上都是危险的（`shareable`），后者的危险只出现在**特定宿主**上（cgroup v1 上是 `host`）。请分别说明这两种失效各由谁触发，并解释为什么后者更难通过"在开发机上跑一遍"发现。

3. 有人认为 `egress.mode=allowlist` 返回 501 是"功能没做完"，应当先"在容器里用 iptables 尽力做一下白名单"。请按 §7.5 的原理反驳：说清"内核级强制"与"沙箱内规则"的差别，以及为什么"看起来受限实际没有"的沙箱比"明确不支持"更危险。

4. `Commit()` 只做一件事：把容器的可写层固化成一个新层。请据此解释两个看似无关的决定——(a) 为什么根文件系统被刻意保持可写（`ReadonlyRootfs=false`）而不是像通常那样设为只读；(b) 为什么从快照/模板建出的沙箱所用镜像，在孤儿镜像回收时必须被保护（提示：沙箱会持有它所用镜像的引用）。两问都只从"层"这个物理事实出发，不涉及快照的产品语义。

5. **动手任务**：在一个真实沙箱上跑出 §7.8 的三层证据，整理成一张对照表。
   - 请求侧：`docker inspect "$C" --format '{{json .HostConfig}}'`，抄下 `NetworkMode`、`IpcMode`、`CgroupnsMode`、`UsernsMode`、`CapDrop`、`CapAdd`。
   - 引擎侧：`docker inspect "$C" --format '{{json .GraphDriver.Data}}'`，数出 `LowerDir` 里有几个目录、记下 `UpperDir` 路径。
   - 内核侧：`docker exec "$C" /bin/sh -c 'readlink /proc/self/ns/ipc; ls /sys/class/net; ip route'`。
   然后回答：如果 `UsernsMode` 读出来是 `private`，而内核侧没有一条能证明 userns 生效的证据，你会信哪一层？为什么？（提示：从 `/proc/self/uid_map` 入手，方法见 §6。）


# §8 沙箱生命周期：暂停、恢复、快照、模板与预热池

## 8.1 创建语义：为什么"等"和"不等"两种都要有

**问题**：创建请求发出后，调用方拿到什么才算成功？立即返回一个 `provisioning` 记录，调用方下一步 `exec` 就撞上"必须 running"的 409；一直等到 `running` 才返回，一个秒级创建就把连接占住，突发时几十个请求全挂在半途。所以真正的问题不是选哪个，而是**这个"等"等的是什么，能不能让调用方自己选**。

**原理**：等待有两种姿态。**等内存事件**——供给 goroutine 干完往 channel 推一条消息，等待方 `select` 它，低延迟，但事件只存在于内存，进程重启就永远收不到。**等持久状态**——周期性地读"这件事被记录成什么样了"，多花几次读，但读的是断电后依然存在、且会被收敛器修正的那份事实，因此对重启免疫。本项目选后者。

**本项目怎么做**：由 handler 决定等不等，默认等（`internal/httpapi/handlers.go:165`，注释 `// agents want ready sandboxes`）。service 层的分叉在意图已落盘、供给工人已派出之后，`internal/service/service.go:153-167`：

```go
	if req.Wait {
		waitTimeout := req.WaitTimeout
		if waitTimeout <= 0 {
			waitTimeout = 120 * time.Second
		}
		if err := s.waitForState(ctx, sb.ID, waitTimeout, domain.StateRunning, domain.StateFailed); err != nil {
			return nil, err
		}
		final, _ := s.St.GetSandbox(sb.ID)
		if final.State == domain.StateFailed {
			return final, &SandboxFailedError{reason: final.Reason}
		}
		return final, nil
	}
	return sb, nil
```

三点含义。第一，**等的是两个状态而不是一个**：只等 `running` 的话，一次必然失败的创建（基础镜像不存在）会让调用方等到超时，再收到含混的"创建中"；失败是确定结局，必须和成功一样结束等待。第二，**失败有两种上报形态但落到同一个字段**：同步时直接返回 `SandboxFailedError`（`internal/service/errors.go:36-42`），handler 映射成 409 并附上沙箱记录（`internal/httpapi/handlers.go:176-183`）；异步时调用方自己轮询，最终看到 `state=failed` 与 `Reason`。第三，**失败记录绕过状态机**：`failSandbox` 直接写 `x.State = domain.StateFailed`（`internal/service/service.go:336-350`），因为它是"内部真相修复"，可能从不属于合法边的前置态收敛而来，注释明说了这个区分。原因还会被清洗并截断到 500 字节。

`waitForState` 每 150 毫秒读一次 store（`internal/service/service.go:380-384`）。轮询而非订阅的理由见 §3；本章关心的是另一个后果：它读的正是**收敛器也会写的那个字段**，所以守护进程在创建中途崩溃、重启后被收敛器（见 §10）改成真实状态，新的等待者读到的仍是同一份事实，等待不会因一次崩溃而断掉。

**为什么两种都要有**：默认等，因为调用方是 Agent，它要的是能立刻 `exec` 的沙箱而不是一个 ID。`wait=false` 服务另两类用法：批量供给（不想在一个连接上串行等），以及**观测供给窗口本身**——端到端套件靠它在供给仍在途中时交回记录（`tests/resilience_test.go:24-28`）。

**替代方案**：永远同步等，则慢镜像会让整个请求层积压；永远立即返回，则把轮询复杂度推给每个调用方。做成默认开启、可关的参数，是同时满足两类的唯一形状。

**怎么验证**：`go test -run TestCrashRecovery -v ./tests/`。它用 `wait=false` 拿到 `provisioning` 记录，途中 `kill -9` 再启动，断言状态**不会停留在 `provisioning`**（`tests/resilience_test.go:38`）。预期通过。

## 8.2 `pause` 的实现：cgroup freezer 与"冻结"在内核里意味着什么

**问题**：一个沙箱跑了半天，内存里攒了一堆状态（载入的模型、缓存、打开的文件），你暂时不用它。删掉重建或停止都会丢掉这些内存状态，所以需要第三种能力：把正在运行的进程组"按暂停键"，且内存不还、状态不丢。

**原理**：Docker 的 `pause` 底层是 **cgroup freezer**。cgroup v2 上是 `cgroup.freeze`（写 `1` 冻结），v1 上是 `freezer.state`（写 `FROZEN`）。冻结时内核遍历该 cgroup 的任务把它们标记为冻结，被冻结的任务**不再被调度器选中送上 CPU**——不参与可运行队列竞争，因此这段时间**不消耗 CPU**；但地址空间、打开的文件、进程结构全部保留。解冻后从被停下的那条指令继续。

要区分三件事：它**不是停止**（停止是发信号让进程退出，退出后内存释放、不可恢复，而冻结的进程活着、只是不被调度）；它**不是换出**（换出不搬动内存，冻结也不搬，页仍驻留内存）；它是**整个进程组**级别的（沙箱里 PID 1 与 `exec` 出的子进程同属一个 cgroup，一起被冻）。由此得到一条必须记住的结论：**`pause` 省的是 CPU，不是内存**——它直接推出下一节的设计。

**本项目怎么做**：`internal/runtime/oci/backend.go:420-432`：

```go
func (b *Backend) Pause(ctx context.Context, ref string, opts runtime.PauseOptions) error {
	ins, err := b.inspect(ctx, ref)
	if err != nil {
		return err
	}
	if ins.State.Paused {
		return nil // idempotent by real phase, not by error-code guessing
	}
	if !ins.State.Running {
		return apperr.ConflictErr("cannot pause sandbox in state %q", ins.State.Status)
	}
	if _, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/pause", nil, nil); err != nil {
		return err
	}
```

注意顺序：**先 `inspect` 再决定**，而不是先发请求再猜错误码。行内注释写明了理由——幂等要"按真实相位判断，而不是靠猜错误码"；否则一旦引擎改了冲突措辞，这段判断就失效。暂停之后的收尾是 8.3 的主题。

service 层走通用转换器（`internal/service/lifecycle.go:105-122`）：记录 `pausing` → 调后端 → `paused`。中途失败的处置在 `transition`（`internal/service/lifecycle.go:69-103`）：它**先 `Inspect` 确认沙箱是否仍健康**，只有后端明确说还在 running 时才退回 `running`（`internal/service/lifecycle.go:87-88`），否则保持过渡态交给收敛器（见 §10）。这处理了"其实暂停成功、但响应丢了"——此时退回 `running` 是错的，因为后端已经是 paused，必须以观察为准。

**为什么 pause 不等于"停止容器"**：暂停后 `inspect` 返回的 `Paused` 为真、`Running` 也为真（`internal/runtime/oci/backend.go:309-318` 把两者分成独立字段）；收敛器据此映射成 `paused` 而非 `stopped`（`internal/service/reconciler.go:287-296`）。若 `pause` 被实现成"停止 + 记一个 paused 状态"，重启后收敛器读取后端看到的是已退出，会把沙箱判成 `stopped`——一个被暂停的实例就此变成死的。

**替代方案**：用快照代替暂停，对容器档不可行——OCI 后端没有内存快照能力（`internal/domain/artifact.go:10-12` 明确标注内存级快照属 microVM 档）；停止后从快照恢复文件系统则会丢掉全部未落盘的内存状态。freezer 是容器档下唯一能原地保留内存的手段。

**怎么验证**：`go test -run TestPauseResume ./internal/runtime/oci/`。它暂停后 `inspect` 确认真实相位是 `Paused`（`internal/runtime/oci/oci_test.go:210-213`），重复暂停验证幂等（第 215 行），恢复后跑一次 `exec` 确认进程活着（第 236-239 行）。断言的是回读结果，而非"请求返回 200"。

## 8.3 `pause_reclaim`：在冻结期把内存要回来

**问题**：8.2 的结论是"暂停不省内存"。可暂停的全部意义就是"我现在不用它"——若它仍占着配置的内存上限，一个租户暂停十个沙箱就是十份内存白占。需要一种办法，在冻结期间真把物理内存还回去，且恢复时沙箱照常能跑。

**原理**：内核回收由**压力**触发。cgroup 用量逼近 `memory.max` 时，内核开始回收：文件页（page cache）可直接丢弃，匿名页（堆栈）无处可丢，必须换出到 swap，否则 OOM。所以"**降低上限**"本身就是回收信号：上限压到当前用量之下，内核立刻丢文件页、换出匿名页，直到用量回落到新上限下。

这件事只能**在冻结期做**，因为回收有代价：换出要写盘、要扫页表，被回收的进程下次访问这些页会遭遇缺页、被拖慢，即**延迟与抖动**。对正在运行的进程这是性能损失；对一个已经冻结、短时间内**没有任何进程在跑**的沙箱，代价为零，因为它根本感知不到。冻结把昂贵动作变成了免费窗口。

**本项目怎么做**：上限常量在 `internal/runtime/oci/backend.go:415-418`（`reclaimTempLimit = 64 << 20`，注释说明冻结的工作负载无法分配，内核可安全驱逐文件页并换出匿名页）。回收动作在 `Pause` 后半段，`internal/runtime/oci/backend.go:438-450`：

```go
	if opts.MemoryBytes > 0 {
		tmp := opts.MemoryBytes
		if tmp > reclaimTempLimit {
			tmp = reclaimTempLimit
		}
		payload, _ := json.Marshal(resources{
			Memory:     tmp,
			MemorySwap: tmp, // swap allowed up to the same ceiling for anon pages
		})
		if _, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/update", nil, bytes.NewReader(payload)); err != nil {
			_ = err // reclaim is opportunistic; the original limit is restored on Resume
		}
	}
```

四点。其一，`opts.MemoryBytes` 是沙箱配置的上限，为 0 时只冻结不回收。其二，临时上限取 `min(配置上限, 64 MiB)`：配置本来只有 32 MiB 就不必抬高，配了 8 GiB 则必须压到足够低才逼得出回收，`64 << 20` 是"够低"与"不把在用的匿名页全换出"之间的折中。其三，**`MemorySwap` 也设为同一个值**——沙箱创建时 swap 是关闭的（`internal/runtime/oci/backend.go:275`），关闭后匿名页无处可去，降上限只会 OOM 而不是回收；这里临时放开 swap，给内核一个换出的去处。其四，**回收失败不致命**（`_ = err`）：行内注释说冻结本身才是遏制手段，深回收是机会主义的，这次没成就等下次宿主内存压力。不因一个优化失败而回滚一次已成功的暂停，是正确的错误姿态。

service 层填 `MemoryBytes` 并受开关控制（`internal/service/lifecycle.go:115-119`）；配置项在 `sandboxd.conf.example:29-30` 与 `internal/config/config.go:80-83`。

**恢复前的还原是本节最凶险的一段**，`internal/runtime/oci/backend.go:462-466`：

```go
	// The original limit MUST be restored before unfreezing, or the first
	// allocations after resume hit the temporary ceiling and OOM. Restore
	// failures are retried; if they persist, the sandbox is left frozen
	// and the error surfaces — an unpaused OOM loop is worse.
	if opts.MemoryBytes > 0 {
```

时序错了会怎样？若先 `unpause` 再还原，解冻瞬间进程恢复分配，却撞上仍是 64 MiB 的上限，**立刻被 OOM 杀掉**，而且每次恢复都重演。所以必须"**先还原上限、再解冻**"。还原本身可能失败，代码**重试 5 次、每次间隔 200 毫秒**（`internal/runtime/oci/backend.go:472-481`）；仍失败则**不强行解冻**，返回错误并让沙箱留在冻结态（第 482-484 行）。注释把取舍写明：一个解冻后立刻 OOM 的循环比一个还冻着的沙箱更糟——前者反复丢数据，后者只是暂时不可用且可重试。

**替代方案**：把进程内存 dump 到磁盘、恢复时读回（真内存快照），对 OCI 后端不可行；暂停时直接 stop、恢复时从快照重建，会丢内存状态；"什么都不做、靠宿主全局压力自然回收"在多租户下不可控——回收了谁、何时回收你说了不算。把回收收敛到"暂停"这个确定窗口，是把不确定性换成确定性。

**怎么验证**：`go test -run TestPauseResume ./internal/runtime/oci/`。它在暂停（`MemoryBytes: 128 << 20`）后恢复，直接检查后端读回的**真实上限**已还原（`internal/runtime/oci/oci_test.go:226-232`）。预期通过。只验证"恢复后能 exec"不够——一个先解冻再还原的实现也可能恰好跑过一次 `echo`。

## 8.4 透明恢复：`auto_resume` 与它的反向姿态

**问题**：`paused` 成了一等状态后，调用方就得知道它存在——`exec` 前先查是否被暂停、是则先 `POST /resume`。这等于把"冻结"这个实现细节泄漏给每一个调用方，而调用方只关心"在这台机器上跑一条命令"。

**原理**：把暂停当成**实现细节**而非**契约**。平台收到请求时若发现沙箱是 `paused` 且策略允许，就**先恢复再执行**，调用方全程不知道发生过一次恢复。这就是透明恢复。

**本项目怎么做**：`internal/service/lifecycle.go:33-49`：

```go
// requireRunning enforces the running precondition, with DSec §6.3
// transparent-resume semantics: a request to a paused sandbox resumes it
// first (config auto_resume, default on) instead of bouncing with 409.
// A frozen workload is an implementation detail of pause, not something
// callers should have to manage.
func (s *Service) requireRunning(ctx context.Context, tenantID, keyID string, sb *domain.Sandbox) (*domain.Sandbox, error) {
	if sb.State == domain.StateRunning {
		return sb, nil
	}
	if sb.State == domain.StatePaused && s.Cfg.AutoResume {
		if _, err := s.ResumeSandbox(ctx, tenantID, keyID, sb.ID); err != nil {
			return nil, err
		}
		return s.GetSandbox(tenantID, sb.ID)
	}
	return nil, apperr.ConflictErr("sandbox must be running, current state is %s", sb.State)
}
```

三个分支就是三种姿态：已 `running` 直接放行；`paused` 且开关开，则恢复、**重新读一次记录**（恢复会改写版本与状态）、把新记录交回；其余（`stopped`、`failed`、过渡态）返回冲突并带上当前状态。**每处 `exec` 与文件操作都先过它**（`internal/service/lifecycle.go:225`、`259`、`281`、`300`、`315`），所以"透明"是系统性的。

这条语义吸收自 DSec §6.3（`docs/DESIGN.md:314` 在 §7.5 的"已落地"清单里列了"透明恢复（auto_resume）"；出处还写在行内注释与 `tests/governance_test.go:124`），与本项目从 DSec 借鉴的预热池、Backend 分级一样，搬的是成熟语义而非代码。

**反向姿态 `auto_resume=false`**：它把 `paused` 变成**有约束力的承诺**——"我暂停了它，就是要它保持冻结"。此时对暂停沙箱的 `exec` 返回 409 且**不得偷偷恢复**（`internal/service/lifecycle.go:48`）。为什么也要支持？一是成本控制：暂停是"确定一段时间不碰它"的显式动作，若一个后台探测或健康检查意外碰一下就把实例唤醒，暂停形同虚设；二是可预测性：某些编排需要"暂停后的状态在下一次显式 `resume` 前不变"。配置见 `internal/config/config.go:77-79`，默认 true。

**替代方案**：干脆没有暂停状态（`pause` 只作运维提示），则成本控制没有合同意义；只提供透明恢复、不给开关，则把上面两类场景挤出去。默认开启、关闭时**拒绝而非静默恢复**，是这个权衡的落点。

**怎么验证**：`go test -run TestLifecycle ./tests/`。`pause_freezes_and_an_exec_transparently_resumes`（`tests/governance_test.go:118-135`）暂停后直接 `exec`，断言命令成功**且之后沙箱是 `running`**——断言的是"之后的状态"，因为真正的透明恢复必然留下状态变更。`with_auto_resume_off_a_paused_sandbox_refuses_work`（第 137-149 行）用 `auto_resume=false`，断言 `exec` 得 409 且状态**仍是 `paused`**。一正一反缺一不可。

## 8.5 快照的产品语义：从可写层到一个可重建的环境

**问题**：一个 Agent 花十分钟装好依赖，随后沙箱到期被回收，下次任务从零重来。需要把"装好的环境"固化下来，之后能快速重建出**同一份**环境，且重建的实例彼此隔离。

**原理**：容器状态分文件系统与内存两半，本项目只做前一半：调用后端 `Commit()` 把容器**可写层**固化成镜像，再用它 `Create` 出新容器（物理机制见 §7）。沙箱里写过的东西都在可写层，因此都能被捕获；进程内存不在，所以它**不是**内存快照。

**本项目怎么做**：提交并校验配额的骨架在 `internal/service/snapshot.go:61-70`：

```go
	// image repository names must be lowercase; sandbox IDs are uppercase
	tag := strings.ToLower("sandboxd-snap-" + sn.ID)
	imageRef, size, err := s.Backend.Commit(ctx, sb.Backend.Ref, tag)
	if err != nil {
		return nil, err
	}
	if err := s.Quotas.CheckSnapshot(tenant, size); err != nil {
		_ = s.Backend.RemoveImage(ctx, imageRef)
		return nil, err
	}
```

第一，**tag 是全小写固定前缀 + 快照 ID**（仓库名不允许大写，而 ID 是大写的）。副作用在 `internal/service/snapshot.go:116-123` 记着：这类镜像**带标签**，不是悬空镜像，`docker image prune` 永远删不掉，能删它们的只有删除快照这条路径与收敛器的镜像遍历（见 §10）。快照不是免费的，它是一份完整文件系统副本。

第二，**配额按 `Commit` 后测得的真实字节校验，拒绝时把镜像删掉**（`internal/service/snapshot.go:67-70`）。只有提交后才知道差分层多大；拒绝时删镜像，是因为"告诉调用方失败、却在盘上留一个它以为不存在的产物"是最坏的不一致。配额算法见 §11。

第三，**"从快照创建"和"从模板创建"走同一条恢复路径**：`internal/service/service.go:126-133` 的 `switch` 显示，两者最终都只是把 `imageRef` 交出去，真正 `Create` 时用的是同一个 `provision` 流程（`internal/service/service.go:299-311`），差别只在记录里存 `TemplateID` 还是 `SnapshotID`：

```go
	switch {
	case req.TemplateID != "":
		sb.TemplateID = req.TemplateID
	case req.SnapshotID != "":
		sb.SnapshotID = req.SnapshotID
	default:
		sb.BaseImage = imageRef
	}
```

`resolveImage`（`internal/service/service.go:186-221`）把模板或快照各解析成一个 `imageRef` 加一份默认规格，之后**再没有第二条分支**。为什么强调这点？若"从快照创建"自实现一套启动逻辑，"轮询确认真实相位""启动失败清理容器""失败写 `failed` + 原因"就会有第二份实现，第二份终将与第一份跑偏。三条创建来源收敛到一条恢复路径，等于只维护一套正确性。

第四，**快照与只读根的冲突**。只读根是通常的下一步加固，但它与这套模型**不兼容**：`Commit()` 捕获可写层，根若只读就没有可写层，产出的是**空快照**。所以本项目**刻意**把 `ReadonlyRootfs` 钉死为 `false`（`internal/runtime/oci/backend.go:191-196` 注释明说"是刻意、不是遗漏"，并给出"与可写 workspace overlay 一起重新设计"的前置条件）。`docs/DESIGN.md:195-200` 把它与 `core=0` 并列，称作"两处与快照语义耦合的决定"。教训：加固项不是孤立添加的，可能和核心能力互斥——遇到时**显式记录为什么不做**，而不是悄悄漏掉。

**替代方案**：只存元数据、恢复时重放安装步骤，恢复时间等于原始安装时间且依赖外部仓库（与气隙部署冲突，见 §13）；或者用暂停代替快照，但暂停的沙箱是**活实例**、受 TTL 约束、不能被克隆给多个新沙箱，而快照是可分发的**产物**——两者解决的问题不同。

**怎么验证**：`go test -run TestFilesAndSnapshots ./tests/`。子用例 `a_snapshot_captures_state_that_a_new_sandbox_inherits`（`tests/governance_test.go:287-316`）写入文件与嵌套目录、做快照、用 `snapshot_id` 建一个**新**沙箱，断言它能读到那两个文件。要害是"新实例"——复用原容器也会读到文件，但那是共享实例。

## 8.6 模板分层与同位嫁接

**问题**：一个"基础环境 + 工具箱"的模板，若每次都把工具箱整块拷进基础镜像再打整镜像，则基础镜像一升级，所有组合镜像全部作废、必须重打——两层被焊死成一体。

**原理**：把"基础"与"工具"拆成**独立版本化的层**，组合发生在构建期，但产物不改变两层的独立性。本项目用"容器到容器的目录树嫁接"落地（不改 dockerd）：把工具容器里某目录的整棵树，**按同一路径**拷进基础环境的同一路径。所谓"**同位嫁接**"即此：层里 `/opt/node20` 的东西组合后仍在 `/opt/node20`，路径语义不变。同路径是刻意的，因为一旦允许"层把内容放到任意路径"，组合结果就不可预测。

**本项目怎么做**：层与组合的入口在 `internal/service/compose.go:13-21`，注释写明"层里 `/opt/node20` 就是组合环境里的 `/opt/node20`"。组合过程（`internal/service/compose.go:63-93`）：以 base 镜像起一个"脚手架"容器；对每个层**另起一个 helper 容器**，把 helper 里 `l.Path` 的整棵树 `CopyDir` 到脚手架同一路径，拷完立刻删 helper；全部嫁接完 `Commit` 脚手架得到组合镜像。嫁接那一步，`internal/service/compose.go:84-89`：

```go
		err = s.Backend.CopyDir(ctx, helperRef, l.Path, scratchRef, l.Path)
		_ = s.Backend.Remove(ctx, helperRef)
		if err != nil {
			return nil, err
		}
		applied = append(applied, l.Image+"@"+l.Path)
```

"同位"被 `CopyDir` 做成**硬校验**，`internal/runtime/oci/files.go:205-207`：

```go
	if baseName(srcPath) != baseName(dstPath) {
		return apperr.Invalid("CopyDir requires equal basenames (%q vs %q)", baseName(srcPath), baseName(dstPath))
	}
```

即同位不是靠调用方自觉，而是实现层拒绝：源与目标基础名不同就直接报错。它的实现（GET archive 返回以源基础名为根的树，再 PUT 到目标父目录，因而树原样落到目标路径，见 `internal/runtime/oci/files.go:192-234`）保证基础目录除被嫁接的内容外不被改动。

组合结果保存的是**组合链**而非黑盒：`internal/domain/artifact.go:58-62` 的 `Composition` 字段，由 `internal/service/compose.go:102` 填成"`[基础镜像, 层镜像@路径, ...]`"。**基础镜像与每层镜像保持独立版本化，只有组合结果是新镜像，而组合链被记下供审计与重建。** 这正好回答开头的问题：基础镜像升级时只需重跑一次组合，工具层无需重建——它从未被并进基础镜像，只是被嫁接过一次。

其它工程细节：层路径须**绝对、无 `..`、不是 `/`**（`internal/service/compose.go:52`）；层数上限 8（`internal/service/compose.go:33-35`）；base 与每层镜像都须**本机存在**（`internal/service/compose.go:44-59`），延续离线姿态；组合中途崩溃会留下带 `compose` 标签的孤儿容器，由收敛器按名字回收（`internal/service/reconciler.go:203-209`，机制见 §10）。

**替代方案**：引入真正的多层镜像构建（`Dockerfile` 多阶段、`buildah`）要依赖 dockerd 或外部工具，与"零依赖、离线"冲突；不支持分层、只支持整镜像模板，则每次组合都重建整镜像，正是要避开的问题。"容器间目录树嫁接 + 同位约定"是在不改引擎、不引依赖前提下拿到分层收益的最小手段。

**怎么验证**：`go test -run TestCopyDirGraftsTree ./internal/runtime/oci/`。它在源容器写入 `/opt/toolkit/bin/run.sh`，`CopyDir` 到另一容器同一路径，读回并逐字节比对（`internal/runtime/oci/oci_test.go:521-541`）。预期通过。组合的端到端行为另见 `go test -run TestTemplates ./tests/`。

## 8.7 pack 防泄漏打包

**问题**：可复用镜像由可写层原样 `Commit` 而来，而沙箱在构建时会留下装包缓存、临时文件、以及**可能被写进去的凭据**（构建脚本从环境变量取的 token、包管理器的认证文件）。这些一旦被固化，而镜像又要分发给多个沙箱，等于**把密钥复制给每个拿到它的人**。反方向的风险同样真实：给 `pack.cleanup` 传 `/usr`，那不是打包策略，是对要被打包的环境搞破坏。

**原理**：提交**之前**对沙箱做一次清理，删掉声明为构建残留的路径，让它们不进镜像；同时**拒绝**把系统目录当清理目标。前者防"不该留的留下"，后者防"不该删的被删"。

**本项目怎么做**：策略由调用方声明，但只能声明"要清哪些残留"，`internal/service/snapshot.go:11-18`：

```go
type PackPolicy struct {
	// Cleanup lists absolute in-sandbox paths removed before committing.
	Cleanup []string `json:"cleanup"`
}
```

清理执行在**提交之前**，且因为删除是容器内动作，一个 `paused` 沙箱会被透明恢复（`internal/service/snapshot.go:28-36`）。清理走 `applyPackCleanup`（`internal/service/snapshot.go:173-187`）：对每个路径调后端 `DeletePath`，校验失败或删除失败都会**中断整个快照**并返回冲突，而不是"删不掉就算了、照样提交"——一个部分清理的镜像混合了干净与不干净，比完全没清理更容易让人误判它干净。

"系统路径拒绝"的清单在 `internal/service/errors.go:110-131`：

```go
		for _, forbidden := range []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/root", "/sbin", "/sys", "/usr", "/var"} {
			if p == forbidden {
				return apperr.Invalid("pack.cleanup refuses system path %s", p)
			}
		}
```

它拒绝的是**精确等于**这些顶层目录的路径（`p == forbidden`），不是"以它们开头"——所以 `/usr/local/cache` 这样的残留仍可清理，`/usr` 本身不行；`/` 单独拒绝（`internal/service/errors.go:121-123`）；数量上限 16，每条先过 `validatePath`（见 8.11）。行内注释把定性写明：清理是给"构建残留"用的，**不是**用来破坏正在打包的环境。

打包结果在快照记录上留一个 `Packed bool`（`internal/domain/artifact.go:21`），并单独记一条 `snapshot.pack` 审计（`internal/service/snapshot.go:80-83`）。这个布尔值重要，因为"这份镜像经过清理流程"是一个**关于镜像属性的事实**，下游需要读到它，而不是去猜。

还有一个同类的泄漏面：沙箱里一次 core dump 会把进程内存（含凭据）写进可写层、进而被提交进快照，项目为此把 core ulimit 钉死为 0，相关控制在 §6。

**替代方案**：提交后扫描镜像、发现敏感内容再删，要在镜像里再起容器、再提交一次，成本更高且"什么算敏感"扫不全；"信任调用方自己清理干净"则把防泄漏责任推给最不该承担它的一方。做成**提交前的必然步骤 + 系统路径硬拒绝**，是加一道无法被跳过的关卡。

**怎么验证**：`bash tests/e2e-enterprise-scenario.sh`（需 Docker）的"复用"一节带 `pack.cleanup` 创建快照并打印 `packed`（`tests/e2e-enterprise-scenario.sh:275-279`）。预期看到 `packed=true` 与提交后字节数。要单独验证拒绝，构造含 `/usr` 的 `pack.cleanup`，预期得到 `invalid_request` 而非被清空的环境。

## 8.8 预热池：为什么"纯净"这个定义比"空闲"强

**问题**：冷创建要起容器、`Start`、轮询确认真实相位，突发时更慢。把"起容器"提前做掉，创建就退化成"把一个已经起好的容器交出去"。但立刻带来安全问题：**被复用过的容器，会不会把上一个租户的东西带给下一个？**

**原理**：把预热容器的安全性钉在一个比"空闲"更强的前提上。**"空闲"只说明它现在没在跑，不说明它曾经跑过什么**——空闲的容器可能刚执行过别人的命令、内存里还留着别人的数据。**"纯净"的定义是：它从未执行过任何工作负载。** 预热容器只经历 `Create` 与 `Start`（PID 1 是一个稳定的 `sleep`），从未被 `exec` 过任何命令，因此它没有"上一个租户"可言——任何租户都不曾在这台容器里留下东西。这就是它可以跨租户交付的原因。

**本项目怎么做**：池的不变量说明在 `internal/service/pool.go:13-25`：池按模板保持 N 个**纯净的、已启动的**容器，"纯净是安全属性"，因为预热容器从未执行过工作负载，交给任何租户都不泄漏。交付路径 `internal/service/pool.go:34-52`，命中时直接改写记录：

```go
	items := s.pool[imageRef]
	if len(items) == 0 {
		s.Reg.Counter("sandboxd_pool_misses_total", "warm pool misses", nil, 1)
		return sb, false
	}
	item := items[0]
	s.pool[imageRef] = items[1:]

	sb.Backend = domain.SandboxBackendRef{Kind: s.Backend.Name(), Ref: item.ref, ImageRef: imageRef}
	sb.State = domain.StateRunning
	s.Reg.Counter("sandboxd_pool_hits_total", "warm pool hits", nil, 1)
	return sb, true
```

两个产品语义。第一，**命中交付直接就是 `running`**：容器已起好，无需 `provision`、无需等状态——取出一个 item、写入后端引用与 `imageRef`、置 `running`、`PutSandbox` 落盘（`internal/service/service.go:137-145`）。这是"毫秒级交付"（`docs/DESIGN.md:82`）的来源，也是命中记 `source=pool`、未命中记 `source=cold` 的原因（`internal/service/service.go:143`、`150`）。第二，**交付点就是所有权移交点**：行内注释（`internal/service/pool.go:28-33`）说明采用**不改容器名**以避免"后端对象身份在变动"的窗口，真正的移交点是**写记录**——写记录前它是池库存（守护进程重启就可能被回收），写记录后它归 store 管。这给崩溃恢复一个明确边界。

**池的键**：这里必须把文档与实现都说清楚，因为两者不一致。设计文档 §3.2 写的是"按 (template, tenant) 维护 N 个预热沙箱"（`docs/DESIGN.md:82`）；而实现里池的键是**解析后的镜像引用**（`internal/service/pool.go:32` 的 `pool map[string][]poolItem`，注释 `// imageRef → warm items`），且补货**只从公开的平台模板**取候选（`internal/service/pool.go:99-104`）。为什么"按镜像引用做键"仍然成立？因为采用也按同一个镜像引用取 item（`poolAdopt` 的唯一调用点在 `internal/service/service.go:137`），而进池的容器只来自平台公开模板——租户私有模板不进池。于是池天生是**平台级共享**、不区分租户。这恰恰把安全责任完全压到"纯净"这一个属性上：**池之所以能跨租户，只因为容器从未被任何人使用过**。一旦"纯净"被打破，按键分租户也救不了——真正的承重墙是纯净定义，不是键的选择。`tests/load_test.go:155-181` 正是拿 alice 写文件、删除、再让 bob 连续领六个沙箱，断言 bob 永远看不到 alice 的文件。

**补货与降级**：后台每 5 秒跑一次 `poolFill`（`internal/service/pool.go:70-81`），把池补到 `PoolSize`，补货会 `Create` + `Start` 一个带 `warm=1` 标签的容器（`internal/service/pool.go:112-126`）。这里有一个实现简化要知道：候选里它**只取第一个公开模板**（`internal/service/pool.go:105-108` 的 `tpl := candidates[0]`），且每次只在池总量不足时补一个——这与设计文档"每 (模板, 租户) N 个"的说法不同（差别见 §16 的已知缺口姿态）。**池为空时的降级路径**就是冷创建：`poolAdopt` 返回 `false`，记录以 `cold` 落盘并派 `provision`（`internal/service/service.go:147-151`）。也就是说，池是**加速器而非依赖**——池没货，创建照样成功，只是慢一点。

**指标**：命中与未命中各暴露一个计数器（`sandboxd_pool_hits_total`、`sandboxd_pool_misses_total`，`internal/service/pool.go:42`、`50`），池规模暴露为 gauge（`sandboxd_pool_size`，`internal/service/pool.go:96`、`127`）。为什么必须可见？因为池的价值完全体现在两个数的比例上；只有"创建总数"时，你无法区分"池在起作用"和"池一直是空的"。指标文本格式见 §13。

**替代方案**：不预起容器，则冷启动的下界就是起容器；预起但允许用过再放回，就要在放回前彻底清理内存状态，而容器档下没有可靠手段（文件能删，匿名内存不能确定清干净）。所以池只能接受**一次性使用**：用掉不回收，缺了再补——这与"纯净"是同一件事的两面。

**怎么验证**：`go test -run TestPoolReuseDoesNotBleed ./tests/`。它开启池，让 alice 写 `/root/secret.txt` 后删除沙箱，再让 bob 连续领六个沙箱，每个都断言该路径**不存在**。预期通过。它的诚实边界被注释写明：通过**不能**区分"复用安全"和"这次恰好没复用上"，只能抓住"真的串了"这个方向（`tests/load_test.go:158-163`）。

## 8.9 TTL 与空闲回收：两条独立的时钟

**问题**：沙箱是被程序创建的对象，程序会崩、会忘。没有自动回收，被遗忘的沙箱会永久占着内存与配额，而多租户最不能容忍"一个租户的遗留物拖垮所有租户"。"什么时候该回收"有两种完全不同的答案。

**原理**：两条时钟回答两个问题。**绝对到期（TTL）**从创建起算，到点就回收、**不管有没有人在用**，防"忘了删"，回答"最多能活多久"。**空闲到期**从**最后一次用户活动**起算，闲了这么久就回收、用着的不会被收，防"建了一堆只用几个"，回答"多久没人碰算废弃"。两者覆盖的失败模式不重叠：TTL 兜底"完全忘了"（连活动都没有），空闲超时兜底"记得但不用"。因此同时保留（`docs/DESIGN.md:80-81`）。配置上 TTL 必填且有上限，空闲超时默认关闭（`defaults.idle_timeout_seconds = 0`，`sandboxd.conf.example:23-24`），因为"多久算空闲"因场景而异。

**本项目怎么做**：回收由 `sweepLoop` 驱动，每 30 秒一次（`internal/config/config.go:228` 的 `SweepEvery`，循环体 `internal/service/reconciler.go:33-44`）。判定在 `internal/service/reconciler.go:76-81`：

```go
		switch {
		case sb.Expired(now):
			expired = append(expired, due{sb: sb})
		case sb.IdleExpired(now) && (sb.State == domain.StateRunning || sb.State == domain.StatePaused):
			expired = append(expired, due{sb: sb, idle: true})
		}
```

两点：**TTL 优先于空闲**（先判 `Expired`），且空闲回收只作用于 `running` 或 `paused`（已经在 `stopping` 的不必再被"空闲回收"）。判定后（`internal/service/reconciler.go:84-113`）先转 `stopping`、调后端 `Stop`、再转 `stopped`，并写成**独立审计事件**：TTL 是 `sandbox.expire`，空闲是 `sandbox.idle_expire`，detail 里带上触发它的秒数（`internal/service/reconciler.go:103-111`）。为什么分开记还要记秒数？因为"它没了"和"它超时了"对值班的人是两页不同的手册；记下秒数，事后才能回答"哪个预算、哪个数值把它收了"。

**"回收由活动驱动"的含义**：空闲时钟的量程起点是**最后一次用户活动**。活动由 `touchActivity` 记录（`internal/service/lifecycle.go:54-67`），**有意义的用户动作都会调用它**：`exec`（`internal/service/lifecycle.go:228`）与上传、下载、列目录、删除文件（`internal/service/lifecycle.go:262`、`284`、`303`、`318`），创建本身亦然。量程 `LastActivityAt` 若从未有活动则用 `CreatedAt`（`internal/domain/sandbox.go:235-241`）。所以"活动驱动"的准确含义是：**只要你还在用，空闲时钟就一直被推后；一旦停下，时钟从你最后一次动手那刻开始走**。一个细节：活动不是每次请求都落盘，而是**每沙箱每分钟最多持久化一次**（`internal/service/lifecycle.go:51-53` 注释与 `57-60` 的节流），因为回收粒度是秒到分钟级，为每次工具调用写一次 WAL（见 §9）不值得。

**回收必须归还配额**：这是"回收"与"改标签"的分界线。回收把状态推进到 `stopped`（终态），而**配额只统计非终态沙箱**——`CountableResources` 对 `stopped`/`failed` 返回全 0（`internal/domain/sandbox.go:277-285`），准入检查 `CheckCreate` 累加每个沙箱的可计资源（`internal/gov/quota.go:77-86`）。于是沙箱一旦变成 `stopped`，下一次创建就不把它计入，槽位自然回到租户手里。整条链路没有任何"显式归还"调用——**归还的机制就是"计数只数活着的"**，这正是它不易出错的原因。配额记账与返还的算法归 §11，这里只强调业务断言：**回收的验收标准不是"记录变成 stopped"，而是"新的创建能被放行"**。

**为什么创建时 `timeout_seconds` 必填（防泄漏）**：若允许"不设 TTL"，被遗忘的沙箱就永远没有兜底时钟。所以规格校验要求它落在 `[30, 平台上限]`（`internal/domain/sandbox.go:174-176`），平台默认值也须 `>= 30`（`internal/config/config.go:458`），而 `mergeSpec` 在请求没给时**填入平台默认值**（`internal/service/service.go:232-234`）——于是"省略"不等于"无限"，只等于用默认的 3600 秒。整条链路上不存在"没有过期时间"的沙箱。`POST /timeout` 可延期，但同样受上限约束且须 `>= 30`（`internal/domain/sandbox.go:269-275`），不能把 TTL 推成无限。

**替代方案**：只有 TTL，则"建一堆只用几个"时空占的沙箱要等满 TTL 才释放，利用率低；只有空闲回收，则危险——只要有任何外部探针定期碰这些沙箱，它们就永远不空闲、永久存活。**两条时钟互为兜底。**

**怎么验证**：`go test -run 'TestElasticity' ./tests/`（需 Docker；空闲预算下限 60 秒，测试较慢）。`TestElasticityIdleReclaimIsActivityDriven`（`tests/commercial_test.go:712-773`）同跑一个不动的沙箱和一个每 5 秒被 `exec` 一次的孪生沙箱，断言前者**不早于**预算被回收、后者仍在 `running` 且还能 `exec`，并断言回收事件恰好一次、detail 秒数正确。`TestElasticityTTLReturnsQuotaWithoutAClientCall`（第 783-822 行）断言配额真的被归还。预期两者通过。

## 8.10 `exec` 的产品语义：argv、硬截止、上限与退出码

**问题**：把"在沙箱里跑一条命令"做成稳定接口，要回答四件事：命令怎么表达（shell 串还是参数数组）；超时是"我最多等这么久"还是"到点就杀"；输出很大怎么办；退出码怎么传回。每个选错都会在真实场景里变成安全问题或数据问题。

**原理**：分别定死。命令以 **argv 数组**为基本形态，shell 串是它的便利包装；超时是**服务端强制的硬截止**；输出上限是**准入处拒绝的上限**，因为控制面把输出缓冲在自己的堆上；退出码透传。

**本项目怎么做**：请求体支持两种表达（`internal/httpapi/handlers.go:308-332`），其中：

```go
	argv := body.Argv
	if len(argv) == 0 && body.Command != "" {
		argv = []string{"/bin/sh", "-c", body.Command}
	}
```

**argv 与 shell 串的差别就是安全含义的差别**。argv 形式下每个元素是独立参数，内核直接交给程序，**不经过 shell 解析**，所以参数里的空格、分号、反引号、`$()` 都只是普通字符，不会被当成命令分隔符或变量展开。shell 串形式则相反：`command` 原样交给 `/bin/sh -c`，由 shell 解析——它"更强大"，但把**注入面**打开了；若调用方把来自外部的字符串拼进 shell 串，那个字符串就能在里面启动别的命令。正确用法：确定"跑这个可执行文件、传这些参数"时用 argv；确需 shell 语义（管道、通配、变量）才用 `command`，且**不要把它与不可信输入做字符串拼接**。两条路径在下游是同一种东西（都是 argv），差别只在谁做了解析。

`cwd`/`env`/`user` 是**这一次执行**的作用域覆盖，映射到后端的 `exec` 创建体 `WorkingDir`/`Env`/`User`（`internal/runtime/oci/backend.go:560-567` 的 `execCreateBody`，由第 590-597 行填充），不改沙箱默认值。`user` 受用户名格式约束（`internal/domain/sandbox.go:196-198`），`workdir` 须绝对无 `..`（`internal/domain/sandbox.go:199-201`）。

**超时是硬截止**，这要和"客户端自己超时"区分开。若只有客户端超时（比如 HTTP 客户端 3 秒放弃），客户端返回"超时"，**但服务端的命令还在继续跑**——它可能再跑半小时、写完输出、改完文件。本项目把截止控制在**服务端**：service 层把客户端值夹进平台契约（超上限直接拒、小于等于零取 60 秒默认，`internal/service/lifecycle.go:229-234`），后端在自己的读循环里检查是否越过 `started.Add(timeout)`，到点让读解冻并置 `TimedOut`（`internal/runtime/oci/backend.go:677-691`）。更关键的是**到点真的杀掉进程**：`internal/runtime/oci/backend.go:642-645` 在超时或出错后 best-effort 发 `DELETE /exec/{id}`，让进程不再继续烧沙箱的 CPU 配额。客户端断开也会经 `r.Context()` 的取消一路传到后端（传播链见 §3），所以"客户端走了"和"服务端到点"两条路都终止命令。

**输出上限是上限，不是偏好**：`max_output` 看似"我想要多少输出"，实际是**上限**——控制面把 stdout+stderr 缓冲在**自己的内存**里再返回，不设顶就能让一个请求逼它申请几个 GB（PoC-4 就是这么打的，注释见 `internal/service/lifecycle.go:235-241` 与 `sandboxd.conf.example:73-77`）。所以 service 在准入处就检查：负值直接拒（避免被下游当成大无符号数），超平台上限 `max_exec_output_bytes`（默认 8 MiB）也直接拒（`internal/service/lifecycle.go:242-248`），并说明 `0` 表示"后端默认的 1 MiB"、**不**表示无限（`internal/runtime/runtime.go:63-67`）。后端还有第二道防线：显式拒绝超过 `MaxBackendOutput`（1 GiB）的 `MaxOutput`（`internal/runtime/oci/backend.go:574-577`、`673-676`），注释说明这是为了接住"忘了在 service 层做准入的调用方"。截断时超限字节被丢弃并置 `Truncated=true`（`internal/runtime/oci/backend.go:722-731`），客户端因此能区分"全部输出"和"被截断"。

**退出码透传**：真实退出码是流结束后从后端查回的**权威值**（`internal/runtime/oci/backend.go:620-625`），不是从输出里猜的。一个不显眼的细节：Docker 会把"exec 启动失败"（OCI runtime 错误）也当成正常流、用退出码 128 加错误文本返回，后端会识别并翻译成可读的冲突错误（`internal/runtime/oci/backend.go:626-640`）；进程表被耗尽（fork 炸弹/进程泄漏）时 `setns` 失败，也被翻译成"沙箱进程表已耗尽"（`internal/runtime/oci/backend.go:600-609`）。

**替代方案**：只支持 shell 串，实现更简单，但把注入面的责任推给每个调用方，且强制"跑一个可执行文件"绕经 shell；超时交给客户端，服务端就无强约束、超时命令会继续跑，对记配额的平台不可接受；输出不限、让调用方控制读取，控制面内存就成了别人能远程决定的资源，正是 PoC-4 的方向。

**怎么验证**：`go test -run TestExec ./internal/runtime/oci/` 与 `go test -run TestExecRejectsMaxOutputAboveThePlatformCeiling ./internal/service/`。前者覆盖 argv 执行、超时、截断等，其中 `TestExecTimeoutKills`（`internal/runtime/oci/oci_test.go:124-141`）跑 `sleep 30` 并设 2 秒超时，断言 `TimedOut` 为真且总耗时不失控；后者验证准入侧上限拒绝（`internal/service/exec_ceiling_test.go:18`）。预期通过。

## 8.11 文件 API：三重校验与各自的边界

**问题**：文件接口接受一个调用方提供的**路径字符串**。任何"把用户给的路径直接交给文件系统"的代码都面临经典危险：`..` 可以跳出预期目录、相对路径不知相对于谁、NUL 或其他控制字符可以藏进来、路径还可能被解析成命令选项。一个能写任意路径的上传接口，仍是一次**未经约束的破坏性操作**。

**原理**：把路径当**不可信输入**，并**在多个层次独立校验**——单一层次一旦被绕过或被后续重构改掉，就没有第二道。本项目有三道：service 层的 `validatePath`；后端各方法的独立复检；以及执行动作时把路径**参数化**（作为被引用的参数，而不是拼进命令文本或当成 tar 成员名）。

**本项目怎么做**：第一道，`validatePath`（`internal/service/errors.go:86-108`），在**每个**文件操作前调用（`internal/service/lifecycle.go:263`、`285`、`304`、`319`）：路径须以 `/` 开头（拒绝相对路径）、长度不超过 4096、不含 NUL、且**按 `/` 分段后不允许任何一段等于 `..`**。

第二道，后端**独立**复检，不假设 service 层查过：`PutFile` 要求绝对且不以 `/` 结尾（不能是目录，`internal/runtime/oci/files.go:27-32`）；`GetFile` 要求绝对（第 91-93 行）；`ListDir` 要求绝对且不含 `..`（第 125-127 行）；`DeletePath` 要求绝对、不含 `..`、且**不是 `/`**（第 160-162 行）。后端可能被测试或未来其它调用方直接驱动，一道在上层被遗漏的准入不能在最底层变成一次任意删除。

第三道，**执行时把路径参数化**：上传时 tar 成员名只取**基础名**（`internal/runtime/oci/files.go:37-38`、`55-60`），路径即便多出别的段也不会变成 tar 里的第二个成员；列目录、删除、补目录这些 shell 动作用的是 `"$1"` 式**被引用参数**并带 `--` 之类的分隔（第 43、129、164 行），路径不会被重新解析成选项或多个参数。

关于**符号链接**要说准确：这些校验都在**字符串层面**、不解析符号链接；但符号链接指向哪里不是"逃出沙箱"的问题——沙箱里的符号链接仍在**沙箱自己的挂载命名空间内**解析（那层边界见 §5、§6）。文件 API 因此能读到的至多是沙箱内某路径的目标，而沙箱内部本就属于该租户。真正要防的是"字符串把操作引到预期之外"和"路径被当成命令解析"，分别由第一、二道与第三道挡住。

各操作自己的边界：

- **上传**（`internal/service/lifecycle.go:254-274`、`internal/runtime/oci/files.go:23-83`）：路径须是**绝对的文件路径**（拒绝目录），父目录不存在时**自动创建**（`mkdir -p`，第 42-51 行），`mode` 缺省 644；**单文件上限** `MaxFileBytes`（默认 100 MiB，`internal/config/config.go:215`），超出返回配额错误而非截断（`internal/service/lifecycle.go:266-268`）。
- **下载**（`internal/service/lifecycle.go:276-293`、`internal/runtime/oci/files.go:88-120`）：路径绝对；超过上限时**返回配额错误**，注释明确"宁可显式报错，也不要静默截断"——被截断的可执行文件比一个错误消息更危险（第 85-87 行）。
- **列表**（`internal/service/lifecycle.go:295-308`、`internal/runtime/oci/files.go:122-156`）：只列**一层**，走 shell 而非归档接口（归档是递归的，列一层用它会浪费，见第 4-6 行注释）。含换行的文件名不受支持——上传侧拒绝控制字符，因此不会出现（第 122-124 行）。
- **删除**（`internal/service/lifecycle.go:310-327`、`internal/runtime/oci/files.go:159-174`）：执行 `rm -rf -- "$1"`，拒绝删除 `/`。

**替代方案**：只在 service 层校验一次，则后端被直接调用时就只剩执行，违反"最底层也要能独立站住"；用真实文件系统调用取代 shell 来列目录/删除，则后端需访问沙箱文件系统——在容器档下意味着挂载宿主路径或进入命名空间，复杂度和耦合更高且会绕过沙箱边界。"参数化 shell + 归档接口"是在既有能力内拿到安全的实用折中。

**怎么验证**：`go test -run TestFiles ./tests/`（需 Docker）。`paths_must_be_absolute_and_traversal_free`（`tests/governance_test.go:250-255`）对相对路径、`/safe/../../etc/escape`、`/root/../etc/hosts` 三种输入断言都得到 `invalid_request`；`uploads_over_the_limit_are_a_governed_refusal`（第 277-285 行）断言超 `max_file_bytes` 的上传得到受治理的拒绝；`file_roundtrip_is_byte_exact`（第 242-248 行）用带 `\x00` 与非 UTF-8 字节的载荷验证往返逐字节一致。预期全部通过。

## 8.12 怎么验证生命周期行为：把"回收是不是真的回收"变成断言

**问题**：本章每条都能被"看起来对了"地实现——暂停后把状态字段改成 `paused`、回收后改成 `stopped`、命中池就改个标签。这些都能让接口返回 200，底层资源却可能根本没动。**怎么用断言区分"真的做了"和"改了个标签"？**

**原理**：去观察**只有"真做了"才会改变的量**，而不是接口自己返回的状态。暂停去看后端真实相位与内存上限（8.2、8.3）；回收去看"配额能不能被重新用掉"（8.9）；删除去看"容器数量有没有降下来"；快照去看"新实例能不能继承状态"（8.5）。凡接口自己"声明"的变化，都不作验收依据。

**本项目怎么做**：分三层，各看一层事实。

**单元/集成层**（直接驱动后端）：`TestPauseResume` 看 `inspect` 回读的真实相位、并在恢复后回读真实内存上限（`internal/runtime/oci/oci_test.go:210-213`、`226-232`）；`TestSnapshotCommitAndRestore` 提交可写层再从镜像起新容器读回文件（第 242-293 行）；`TestCopyDirGraftsTree` 验证嫁接内容逐字节一致（第 521-541 行）。共同点是**全从后端读事实**。

**端到端层**（`tests/`，真实 Docker）：`tests/governance_test.go:118-149` 一正一反验证透明恢复；`tests/commercial_test.go:712-773` 用"不动的沙箱 vs 被持续使用的孪生沙箱"把空闲回收的**活动驱动**性质证死；`tests/commercial_test.go:783-822` 用"配额恰好为 1"这个无第三解的设置证明配额真被归还。资源泄漏在这里被抓：`TestNoBackendLeaks`（`tests/resilience_test.go:171-195`）数管理中的容器，断言"每个运行中沙箱对应一个容器"、删除后回到起点，注释写明要害——"记录消失而容器继续跑"才是最终把节点填满的那种泄漏。`TestSnapshotsDoNotLeakImages` 与 `TestOrphanedSnapshotImagesAreReaped`（第 197-258 行）盯的是快照镜像这条**带标签、因此 prune 永不删**的泄漏路径（见 §10）。

**"回收是真的回收，不是改个标签"为什么重要**：它对应两个完全不同的运维后果。"真的回收"意味着内存、容器、镜像、配额都回来了，节点能继续接活；"改个标签"意味着记录说 `stopped`、容器还在跑、内存还占着、配额还被计——**监控面板一片绿，节点却在慢慢被填满**，直到某天突然拒绝所有创建。这类缺陷不会自己暴露，因为它的表现是"一切正常"直到耗尽。`TestElasticityTTLReturnsQuotaWithoutAClientCall` 的注释把这句话点破了：一个"被标记为 stopped 却仍计入配额"的沙箱，"不是被回收了，是被重新贴了个标签"（`tests/commercial_test.go:778-779`）。把验收断言放在**资源**上而非**状态字段**上，是本章所有验证的共同方法。测试方法论全貌见 §14。

**替代方案**：只断言接口返回的状态，则上面这些缺陷一个都抓不住——它们恰恰是"状态对、资源错"；只看监控指标，则指标也是被测代码自己打的，若回收逻辑根本没跑，指标可能连上报的机会都没有，看不到不等于没发生。

**怎么验证**：把三条关键断言一起跑：`go test -run 'TestPauseResume' ./internal/runtime/oci/`、`go test -run 'TestNoBackendLeaks|TestSnapshotsDoNotLeakImages' ./tests/`、`go test -run 'TestElasticity' ./tests/`。预期全部通过（弹性用例较慢）。若把任意一条回收路径改成"只改状态不碰后端"，对应用例会以"资源没有变化"而非"状态不对"失败——这正是它们的设计意图。

---

### 本章检验

1. `pause` 与 `stop` 在**后端真实状态**上的区别是什么？请从 `inspect` 返回的字段层面说明，并解释为什么如果 `pause` 被实现成"停止容器 + 记一个 paused 状态"，收敛器会在守护进程重启后把沙箱判成 `stopped`。
2. `pause_reclaim` 为什么要先还原内存上限、再解冻？请描述"先解冻再还原"会导致的具体故障序列，并解释代码为什么在还原重试失败后选择"把沙箱留在冻结状态"而不是"先解冻、稍后再修上限"。
3. 预热的容器按"纯净"定义交付给任意租户，而实现里池的键其实是**镜像引用**而非 (租户, 模板)。请说明：为什么"按键分租户"并不能替代"纯净"这个安全属性？如果你要允许一个租户的私有模板也进池，需要先确保什么？
4. `exec` 的 `max_output` 被称作"上限而非偏好"。请从"控制面把输出缓冲在谁的堆上"的角度，解释它为什么必须在**准入处**被拒绝，而不能等到后端返回时再截断；并说明 `0` 为什么**不**表示"无限"。再说清"客户端超时"与"服务端硬截止"在**命令是否继续运行**上有什么本质区别。
5. **动手任务**：打开 `internal/service/reconciler.go` 的 `sweepExpired`（第 65 行起）与 `internal/domain/sandbox.go` 的 `CountableResources`（第 277 行起），顺着"沙箱变为 `stopped`"这条路径，把配额之所以能"自动归还"的每一环写出来（从状态字段到 `CheckCreate` 的计数）。然后回答：`TestElasticityTTLReturnsQuotaWithoutAClientCall` 如果把断言从"第二个创建成功"改成"第一个沙箱的状态是 stopped"，它为什么就不再能证明"配额被归还"了？把答案写成一段话。


# §9 持久化：预写日志、崩溃原子性与备份

## 9.1 从"重写整个 JSON 文件"到预写日志

**问题。** 宿主机被强制断电，重启后控制面起不来。崩溃前最后一次写入只是把某个沙箱从 `provisioning` 改成 `running`，可那次写入是把整个状态重新序列化后覆盖写回同一个文件，写到约四成时断电——前半是新副本、后半是旧副本。致命的不是那一个字段：几个月积累的租户、沙箱、快照记录一起不可解析。

**原理。** 差别在于**新字节落在哪里**。原地改写时新字节覆盖旧字节占用的同一段区间，崩溃点可以是区间内任意位置，新旧字节混在一起，无法判断哪一段完整。追加写只把新字节放到末尾，崩溃最多影响**最后一条**记录，之前的字节一个都没被碰过——**破坏被限制在最后一个写入单元内**。顺带还解决两件事：写入量从"状态规模"变成"改动量"；而**追加的记录可以是一条尚未发生的意图**——"我打算创建一个沙箱"塞不进"当前完整状态"的快照，却能作为一条记录追加进去（状态语义见 §10）。

最小例子（把原地改写制造的混合体直接造出来）：

```go
package main

import ("fmt"; "os")

func main() {
	os.WriteFile("/tmp/s.json", []byte(`{"n":1,"tail":"AAAAAAAA"}`), 0o600) // 25 字节旧状态
	f, _ := os.OpenFile("/tmp/s.json", os.O_WRONLY, 0o600)
	f.WriteAt([]byte(`{"n":2}`), 0) // 新状态只写进前 7 字节就断电
	f.Close()
	b, _ := os.ReadFile("/tmp/s.json")
	fmt.Println(len(b), string(b))
}
```

预期输出：`25 {"n":2}"tail":"AAAAAAAA"}` ——长度没变、JSON 已不合法，看不出崩在哪一条改动上。追加写不会有这种混合体：新字节只在尾部，最后一条不完整就停在它之前。先看最朴素的那种写法会被哪三个场景击穿。

最朴素的持久化是：状态全在内存，每次改完就把整个状态序列化成一个 JSON 文件重写一遍。它会被三个场景击穿。

**场景一，写到一半崩溃。** 重写不是原子动作。进程写到第 200 字节时被 `kill -9`，文件里留下"前半段旧内容 + 后半段新内容"的混合体，JSON 解析直接失败——旧状态和新状态一起报废。

**场景二，写放大。** 假设有 10 万条记录、每条 1 KiB，你只想把某条记录的 `last_activity_at` 往前推一秒，却必须序列化并写下 100 MiB。**写入量与状态规模成正比，而不是与改动量成正比**——这是一个随系统成长而恶化的成本。

**场景三，无法支持"先记意图再执行"。** 本项目要求在真正创建容器之前，先把"我打算创建一个沙箱"持久化下来（状态语义与崩溃后的裁决见 §10）。可"意图"描述的是一个尚未发生、连后端引用都不存在的事实，塞不进一个"已完整的对象快照"。

**预写日志（WAL）的核心思想**绕开了这三点：**先顺序追加一条不可变的记录，再改内存；崩溃后靠重放记录恢复。** 它把"一次改动"从"重写整个状态"降级为"追加一小段字节"，于是写入量与改动量成正比（解决场景二）；追加是顺序写且每条记录自带校验，读时能判断它完整与否（解决场景一）；记录可以是"我要做某事"这种增量事实（解决场景三）。内存状态从此不再是真相，而是日志的物化视图。

**本项目怎么做。** 设计写在 `internal/store/store.go:1` 的包注释：每改动对应一帧 `[len u32][crc32 u32][json payload]`，落盘后调用才返回；打开时重放日志并截断撕裂尾部；压实把状态固化成 `data.json` 快照并清空日志；数据目录上有一把 `flock` 防止两个 `sandboxd` 共用。

钉住正确性的是紧随其后的五条不变式（`internal/store/store.go:18`）。第一条是本章一切推论的地基：**每条变更的"追加 WAL 帧"与"发布到内存 map"，必须发生在同一把写锁的临界区内。** 因为磁盘上的帧顺序，必须与进程对外服务时的可见顺序一致。如果两者能交错——帧已落盘、内存还没更新——那么一次崩溃重放出来的历史，就与进程此前对外宣称的历史不同：某个已返回成功的改动，重启后消失了。

看一条写入路径（`internal/store/store.go:207`）：

```go
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked(fkTenant, "put", t.ID, t); err != nil {
		return err
	}
	s.tenants[t.ID] = cloneTenant(t)
	return nil
```

`appendLocked` 在锁内先落盘并 fsync（本节当它是黑盒，机制见 9.2、9.3），只有它返回 `nil`，`s.tenants[t.ID]` 才被更新，锁一直持到函数返回。反过来读：它失败则内存不会改，它成功则内存一定在解锁前改好——两种情况都不留"盘上有、内存无"或"内存有、盘上无"的窗口。最后一行用 `cloneTenant`，存进 map 的是副本，调用方手里的指针再被改也动不了已持久化的状态。

**替代方案**：可以直接用 SQLite 或 BoltDB，省掉本章后面几乎所有细节。但本项目**状态常驻内存，WAL 只为崩溃后重建它**，不需要查询、二级索引或跨表事务。自写 WAL 换来三样东西：帧格式完全可控（9.5 那个"尾部撕裂 vs 中段损坏"的区分，只有你能决定帧边界与校验方式时才成立）、零第三方依赖（见 §16）、备份与时点恢复可自实现。代价是**并发正确性成为自己的责任**——v1.1.0 那次 4.2% 的丢失就是账单（见 9.9）。

**怎么验证**：`go test ./internal/store/ -run TestReopenRecoversState`（`internal/store/store_test.go:41`）。它写入租户与沙箱、把沙箱从 `provisioning` 转到 `running`，`Close()` 后重开，断言沙箱仍在、状态仍是 `running`、`Version` 恰为 2。

## 9.2 帧格式逐字节：`[len][crc32][payload]`

**问题。** 假设日志就是一串 JSON 对象直接拼接：`{"k":"tenant"...}{"k":"sandbox"...}`。某天磁盘翻了一个比特，第一个对象里的 `}` 变成别的字节。恢复程序从头解析，它不知道第一条记录到哪结束，只能一路试探——**对齐一旦丢失，后面的记录都会被解释错**：要么切出长度错误的记录让校验永远对不上。

**原理。** 三个字段各管一件事，而且**顺序被依赖关系钉死**。长度在最前：读的人得先知道这一帧多少字节，才能把字节范围切出来；若把校验值放在长度前面，你要先读 4 字节校验值，可此时还不知道帧有多长，也就不知道该对哪段字节算校验。校验值在载荷之前：这样"先拿期望值、再边读边算"可以在读到坏数据时立刻停下，不必先把一段可能很大（上限 64 MiB）的载荷读完才发现它坏了。载荷在最后：它长度可变，两个定长字段在前，每帧才有稳定的入口点；于是读日志不需要任何搜索。

最小例子（把一帧的头 8 字节打出来）：

```go
package main

import ("encoding/binary"; "fmt"; "hash/crc32")

func main() {
	p := []byte(`{"k":"tenant","op":"put","id":"t1"}`)
	buf := make([]byte, 8+len(p))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(p)))
	binary.BigEndian.PutUint32(buf[4:8], crc32.Checksum(p, crc32.MakeTable(crc32.Castagnoli)))
	copy(buf[8:], p)
	fmt.Printf("payload=%d bytes, header=% x\n", len(p), buf[:8])
}
```

预期输出：`payload=35 bytes, header=00 00 00 23 19 a5 e5 e5` ——头 4 字节是长度 35（`0x23`），其后 4 字节是这段载荷的 CRC-32C。大端排布让排障的人用 `xxd` 看日志时前四字节就是长度。

先问：给你一段连续字节流，你怎么知道第一帧到哪结束、第二帧从哪开始？没有明确切分，恢复程序读到一半就失去对齐。这就是长度字段存在的理由，也是它必须**在前**的理由——**读的时候得先知道要读多少字节，才能读到校验值本身。**

**本项目怎么做。** 常量与 CRC 表在 `internal/store/wal.go:17`：

```go
var frameCRCTable = crc32.MakeTable(crc32.Castagnoli)

const (
	frameHeaderLen = 8 // [len u32][crc32 u32]
	maxFrameLen    = 64 * 1024 * 1024
)
```

组帧在 `internal/store/wal.go:52`：

```go
buf := make([]byte, frameHeaderLen+len(payload))
binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)))
binary.BigEndian.PutUint32(buf[4:8], crc32.Checksum(payload, frameCRCTable))
copy(buf[8:], payload)
```

四字节长度、四字节 CRC、其后载荷。**字节序是大端**，读写一致（组帧 `internal/store/wal.go:53`，解帧 `internal/store/wal.go:257`）。大端与平台无关，用 `xxd` 看日志时前四字节读出的就是载荷长度，不必脑内翻转；小端在 x86 上转换更快，但会让日志字节流对人不友好，而这个文件的首要用途是被排障的人与工具读取。

**`crc32` 防的是意外损坏，不是恶意篡改。** 磁盘位翻转、总线错误、写到一半断电——这些场景出错比特是"随意落下"的，CRC-32C 检出能力很强：全部单比特错误、全部奇数个比特错误、所有长度不超过 32 比特的突发错误。**但它不是密码学校验和**：CRC 是线性函数且无密钥，知道格式的人可以构造一段不同却 CRC 相同的字节让它"合法通过"。**CRC 的假设是"没有人故意骗它"。** 如果日志可能落在攻击者手里、需要发现篡改，就该用**带密钥的消息认证码**（如 HMAC-SHA-256，密钥不出进程）或数字签名。本项目对这个文件不假设敌手，所以选了廉价的 CRC；真正要防篡改之处它确实用了密码学工具——API Key 只落库 SHA-256 哈希（见 §11）。

CRC **检不出什么**？因为 CRC 对覆盖字节是线性的，**如果损坏模式本身恰是生成多项式的倍数（一个码字），异或进去的增量为零，CRC 不变**——某些特定多比特翻转或落在 32 比特窗口边界的突发错误会漏检，对随机损坏概率约为 2 的负 32 次方。还有一个结构性盲点：**CRC 只覆盖载荷，不覆盖它前面的 8 字节头**。长度或 CRC 字段自身被写坏时 CRC 并不直接校验它们；只是长度一旦被改，切出的载荷区间就变了，CRC 对不上，错误仍会被"间接"发现（`internal/store/wal.go:266`）。

载荷为什么用 JSON？看帧结构体（`internal/store/store.go:186`）：

```go
type frame struct {
	K    frameKind       `json:"k"`
	Op   string          `json:"op"` // "put" | "del"
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data,omitempty"`
}
```

选 JSON 是**可读性与可调试性优先于体积**的取舍：现场可以直接 `xxd` 或抽出载荷 `jq` 一下，看见 `{"k":"sandbox","op":"put","id":"sbx_..."}`，不需要专门解码器；省下的那点字节在"帧数 × 每帧几百字节"的量级上不构成瓶颈。代价是序列化慢一些、体积大一些，本项目接受。

最后回到核心问题：**撕裂尾部为什么能被识别？** 代码里**没有任何文件尾标记或结束帧**，识别靠三件事之一，全在 `decodeFrameAt`（`internal/store/wal.go:253`）：剩余字节连 8 字节头都不够，报 `"trailing bytes shorter than a frame header"`；长度声明超出文件剩余量，报 `"frame claims %d bytes but only %d remain"`；长度读全但 CRC 对不上，报 `"frame crc mismatch"`。崩溃时最后一条写入可能只落了半个头、或头全了载荷缺一截、或载荷全了但被截断——这三种残缺分别被长度检查和 CRC 检查逮住。帧间靠长度前向推进（`internal/store/wal.go:243`、`273`），从新偏移继续解下一帧，直到追平文件长度。

**替代方案**：可以用 Protocol Buffers 或定长二进制，体积更小、解析更快，代价是文件不再可被人眼读取、排障依赖工具链。对"日志要被运维直接看"这个需求，JSON 更合适。

**怎么验证**：`go test ./internal/store/ -run TestCorruptedFrameTruncatesTail`（`internal/store/store_test.go:185`）。它写一条记录后关闭，翻转载荷区一个字节（`blob[len(blob)-3] ^= 0xAA`），重开并断言被损坏的记录**不能**进入内存。对照物是回归测试的 `encodeFrame`（`internal/store/regression_test.go:38`），它用包内的 `frameCRCTable` 与 `frameHeaderLen` 组帧，测试构造的字节不可能与生产格式悄悄漂移。

## 9.3 fsync 的真实语义与代价

**问题。** 一次写入返回 `nil`，调用方把 201 回给了客户端。三十秒后宿主机断电，重启后那条记录不在了。查代码：`Write` 没报错、`Close` 没报错、文件系统也没报错——**它只是还没到盘上**。这类丢失不在任何错误路径上，只有掉电能把它暴露出来。

**原理。** `write()` 做的是"把字节拷进内核页缓存"，它不等设备；`fsync(fd)` 做的是"把尚未落盘的脏页提交给设备，并等设备说写完了"。差别就是**可见**与**持久**的差别：写入后另一个进程立刻 `cat` 能读到新字节，这什么也证明不了——它读的是同一份页缓存。

`fsync` 不保证的事同样重要：它保证"这个文件的数据与元数据已到设备"，**不保证"文件名到 inode 的映射已到设备"**。目录是另一种文件，存的是"名字 → inode"的条目；新建文件、或用 `rename` 让名字指向另一个 inode，改的是**目录的内容**，所以创建或改名之后必须对**目录本身**再 fsync 一次。

最小例子（证明"能读到"不等于"已落盘"）：

```bash
printf 'AAA' > /tmp/demo.log   # write() 返回，字节进了内核页缓存
cat /tmp/demo.log              # 另一个进程立刻读到，与是否落盘无关
```

预期输出：`AAA`。要让文件名落盘，还得对 `/tmp` 这个目录再 fsync 一次。本项目里这两件事分别由 `s.wal.Sync()` 与 `syncDir` 承担（`internal/store/wal.go:70`、`488`）。

先问：**`write()` 成功返回，数据到底在哪？** 大概率只在**内核页缓存**里。写系统调用把字节从用户缓冲区拷进内核页就返回了，磁盘上此刻可能一个字节都没有；这时掉电，页缓存连同写入一起消失。**把"write 返回了"当成"数据安全了"，是这类系统里最常见也最昂贵的错误。**

`fsync(fd)` 把这个文件所有**已进页缓存但未落盘**的修改提交给设备，并**等设备确认写完**才返回。它保证：返回那一刻即使立刻断电，重新上电后这些字节仍在文件里。两个细节：其一，`fsync` **阻塞**，它在等一次物理 I/O，这是代价来源；其二，它只管文件数据与元数据，**不管目录项**——新建文件或 `rename` 之后，要让"这个文件名指向这个 inode"也持久化，还得对**目录** fsync，这就是 `syncDir`（`internal/store/wal.go:488`）存在的理由。

**本项目怎么做。** fsync 出现在四处。第一，每次写帧之后（`internal/store/wal.go:70`）：

```go
		if err := s.wal.Sync(); err != nil {
			// The bytes are in the file but not durable. Returning an error
			// without rewinding would let a later restart replay a mutation
			// the caller was told had failed — a phantom write. Rewind.
			return s.rewindLocked(before, nil, n, len(buf))
		}
```

注释里 "phantom write"（幽灵写）值得展开：`write` 成功而 `fsync` 失败时，帧的字节其实已在文件里。此时直接返回错误，调用方会以为改动没生效而放弃；可重启后重放日志，这条"被告知失败"的改动却生效了——**系统撒谎了**。所以 fsync 失败也必须回滚（`rewindLocked` 把文件截回这一帧之前的偏移并再次 fsync）。`DisableSync`（`internal/store/store.go:56`）只给基准与一次性测试用。第二、三处在压实：先写 `data.json.tmp` 并 fsync（`internal/store/wal.go:463`），`rename` 后再 `syncDir`。第四，清空日志后再对日志 fsync 一次（`internal/store/wal.go:479`）。

**代价量级**：必须诚实地说，**本仓库的代码、测试与文档都没有给出 fsync 实测耗时，因此本章不写具体数字。** 文档对量级的描述只有一句（`docs/LEARNING-GUIDE.md:883`）："fsync 保证落盘，但每次都要等磁盘（这是吞吐的代价）。"据此能得出的、不越界的结论只是：**一次 fsync 约等于一次"等设备确认"的往返，与设备类型强相关，本项目没有在目标硬件上测过它。**

**每事务一次 fsync，还是组提交？** **组提交（group commit）**把时间窗内多个事务攒在一起只做一次 fsync，让一次设备往返摊到 N 个事务上，吞吐提升，代价是每个事务的确认延迟被拉长到窗口长度。本项目选**每事务一次 fsync**：`appendLocked` 在锁内写完帧就同步等落盘，返回即代表持久。理由是写路径频率与延迟预算——控制面写的是租户、Key、沙箱、审计这类记录，不是高频交易流，组提交换来的吞吐有限，而"窗口长度"参数与"提交延迟不确定"会破坏上层"返回即持久"的简单心智模型。不过本项目有另一层"批量"：一次 `UpdateSandbox` 的临界区可连做三次状态转换，只落一帧、只 fsync 一次（见 9.7）——这是**内存内部的合并**，不是跨请求的 fsync 合并。

**fsync 会被谁悄悄放大。** 这是一个真实缺陷，记在 `internal/httpapi/server.go:108`。v1.1.0 把限流器放在认证**内部**（"它需要已认证的租户"），于是**认证失败的请求根本不经过任何限流器**，却仍写一条审计事件——而写审计就是一次 WAL 追加加一次 fsync。注释原文：

```
//	recover → request-id → pre-auth limit → body-limit → authenticate
//	        → per-tenant limit → handler
```

后果：**400 个未认证请求 → 400 次 fsync、0 个 429。** 任何人不需要凭据就能把控制面 I/O 打到饱和。修法不是挪限流器，而是**两级限流**：`preAuthLimit` 在认证之外、按客户端 IP 计数，充当粗糙防洪闸；`rateLimit` 仍在认证内、按租户计数，作为权威业务配额（算法见 §11）。同时认证失败的审计按 IP 分窗聚合，首次全量、之后每 100 次记一条带计数的记录（审计流设计见 §11）。

**替代方案**：可以用 `fdatasync`，只保证文件数据而不保证 `mtime` 之类元数据落盘，通常略快；本项目用 `*os.File.Sync()`（对应 `fsync`），因为语义更强且这点元数据成本在写入频率下无关紧要。另一个"替代"是不 fsync、只依赖定期同步——那等于放弃"返回即持久"，§10 的崩溃恢复叙事随之失效。

**怎么验证**：`go test ./internal/store/ -run TestBatchMutationsSingleFsync`（`internal/store/store_test.go:107`）。它在一个 `UpdateSandbox` 里连做三次状态转换（`running → pausing → paused`），断言 `Version` 只加一、状态是 `paused`——整个临界区只落一帧、只 fsync 一次。放大器由两条测试从两侧夹住：`go test ./internal/httpapi/ -run TestUnauthenticatedFloodDoesNotAmplifyIntoWALWrites`（`internal/httpapi/regression_test.go:124`）断言 400 个被拒请求产生的审计事件远少于请求数；`-run TestAuthFailureAuditIsAggregatedPerWindow`（`internal/httpapi/regression_test.go:147`）放开闸门，断言 250 次失败只塌缩成 3 条审计事件。

## 9.4 原子替换与 flock

**问题。** 运维照文档用 `cp -a` 复制数据目录，复制到一半时进程还在写，备份里的 `data.json` 是"前半新、后半旧"。另一种更常见：压实写 `data.json` 写到一半时进程被杀，正式文件直接报废，而旧版本已被自己覆盖。两者共同点是：**写入期间存在一个"文件既不是旧版本也不是新版本"的时间窗**，任何在这个窗口里读它的人、或在这个窗口里崩溃的时刻，都会拿到坏数据。

**原理。** `rename` 的原子性来自它**改的是目录里的一个条目，而不是文件的内容**：让名字 `data.json` 从旧 inode 重新指向新 inode，是对目录的一次单一修改，内核保证它要么完成、要么不发生。于是任何时刻去读 `data.json`，看到的要么是完整旧内容、要么是完整新内容。

正确顺序是：新内容写进**同目录**的临时文件 → fsync 该文件 → `rename` 覆盖正式名 → fsync 目录。两个前提不能省：跨文件系统的 `rename` 会退化成"复制 + 删除"，把刚消掉的中间窗又请回来；`rename` **之前**必须先 fsync 文件本身，否则名字已指过去、内容还在页缓存里，掉电后得到内容残缺的文件。

最小例子。写者只写临时文件、再用 `mv`（即 `rename`）替换正式文件；读者反复读正式文件，并数出"不完整"的行：

```bash
mkdir -p /tmp/at && printf 'old\n' > /tmp/at/d.json
for i in $(seq 1 120); do printf 'record-%03d\n' $i > /tmp/at/d.json.tmp; mv /tmp/at/d.json.tmp /tmp/at/d.json; done &
for i in $(seq 1 200); do cat /tmp/at/d.json; done | grep -Evc '^(record-[0-9]{3}|old)$'
```

预期输出：`0` ——两百次读取里没有一次看到半截行。`cat` 打开文件时拿到的是当时名字指向的那个 inode，改名前后各自完整。

**原子替换**解决 9.1 的场景一：原地覆盖会在中途崩溃时留下坏文件。解法是**写临时文件 + `rename`**。`rename` 为什么原子？因为 POSIX 规定，在**同一文件系统**内它是原子操作：要么把新名字指向新 inode、要么什么都不做，**不存在"改到一半"的中间状态被别的进程观察到**——任何时刻去看，看到的要么是完整旧内容、要么是完整新内容。前提是"同一文件系统"，跨文件系统的 `rename` 会退化为"复制 + 删除"，不再原子，所以临时文件必须与最终文件同目录。

**本项目怎么做。** 压实的落盘序列是标准示范（`internal/store/wal.go:454`）：

```go
	tmp := s.compactedPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	// ... write blob ...
	if !s.opts.DisableSync {
		if err := f.Sync(); err != nil {
			f.Close()
			return apperr.InternalErr("compact fsync: %v", err)
		}
	}
```

先写 `data.json.tmp`，`Sync()` 把内容逼到盘上，再 `os.Rename(tmp, s.compactedPath())`（`internal/store/wal.go:472`）。顺序很讲究：**必须先 fsync 临时文件内容、再 rename**，否则可能出现"改名成功但新内容还在页缓存里"的窗口，断电后名字指向一个内容不完整的文件——原子替换反而制造了损坏的正式文件。rename 之后 `syncDir`（`internal/store/wal.go:475`），让目录项本身也持久化。

**flock 防双进程。** 再问：如果误起了**两个 `sandboxd`，指向同一数据目录**，会怎样？两进程各有内存状态，各往同一个 `wal.log` 追加，帧交错插在一起，任何一次崩溃重放的历史都不属于任何一方真正服务过的历史；更糟的是两者可能对同一沙箱下矛盾指令。**两个写者 = WAL 前提被破坏。** `Open` 第一步就抢锁（`internal/store/wal.go:504`）：

```go
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, apperr.ConflictErr("data directory is locked by another sandboxd process (%v)", err)
	}
```

它打开数据目录下的 `lock` 文件，请求**排他且非阻塞**的 `flock`。第二个进程抢到时立刻得到 `EWOULDBLOCK`，被转成 `ConflictErr`（映射 409）并**拒绝启动**。锁句柄是 `Store` 实例字段而非包级全局变量（`internal/store/store.go:70`）——这条是 v1.1.0 的缺陷，复盘见 9.9。

**flock 防不住什么？** 三件事。其一，**`flock` 在 NFS 等网络文件系统上语义不可靠**，某些实现把 `flock` 映射成本地锁或干脆返回成功，你以为锁住其实没有，所以数据目录必须在本地文件系统上。其二，**忘了检查返回值等于没锁**：调用方若忽略 `flock` 的错误，锁失败会被静默吞掉，双写者就真发生了；本项目把这个错误一路返回成 `Open` 的失败。其三，**它是劝告锁（advisory）**：只有也遵守 `flock` 的进程才被挡住，一个不用 `flock` 就去写 `wal.log` 的手写脚本照样能进来——所以修复必须走 `Store.Open` 的恢复路径。

**替代方案**：可以用 `O_CREAT|O_EXCL` 的 pidfile，或 `fcntl` 记录锁。pidfile 的问题是崩溃后文件残留，重启时要判断 PID 是否还活着、是否被复用，逻辑易错；`fcntl` 锁与进程关联，进程内任意 fd 关闭都可能释放锁，反而更难推理。`flock` 的"锁与打开的描述符关联、进程退出即自动释放"正好匹配"一个进程占一个数据目录"。

**怎么验证**：`go test ./internal/store/ -run TestDirectoryLockPreventsSecondStore`（`internal/store/store_test.go:326`）断言同一目录第二次 `Open` 必然失败。`-run TestClosingOneStoreDoesNotReleaseAnothersLock`（`internal/store/regression_test.go:561`）断言关甲目录不释放乙目录的锁、甲自己的锁会释放。`-run TestFailedOpenReleasesTheDirectoryLock`（`internal/store/regression_test.go:606`）断言"打开失败也要还锁"，否则一次坏配置就能让数据目录永久无法打开。

## 9.5 撕裂尾部 vs 中段损坏

**问题。** 一次断电后启动，恢复程序报"第 412 帧校验对不上"。运维的第一反应是"把坏的那条之后全截掉，先起来再说"。同一个动作在不同位置上后果截然相反：坏帧在末尾，截掉它一切正常；坏帧在中间，截掉它之后的内容，等于把**几百条已被 fsync、已被确认**的改动一次性抹掉。**同一条处置，在两种位置上一个无害、一个等于制造事故。**

**原理。** 分辨两者只需回答一个问题：**坏帧之后还有没有能解出来的帧？** 有，说明写下那些后续帧时进程还活着，它们被 fsync 过、被确认过；没有，说明写入是在这条坏帧处中断的。校验和只说"这一帧读不出来"，不区分位置——**位置是校验和之外的信息，却决定了语义**。

因此"在第一个坏帧处一律截断"是错的：它把"未提交"与"已提交"当成同一种状态。未提交的残缺可以直接丢，因为调用方从未拿到成功；已提交的残缺不能丢，因为调用方已据此做了后续动作。

最小例子（两种损坏各自的识别方式）：

```go
package main

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

func main() {
	tbl := crc32.MakeTable(crc32.Castagnoli)
	frame := func(id string) []byte {
		p := []byte(`{"id":"` + id + `"}`)
		b := make([]byte, 8+len(p))
		binary.BigEndian.PutUint32(b[0:4], uint32(len(p)))
		binary.BigEndian.PutUint32(b[4:8], crc32.Checksum(p, tbl))
		copy(b[8:], p)
		return b
	}
	all := append(append(frame("a"), frame("b")...), frame("c")...)
	torn, mid := all[:len(all)-3], append([]byte(nil), all...)
	mid[22] ^= 0xFF // 第二帧（中段）的 CRC 被改坏：偏移 18 是它的头，22 起是 CRC
	for _, b := range [][]byte{torn, mid} {
		off := 0
		for off+8 <= len(b) {
			ln := int(binary.BigEndian.Uint32(b[off : off+4]))
			if off+8+ln > len(b) || crc32.Checksum(b[off+8:off+8+ln], tbl) != binary.BigEndian.Uint32(b[off+4:off+8]) {
				break
			}
			off += 8 + ln
		}
		fmt.Printf("解不出的偏移=%d，其后还剩 %d 字节\n", off, len(b)-off)
	}
}
```

预期输出：

```text
解不出的偏移=36，其后还剩 15 字节
解不出的偏移=18，其后还剩 36 字节
```

每帧 18 字节。第一行剩 15 字节，装不下一帧，是"写到一半"的尾部撕裂；第二行剩 36 字节，足以容纳后续帧，必须去确认那些帧是否完好——`intactFrameAfter`（`internal/store/wal.go:342`）做的正是这件事。

这是本章最重要的一节，因为它讲的是一个**安全判断**，不是技术判断。技术上 CRC 只告诉你"这一帧读不出来"，它在文件哪个位置并不改变"它坏了"这个事实。但**同一条坏帧出现在尾部还是中段，含义完全不同，正确动作也恰好相反。**

**尾部损坏**：崩溃时最后一条写入可能只落了一半——进程在 fsync 返回前就被杀了。这意味着**这一帧对应的改动从未被确认为"已提交"**：调用方还在等 `appendLocked` 返回，还没拿到成功、还没据以做任何后续动作。所以它"不完整"完全正常——它本就还不是一次已提交事务。丢掉它，等于假装那次写入从未发生，而这正是调用方所认为的事实。**自动截断尾部是安全的。**

**中段损坏**：文件中间一帧坏了，**但它后面还有完好的帧**。那些后面的帧，是被一个**还在正常运行**的进程写下、并**已 fsync 返回、已向调用方确认**的——它们能出现在损坏帧之后，恰恰证明写它们时进程还活着。此时若像对待尾部那样"从损坏处往下全截断"，抹掉的就是一串**已被确认、已持久化的事务**——这不是崩溃恢复，这是**静默丢数据**，丢的正是 WAL 存在的全部意义。所以正确动作是**拒绝启动，把决定权交给能看到全貌的人**。

**本项目怎么做。** 这个区分写在设计文档的一张表里（`docs/DESIGN.md:219`）：尾部损坏判定为"该帧本就不是已提交事务"，默认"自动截断，记 `RepairReport`"；中段损坏判定为"那些帧已 fsync 并被确认"，默认"拒绝启动，要求人工决定"。三种策略（`internal/store/wal.go:165`）：

- `RepairTailOnly`（默认）：尾部撕裂自动截断；**中段损坏拒绝启动**。注释一句话点透——静默截断中段，丢的是已确认且 fsync 过的事务，"which is exactly the failure a WAL exists to prevent"。
- `RepairSalvage`：在**第一个坏帧处**截断，无论它在哪，损坏点之后的东西**公开地、有意地**丢弃。这是 v1.1.0 的行为，被保留成显式选项，留给"宁可丢一截也不愿停机"的运维。
- `RepairStrict`：任何损坏都不自动修，**连尾部撕裂也要人来确认**。

分类发生在处置**之前**，代码在 `handleDamage`（`internal/store/wal.go:282`）：它调用 `intactFrameAfter` 判断损坏点之后是否还有能解出的帧，把 `kind` 定性成 `"torn-tail"` 还是 `"mid-log"`，再按策略决定 `truncate` 还是 `refuse`。`intactFrameAfter`（`internal/store/wal.go:342`）先走快路径（按当前帧声明的长度推算下一帧边界试解），失败才向后扫描，且**范围被 `damageScanWindow` 限死在 1 MiB**（`internal/store/wal.go:279`），避免整片循环损坏把启动变成 O(n²) 扫描。拒绝启动时错误信息必须给出**可执行的出路**（`internal/store/wal.go:314`）：点名这是 `mid-log`、给出偏移、说明前有多少字节 / 多少帧、提示"从备份恢复或 `repair=salvage` 显式接受丢失"，并且**不改动日志**——保留现场供人拷出分析。

**`RepairReport` 为什么要量化。** 字段在 `internal/store/wal.go:196`：损坏类型 `Kind`、原因 `Reason`、动作 `Action`、偏移 `AtOffset`、保留帧数 `FramesKept`、保留字节 `BytesKept`、丢弃字节 `BytesDropped`。设计文档 §3.8 结尾那句话是核心命题——"**不存在静默丢数据的路径**"。它的**具体含义**是：任何一次丢失都必须满足两个条件——(1) 它不是自动发生的默认行为（中段损坏默认拒绝启动，必须有人显式选 `salvage` 才丢），(2) 丢了多少被如实记录并暴露在启动日志里（经 `Store.Repair` 可读）。换句话说，"可以丢，但不能悄悄丢"。

**替代方案**：v1.1.0 的做法是"在第一个坏帧处一律截断"。它处理尾部撕裂时确实正确，却把"尾部撕裂"和"中段损坏"这两类截然不同的事混成一件事——**默认动作对前者安全、对后者致命**。既然系统已有能力区分它们，就没有理由不区分；本项目因此把"一律截断"降级为需显式开启的选项。

**怎么验证**：`go test ./internal/store/ -run TestTornTailIsRepairedAutomatically`（`internal/store/regression_test.go:418`）构造 `[好帧][好帧][半帧]`，断言默认可启动、报告 `torn-tail`、动作 `truncate`、撕裂点前两帧仍在、半帧没进来。`-run TestMidLogDamageRefusesToStartByDefault`（`internal/store/regression_test.go:447`）构造 `[好帧][坏帧][好帧][好帧]`，断言默认**拒绝启动**、错误信息同时提到 `mid-log`、`offset`、`salvage`，且**日志字节数一字未改**（拒绝不能销毁证据）。`-run TestMidLogDamageSalvageIsExplicitAndReported`（`internal/store/regression_test.go:476`）断言 `salvage` 下报告如实记录 `mid-log` + `salvage`、`BytesDropped > 0`。再加边界：`-run TestIntactLogIsNotReportedAsDamaged`（`internal/store/regression_test.go:516`）确认完好日志不被误判。

## 9.6 压实：日志不能无限增长

**问题。** 一个只追加、从不清理的日志，跑几个月会变成什么？每天两万次改动、每帧约 300 字节，三个月 540 MB。真正的问题不是磁盘占用，而是**启动时间**：每次重启都要把这 540 MB 从头重放到尾，哪怕绝大多数帧是同一个沙箱被反复覆盖的中间态——同一个沙箱更新 300 次，前 299 帧对重建"当前状态"毫无贡献。

**原理。** 日志的用途只有一个：**把内存状态重建到崩溃那一刻**。一旦"当前状态"被固化成一份快照，快照之前的全部帧就失去作用——它们描述的中间态已被快照吸收。压实就是：把内存里全部实体序列化成 `data.json`，原子替换旧快照，再清空日志。

顺序为什么是"先换快照、再清日志"？反过来致命：先清日志再写快照，中间崩溃就什么都没了。按这个顺序，每个崩溃点都至少留下一份完整数据——崩在写快照之前：旧快照 + 完整日志；崩在快照换好之后、清空之前：新快照 + 一段日志，其中改动已包含在新快照里；崩在清空之后：新快照 + 空日志。中间那种情况要求一个性质：**重放必须幂等**，把已包含在快照里的记录再应用一次，结果不变。

**本项目怎么做。** 压实主体在 `internal/store/wal.go:440`：

```go
	blob, err := s.marshalAll()
	if err != nil {
		return err
	}
	tmp := s.compactedPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	// ... write blob, Sync, Close ...
	if !s.opts.DisableSync {
		if err := f.Sync(); err != nil {
			f.Close()
			return apperr.InternalErr("compact fsync: %v", err)
		}
	}
	// ...
	if err := os.Rename(tmp, s.compactedPath()); err != nil {
		return apperr.InternalErr("compact rename: %v", err)
	}
	syncDir(s.dir)
	if err := os.Truncate(s.walPath(), 0); err != nil {
		return apperr.InternalErr("compact truncate wal: %v", err)
	}
	if !s.opts.DisableSync {
		if h, err := os.OpenFile(s.walPath(), os.O_WRONLY, 0o600); err == nil {
			_ = h.Sync()
			h.Close()
		}
	}
```

逐行看：`internal/store/wal.go:441` 的 `s.mu.Lock()` 在函数第一行就拿下写锁，所以 `:450` 的 `marshalAll()` 读到的是**一致时点**的内存状态；`:454` 的临时文件是 `compactedPath() + ".tmp"`，与目标同目录（见 `internal/store/store.go:137`），保证 `rename` 不跨文件系统；`:463` 先 fsync 内容、`:472` 才改名、`:475` 再 `syncDir`，依据见 9.4；`:476` 把日志截到 0；`:479-484` 重新打开日志再 fsync 一次——`Truncate` 改的是日志的长度元数据，不清空它也可能在掉电后被恢复。

触发它的是后台循环 `internal/service/reconciler.go:46` 的 `compactLoop`，间隔由配置给出（`internal/config/config.go:230`，见 §13）。

如果只有追加没有清理，`wal.log` 会**无限增长**。为什么这是真问题？日志里绝大多数帧是**历史的中间态**——同一个沙箱更新一百次，前九十九帧对重建当前状态毫无用处；而重放必须从头读到尾，于是即使每帧只有几百字节，运行数周也会得到几百 MiB 的日志和越来越慢的启动。**日志的用途是"重建当前内存状态"，所以一旦当前状态能被固化，旧帧就可安全丢弃。**

**压实的步骤**是"全量重写 + 截断"：把内存全部实体序列化成快照 `data.json`，原子替换旧快照，再清空 `wal.log`。实现在 `internal/store/wal.go:440`：`marshalAll()` → 写 `data.json.tmp` 并 fsync → `rename` → `syncDir` → `Truncate(walPath, 0)` → 对日志 fsync。它由后台循环每 10 分钟跑一次（`internal/service/reconciler.go:46`，间隔来自 `internal/config/config.go:230` 的 `CompactEvery: 10 * time.Minute`）。

**压实过程中崩溃会怎样？** 逐段分析：

- **崩在 `marshalAll` 或写临时文件阶段**：`data.json` 还是旧的、`wal.log` 完整。重启后 `loadCompacted` 读旧快照，再重放整段日志（包含旧快照之后的全部改动），状态正确。
- **崩在 `rename` 之前**：同上，`data.json.tmp` 是孤儿被忽略。
- **崩在 `rename` 之后、`Truncate` 之前**：`data.json` 已是新快照，`wal.log` 仍保存**从上一个快照到此刻的全部帧**。重启后先加载新快照、再重放这段日志。安全性依赖一个性质：**重放是幂等的**——`applyFrame`（`internal/store/wal.go:363`）对 `"put"` 的动作是"用这条记录覆盖 map 里的同名项"，把已包含在快照里的记录再应用一次，结果与只应用一次相同。
- **崩在 `Truncate` 之后**：`data.json` 新、`wal.log` 空，直接就是压实完成后的正确状态。`Truncate` 不是原子的，但即便只截了一半，那段不完整尾部会被 9.5 的撕裂尾部检测逮住并截掉。

**它与正常写入的互斥关系。** 压实全程持有 `s.mu` 的**写锁**（`internal/store/wal.go:441`），所以不可能与任何一次 `appendLocked` 交错。这一点正是 9.9 那个 PoC 的核心：v1.1.0 里 `Put*` 在锁**外**追加，于是 `Compact` 能在"帧已落盘、内存未更新"的窗口抢到锁，把快照里缺了这条记录、又把含这条帧的日志整段清空，调用方却已收到成功。**修复后 append 与发布共用同一把锁，压实与写入自动互斥**，不需要额外协调。压实还会先检查存储是否毒化（`internal/store/wal.go:447`）：一个写入失败且回滚也失败的存储，内存可能缺一条其实已在盘上的改动，此时压实会把"可恢复的歧义"变成"永久丢失"，所以直接拒绝。

**替代方案**：可以用**增量检查点**或**日志分段**（压实只重写最旧的段），减少每次压实的停顿，代价是引入更多状态与更复杂的崩溃情形。本项目实体不多、内存快照体积可控，全量重写换来"压实逻辑一条直线、崩溃分析只有四种情形"，这个简单性对可靠性更重要。

**怎么验证**：`go test ./internal/store/ -run TestCompactionPreservesState`（`internal/store/store_test.go:210`）写 1 个租户、3 个沙箱、1 个快照，`Compact()` 后断言日志清空到 0 字节，重开后统计仍是 1/3/1 且快照 `SizeBytes` 没丢。`-run TestAcknowledgedWritesSurviveRestartUnderConcurrentCompaction`（`internal/store/regression_test.go:163`）把压实与 24 个并发写入放一起跑 12 轮，断言每轮重启后**没有任何一条已确认写入丢失**。

## 9.7 在存储层实现 CAS：`BaseVersion` 的比对必须锁内完成

**问题。** 一个沙箱正在 `running`。客户端甲想暂停它、客户端乙想续期超时，两人几乎同时读到它，都看到 `version: 7`。甲先算出"暂停后的沙箱"写回，版本变 8；乙后算出"续期后的沙箱"也写回，版本变 9——**乙的写入基于版本 7 的副本，把甲刚写进去的"暂停"整个覆盖掉了**。甲收到 200，它的暂停却不见了，而且没有任何错误。

**原理。** 解法是**乐观并发控制**：给"写回"附加一个前提——"我基于的那个版本仍然是当前版本"。存储层写回时检查这个前提，不符就拒绝，调用方拿到冲突后重新读取、重试。相对的是**悲观**做法：写之前先取得这行记录的排他锁，冲突者排队等待。单机内存场景选乐观，是因为这里没有跨网络的锁管理器，让请求在锁上排队会把可以立刻失败并重试的操作变成阻塞；把冲突暴露成 409，重试的决策权就回到调用方手里（`BaseVersion` 与 409 的契约属于 §10，本节只讲存储层怎么实现它）。

关键在于**检查与写回必须是一个动作，不能是两个**。若分两步——先读当前版本检查、通过后再加锁写回——两个写者可以在同一个旧版本上各自检查通过，然后各自写回，后写的照样覆盖先写的：**检查的意义完全依赖于"从我读到它、到我写下它之间没人能插进来"**。把检查、递增、落盘、发布放进同一个临界区，这个窗口就消成零。

**本项目怎么做。** 更新路径在下面给出，这里看删除路径——同一套检查也套在删除上（`internal/store/store.go:430`）：

```go
func (s *Store) DeleteSandbox(id string, baseVersion int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.sandboxes[id]
	if !ok {
		return nil, apperr.NotFoundErr("sandbox")
	}
	if baseVersion >= 0 && cur.Version != baseVersion {
		return nil, apperr.ConflictErr("sandbox %s was modified concurrently", id)
	}
	if err := s.appendLocked(fkSandbox, "del", id, nil); err != nil {
		return err
	}
	delete(s.sandboxes, id)
	return nil
}
```

`internal/store/store.go:437` 的 `baseVersion >= 0 && cur.Version != baseVersion` 就是那道闸门。`internal/store/store.go:440` 落的是 `"del"` 帧，数据参数是 `nil`，重放时 `applyFrame` 看到 `op == "del"` 就直接 `delete`（`internal/store/wal.go:393`）。`internal/store/store.go:443` 的 `delete` 排在 `appendLocked` 之后：盘上先有删除记录，内存里才消失。

先描述问题。两个客户端几乎同时读到同一个沙箱（都读到 `Version = 1`），都决定改它——甲把状态从 `running` 改成 `pausing`，乙续期 `timeout`。若不加保护就会**丢失更新**：两人各自基于 `Version = 1` 的新副本写回，后写的覆盖前写的，前一个人的改动无声消失。

这个机制的语义（`BaseVersion` 的含义、409 的契约、为什么只有客户端路径需要它）属于 §10；本节只讲**存储层怎么把它实现成"不可能发生丢失更新"**。领域对象上的两个字段（`internal/domain/sandbox.go:231`）：

```go
Version        int64     `json:"version"`
BaseVersion    int64     `json:"base_version"`
```

`Version` 是记录被写入的次数；`BaseVersion` 是"读它的人当时看到的 `Version`"。写回时调用方把读到的 `BaseVersion` 一并传入，存储层拿它与**当前**记录的 `Version` 比对。核心在 `UpdateSandbox`（`internal/store/store.go:407`）：

```go
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.sandboxes[id]
	if !ok {
		return nil, apperr.NotFoundErr("sandbox")
	}
	next := cloneSandbox(cur)
	if err := fn(next); err != nil {
		return nil, err
	}
	if baseVersion >= 0 && cur.Version != baseVersion {
		return nil, apperr.ConflictErr("sandbox %s was modified concurrently", id)
	}
	next.Version = cur.Version + 1
	next.UpdatedAt = time.Now().UTC()
	if err := s.appendLocked(fkSandbox, "put", id, next); err != nil {
		return nil, err
	}
	s.sandboxes[id] = next
	return cloneSandbox(next), nil
```

逐行看：先拿写锁；取出当前记录 `cur`；**克隆**一份给回调 `fn` 改（改副本，见 9.1 不变式二）；回调失败则原样返回、不落盘；然后才做 CAS 比对——传入的 `baseVersion` 与 `cur.Version` 不等就返回 `apperr.ConflictErr`（映射 409，见 `internal/apperr/apperr.go:42`）；通过则 `next.Version = cur.Version + 1`，**追加日志并 fsync**，最后才发布进 map。注意顺序：**「比对 → 递增 → 落盘 → 发布」全在同一次 `s.mu.Lock()` 的临界区内**，中间不放锁。`baseVersion == -1` 表示跳过比对，留给已持有权威视角的内部流程，其语义见 §10。

**为什么必须在持写锁的临界区内完成？** 证明如下。假设把"先查后写"拆开——比对一次、落盘发布在另一时刻。两个 goroutine 甲、乙都读到 `Version = 1`、`baseVersion` 也都是 1：甲比对通过，乙比对通过——**因为两次比对看到的都是同一个旧值 `cur.Version = 1`**。于是两人各算出 `Version = 2` 并各自落盘，甲的改动被乙覆盖，丢失更新就此发生。本质是：**比对与写回之间存在一个窗口，若窗口里别人能改状态，比对就失去意义。** 把比对、递增、落盘、发布塞进**同一个不可分割的临界区**，窗口就消成零——临界区里永远只有一个写者，它读到的 `cur.Version` 在它写下 `next.Version` 前不可能被别人改动。这既是"先查后写必须同锁"的完整理由，也是 9.1 不变式一在 CAS 场景下的又一体现。它也顺带解决了批量问题：回调 `fn` 在同一临界区可连做多次状态转换，外层只落一帧、只 fsync 一次，于是"内存里改三次、盘上写一次"是真的。

**替代方案**：可以用"`Version + 1` 相对比较"——要求调用方传入的期望版本是当前版本加一。它的缺陷是**强制"内存改一次就必须立刻落盘一次"**，不允许批内合并，而且调用方必须知道当前版本才能加一。`BaseVersion` 允许用"我读到的那个版本"作基线，天然支持批内合并。也可以用数据库行锁（若换 Postgres 后端），但那是"悲观锁 + 阻塞等待"，本项目在单机内存场景下要的是无阻塞的乐观并发。

**怎么验证**：`go test ./internal/store/ -run TestCASMechanics`（`internal/store/store_test.go:83`）覆盖三条路径——过期基线被拒绝、`-1` 能禁用比对、正常更新让 `Version` 递增。压并发的是 `go test ./internal/store/ -run TestConcurrentCASExactlyOneWinner -race`（`internal/store/store_test.go:300`）：16 个 goroutine 拿同一基线抢更新，断言**恰好一个赢**（`-race` 的用法见 §14）。

## 9.8 备份与时点恢复

**问题。** 运维每天凌晨停机做一次 `cp -a`。某天宿主机磁盘故障，数据目录整块丢失；他拿出备份恢复，却发现备份打不开——那次复制是在服务**运行中**做的：复制 `wal.log` 时进程又追加了几帧。得到的是一份"来自不同时刻的字节拼在一起"的目录，重放时校验对不上。**备份的难点从来不是"复制"，而是"复制到的是不是一个自洽的时刻"。**

**原理。** 一份可用的备份必须**自洽**：里面的字节要能被恢复程序按同一套规则从头解释一遍。`data.json` 是一份完整快照，`wal.log` 是它之后的增量；只要两者属于同一时刻，"先加载快照、再重放日志"就能重建状态（`internal/store/store.go:121`、`124`）。运行中直接复制破坏了这个前提：复制持续若干秒，写入一直在发生。

**本项目怎么做。** 一致性导出在 `internal/store/store.go:794`：

```go
func (s *Store) Backup(dir string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return apperr.InternalErr("backup mkdir: %v", err)
	}
	blob, err := s.marshalAll()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "data.json"), blob, 0o600); err != nil {
		return apperr.InternalErr("backup write: %v", err)
	}
	return nil
}
```

`internal/store/store.go:795` 取的是**读锁**而不是写锁：导出期间仍允许并发读，但挡住了所有写入临界区；`:800` 的 `marshalAll()`（`internal/store/store.go:810`）拿到的是一致时点的内存状态。`:804` **只写 `data.json`，不写 `wal.log`**——产物就是"一份完整快照 + 一个不存在的日志"，打开时 `loadCompacted` 加载快照、`replayAndRepair` 发现日志不存在便直接返回（`internal/store/wal.go:227`），不需要任何增量重放。停机场景下由部署文档给出等价做法：直接 `cp -a`（`docs/DEPLOYMENT.md:99`）。

**为什么数据目录可以直接 copy？** 因为恢复所需的一切都在目录里、且格式自包含：`data.json` 是压实时固化的全量快照，`wal.log` 是自上次快照以来的全部增量帧。`Open` 的流程就是"先加载快照、再重放日志"（`internal/store/store.go:121`、`124`），所以把整个目录原样复制到干净位置、让 `sandboxd` 指向它，就能完整重建状态——不需要外部数据库、不需要单独备份索引。部署文档给的也正是这一句（`docs/DEPLOYMENT.md:99`）：备份 = 复制 `<data_dir>`（WAL + `data.json`）；运行中备份用存储层的一致性导出，离线（停机）备份直接 `cp -a`。

"运行中备份"要用一致性导出，是因为**边跑边 copy 可能读到撕裂的文件**：你复制 `wal.log` 时，进程可能正往末尾追加。存储层为此提供 `Store.Backup`（`internal/store/store.go:794`）：它在**读锁**下（`s.mu.RLock()`）调用 `marshalAll()` 把内存全量状态序列化出来写成一个 `data.json`。持读锁意味着此刻没有写入在临界区内，导出的是一份**逻辑上一致的时点快照**；它输出完整快照而非"快照 + 日志"，所以一次 `Store.Backup` 的产物本身就是一个可 `Open` 的干净数据目录。

**能恢复到哪个时点、不能恢复到哪个？** 看端到端测试 `TestPointInTimeRecovery`（`tests/resilience_test.go:76`）：它在备份后继续创建一个新租户，然后模拟灾难（`rm -rf` 掉实时目录）、把备份恢复回去，断言三件事——备份前存在的沙箱与快照**都在**、备份前那对凭据仍能用（能认证读 `/v1/sandboxes` 得到 200）、备份**之后**创建的租户**不在**。最后一条是"时点恢复"的关键：恢复的是**备份那一刻**的状态，不是"合并"——备份之后的任何变化都不出现。所以**能恢复到备份时刻，不能恢复到备份之后的任何中间时刻**；两个备份之间的数据窗口是丢失的。

**"数据目录丢失 = 不可恢复"为什么成立？** 因为租户、API Key、沙箱、快照、模板、审计事件这些**记录**只存在于数据目录里。容器、镜像这些**后端对象**虽然物理上还在宿主机上，但只是记录的"实现物"——没有记录指向它们时，系统既不知道属于谁、也不知道该是什么状态；收敛器会把它们当孤儿回收（回收机制见 §10）。所以目录一丢又没备份，那些记录就永久消失，没有任何其他地方能重建。这就是部署文档把"定期备份 data_dir 并演练恢复"列入安全基线清单的原因（`docs/DEPLOYMENT.md:123`）。

还有一层恢复后必须做的事：**对齐记录与后端对象的引用一致性。** 备份里可能记录着一个 `running` 的沙箱，而它引用的容器早已不在。恢复后必须跑一次收敛，让"记录里有、后端没有"的对象被明确判失败，而不是挂一个永不消失的 `running` 假象。收敛器规则与状态语义属于 §10。

**替代方案**：可以做持续 WAL 归档 + 任意时点恢复（异地留存每个日志段，允许恢复到任意一秒），能缩小数据窗口，代价是多一套归档与传输组件，以及恢复时"重放到哪一秒"的新决策。本项目定位单机自托管，`cp -a` 一个目录的简单性优先。

**怎么验证**：`go test ./tests/ -run TestPointInTimeRecovery`（`tests/resilience_test.go:76`）走完整灾难流程。更轻的一致性导出由 `go test ./internal/store/ -run TestBackupRestoreRoundTrip`（`internal/store/store_test.go:276`）覆盖：`Backup` 出的目录能被直接 `Open`，租户与沙箱都在。

## 9.9 三个 PoC 复盘

**问题。** 这三个缺陷在 v1.1.0 里都通过了当时的测试套件。锁外追加在单次运行里大概率不复现——它依赖"帧已落盘、内存未更新"这个窗口恰好被压实的加锁撞上；回滚是空操作在没有 I/O 错误的机器上永远不出现，而持续集成从不制造 I/O 错误；包级全局锁只在"同一进程里存在两个 Store"时才显形，而正常路径永远只开一个。那么"修好了"该怎么证明？

**原理。** "证明修好了"需要三类不同的证据，缺一类就留下盲区。

第一类是**可复现**：把缺陷从"我推断它会发生"变成"它确实发生过"。这需要主动构造触发条件：统计性的并发交错要跑很多轮才能命中窗口，残缺的磁盘字节则要**亲手写下去**。

第二类是**不改变被测对象**：复现程序只调用导出 API。一旦为了复现而在产品代码里开一个故障注入开关，你测的就不是线上跑的那个东西（`poc/README.md:4`）。

第三类是**把结论固化成断言**：复现程序能证明"缺陷存在过"，却不能防止它被人改回去。所以修复之后要把它改造成"行为一旦被破坏就以非 0 退出"的 harness，对统计性缺陷还要加**静态断言**——用源码结构而不是运行结果来断言性质，因为一次跑绿的负载测试不构成正确性证据（`internal/store/regression_test.go:106`）。

**本项目怎么做。** 三个程序各自把"触发条件"显式构造出来。`poc/walrace` 的核心只有十几行（`poc/walrace/main.go:56`）：

```go
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = st.Compact()
		}()

		ids := make([]string, 0, putsPerTry)
		for i := 0; i < putsPerTry; i++ {
			id := fmt.Sprintf("tnt_poc_%02d", i)
			if err := st.PutTenant(&domain.Tenant{ID: id, Name: "poc"}); err != nil {
				panic(err)
			}
			ids = append(ids, id) // PutTenant 返回 nil == 已提交成功
		}
		wg.Wait()
```

`poc/walrace/main.go:58-61` 起一个 goroutine 跑一次 `Compact()`；`poc/walrace/main.go:64-70` 主线程连做 24 次 `PutTenant`，把每次返回 `nil` 的 id 记下来——**返回 `nil` 就是"已提交"的凭证**；`poc/walrace/main.go:71` 等压实结束，然后 `:78` 重新打开同一目录模拟重启、`:83-87` 逐条查回。跑 120 轮并报告丢失率与首次复现轮次（`poc/walrace/main.go:102`）。

**替代方案。** 第一种替代是"只写回归测试，不写复现程序"。测试能锁住行为，却锁不住**"这个缺陷真实存在过"这件事本身**。PoC 留下的是一条可运行的因果链——改回去就退出非 0。

第二种替代是在产品代码里加故障注入开关（让 `write`/`fsync` 可按配置失败），用测试驱动它。它覆盖面更广，代价是被测对象与线上对象不再是同一个二进制。这三个程序选择只调用导出 API（`poc/README.md:4`），宁可覆盖面窄，也不为测试改动产品代码。

`poc/` 下三个独立程序专门复现存储层的三个已修复缺陷，共同点是**只读代码、只调用导出 API、不改产品逻辑**（`poc/README.md:4`），而每个都值得单独写成一个程序。

**`poc/tornframe`——撕裂帧的"回滚"曾是空操作。** 缺陷（`poc/tornframe/main.go:4` 起）：v1.1.0 的写入失败回滚调用 `Truncate(walSize())`，而 `walSize()` 在**写完之后**读文件大小——截断到当前大小等于什么都没做，撕裂帧留在盘上；恢复路径又在**首个坏帧处直接 `break` 并把其后全部截断**，于是坏帧之后那些已 fsync 成功、已确认的事务被整体丢弃（PoC 记录：坏帧后 2 条已确认事务消失，WAL 从 374 字节缩到 94 字节）。修复后它变成一个**验证 harness**：构造五种日志布局（尾部撕裂、中段损坏、中段损坏 + 显式 `salvage`、`strict`、完好日志），逐一断言新策略行为，并在默认策略下出现"中段损坏被静默截断"时**以非 0 退出**。它值得单独写程序，是因为要直接写下**原始的、故意残缺的磁盘字节**（如 `half[:len(half)/2]`），跨多种损坏形态 × 三种策略组合验证；而且它本身是一个**可执行的结论**——把"不存在静默丢数据的路径"从报告里的一句话变成"若被破坏就退出非 0"的声明。修复后锁住它的是 `internal/store/regression_test.go` 的六条测试（`TestTornTailIsRepairedAutomatically:418`、`TestMidLogDamageRefusesToStartByDefault:447`、`TestMidLogDamageSalvageIsExplicitAndReported:476`、`TestStrictRepairRefusesEvenTornTail:503`、`TestIntactLogIsNotReportedAsDamaged:516`、`TestUnknownFrameKindRefusesRatherThanTruncates:538`）——最后一条针对未知帧类型：它**不是损坏**，但在此截断会丢掉其后所有帧，所以必须停止启动而非静默截断。

**`poc/walrace`——锁外追加与压实并发导致已确认写入丢失。** 缺陷（`poc/walrace/main.go:1` 起）：`PutSandbox/PutTenant/...` 的顺序是"先 `append`（写盘 + fsync，此时**未持锁**）→ 再取锁更新内存 map"，而 `Compact()` 全程持写锁。交错卡在窗口里：`append` 完成（帧已在 WAL）→ `Compact` 抢到锁，此刻记录**尚未进内存 map**，于是它既没被写进 `data.json`、其 WAL 帧又被 `Truncate(wal.log, 0)` 清掉 → 调用方拿到 `nil`（成功），但记录在磁盘上不存在，重启即丢。程序让一次 `Compact` 与 24 次并发 `PutTenant` 竞争，反复最多 120 轮。CHANGELOG 记录的实测是"2880 次写入全部返回成功、重启后丢 120 条（4.2%，120/120 轮复现）"（`CHANGELOG.md:373`）。它值得单独写程序，是因为这是一个**统计性**并发缺陷——单跑一次测试完全可能因交错没命中而变绿，程序把"何时能复现"也变成可观测（跑多轮、报告丢失率与首次复现轮次）。修复后锁住它的是两道互补断言：静态的 `TestAppendAlwaysUnderWriteLock`（`internal/store/regression_test.go:106`）用 AST 扫描源码，断言每个调用 `appendLocked` 的函数都同时持写锁——**故意做成静态检查**，注释写明"这种形状的并发缺陷按统计复现，一个通过的负载测试不构成正确性证据；v1.1.0 自己的测试套件是全绿的"；行为侧的 `TestAcknowledgedWritesSurviveRestartUnderConcurrentCompaction`（`internal/store/regression_test.go:163`）跑 12 轮并发压实 + 写入，断言零丢失。两条一起，才既锁住"代码结构对"、又锁住"运行行为对"。

**`poc/lockleak`——单写者目录锁用了包级全局变量。** 缺陷（`poc/lockleak/main.go:1` 起）：v1.1.0 把锁句柄存成包级全局变量，同一进程内打开第二个 `Store` 会**覆盖**第一个的句柄，造成两个后果——(a) 第一个 Store 的锁 fd 永久泄漏；(b) 关闭第一个 Store 时会去解**第二个**数据目录的锁，而第二个 Store 还开着。程序打开两个指向不同目录的 Store，用"独立 `open()` 能否取得排他 `flock`"探测锁是否仍被持有，先关 A、再查 A 与 B。CHANGELOG 记录的实测是"关闭 A 后 B 的锁被释放；残留 1 个 fd"（`poc/README.md:23`）。它值得单独写程序，是因为它关心的不是数据而是**进程级资源状态**——文件描述符表和 `flock` 跨 fd 的语义，这种缺陷要在"同一进程里同时存在两个不相关的 Store"这个布局下才显形，很难用一个干净的单元断言表达；程序用 `os.ReadDir("/dev/fd")` 数 fd、用独立 `open()` 探锁，把 (a)(b) 分别变成可见输出。它还直接证伪了一条文档承诺——`THREAT-MODEL.md` 与 `DEPLOYMENT.md` 都声称"flock 防双进程共写"，而只要进程内出现过第二个 Store，这个保证就失效。修复后锁住它的是 `TestClosingOneStoreDoesNotReleaseAnothersLock`（`internal/store/regression_test.go:561`）与 `TestFailedOpenReleasesTheDirectoryLock`（`internal/store/regression_test.go:606`）。

**怎么验证**：`go run ./poc/tornframe`、`go run ./poc/walrace`、`go run ./poc/lockleak`（均从 `castle/` 目录执行，`poc/README.md:9` 给了运行前提）。前两个在修复后应分别打印"尾部撕裂自动修复；中段损坏默认拒绝启动、可显式 salvage"与"零丢失"，若修复被回退会以非 0 退出；第三个应显示 A 的锁正常释放、B 的锁仍被持有、无 fd 残留。

---

### 本章检验

1. `frameHeaderLen` 是 8，但 CRC 只覆盖载荷、不覆盖它前面的 8 字节头。请构造一个具体场景，说明"长度头被写坏"为什么仍会被现有检查发现（或说明它在什么情况下会漏掉），并指出这一分析依赖 `decodeFrameAt`（`internal/store/wal.go:253`）里的哪几行。
2. 假设把 `UpdateSandbox`（`internal/store/store.go:407`）的 CAS 比对挪到 `s.mu.Lock()` **之前**（先读 `cur.Version` 比对、再重新加锁落盘）。请写出两个 goroutine 的交错时序，说明为什么这会导致丢失更新，并解释为什么"重新加锁"本身无法弥补这个窗口。本题只要求讲机制；`BaseVersion` 的语义与 409 契约由哪一章负责，请指出。
3. 压实"崩在 `rename` 之后、`Truncate` 之前"时，重启后的正确性依赖什么性质？如果 `applyFrame`（`internal/store/wal.go:363`）对 `"put"` 的语义改成"若已存在则跳过、不覆盖"，这个崩点还会安全吗？给出判断与理由。
4. 一个运维说："日志尾部那一帧读不出来，那就是坏的，直接删掉不就完了？"请用 9.5 的框架反驳他，并说明为什么同一条"坏帧"出现在中段时正确处置恰好相反。回答时要解释清楚"已提交"与"能读出来"为什么不是同一件事。
5. **动手任务**：从 `castle/` 目录依次跑 `go run ./poc/tornframe` 与 `go test ./internal/store/ -run 'TestTornTail|TestMidLog|TestStrict' -v`，对照两者输出，找出程序里的"场景 B（中段损坏）"与测试 `TestMidLogDamageRefusesToStartByDefault` 各自断言了哪几件事，说明它们**重合的断言**与**互不覆盖的断言**各是什么。然后回到 `internal/store/wal.go` 的 `handleDamage`（第 282 行），逐句确认策略分派（`RepairStrict` / `RepairSalvage` / 默认 `RepairTailOnly`）与程序四个场景的行为完全对应。把你的对照写成一段结论，并回答："如果只留下测试、删掉 PoC，我们会损失哪一类证据？"


# §10 崩溃恢复与一致性：意图先行、对账与乐观锁

## 10.1 崩溃留下的三种不一致

### 问题

`sandboxd` 是一个持有内存状态的进程：它知道"我创建了哪些沙箱、每个现在是什么状态"。与此同时，磁盘上的数据目录里有一份可重放的日志，容器引擎里有一批**真实存在的对象**。这三份东西不是同一份，它们靠运行中的进程去同步。现在执行一条命令：

```bash
kill -9 $(pgrep sandboxd)
```

`SIGKILL` 不可捕获：不走 defer、不走信号处理、不给优雅停机任何机会。进程立刻消失，而它留下的世界是这样的——

- 内存中"沙箱 A 正在 provisioning"这条记录没了；
- 引擎里为沙箱 A 创建的容器还在跑（它属于引擎，不属于这个进程）；
- 日志的最后一帧可能停在任何位置（见 §9）。

新进程启动后面对一个**已经不一致的世界，而且它自己对此一无所知**。它必须回答三个问题：那条 `provisioning` 记录对应的容器到底建成了没有；引擎里那些它没有记录的容器是谁的、能不能删；它记为"运行中"的沙箱是不是早就被外部干掉了。

三个问题答错的后果并不同：第一个答错产生幽灵记录（系统以为有，实际没有）；第二个答错会**删掉别人的东西**；第三个答错会让平台对租户承诺一个不存在的沙箱。这一章讲的就是本项目怎么把这三个问题变成**可判定的**。

### 原理

为什么一致性不能靠"同时更新"来保证？因为三份状态分属三个所有者：进程内存、日志文件、容器引擎，三者之间**不存在共同的事务边界**。唯一能同时触及三者的角色就是那个进程，它死掉的瞬间，同步者消失，而另外两方各自留在自己的状态里。这不是代码写错了，是结构决定的。

所以"一致性"在这里只能是**事后的**：由新一轮进程拿着一份持久化的记录，去和一个仍然活着的真相源比对。这样做能成立的前提只有一个——**必须有一方持有不可否认的真相**。容器存在与否是引擎能回答的事实，而"我打算创建"只是意图。于是恢复的正确性问题，退化成了一个可枚举的判定问题：意图在不在、对象在不在，四种组合，每种给一个裁决。

### 本项目怎么做

三个问题在本章各有归属小节：意图与对象的组合裁决见 10.3，谁来跑这个裁决、什么时机跑见 10.4 与 10.11，没有记录的后端对象怎么处置见 10.6 与 10.10。把它们串起来的入口在启动路径上（`cmd/sandboxd/main.go:56` 的 `run`）：先打开存储（日志重放，见 §9），再起后台循环，再做一次启动对账，**最后才创建 HTTP 监听器**（`cmd/sandboxd/main.go:109`）。顺序本身就是这个设计的可见形态。

### 替代方案

第一种是把三份状态纳入一个分布式事务（两阶段提交）：引擎不提供 prepare/commit 接口，容器创建不是一个可回滚的参与者，此路不通。第二种是"重启时把所有非终态记录一律标记成 failed"：这是**猜测**，它会误杀那些其实已经建好的容器。第三种是把状态托管给外部数据库、由数据库事务保证：多一个部署依赖，且数据库回答不了"引擎里有什么"，比对照样要做。

### 怎么验证

```bash
go test ./tests/ -run TestCrashRecovery -v
```

三个子测试分别断言：`a_sigkill_during_create_converges_instead_of_hanging`（`tests/resilience_test.go:23`）在 `wait=false` 制造的在途窗口里杀进程，重启后断言状态**不再是** `provisioning`（`tests/resilience_test.go:38`）；`a_live_sandbox_survives_a_daemon_crash`（`:41`）断言活着的沙箱重启后仍 `running` 且仍可 exec；`repeated_crashes_do_not_lose_or_duplicate_records`（`:54`）连做三轮崩溃重启，断言记录数不变、状态不变。需要本机有可用的容器运行时（套件在 `SBX_REQUIRE_DOCKER=1` 下不允许静默跳过，见 `docs/DESIGN.md:304`）。

## 10.2 不变式：本地记录是假设，后端是真相

### 问题

重启后的进程手里只有一份自己写的记录。它凭什么相信这份记录？如果信任它，就会出现"记录说有、实际没有"的幽灵沙箱；如果完全不信任它，又无从知道该去查哪些对象。缺少一条判定规则，恢复代码就会变成一堆按经验写的 if。

### 原理

规则要解决的是**信任分配**问题。两份冲突的信息里，必须指定一方为仲裁者，否则任何裁决都是任意的。选择仲裁者的标准是：**哪一方不会因为进程死亡而停留在中间态**。意图会——进程死了，意图永远停在"打算做"；事实不会——容器要么存在要么不存在，引擎总能回答。所以仲裁者只能是后端。

这条规则还有一个方向性的后果值得单独点出：它意味着**写方向**和**读方向**的裁决是相反的两条。记录比后端多（记录说有、对象没有）→ 删记录、判失败；后端比记录多（对象在、没人认）→ 删对象、判残骸。两个方向都要动，但动的是不同的一侧。

### 本项目怎么做

`internal/service/reconciler.go:13` 的注释把整套设计压成四行，值得逐字读：

```go
// The reconciler is the crash-recovery brain. Its one rule: local records
// that describe in-flight operations are hypotheses; the backend is truth.
// Every non-terminal record is compared against the backend and converged;
// backend objects without records (crash residue) are reaped.
```

两条可执行规则：凡是非终态的本地记录，都要拿去跟后端比对并收敛；凡是没有本地记录对应的后端对象，都是崩溃残骸，要被回收。它把"崩溃恢复"从猜测变成了查询——系统不问自己记得什么，去问后端真实存在什么。

### 替代方案

反过来把数据库当真相是更常见的选择：先信记录，再拿记录去"修复"后端（记录说 running 就重新起一个容器）。这在"后端对象是进程私有物"的假设下成立，在容器引擎下不成立——引擎上可能跑着别人的容器，重建动作会与真实所有者冲突。另一个替代是"双向合并"（取记录的元数据 + 后端的状态），代价是要定义每个字段的合并规则，字段一多就无法推理。

### 怎么验证

读 `internal/service/reconciler.go:125-228` 一次，把每一处写操作按"改的是记录侧还是对象侧"分类。预期：`s.failSandbox` 与 `UpdateSandbox` 只改记录，`s.Backend.Remove` 与 `RemoveImage` 只动对象，两者不会在同一次判定里同时发生。若发现某条路径两边都改，那条路径就违反了这条不变式。

## 10.3 意图先行：为什么先落盘再碰后端

### 问题

创建沙箱要走两步：在存储里留一条记录，在引擎里起一个容器。两步之间的任意时刻都可能断电或被杀。如果顺序是"先起容器、再写记录"，那么窗口期内进程一死，就留下一个**没有人知道它属于谁、该是什么状态**的容器——它既不会被任何记录引用，也不会被任何代码删除。

### 原理

意图先行（intent-first）的含义是：**先把"我打算做 X"持久化，再去做 X**。它把崩溃后可能的世界压缩成一张 2×2 的表：意图存在与否 × 对象存在与否。四种组合里，三种可以纯靠本地信息裁决（意图不在 → 当没发生过；意图在、对象不在 → 判失败；意图在、对象在 → 判成功），只有"意图在、对象**未知**"这一格不查询就无法判定，而它恰好可以通过一次 `Inspect` 变成已知。可判定的前提是**意图先于动作落盘**，而不是先动作后记账。

### 本项目怎么做

`internal/service/service.go:92` 的注释把顺序和它的收益写在了一起：

```go
// CreateSandbox admits the request against governance, records the intent
// (state=provisioning), then realizes it on the backend. A crash anywhere
// in between leaves a transitional record that the reconciler converges —
// intent-first persistence is what makes crash recovery decidable.
```

落盘在 `internal/service/service.go:147` 的 `s.St.PutSandbox(sb)`（此时状态是 `provisioning`，见 `internal/domain/sandbox.go:257`），真正触碰后端是下一行的 `go s.provision(sb.ID, imageRef)`（`:151`）。`provision` 内部还有一层同样方向的细节（`internal/service/service.go:316`）：容器建成功后，先 `UpdateSandbox` 把后端引用写回记录，写回失败就立刻 `Remove` 掉刚建好的对象（`:320`）——**不让任何对象处于"已存在但无人知晓"的状态**。

### 替代方案

第一种是后落盘：如上所述会产出不可回收的孤儿。第二种是把意图写进独立的任务队列表并靠后台消费：多一份需要恢复的状态，而且队列项本身也有"已取出未执行"的中间态，问题只是被搬了个地方。第三种是不记意图、靠后端对象上的标签反推归属：这要求对象一定建成，而"创建请求发出后进程就死了"正是对象可能不存在的情况，反推无据。

### 怎么验证

```bash
go test ./tests/ -run 'TestCrashRecovery/a_sigkill_during_create_converges_instead_of_hanging' -v
```

预期通过：请求返回 201 后立刻杀进程、重启，沙箱在 60 秒内落到 `running`/`failed`/`stopped` 之一，绝不停在 `provisioning`。若把 `internal/service/service.go:147` 的落盘挪到 `:151` 之后，这个测试会失败——或者更糟，留下一个无人认领的容器，`TestNoBackendLeaks`（`tests/resilience_test.go:171`）随后也会失败。

## 10.4 启动对账与周期对账：一个参数，两种时间语境

### 问题

同一个 `reconcile` 函数被两个入口调用，差别只有 `atBoot` 一个布尔值。为什么要区分？因为在运行中的进程里，一条 `provisioning` 记录**可能完全正常**——它刚被创建，工人 goroutine 还没轮到；而在刚重启的进程里，同样的记录意味着**一定被打断了**。用同一套宽限规则处理这两种语境，必然有一边是错的。

### 原理

"在途"是一个**时间语境概念**，不是一个记录字段。运行中，"在途"由活着的 goroutine 定义；重启后，进程地址空间已经清零，没有任何东西还在途。所以宽限期是发给"正常的慢"的豁免，它对"崩溃"这个语境根本不适用。把两种语境分开，本质上是承认：**同一份数据在不同时间背景下有不同的语义**。

### 本项目怎么做

两个入口在 `internal/service/reconciler.go:118` 与 `:123`：

```go
func (s *Service) Reconcile(ctx context.Context) { s.reconcile(ctx, false) }

// ReconcileAtBoot is the startup pass. After a process death nothing is
// "in flight" anymore: the usual provisioning grace period does not apply,
// so every unconverged record is repaired immediately.
func (s *Service) ReconcileAtBoot(ctx context.Context) { s.reconcile(ctx, true) }
```

周期对账给的宽限是 5 分钟（`internal/service/reconciler.go:149`）：

```go
if !atBoot && sb.State == domain.StateProvisioning && time.Since(sb.CreatedAt) < 5*time.Minute {
	continue
}
```

`atBoot` 为真时这个豁免被取消，所有未收敛的记录立刻修。逐条判定的四种结局（同一个循环，`:141` 起）：

```go
	if sb.Backend.Ref == "" {
		if !atBoot && sb.State == domain.StateProvisioning && time.Since(sb.CreatedAt) < 5*time.Minute {
			continue
		}
		s.failSandbox(sb.ID, "provisioning interrupted before backend object existed")
		fixed++
		continue
	}
```

没有后端引用 → 意图落了盘、创建请求没发出去 → 直接判失败，**不需要查后端**。有引用则 `Inspect`（`:156`）：

```go
	st, err := s.Backend.Inspect(ctx, sb.Backend.Ref)
	if err != nil {
		if apperr.From(err).Code == apperr.NotFound {
			s.failSandbox(sb.ID, "backend object disappeared (reconciled)")
			fixed++
		}
		continue // transient backend errors: try next tick
	}
```

三种处置：错误码是 `NotFound` → 以后端为准判失败（用错误码判断而不是字符串匹配，错误模型见 §2）；**其它错误一律 `continue`，留给下一个 tick**——区分"对象不存在"与"我现在查不了"是这段最容易写错的地方。查询成功则比对（`:164`）：

```go
	truth := backendToState(st)
	if truth == sb.State {
		continue
	}
	_, err = s.St.UpdateSandbox(sb.ID, -1, func(x *domain.Sandbox) error {
		x.State = truth // reconciler sets truth directly, state machine governs client paths
```

`backendToState`（`:287`）只返回 `paused`/`running`/`stopped` 三种之一，也就是说后端能表达的真相粒度比记录的状态集更粗，收敛永远不会凭空造出一个过渡态。同时 `st.OOMKilled` 为真时把原因写成 `"killed by memory limiter (reconciled)"`（`:171`），外部停止写成 `"stopped externally (reconciled)"`（`:173`）——对租户而言"自己退出"与"被内存上限杀掉"是两件事（配额见 §11.5）。

### 替代方案

统一成一种策略只有两种可能，且都是错的：处处给宽限 → 启动后一条卡住的 `provisioning` 要等 5 分钟才被修，期间客户端读到的是一个谎言；处处不给宽限 → 周期对账会误杀正在排队等信号量的正常创建。另一个想法是把"我拿了信号量"持久化下来，让重启后能分辨谁真在途：这引入了一份新的待恢复状态，而且它同样在进程死后不可信——真正在途的证据只存在于活着的进程里。

### 怎么验证

```bash
go test ./tests/ -run TestCrashRecovery -v -count=1
```

预期三个子测试全过。反向验证：把 `internal/service/reconciler.go:149` 的 `!atBoot &&` 删掉（让启动对账也给 5 分钟宽限），第一个子测试会失败——重启后记录停在 `provisioning` 超过 60 秒，断言 `neq(sb.State, StateProvisioning)` 不通过。这个实验比读十遍注释更能说明 `atBoot` 存在的意义。

## 10.5 三态划分：过渡态、稳定态、终态的判据

### 问题

8 个状态如果只按"活/死"二分会出事：一条 `provisioning` 记录和一条 `running` 记录都"不是死的"，但它们需要的处置完全不同——前者必须去后端查，后者只需要周期性核对。分类的判据若不写进代码，恢复逻辑就会退化成"对所有非终态做同一件事"，然后在某个状态上做错。

### 原理

正确的判据不是"沙箱是否可用"，而是**这条记录是否需要被外部真相修正**，以及**修正的紧迫性**：

- **过渡态**：记录描述的是一个**未完成的动作**（正在建、正在暂停、正在恢复、正在停止）。崩溃后它可能是半途而废的，必须立刻向后端重新推导。
- **稳定态**：记录描述的是一个**已完成的稳定事实**（运行中、已暂停）。它自身不是断裂的，但后端仍可能因外部事件变化（OOM、被人 `docker stop`），所以它仍需被周期性核对，只是不急。
- **终态**：不再需要任何对账。

注意两件事：`paused` 既不在过渡态集合里也不在终态集合里；`failed` 是终态，但它在转移表里仍有一条出边。所以"终态"定义的是**收敛义务的终止**，不是"不能再跃迁"。

### 本项目怎么做

分类写在 `internal/domain/sandbox.go:49` 与 `:55`（状态集合在 `:24`，转移表在 `:37`）：

```go
// Terminal reports whether s needs no further reconciliation.
func (s SandboxState) Terminal() bool {
	return s == StateStopped || s == StateFailed
}

// Transitional reports whether s means an operation was in flight; the
// reconciler must re-derive these from the backend on startup.
func (s SandboxState) Transitional() bool {
	switch s {
	case StateProvisioning, StatePausing, StateResuming, StateStopping:
		return true
	}
	return false
}
```

收敛器用 `Terminal()` 做唯一的跳过条件（`internal/service/reconciler.go:142`），也就是说稳定态同样会被 `Inspect`——这正是"OOM 被杀的 `running` 沙箱能被发现"的原因。`failed` 的出边是 `StateFailed: {StateStopping}`（`internal/domain/sandbox.go:45`），注释写着 `// cleanup path`：终了之后仍要走删除清理，但那不再是对账。

反向的一侧也受这条划分保护（`internal/service/service.go:340`）：

```go
	_, err := s.St.UpdateSandbox(sbID, -1, func(x *domain.Sandbox) error {
		if x.State.Terminal() {
			return nil // someone else converged it; keep their truth
		}
```

`failSandbox` 在终态上直接返回：别人已经给了裁决，就不要用后到的失败去覆盖它。**收敛是单向的，一旦落到终态就不再改写**。

补充一点：同一批状态在另一处被重新划分过——`CountableResources`（`internal/domain/sandbox.go:279`）把过渡态也算作占用。所以一条卡住的过渡态记录不只是"显示错了"，它真的占着资源，这也解释了为什么过渡态必须尽快收敛。

### 替代方案

二分（活/死）无法区分"正在建"与"已建好"，崩溃后只能靠猜。另一种做法是给过渡态挂一个超时时间戳而不设为独立状态：时钟不可靠（挂钟回拨、长时间 GC 停顿），而且它无法表达"卡在哪一步"——`pausing` 和 `resuming` 需要不同的回滚动作，一个时间戳表达不了。还有一种是把过渡态直接做成"操作日志"而非状态：那等于多一份要恢复的状态，而状态字段的好处是它和记录同生同死、一次读取就能分类。

### 怎么验证

```bash
go test ./internal/domain/ -run 'TestTransitionalStatesCoverAllInFlightOps|TestStateMachineIllegalTransitions' -v
```

`TestTransitionalStatesCoverAllInFlightOps`（`internal/domain/domain_test.go:59`）遍历全部 8 个状态，断言除 `running`/`paused` 外每个状态都**恰好属于**终态与过渡态之一——注释点明意图：一个既非终态又非过渡态的状态会被收敛器静默跳过。`TestStateMachineIllegalTransitions`（`:42`）钉住 `{StateStopped, StateRunning}`（`resurrection forbidden`）与 `{StateRunning, StatePaused}`（必须经 `pausing`）等非法边。

## 10.6 孤儿镜像：一个只有收敛器能删的东西

### 问题

快照是把沙箱文件系统 `Commit()` 成镜像（见 §7、§8），这些镜像**带标签**（`sandboxd-snap-<id>`）。而 `docker image prune` 只删悬空（无标签）镜像。于是一旦数据目录丢失、从提交前的备份恢复、或节点被重建，这些镜像就会没有记录指向它们，永久留在盘上——每一份都是**一个完整的沙箱文件系统副本**。

### 原理

删除逻辑的正确性问题从来不是"怎么删"，而是"**哪些东西绝对不能删**"。镜像这个场景把不对称性放大到了极致：漏删一个的代价是磁盘慢慢变满（可观测、可补救），错删一个正在被活沙箱使用的镜像的代价是**正在运行的工作负载当场崩掉**（不可补救）。所以保护集合必须按"可能被谁引用"来构造，而不是按"我在管哪些记录"来构造。

### 本项目怎么做

原注释把两个方向都讲了（`internal/service/reconciler.go:230` 起），先说为什么非管不可：

```go
// These images are tagged (sandboxd-snap-<id>), so they are not *dangling* and
// no engine GC will ever remove them. If the data directory is lost, restored
// from before a commit, or simply replaced when a node is rebuilt, the images
// survive with nothing pointing at them, and each one is a full copy of a
// sandbox filesystem. On the machine this was written on, ninety such images
// had accumulated. The check below is the only thing that ever deletes them.
```

再说危险方向（`internal/service/reconciler.go:239`）：从快照创建的沙箱仍然持有该快照的镜像引用，所以保护集合不能只算快照记录。实现里保护三类（`internal/service/reconciler.go:252-268`）：快照记录、模板记录、以及**每一条沙箱记录**的 `Backend.ImageRef`。第三条连 `stopped` 的沙箱也保护，注释给的理由：

```go
		// A stopped sandbox can never be resumed (stop is terminal), but its
		// record is still an operator-visible thing; the conservative and
		// cheap choice is to protect every recorded image and let the record
		// deletion be the event that frees it.
```

枚举也有约束（`internal/runtime/oci/backend.go:850`）：只按 `sandboxd-snap-` 与 `sandboxd-tpl-` 两个前缀过滤，注释写明一台节点上可能有几千个无关镜像，全量枚举等于每次对账给自己制造一次负载尖峰。

### 替代方案

交给引擎 GC：带标签的镜像永远不会被 GC，此路不通。改成生成时不打标签：那就要自己维护"标签到镜像"的映射，且 `docker image prune` 的语义随引擎版本变化，不可依赖。定期全量清理（按时间删老镜像）：时间不是引用关系的证据，一个跑了三个月的长任务快照会被误删。

### 怎么验证

```bash
go test ./tests/ -run TestOrphanedSnapshotImagesAreReaped -v
```

`tests/resilience_test.go:230`：先建快照并断言镜像存在，然后杀进程、`os.RemoveAll` 掉数据目录、重建空目录、重启（`:246-253`），断言该镜像在 90 秒内消失。它刻意**不通过 API 删除快照**——记录是以灾难的方式消失的，这样才真的测到"没有记录指向"这条路径。真实规模见 `README.md:180`：回收前 89 个，回收后 0 个。

## 10.7 乐观锁语义：`BaseVersion`、409 与客户端该怎么做

### 问题

两个客户端几乎同时读到同一个沙箱，都决定改它：甲暂停，乙续期。若无保护，两人各自基于读到的副本写回，后写的覆盖前写的，甲的改动无声消失。更麻烦的是客户端怎么知道该不该重试——HTTP 409 本身不回答这个问题。

### 原理

乐观锁的思路是：读取时记下"我看到的版本"，写回时要求"版本没变过"。版本变了说明有人插进来，此时**你的决策前提已过期**，正确的动作不是硬写，而是重新读取、重新决策。存储层把它实现成持锁临界区内的比对（见 §9.7），本节只讲它对外暴露的语义。

为什么基线是"读到的版本"而不是"版本号 +1"？因为后者强制"内存改一次就必须落盘一次"，而现实里你会想先改三个字段再一次性保存。基线是读到的版本时，写入方不需要知道当前版本，只需要知道自己基于什么做的决策。

### 本项目怎么做

冲突的产生点在 `internal/domain/sandbox.go:80`：

```go
func (sb *Sandbox) Transition(to SandboxState) error {
	if !to.Valid() {
		return apperr.InternalErr("unknown target state %q", to)
	}
	if !sb.State.CanTransition(to) {
		return apperr.ConflictErr("cannot transition sandbox from %s to %s", sb.State, to)
	}
	sb.State = to
	return nil
}
```

错误消息里带上**当前状态**（`internal/domain/sandbox.go:85`），因为客户端收到 409 时第一个问题一定是"那它现在到底是什么状态"。映射成 HTTP 409 与错误包络见 §12。

对称的一面是：收敛路径传的基线是 `-1`（`internal/service/reconciler.go:168`、`internal/service/service.go:340`），即跳过比对。为什么它不需要乐观锁？因为它不基于"某个客户端读到的旧值"做决策，而是基于**刚刚从后端读回来的真相**，而这条真相是由单个 goroutine 在持锁临界区内写下的。需要 CAS 的是"拿着过期前提还想写"的客户端路径；不需要的是"此刻才知道真相"的收敛路径。

### 替代方案

悲观锁（阻塞等待）在单机内存场景下会把并发串行化，且无法处理"客户端拿到响应后隔了十分钟才回来"这种跨请求的时间跨度。last-write-wins（直接覆盖）就是丢失更新本身。还有一种看起来合理的做法：让客户端对 409 自动重试 N 次——这是错的，因为 409 有两种成因：别人抢先改了（重试有意义），或者**状态已经变了**（比如你要暂停的沙箱已被回收，重试只会再拿一次 409）。本项目把 `retryable` 做成显式字段而不是让客户端猜状态码（§12），就是为了让这两类能被区分。

### 怎么验证

```bash
go test ./internal/store/ -run TestCASMechanics -v
```

覆盖三条路径：过期基线被拒绝、`-1` 能禁用比对、正常更新让 `Version` 递增（`internal/store/store_test.go:83`，机制见 §9.7）。并发侧由 `go test ./internal/store/ -run TestConcurrentCASExactlyOneWinner -race` 压：16 个 goroutine 拿同一基线抢更新，断言恰好一个赢。

## 10.8 并发闸与背压：在途创建数被封顶意味着什么

### 问题

限流管每秒多少次（见 §11.3），配额管同时占多少（见 §11.5）。还剩下第三种压力：**同时有多少个创建正在实施中**。它既不是速率也不是总量，而是"这台机器上同时开工的创建动作数"——拉镜像、起容器、等启动，每一项都是真开销。不约束它，一次突发会把引擎调用队列撑爆，而所有请求会一起变慢。

### 原理

并发闸是一个容量为 N 的全局信号量：拿不到名额就排队，排队也有上限，超时则**显式失败**。它的关键性质是**它限制的是实施数，不是准入数**——请求已经通过治理检查、已经落盘成 `provisioning` 记录，被闸挡住的只是"开始动手"这一步。

对本章而言它还有第二重含义，也是它出现在这一章的原因：**闸的容量 N 就是"在任何时刻处于过渡态、因而崩溃时需要被裁决的创建记录数"的上界**。安全契约把这一点写明了（`docs/SECURITY-CONTRACT.md:126`）：越限幅度被在途创建数封顶，而在途创建数受创建速率与 `max_concurrent_creates` 双重约束。换句话说，并发闸不只保护引擎，它还**给恢复路径的规模定了上界**——启动时最多有 N 条"意图在、对象未知"的记录需要 `Inspect`。

### 本项目怎么做

信号量是带缓冲的 channel（`internal/service/service.go:51`：`createSem: make(chan struct{}, cfg.MaxConcurrentCreates)`），默认容量 8、排队上限 15 秒（`internal/config/config.go:218`）。`provision` 的入口（`internal/service/service.go:283`）：

```go
func (s *Service) provision(sbID, imageRef string) {
	select {
	case s.createSem <- struct{}{}:
	case <-time.After(s.Cfg.CreateQueueTimeout):
		s.failSandbox(sbID, "create queue timeout — node is saturated")
		return
	}
	defer func() { <-s.createSem }()
```

三件事：拿不到名额就等，等到超时就把这条已落盘的记录**判失败并写明原因**（而不是无限等待或让它永远挂着）；`defer` 保证名额一定归还；失败原因写进记录，客户端读到的是 `state=failed` 加一句可读的解释，而不是超时后无响应。用 channel 而不是计数器加锁，是为了让"等待"和"超时"能直接写进同一个 `select`（channel 用法见 §3）。

### 替代方案

拒绝而非排队（满了直接 402/503）：省掉等待队列，代价是突发时把本可以在几百毫秒内完成的创建变成失败，而"排队一会儿就成功"对调用方更友好。反过来无限排队：看起来温和，实际是把背压转换成不断增长的延迟，调用方最终仍会超时，而且期间那些记录一直挂在 `provisioning` 上、一直占着记账。按租户分闸而不是全局：创建消耗的是这台机器的资源，与请求来自谁无关，分闸只是把上限切成若干份，机器该忙还是忙。

### 怎么验证

```bash
go test ./tests/ -run TestQuotaAdmissionIsBestEffortButConverges -v
```

`tests/concurrency_test.go:169`：20 路并发创建对上限 5，断言无 5xx、记录数等于准入数（无幽灵记录）、突发结束后下一次创建被 402 拒绝。测试注释（`:205`）直接点出"越限幅度被在途创建数封顶，在途数受创建速率与 `max_concurrent_creates` 双重约束"——这正是本节要的那条性质。

## 10.9 回收是治理事件，不是错误

### 问题

系统里"沙箱消失"有两种成因：出错（创建失败、被内存上限杀掉、外部停止）与**到期**（TTL 到、空闲超时到）。如果两者在日志和审计里没有区别，运维就无法回答"平台是在出故障，还是在正常回收"，于是每一次回收都会被当成一次故障去看。

### 原理

定性决定归属：到期回收是**系统主动执行的一条策略**，不是一次失败。把它记成错误，等于把"按预期工作"和"偏离预期"混进同一个信号通道，信噪比随规模下降。区分的方法不是往一个事件里塞 `reason` 字符串，而是**用不同的事件类型**——类型可以被过滤、被计数、被告警规则单独匹配，字符串不能。

### 本项目怎么做

`internal/service/reconciler.go:65` 的注释给了定性：

```go
// expiry is a governance event, not an error, and is audited as such
```

两种到期被分成两个动作（`internal/service/reconciler.go:103`）：

```go
		if err == nil {
			action := domain.ActSandboxExpire
			detail := map[string]string{"ttl_seconds": itoa(sb.Spec.TimeoutSeconds)}
			if d.idle {
				action = domain.ActSandboxIdleExpire
				detail = map[string]string{"idle_timeout_seconds": itoa(sb.Spec.IdleTimeoutSeconds)}
			}
```

一个是 `ActSandboxExpire`，一个是 `ActSandboxIdleExpire`，detail 字段也不同（TTL 秒数 vs 空闲超时秒数），审计流可以按类型过滤（审计设计见 §11.6）。两条时钟为什么并存见 §8.9。

回收动作本身仍然走状态机（`internal/service/reconciler.go:88-99`）：`running → stopping → stopped`，并且在调 `Backend.Stop` **之前**先落一次 `stopping`。这又是一次意图先行：若先 Stop 再落盘，中间崩溃的话你既不知道它是自己退出的、还是被回收的。

### 替代方案

统一成一个 `ActSandboxStop` 加 `reason` 字段：少一个类型，代价是"今天有多少是因为空闲"这个问题必须做字符串匹配才能回答，而字符串是可以被改写、被本地化的。另一种是把到期也报成错误码（比如给客户端返回 410）：客户端并没有发起这个操作，用响应码表达一个它没有参与的事件没有意义——它是审计事件，不是请求结果。

### 怎么验证

```bash
go test ./tests/ -run TestElasticity -v
```

`tests/commercial_test.go:712` 与 `:783` 两条分别断言空闲预算 60 秒的沙箱被回收、同期被使用的 twin 存活，以及 TTL 到期后**配额真的被归还**（随后新建成功）。关键在于它们断言的是"资源真的空出来了"，不是响应里有个字段（判定方法见 §14）。

## 10.10 孤儿对象：后端比记录多、或少的时候

### 问题

理想情况下记录与后端对象一一对应。崩溃会制造两个方向的偏离：一是**后端对象比记录多**（记录还没写就被删了、或创建成功后写回失败），二是**记录比后端多**（记录还在，对象已被外部删除）。前者占着机器资源没人认领，后者让平台承诺一个不存在的沙箱。两个方向都要处理，而且处理动作落在不同的一侧。

### 原理

"我没有记录"这件事**不能逐条判断**。"孤儿"是一个集合概念：一个后端对象是孤儿，当且仅当它不在所有已知引用的全集里。若对每个对象去查一次存储，不仅慢，还会漏掉"两条记录引用同一对象"这类情形。所以正确顺序是：先构造引用全集，再遍历后端对象做差集。

方向性同样重要：多出来的对象要**删对象**（动后端），缺失的对象要**改记录**（动存储）。判断依据只能是"哪一侧是真相"——后端说它存在而记录没有，说明记录侧丢失了归属，删对象；记录说有而后端说没有，说明对象已消失，改记录。

### 本项目怎么做

全集在遍历前一次性建好（`internal/service/reconciler.go:134`），随后 `s.Backend.Orphans(ctx)` 拉取带本产品标签的全部容器（`internal/runtime/oci/backend.go:812`，标签过滤由引擎侧完成，含已停止的容器），逐条比对。三类残骸各有判定：

预热池对象按名字前缀认（`internal/service/reconciler.go:193`），且只有当内存中的池**已经不追踪它**时才删：

```go
		if isWarmName(o.Ref) {
			// pool inventory without a store record: live only if the
			// in-memory pool still tracks it. A crashed daemon's leftovers
			// are pristine (never exec'd) and safe to reap unconditionally.
			if !s.isLivePoolItem(o.Ref) {
				_ = s.Backend.Remove(ctx, o.Ref)
				reaped++
			}
			continue
		}
```

`isLivePoolItem`（`internal/service/pool.go:57`）在同一把锁下查询，避免回收一个正在被领养的池对象（预热池语义见 §8.8）。能"无条件回收"的安全论证是：**崩溃守护进程留下的预热容器从未执行过任何工作负载**。

模板组合的临时容器（`:203`）同样按名字认（`sandcastle-compose-`），残留意味着守护进程死在组合中途（组合语义见 §8）。

第三种靠标签归因（`:210`）：

```go
		sid := o.Labels["sandbox.id"]
		if sid == "" {
			continue // ours by product label but not attributable; leave for ops
		}
		// a sandbox-labeled object with no record: crash residue from a
		// delete whose record removal succeeded first — reap it
		if _, ok := s.St.GetSandbox(sid); !ok {
			_ = s.Backend.Remove(ctx, o.Ref)
			reaped++
		}
```

`sid == ""` 时选择"留给运维"：它虽打着本产品标签，却**无法归因到任何租户**。这是"宁可不删也不错删"——删错一个正在服务的沙箱，代价远大于漏掉一个孤儿。

反方向（记录比对象多）没有单独的循环，它就落在 10.4 的 `Inspect` 返回 `NotFound` 那条分支上：判失败、写审计。

### 替代方案

按名字/标签之外，另一条路是让后端对象自己带 TTL（创建时写入到期时间，靠外部定时清理）：这要求引擎侧有清理机制，且 TTL 必须比最长任务长，等于把"该不该删"的决策从"有没有记录"退化为"活了多久"。还有一种是无条件清理所有带本产品标签的容器：简单，但会删掉刚刚创建、记录还没落盘的沙箱——那正是 `isLivePoolItem` 与 `knownRefs` 存在的理由。

### 怎么验证

```bash
go test ./tests/ -run 'TestNoBackendLeaks|TestCrashRecovery' -v
```

`TestNoBackendLeaks`（`tests/resilience_test.go:171`）建 6 个沙箱、逐个删除，断言托管容器数回到基线（`:192`）——注释点明：记录消失了而容器还在跑，就是那种最终会填满一台节点的泄漏。孤儿侧的发现能力由 `TestOrphansFindsLabeledContainers`（`internal/runtime/oci/oci_test.go:431`）在单元层覆盖。

## 10.11 四个后台循环：为什么周期不同

### 问题

启动之后有四个循环在跑，它们的结构几乎一样（ticker + 退出通道，见 §3），但周期从 5 秒到 10 分钟不等。周期不是随手填的：它决定了**不一致窗口的最大长度**——一个沙箱被外部停止后，最长多久才会在记录里反映出来。周期还决定了给引擎的调用压力。

### 原理

一个循环该多久跑一次，取决于两件事：**它读的是本地信息还是远端真相**，以及**延迟的代价是什么**。读本地时钟的循环可以很频繁，因为它不产生外部调用；每跑一次就要对 N 个对象各发一次 `Inspect` 的循环，单位时间的引擎调用数是「记录数 ÷ 周期」，周期越短，成本随规模线性放大。

代价那一边同样要算：清扫延迟的代价是资源晚一点还回来（利用率问题），对账延迟的代价是客户端读到的状态与现实不符（信任问题）。两者都不是零，所以两个周期都不是越小越好。

### 本项目怎么做

四个循环由 `internal/service/service.go:60` 拉起：

```go
func (s *Service) StartLoops() {
	go s.sweepLoop()
	go s.reconcileLoop()
	go s.compactLoop()
	if s.Cfg.PoolEnabled && s.Cfg.PoolSize > 0 {
		go s.poolLoop()
	}
}
```

周期来自配置（`internal/config/config.go:228`）：`SweepEvery: 30 * time.Second`、`ReconcileEvery: 60 * time.Second`、`CompactEvery: 10 * time.Minute`，预热池补货循环固定 5 秒（`internal/service/pool.go:71`）。职责划分是互斥的：

- **补货**（5 秒）：只查本地池库存、只补差额，不参与一致性。周期最短，因为它只影响命中率，不影响正确性。
- **清扫**（30 秒）：只执行策略。`sweepExpired` 的判定纯是本地时间比较（`sb.Expired(now)`、`sb.IdleExpired(now)`，`internal/service/reconciler.go:77`），不查后端。
- **对账**（60 秒）：只读取真相，处理的是外部事件造成的漂移。它的代价是每条非终态记录一次 `Inspect`——端到端测试的注释直接点明了这一点（`tests/commercial_test.go:277`）。
- **压实**（10 分钟）：只减日志体积，不改任何语义（见 §9.6）。周期最长，因为它唯一的作用是控制启动重放的时长。

每次对账还自带 5 分钟超时（`internal/service/reconciler.go:26`）：一次卡住的对账不能把整个后台循环堵死。于是单次漂移的最长修复时间是「周期 + 单次耗时」的上界，而不是无限。

### 替代方案

统一成一个周期（比如全部 30 秒）：省事，代价是压实的写放大与对账的引擎调用都被拉到同一个频率上，其中至少一个会成为自伤。反过来按事件驱动（引擎推送容器事件、有变化才对账）：延迟最小，但推送通道本身会丢事件（引擎重启期间的事件无人接收），丢了就没有第二机会——**周期轮询的价值不在于快，在于它每次都是一个完整的重新推导**，不依赖上一个事件有没有收到。

### 怎么验证

```bash
go test ./tests/ -run TestStartupIsMeasuredNotJustTimedOut -v
```

`tests/commercial_test.go:230`：冷启动计时、ready 后首个业务请求必须真的建成、带 9 个存活沙箱重启后全部仍 `running` 且后端对象数不变（`:288`）。这条同时验证了启动对账的**成本形状**——注释（`:274`）说明断言的是"重启代价由固定工作决定、不随 fleet 规模增长"，而不是一个绝对毫秒数。`README.md:201` 记录的实测是 166 ms。

---

### 本章检验

1. `reconcile` 里有一句 `x.State = truth // reconciler sets truth directly, state machine governs client paths`。请说明：如果收敛器改用 `Transition()` 走状态机，哪一类崩溃后的状态将无法被修复？构造一个具体例子，并说明它与 10.5 讲的三态划分有什么关系。
2. `failed` 在 `Terminal()` 里返回真（不再需要对账），但转移表里仍有 `StateFailed → StateStopping` 这条边。请解释这两个事实为什么可以同时成立，并说明如果把 `failed` 从 `Terminal()` 里去掉会对收敛器产生什么后果。
3. 周期对账给 `provisioning` 5 分钟宽限，启动对账不给。假设某人提议"改成在记录里持久化一个 `picked_up_at` 时间戳，重启后据此判断谁真在途"，请指出这个方案在语义上为什么仍然无法取代 `atBoot`。
4. 并发闸的容量是全局的 8。请论证：这个数字为什么同时是"崩溃启动时最多有多少条创建记录需要 `Inspect` 裁决"的上界？如果把 `provision` 里的 `defer func() { <-s.createSem }()` 改成在函数末尾显式释放（去掉 defer），这个上界会怎样失效？
5. 孤儿回收里，预热池对象可以"无条件回收"，带 `sandbox.id` 标签但 `sid == ""` 的对象却要"留给运维"。这两个判断的安全论证分别是什么？并说明：为什么孤儿判定必须先构造 `knownRefs` 全集，逐条查询存储会漏掉什么情形？
6. **动手任务**：读 `internal/service/reconciler.go:141-183`，把四条判定路径（无后端引用 / `NotFound` / 状态不一致 / 瞬时错误）分别映射到 10.5 的三类状态上，指出哪一类状态会走哪几条路径。然后做两个改动并各跑一次 `go test ./tests/ -run TestCrashRecovery -v`：(a) 删掉 `:149` 的 `!atBoot &&`；(b) 把 `:162` 的 `continue` 改成 `s.failSandbox(sb.ID, "backend unreachable")`。记录两次分别坏在哪个子测试、失败信息是什么，并解释 (b) 的改动会在什么真实场景下造成数据损失。


# §11 多租户治理：认证、限流、配额、审计

## 11.1 治理的四件事，以及准入为什么跑在业务逻辑之前

### 问题

两个互不信任的调用方共用一台机器。某个脚本写成了死循环，一秒发几百次创建请求。没有准入层时，这些请求会完整走到后端：解析镜像、创建容器、挂可写层、配控制组。等资源耗尽时，已经成活的那批仍占着 CPU 和内存，而失败发生的那一层未必有能力把它们清掉——**拒绝来得太晚，它的代价就不再是零**。所以要解决的是让拒绝发生在动作之前，且被拒绝的请求不留痕迹。

### 原理

多租户治理要回答四件事，作用域完全不同："你是谁"每请求一次；"你能多快"每调用一次；"你能占多少"跨越租户的整个生命周期；"你干了什么"是永久的。

排序也不是随意的。"你是谁"必须排第一，因为后三件事都要拿租户做键。"你能多快"与"你能占多少"必须排在**昂贵动作之前**——昂贵动作的定义就是"做了一半失败需要收尾"，否认一份已经开工的工作，成本是做一遍加清一遍，而清理路径自己也会失败。唯一必须事后追加的是审计：它记录的结果只有动作确定之后才存在。

### 本项目怎么做

包注释把定位写在第一行（`internal/gov/gov.go:1`）：

```go
// Package gov implements multi-tenant governance: authentication, rate
// limiting, quota accounting, and audit. The service layer composes these
// before any business logic runs.
```

组合动作本身在 `internal/service/service.go:23`：四个治理组件作为字段挂在 `Service` 上，由构造函数一次装好（`service.go:45`），由关闭流程停掉（`service.go:74`）。这样是为了消除一种失效模式——"新增了一个端点却忘了接治理"；集中组合之后，新端点拿到的就是已组合好的 `Service`。

拒绝到底发生在哪一步，看 `CreateSandbox` 的顺序（`service.go:96`）：

- 第一行就挡掉被停用的租户（`service.go:97`）；
- 解析镜像、校验规格、拒绝磁盘配额请求、校验出口策略（`service.go:101` 到 `service.go:117`）；
- 然后才是准入检查（`service.go:118`）；
- 直到 `service.go:122` 才产生沙箱记录，`service.go:151` 才真正去碰后端。

也就是说，一次因为配额被拒的创建**连记录都不会产生**。

### 替代方案

第一个做法：让每个 HTTP 处理器在自己开头调用检查。它能工作，但把"别忘了接"变成一条口头纪律；漏掉一个端点，那个端点就成了没有配额的入口。集中组合把这种失效变成"构造时少填一个字段"。

第二个做法：先做，发现超额再回滚。代价是双倍后端调用，加上一条自己也会失败的清理路径；进程在两者之间死掉，就留下没人认领的残骸（见 §10）。放到进程外的反向代理也不行：代理拿不到租户配额表，做不了按租户的判断。

### 怎么验证

```bash
cd castle
go test ./tests/ -run 'TestQuotaAndAdmission' -v -count=1
```

预期四个子用例通过。`sandbox_ceiling_binds_and_frees_on_delete`（`tests/governance_test.go:26`）断言了本节最在意的两件事：第三次创建得到 402，删掉其中一个之后额度立刻可用。

## 11.2 租户模型：配额挂在谁身上

### 问题

某个租户给 CI 流水线每次构建都铸一把新 Key。如果配额挂在 Key 上，这条流水线就获得了"每换一把 Key 就多一份额度"的无上限配额；吊销某把 Key 之后，它名下的沙箱变成没人能列举、没人能删除的孤儿。两个后果来自同一个错误：**把配额挂在了一个可自由增生的实体上**。

### 原理

两类实体的职责必须分开。**凭据**证明"这一次调用是谁发起的"，可以随时替换；**主体**持有资源与配额，生命周期远长于任何一把凭据。方向是凭据指向主体而不是反过来，轮换因此不触碰任何沙箱。

推论是：配额的归属键只能是主体 ID。任何能被调用方低成本造出很多个的东西都不合格。

### 本项目怎么做

`domain.Tenant`（`internal/domain/tenant.go:77`）同时持有 `Quota` 与 `Status`。配额是一个**内嵌的值结构**（`tenant.go:81`），随租户一起序列化落盘，不存在第二张配额表；六个字段在 `tenant.go:38`。边界由 `Validate`（`tenant.go:47`）钉死：并发沙箱数上限 10 000（`tenant.go:48`）；CPU 与内存各不超过平台单沙箱上限的 100 倍（`tenant.go:51`、`tenant.go:54`，上限类型见 `internal/domain/sandbox.go:144`）；速率所在的组必须是已知六组之一（`tenant.go:58`），每秒数与突发值落在有限区间内（`tenant.go:61`）。租户状态只有两个取值（`tenant.go:12`）：active 与 suspended。停用只影响"能不能新增"，它在 `CreateSandbox` 第一行被检查（`service.go:97`），**不动任何已有对象**。宁可让存量负载自然到期，也不一笔勾销。

Key 与租户的关系写在 `domain.APIKey`（`tenant.go:120`）：`TenantID` 为空只属于平台管理员 Key（注释在 `tenant.go:124`），`Valid`（`tenant.go:148`）据此要求租户级 Key 必须指向某个租户（`tenant.go:157`）。

### 替代方案

合并成一层（一个 Key 就是一个租户）：轮换凭据势必要迁移资源归属，否则就是把沙箱变成孤儿。加到三层（组织 → 租户 → Key）：当前无对应产品形态。把配额抽成独立一张表更灵活，但引入了"配额与其归属者可能不一致"的窗口；嵌进租户对象之后，两件事共用同一把互斥保护（原语用法见 §3）和同一个版本号。

### 怎么验证

```bash
cd castle
go test ./internal/domain/ -run 'TestTenantQuotaValidation|TestAPIKeyValidity' -v -count=1
```

预期两者通过。前者（`internal/domain/domain_test.go:175`）断言凭空造出一个操作组名会被拒；后者（`domain_test.go:147`）把四种失效形态分开验收：过期、吊销、租户级 Key 缺租户名、管理员 Key 缺租户名仍然有效。端到端那一侧是 `TestQuotaAndAdmission/quotas_are_per_tenant`（`tests/governance_test.go:40`）。

## 11.3 API Key 的铸造、哈希存储与常数时间比对

### 问题

最省事的实现是把明文 Key 存进表，认证时按明文查。问题是泄漏半径：一次备份外泄，所有租户的凭据同时作废。

可一旦改成"只存哈希"，立刻出现查找难题：认证时你手里只有明文，库里只有哈希——**两边都没有可以做匹配键的东西**。第三个问题更隐蔽：即使存了哈希，比对方式不当也会让攻击者把它逐字节测出来。

### 原理

第一，只存哈希。哈希是单向的，一次存储泄漏不会直接给出可用凭据。关键判断是**可以用快速哈希**：明文是密码学随机数发生器产出的全熵随机串，攻击者无法通过枚举找到它；慢哈希防的是"人类自选的低熵口令可被字典枚举"，对全随机密钥没有收益，只给每个请求加上几十毫秒。这是**有前提**的取舍，前提是明文永远由系统生成。

第二，前缀索引。既然哈希不能由明文推出位置，就把明文里公开的一小段当作查找键——它不必保密，又能把候选缩到极少数。

第三，常数时间比对。多数标准库的判等会在第一个不同的字节处返回，耗时随"猜对了前几位"上升；攻击者远程重复这个过程，就能把哈希一位一位测出来。

```go
func naiveEq(a, b string) bool {   // 不要这样写
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return false   // 第一个不同处就返回：耗时随"猜对了几位"增长
		}
	}
	return true
}
```

### 本项目怎么做

铸造在 `MintKey`（`internal/gov/gov.go:42`）。三个常量在 `gov.go:23`：随机字节数、不含易混字符的目标字母表、前缀长度。30 字节即 240 位熵，这是"用快哈希"成立的前提；熵源失败直接返回服务端错误（`gov.go:44`），**不退回更弱的随机源**。

明文只在返回值里出现一次（`gov.go:59`），注释（`gov.go:39`）把它定为纪律：不得存储、不得写日志、不得回显。服务层遵守得很具体：`MintTenantKey`（`internal/service/admin.go:53`）把明文原样交给调用方，审计只记 Key 的 ID（`admin.go:61`）。存储用 `hashSecret`（`gov.go:62`）即 SHA-256 加 base64，写入时同时维护按前缀组织的索引（`internal/store/store.go:293`）。

认证 `Authenticate`（`gov.go:70`）走四个闸门：格式（`gov.go:71`）→ 前缀查索引（`gov.go:75`，常数代价，实现在 `store.go:307`）→ 常数时间比对（`gov.go:80`，`constEq` 在 `internal/gov/crypto.go:13`）→ 有效性与吊销（`gov.go:83`）。闸门顺序的取舍见 §11.4。

抽取 `ExtractSecret`（`gov.go:91`）同时接受 `X-API-Key` 与 `Authorization: Bearer`，理由是与其他沙箱平台客户端兼容；代价是认证面多了一个入口。

读代码时留意一处注释漂移：`Prefix` 注释写"前 12 个字符"（`tenant.go:127`），实际取 16 个（`gov.go:26`）；`Hash` 注释写"hex sha256"（`tenant.go:128`），实现用 base64（`gov.go:64`）。

### 替代方案

存明文外加数据库层面加密：把保护责任搬到密钥管理上；一旦有人拿到解出来的库，明文就回来了，而哈希的价值恰恰在于它**不可恢复**。换成慢哈希：上面讲过了，收益为零。换成 JWT 之类的自包含令牌：好处是无状态、自带过期时间；代价是吊销必须配一份额外黑名单，或者接受一段延迟——本项目让吊销在存储层发生一次状态迁移，下次认证立刻生效。

### 怎么验证

```bash
cd castle
go test ./internal/gov/ -run 'TestMintAndAuthenticate|TestExtractSecretHeaders' -v -count=1
```

预期两者通过。`TestMintAndAuthenticate`（`internal/gov/gov_test.go:24`）里最值得记住的三条断言：`Hash` 既不等于明文也不为空（`gov_test.go:32`）；从存储里重新读出的副本，任何字段都不可能是明文（`gov_test.go:36`）；尾部改一个字符必须认证失败（`gov_test.go:57`）。`TestExtractSecretHeaders`（`gov_test.go:221`）把两种头的优先级钉成测试。

## 11.4 Key 的过期与吊销：为什么打标记而不是删除

### 问题

一把 Key 泄漏了。值班的人首先要"立刻让它失效"，其次要"事后说清它什么时候失效、失效前有过哪些活动"。如果吊销实现成"从表里删掉这条记录"，第二件事就做不成了；更隐蔽的后果是：审计事件的行为者字段是 Key 的 ID，记录删掉之后，历史事件的作者变成一个查不到主人的标识符。

### 原理

吊销的语义是"作废一个凭据"，不是"抹去它存在过"。所以它是一个**状态迁移**，而不是一次删除。这一点与审计流的不可变是同一个原则的两面（见 §11.9）：历史不能被改写。

实现上有两条路。删除记录：最省事，但丢历史、丢可比对的哈希，还会让前缀索引出现空洞。打时间戳：记录保留，有效性在读取时判定，代价是每次认证多判一次。

过期与吊销在逻辑上可以统一成一个判据：**当前时刻是否落在凭据的有效区间里**。过期是右端点在铸造时就定了，吊销是运行时把它提前到"现在"。

### 本项目怎么做

`Valid`（`tenant.go:148`）就是这个判据。先看吊销戳（`tenant.go:149`），再看过期时间（`tenant.go:152`），最后按 scope 校验归属（`tenant.go:157`）。**吊销排在过期之前**是有意的：一把已被吊销、同时也已过期的 Key 如果报"已过期"，会把一次人工操作伪装成自然失效。

吊销本体在 `RevokeKey`（`internal/service/admin.go:69`）：先做形态与存在性检查（`admin.go:70`、`admin.go:73`）；跨租户吊销一律拒绝（`admin.go:77`）；写入走乐观并发（`admin.go:80`，数字段语义见 §10.7），且**幂等**——已被吊销过就直接报告成功（`admin.go:81`）；最后落一条撤销事件（`admin.go:91`），行为者是操作者的 Key ID。做成更新还有一个附带收益：前缀索引的一致性由存储层维护，甚至考虑到了将来某次更新改掉前缀的情形（`store.go:355`）。

### 替代方案

独立黑名单：一处额外组件加一个一致性窗口；本项目把黑名单做成对象上的一个时间戳字段，与持久化（见 §9）共用同一份日志。只给短生命周期、不做吊销：暴露窗口等于生命周期长度，还要求所有客户端实现轮换协议。

### 怎么验证

```bash
cd castle
go test ./internal/gov/ -run 'TestKeyExpiryAndRevocation' -v -count=1
```

预期通过。用例（`gov_test.go:62`）分两半：铸一把寿命 50 毫秒的 Key，立刻认证成功，睡 60 毫秒后必须失败（`gov_test.go:69`）；再铸一把，通过一次带版本的更新打上吊销戳，此后必须失败（`gov_test.go:85`）。

## 11.5 令牌桶：从计数窗口推到连续补充

### 问题

"每秒最多一次创建"听起来只需要一个计数器。但三种朴素做法各有漏洞：固定窗口计数在边界上翻倍——第 0.9 秒打满十次、第 1.1 秒再打满十次，真实速率是 0.2 秒二十次，而每行监控都合规；严格匀速会抹掉合理突发；至于"计数然后拒绝"，客户端不知道多久可以再来，只能盲目重试，把拒绝放大成重试风暴。

### 原理

把额度想成一个可以攒的容器：容量是突发值，决定攒满之后能连着做几次；补充速度是每秒数，决定长期平均；取不到就拒绝，并按补充速度算出还要多久。

三个推导决定了它的形状。**为什么按时间差连续补充**：`tokens += Δt × 每秒数`，不需要后台任务发令牌，因此没有定时器精度与生命周期问题。**为什么突发值不是多余参数**：它把长期均值与瞬时形状解耦，每秒一次加突发五，描述的是"平均每秒一次，但允许一次连来五个"。**为什么还要封顶**：不封顶时，闲置三天后的第一次突发就是无限大。

下面这段可以直接跑：

```go
package main

import ("fmt"; "time")

func main() {
	tokens, perSec, burst := 5.0, 1.0, 5
	last := time.Now()
	try := func(dt time.Duration) bool {
		now := last.Add(dt)
		tokens += now.Sub(last).Seconds() * perSec
		last = now
		if tokens > float64(burst) { tokens = float64(burst) }
		if tokens >= 1 { tokens--; return true }
		return false
	}
	n := 0
	for i := 0; i < 8; i++ { if try(0) { n++ } }
	for i := 0; i < 3; i++ { if try(time.Second) { n++ } }
	fmt.Println("allowed:", n, "tokens left:", tokens)
}
```

```text
allowed: 8 tokens left: 0
```

同一瞬间打八次，通过五次（突发值）；之后每秒一次，通过三次。"先给突发值，之后按每秒数走"就是令牌桶的全部内容。

### 本项目怎么做

状态在 `type bucket`（`gov.go:108`）：余额是浮点——按秒补充时时间差是小数，整数实现会有量化误差且总是偏宽松。

`Allow`（`gov.go:164`）入口先处理"没有策略"：策略为空或每秒数非正时直接放行（`gov.go:165`）。没有配置意味着上限待定而不是禁止；把空指针解释成"禁止"会让一处配置缺失变成全线故障，所以这条反直觉规则被 `TestNilOrZeroRateNeverBlocks`（`internal/gov/limiter_regression_test.go:83`）钉住。

结算三段（`gov.go:186` 到 `gov.go:194`）是补充、封顶、消费。`b.last = now`（`gov.go:187`）写在取时间差之后、判定之前：**无论放行还是拒绝，时间都在推进**；否则下一次补充会凭空变出一大笔令牌。

拒绝时的等待时间（`gov.go:196` 到 `gov.go:200`）：还差多少令牌除以每秒数，再加 0.999 实现向上取整——少报一秒会让客户端提前重试；至少为一，是因为零秒在部分客户端里被理解成"立刻重试"。策略变了会换桶（`gov.go:178`），下调速率的那一刻租户反而拿到全新满桶，配额变更低频所以可以接受。

### 替代方案

漏桶（按固定速率出队）：输出更平滑，但需要队列、丢弃策略与一个"排队多久"的新语义——那正是并发闸的职责（见 §10.8），两处重复会让两套超时参数互相打架。

滑动窗口计数（保留最近一秒的时间戳队列）：精度接近，代价是内存随窗口内请求数增长。外部存储加脚本的分布式限流：跨节点一致，代价是多一个组件（见 §16.1）。

### 怎么验证

```bash
cd castle
go test ./internal/gov/ -run 'TestLimiterBurstAndRefill' -v -count=1
```

预期通过。用例（`gov_test.go:90`）把推导压成两件事：每秒十次、突发三的桶，连打五次只通过三次（`gov_test.go:101`）；等 300 毫秒后额外一次必须通过（`gov_test.go:105`）——这个数字来自"300 毫秒乘以每秒十次等于三"。

## 11.6 分桶维度：租户 × 操作组，以及桶键不可被拼接伪造

### 问题

单租户单桶会撞上两类故障。其一：一个 CI 客户端每 200 毫秒轮询沙箱状态，这是合法读流量，却把唯一的桶打空，同一租户的创建请求被整体饿死——**限制生效了，但作用在错误的对手身上**。其二更隐蔽：管理员 Key 的租户 ID 是空串时，所有管理员落在同一个键上共用额度，最忙的把其他人饿死。它**不是限流失效**，而是限流生效在了错误的身份上，所以当时没有测试变红。

### 原理

份额该沿哪两个维度切，取决于两条判据。**成本判据**：一次调用消耗的资源量——起一个容器和读一个内存中的状态相差几个数量级，共享预算时廉价操作的合法突发会挤占昂贵操作的额度。**互不信任判据**：不该互相影响的调用方必须落在不同桶里，键必须由服务端生成。后一条不容妥协：一旦攻击者能用输入拼出别人的键，限流就从防御变成武器。

### 本项目怎么做

六个操作组定义在 `tenant.go:25`，外加便于批量构造的清单（`tenant.go:34`）。分组依据是后端动作的重量：创建最贵（每秒一次、突发五），快照次之（每秒零点五次、突发二），读最便宜（每秒二十次、突发四十）。默认值写在配置构造函数里（`internal/config/config.go:205`），配置文件没有对应键（`sandboxd.conf.example:95`）——调整只能按租户逐个下发。

键是身份加组的复合。`bucketKey`（`gov.go:153`）用长度前缀而不是分隔符：

```go
func bucketKey(identity, group string) string {
	return strconv.Itoa(len(identity)) + ":" + identity + "|" + group
}
```

为什么不用 `identity + ":" + group`：那样 `("a:b", "c")` 与 `("a", "b:c")` 会拼成同一个串。`gov.go:147` 的反驳很准——"不会有 ID 含分隔符"这种推理，恰恰会在后来者开始用别的东西做身份时失效。长度前缀则没有任何输入对能互相落在一起。

身份由谁决定？不是调用方。`limiterIdentity`（`internal/httpapi/server.go:411`）自己生成：租户级 Key 按租户，没有租户的管理员 Key 按 Key 自身的 ID；注释（`server.go:405`）记录了上面那个版本缺陷。`Allow` 里还有一道兜底：身份为空时落到匿名（`gov.go:168`）。

策略怎么找到：`RateFor`（`internal/gov/quota.go:63`）先看租户有没有覆盖，再看平台默认，都没有就返回空。结合 §11.5 的"空策略等于放行"，一个反直觉的结论浮出来——**从默认表里删掉一个组，等于把这个组设成无限**。所以组名清单与它的校验不能被管理接口绕过。

### 替代方案

只按租户分桶：解决不了开头那个"读流量饿死创建"的场景。按端点路径分桶：但端点会改名、会加版本前缀，旧策略随即静默失效；六个操作组是一层**有意的间接**：端点可以变，分类不变。按 Key 分桶对本租户不合适，轮换 Key 等于额度翻倍；管理员 Key 用它，正是因为 Key 不属于任何租户。

### 怎么验证

```bash
cd castle
go test ./internal/gov/ -run 'TestLimiterPerTenantPerGroupIsolation|TestGroupsAreIndependentPerIdentity|TestDistinctIdentitiesGetDistinctBudgets|TestBucketKeyCannotBeForgedByConcatenation' -v -count=1
```

预期四个全部通过，对应本节四个论点：组之间不共享预算（`gov_test.go:120`）、租户之间不共享（`gov_test.go:123`）、管理员凭据之间不共享（`limiter_regression_test.go:37`）、键不可通过拼接伪造成另一个调用方的键（`limiter_regression_test.go:68`，用例里连空字节和竖线都试过了）。

## 11.7 限流、配额、并发闸：三道门的失败信号为什么不同

### 问题

设计评审里最常见的混淆是"我们已经限速了，为什么还要配额"。看一个场景：租户的创建速率是每秒一次，看着很克制，但它从不删除沙箱——跑一万秒之后它就持有一万个沙箱，**速率全程合规，机器已经被占死**。反过来：一个还有两百个名额的租户在一瞬间发来两百次创建，配额检查对这批全部通过，因为它们看到的是同一份还没有新记录的台账。

### 原理

三者约束的是时间轴上三个不同的量：限流约束**速率**（单位时间的次数），配额约束**存量**（同时持有的资源），并发闸约束**在途数**（正在实施的次数）。

| | 约束的量 | 时间尺度 | 释放方式 | 失败时的指令 |
|---|---|---|---|---|
| 限流 | 速率 | 秒 | 自动，时间流逝补充令牌 | 稍后照发：429 + 等待秒数 |
| 配额 | 存量 | 长期，直到释放 | 显式，删除对象 | 先释放或加容量：402 |
| 并发闸 | 在途数 | 毫秒到秒 | 自动，动作结束 | 排队超时，`state=failed` |

为什么不能互相替代，从量纲就能推出来。速率乘时间等于存量，只要时间够长、对象不释放，任何非零速率都能积累出任意大的存量——限流管不住存量。而一批同时到达的请求在检查时看到的台账完全相同，所以配额管不住瞬时压力。

最后一列最需要记住：三者都是"没成"，但对客户端是**三条不同的指令**。合成一个"失败了"，客户端就只剩盲目重试（错误包络的机制见 §12.4）。

### 本项目怎么做

限流在 HTTP 层，落到 `rateLimit`（`server.go:373`）：先取到凭据与租户（`server.go:375`、`server.go:383`），再由 `RateFor` 选策略、`Allow` 判定（`server.go:391` 到 `server.go:393`）。它的失败方向值得留意：拿不到凭据时选择报错而不是放行（`server.go:376`）——限流器出现在认证之后却拿不到凭据，说明调用栈装错了（顺序见 §12.3）。拒绝时同时写两个响应头：等待秒数（`server.go:394`）与该组速率上限（`server.go:395`），错误详情里带上组名（`server.go:397`）。

配额在服务层（`service.go:118`），是**业务语义的一部分**而不是 HTTP 的事：同一个检查函数被服务层的单元测试直接调用（`internal/service/service_test.go:526`）。并发闸则是按这台机器定的全局信号量，失败形态是排队中的创建超时后变成 `failed` 并写明原因（见 §10.8）。

### 替代方案

只包含限流：存量没有上界，就是开头那个"一万秒"的场景。只包含配额：瞬时洪峰会全部通过准入检查，然后一起撞向同一个节点。三者合并成一个"预算"对象概念更少，但三个量单位不同，强行统一后任何一维出问题都只能到另一维打补丁。

### 怎么验证

```bash
cd castle
go test ./tests/ -run 'TestQuotaAndAdmission/create_rate_limit_is_governed_and_retryable' -v -count=1
```

预期通过。这个子用例（`tests/governance_test.go:45`）把创建组掐到每秒零点零一次、突发一，用有意的缓慢让判定与时间无关：第二次创建必须得到 429 并带上等待秒数（`governance_test.go:64`），且这一次绝不能产生沙箱（`governance_test.go:69`）。
## 11.8 配额记账：继承、越限，以及"返还"是怎么保证的

### 问题

配额系统最典型的失效是只有加法没有减法：删除了沙箱，额度没有回来。三天之后所有创建都得到 402。这类缺陷一定会以工单的形式回来，因为**账单按被占住的额度算**

### 原理

**增量计数器**：创建加一、删除减一。判定是常数代价，但要求每一条终态路径都记得减——正常删除、强制删除、TTL 到期、空闲回收、创建失败回滚，漏一处就是泄漏，且泄漏早期不可察觉。

**现场求和**：每次判定时重新遍历该租户的对象并按状态汇总。慢一些，但**没有可以出错的计数器**：对象的消失就是额度的归还，两者不可能漂移，因为它们本来就是同一份数据。几百个对象规模上一次遍历是微秒级；让增量计数器不漂移却需要常驻一套校对逻辑——前者是一次性成本，后者是永久复杂度。

### 本项目怎么做

`CheckCreate`（`quota.go:74`）遍历全部沙箱，按租户过滤（`quota.go:78`），交给 `CountableResources`（`internal/domain/sandbox.go:279`）求和。这个函数按状态决定是否计数（`sandbox.go:281`）：从开始供应到暂停中的五个状态都计一个沙箱并带上它的 CPU 与内存，停止与失败都计零。**"什么时候算被占住"由对象决定，而不是由记账代码决定**——这正是选现场求和才能得到的性质；`gov_test.go:177` 的注释把这个边界钉住了。

三项检查各自都会产出带具体数字的拒绝（`quota.go:88`、`quota.go:91`、`quota.go:94`）：当前值、上限，以及这次请求会带来什么。零值表示继承平台默认（`Effective`，`quota.go:41`）；代价是无法表达"明确允许零个"，收益是管理接口传 JSON 时不必区分缺失字段与零值。

越限来自"检查"与"写入"之间没有任何约束把两者绑在一起（`quota.go:70` 的注释自己写明它在竞态下是尽力而为的）。用一段可运行的代码看清机制：

```go
package main

import ("fmt"; "sync"; "time")

func main() {
	n := 0
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		// 检查与写入之间只要有真实工作，20 路就全部通过检查
		go func() { defer wg.Done(); if n < 5 { time.Sleep(2 * time.Millisecond); n++ } }()
	}
	wg.Wait()
	fmt.Println("admitted:", n)
}
```

```text
admitted: 20
```

把那次休眠去掉，本机连跑八次都是 `admitted: 5`，但加上竞争检测会报告数据竞争——越限程度取决于检查与写入之间的**窗口有多宽**，不是运气。产品的窗口里有存储遍历与一串校验，所以稳定越限：20 路并发对着上限 5 全部通过。能接受它的前提是两侧夹住：在途创建数有上界（见 §10.8），突发结束后的下一次创建必然交付 402。公开这个数字，见 §16.2。

### 替代方案

增量计数器加后台校对：快，但要留住两个真相来源，还得回答"校对时发现不一致怎么办"。

在检查与写入之间加锁：精确，但每一次创建都要持有一把跨"检查、后端创建、落记录"的锁，而后端创建是秒级的，所有创建因此被串行化。用秒级的全局锁换"突发时不越限"，是把可收敛的软问题换成吞吐量的硬问题。

### 怎么验证

```bash
cd castle
go test ./tests/ -run 'TestQuotaAndAdmission/sandbox_ceiling_binds_and_frees_on_delete' -v -count=1
go test ./tests/ -run 'TestQuotaAdmissionIsBestEffortButConverges' -v -count=1
```

第一条验收返还：上限二的租户第三次创建得到 402，删掉一个之后立刻可以再次创建（`tests/governance_test.go:26` 到 `governance_test.go:38`）。第二条验收收敛：

```text
concurrency_test.go:198: burst of 20 concurrent creates against a ceiling of 5: 20 admitted, 0 refused (overshoot 15)
```

它断言的不是"恰好五个"，而是三件事：没有任何请求得到五开头的状态码（`tests/concurrency_test.go:193`）、被放行数量不少于上限且不多于发出数量（`concurrency_test.go:203`、`concurrency_test.go:206`）、存量必须等于被放行数（`concurrency_test.go:211`）。

## 11.9 审计流：不可变、类型化，以及写入成本的边界

### 问题

半夜有人删了一个客户的沙箱。谁删的、什么时候、以什么名义——如果这些信息可以从同一份数据里被抹掉，"查不出来"与"被删了"就无从区分。审计存在的唯一理由是回答"到底发生了什么"，它一旦可疑，系统可信度随之崩塌。

另一个问题是成本：写入要落预写日志并刷一次盘（机制见 §9），若每个请求一次刷盘，一次洪泛的成本就由你的磁盘代付。

### 原理

四条原则。**不可变**：写入后既不修改也不删除，这不是"我们不打算改"，而是数据结构上不提供改的能力；理由是递归的——一条事件若可被改，"谁改了它"又需要另一条事件证明。**每个状态变更一条事件**：只存状态时"它怎么变成这样的"不可回答，存了动作则能重放出状态。**类型优于字符串**：把语义编码进动作类型而不是一个原因字段，差别在于能否被按键过滤和计数。**成本边界**：攻击者可控且频率可任意提高的入口，全量记录等于把对方的输入换成你自己付；出路只有聚合，同时保住起点可见。

### 本项目怎么做

事件模型在 `internal/domain/event.go:39`，约束写在紧邻的类型注释里（`event.go:37`）：详情只能放标识符、计数、时长，**不得放文件内容、环境变量值或密钥材料**；行为者记 Key 的 ID 而不是名字或明文（`event.go:44`），所以 Key 被吊销之后历史事件的行为者仍指向一个可解释的实体。

动作是枚举常量而不是字面量（`event.go:10` 到 `event.go:34`），回收因此被拆成 TTL 到期与空闲到期两个类型，"多少因为到期、多少因为空闲"就是一次按键过滤（思路来源见 §10.9）。

写入走 `Audit.Record`（`internal/gov/audit.go:28`）到 `Store.AppendEvent`（`internal/store/store.go:695`），后者的注释（`store.go:689`）自称是全产品频率最高的一次刷盘，并要求：可被无凭据调用方触达的位置必须在调用它之前限流（`store.go:692`）。内存留存有界，超阈值一次丢掉最老的百分之十（`store.go:702`）而不是一条一条丢，默认十万条（`store.go:92`）。

租户隔离比"记得加过滤条件"更硬：`Service.ListEvents`（`internal/service/admin.go:97`）第一行就把查询里的租户字段无条件覆盖为路径上的那个。记录失败不阻塞它所描述的操作（`audit.go:10`），但涉及凭据与管理的调用方会自己检查这个错误（`audit.go:13`）。写进详情的自由文本先过一遍清洗（`internal/service/service.go:396`）把控制字符换掉——日志一行一条 JSON，一个换行符就能伪造出一整条。

### 替代方案

可更新的审计表加"更正行"：能修录入错误，却引入"哪一行才是最新"，更糟的是给了任何能写库的角色一个改写历史的接口。改为结构化日志：按大小轮转、可被裁剪，留存不可承诺。只审计管理动作：省下绝大部分写入，但"谁删了这个沙箱"就无从回答。

### 怎么验证

```bash
cd castle
go test ./tests/ -run 'TestAuditTrail' -v -count=1
```

预期三个子用例通过：每条事件的租户字段都等于本租户（`tests/governance_test.go:333`）、按动作过滤不串值（`governance_test.go:338`）、别的租户的事件不会进来（`governance_test.go:345`）。

## 11.10 攻击者能自己造键的地方：桶表与审计写放大

### 问题

本节处理两处同源的风险：令牌桶表的键来自请求方（未认证阶段的身份就是来源地址，而来源地址可以换出口无数次变化），审计写入挂在一条无凭据也能触达的路径上。攻击者发出 N 个请求，就迫使服务端付出 N 份内存或 N 次刷盘。

一句可以记住的话：**如果一个你不信任的人能决定一张映射表的键，那张表本身就是攻击面**。

### 原理

防御按强度递进有三层：定期清扫，必要但不充分——两次清扫之间表可以任意增长；硬上限加批量淘汰，把"任意增长"变成"最多到某个数"；重新设计键的来源，让对方根本没法挑选。

写入成本那一侧没有清扫可用，能做的只有三件事：门口限流、按来源聚合、指标不采样。

### 本项目怎么做

事前闸门：`preAuthLimit`（`server.go:273`）的注释（`server.go:271`）记下它的来历——某个版本完全没有它，被拒绝的请求既享受不到限制，还要付一次刷盘。它按来源地址分桶（`server.go:276`），默认每秒两百、突发五百（`config.go:225`）。两处反直觉：它只读远端地址而不读转发头（那是由攻击者提供的值，跟随它就等于让一台主机无限变换身份，`server.go:256`）；阈值刻意宽松（`config.go:97` 说共享出口的 CI 池会把很多正常客户端挤在同一个地址后面），真正的业务约束是按租户分组的那六个桶。

桶表的另外两层：`maxBuckets` 定为二十万（`gov.go:129`），满了又要新建时一次淘汰八分之一（`gov.go:180`）——`evictOldestLocked`（`gov.go:234`）要按最后活跃时刻把整张表排序，每次只淘汰一个意味着每次插入都付一遍全排序。`sweeper`（`gov.go:252`）每十分钟清掉半小时没活动的桶，替代不了上限（`gov.go:126` 给了原因）；`Stop`（`gov.go:141`）结束清扫协程，`gov.go:137` 点明泄漏形态——每个服务实例泄漏一个，测试里就是每个用例泄漏一个。

审计聚合：`authFailureRecorder`（`server.go:299`）按来源地址记数，窗口内第一次完整入库以保留攻击起点的证据，之后每一百次才再记一次并附上累计数（`server.go:348`）。三个常量在 `server.go:312`：窗口一分钟、阈值一百、键上限五万，到顶整表丢弃（`server.go:336`）。指标则不采样：`noteAuthFailure`（`server.go:351`）无条件加一，`server.go:352` 解释它为什么故意不带标签——把来源地址当标签值，等于让任何人无限撑大指标的注册表。

### 替代方案

精确的 LRU 链表：单次代价更低，但每次访问都要移动节点，临界区随之变大。对未认证请求完全不写审计：省下成本，代价是"攻击从什么时候开始"再没有答案。把刷盘改成异步：更快，但改的是持久化语义本身（见 §9），本项目选择少写。

### 怎么验证

```bash
cd castle
go test ./internal/gov/ -run 'TestEvictionBoundsTheBucketTable|TestSweepDropsIdleBuckets|TestStopTerminatesTheSweeper' -v -count=1
go test ./internal/httpapi/ -run 'TestAuthFailureAuditIsAggregatedPerWindow' -v -count=1
```

第一组验收上界、清扫与协程退出：`TestEvictionBoundsTheBucketTable`（`limiter_regression_test.go:121`）断言只删最老的一批，最新的一批必须还在（`limiter_regression_test.go:141`）；`TestStopTerminatesTheSweeper`（`limiter_regression_test.go:148`）用协程数量对比证明清扫协程退出。第二条把 250 次失败压成 3 条审计事件，本机输出：

```text
regression_test.go:165: 250 failures collapsed into 3 audit events
```

它还断言 `sandboxd_auth_failures_total{}` 仍然等于 250（`internal/httpapi/regression_test.go:174`）——审计可以聚合，指标不可以。

---

### 本章检验

1. `CheckCreate` 每次重新遍历求和而非维护计数器。请数出"应当做减法的路径"有几条，说明这个选择如何把清零风险挪出代码，并解释 `CountableResources` 为何必须按状态返回零或一。
2. `Allow` 中 `b.last = now` 写在补充之后、判定之前。若改成"只在放行时更新时间"，请给出速率、突发值与请求序列，算出桶最后停在什么时间上。
3. `Allow` 在策略为空或速率非正时放行，`Validate` 又要求速率组必须是六个已知组之一。若有人从清单里删掉一个组名，配置校验与准入判定各会发生什么。
4. 429 与 402 都表示"这次没成"。各给出一个只会触发其中一个的调用形态，并推演：合并成一个错误码后，只会重试的客户端面对不该重试的拒绝时会做什么。
5. 审计详情允许路径与 Key ID，不允许文件内容与密钥明文，actor 记 Key ID 而非名字。请解释为什么同样"与本次操作有关"，有的能进有的不能；以及 actor 记 ID 为何是吊销后仍可解释的前提。
6. **动手任务**：照 `TestSweepDropsIdleBuckets`（`internal/gov/limiter_regression_test.go:96`）注入手动时钟，把 `TestLimiterBurstAndRefill`（`internal/gov/gov_test.go:90`）改写成不依赖真实休眠的确定性版本，并断言：余额落在 0 与 1 之间时 `Allow` 返回的等待秒数必须是 1。用 `go test ./internal/gov/ -run '你的用例名' -v -count=1` 跑三遍确认稳定。


# §12 HTTP API 与契约设计

## 12.1 契约就是产品：三个必须先回答的问题

### 问题

把 HTTP 层当成"把函数暴露出去"的胶水，在单用户工具里没问题，在一个**要被不信任的客户端调用**的服务里会出事。三个失败模式：

1. **重试风暴**：客户端分不清"可以重试"和"重试也没用"，于是对 5xx 一律重试，把已过载的服务打得更死。
2. **信息泄漏**：错误响应带上内部细节（堆栈、路径、SQL），等于给攻击者发侦察报告。
3. **无法定位**：线上出问题，客户端只说"我调你们接口失败了"，而你没有东西能把这条请求和你的日志对上。

答案都不在业务逻辑里，在**契约设计**里：本章的每个决定都在回答**客户端拿到什么、该怎么反应、反应错了会怎样**。

### 原理：三层信息，各有一层会失效

一次响应能携带三类"出了什么错"的信息，可靠性完全不同：

| 层 | 载体 | 谁会改它 | 可靠吗 |
|---|---|---|---|
| 传输层 | HTTP 状态码 | 反向代理、WAF、网关 | 不可靠，会被改写 |
| 语义层 | 响应体里的 `code` | 只有服务端 | 可靠 |
| 追溯层 | `request_id` | 只有服务端 | 可靠 |

状态码不可靠不是理论担忧：后端超时时代理返回 504，而后端**已经执行了那个创建动作**；有 WAF 把 429 改写为 503，客户端于是"稍后重试"，真语义却是"你超配额了，永远别重试"。**状态码是给中间设备看的，不是给客户端业务决策看的。**

写成可执行形式，就是按 `code` 分派：

```go
package main

import "fmt"

type envelope struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

func decide(e envelope) string {
	switch e.Code {
	case "rate_limited", "backend_unavailable":
		return "backoff and retry"
	case "invalid_request", "auth_failed", "forbidden", "not_found":
		return "fail fast"
	case "conflict":
		return "re-read state, then decide"
	}
	if e.Retryable {
		return "retry once"
	}
	return "fail"
}

func main() {
	for _, e := range []envelope{
		{"rate_limited", true}, {"invalid_request", false},
		{"conflict", false}, {"internal", true},
	} {
		fmt.Printf("%-18s -> %s\n", e.Code, decide(e))
	}
}
```

预期输出：

```text
rate_limited       -> backoff and retry
invalid_request    -> fail fast
conflict           -> re-read state, then decide
internal           -> retry once
```

注意 `internal` 这一行：它依赖包络里的 `retryable` 而不是"500 就重试"。这就是显式字段与按状态码推导的区别。

### 本项目怎么做

包络由 `writeError`（`internal/httpapi/server.go:429`）统一写出，非 2xx 响应全部走它，形状固定四个字段：

```go
_ = json.NewEncoder(w).Encode(map[string]any{
    "error": map[string]any{
        "code":       e.Code,
        "message":    e.Message,
        "retryable":  e.Retryable,
        "request_id": rid,
    },
})
```

`code` 是稳定枚举（`internal/apperr/apperr.go:17`），`HTTPStatus()`（`internal/apperr/apperr.go:32`）把它映射成状态码。这个映射**单向**：服务端从 code 推出 status，客户端不反向依赖。规则写死在包级文档注释里（`internal/apperr/apperr.go:3`）：

```go
// Error codes are part of the public API contract: clients branch on Code,
// never on HTTP status or message text. Retryable is an explicit field —
// HTTP 409 can be retryable (optimistic-lock conflict) or not (illegal
// state transition), so it must never be inferred from the status code.
```

注释里给了一个具体反例：同样是 409，乐观锁冲突可以重试，非法状态转换不可以。**状态码承载不了这个区分，所以判定依据必须是另一个字段。**

### 替代方案

- **按状态码分支。** 就是上面两个被改写的真实场景；代价是客户端在不知情时把一次永久失败放大成 N 次请求。
- **按 `message` 文本匹配。** 措辞调整、拼写修正、本地化都会打断匹配，且这种断裂只在运行时暴露。
- **换成 gRPC 或自定义二进制协议。** 状态码问题确实消失，但脱离 HTTP 生态：curl 不能调、代理不能看，集成形态也要重写（选 REST 的理由见 §15）。

### 怎么验证

```bash
go test ./tests/ -run 'TestHTTPContract' -v -count=1
```

该用例含 12 个子测试，其中 `errors_carry_the_code_clients_branch_on` 断言 `e.Code == apperr.NotFound`、`e.Message != ""`、`e.RequestID != ""`——四个字段里三个被逐条钉住。实测全 12 项 PASS。

## 12.2 REST 资源建模：什么时候用名词，什么时候用动作

### 问题

沙箱有 `pause`/`resume`/`stop` 三个端点，它们是动词。这还算 REST 吗？若坚持"URI 里只能有名词"，就得改造成 `PATCH /v1/sandboxes/{id} {"state":"paused"}`。这个改造看上去更"纯"，却引入了一个新问题：一个端点要同时表达三种互不相干的后端动作。

### 原理：判断标准是"语义能否折叠"

折叠成一个字段的前提是：这些取值是同一属性的不同状态，校验规则一致，失败语义一致。一旦不满足，折叠就是把**类型信息降级成字符串**。

判据只有一条：能表述成"把某字段设成某值、其余不变"的是属性修改，用 `PATCH`；触发一个自带前置条件、自带失败模式、自带副作用的过程的是动作，给它自己的端点。`pause` 与 `stop` 属于后者——前者冻结进程组，后者终止容器，前置条件与失败模式都不重合（后端动作见 §8）。

### 本项目怎么做

路由表节选（`internal/httpapi/server.go:72`）：

```go
mux.Handle("POST /v1/sandboxes", s.chain(domain.GroupCreate, s.createSandbox))
mux.Handle("GET /v1/sandboxes", s.chain(domain.GroupRead, s.listSandboxes))
mux.Handle("GET /v1/sandboxes/{id}", s.chain(domain.GroupRead, s.getSandbox))
mux.Handle("POST /v1/sandboxes/{id}/pause", s.chain(domain.GroupRead, s.pauseSandbox))
mux.Handle("POST /v1/sandboxes/{id}/resume", s.chain(domain.GroupRead, s.resumeSandbox))
mux.Handle("POST /v1/sandboxes/{id}/stop", s.chain(domain.GroupRead, s.stopSandbox))
mux.Handle("POST /v1/sandboxes/{id}/timeout", s.chain(domain.GroupRead, s.extendSandbox))
mux.Handle("DELETE /v1/sandboxes/{id}", s.chain(domain.GroupCreate, s.deleteSandbox))
mux.Handle("POST /v1/sandboxes/{id}/exec", s.chain(domain.GroupExec, s.exec))
```

支持这么建模的三条理由：

**审计需要区分动作。** 审计事件按动作类型过滤（§11.9），`ActKeyAuthFail` 这类常量是**类型**而不是字符串。折叠进 `PATCH` 后，"这次到底是暂停还是停止"只能从 payload 反推。

**动作与资源按成本分组。** `DELETE /v1/sandboxes/{id}` 用的操作组是 `domain.GroupCreate` 而不是某个 "delete" 组（`server.go:79`）——删除和创建消耗的是**同一类资源**，都要调用引擎、都要改配额。放进同一个速率桶，能挡住"疯狂创建 + 疯狂删除"耗尽引擎调用预算这种形状。这是按**成本**分组而非按**语义**分组的例子（分组机制见 §11.6）。

### 替代方案

- **折叠成 `PATCH` + `state` 字段。** 上面三条理由。额外代价：请求校验要在 handler 里判断"这次改的是哪个字段"，每加一个状态就改一遍。
- **RPC 风格：所有调用走 `POST /v1/invoke`。** 405 消失、可缓存性消失、代理看不到调用意图；收益只有"端点数量少"，而这从来不是瓶颈。

### 怎么验证

```bash
# 动作端点存在；未注册的方法返回 405，不会退化成 404
curl -s -o /dev/null -w '%{http_code}\n' -X PATCH \
  -H "Authorization: Bearer $ADMIN_KEY" $BASE/v1/sandboxes
```

预期 `405`。`unknown_routes_are_404` 与 `wrong_methods_are_405` 两个子测试分别钉住"未注册路径 = 404"与"方法不匹配 = 405"——这两个语义不能被彼此冒充。

## 12.3 请求 ID：贯穿全链路的那根线

### 问题

上一节的缺陷能被发现，前提是有人能复现它。而复现线上问题的第一步永远是"把这行日志和那次请求对上"。响应里没有任何标识，客户端就只能说"我调你们接口失败了"。

### 原理：ID 必须在"最外层可见"的位置生成

请求 ID 的作用双向：服务端写日志时带上它，于是日志可按 ID 检索；客户端从响应头或包络里拿到它，于是报障时能给出它。

要让第二点成立，ID 必须在**任何可能提前返回的层之外**生成。否则被限流、被 panic 的请求就拿不到 ID——而那恰恰是最需要排查的请求。

### 本项目怎么做

`requestID`（`internal/httpapi/server.go:168`）接受客户端传入的 ID：

```go
id := r.Header.Get(requestIDHeader)
if id == "" || len(id) > 64 {
    var b [8]byte
    _, _ = rand.Read(b[:])
    id = hex.EncodeToString(b[:])
}
w.Header().Set(requestIDHeader, id)
```

为什么接受外部传入的 ID？因为调用方通常是另一个服务，它自己也有链路追踪 ID，允许透传就能把**上游的链路和你的日志对起来**。`len(id) > 64` 这个条件是防御性的：超长字符串会进访问日志与错误包络，可能污染日志、撑爆索引，甚至被用来做日志注入。限制长度而非字符集是刻意的——字符集校验会把带冒号的 trace id 挡在外面。

三条输出路径都带 ID：`requestID` 写进响应头（`server.go:176`）、`accessLog` 写进每一条访问日志（`server.go:193`）、`writeError` 放进错误包络（`server.go:434`）。判据就一句：**任何一次外部可见的输出，都必须能追溯到一条请求。**

### 替代方案

- **只用服务端生成的 ID。** 上游服务自己的链路追踪 ID 传不进来，跨服务链路就断了——而跨服务恰恰是最需要 ID 的场景。
- **信任任意外部 ID。** 客户端可控的字符串进了日志系统，等于让调用方决定日志的内容与体积。

### 怎么验证

```bash
curl -s -D - -o /dev/null -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'X-Request-Id: caller-supplied-id' $BASE/v1/sandboxes | grep -i x-request-id
curl -s -D - -o /dev/null -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'X-Request-Id: $(python3 -c 'print("A"*200)')' $BASE/v1/sandboxes | grep -i x-request-id
```

实测输出：

```text
X-Request-Id: caller-supplied-id
X-Request-Id: 73c6f17ad60396ab
```

短 ID 原样回显；200 字符的 ID 被替换成 16 个十六进制字符（8 字节随机数）。端到端子测试 `request_ids_are_echoed_or_generated`（`tests/contract_test.go:152`）断言这两条，其中第二条还断言长度恰好为 16。

## 12.4 中间件链：为什么顺序是安全属性

### 问题

每一层都"包"在另一层外面，代码上只是一次函数调用，看起来顺序只是执行次序。但看一个具体反例：如果 `recover` 包在 `accessLog` 外面，一次 panic 会被 `recover` 就地写成 500，而 `accessLog` 根本没机会看到它——**最需要记进访问日志的那条请求，恰好是唯一不会被记录的那条**。

### 原理：外层 = 更早生效 = 能看到更多

嵌套结构决定两件事：**谁先拒绝**（外层先执行，外层拒绝时内层完全不跑），**谁能观察到最终状态**（外层的后置代码最后执行，能看到内层写出的一切）。

于是对每个相邻对问两个问题就够：

- 这一层需不需要下一层产出的东西？（需要 → 必须待在下一层之内）
- 这一层要不要在下一层之前就把请求挡掉？（要 → 必须在下一层之外）

链的构造在 `internal/httpapi/server.go:127`：

```go
func (s *Server) chain(group string, h http.HandlerFunc) http.Handler {
	return s.accessLog(s.recover(s.requestID(
		s.preAuthLimit(
			s.bodyLimit(
				s.authenticate(
					s.rateLimit(group, h),
				),
			),
		),
	)))
}
```

从外到内：`accessLog → recover → requestID → preAuthLimit → bodyLimit → authenticate → rateLimit → handler`（闭包机制见 §4，这里只看位置）。

**`accessLog` 在最外层。** 它必须能观察到最终状态码，包括 `recover` 捕获 panic 后写出的 500；它靠 `statusWriter`（`server.go:199`）捕获状态码。放里面，panic 就不进访问日志。

**`recover` 在 `requestID` 之外。** panic 发生时 `request_id` 已被写进响应头，`recover` 的 `writeError` 能把它读回来（`server.go:434`）。**用户报障时手上一定有一个能给你的 ID。** 反过来放，这条请求永久失去可追溯性。

**`preAuthLimit` 在 `bodyLimit` 之外。** 这一对最容易搞反。直觉是"应该先限 body 再限流"，但 `bodyLimit` 用的是 `http.MaxBytesReader`（`server.go:215`），它只包一层计数 reader，**不触发任何读**。所以把它放内层不会先吃掉内存；反过来让 `preAuthLimit` 待在外层，能让洪水在进入任何后续逻辑前就被拒。

**`bodyLimit` 在 `authenticate` 之外。** 请求体上限是**传输层的属性**，不是身份的属性。装在所有可能读 body 的代码之前，就不必依赖每个 handler 记得自己限一次。文件上传是唯一例外（`server.go:212`）：

```go
limit := s.Cfg.MaxRequestBody
if strings.HasSuffix(r.URL.Path, "/files") && r.Method == http.MethodPut {
    limit = s.Cfg.MaxFileBytes + 4096 // raw body 上传路径允许大请求体
}
```

**`authenticate` 在 `rateLimit` 之外。** 限流按租户计费，需要已认证身份。而 `rateLimit` 里有一个**失败即关闭**的守卫（`server.go:376`）：

```go
key := keyFrom(r)
if key == nil {
    // authenticate always runs before this in chain(). Reaching here
    // means the stack was misassembled, so fail closed rather than
    // wave the request through unlimited.
    s.writeError(w, r, apperr.InternalErr("rate limiter reached without an authenticated key"))
    return
}
```

价值在于：将来有人把链装反，结果是**报错并拒绝**，而不是"不限流地放行"。顺序约束从"靠人记住"变成"装错了会响"。

### 本项目怎么做

上面的逐层分析就是全部实现——顺序没写在配置里，也没写在文档里，而写在这一个函数的嵌套结构里。包级文档注释（`server.go:1`）把它称为 "the security contract"，但注释与代码有一处不一致：注释写的是 `recover → request-id → access-log → ...`，而代码里 `accessLog` 在 `recover` 之外。**以代码为准：注释会过期，嵌套结构不会。**

### 替代方案

- **每个 handler 自己按序调用认证、限流、记账。** 每加一个端点抄一遍，漏一次就是一次裸奔；顺序散落在几十处，无法一眼审完。
- **声明式中间件表 + 框架自动排序。** 顺序被藏进框架内部，出问题时得先读懂框架的排序规则，才能判断当前的链是什么。

### 怎么验证

```bash
go test ./internal/httpapi/ -run 'TestPreAuthGateRunsBeforeAuthentication' -v -count=1
```

它把防洪闸设成 `burst=1` 后发两个未认证请求：第一个必须是 401（说明它**穿过**了防洪闸、到达了 `authenticate`），第二个必须是 429（说明防洪闸在 `authenticate` **之外**）。两条断言合起来唯一确定了相对顺序。

## 12.5 顺序错了会发生什么：两个版本、两次顺序缺陷

### 问题

12.4 讲完了"正确的顺序是什么"。但"顺序即安全属性"这句话的分量，要等看见顺序错了的后果才成立。这个仓库里有**两次**顺序缺陷，而第二次是第一次的修复动作直接造成的。

### 原理：先问"作用在哪个身份上"

第一次缺陷的表面描述是"限流没生效"，实际原因是**限流生效在了一个空的身份上**：当时只有一个限流器，挂在认证之后按租户计数，而未认证请求没有租户，于是不受限。

把它"挪进认证之后"修掉了空身份问题，同时制造了第二个问题：**未认证请求从此完全不经过任何限流器**，但仍然会写一条审计事件，也就是一次 fsync。于是"认证失败"从一个廉价操作变成了一个 I/O 放大器——触发它不需要任何凭据。

共同根因是同一个：把"顺序"当成了唯一的设计变量。换成"这个机制作用在哪个身份上"，答案立刻变成：未认证流量的身份是**客户端 IP**，它需要一个自己的限流器，位置在认证之前。

### 本项目怎么做

第一次缺陷记录在 `CHANGELOG.md:533`：

```text
1. **限流中间件顺序反了**：rateLimit 在 authenticate 外层，认证前 key 为空导致
   限流被静默跳过——HTTP 级限流测试（而非仅 Limiter 单测）抓出。
```

第二次缺陷记录在 `CHANGELOG.md:400`，因果写进了代码注释（`internal/httpapi/server.go:110`）：

```text
// The v1.1.0 stack ran the rate limiter INSIDE authenticate ("it needs the
// authenticated tenant"). That is true for the per-tenant business quota, but
// it meant a request that failed authentication never reached a limiter at
// all — while still costing one audit event (and therefore one fsync) per
// request. 400 unauthenticated requests produced 400 fsyncs and zero 429s
// (poc/probe_live.sh, PoC-5): an I/O amplifier that needs no credentials.
```

修复方式写在紧随其后的注释里（`server.go:122`）：

```text
// The fix is two limiters on two identities, not one limiter moved:
//   - preAuthLimit runs outside authenticate, keyed by client IP. It is a
//     coarse flood gate (generous by default) and applies to everyone.
//   - rateLimit still runs inside, keyed by tenant, and remains the
//     authoritative per-tenant business quota.
```

"**两个限流器挂在两个身份上，而不是把一个限流器挪位置**"。实现对应两处：`preAuthLimit`（`server.go:273`）按 `"ip:" + clientIP(r)` 计数；`rateLimit`（`server.go:373`）按 `limiterIdentity(key)` 计数（后者处理管理员 Key 租户 ID 为空的问题，见 §11.6）。

配套的第二道闸是审计写入聚合（`server.go:290`）：

```text
// authFailureRecorder stops an unauthenticated caller from turning each 401
// into a WAL frame plus an fsync. It aggregates per client IP over a window:
// the first failure is recorded in full — so an attack's onset is visible in
// the audit log — and after that only every authFailAuditEvery-th one is, with
// the running count attached. 400 failures become a handful of fsyncs instead
// of 400, and the growth curve stays legible.
```

细节值得单独学：**首次全量记录，之后每 100 次带计数**。干脆不记，攻击的开始时刻就从审计日志里消失了；全记，那就是放大器本身。窗口与阈值在 `server.go:312`：`authFailWindow = time.Minute`、`authFailAuditEvery = 100`、`authFailMaxKeys = 50_000`。

### 替代方案

- **只挪位置。** 这就是 v1.0.0 到 v1.1.0 之间发生的事，结果是缺陷换了个位置而不是消失。
- **一个限流器，在无租户时退化成按 IP 计数。** 一个桶里混两种身份：已认证流量的配额会被未认证洪泛吃掉，反过来也一样，桶的语义不再可解释。
- **只在反向代理层做防洪。** 阈值不在代码里、不在测试里，换部署环境就变；而且代理看不到审计写入被放大这件事。

### 怎么验证

```bash
go test ./internal/httpapi/ -run 'TestUnauthenticatedFlood|TestAuthFailureAudit' -v -count=1
go test ./tests/ -run 'TestUnauthenticatedFloodGate' -v -count=1
```

实测输出：

```text
status distribution over 400 unauthenticated requests: map[401:5 429:395]
400 rejected requests produced 1 audit events
250 failures collapsed into 3 audit events
```

第一行对应 `TestUnauthenticatedFloodIsRateLimited`：395/400 被防洪闸拦下。第二行对应 `TestUnauthenticatedFloodDoesNotAmplifyIntoWALWrites`：400 次拒绝只落 1 个 WAL 帧。第三行对应 `TestAuthFailureAuditIsAggregatedPerWindow`：把防洪闸开到最大之后，250 次失败仍只落 3 条审计（首条 + 第 100 + 第 200）——这条断言就是为了防止"靠调松防洪闸把放大器放回来"。

## 12.6 错误包络：四个字段各自顶掉了什么

### 问题

如果错误响应只带一个 `message`，客户端能做的只有两件事：把消息显示给人看，或者匹配字符串决定分支。前者够用，后者是埋雷。契约设计要回答的是：**客户端需要的每一个决策依据，分别由哪个字段提供。**

### 原理：一个字段对应一个决策

| 客户端决策 | 依据 | 为什么不能是别的字段 |
|---|---|---|
| 走哪个分支（补凭据 / 降配额 / 重读状态） | `code` | `message` 会改；status 会被改写 |
| 要不要再发一次 | `retryable` | status 会被改写 |
| 给人看什么 | `message` | 只有它是给人写的 |
| 报障时提供什么 | `request_id` | 只有它能定位到日志行 |

四个字段缺一不可，且**互不替代**。这也解释了 `code` 为什么必须是稳定枚举：它是 `switch` 的分支键，加一个取值是一次服务端发版，改一个取值是一次客户端事故。

### 本项目怎么做

`code` 取自 `apperr.E.Code`（`internal/apperr/apperr.go:17`），各 code 对应的状态码与可重试性在 `docs/API.md:16` 的表格里，映射实现在 `internal/apperr/apperr.go:32`。

四个字段的处理各不相同：

- **`code`** —— 枚举取值，客户端据此分派。
- **`message`** —— 各工厂函数写入，随时可改，客户端不得依赖。
- **`retryable`** —— 只由工厂函数**显式**设置。`InternalErr`（`internal/apperr/apperr.go:93`）与 `Unavailable`（`internal/apperr/apperr.go:97`）带 `Retryable: true`，`Invalid`（`internal/apperr/apperr.go:81`）、`Auth`（`internal/apperr/apperr.go:105`）、`ForbiddenErr`（`internal/apperr/apperr.go:109`）不带。
- **`request_id`** —— 从**响应头**读回（`server.go:434`），保证 panic 路径与正常路径拿到同一个 ID。

一个兜底值得注意（`internal/apperr/apperr.go:125`）：

```go
// From extracts an *E, wrapping unknown errors as retryable internal errors.
// Unknown errors are wrapped verbatim so logs keep the root cause. A nil
// error yields nil — callers checking code on a nil error is a bug, but a
// panic in an error helper would take the daemon down with it.
func From(err error) *E {
```

任何没被显式归类成 `apperr.E` 的错误都被包成 `internal` 并标记 `retryable: true`。这个默认值的代价就是下一节讲的那个缺陷。

### 替代方案

- **每种错误独占一个 HTTP 状态码。** 数量不够（光本项目就有 10 个 code），且会被中间设备改写。
- **把 `message` 当作契约。** 最脆弱的一种：措辞不是接口，却是最常被当接口用的字段。

### 怎么验证

```bash
curl -s -w '\nHTTP %{http_code}\n' \
  -H "Authorization: Bearer $ADMIN_KEY" "$BASE/v1/sandboxes?state=bogus"
```

实测输出：

```json
{"error":{"code":"invalid_request","message":"unknown state filter \"bogus\"","request_id":"1279266d9a051470","retryable":false}}
```

```text
HTTP 400
```

四个字段齐备。对应的端到端子测试是 `unknown_json_fields_are_refused` 与 `malformed_json_is_a_governed_400`。

## 12.7 `retryable` 的代价：一次拼写错误引发的重试风暴

### 问题

12.6 里有一行容易读漏的话：任何没被显式归类的错误，都会被 `apperr.From` 包成 `internal` 并标记 `retryable: true`。意思是——**服务端没想清楚的错误，会被告诉客户端"再试一次"**。

客户端拼错了沙箱 ID。这个请求永远不可能成功。但它返回 500 且 `retryable: true`。客户端的重试逻辑照做，无限重试。

### 原理：默认值朝哪边倒，决定了事故的形状

`retryable` 是布尔字段，必须有默认值，两个方向各有各的事故形状：默认 `false`，客户端漏掉一次本可自动恢复的机会，表现为可用性下降；默认 `true`，客户端把一次永久失败放大成无限重试，表现为流量放大。

本项目选了后者，理由是 internal 错误通常与后端瞬时状态有关。但这就把一份责任推给了**每一个可能返回错误的函数**：它必须自己判断"这是客户端输入问题还是服务端状态问题"，并把答案显式写出来。忘了判断，就默认可重试。

这解释了一个反直觉现象：**`retryable` 显式化并没有消除"猜"，它只是把猜的位置从客户端搬到了服务端，并要求服务端每次都写下答案。**

### 本项目怎么做

修复后的边界校验在 `internal/service/errors.go:44`：

```go
// idgenCheck validates that id carries the expected kind prefix.
//
// A malformed id is client input, so it must surface as invalid_request.
// It used to return the bare error from idgen.Parse, which apperr.From wraps
// as an *internal* error — and those are marked retryable. The result was a
// 500 telling the caller to try again for a request that can never succeed:
// a retry storm generated by a typo, reported to the client as the server's
// fault. Found by the end-to-end suite, which asserted the error code rather
// than merely that the request had failed.
func idgenCheck(prefix, id string) error {
	if err := idgen.Parse(prefix, id); err != nil {
		return apperr.Invalid("%v", err)
	}
	return nil
}
```

关键在 `apperr.Invalid("%v", err)`：把 `idgen.Parse` 的裸错误**重新归类**为 `invalid_request`，状态码 400，`retryable` 取零值 `false`。

注意这个修复的**边界**：形状正确但不存在的 ID 仍然是 404 `not_found`（`docs/SECURITY-CONTRACT.md:161`、`CHANGELOG.md:221`）。三类响应因此互不重合：

| 输入 | code | HTTP | retryable |
|---|---|---|---|
| `sbx_missing`（长度不对） | `invalid_request` | 400 | false |
| `sbx_OOOO…`（含非法字符） | `invalid_request` | 400 | false |
| 形状正确但不存在 | `not_found` | 404 | false |

这一条被单独列进对外契约文档 `docs/SECURITY-CONTRACT.md:161`，作为"已修复"的已知缺陷之一，而不是静悄悄改掉。

### 替代方案

- **从状态码推导可重试性。** 状态码会被改写，客户端在不知情时把永久失败当临时失败。
- **全部标 `retryable: true`。** 就是本节开头的事故：一次拼写错误引发重试风暴，并被报成服务端的错。
- **全部标 `retryable: false`。** 反方向的失败：后端短暂不可达时客户端放弃。这也是为什么 `Unavailable`（`internal/apperr/apperr.go:97`）必须显式带 `true`——它明确指向外部依赖，运维能据此判断是引擎挂了而不是程序有 bug。

一个仍然存在的缺口，如实记下：`RateLimit`（`internal/apperr/apperr.go:113`）没有显式设置 `Retryable`，所以 429 的包络里是 `"retryable": false`，而 `docs/API.md:24` 的表格把它标成"是"。实测响应：

```json
{"error":{"code":"rate_limited","message":"rate limit exceeded","request_id":"74054f5323fee83a","retryable":false}}
```

严格按"客户端只按 `retryable` 分支"执行的客户端，永远不会重试被限流的请求——尽管服务端同时给出了 `Retry-After: 1` 头。这正是"契约必须逐字核对"的反面教材：设计意图（`docs/DESIGN.md:249`）、文档表格（`docs/API.md:24`）、构造函数（`internal/apperr/apperr.go:113`）三处有分歧，而只有第三处决定线上行为。另外注意 `E.RetryAfter` 字段（`internal/apperr/apperr.go:62`）只以 HTTP 头形式出现，**不在包络里**。

### 怎么验证

```bash
for bad in sbx_missing sbx_OOOOOOOOOOOOOOOOOOOOOOOOOO; do
  curl -s -H "Authorization: Bearer $ADMIN_KEY" "$BASE/v1/sandboxes/$bad"; echo
done
```

实测输出：

```text
{"error":{"code":"invalid_request","message":"id \"sbx_missing\" has wrong length","request_id":"4fb5f051e3b31647","retryable":false}}
{"error":{"code":"invalid_request","message":"id \"sbx_OOOOOOOOOOOOOOOOOOOOOOOOOO\" contains invalid character 'O'","request_id":"81568434a7a6e244","retryable":false}}
```

第二条特意用了 `O`，因为它不在 Crockford base32 字符集里。端到端断言是 `tests/contract_test.go:166` 的 `a_malformed_id_is_refused_as_client_input`，它对四个畸形 ID 逐个断言，**并且单独断言 `r.Err().Retryable == false`**——只断言状态码抓不到这个缺陷，因为 500 也是"失败"。

## 12.8 分页：为什么用游标而不是 offset

### 问题

`GET /v1/sandboxes` 与 `GET /v1/events` 都是列表，列表就有翻页问题。最直觉的做法是 `?page=3&limit=50`，但它在本项目的使用场景下会同时犯两个错。

### 原理：offset 的两种失效方式

**漂移。** `offset` 是"跳过前 N 条"。你在看第 3 页时前面有 5 条被回收了，第 3 页的内容整体前移：你会看到重复项，并漏掉尾部若干条。对一个"沙箱随时在被创建和回收"的系统来说，这不是边缘情况，是常态。

**深翻页成本。** 执行 `OFFSET 10000` 需要先扫过前 10000 条。当前实现是内存索引，但换成外部存储后这条依然成立。

游标分页换成"从某个位置继续"，它要求两件事：

1. **游标里装的是排序键的值**，不是行号。本项目的事件流按时间戳排序，游标就是一个 RFC3339 时间戳（`internal/store/store.go:710`）。
2. **并发写入下不重不漏**，需要排序键在全序上单调。事件的追加发生在写锁内（`internal/store/store.go:696`），所以内存里的事件数组天然按时间有序（锁的语义见 §3）。

第二条值得说清：如果排序键可能并列（同一毫秒的多个事件），纯时间戳游标会漏数据。稳妥的形状是复合游标 `(timestamp, id)`——先按时间截断，再用 ID 打破并列。当前实现只用时间戳，在单机、写入串行的前提下成立；一旦换成外部存储或多节点写入，就得把 ID 补进游标。

### 本项目怎么做

事件端点的查询参数解析在 `internal/httpapi/handlers.go:532`：

```go
if v := r.URL.Query().Get("limit"); v != "" {
    if n, err := strconv.Atoi(v); err == nil {
        q.Limit = n
    }
}
if v := r.URL.Query().Get("before"); v != "" {
    if ts, err := time.Parse(time.RFC3339, v); err != nil {
        q.Before = ts
    }
}
```

`before` 就是游标：把上一页最后一条的 `ts` 原样传回来，服务端从那里继续往更早的方向取。取数据的循环在 `internal/store/store.go:731`：

```go
out := make([]*domain.AuditEvent, 0, q.Limit)
for i := len(s.events) - 1; i >= 0 && len(out) < q.Limit; i-- {
	ev := s.events[i]
	// ...
	if !ev.TS.Before(q.Before) {
		continue
	}
	out = append(out, cloneEvent(ev))
}
```

倒序遍历 + 严格小于，所以边界那条不会被重复返回；同时因为数组按时间有序，也不会跳过中间任何一条。租户过滤在同一循环里（`store.go:733`），所以游标语义与租户边界互不干扰。

`limit` 还有一处钳制（`internal/store/store.go:724`）：不在 `(0, 1000]` 区间内的值一律回落到 100。**请求里的分页参数是上限而不是偏好**——它决定服务端要物化多少对象。

一处落差需要如实指出：设计文档把沙箱列表写成 `GET /v1/sandboxes?state=&limit=&cursor=`（`docs/DESIGN.md:234`），但 `listSandboxes`（`internal/httpapi/handlers.go:190`）没有读这两个参数，它返回本租户全量；`docs/API.md:32` 记的是实现现状："列表端点返回全量（单机内存有序），`limit` 参数仅对 `/v1/events` 生效"。**文档与实现不一致时，实现是契约**——契约的真身在代码里，文档只是它的索引。

### 替代方案

- **`offset`/`limit`。** 上面两种失效方式。对"内容在被持续增删"的列表，漂移不是偶发而是必然。
- **返回 `total` 与总页数。** 需要一次额外 count，而且在持续写入的流里，这个数字在响应抵达客户端时就已经过期。

### 怎么验证

```bash
# 取一页，把最后一条的 ts 当下一页的游标
curl -s -H "Authorization: Bearer $ADMIN_KEY" "$BASE/v1/events?limit=2" | python3 -m json.tool
TS=$(curl -s -H "Authorization: Bearer $ADMIN_KEY" "$BASE/v1/events?limit=500" \
     | python3 -c 'import json,sys;print(json.load(sys.stdin)["events"][2]["ts"])')
curl -s -H "Authorization: Bearer $ADMIN_KEY" "$BASE/v1/events?limit=500&before=$TS" \
     | python3 -c 'import json,sys;print(len(json.load(sys.stdin)["events"]),"events before", "'$TS'")'
```

实测：第一次返回 2 条（最新在前）；用第 3 条的 `ts` 作游标后返回 0 条——游标是**排他**的。分页行为在 `TestAuditTrail`（`tests/governance_test.go:319`）里被间接覆盖，但 `before` 参数目前没有专门的端到端断言，这是一处可以补的测试缺口。

## 12.9 请求体上限与 slowloris

### 问题

"写"是攻击面，这容易想到；"读"也是，这容易漏。服务端在真正开始处理之前，要先**把对方送来的字节收下来**。收这个动作本身就消耗内存、连接槽、goroutine。如果对方能决定你收多少、收多慢，他就握住了你的资源开关。

### 原理：两种"慢慢来"的攻击

**体积放大。** 一个 1 GiB 的 JSON body 被反序列化后可能占几倍内存；控制面把这些字节缓冲在自己的堆上，所以一个请求字段等于让调用方远程决定进程的内存占用。

**时间拖延（slowloris）。** 攻击者建立连接后极慢地发送请求头（每 10 秒几个字节，永远不发完），把连接与 goroutine 占住。这一招打的是"读请求头"这一步，所以防御必须掐在同一步。

两条要同时设，因为限的是不同阶段：`ReadHeaderTimeout` 限"读完请求头"，`ReadTimeout` 限"读完整个请求（含 body）"。只设后者，慢速发头依然能占住连接（各字段的逐个解释见 §4）。

### 本项目怎么做

超时与头部上限集中在 `cmd/sandboxd/main.go:109`：

```go
srv := &http.Server{
	Addr:              cfg.Listen,
	Handler:           api.Handler(),
	ReadHeaderTimeout: 10 * time.Second,
	// exec streams are long-lived; per-request contexts govern them
	WriteTimeout:   0,
	IdleTimeout:    120 * time.Second,
	MaxHeaderBytes: 1 << 20,
}
```

`WriteTimeout: 0` 通常是个危险信号，但理由就写在上面那行注释里：exec 是长流式的，一次执行可能持续几分钟，写超时会把它们全部切断。补偿手段是让每个请求自己的 context 管生命周期，而不是靠连接级写超时。

请求体上限的默认值在 `internal/config/config.go:214`：`MaxRequestBody: 1 << 20`（1 MiB）、`MaxFileBytes: 100 << 20`（100 MiB），键分别是 `max_request_body_bytes` 与 `max_file_bytes`（`internal/config/config.go:367`）。拦截发生在 `bodyLimit`（`internal/httpapi/server.go:209`），用 `http.MaxBytesReader` 而不是先读再判断：

```go
r.Body = http.MaxBytesReader(w, r.Body, limit)
```

超限之后不是在中间件里拒绝，而是让 `decode`（`internal/httpapi/server.go:454`）在读的时候失败，并归类成 `invalid_request`：

```go
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apperr.Invalid("invalid JSON body: %v", err)
	}
	return nil
}
```

`DisallowUnknownFields()` 是另一个契约决定：**接受未知字段并保持沉默，是让客户端把一个拼错的字段用上一年的方式**。比如 `"priviliged": true` 会被静默忽略，调用方以为自己关掉了某项隔离，实际从未生效（对应子测试 `unknown_json_fields_are_refused`）。

未认证流量的防洪阈值单独配置（`sandboxd.conf.example:79`，键 `preauth.per_second` / `preauth.burst`），并且刻意不读 `X-Forwarded-For`——代码里的说明写清了理由（`internal/httpapi/server.go:256`）：

```text
// It deliberately reads ONLY RemoteAddr. X-Forwarded-For is attacker-supplied
// unless a trusted proxy rewrites it, so honouring it by default would let one
// host rotate a header value into an unlimited number of buckets — turning the
// flood gate into a memory-growth vector instead of a defence.
```

一句通用规则：**任何用于安全决策的字段，都必须是无法由请求方伪造的。** 这条和 §6 讲的"配置不是证据"是同一种思维方式。

### 替代方案

- **在反向代理层设 `client_max_body_size`。** 阈值不在代码里、不在测试里，换环境就变；而且代理挡不住慢速发头。
- **先 `io.ReadAll` 再检查长度。** 内存已经吃进去了——检查发生在损害之后。
- **给 `WriteTimeout` 设一个正数。** 长流式 exec 会在超时点被切断，而这类请求恰恰是产品的主要工作负载。

### 怎么验证

```bash
# 超过 1 MiB 的请求体被拒，且归类为 invalid_request
python3 -c 'import json;print(json.dumps({"base_image":"alpine:3.22",
  "spec":{"cpu_millicores":100,"memory_mib":64,"timeout_seconds":60},
  "metadata":{"pad":"A"*(2*1024*1024)}}))' > /tmp/big.json
curl -s -w '\nHTTP %{http_code}\n' -X POST -H "Authorization: Bearer $TENANT_KEY" \
  -H 'Content-Type: application/json' --data-binary @/tmp/big.json "$BASE/v1/sandboxes?wait=false"
```

实测输出：

```json
{"error":{"code":"invalid_request","message":"invalid JSON body: http: request body too large","request_id":"348be89e7ea7d432","retryable":false}}
```

```text
HTTP 400
```

注意这里必须用**租户 Key**：用管理员 Key 会在 `resolveTenant`（`internal/httpapi/handlers.go:120`）处先拿到 403，`decode` 根本不会执行——这本身就是 12.4 那条"谁先拒绝"判据的一个活例子。

## 12.10 健康检查三兄弟：三个端点为什么不能合并

### 问题

`/healthz`、`/readyz`、`/metrics` 看起来都在回答"它还活着吗"。合并成一个不是更简单吗？不能——它们回答的是三个**时序不同**的问题，而且给三类不同的调用方用。

### 原理：存活 ≠ 就绪 ≠ 可观测

区别不在"检查多少项"，而在"失败时希望调用方做什么"：

| 端点 | 回答的问题 | 失败时的正确动作 |
|---|---|---|
| `/healthz` | 进程还在跑吗 | 重启它 |
| `/readyz` | 现在能接活吗 | 先别给它流量 |
| `/metrics` | 它一直表现如何 | 人来看 |

一个具体反例：后端（容器运行时）短暂不可达时，进程完全健康，只是暂时不能接活。如果只暴露一个 `/health`，它必然同时用于存活探测与就绪探测，那么这次抖动会让编排系统**重启**一个其实只需要**摘流量**的进程。两类故障的动作被压成一个信号，就必须选一边错。

另一条硬约束：**不鉴权的端点会被高频调用，所以它必须"构造上廉价"**。

### 本项目怎么做

注册点在 `internal/httpapi/server.go:54`：

```go
// Liveness and readiness stay unauthenticated so a load balancer can probe
// them. They are cheap by construction: no store access beyond a mutex, no
// audit write, no fsync.
mux.HandleFunc("GET /healthz", s.health)
mux.HandleFunc("GET /readyz", s.ready)
```

`health` 只返回一个常量（`internal/httpapi/handlers.go:28`），不碰存储也不碰后端。`ready` 会真的探后端与存储（`handlers.go:38`），所以它能回答"能接活吗"。

管理面端点不在那个组里，注释写明了原因（`server.go:60`）：

```text
// /metrics is NOT in that group. In v1.1.0 it was registered with
// mux.HandleFunc, which bypasses s.chain() entirely — so it answered 200
// to anyone, on the same port as the control plane. Operational data about
// every tenant belongs behind admin auth.
```

```text
// The self-check inventory is admin-only for the same reason /metrics is:
// it enumerates the controls actually in force, which is reconnaissance
// for anyone planning an escape.
```

第二句里有一个反直觉的安全原则：**你的防御清单本身就是敏感信息**。所以"隔离档位"这个结论对外公开（客户最需要独立核对的就是它），而"完整控制项清单"藏在 admin 端点后面（§16.3 展开这个取舍）。

三个端点的**契约**（只讲鉴权与返回形状，语义归 §16.3）：

| 端点 | 鉴权 | 响应 |
|---|---|---|
| `/healthz` | 无 | `{"status":"ok"}` |
| `/readyz` | 无 | `status`、`backend`、`isolation`、`isolation_note`、`isolation_verified`、`isolation_checked_at`、`isolation_userns`；失败时另有 `detail` 与 `isolation_failures` |
| `/metrics` | admin | 文本（格式见 §13.4） |

`/readyz` 的隔离字段**不鉴权且始终存在**，自检尚未运行时 `isolation_verified` 为 `false` 并附说明，不会被渲染成成功。

### 替代方案

- **三个端点合并成一个 `/health`。** 上面那个"重启还是摘流量"的反例。合并等于强迫调用方用一个信号同时做两类决策。
- **给 `/readyz` 也加鉴权。** 负载均衡探不了；若为探活而给 LB 配一个租户 Key，等于把一个凭据放进了基础设施配置。

### 怎么验证

```bash
curl -s $BASE/healthz; echo
curl -s $BASE/readyz | python3 -m json.tool
curl -s -o /dev/null -w '%{http_code}\n' $BASE/metrics
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $ADMIN_KEY" $BASE/metrics
```

实测输出：

```text
{"status":"ok"}
```

```json
{
    "backend": "oci",
    "isolation": "container-shared-kernel",
    "isolation_checked_at": "2026-10-09T20:22:08.302143+08:00",
    "isolation_note": "sandbox shares this host's kernel: suitable for trusted or semi-trusted workloads, NOT for running arbitrary untrusted code",
    "isolation_userns": "off",
    "isolation_verified": true,
    "status": "ready"
}
```

```text
401
200
```

端到端断言：`health_and_readiness_are_unauthenticated_but_honest`（断言匿名可探活，且 `isolation` 非空、`isolation_verified` 为真）与 `the_isolation_inventory_stays_admin_only`（断言匿名访问 `/v1/isolation` 是 **401 而非 403**）。`TestHealthAndReadyStayUnauthenticated`（`internal/httpapi/regression_test.go:250`）在更低层次再钉一次。

## 12.11 admin scope：认证通过不等于有权限

### 问题

一个租户 Key 通过了认证——一把真实的、未过期的、未吊销的 Key。那么它能不能读 `/metrics`？认证只回答"你是谁"，没回答"你能不能做这件事"。这是两个问题，需要两次判断。

### 原理：认证与授权是两个问题

| 阶段 | 问题 | 失败码 |
|---|---|---|
| 认证 authentication | 你是谁 | 401 |
| 授权 authorization | 你能不能做这件事 | 403 |

把两者混在一起，典型后果是"能用任何一把合法 Key 访问管理面"。这个后果在 v1.1.0 真实发生过，原因不是逻辑写错，而是**注册端点时用错了函数**：`mux.HandleFunc`（不带中间件）与 `s.chain(...)`（带中间件）在代码里只差几个字符，一个手滑就绕过了整条链。

### 本项目怎么做

`chainAdmin`（`internal/httpapi/server.go:141`）让管理端点走完整链，再加一层 scope 检查：

```go
func (s *Server) chainAdmin(h http.HandlerFunc) http.Handler {
	return s.chain(domain.GroupAdmin, s.requireAdmin(h))
}
```

`requireAdmin`（`server.go:145`）在最内层判断 scope：

```go
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if k := keyFrom(r); k != nil && k.Scope != domain.ScopeAdmin {
			s.writeError(w, r, apperr.ForbiddenErr("this endpoint requires an admin-scoped key"))
	}
}
```

它必须在 `authenticate` 之内，因为要读 `keyFrom(r)`——而 `keyFrom` 的来源是 `authenticate` 塞进 context 的对象（`server.go:420`）。这就是 12.4 那条判据的实例：**需要下一层产出的东西，就得待在下一层里面。**

修复方式值得学：不是"加强代码审查"，而是让管理端点的注册路径只有一条——`chainAdmin`。**当"安全的写法"和"不安全的写法"只差几个字符时，漏掉只是时间问题；正确的做法是让不安全的写法根本不存在。**

另一条相关契约：管理员操作租户资源时**必须**点名目标租户（`internal/httpapi/handlers.go:120`）——不带 `X-Tenant-Id` 的写操作直接 403。一次支持操作不该变成一次跨租户写入。

### 替代方案

- **用布尔 `is_admin`。** 无法表达第三种角色，将来要加"只读运维"就得改数据模型（§11.2）。
- **按路径前缀硬编码白名单。** 新端点容易漏注册——这正是 `/metrics` 出事的方式。注册路径只有一条时，漏注册不可能发生。
- **靠"admin key 不发给租户"兜底。** 凭据会泄漏；单点的信任假设没有纵深。403 是第二道门，不是第一道的替代品。

### 怎么验证

```bash
go test ./tests/ -run 'TestAuthenticationAndScope' -v -count=1
```

其中 `tenant_keys_cannot_reach_admin_surfaces` 对 `/v1/tenants`、`/metrics`、`/v1/isolation` 三个路径逐个断言 **403**（而不是 401）——它证明"已认证但越权"这条独立分支存在且可达。`an_admin_key_must_name_the_tenant_it_acts_on` 则断言管理员不带 `X-Tenant-Id` 写租户资源时是 403、带上之后是 201。

## 12.12 契约的可测试性：把断言写在黑盒层

### 问题

12.5 里那个限流缺陷有一个刺眼的属性：**限流器本身的单测是全绿的**。坏的不是算法，是接线，而接线只存在于"从外到内跑一遍"的那一刻。所以契约设计必须连带回答：这些承诺由谁来守？答案是只有黑盒层的断言。

### 原理：单测覆盖算法，黑盒覆盖接线

两类测试抓两类缺陷，互不替代：

| 层次 | 抓什么 | 抓不到什么 |
|---|---|---|
| 单元 | 令牌桶补充速率、ID 校验规则 | 中间件有没有被装进链 |
| 黑盒端到端 | 状态码、`code`、响应头、顺序 | 内部实现的边界分支 |

第二条原则比第一条更容易被忽略：**断言必须落在语义上，不能落在"是否失败"上**。`a_malformed_id_is_refused_as_client_input` 之所以能抓到那个缺陷，是因为它断言 `code == invalid_request` 且 `retryable == false`；如果只断言"请求失败了"，500 也会让它通过。

### 本项目怎么做

端到端套件的文件头把这条原则写在最前面（`tests/contract_test.go:3`）：

```text
// The public API contract. Clients are documented to branch on the error
// Code, never on the status or the message, so every refusal asserted here
// checks the code — and MustErr additionally checks that the code travelled
// on the transport its own mapping table says it should.
```

"**每个拒绝都检查 code**"，并且额外校验 code 与状态码的映射一致。包内回归套件的文件头写的是因果（`internal/httpapi/regression_test.go:6`，即 12.5 引用的那次洪泛）。两个套件跑的是同一条链的两个尺度：`internal/httpapi/regression_test.go` 直接构造 `Server` 并用 `httptest` 打进去，快且能精确控制配置；`tests/contract_test.go` 起一个真进程、用带类型的客户端走真实 HTTP，慢但覆盖进程边界。

`TestPreAuthGateRunsBeforeAuthentication`（`internal/httpapi/regression_test.go:314`）是这一节最好的例子，注释直说了意图：

```text
// Guards the assumption the whole design rests on: the limiter must be reachable
// from the unauthenticated path. If someone reorders `chain`, the pre-auth gate
// silently stops protecting anything, and only this assertion notices.
```

"**如果有人重排 `chain`，防洪闸会静悄悄地停止保护任何东西，只有这条断言会注意到。**"顺序写在代码里，但它是否还成立，只有测试会告诉你。

### 替代方案

- **只测 Limiter。** 就是 v1.0.0 的情形：算法全绿，接线是坏的。
- **只断言"请求失败"。** 不区分 `code` 的断言在重构中存活率很高，也因此在缺陷面前毫无反应——它什么都没钉住。
- **用 JSON Schema 校验响应。** 能抓形状（字段缺失、类型错误），抓不到语义（`retryable` 该不该为 true）。

### 怎么验证

```bash
go test ./internal/httpapi/ -run 'TestRateLimitOverHTTP' -v -count=1
go test ./tests/ -run 'TestSecretsNeverReachTheLog' -v -count=1
```

实测输出：

```text
    httpapi_test.go:69: status distribution: map[200:5 429:45]
--- PASS: TestRateLimitOverHTTP (0.01s)
--- PASS: TestSecretsNeverReachTheLog (2.40s)
```

第一行是租户级限流：50 次并发 GET、burst 为 40，于是 5 个 200、45 个 429，证明整条链（认证 → 限流）确实在工作。第二条覆盖另一类承诺：正常请求与被拒绝的请求都不会把凭据明文写进守护进程输出。

---

### 本章检验

1. `chain`（`internal/httpapi/server.go:127`）里 `accessLog` 在 `recover` 之外。如果交换这两个位置，会出现什么具体的用户可见后果？为什么这个后果恰好落在"最需要记录的那类请求"上？

2. `preAuthLimit` 在 `bodyLimit` 之外，而 `bodyLimit` 又在 `authenticate` 之外。请说明：为什么把 `bodyLimit` 放在 `preAuthLimit` 之内**不会**导致"洪水先把内存吃掉"（提示：`http.MaxBytesReader` 的时机）？以及为什么把 `bodyLimit` 放在 `authenticate` 之外仍然有意义？

3. `apperr.From`（`internal/apperr/apperr.go:125`）把未归类错误包成 `internal` 且 `retryable: true`。请沿着这条默认值的因果链走一遍：畸形 ID 是如何变成一次无限重试风暴的？为什么把 `idgenCheck` 改成返回 `apperr.Invalid` 就修复了它？

4. 本章指出 429 的包络里是 `"retryable": false`，而 `docs/API.md:24` 的表格标的是"是"。请以"客户端只按 `retryable` 分支"为前提，推演严格遵守该规则的客户端在遭遇限流时的行为，并说明应当修哪一侧（代码还是文档）、为什么。

5. `/healthz` 与 `/readyz` 不鉴权，理由是"负载均衡需要能探"。请说明：为什么这个理由同时强制要求它们"构造上廉价"？如果一个不鉴权的端点会触发 fsync，攻击者能用它做什么？本项目在 401 路径上是如何补偿的？

**动手任务**：复制 `sandboxd.conf.example` 起一个本地守护进程，把 `preauth.burst` 调小，然后依次执行：

```bash
# 1. 匿名洪泛：观察 401 与 429 的分布
for i in $(seq 1 12); do
  curl -s --noproxy '*' -o /dev/null -w '%{http_code} ' $BASE/v1/sandboxes
done; echo

# 2. 畸形 ID：确认 400 + invalid_request + retryable:false
curl -s --noproxy '*' -w ' <- HTTP %{http_code}\n' \
  -H "Authorization: Bearer $ADMIN_KEY" $BASE/v1/sandboxes/sbx_missing

# 3. 请求 ID：短 ID 回显、超长 ID 被替换
curl -s --noproxy '*' -D - -o /dev/null -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'X-Request-Id: abc' $BASE/v1/sandboxes | grep -i x-request-id
curl -s --noproxy '*' -D - -o /dev/null -H "Authorization: Bearer $ADMIN_KEY" \
  -H "X-Request-Id: $(python3 -c 'print("A"*200)')" $BASE/v1/sandboxes | grep -i x-request-id

# 4. 管理面：匿名 401、租户 Key 403、admin Key 200
for k in "" "$TENANT_KEY" "$ADMIN_KEY"; do
  curl -s --noproxy '*' -o /dev/null -w '%{http_code} ' \
    -H "Authorization: Bearer $k" $BASE/metrics
done; echo
```

对照本章的实测输出自查：第 1 步应在前 `burst` 次为 401、之后全为 429；第 3 步第二条请求应返回 16 个十六进制字符；第 4 步应依次得到 `401 403 200`。然后读 `internal/httpapi/regression_test.go:314` 的注释，确认"顺序约束由测试守护"这句话在本项目里具体是怎么落地的。


# §13 可观测性与运维

前十二章讲的是"它怎么工作"。这一章讲的是另一件事——**它跑起来之后，你怎么知道它是好的**。

开发阶段这个问题不存在：敲一条命令就能看到输出。到了生产，进程是一个黑盒，你只能看到它吐出来的三类东西：**日志、指标、端点状态**。它们的质量决定了出事时的反应速度，而反应速度取决于一个更前置的问题——**它们说的是不是事实**。本项目的运维设计，就是围绕"让一个不成立的声明说不出口"来做的。

## 13.1 启动顺序即设计

### 问题

把启动写成一串"初始化"是最容易出事的地方。考虑一个具体时序：某次发布后容器运行时还没就绪，而 daemon 的监听端口已经开了。负载均衡探测到进程在应答，开始转发流量；每个创建请求都撞上后端连接失败，daemon 把沙箱记为 `failed`。

更麻烦的是：**没有人能从外部区分"后端挂了"和"daemon 自己还在启动"**。根因不是哪个函数写错了，而是启动这件事**没有顺序语义**——谁都可以先跑，于是"服务已可用"这个状态变得不可判定。

### 原理

一个有序的启动序列，价值在于把"部分初始化"这个状态消灭掉：进程在任一时刻要么**尚未监听**（外部观察不到），要么**已完成全部前置验证**（可以安全接流量）。中间状态依然存在，但不可被外部观察到。

判定一条顺序是否被锁死，标准很简单：**每一步的前置条件是不是由上一步提供的**。如果是，顺序就改不动；如果不是，顺序就是随意的。

### 本项目怎么做

`run()`（`cmd/sandboxd/main.go:56`）不到一百行，按顺序是九步：

1. **加载配置**（`cmd/sandboxd/main.go:57`）——它决定后面一切，加载失败直接返回。
2. **构造后端并探活**（`cmd/sandboxd/main.go:64`）——`buildBackend` 内部先 `Ping` 再 `Init`（`cmd/sandboxd/main.go:210`、`cmd/sandboxd/main.go:218`）。
3. **打开存储**（`cmd/sandboxd/main.go:70`）——WAL 恢复在这一步完成。
4. **报告恢复结果**（`cmd/sandboxd/main.go:82`）。
5. **建指标注册表与 service**（`cmd/sandboxd/main.go:88`）。
6. **admin 引导**（`cmd/sandboxd/main.go:91`）与**模板播种**（`cmd/sandboxd/main.go:94`）。
7. **隔离自检**（`cmd/sandboxd/main.go:98`）。
8. **启动后台循环**（`cmd/sandboxd/main.go:102`）与**启动对账**（`cmd/sandboxd/main.go:105`）。
9. **开始监听**（`cmd/sandboxd/main.go:119`）。

每一步为什么在这个位置，挑关键的几条说：

- **后端探活在存储之前**：不是硬约束，但先失败就不用动磁盘。打开存储会创建锁文件、可能触发修复；一个注定要退出的进程不该先去修改数据目录。
- **模板播种在 admin 引导之后**：播种产生的公共模板要由租户配额约束（见 §11）。更重要的是**可诊断性**——启动日志里"admin key"永远排在"seeded template"之前，看日志的人不需要猜。
- **隔离自检在后台循环之前**：自检会真的起一个容器，而清扫器一旦运行就可能扫到"有容器、没记录"的对象。先自检、后循环，保证循环启动时自检容器已被回收（`internal/runtime/oci/isolation.go:100`）。
- **启动对账与监听在最后**：见 13.2。

### 替代方案

**方案一：用依赖注入容器（fx、wire 一类）编排初始化。** 好处是顺序由类型图推导，改错了编译期就报。代价是要引入依赖（与零依赖冲突，见 §16.1），而且**它保证的是"构造顺序"，不是"验证顺序"**——"要求 microVM 而本机不具备就拒绝启动"这种决策，类型图表达不了。

**方案二：全部并行初始化，靠健康检查兜底。** 这是最常见的做法，也正是本节开头那个故障的来源。它的隐含假设是"健康检查能覆盖所有失败模式"，而隔离自检恰恰证明这个假设不成立：一个配置正确、但引擎没有兑现的设置，健康检查只会把配置回读一遍（机制见 §6）。

### 怎么验证

通读 `run()`（`cmd/sandboxd/main.go:56-147`），给每一步写出它的前置条件与它提供给下一步的东西。写不出来，说明这一步的位置是随意的。端到端由 `TestStartupIsMeasuredNotJustTimedOut`（`tests/commercial_test.go:230`）覆盖：它断言 ready 之后的第一个建沙箱请求返回 201 且状态是 `running`，而不只是"端口通了"。

## 13.2 监听器为什么最后开

### 问题

假设把监听挪到启动对账之前。那么存在一个窗口：端口在应答，而上一轮进程留下的"半截记录"还没被收敛。这个窗口里的第一个 `GET /v1/sandboxes/{id}` 可能读到一个 `provisioning` 状态——而那个沙箱的容器早就创建好了，只是没人去核对。客户端据此判断"还在创建中"，于是继续等；而它等待的事件永远不会到来。

故障不表现为错误码，表现为**一个正确的响应，描述了一个已经不存在的状态**。

### 原理

"就绪"在工程上不是一个布尔标志，而是一句**进程必须能兑现的承诺**。要兑现它，监听之前必须完成三件性质不同的事：

- **依赖可达性**：后端能被 ping 通（否则创建必然失败）。
- **安全属性已被证明**：隔离档位不是"我请求了"，而是"我从里面读到了"（见 §6）。
- **状态已被解释**：上一轮遗留的过渡态记录已经与后端真相对齐。

三者缺一，端口就不应该开。这里和第 12 章的分工是：`/readyz` 的**契约**（哪些鉴权、返回什么字段）见 §12.8；本章讲的是**为什么这些前置验证必须在端口打开之前完成**——ready 若先于它们成立，端点就在说一句它还兑不了现的话。

### 本项目怎么做

三处证据，都在注释里：

**自检在监听之前**（`cmd/sandboxd/main.go:96-97`）：

```go
// Prove the isolation before serving the first request: a claim that only
// appears in the log once traffic is flowing is a claim nobody checked.
```

**对账在监听之前**（`cmd/sandboxd/main.go:103-105`）：

```go
svc.StartLoops()
// converge whatever the previous process left mid-flight, before the
// first request can observe a stale state; at boot nothing is in flight
svc.ReconcileAtBoot(ctx)
```

注意 `StartLoops` 在 `ReconcileAtBoot` **之前**：对账会改状态、会打后端，而它依赖的清扫器与压实器要先就位；同时周期对账的首次触发是等一个 `ReconcileEvery` 之后，所以启动这一趟必须显式跑（两者差别见 §10.4）。

**监听在最后**（`cmd/sandboxd/main.go:119-128`）：监听放进一个 goroutine，错误送进 `errCh`，主流程继续往下注册信号。

自检跑不动时也写清楚了（`cmd/sandboxd/main.go:253-261`）：自检无法执行而运维配了 `userns = require`，则**拒绝启动**；否则降级为一条 warning，且这条 warning 明说"此刻 `/readyz` 上的是配置，不是已验证的事实"。**"没跑过"和"跑过并失败"不允许互相冒充。**

### 替代方案

**方案一：先监听，用 `/readyz` 挡住流量。** 这是行业常规，但它要求负载均衡**正确配置了就绪探测**。一次误配（探测 `/healthz` 而不是 `/readyz`）就足以让流量进入未验证的节点。把监听放在最后，等于不把这件事交给部署者的配置——**依赖人的正确配置来保安全的控制，不算控制。**

**方案二：进程内维护一个 ready 布尔量，未就绪时返回 503。** 能做到，但 503 与"端口没开"对负载均衡是两回事：前者计入实例健康统计，后者是明确的连接拒绝。而且它还引入了新的状态位需要同步。

**方案三：靠 systemd 的 `Type=notify` 通知就绪。** 它只解决"systemd 何时认为它起来了"，不解决"节点是否已经验证过自己"。

### 怎么验证

```bash
SBX_REQUIRE_DOCKER=1 go test ./tests/ -run TestCrashRecovery -v -count=1
```

`tests/resilience_test.go:19` 的第一个子场景（`a_sigkill_during_create_converges_instead_of_hanging`，:23）在 `wait=false` 返回的创建途中 `kill -9`，重启后断言记录**不再停留在 `provisioning`**。这正是"对账必须先于服务"的可执行证明：如果监听先开、对账后跑，就会出现一个能读到过渡态的窗口。

## 13.3 配置系统：TOML-lite、env 覆盖与坏值拒启

### 问题

运维在部署脚本里写下 `SBXD_MAX_EXEC_OUTPUT_BYTES=100MiB`。如果程序解析失败、回退到默认 8 MiB，那么"我以为的上限"和"实际生效的上限"就分开了。而系统对这件事说的唯一一句话是：**启动成功**。

对安全与容量类配置，静默回退尤其危险，因为它把"我的意图"和"实际生效"分开——而这两者分离正是本项目从头到尾反对的事（§6 讲"配置不是证据"，这里是同一原则的另一面：**配置错了要出声，不要悄悄换一个**）。

### 原理

配置层的职责不是"尽量让进程跑起来"，而是**在启动时把运维的意图与系统的能力对齐**。它有三件事，失败的语义各不相同：

- **解析**：把文本变成类型化的值。失败意味着意图无法表达——必须出声，因为没有任何"次优选择"是可信的。
- **覆盖**：文件 < 环境 < 命令行。这是常规分层，关键在于**空值算不算提供**。
- **校验**：值之间有跨字段关系（平台上限不能低于默认规格），这类错误只有在全部加载完才查得出来。

### 本项目怎么做

**为什么自己写一个 TOML-lite 解析器。** `internal/config/config.go:1` 的包注释说格式就是 `key = value`。实现是 `bufio.Scanner` 逐行加一次 `strings.IndexByte(text, '=')`（`internal/config/config.go:253-278`），去掉 `#` 注释与空行，剥掉值两侧的引号。整个文件里没有反射、没有结构体标签。

理由不是"TOML 不好"，而是**需求与供给的错配**：这里需要的只是二十来个 flat key，而完整的 TOML 库为了支持数组、内联表、多行字符串、日期类型会带进一棵依赖树。当格式需求只有真正格式的百分之五时，自己写那百分之五比引入百分之百的实现便宜（依赖成本见 §16.1）。

**未知键是硬错误**（`internal/config/config.go:392-394`）：

```go
default:
	return fmt.Errorf("unknown key %q", key)
```

这一条比看上去重要：拼错的键如果静默忽略，运维会以为自己配了。实测：

```bash
$ printf 'listen = "127.0.0.1:18099"\nbogus_key = 1\n' > /tmp/probe.conf
$ sandboxd -config /tmp/probe.conf
... level=fatal msg="sandboxd exited: config: invalid_request: config /tmp/probe.conf:2: unknown key \"bogus_key\""
```

注意错误消息里带了**行号**，这是逐行扫描顺手就能拿到的东西，而在通用解析器里往往拿不到。

**坏值拒启**（`internal/config/config.go:398-400`）：

```go
// applyEnv applies SBXD_* overrides. A malformed value is an error, never a
// silent fallback to the default: an operator who typo'd a limit must find out
// at startup, not discover months later that the limit was never in effect.
```

实测：

```bash
$ SBXD_MAX_EXEC_OUTPUT_BYTES=8MiB sandboxd
... level=fatal msg="sandboxd exited: config: invalid_request: SBXD_MAX_EXEC_OUTPUT_BYTES=\"8MiB\" is not a number"
```

**空值视为未提供。** `env()` 辅助函数（`internal/config/config.go:402-405`）要求 `ok && v != ""`。编排系统常把环境变量设成空串来"清空"一个值，而这里空串等于没设——布尔键如 `SBXD_AUTO_RESUME=` 不会覆盖文件里的值；字符串键如 `listen` 被设成空串时保留原值，由 `validate`（`internal/config/config.go:452`）兜住。

**三态而不是布尔。** `isolation` 与 `userns` 各是三值（`internal/config/config.go:150-172`）。为什么不是 bool？因为"没配"和"显式关掉"是两种意图。`userns` 的注释（`internal/config/config.go:58-61`）说得直白：*"因为 'off' 不等于 '没要求'"*。

**跨字段校验**（`internal/config/config.go:461-463`）：平台 CPU 上限低于默认规格时拒绝启动。这类关系错误在单个 `set()` 里查不出来，只能在 `validate()` 里查。

**接受风险要留痕。** 三种配置**允许**启动，但会往 stderr 打 WARNING：`wal_repair = salvage`（`internal/config/config.go:485-489`）、`userns = off`（`internal/config/config.go:496-500`）、`enforce_disk_quota = false`（`internal/config/config.go:505-509`）。它们不是被禁止的，而是被标记过的——**"你选择接受这个风险"这件事必须出现在日志里，而不是只写在配置文件的注释里**。

### 替代方案

**用成熟配置库（viper 加 TOML/YAML）。** 代价是依赖树，以及"隐式来源"——这类库会同时读文件、环境、远程配置中心，出问题时"这个值到底从哪来"本身需要调试。手写的解析器行为可以在二十五行内读完。

**以命令行参数为主。** 参数在 `ps` 输出里可见，而配置里有 `bootstrap_admin_key` 这类字段。所以命令行只留 `-config` 一个（`cmd/sandboxd/main.go:42`）。

**坏值回退默认并打 warning。** 就是本节"问题"里描述的那个失败。唯一支持理由是可用性，但启动失败是 fail-fast，代价几秒钟；静默降级要几个月后才被发现。

### 怎么验证

```bash
go test ./internal/config/ -run TestMalformedNumericEnvIsAnError -v -count=1
```

`internal/config/config_test.go:210` 断言非数值必须让 `Load` 返回错误，且错误消息要点名变量。同一文件的 `TestIsolationCanBeRequiredFromFileAndEnv`（:152）与 `TestUsernsCanBeRequiredFromFileAndEnv`（:116）验证三态开关能从两条路径表达——**一个只能在配置文件里写的"要求"，在容器化部署里等于不存在**。

## 13.4 指标：手写一个 Prometheus 文本导出器

### 问题

零依赖（见 §16.1）意味着不能用官方 Prometheus 客户端库。而"手写一个指标导出器"听起来只是字符串拼接——直到某个标签值里出现一个换行，整个 `/metrics` 抓取变成格式错误，**所有序列一起消失**。

### 原理

Prometheus 文本格式（version 0.0.4）对导出器有四条硬约束：

1. 每个指标名**只能**出现一行 `# HELP` 和一行 `# TYPE`，且必须在所有样本之前。
2. 样本行形如 `name{label="value",...} value`。
3. 标签值里的 `"`、`\`、换行必须转义；未转义的换行会把一行拆成两行，破坏整个抓取。
4. 同一个 `name + 标签集` 必须是同一条序列——这需要一个把标签集映射为唯一键的函数，且**不同的标签集不能映到同一个键**。

### 本项目怎么做

**先说清楚不做什么**（`internal/metrics/metrics.go:1-3`）：

```go
// Package metrics is a tiny Prometheus-text registry, stdlib-only.
// It deliberately does not attempt full prometheus client parity: the
// daemon needs counters, gauges, and histograms-by-buckets, labeled.
```

**标签契约是扁平的 name/value 对序列**（`internal/metrics/metrics.go:7-11`）：

```go
//	[]string{"status", "200"}                  // one label
//	[]string{"action", "sandbox.stop"}         // one label
//	[]string{"method", "GET", "status", "200"} // two labels
```

**一个真实的缺陷。** `internal/metrics/metrics.go:13-19` 记录了 v1.1.1 修掉的问题：三个生产调用点传的是 `[]string{"status"}`——只有标签名、没有值——旧实现按对遍历后整体丢弃，于是序列渲染成无标签，那个维度就凭空消失了。仓库把它列为 PoC-7（`poc/README.md:27`，复现脚本 `poc/probe_live.sh:119`），修复记录见 `CHANGELOG.md:410-414`。

这个缺陷的恶劣程度在于：**它不报错，只是让指标少一个维度**。你以为在按状态码统计，实际在统计一个没有状态码的总量；等你想按状态码排查时，才发现数据从来没记过。

修复后的三个调用点分别是 `internal/httpapi/server.go:195`（`{"status", strconv.Itoa(sw.status)}`）、`internal/service/service.go:393`（`{"source", source}`）、`internal/service/lifecycle.go:101`（`{"action", action}`）。

**修复方式本身更值得学。** 不是简单地拒绝调用，而是把畸形标签集**可见地**渲染出来（`internal/metrics/metrics.go:143-147`）：

```go
if len(labels)%2 == 1 {
	// A trailing name with no value. Render it as visibly broken rather
	// than dropping it: silence is what let the original bug live.
	write(labels[len(labels)-1], "<missing>")
}
```

同时 `noteMalformedLocked`（`internal/metrics/metrics.go:98-105`）给 `sandboxd_metrics_malformed_calls_total`（`internal/metrics/metrics.go:45`）加一。**让错误出现在你看得见的地方，而不是消失。** 这个指标刻意不带标签，所以记录它本身不会递归（`internal/metrics/metrics.go:41-44` 的注释点明了这一点）。

**键碰撞**（`internal/metrics/metrics.go:59-65`）：

```go
// key identifies a series. It is built from the same canonical rendering the
// exposition uses, so two label sets that render identically cannot collide
// (the old raw join of names and values could: ["a","1,2"] and ["a,1","2"]
// both produced `a,1,2`).
func key(name string, labels labelSet) string {
	return name + "{" + renderLabels(labels) + "}"
}
```

修法是让键使用与渲染**同一个**规范化形式：渲染一致的必然键一致，从而消除了"两条不同序列互相覆盖"的可能。

**转义**（`internal/metrics/metrics.go:107-115`、`internal/metrics/metrics.go:118-123`）：标签值与 HELP 文本分别处理，且都先判断是否需要转义再走 replacer。注释给出的理由是：一个含引号或换行的标签值（租户提供的字符串、路径）会破坏**整个 registry** 的抓取，而不止那一条。

**渲染**（`internal/metrics/metrics.go:166-180`）：用 `seenHelp` 保证 HELP/TYPE 只输出一次，用 `order` 切片保证输出顺序稳定——Go 遍历 map 的顺序不确定，而稳定的顺序让抓取结果可以被 diff。

**基数纪律。** `noteAuthFailure`（`internal/httpapi/server.go:351-355`）的注释解释了为什么认证失败指标**刻意不带标签**：客户端 IP 是攻击者可选的字符串，把它作为标签值等于允许任何人无上限地撑大注册表。写新指标时的第一问应该是：**这个标签值的取值集合，是不是我控制的？**

### 替代方案

**用官方 client_golang。** 代价是依赖（见 §16.1）；收益是直方图分位数、进程与 Go 运行时指标开箱可用。本项目选择只实现用到的部分，并把"没有分位数"这件事写进 13.10。

**只导出无标签总量。** 实现最省事，但"5xx 有几个""池命中率多少"全部无法回答——指标退化成一个计数器，失去了维度。

**用 OpenTelemetry 加 OTLP 推送。** 需要一个 collector 常驻，与"单二进制、零外部组件"冲突。

### 怎么验证

```bash
go test ./internal/metrics/ -run 'TestNameWithoutValueIsVisibleNotSilent|TestKeyCannotCollideAcrossDifferentLabelSplits|TestLabelValuesAreEscaped' -v -count=1
```

三条断言分别对应三个修复点：畸形标签集渲染为 `broken_total{status="<missing>"} 3` 且触发自检计数（`internal/metrics/metrics_test.go:33`）；`["a","1,2"]` 与 `["a,1","2"]` 是两条独立序列（:75）；值里的引号、换行、反斜杠被正确转义（:57）。

端到端还有 `TestMetricsExposeRealLabelValues`（`internal/httpapi/regression_test.go:202`）：它真的发一次 200 请求和一次 401 请求，再抓 `/metrics`，断言 `sandboxd_http_requests_total{status="200"}` 与 `{status="401"}` 都在，且畸形计数器保持沉默。

## 13.5 结构化日志与请求 ID

### 问题

纯文本日志只能被人读。当客户问"过去一小时我们收到多少个 402""哪个租户的错误最多"时，纯文本日志让你去写 awk；而当你要把日志接进聚合系统时，靠空格切字段是不可靠的——字段值里出现一个空格，后面的列就全错位了。

### 原理

结构化日志就是一行一个 JSON 对象，键是固定字段集。它的价值不在"好看"，而在**日志从文本变成了可查询的数据**。

日志里必须有四类字段：时间（带时区、亚秒精度）、级别、事件名、以及回答"哪一次"的关联 ID。绝不能有的一类东西：凭据明文。

### 本项目怎么做

**实现只有八行**（`internal/httpapi/server.go:463-470`）：

```go
func logJSON(level, msg string, fields map[string]any) {
	entry := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "level": level, "msg": msg}
	for k, v := range fields {
		entry[k] = v
	}
	blob, _ := json.Marshal(entry)
	fmt.Println(string(blob))
}
```

**访问日志的字段集**（`internal/httpapi/server.go:190-194`）：

```go
logJSON("info", "access", map[string]any{
	"method": r.Method, "path": r.URL.Path, "status": sw.status,
	"duration_ms": time.Since(start).Milliseconds(),
	"request_id":  w.Header().Get(requestIDHeader),
})
```

五个字段对应五个问题：做了什么（method + path）、结果（status）、多慢（duration_ms）、哪一次（request_id）。

**`duration_ms` 取毫秒而不是秒。** 秒级精度对 API 延迟没有任何意义——你要找的是几十毫秒的差异，而在秒级分辨率下它们全是 `0`。

**`request_id` 从响应头回读。** 它与 §12.5 讲的中间件是同一根线。运维侧的收益很具体：客户报障时给你一个 ID，你能把所有相关行一次捞出来，而不是在时间戳附近翻。

**凭据绝不能进日志。** 只有**本进程生成**的 admin key 才回显到 stdout（`cmd/sandboxd/main.go:293-298` 的注释，以及 :314-317 那个 `if !generated` 分支）：

```go
// Only a key this process GENERATED is echoed. A key the operator
// supplied in configuration is already in the config file, and stdout
// goes to the journal — printing it again would leave a live
// platform-admin credential in a log with a retention policy nobody
// chose. Found by the end-to-end suite, which asserts that no credential
// reaches the daemon's output.
```

这条也是真实缺陷：运维通过配置提供的 bootstrap admin key 仍被打印到 stdout/journal，已修复，记录见 `docs/SECURITY-CONTRACT.md:162`。

### 替代方案

**纯文本加正则。** 字段值里出现空格或引号就破；而且每加一个字段，所有正则都要跟着改一遍。

**引入日志库。** `log/slog` 其实就在标准库里，本项目没用它而写了八行函数——区别在需求：这里只需要"一行 JSON"，不需要 handler 体系、级别过滤与上下文注入。

**把 request_id 只放在响应头里、不进日志。** 那样"从一个 ID 找到全部相关日志"这条路径就不存在了，ID 只剩给客户端看的价值。

### 怎么验证

```bash
SBX_REQUIRE_DOCKER=1 go test ./tests/ -run TestSecretsNeverReachTheLog -v -count=1
```

`tests/contract_test.go:244` 先发一次正常请求和一次 401——两者都会产生访问日志——然后读 daemon 的完整输出，断言租户 Key 与 admin Key 的明文都不在其中。

## 13.6 优雅停机与 SIGHUP

### 问题

两条停机路径都会出错：进程秒退但把在途请求丢下；或者进程卡住不退出，直到被 `kill -9`。后者更常见，原因通常是某个后台 goroutine 没有退出条件——它比创建它的 Service 活得更久，表现为内存缓慢增长、测试套件越来越慢。

### 原理

停机的正确顺序是**先关门，再熄灯**：先停止接收新工作，再等待在途工作完成，最后才停掉支撑在途工作的设施。

反过来的后果是：在途请求会撞上一个已关闭的下游，产生一堆 500 和半截状态——而这些 500 会被记成服务端的错，掩盖真正的成因（我们在发布）。

宽限期必须是**有限**的：无限等待意味着一个卡住的请求能让进程永不退出，而部署系统最终仍会强杀。既然结果一样，不如自己控制这个窗口，并在超时前尽量干净地收尾。

### 本项目怎么做

```go
shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
defer cancel()
_ = srv.Shutdown(shutdownCtx)
svc.Stop()
logf("info", "sandboxd stopped cleanly")
```

`cmd/sandboxd/main.go:141-145`。`srv.Shutdown` 停止接收新连接并等待在途请求（机制见 §4）；此时业务层仍在工作，所以在途请求能被正常处理完。随后 `svc.Stop()` 关闭 `stopCh` 并停掉限流器（`internal/service/service.go:69-76`）——注释点明了为什么必须连带停限流器：*只停 service 自己的循环，会让限流器的清扫 goroutine 继续对着一张已被丢弃的桶表跑*。

再往前一步，等待阶段用 `select` 同时等信号和 HTTP 错误（`cmd/sandboxd/main.go:130-139`）：如果监听失败（端口被占），进程立刻返回，而不是等一个不会来的信号。其中 `errors.Is(err, http.ErrServerClosed)` 被显式排除——`Shutdown` 会让 `ListenAndServe` 返回这个哨兵错误，它是"正常关闭"的表示；漏掉这个判断会把每次优雅停机都记成一次异常退出。

**SIGHUP 的真实状态必须写清楚。** `docs/DESIGN.md:207` 在运维面一栏里写了"`SIGHUP` 日志级别热调"，但当前二进制**没有**注册 SIGHUP 处理器——`cmd/sandboxd/main.go:131` 只 `Notify` 了 `SIGINT` 与 `SIGTERM`，代码里也不存在任何重载路径。后果是：向它发 SIGHUP 会按系统默认动作处理，而不是重载配置。

这不是"已支持热调"，而是**设计意图与实现之间一条尚未闭合的缝**。要闭合它需要的额外工作也值得先想清楚：配置在 `run()` 里只加载一次，加载后被 service 按值持有，且 `cfg.LogLevel` 目前没有任何消费方——所以热调既没有入口，也没有出口。

### 替代方案

**用 `Close()` 而不是 `Shutdown()`。** `Close` 立即关闭所有连接，包括正在写的那条。停机是快了，代价是客户端看到截断响应，而这个截断同样会被记成服务端的错。

**不设内部宽限，靠 systemd 的 `TimeoutStopSec`。** 等于把"收尾能做多久"的决定权交给外部；一旦外部超时后 `kill -9`，进程连一行"我要停了"的日志都留不下。

**让每个 goroutine 各自监听退出信号。** 可行，但退出条件散落在 N 处，漏一处就泄漏一个；集中到一个 `stopCh`（`internal/service/service.go:37`）只需要检查一处。

### 怎么验证

```bash
SBX_REQUIRE_DOCKER=1 go test ./tests/ -run TestCrashRecovery -v -count=1
```

`tests/resilience_test.go:68` 的 `a_graceful_restart_keeps_serving` 子场景：建一个沙箱、优雅重启、断言它仍是 `running` 且能 exec。这一条同时验证了三件事——**停机没有杀掉沙箱、启动对账没有把存活的容器判成丢失、重启后控制面与后端对象一致**（`managedContainers` 前后相同，见 :289）。

## 13.7 systemd 与气隙部署

### 问题

一个管理容器的 daemon 必须比容器运行时**后**启动、比它**先**停止——否则启动期的 `Ping` 必然失败。而企业内网通常连不上公共镜像仓库，"创建时现拉镜像"这条路根本不存在。

### 原理

systemd 用 `After`/`Requires` 表达依赖顺序，用 `Restart` 表达失败策略，用 `StateDirectory` 表达"这个服务有一份需要持久化的状态"。

气隙部署的本质是把**镜像从"网络资源"降级为"文件产物"**：在联网机器上导出成 rootfs，以文件形式过审、过闸进入内网，再由目标节点导入。这一步把"部署时能否联网"这个外部条件，变成了"有没有带上那个文件"这个内部条件。

### 本项目怎么做

`deploy/sandboxd.service` 全文十四行，逐项说：

```ini
[Unit]
Description=sandboxd agent sandbox runtime
After=docker.service
Requires=docker.service
```

`After` 只管次序，`Requires` 管连带：docker 停掉时本服务也停。这不是可选项，因为后端在启动时会 `Ping`（`cmd/sandboxd/main.go:210`），运行时不在就直接拒绝启动。

```ini
[Service]
ExecStart=/usr/local/bin/sandboxd -config /etc/sandboxd/sandboxd.conf
Restart=always
RestartSec=2
LimitNOFILE=65536
StateDirectory=sandboxd
```

- **`Restart=always` 加 `RestartSec=2`**：注意它和"坏值拒启"的相互作用——如果配置要求 `isolation = microvm` 而本机没有 KVM，进程会**每两秒被拉起一次，无限循环**。这是有意的失败放大：一个静默降级、继续接流量的节点，比一个在 journal 里持续刷屏的重启循环危险得多。但运维得事先知道这个循环长什么样，否则会把它当成"服务起不来"而不是"服务在拒绝一个无法兑现的要求"。
- **`LimitNOFILE=65536`**：每个沙箱的 exec 与文件传输都会占 fd，控制面本身也持有监听连接。默认上限在几十个沙箱的规模上先撞墙，而撞墙的表现是 `accept` 失败——一个和容器运行时毫无关系的报错。
- **`StateDirectory=sandboxd`**：为服务创建并归属 `/var/lib/sandboxd`。这一项与"数据目录就是全部状态"直接对应（备份与恢复见 §9、§10）。

**气隙怎么拿到基础镜像**（`docs/DEPLOYMENT.md:66-78`）：联网机器准备 rootfs tarball，随二进制一起分发，启动后用 admin key 调 `POST /v1/templates/import` 导入（端点注册见 `internal/httpapi/server.go:104`）。rootfs 必须是完整用户态，含 `/bin/sh` 与动态链接器；导入时会自动归一化 merged-/usr 布局。

**气隙下的模板默认值值得一提。** `bootstrapTemplates`（`cmd/sandboxd/main.go:330-354`）在配了 `bootstrap.image` 但本机没有该镜像时，**直接返回：不播种、不报错、不打日志**。注释称这是 "the correct air-gapped default"——气隙节点本来就不该凭空变出镜像，静默跳过比编造一个模板更诚实。

**一个 systemd 不替你解决的问题。** 这个 unit 没有写 `Type=notify`，所以按默认的 `simple` 处理：systemd 在 `ExecStart` 派生的那一刻就认为服务已启动，而此时 daemon 还在 ping 后端、跑自检、做启动对账。因此 `systemctl start` 返回成功**不代表端口在监听**——部署脚本必须自己轮询 `/readyz`（升级流程见 `docs/DEPLOYMENT.md:80-86`）。

### 替代方案

**用容器编排或托管运行时跑 daemon。** 与"单二进制、零外部组件"的目标冲突（见 §16.5）。

**气隙环境里搭一个内网镜像仓库。** 可行，但引入了一个要维护、要打补丁、要有存储的常驻组件；rootfs 文件导入不需要任何常驻服务。

**把 `Restart` 改成 `on-failure`。** 会让"拒启"表现为"已停止"，反而更难被注意到。持续重启的循环在 journal 里是显眼的，停止不动的服务不是。

### 怎么验证

```bash
journalctl -u sandboxd -b --no-pager | head -40
```

预期在启动日志里按固定顺序看到：`sandboxd 1.1.5 starting; listen=...` → `backend=... ready` → `wal recovery: ...` → 隔离自检逐行事实 → `reconciliation complete: ...` → `http listening on ...`。

**顺序本身就是断言**：如果 `http listening` 出现在 `reconciliation complete` 之前，说明启动顺序被改动了。

## 13.8 容量估算：一个可复算的方法

### 问题

"这台机器能跑多少个沙箱"是最常被问、也最常被拍脑袋回答的问题。直接给一个数字没有用——换一台机器、换一套租户配额，数字就失效了，而照着失效数字做的采购决策不会自动失效。

### 原理

容量由**三个相互独立的上限取最小值**决定：

1. **内存上限**：宿主可用内存 ÷ 单沙箱内存规格。这是最硬的一条——沙箱内存是**承诺**（cgroup 上限），超了就是 OOM。
2. **CPU 上限**：宿主核数 × 允许的超订比 ÷ 单沙箱 CPU 规格。CPU 是可压缩资源，超订只影响速度、不影响正确性，所以这条通常不是瓶颈。
3. **治理上限**：所有租户配额之和，以及平台级并发闸。这条决定的是"谁先用完"，不是"机器什么时候满"。

### 本项目怎么做

三个输入全部能从代码里读出来，不需要猜：

| 输入 | 值 | 来源 |
|---|---|---|
| 单沙箱默认内存 | 512 MiB | `internal/config/config.go:193` |
| 单沙箱内存硬上限 | 16384 MiB | `internal/config/config.go:184` |
| 租户默认内存配额 | 8192 MiB | `internal/config/config.go:202` |
| 租户默认沙箱数配额 | 10 | `internal/config/config.go:200` |
| 全局并发创建数 | 8 | `internal/config/config.go:218` |
| 创建排队超时 | 15 s | `internal/config/config.go:219` |
| 预热池（每模板） | 2 | `internal/config/config.go:227` |

**CPU 怎么换算**（`internal/runtime/oci/backend.go:273`）：

```go
NanoCPUs:   int64(req.CPUMillicores) * 1_000_000,
```

即 1000 毫核等于一个核。默认规格 1000 毫核，也就是一个核。

**内存与 swap**（`internal/runtime/oci/backend.go:274-275`）：

```go
Memory:     int64(req.MemoryMiB) << 20,
MemorySwap: int64(req.MemoryMiB) << 20, // swap off: an OOM must hit this sandbox only, and visibly
```

`MemorySwap` 与 `Memory` 设为同一个值，等于关掉这个沙箱的 swap。注释给出的理由是"一次 OOM 必须只打到这一个沙箱，而且是可见的"。**不关 swap 的话，一个沙箱的内存压力会变成宿主的匿名页换出**，故障就不再局限在那一个沙箱里——而"局限且可见"正是 §5、§6 那套遏制论证的前提。

**于是单机沙箱数可以写成**：

```text
单机沙箱数 = min(
    宿主可用内存 / 单沙箱内存规格,
    宿主核数 × 超订比 / 单沙箱 CPU 规格,
    Σ 租户 max_sandboxes 配额
)
```

其中"宿主可用内存"要先扣掉三样：宿主自身常驻、镜像层常驻、`pool.size` 个预热容器。**预热池是常驻开销**——它让突发创建走"领养"快路径（`internal/service/service.go:137`），代价是这些空转容器一直占着配额。

**并发创建数不是容量上限**（`internal/config/config.go:218`）。它只限制同时"实施"的创建数，超出的排队，超过 15 秒以 `state: failed` 加 "create queue timeout — node is saturated" 显式失败，而不是无限堆积。所以它决定的是**创建延迟的形状**，不是能跑多少。

**第三项只能当规划值**：配额准入在竞态下是尽力而为的（见 §11）。

### 替代方案

**直接用压测得到的数字。** 可复现性差：它测的是"那台机器加那个负载"，换环境就失效，而读者会把数字当成产品属性记下来。

**按 CPU 核数估。** 忽略内存才是硬约束——本项目关掉了 swap，所以内存超订会直接变成 OOM，连"慢一点"这个缓冲都没有。

**做成自动弹性伸缩。** 需要多节点、调度器与共享状态，不在本版范围内（见 §16.6）。

### 怎么验证

把三个上限各自算出来取最小值，再和实测对照：

```bash
SBX_REQUIRE_DOCKER=1 go test ./tests/ -run TestControlPlaneLatencyIsFlatInFleetSize -v -count=1
```

`tests/commercial_test.go:425` 在 fleet=1 与 fleet=12 时分别测同一个请求的 p50，断言比值有界（实测读 p50 ×0.9、exec p50 ×1.1）。

这条断言和容量估算的关系是：**你估出来的容量成立的前提，是每请求开销不随沙箱数变化**。一旦某条热路径退化成对全舰队的扫描，它会先于容量问题报错（写成比值而非绝对毫秒的理由见 §14.5）。

## 13.9 从指标到告警：挑能判定的信号

### 问题

指标导出来了，不等于知道该告警什么。告警太多会被静音，而**静音的告警保护不了任何东西**；告警太抽象（"CPU 高"）则没人知道该做什么。

### 原理

一条好告警要同时满足三个条件：**能被判定**（有明确的阈值或比值）、**指向一个动作**（有人知道该干什么）、**不会自己骗自己**（不会因为采样、降级或静默失败而消失）。

按信号来源分三类，恰好对应三种判定方式：端点能直接核对的事实、指标之间的比值、宿主的物理上限。

### 本项目怎么做

`docs/DEPLOYMENT.md:88-95` 给了一份清单，这里说清每一条为什么是它：

- **`/readyz` 非 200**：唯一一条"外部可见、且同时覆盖后端与存储两条依赖"的信号。它不鉴权（见 §12.8），所以任何探针都能查。
- **`sandboxd_creates_total{source}` 里 pool 的占比**：这是**比值**，不是绝对值。它随负载升高是正常现象；只有当它不再升高、而 `sandboxd_pool_misses_total` 持续增长时，才说明池容量不足。
- **`sandboxd_http_requests_total{status}` 中的 5xx**：目标是 0。选 5xx 而不是延迟，因为延迟的合理阈值随机器变，而"服务端错误应为 0"不随机器变。
- **磁盘超过 80%**：WAL 与镜像层共用 `data_dir` 所在的卷，所以磁盘既影响持久化也影响创建。
- **宿主内存**：所有沙箱内存上限之和不超过宿主物理内存的 80%。这 20% 留给宿主自身、页缓存与镜像层——**因为 swap 被关掉了，这个余量就是唯一的缓冲**。

### 替代方案

**对每个指标都设阈值。** 结果是告警风暴，最终被静音。

**只告警 `/healthz`。** 它只回答"进程还活着"，回答不了"能不能接活"——而后者才是容量与依赖故障会先表现出来的地方。

### 怎么验证

取一份真实抓取，逐条走一遍"如果它响了，我做什么"：

```bash
curl -s -H "Authorization: Bearer $ADMIN_KEY" "$BASE/metrics" | grep -E '^sandboxd_'
```

预期至少能看到 `sandboxd_http_requests_total{status="200"}`、`sandboxd_creates_total{source="cold"}`、`sandboxd_pool_size`。

如果看到 `sandboxd_metrics_malformed_calls_total`，那是**代码缺陷信号，不是负载信号**——有调用点在传畸形标签集。这个区分很重要：把代码缺陷当负载来处理，会调错旋钮。

## 13.10 已知缺口：这一章没做的事

### 问题

一份只讲"我们做了什么"的运维章，会让人以为覆盖是完整的。而覆盖不完整这件事，运维通常最后一个知道。

### 原理

缺口的写法和缺陷的写法一样：**给出代码事实、给出后果、说清它是"没做"还是"选了另一边"**，并标上版本，让读到的人知道这个判断是什么时候下的。

### 本项目怎么做

- **SIGHUP 未接线**：`docs/DESIGN.md:207` 写了"`SIGHUP` 日志级别热调"，`cmd/sandboxd/main.go:131` 只注册了 `SIGINT` 与 `SIGTERM`（详见 13.6）。
- **`log_level` 目前没有消费方**：`internal/config/config.go:28` 声明，`internal/config/config.go:325` 与 `internal/config/config.go:428` 分别从文件和环境读取，但没有任何地方根据它过滤输出。它是个**惰性配置键**：改它不会报错，也不会生效。
- **指标注册表无基数上限**：`Registry.order`（`internal/metrics/metrics.go:52`）只增不减。当前调用点的标签值都是固定枚举，规模有限；但一旦有人用租户 ID、路径或错误文本做标签值，注册表就会无上限增长。限流器的桶表有上限与最旧淘汰，注册表没有——**这是一处不一致**。
- **无分布式追踪**：请求 ID 只在本进程内贯穿（见 §12.5），跨进程没有传播载体。
- **无 HA**：单节点，停机即不可用；数据目录是唯一状态（备份与恢复见 §9、§10）。

### 替代方案

**把缺口从文档里删掉。** 等于让客户自己发现。§16.6 讲过这类清单的价值在哪里。

**立刻补齐所有缺口。** 顺序反了：先说清边界，再决定做不做。`log_level` 这类缺口要不要补，取决于是否真需要运行时调级。

**用"计划中"模糊带过。** 一个没有代码事实支撑的"计划中"，在读者眼里与"没想过"没有区别。

### 怎么验证

```bash
grep -rn "LogLevel" internal/ cmd/    # 只有声明与解析，没有消费方
grep -rn "SIGHUP" internal/ cmd/      # 空：没有注册处理器
grep -n "MaxMemoryMiB\|MaxConcurrentCreates\|PoolSize" internal/config/config.go
```

第三条用来核对 13.8 那张输入表的当前值。如果哪天改了，应当改的是表，而不是照抄这份文档里的数字。

---

### 本章检验

1. 隔离自检必须在监听之前完成。请给出一个具体时序，说明反过来会产生什么**无法通过重试修复**的后果，并说明为什么"让客户端重试"在这里不是解法。

2. `applyEnv` 对畸形数值报错而不是回退默认。请举出**另一个**安全或容量类配置，说明静默回退会造成的后果链条，并说明这个原则在什么情况下**不该**照搬（提示：想想哪些配置的值是"偏好"而非"保证"）。

3. `metrics.key()` 用与渲染相同的规范化形式构造键。请自己构造一组能证明朴素拼接会碰撞的输入，并说明：为什么这个 bug 的表现是"指标少一个维度"而不是崩溃？为什么修复选择"可见地渲染并计数"而不是 panic？

4. `deploy/sandboxd.service` 里没有 `Type=notify`。请说明这在生产部署上造成的具体问题，以及为什么"修 unit 文件"不足以替代"监听放最后"这个设计。

5. 手册建议"所有沙箱内存上限之和 ≤ 宿主物理内存的 80%"。请说明那 20% 留给谁、为什么在**关掉 swap** 之后这个余量从"性能考虑"变成了"正确性考虑"。

**动手任务**：给 `run()`（`cmd/sandboxd/main.go:56-147`）写下九步的完整清单，对每一步回答两个问题——"它的前置条件由谁提供"和"它给下一步提供了什么"。然后从中挑出**至少两对**相邻的、可以安全交换顺序的步骤，并说明理由（能交换说明这两步之间没有依赖，这本身就是信息）。最后用 `journalctl -u sandboxd -b` 或本地启动日志核对你的清单顺序是否与实现一致。


# §14 测试方法论

## 14.1 先问一个更难的问题：什么时候"测试通过"等于什么都没证明？

### 问题

假设你要证明"沙箱里的代码碰不到宿主机"。最直觉的写法是这样：

```bash
docker exec "$cid" sh -c 'rm -rf /host 2>/dev/null'
test -d /host && echo "host intact"    # 断言：宿主目录还在
```

这条断言在**下面每一种情况**下都会通过：

1. 沙箱真的被隔离了，`rm` 失败了；
2. 沙箱根本没隔离，`rm` 成功删掉了 `/host`——但 `/host` 这个路径从来不存在；
3. `docker exec` 本身失败了（容器已退出），`rm` 压根没跑；
4. 脚本里的 `$cid` 是空的，`docker exec` 报错但错误被 `2>/dev/null` 吞掉。

四种情况里只有第一种是"隔离生效"。一个**无声地什么都不做的探针**，和一个**真正被拦住的攻击**，通过的是同一条断言。这就是本项目原来的 `tests/malicious.sh` 的问题：它断言"宿主没事"，却从未证明载荷真的干过什么。

这个失败模式有名字，叫**断言缺少正向对照**（positive control）。它是测试设计里最贵的一类错误，因为它不产生红灯——它产生一个假的绿灯，而这个绿灯会一直亮到你真的被攻破那天。

所以本章的出发点不是"怎么写测试"，而是：**一条断言在什么条件下会无条件通过？** 如果答案是"在被测行为失效时也会通过"，那这条断言就是装饰品。

### 原理：断言的价值等于它能被推翻的程度

一条断言的信息量，等于**它可能为假的那些世界里，有多少个会让它变红**。

把这个标准套到四种常见写法上：

| 写法 | 什么情况下会变红 | 被推翻的世界里有多少能抓到 |
|---|---|---|
| `resp.Status == 201` | 只要不是 201 | 语义错误、参数错误、权限错误——抓得到 |
| `err == nil` | 只要报错 | 抓得到"崩溃"，抓不到"静默做错" |
| `test -d /host` | 目录消失 | 目录从来不存在时也通过 |
| `grep -q "capabilities" config.json` | 字段不见 | **只证明配置里写了字，不证明内核认了**（见 §6） |

第三行和第四行是同一类病：**断言的对象不是被测的东西**。第三行断言的是"路径存在"，被测的是"沙箱写不进去"；第四行断言的是"配置文件里有这个词"，被测的是"内核兑现了这个限制"。

本项目对第四行的处理是方法论级别的，写在了 `tests/isolation_test.go:1-19` 的文件头：

```go
// Every scenario measures one fact from *inside* a real sandbox and compares
// it against the value the contract requires. That direction matters: a
// container engine will accept security settings it cannot honour and report
// them back as applied, so configuration is intent and only the kernel
// talking from inside the sandbox is evidence.
```

这句话是全章的骨架：**配置是意图，内核事实才是证据**。机制部分（为什么引擎会回读一个它不兑现的设置）在 §6 讲，本章只讲这条原则如何变成可执行的断言形状。

### 本项目怎么做：把"断言方向"写进测试名

`tests/isolation_test.go:38-64` 的前四个场景，每一个的名字都是一句主张，而不是一个名词：

```go
t.Run("seccomp_filter_is_in_force", func(t *testing.T) {
	// 2 = SECCOMP_MODE_FILTER. Any other value means no syscall filter,
	// and the syscall surface is the main escape surface.
	eq(t, statusField("Seccomp"), "2", "seccomp mode")
})
```

关键在三处：

1. **读的是 `/proc/self/status`**，通过 `awk` 在沙箱内部执行（见 `tests/isolation_test.go:34-36`）。这是内核对"这个进程现在处于什么 seccomp 模式"的答复，不是 daemon 的配置回显。
2. **比较的是字面量 `"2"`**。`SECCOMP_MODE_FILTER` 在内核里就是 2；`0` 是禁用，`1` 是严格模式（只允许 read/write/exit/sigreturn，实际上跑不了通用容器）。断言写一个精确值而不是"非 0"，是因为"非 0"会把严格模式也算通过，而严格模式不是本项目的档位。
3. **`eq` 的第三个参数是"被测的是什么"**（`"seccomp mode"`）。失败信息会打印 `seccomp mode: got 0, want 2`，读者不需要回去看代码就知道哪一层塌了。

第四个场景 `tests/isolation_test.go:57-64` 更值得看，因为它断言的是一个**反直觉的组合**：

```go
t.Run("uid_is_root_but_powerless", func(t *testing.T) {
	// The contract is not "non-root". It is root-by-name with an empty
	// effective capability set: ...
	eq(t, tn.Out(t, sb, "id -u"), "0", "uid inside the sandbox")
	mustBeAllZeroHex(t, statusField("CapEff"), "effective capabilities of uid 0")
})
```

它同时断言 `id -u == 0` **和** `CapEff == 0`。一个只断言"非 root"的测试在这里会失败，而一个只断言"uid 是 0"的测试会漏掉真正的风险。合同是"名字上的 root，实权为空"——这个措辞必须原样写进测试，因为客户的安全评审一定会问这一句。

### 替代方案：为什么不直接断言"引擎返回的 HostConfig"

最省事的写法是读引擎的 inspect 结果，检查 `SecurityOpt` 里有没有 `no-new-privileges`。它有三个问题：

1. 它证明的是**你提交了什么**，不是**内核接受了什么**。§6 里记录了实测：`--security-opt` 写了会被回读，但未必生效。
2. 它把"断言"变成了"配置快照的回归测试"——这种测试会**阻止你改进配置**，因为任何正确的改动都会让测试变红，而任何错误的回读都不会。
3. 它对**后端替换**完全无效。今天用 Docker Engine，明天换成 Kata，inspect 里的字段全变了，而"沙箱内部读 `/proc`"这个断言一个字都不用改。

选择"从内部读内核"的代价是慢（每次都要真起容器、真执行一条命令），收益是**断言与后端实现解耦**。这个取舍在 §16 会再出现一次，它是同一条原则的两个面。

另一个被否掉的替代方案是**用外部扫描器**（比如在生产环境跑一次 CIS Benchmark）。它的问题是没有"当下这一个沙箱"的粒度：扫描器告诉你节点配置如何，而你要证明的是**这次创建的这个实例**被隔离了。

### 怎么验证

```bash
cd castle
make build
SBX_REQUIRE_DOCKER=1 go test -count=1 -v -run 'TestIsolationContract' ./tests/
```

预期：22 个子场景全部 `--- PASS`。这 22 个场景的名字列在 `tests/isolation_test.go`，从 `seccomp_filter_is_in_force`（:38）到 `unenforceable_limits_are_refused_not_silently_ignored`（:229）。**可以只跑其中一个**来观察它怎么失败，例如：

```bash
SBX_REQUIRE_DOCKER=1 go test -count=1 -v -run 'TestIsolationContract/seccomp' ./tests/
```

---

## 14.2 第四层测试：为什么端到端脚手架要"重复造一遍"

### 问题

假设你有六个端到端场景要写：认证、配额、崩溃恢复、隔离、负载、弹性。不用脚手架的话，每个文件开头都得写这三十行：

```bash
cat > /tmp/sbxd.conf <<EOF
listen = "127.0.0.1:$PORT"
data_dir = "/tmp/sbx-data-$PORT"
EOF
pkill -9 -f 'sandboxd -config /tmp/sbxd.conf' || true
rm -rf /tmp/sbx-data-$PORT
./bin/sandboxd -config /tmp/sbxd.conf &
sleep 2
ADMIN_KEY=$(grep -o 'sk_[A-Za-z0-9]*' /tmp/sbxd.log | head -1)
```

这三十行里**没有一行是被测对象**，但它们每一行都能让测试失败，而且失败的方式各不相同：

- `sleep 2`：在慢机器上不够。你会得到一个偶发的红灯，然后你会把它改成 `sleep 5`，然后在 CI 上它又不够。
- `grep -o 'sk_...'`：banner 文案一改就选中错误的 key。更糟的是它可能选中**上一次**启动留下的日志行。
- `/tmp/sbx-data-$PORT`：两轮并发跑就撞车。

真正致命的是最后一条：这些失败会被归到"这次场景没过"的账上，于是**你去修场景，而不是修脚手架**。时间就这样花在了不该花的地方。

### 原理：脚手架的职责是"报告"，不是"爆炸"

`tests/harness_test.go:1-17` 把这个动机写成了注释：

```go
// Six bash scripts used to each re-implement the same thirty lines: write a
// config, pkill the previous daemon, rm -rf the data dir, start the binary,
// sleep two seconds, grep the admin key out of the log, define two counters.
// Every one of those steps had a failure mode — a sleep that is wrong on a
// slow machine, a grep that picks the wrong key the moment the banner changes,
// a fixed /tmp path that collides between rounds — and none of them was the
// thing under test.
```

脚手架要做的三件事，按重要性排序：

1. **把"等待"变成"等待正确的东西"**。不是 `sleep 2`，是轮询到某个条件成立。
2. **让失败可读**。启动失败要打印配置和日志，不是打印"超时了"。
3. **保证清理一定会发生**，且清理失败不会把通过的测试染红。

第三点有个反直觉的推论：**脚手架里的错误必须是静默的**（清理路径上），而**脚手架里的失败必须是响亮的**（准备路径上）。`tests/harness_test.go:547-566` 的 `Drain` 注释把这个界线写清楚了：

```go
// Drain force-deletes every sandbox the harness created, best effort. It runs
// with the daemon alive where possible; failures are silent because cleanup
// must never turn a passing scenario red.
```

而 `WaitReady`（`tests/harness_test.go:501-514`）在准备路径上，所以它一旦失败就把配置和整段日志都吐出来：

```go
t.Fatalf("daemon not ready within %s\nconfig:\n%s\nlog:\n%s", limit, h.Config(), h.Log())
```

### 本项目怎么做：一次编译、私有端口、固定凭据

`tests/harness_test.go:276-302` 的 `New` 是入口，它做了五件有讲究的事：

```go
func New(t *testing.T, opts Options) *Harness {
	t.Helper()
	requireDocker(t)
	if opts.Image == "" {
		opts.Image = testBaseImage
	}
	h := &Harness{
		t: t, opts: opts, bin: daemonBinary(t),
		AdminKey: fixedAdminKey, port: freePort(t),
	}
	...
	t.Cleanup(func() {
		h.Drain()
		h.Kill()
		h.Reap()
	})
	return h
}
```

1. **`daemonBinary` 每次测试二进制只编译一次**（`tests/harness_test.go:76-102`），用 `sync.Once`。而且默认编译**不带** `-race`，只有 `SBX_IT_RACE=1` 时才带——因为被测对象的并发才是要查的，脚手架自己的并发不是。
2. **`freePort` 而不是固定端口**（`tests/harness_test.go:184`）。端口冲突在并发跑套件时是常态，不是意外。
3. **`fixedAdminKey`**（`tests/harness_test.go:57`）。原来那套"从日志里 grep 出 admin key"的做法被彻底删除：

```go
// fixedAdminKey replaces the old "scrape sk_… out of the log" dance. The
// daemon upserts it on every boot when bootstrap_admin_key is set, so the
// fixture knows the credential before the process has printed a byte.
```

这里的机制值得多看一眼：不是"测试去适应输出格式"，而是**让被测系统接受一个测试预先知道的凭据**。前者把测试耦合到日志格式上，后者把耦合变成配置契约的一部分（见 §13 的配置系统）。

4. **`t.TempDir()`**（`tests/harness_test.go:287`），每个测试一个私有目录，Go 运行时负责删除。撞车问题从根上消失。
5. **`t.Cleanup` 里三步清理**：先 `Drain`（通过 API 删记录，让 daemon 走正常回收路径），再 `Kill`（杀进程），最后 `Reap`（按标签直接删容器，不依赖 daemon 活着）。

`Reap` 为什么要存在？`tests/harness_test.go:568-586`：

```go
// Reap removes backend containers this harness created, by ownership label,
// without needing the daemon to be alive. Left-behind containers cost the
// operator disk and, worse, make the next run's orphan assertions ambiguous.
```

后半句是关键：**遗留容器会污染下一轮的孤儿断言**。`TestNoBackendLeaks`（`tests/resilience_test.go:171`）断言"后端对象数等于记录数"，如果上一轮留了三个容器，这一轮的基线就偏了，而这个偏差会表现为"孤儿检测有 bug"。脚手架的清理不是卫生问题，是**断言有效性问题**。

`Drain` 里还有一处细节，值得单独记住（`tests/harness_test.go:551-555`）：

```go
// The tenant is named explicitly: an admin key acting on a tenant resource
// must identify the tenant (see handlers.go), so a bare admin DELETE is
// refused with 403. That refusal used to be swallowed here, which made Drain a
// silent no-op and left Reap ... as the sole real cleanup.
```

也就是说：**这个清理函数曾经整整一段时间什么都不干，而且没人发现**。原因是它吞掉了 403。这条注释本身就是 §12 里"admin 操作必须带 `X-Tenant-Id`"这条契约的一个测试用例——只是它当时在被违反的那一侧。

### 替代方案：为什么不用 Docker Compose 起一套环境

一个明显的替代是写一个 `docker-compose.yml`，把 daemon、数据库、消息队列都起起来。没选它的理由有两条，一条技术一条方法论：

- 技术：本项目**没有数据库也没有消息队列**（见 §1 的分层与 §9 的存储）。唯一外部依赖是容器引擎，而容器引擎**不能被容器化到测试里**——你需要的是引擎的 socket，不是另一个引擎。
- 方法论：Compose 解决的是"拓扑怎么搭"，而这里的难题是"**什么时候算就绪**"和"**怎么保证清理**"。前者是产品语义（`/readyz` 覆盖后端可达性，见 §12），后者是所有权标签（`sandbox.id`，`tests/harness_test.go:59-61`）。这两件事 Compose 都帮不上。

另一个替代是**只用单元测试 + 假的后端**。这在 §16 会作为"测试金字塔的取舍"展开；简言之，假后端能测状态机，测不了"内核真的兑现了限制"，而后者是本项目一半的卖点。

### 怎么验证

```bash
cd castle
go test -count=1 -v -run 'TestNoBackendLeaks' ./tests/
# 跑完后确认没有残留容器：
docker ps -a --filter label=sandbox.id --format '{{.ID}} {{.Names}}'
```

预期：测试 PASS，第二条命令输出为空。如果第二条有输出，说明 `Reap` 没生效——这本身就是一条你可以手工执行的断言。

---

## 14.3 正向对照：把"没出事"变成可判定的

### 问题

回到 14.1 的那个例子。要让它变成一条真断言，缺的是一步：**先证明载荷在它能碰到的地方真的有效**。

`rm -rf /` 在容器里会删掉容器自己的根文件系统——如果容器根是 overlayfs 的可写层，这一刀是真的砍下去的。所以正确的断言顺序是：

1. 先在沙箱里放一个它能写的文件，执行 `rm -rf /`，**断言这个文件没了**（载荷有效）；
2. 再断言**宿主上的对应路径还在**（隔离有效）。

只有第 1 步通过，第 2 步才有意义。

### 原理：对照组的三种形态

正向对照不一定是"另一个目录"。在本项目里有三种形态，按成本从低到高：

| 形态 | 做法 | 用在哪 |
|---|---|---|
| **同路径对照** | 证明同一个操作在"可达目标"上成功、在"受保护目标"上失败 | `rm -rf /`、写宿主路径 |
| **金丝雀** | 在宿主侧放一个唯一命名的标记物，断言沙箱内看不见它 | 挂载泄漏检测 |
| **资源耗尽对照** | 证明某个载荷真的吃掉了它被允许的那份资源 | fork 炸弹、内存炸弹、CPU 独占 |

第三种最容易被写错。`TestMemoryBombIsContained`（`tests/containment_test.go:166`）要证明的**不是**"内存炸弹不会 OOM"，而是"**它 OOM 了自己，没有 OOM 掉别人**"。如果炸弹因为写错而根本没分配内存，测试照样通过——所以必须先看到它自己被 OOM killer 干掉。

### 本项目怎么做：五个遏制场景的正向对照

`tests/containment_test.go:1-17` 的文件头把原则写死了：

```go
// These scenarios replace tests/malicious.sh. The bash version asserted "the
// host is fine" without ever proving the payload did anything, and a probe
// that silently no-ops passes exactly the same assertion a fully contained
// attack does. Every scenario here therefore carries a positive control:
// evidence that the hostile command really executed and really worked on the
// thing it was aimed at. "Nothing bad happened" is only meaningful once
// "something happened" is established.
```

五个场景（`tests/containment_test.go:49/122/166/191/248`）分别是：

| 场景 | 行号 | 正向对照是什么 |
|---|---|---|
| `TestBlastRadiusIsTheSandbox` | :49 | 沙箱内 `rm -rf /` 真的把沙箱自己毁了（:62 `the_payload_really_works_on_a_path_it_can_reach`），宿主未受影响（:101） |
| `TestForkBombIsContained` | :122 | 真的 fork 到 pids limit 上限，而不是没 fork |
| `TestMemoryBombIsContained` | :166 | 真的触发了 OOM，且 OOM 的是自己 |
| `TestCPUBurnDoesNotStarveTheControlPlane` | :191 | 真的占满了分配给它的 CPU 配额，而控制面延迟没有随之上升 |
| `TestInSandboxHostPathWritesDoNotReachTheHost` | :248 | 沙箱内写入真的成功了，宿主同名路径没有变化 |

第一个场景的四个子断言（`tests/containment_test.go:62/76/101/110`）值得按顺序读一遍，它们演示了完整的对照结构：

- `:62` `the_payload_really_works_on_a_path_it_can_reach` —— 正向对照，载荷有效；
- `:76` `rm_rf_root_wrecks_only_that_box` —— 爆破半径是那个沙箱；
- `:101` `the_host_is_untouched` —— 宿主没事；
- `:110` `the_wreck_is_still_reclaimable` —— **毁掉的沙箱仍然能被回收**。

最后一条是最容易被漏掉的，也是最有产品价值的一条：一个被 `rm -rf /` 打烂的沙箱，如果没有第四条断言，可能会卡在"删不掉"的状态里，变成幽灵记录（幽灵记录的回收机制见 §10）。**攻击成功之后的清理路径，和攻击被拦住一样重要。**

金丝雀形态用在 `tests/isolation_test.go:111-123`：

```go
t.Run("no_host_directory_is_mounted", func(t *testing.T) {
	// Plant a canary on the daemon's own filesystem, then prove the
	// sandbox cannot see it. This is the assertion that a bind mount
	// added to backend.go would break immediately.
	canary := "sbx-canary-" + strings.ReplaceAll(tn.ID, "_", "")
	plantHostCanary(t, canary)
	defer removeHostCanary(t, canary)

	eq(t, tn.Exit(t, sb, "test -e /tmp/"+canary), 1, "canary visible at /tmp inside the sandbox")
	eq(t, tn.Exit(t, sb, "test -e /hosttmp/"+canary), 1, "canary visible at /hosttmp inside the sandbox")
	mounts := tn.Out(t, sb, "cat /proc/self/mounts")
	notContains(t, mounts, canary, "sandbox mount table")
})
```

金丝雀的名字带上了租户 ID，**保证每轮唯一**——否则上一轮残留的同名文件会让这一轮的金丝雀"本来就存在"，于是断言失去意义。第三条断言读 `/proc/self/mounts` 是第三重保险：前两条只证明"没在那个路径看到"，第三条证明"挂载表里根本没有它"。

`defer removeHostCanary` 这一行也不能省：金丝雀是在**宿主的真实文件系统**上种下的，它必须被收回，否则它会污染下一轮——和 14.2 里遗留容器是同一个道理。

### 替代方案：为什么不用"跑一遍已知 CVE 的 exploit"

最硬核的对照是直接跑一个真实的内核提权 EXP。没这么做，理由不是道德风险而是**信息量**：

- 它通过了，你不知道是哪一层拦住的（seccomp？capabilities？还是 EXP 本身对内核版本不适用）；
- 它失败了，你也不知道该修哪一层。

而"逐层断言 + 每层一个正向对照"的好处是**失败是可定位的**：`CapEff` 不是全零，你就知道是能力裁剪那一层出问题（见 §6）。测试的价值不只在于变红，在于变红之后你能立刻知道去哪。

另一个替代是**模糊测试**（随机生成系统调用序列）。它能发现你没想到的问题，但不能作为验收门——因为它的结果不可复现，而买方要的是"这 22 条每一条都通过"。

### 怎么验证

```bash
cd castle
make build
SBX_REQUIRE_DOCKER=1 go test -count=1 -v -run 'TestBlastRadiusIsTheSandbox' ./tests/
```

预期四个子场景全 PASS。想看正向对照怎么起作用，可以做一个破坏性实验：把 `tests/containment_test.go:62` 那个子场景临时改成 `t.Skip`，重跑，观察剩下的断言依然全绿——**这就是原版 `malicious.sh` 的真实状态**。

---

## 14.4 混沌测试：让进程在最不方便的时刻死掉

### 问题

"崩溃后状态不漂移"这句话，如果只在**没有崩溃发生**的时候测，它就是一句广告词。

难点在于"崩溃"这个事件本身很难制造：你需要在**正确的时刻**杀掉进程。杀早了，请求还没到后端；杀晚了，WAL 已经落盘、后端已经返回。真正有意思的窗口是"记录已建、后端还在忙"——这个窗口在正常路径上只存在几十毫秒。

### 原理：意图先行留下的可观测窗口

§10 讲的意图先行（intent-first）在这里有一个意外的好处：它把那个窗口变成了一个**可以用 API 参数稳定进入的状态**。创建时带 `?wait=false`，服务端在记录落盘之后、后端完成之前就返回 201。于是"崩溃发生在中途"不再需要碰运气，它是一个**你可以主动停在里面的状态**。

`tests/resilience_test.go:19-33` 就是这么做的：

```go
t.Run("a_sigkill_during_create_converges_instead_of_hanging", func(t *testing.T) {
	// wait=false hands back the record while provisioning is still in
	// flight, which is exactly the window a crash makes interesting.
	r := tn.TryCreateJSON(t, createBody{BaseImage: h.opts.Image, Spec: specOf()}, "?wait=false")
	r.MustStatus(t, http.StatusCreated)
	id := decode[*domain.Sandbox](t, r).ID
	h.track(tn.ID, id)

	h.Kill()
	h.Start(t)
```

`?wait=false` 不是"为了测崩溃而加的测试专用开关"，它是产品 API 的一部分（见 §12）——**能用产品的公开参数进入被测状态，就不要加测试专用的后门**。后门会让测试证明的东西和实现细节绑死。

`h.Kill()` 和 `h.Stop()` 的区别也必须分清（`tests/harness_test.go:418` vs `:434`）：

```go
// Kill sends SIGKILL and waits for the process to be gone. It deliberately
// skips Drain: crash scenarios need the daemon dead mid-flight.
```

`Kill` 走 `SIGKILL`，不给优雅停机的机会——这正是崩溃的定义。`Stop` 走 `SIGTERM` 并等待，用于优雅重启场景。用错一个，你测的就是另一件事了。

### 本项目怎么做：六个韧性场景

`tests/resilience_test.go` 有六个顶层用例：

| 用例 | 行号 | 破坏什么 | 断言什么 |
|---|---|---|---|
| `TestCrashRecovery` | :19 | 进程被 SIGKILL，包括创建中途 | 重启后收敛，不悬挂、不漂移 |
| `TestPointInTimeRecovery` | :76 | 数据目录被替换成旧备份 | 备份时点之后的操作确实消失 |
| `TestBackendOutageIsDisclosedAndRecovers` | :136 | 容器引擎被停掉再拉起 | 问题是**被披露**的，不是静默降级 |
| `TestNoBackendLeaks` | :171 | 无 | 后端对象数 == 记录数 |
| `TestSnapshotsDoNotLeakImages` | :197 | 反复创建/删除快照 | 镜像引用计数归零后被回收 |
| `TestOrphanedSnapshotImagesAreReaped` | :230 | 制造孤儿镜像 | 孤儿被扫描回收（机制见 §10） |

第三个用例的措辞值得注意：**"disclosed"（披露）而不是"handled"（处理）**。它的断言不是"后端挂了服务还能用"，而是"后端挂了这件事会出现在 `/readyz` / 错误码里"。这是一个产品主张：控制面不假装自己健康。这个主张在 §12 的健康检查三兄弟和 §13 的启动顺序里都有对应物。

`TestBackendOutageIsDisclosedAndRecovers` 被单独隔离到 `make test-e2e-destructive`（`Makefile:42-44`），因为它要**重启整机的容器引擎**：

```makefile
# The two scenarios that disrupt every other workload on the machine (cycling
# the container runtime) are gated separately; nothing else needs this flag.
```

这是一条通用的测试工程原则：**破坏性测试必须默认关闭，但必须存在，且必须一键可跑**。默认关闭是因为它会干扰别人；存在是因为它是唯一能证明"引擎挂了之后会怎样"的方法；一键可跑是因为否则没人会跑。

### 替代方案：为什么不注入故障（fault injection）而不是真杀进程

故障注入（在代码里埋一个"当环境变量 X 为真时，在写 WAL 之后 panic"的钩子）看起来更精确。没选它的理由是**它测的是注入点，不是崩溃**：

- 你只能在你想到要埋点的地方制造失败，而真实的崩溃发生在你没想到的地方；
- 埋点代码本身会进入生产二进制，或者需要编译标签让生产版本和测试版本不同——后者意味着**你测的不是你要发布的那个东西**。

`SIGKILL` 的好处是它不可预测地打断任何事情，包括你没想到的那些。代价是**结果需要靠收敛性断言来判定，而不是靠精确状态断言**：你断言"重启后最终一致"，不断言"崩溃瞬间的内存状态是什么"。

这个代价换来的东西，和 `make test-e2e-race`（`Makefile:37-38`）的做法是同源的：

```makefile
# The same suites with the daemon under test built by -race, so the race
# detector instruments the daemon's goroutines rather than only the harness's.
```

注意这里编译的是**被测的 daemon**，不是测试代码。默认 `go test -race` 只给测试二进制插桩，而这里真正跑并发的是另一个进程。这类"工具插错了对象"的错误在集成测试里非常常见。

### 怎么验证

```bash
cd castle
make build
SBX_REQUIRE_DOCKER=1 go test -count=1 -v -run 'TestCrashRecovery' ./tests/
# 破坏性的那个单独跑：
make test-e2e-destructive
```

预期：前者 PASS；后者会**重启你机器上的容器引擎**，跑之前先关掉别的工作负载。

---

## 14.5 flaky 治理：绝对毫秒数是关于笔记本的命题

### 问题

你想断言"读一个沙箱的信息必须在 5ms 内返回"。在你的机器上它稳定通过。CI 机器上它偶尔失败。于是你把它改成 20ms。然后某天它在另一个人的笔记本上失败了，你再改成 50ms。到最后这条断言变成了"读一个沙箱必须在 50ms 内返回"——**它现在什么也不能证明**，因为一个真的 O(n) 全表扫描在 1000 个沙箱下可能只要 30ms。

更糟的是心理效应：**一个曾经随机失败过的断言，下次失败时你会先怀疑机器**。这条断言就死了，即使它变红你也不会行动。

### 原理：把断言写成比值

`tests/commercial_test.go:1-35` 的文件头给出了本项目的解法，而且说清了为什么：

```go
// The rule that shapes every latency assertion in this file: an absolute
// millisecond budget is a statement about the laptop, and a suite that fails on
// a busy machine gets muted, and a muted suite protects nothing. So every bound
// below is a ratio against another number measured on this same machine, in
// this same run — either against the cheapest request the daemon can serve
// (GET /healthz) or against the same measurement taken at a different fleet
// size.
```

这段话里最重的一句是 **"a muted suite protects nothing"（被静音的套件保护不了任何东西）**。测试被忽略的方式不是被删掉，是被 `@Ignore`。

比值断言的结构是：

```
p50(被测操作)  ≤  K × p50(基准操作)  +  C
```

分母选 `GET /healthz`，因为它**捕获机器的噪声与调度，不捕获产品的任何工作量**。这就是它的全部要求：它必须慢到足以让调度抖动显现，又必须轻到不碰后端、不碰存储。

常数项 `C`（10ms / 25ms / 20ms）的作用是**给比值一个地板**：当分子和分母都非常小时（比如都是 80µs），比值会不稳定，一个绝对的小余量比一个抖动的比例更可靠。

四条实际使用的断言（`tests/commercial_test.go`）：

| 断言 | 形式 | 它在说什么 |
|---|---|---|
| `p50(401) ≤ 2 × p50(/healthz) + 10ms` | 中间件 vs 地板 | 认证拒绝必须在任何后端工作之前发生 |
| `p50(读) ≤ 10 × p50(/healthz) + 25ms` | 读 vs 地板 | 已认证读只比地板多一次存储查找 |
| `p50(读, fleet=12) ≤ 3 × p50(读, fleet=1) + 20ms` | 大舰队 vs 小舰队 | 热路径不能是 O(沙箱数) 扫描 |
| `p50(后半段) ≤ 3 × p50(前半段) + 30ms` | 负载后段 vs 前段 | 持续负载下延迟不能漂移 |

第三条是本项目的核心产品主张之一：**控制面延迟与舰队规模无关**。一个"读沙箱"操作如果每次都扫全表，舰队从 1 涨到 12 时延迟会涨 12 倍；比值 3 允许实现细节有常数开销，但不允许线性扫描。这个主张在 §11（配额与记账）和 §10（对账循环）里都有对应实现。

### 本项目怎么做：测量工具本身也是被测对象

`tests/commercial_test.go:58-82` 的 `measure` 有一条容易被忽略的规则：

```go
// A sample that did not reach the server, or that came back with a status the
// caller did not expect, fails the test rather than being dropped: a latency
// distribution computed over requests that did not succeed is not a latency
// distribution, it is a distribution of refusals wearing its clothes.
```

**"拒绝的分布穿着延迟分布的衣服"** —— 这句话值一整节。如果你在测延迟时把 429 的样本丢掉，那么一个把请求全拒绝掉的实现会给你一个非常漂亮的 p50。所以它不丢弃样本，它让测试失败。

配套的三个工具：

1. **`warm`**（`tests/commercial_test.go:97-105`）——测量前先打几次废请求：

```go
// warm issues throwaway requests so a measurement never includes the cost of a
// cold connection pool, a cold page cache, or a first-write WAL append. Without
// this, whichever measurement happens to run first absorbs every one-time cost
// and the comparison between them stops being about the thing under test.
```

不 warm 的话，**排在前面的测量会吸收所有一次性成本**。这直接破坏比值：分母 `p50(/healthz)` 恰好排第一时，它会被污染成一个偏大的数，于是后面所有比值都变宽松。

2. **`tally`**（`tests/commercial_test.go:107-159`）——统计并发状态分布，并且**把 429 缺 `Retry-After` 单独计数**：

```go
if r.Status == http.StatusTooManyRequests && r.Header.Get("Retry-After") == "" {
	x.noRetryAfter++
}
```

因为对一个限流响应来说，`Retry-After` 是契约的一部分，它的缺失就是缺陷（见 §11 的令牌桶、§12 的错误包络）。

3. **`tally.fiveXX`**（`tests/commercial_test.go:144-159`）——任何 5xx 都不可接受：

```go
// A 5xx is never an acceptable answer to a well-formed request in this product:
// the governor has 401, 402, 403, 409, 429 and 501 available, and each of them
// says something a client can act on, whereas 500 says nothing.
```

这条断言的措辞方式值得学：**它先列出所有可用的语义化状态码，再说 500 没有信息量**。这不是"5xx 不好"的教条，是"你明明有话可说却选择了沉默"的批评。

### 替代方案：基准测试（`go test -bench`）为什么不够

`go test -bench` 能给出漂亮的 ns/op 数字，但它有两个结构性缺陷：

- 它跑在**进程内**，测的是函数，不是"客户端 → HTTP → 中间件 → 服务 → 后端 → 引擎"这条完整链路。本项目一半的延迟不在自己的代码里（exec 的 p50 是 26.885ms，而控制面自身只贡献约 10µs，数字见 §1）。
- 它没有"通过/失败"。基准数据是给人看的，不是给 CI 看的。

所以本项目用的是 `measure` + `atMost`（`tests/commercial_test.go:47-56`）：**唯一新增的断言原语就是 `atMost`**，其余全部复用已有的 `eq/truthy/lessThan`。注释解释了为什么：

```go
// It is deliberately the only new assertion primitive this file adds:
// everything else reuses eq/truthy/lessThan so that "measured and compared"
// never gets confused with "could not measure".
```

断言原语越少，"测量并比较了"和"根本没测成"越不容易混淆。

### 怎么验证

```bash
cd castle
make test-commercial    # 约 4 分钟
```

预期：8 个用例 PASS，日志里打印全部绝对数字（p50/p99/max），但**绝对数字没有任何一个参与断言**。想确认这一点：

```bash
grep -n 'atMost' tests/commercial_test.go
```

你会看到每一处的第二个参数都是表达式（比值），没有一个是字面毫秒数。

---

## 14.6 商用五维验收：把买方的口径拆成五个轴

### 问题

"我们通过了自己的测试"这句话，买方听不进去。买方的技术评审有自己的清单，通常是五个问题：

1. 并发下会不会错（**高并发**）
2. 安全边界在哪（**安全**）
3. 会不会挂（**可用性**）
4. 资源会不会还回来（**弹性**）
5. 启动和响应有多快（**启动实效与响应性**）

如果你的测试套件是按**代码模块**组织的（store 测一遍、service 测一遍、httpapi 测一遍），你没法回答这五个问题中的任何一个——因为每个问题都横跨所有模块。

### 原理：按"买方的问题"组织，还是按"代码的模块"组织

这两种组织方式不是风格差异，它们会导致**不同的覆盖空洞**：

- 按模块组织 → 每个模块都被测到，但"模块之间的组合行为"没人管。空闲回收涉及扫描器、存储、后端、配额四个模块，每个模块的单测都绿，组合起来可能根本不工作。
- 按问题组织 → 每个买方问题都有答案，但要小心**同一个断言在多个问题里重复出现**，于是绿灯数虚高。

本项目的处理方式是**先找出按模块组织时真实存在的空洞**，只补这些。`tests/commercial_test.go:3-33` 明确列出了四个空洞：

```go
//   - 启动实效: startup was only ever a 90-second timeout (Harness.WaitReady).
//     The suite could tell that a daemon had started, never how long it took.
//   - 响应性: latency was reported and deliberately not asserted — load_test.go's
//     header says so ("Latency is reported, not asserted"). ...
//   - 弹性: idle reclamation and TTL reclamation had *no* end-to-end test at
//     all. They were unit-tested (internal/service/service_test.go
//     TestIdleSweeperExpires) and never exercised through the API.
//   - 可用性: availability was only ever checked *after* load stopped ...
```

注意每一条都是**"原来是什么样"**的具体描述，不是"我们缺少覆盖"这种抽象说法。而且它明确承认 `load_test.go` 当年的立场（"延迟只报告不断言"）在**绝对阈值**上是对的、在**产品主张**上是错的——这个区分就是 14.5 的比值断言的由来。

安全轴不在这一份文件里，也给了解释：

```go
// Security does not appear here because it already has three dedicated suites
// (contract_test.go, containment_test.go, isolation_test.go) and restating it
// would produce a green test that proves nothing new.
```

**"复述一遍只会得到一个绿但不新的测试"** —— 这就是上面说的"绿灯数虚高"的自觉防范。

### 本项目怎么做：八个场景对应四个轴

| 用例 | 行号 | 轴 |
|---|---|---|
| `TestStartupIsMeasuredNotJustTimedOut` | :230 | 启动实效 |
| `TestAdmissionIsExactUnderConcurrency` | :303 | 高并发 |
| `TestLatencyIsBoundedByTheCheapestRequest` | :376 | 响应性 |
| `TestControlPlaneLatencyIsFlatInFleetSize` | :425 | 响应性 |
| `TestServiceStaysAvailableUnderSustainedLoad` | :491 | 可用性 |
| `TestOneTenantsOverloadIsNotAnotherTenantsLatency` | :607 | 高并发（隔离性） |
| `TestElasticityIdleReclaimIsActivityDriven` | :712 | 弹性 |
| `TestElasticityTTLReturnsQuotaWithoutAClientCall` | :783 | 弹性 |

第一个用例解决的是"启动从来只被超时约束，没被测量"。做法是在脚手架上加两个东西：`Options.NoStart` 和 `Harness.StartAndTime`（`tests/harness_test.go:376-382`）：

```go
// StartAndTime is Start with the startup duration returned instead of thrown
// away. Startup used to be nothing but a 90-second timeout: the suite could
// tell that a daemon had started, never how long it took, so a regression that
// turned a two-second boot into a forty-second one was invisible until it
// crossed the timeout.
```

注意最后的理由：**从 2 秒退化到 40 秒是不可见的，直到它超过超时阈值**。这是"有上限但无测量"的典型后果——你只有一个二元信号，而你需要一个连续量。

`Options.NoStart`（`tests/harness_test.go:248`）本身也是一个设计决策：

```go
// NoStart builds the fixture without launching the daemon, so a scenario can
// measure the launch itself. Without it every startup measurement is
// impossible: New() waits for readiness before it returns.
```

也就是说：**脚手架的默认行为（等就绪）会让它自己无法测量"到达就绪需要多久"**。这是脚手架设计里一个普遍的陷阱——便捷性会吃掉可测量性。

可用性轴的采样器（`tests/commercial_test.go:180-215`）解决的是另一个方向性问题：

```go
// readySampler polls /readyz on its own goroutine for the duration of a load
// run. Availability is not "the daemon answered after the traffic stopped", it
// is "the daemon kept answering while the traffic was happening", so the
// sampling has to be concurrent with the load to mean anything.
```

"**负载停止之后还活着**"和"**负载进行中一直活着**"是两件事。前者在原版 `load_test.go` 里就有（`the_daemon_is_still_healthy_after_load`），后者没有。一个在压力下把请求排队、压力消失后立刻恢复的实现，能通过前者，通不过后者。

弹性轴的两个用例，测的是**配额真的被归还**，而不只是"沙箱消失了"。`TestElasticityTTLReturnsQuotaWithoutAClientCall` 的名字里就写明了这一点：`WithoutAClientCall`——TTL 到期回收是服务端主动做的，不需要客户端调删除。如果实现是"标记删除，等客户端来取"，这个用例会失败。

### 替代方案：为什么不直接对齐行业基准（比如 YCSB / TPC）

行业基准的价值是**可比性**：你可以在广告里写"我们比 X 快 3 倍"。代价是：

- 基准的工作负载和你的真实工作负载不同。本项目 80% 的成本在容器引擎往返（exec p50 26.885ms，控制面只占 10µs），而通用基准主要压的是数据库式的读写。
- 基准不检查语义。一个把请求全拒绝的实现可以在吞吐基准上拿高分。

所以本项目的做法是**自己定义五条可证伪的产品主张，然后为每条主张设计一个比值断言**。它放弃了横向可比性，换取了"每条绿灯都对应一句可以对客户说的话"。

### 怎么验证

```bash
cd castle
make test-commercial 2>&1 | tail -40
```

预期：8 个 PASS，耗时约 4 分钟。慢是设计的：`Makefile:51-54` 说明了原因——

```makefile
# Two scenarios in it are slow by construction: idle and TTL reclamation have a
# 60s floor (domain.SandboxSpec.Validate), so this target takes about four
# minutes. That is the honest cost of testing expiry through the API instead of
# through a unit test with a fake clock.
```

**"这是按 API 口径测过期、而不是用假时钟做单测的诚实代价"** —— 用假时钟做单测当然快，但它测不到"扫描器真的被调度起来了、真的读到了正确的字段"这一整条链路。14.9 会给出一个真实的例子：正因为走了端到端，才抓到了一个假时钟永远抓不到的产品缺陷。

---

## 14.7 测试抓到的真实缺陷：为什么不删掉 `poc/`

### 问题

一个缺陷被修复之后，通常剩下一行 CHANGELOG 和一个回归测试。但几个月后有人问"这个修复真的必要吗"，或者"现在的写法和当时有什么不同"，答案是找不到的——因为**证据已经被清理掉了**。

### 原理：从"静态推断"到"可运行复现"

`poc/README.md:1-5` 说明了这个目录存在的理由：

```markdown
这些程序/脚本用于把审查报告中的结论从"静态推断"变成"可运行复现"。
**它们只读代码、不改产品逻辑**：Go 程序只调用 `internal/store` 的**导出 API**，
脚本只通过 REST API 操作 daemon。
```

两个约束都是有意的：

1. **只调用导出 API** —— PoC 不能钻进内部结构去制造问题，否则它证明的是"如果绕过封装会怎样"，而不是"用户能不能碰到这个 bug"。
2. **不改产品逻辑** —— PoC 是观察工具，不是补丁。

`poc/README.md:19-27` 的清单里有七项，前三项是纯存储层的（不依赖 Docker）：

| PoC | 命令 | 缺陷 | 预期输出 |
|---|---|---|---|
| 1 WAL 竞态 | `go run ./poc/walrace` | `Put*` 在锁外写 WAL，与 `Compact()` 并发导致已确认写入丢失 | 120/120 轮丢数据，丢失率约 4% |
| 2 撕裂帧 | `go run ./poc/tornframe` | "回滚"实现成了 `Truncate(walSize())`（空操作），坏帧之后已 fsync 的事务在重启时被整体丢弃 | 坏帧后 2 条已确认事务消失，WAL 374→94 字节 |
| 3 全局锁 | `go run ./poc/lockleak` | `lockFile` 是包级全局变量，关闭 Store A 会解掉 Store B 的锁并泄漏 fd | 关闭 A 后 B 的锁被释放；残留 1 个 fd |

PoC 2 是理解最深的一个：它的缺陷不是"崩溃会丢数据"（那是 WAL 的正常语义边界），而是**"声称的回滚其实什么也没做"**——`Truncate(walSize())` 把文件截断到它当前的长度，是一个空操作。这类 bug 在代码审查里几乎看不出来，因为函数名和注释都对，只有跑一遍才知道它没干活。WAL 的帧格式与恢复语义见 §9。

后四项是活体探针（`poc/probe_live.sh`、`poc/probe_isolation.sh`），各自起一个临时 daemon：`poc/README.md:30-31` 说明它们用独立端口（18123/18124）和独立数据目录，退出时 trap 杀掉，**不触碰你正在跑的服务**。

### 本项目怎么做：把 PoC 变成回归测试，而不是把 PoC 删掉

处理顺序是固定的三步：

1. **PoC 复现** —— 证明问题真实存在，且有可观测输出；
2. **修实现**；
3. **把"这个场景"变成回归测试** —— 不是把 PoC 变成测试，而是断言**产品行为**。

第三点的措辞很关键，CHANGELOG 里反复强调：新加的弹性场景断言的是**"回收真的发生了"**，而不是"响应体里有那个字段"。这两者的区别是：后者在"字段存在但没生效"时依然通过——这正是那个被抓到的缺陷的形态。

被抓到的产品缺陷是这样的：`internal/service/service.go` 的 `mergeSpec` 对 `spec.idle_timeout_seconds` **有默认回填，却没有请求覆盖分支**，而且它是唯一一个这样的字段。后果链：

- 校验照常通过（`domain/sandbox.go` 检查了区间）；
- 创建照常返回 **201**，不报错、不打日志；
- `docs/API.md` 把它文档化为可逐沙箱设置的旋钮；
- 而存下来的 spec **永远携带平台默认值**（默认 0 = 关闭）。

也就是说：**逐沙箱空闲回收在修复前根本不可达**。失败模式是最坏的一种——无错误、无日志、有 201，客户端以为自己设置了回收。

发现路径本身也值得记（CHANGELOG v1.1.5）：新场景在预算 60 秒、`sweep_every=1s` 下，95 秒都没回收。逐项排除了 `sweep_every` 没生效、沙箱不是 `running`、`IdleExpired` 判定算错，最后剩下唯一可能——**预算根本没写进沙箱**，于是读到 `mergeSpec`。

这个排查顺序是方法论：**先怀疑测试，再怀疑被测物，最后才怀疑"两者之间的连接"**。绝大部分"测试失败"其实出在第一项（脚手架问题，见 14.2），而最贵的 bug 出在第三项。

同一版还抓到一个**脚手架缺陷**：`WaitForState` 在 `limit <= 0` 时空指针 panic。起因是一次误写 `ttl+40`（无类型常量，目标类型是 `time.Duration`，于是成了**纳秒**），轮询循环一次都没执行，`last` 为 nil，然后在**失败信息里**解引用它。

这条被单独记录下来，因为它违反了一条底线：

> **脚手架不该"爆炸"，它应该"报告"。**

真实问题是"没有任何东西被观测到"，报出来的是一段栈回溯。修法是加正数校验 + nil 检查——**在生成错误信息的代码里崩溃，是最糟糕的崩溃位置**，因为它把唯一的线索也毁掉了。

### 替代方案：为什么不只写回归测试、不保留 PoC

只留回归测试的代价是：回归测试只会告诉你"现在坏了"，不会告诉你"坏成什么样"。PoC 给出的是**可运行的、定量的现象**（"120/120 轮丢数据，丢失率约 4%"），这个数字在讨论"这个 bug 有多严重"时比一句"测试红了"有用得多。

保留 PoC 的代价是维护：产品 API 变了，PoC 会腐坏。本项目用两条约束控制它：PoC 只调导出 API（上面第 1 点），且 `poc/README.md:33-40` 记录了环境适配（本机 `HTTP_PROXY` 会拦截对本机端口的请求，所以 curl 显式直连；本机 `ps` 被沙箱禁止，所以改用响应体大小度量）。**把环境噪声写进 README，PoC 才不会被人误判为"复现不出来"。**

### 怎么验证

```bash
cd castle
go run ./poc/walrace        # 不依赖 Docker
go run ./poc/tornframe
go run ./poc/lockleak
```

预期：三者都能复现 README 里写的现象（注意：它们复现的是**修复前**的行为；如果当前代码已修复，输出会变成"无丢失"——这时 PoC 的作用是**证明回归测试确实覆盖了那条路径**）。活体探针需要先 `make build` 且有 Docker。

---

## 14.8 派生件：图和 trace 必须由演练生成

### 问题

`docs/scenario-flow.html` 是一张流程图，展示 47 次调用经过哪些组件。它是给买方看的。

危险在于：这种图很容易变成**手写的**，而手写的图会和实现脱节。三个月后有人改了流程，图还是旧的，而它在一个"本文档所有内容都经过验证"的目录里。

### 原理：手写件会腐坏，派生件不会

判据很简单：**这个东西能不能被一条命令重新生成？** 能，它是派生件；不能，它是手写件，会腐坏。

`Makefile:65-69` 把这条规则写进了构建流程：

```makefile
# Regenerate the dynamic flow diagram FROM the last scenario trace. The
# diagram is derived, never hand-written: to change the flow, re-run the
# scenario rather than editing the picture.
scenario-flow: scenario
	python3 tests/render-scenario-flow.py
```

注意 `scenario-flow` **依赖 `scenario`**——你不能只重新渲染图，你必须先重跑演练。这条依赖是整条规则的执行机制：它让"改图"这个动作在物理上不可能，因为图不在版本控制的意义上存在，它只是最后一次演练的产物。

数据流是：

```
tests/e2e-enterprise-scenario.sh  →  docs/scenario-trace.jsonl  →  tests/render-scenario-flow.py  →  docs/scenario-flow.html
```

中间那份 `scenario-trace.jsonl` 是**机器可读的、每次真实调用的记录**。它比图更重要：图是给人看的，trace 是可以被断言的。

### 本项目怎么做：演练与 Go 套件的分工

`Makefile:58-63` 说清了演练的定位：

```makefile
# The enterprise walkthrough: one scenario, end to end, against a live daemon,
# writing a machine-readable trace of every real call it makes. It is not a
# substitute for the Go suites — it is the buyer-facing rehearsal, and the
# source of docs/scenario-flow.html.
```

"**不是 Go 套件的替代品**" —— 演练是一个**顺序的、单租户的、理想路径**的走查，它证明的是"这个产品能按文档走通一遍"。Go 套件证明的是"每个边界条件都被断言了"。两者不可互换：演练的 43 个断言里有很大一部分是"响应码符合预期"，而 Go 套件里有 83 个子场景是"在异常条件下仍然成立"。

演练的诚实性来自它**记录了它没证明什么**。这一点在 §16 会展开，因为它是产品定位的一部分，不是测试技术的一部分。

### 替代方案：为什么不手写文档 + 人工维护一份"已验证清单"

人工维护的清单的问题是它没有**时间戳**：清单上的"已验证"是三个月前那个人跑的。而 `scenario-trace.jsonl` 里每一次调用都有它发生的事实记录，重跑一次就得到新的一份。

代价是演练需要真实环境（Docker + 基础镜像），所以 `make scenario` 在 CI 上未必能跑。这是可接受的：它本来就是买方演示，不是 CI 门禁。

### 怎么验证

```bash
cd castle
make scenario-flow     # 会先跑演练，再渲染
git diff --stat docs/  # 观察哪些文件被重新生成
```

预期：`docs/scenario-trace.jsonl` 与 `docs/scenario-flow.html` 都被更新。如果你手改了 `scenario-flow.html` 然后重跑这条命令，你的改动会**被覆盖**——这正是设计意图。

---

## 14.9 四层测试与它们的准入条件

### 问题

"测试金字塔"这个词被用得太泛，以至于它经常变成一句口号。有用的是**每一层的准入条件**：什么样的东西必须在哪一层测，以及为什么。

### 原理：按"能不能在这一层观察到"分层

本项目实际是四层，分层依据不是"粒度"，而是**"被测行为在这一层是否可见"**：

| 层 | 位置 | 能观察到什么 | 观察不到什么 |
|---|---|---|---|
| 1 单元 | `internal/**/*_test.go` | 状态机、编解码、算法、错误映射 | 内核是否兑现、进程是否被真的杀掉 |
| 2 集成 | `internal/runtime/oci/`、`internal/store/` | 真实文件、真实引擎、真实锁 | 完整的 HTTP 契约、`/readyz` 语义 |
| 3 端到端 | `tests/` | 完整链路、真实内核、真实崩溃 | 无（最慢，所以要有准入条件） |
| 4 演练 | `tests/e2e-enterprise-scenario.sh` | 买方视角的完整故事 | 边界条件 |

第 3 层的准入条件是最严的一条，写在 `Makefile:24-32`：

```makefile
# The end-to-end suites live in ./tests as Go tests over a shared harness
# (daemon lifecycle, typed client, tenant and sandbox fixtures). They are the
# acceptance gate, so this target sets SBX_REQUIRE_DOCKER=1: a missing
# container runtime fails the run instead of skipping every scenario, because
# a suite that silently skips everything is indistinguishable from one that
# passes.
```

**"一个静默跳过一切的套件，和一个通过的套件，是不可区分的"** —— 这是 14.1 那个"无声探针"的套件级版本。默认行为必须是"缺环境就失败"，而不是"缺环境就跳过"。要跳过的人可以显式地直接跑 `go test ./tests/`。

### 本项目怎么做：测试代码也是产品代码

`tests/client_test.go:1-19` 的文件头有三条约束，每一条都对应一个真实踩过的坑：

```go
// Response *domain* types are imported from internal/domain on purpose: a
// Sandbox decoded here is the same value the service layer persisted, so a
// field rename breaks the suites at compile time instead of at 3am.
//
// Request *body* types are restated locally. That looks like duplication and
// is not: the wire shape is itself the contract under test, so a suite that
// compiled against an unexported server-side struct could never notice the
// wire name changing under it.
//
// The client holds no testing.T. Assertions take one explicitly. An earlier
// iteration captured the *parent* test in the client, so the first failure
// inside a subtest tore down the whole parent and silently skipped every
// scenario after it — the worst possible behaviour for a suite whose entire
// job is to report the status of many independent claims.
```

三条分别处理三个不同的失效方向：

1. **响应类型复用生产类型** —— 让"改名"变成编译错误。这是 14.8 那条"派生而非手写"原则在类型层面的应用。
2. **请求体类型本地重述** —— 反直觉但正确：请求体的**线上形状本身就是被测试契约**，如果测试直接用服务端的内部结构体，服务端改了 JSON tag 而测试编译通过，你就永远发现不了。**这一处"重复"是刻意的，因为被重复的东西正是被测物。**
3. **客户端不持有 `*testing.T`** —— 否则子测试里第一次失败会掀掉整个父测试，静默跳过后面所有场景。这就是 14.2 里"脚手架不该爆炸"的又一面。

第 3 条的后果形态值得记住：一个"报告多个独立主张"的套件，如果第一个失败会终结其余主张，它实际上从"N 条断言"退化成了"1 条断言"。

### 替代方案：为什么不用 BDD 框架（Ginkgo / Cucumber）

BDD 框架的价值是**可读的场景描述**（`Given ... When ... Then`），代价是：

- 它引入第三方依赖，直接违反本项目的零依赖约束（见 §16 对零依赖代价的完整讨论）；
- 它的断言库通常比 `if got != want { t.Fatalf(...) }` 更难调试——失败时你看到的是框架的表达树，不是你的值。

本项目用 `t.Run("一句主张", ...)` 达到同样的可读性（`tests/isolation_test.go` 的 22 个场景名就是例子），而不需要框架。**场景名写成一句完整的主张**，是 BDD 里唯一真正有价值的那部分，可以零成本拿走。

### 怎么验证

```bash
cd castle
make test              # 第 1 层：go test -race ./internal/...
make test-int          # 第 2 层：store + oci
make test-e2e          # 第 3 层：tests/ + pi-sbx harness
make scenario          # 第 4 层：演练
```

想验证准入条件真的生效，可以临时把容器引擎停掉再跑 `make test-e2e`：预期是**失败**（不是跳过）。

---

### 本章检验

1. 一条断言说"宿主目录还在"。请说出至少三种它会无条件通过、而沙箱其实完全没隔离的情况，并说明"正向对照"分别如何在每一种情况下让断言变红。

2. `tests/isolation_test.go:57` 断言 `id -u == 0` 且 `CapEff == 0`。如果只保留后者（不检查 uid），你会漏掉什么产品信息？如果只保留前者，你会错误地宣称什么？

3. 为什么比值断言的分母必须是一个"慢到能体现调度抖动，又轻到不碰后端和存储"的操作？如果分母换成"创建一个沙箱"，四条断言会各自发生什么变化？

4. `tests/client_test.go` 里，响应类型复用生产类型、请求体类型却本地重述。这两个决定看起来方向相反，请说明它们各自防的是什么，以及为什么不能统一。

5. `make test-e2e` 用 `SBX_REQUIRE_DOCKER=1` 让"缺环境"变成失败而不是跳过。请举出一个你自己的项目里"跳过被当成通过"的例子，并说明改成失败会带来什么代价。

**动手任务**：在 `tests/isolation_test.go` 里挑一个场景（建议 `no_host_directory_is_mounted`），先读懂它的金丝雀是怎么种的、怎么验证的、怎么收回的；然后**故意**在 `internal/runtime/oci/backend.go` 的 HostConfig 里加一个把宿主 `/tmp` 挂进去的绑定挂载，重跑这个场景，确认它变红。最后把改动还原。这一步的意义是：你会亲眼看到"断言能失败"，而这是断言唯一的价值。


# §15 Agent 集成与平台对接

## 15.1 为什么 Agent 特别需要沙箱：工具的语义变了

### 问题

一个人类开发者敲 `pip install requests`，他知道自己要装什么、装到哪里、失败了会怎样。一个 Agent 敲同一条命令时，这三件事它都不完全知道——它是根据对话上下文**推断**出来的。

推断会出错。而出错的形式对人类和对 Agent 是不同的：

- 人类的错误通常**局限在一条命令**（路径写错、包名拼错）；
- Agent 的错误可能是**一连串自洽但方向错误的命令**：它以为自己在项目目录里，于是 `rm -rf ./build`；它以为依赖装上了，于是连续重试三种安装方式；它在错误的分支上执行了 `git push --force`。

更根本的区别是**执行频率与人在回路的位置**。人类一小时敲几十条命令，每条都在自己的注视下。Agent 一次会话可能跑几百条命令，人只在最后看结果。所以"每一条命令都经过人眼审查"这个安全假设，在 Agent 场景下**不成立**。

这不是理论担忧。2026 年出现了第一个发给 agentic AI 系统的 CVE（`docs/RESEARCH.md:19` 记录了这条论据的来源），它的成因正是"Agent 拥有 shell 但没有边界"。

### 原理：从"工具调用"到"执行环境"

要理解沙箱在 Agent 栈里的位置，先区分两个经常被混用的概念：

| | 工具路由（tool routing） | 执行环境（execution environment） |
|---|---|---|
| 决定的是 | 这一条命令交给谁去执行 | 这一条命令在哪里执行、能碰到什么 |
| 谁负责 | Agent 宿主（pi 这类框架） | 沙箱运行时（本项目） |
| 改变命令本身吗 | 不改 | 不改 |
| 出问题时 | 命令没被送达 | 命令执行了，但被限制住了 |

这个区分是回答"如果 pi 自己内置了沙箱，我的项目不就冲突了吗"这个问题的关键——**这是两个不同的层**。工具路由是"把命令送出去"的机制，执行环境是"命令落地的地方"。框架提供前者是自然的，因为它掌握工具的定义；它提供后者则需要实现一整套运行时（镜像、快照、配额、租户、审计），那是另一个产品。

本项目只做后者，并且**通过 REST 把自己接进前者**。

### 本项目怎么做：把自己定位成"可替换的后端"

`docs/RESEARCH.md:16` 记录了调研结论，措辞很精确：

```text
| [earendil-works/pi](...) | ★109229，TS，MIT，极活跃 | **集成对象**（"本地 pi agent"）。官方文档《Run Pi in an isolated environment》给出 4 种接入模式；其中 Gondolin 模式 = pi 留在宿主机、把内置工具(bash/read/write/edit/grep/find/ls)路由进隔离环境——本产品按同一模式提供官方 pi 扩展 |
```

"按同一模式提供"这几个字很重要：**本项目是那个模式的一个后端实现，不是那个模式的竞争者**。后果是双面的：

- 好处：pi 换了更好的工具路由机制，本项目不用改（因为集成点是一个稳定的扩展 API）；
- 风险：pi 哪天自己把执行环境也做了，本项目在这一条集成路径上就被挤掉了。

第二个风险在 §16 会作为"已知风险"完整评估。这里只先把结构讲清楚：集成的价值取决于**上游框架的扩展机制是否稳定**，而这个稳定性不在你的控制之内。

### 替代方案：把整个 Agent 跑进沙箱里

第二个模式是"把 pi 进程本身放进容器"（`docs/RESEARCH.md:27` 提到本项目也提供这个镜像配方）。它的取舍：

| | 只路由工具（模式一） | 整个 Agent 进沙箱（模式二） |
|---|---|---|
| LLM 凭据位置 | 留在宿主机 | 必须进沙箱，或走代理 |
| 宿主目录 | 不挂载（本项目选择） | 通常要挂工作目录 |
| 沙箱逃逸的后果 | 拿不到凭据、碰不到宿主文件 | 凭据和宿主文件都在里面 |
| 改造成本 | 一个扩展文件 | 镜像 + 卷 + 网络配置 |

本项目选模式一，理由在 `integrations/pi/pi-sbx/index.ts:4-6` 写得很直白：

```ts
/**
 * Deployment model (pi docs: "Route tools through Gondolin" pattern):
 * the pi process and provider credentials stay on the host; bash commands
 * execute inside an isolated sandboxd sandbox through the REST API.
 */
```

**"pi 进程与 LLM 凭据留在宿主机"** 是这一整节的锚点。它决定了后面每一个设计选择：为什么宿主目录不挂载、为什么只需要一个 REST 客户端、为什么凭据不进沙箱。

### 怎么验证

```bash
cd castle
cat integrations/pi/pi-sbx/README.md | head -14
```

预期看到那张"什么在哪"的表：pi 进程与 LLM API Key 在宿主机；bash 命令与命令产生的文件在沙箱；**宿主机工作目录不进沙箱**。

---

## 15.2 官方四种隔离模式：pi 到底提供了什么

### 问题

在动手写集成之前，必须先确认一件事：**上游框架是否已经内置了沙箱**。如果内置了，你做的东西和它冲突；如果没内置，你要搞清楚它的扩展点在哪。

这个问题不能靠猜，也不能靠"我记得它有个 `--sandbox` 参数"。它必须去读官方文档，而且要区分两件事：

1. 文档推荐的做法是**框架自带**的还是**外部组件**？
2. 如果是外部的，框架提供的是**机制**还是**实现**？

### 原理：机制与实现的分离

pi 官方《Run Pi in an isolated environment》给出四种方式（以下为 2026-09 核对官方文档时的口径）：

| 方式 | 隔离由谁提供 | pi 提供什么 |
|---|---|---|
| 普通 Docker | 用户自己写的 Dockerfile / compose | 一份运行指引 |
| Docker Sandboxes | Docker 官方的 sandbox 能力 | 一份运行指引 |
| OpenShell | 第三方项目 | 一份运行指引 |
| Gondolin | 第三方项目 | **工具路由的配置方式** |

四种方式**全部依赖外部组件**。pi 自己不实现 namespace、不实现 seccomp、不实现镜像管理。它提供的是"把内置工具指向某个执行环境"的**机制**。

这个区分有一个直接的推论：**执行环境是可替换的后端**。pi 不会因为换了一个沙箱而失效，就像它不会因为换了一个 LLM provider 而失效——两者都是被路由到的目标。

官方文档里有两句必须原样记住的免责声明，因为它们是本项目边界声明的依据：

- 工具级隔离**不约束 pi 宿主进程本身**，也不约束那些不走隔离后端的扩展工具；
- 对 Gondolin 模式，官方明确说**不要把它当成凭据边界**——因为命令会继承宿主的环境变量。

第二句是本项目最重要的一处**分歧点**，下一节展开。

### 本项目怎么做：同一模式，相反的环境变量取舍

官方 Gondolin 模式把宿主的当前工作目录挂进隔离环境的 `/workspace`，并且命令继承宿主环境。本项目**反着来**，`integrations/pi/pi-sbx/README.md:12-14` 写明：

```text
| 宿主机工作目录 | **不进沙箱**（与 gondolin 的 /workspace 挂载不同；需要传文件时用 heredoc） |

这是刻意的取舍：不挂载宿主目录，沙箱内代码无法读写宿主机文件，边界更硬。
```

"边界更硬"不是一个形容词，它有具体含义，拆开是三条：

1. **不挂载 → 沙箱里没有任何指向宿主文件的路径**。`tests/isolation_test.go` 里那条金丝雀断言（见 §14.3）之所以能通过，前提就是这里没有绑定挂载。
2. **不继承宿主环境 → LLM API Key 不会出现在沙箱的进程环境里**。官方对 Gondolin 的"不要当凭据边界"警告，正是针对这一条。本项目因为凭据根本没进沙箱，这条警告不适用。
3. **代价 → 传文件要靠 heredoc**。这不是顺手做的选择，它是"边界更硬"的账单。`integrations/pi/pi-sbx/README.md:12` 括号里的那句"需要传文件时用 heredoc"就是这个账单的具体金额。

工具的描述文本里也把这条限制直接写给了 Agent（`integrations/pi/pi-sbx/index.ts:186-188`）：

```ts
	description:
		"Runs a bash command inside the sandboxd sandbox (isolated filesystem, no host access). " +
		"The host working directory is NOT visible inside the sandbox; upload files with `cat > file` heredocs via this tool if needed.",
```

这一步经常被漏掉：**工具描述也是 Agent 的输入**。如果不写清楚，模型会假设工作目录是可见的，然后生成一堆失败的命令，最后它会得出结论"这个环境有问题"。把限制写进工具描述，等于把边界告诉了使用者。

### 替代方案：挂载工作目录 + 双向同步

完整的 Gondolin 模式要挂 `/workspace`，并且 read/write/edit 也在隔离环境里操作。这需要一层双向同步：宿主的编辑要进沙箱，沙箱里产生的文件要能回来。

没做，理由写在 `integrations/pi/pi-sbx/README.md:65-69`：

```text
## 范围声明（诚实语义）

v1 路由 `bash` 一个工具（执行面），read/write/edit 仍在宿主机工作目录上操作。
"全部工具路由 + 工作目录挂载" 对应 gondolin 的完整模式，在路线图中
（需要 sandboxd 文件 API 之上的双向同步层）。安全边界以本文表格为准。
```

"**诚实语义**"这个词是本项目反复出现的原则（§16 会系统讲）：**做不到的事情要说出来，不能让它看起来像做到了**。这里的具体含义是——如果 v1 只路由了 `bash`，那么在文档、工具描述、README 三处都要写明，不能让使用者以为全套工具都进了沙箱。

### 怎么验证

```bash
cd castle
sed -n '1,20p' integrations/pi/pi-sbx/README.md
```

确认三件事：模式名（Gondolin）、凭据位置（宿主机）、工作目录（不挂载）。这三条是本节全部内容的判据。

---

## 15.3 扩展机制与工具覆写：Agent 侧一行不改

### 问题

要让 Agent 的命令进沙箱，最直觉的做法是改 Agent 的源码，把它的 `exec` 换成 HTTP 调用。这条路走不通：

- 你维护不了上游的分叉（它每天都有提交）；
- 每次上游升级你都要重新合一遍；
- 你的改动是"把命令发给我的服务"，这种改动**不会被上游接受**。

所以需求变成：**在不改 Agent 一行代码的前提下，替换它执行 bash 的方式**。

### 原理：覆写而不是分叉

现代 Agent 框架普遍提供扩展机制，形式通常是：宿主暴露一个 API 对象，扩展在加载时往里注册东西。关键在于**注册同名工具会发生什么**——如果宿主允许后注册者覆盖先注册者，你就得到了一个合法的替换点。

`integrations/pi/pi-sbx/index.ts:184-189` 就是这么做的：

```ts
	pi.registerTool({
		...createBashTool(process.cwd(), { operations: bashOperations() }),
		description:
			"Runs a bash command inside the sandboxd sandbox (isolated filesystem, no host access). " +
			"The host working directory is NOT visible inside the sandbox; upload files with `cat > file` heredocs via this tool if needed.",
	});
```

注意结构是 `createBashTool(process.cwd(), { operations: bashOperations() })`：**复用官方的 bash 工具工厂，只替换它的 `operations`**。

这个选择比"自己写一个 bash 工具"好，好在三处：

1. **工具的名字、参数 schema、输出格式全部继承官方**。模型看到的还是那个它熟悉的 `bash`，提示词里关于 bash 的一切知识都还适用。
2. **`createBashTool` 负责把 ops 的输出打包成 `result.content[].text`**，这是 pi 的结果格式；自己写就要重新实现，而且容易和上游的格式演进脱节。
3. **替换面被压到最小**：只有一个对象 `BashOperations`，只有一个方法 `exec`。上游改了工具的描述、改了参数校验、改了结果结构，这个扩展都不用动。

`createBashTool(process.cwd(), ...)` 里的 `process.cwd()` 看起来和"工作目录不进沙箱"矛盾，其实不矛盾：它填的是工具被告知的"当前工作目录"这个**元数据**，而 `operations.exec` 的第二个参数 `_cwd` 被**显式忽略了**（`index.ts:153`）：

```ts
		exec: async (command, _cwd, { onData, signal, timeout }) => {
```

参数名以 `_` 开头是 TypeScript/Go 里都通用的"我知道它存在但我不用"的记号。这里不用它是**安全决策**：把宿主的 cwd 传进沙箱，等于告诉沙箱里的进程一个宿主路径，那既没意义（沙箱里不存在）又泄漏信息（暴露宿主目录结构）。

### 本项目怎么做：`BashOperations` 的三个契约

`integrations/pi/pi-sbx/index.ts:151-182` 实现 `bashOperations()`。它要满足三个契约，缺一个模型就会"变瞎"或"变聋"：

```ts
	function bashOperations(): BashOperations {
		return {
			exec: async (command, _cwd, { onData, signal, timeout }) => {
				const active = await acquire();
				const effectiveTimeout = timeout && timeout > 0 ? timeout : DEFAULT_EXEC_TIMEOUT;
				const controller = new AbortController();
				const onAbort = () => controller.abort();
				signal?.addEventListener("abort", onAbort, { once: true });
				const timer =
					effectiveTimeout > 0
						? setTimeout(() => controller.abort(), effectiveTimeout * 1000)
						: undefined;
				try {
					// The daemon enforces the hard deadline server-side; the
					// client abort just stops waiting for the response.
					const result = await active.exec(command, effectiveTimeout + 5);
					// BashOperations streams output through onData as bytes —
					// dropping it here would leave the agent blind to results.
					const encoder = new TextEncoder();
					if (result.stdout) onData(encoder.encode(result.stdout));
					if (result.stderr) onData(encoder.encode(result.stderr));
					return { exitCode: result.exitCode };
				} catch (error) {
					if (signal?.aborted) throw new Error("aborted");
					throw error;
				} finally {
					if (timer) clearTimeout(timer);
					signal?.removeEventListener("abort", onAbort);
				}
			},
		};
	}
```

**契约一：输出必须回到 `onData`。** 注释说得很重——"dropping it here would leave the agent blind to results"（丢掉它，Agent 就瞎了）。这是集成代码里最常见的 bug：命令执行成功了，但结果没送回模型，于是模型认为命令没有输出，接着重复执行同一条命令。

注意 `onData` 接收的是**字节**（`encoder.encode(...)`），不是字符串。这是宿主定义的流式输出契约。

**契约二：退出码必须回传。** `return { exitCode: result.exitCode }` —— 模型靠退出码判断成败。exit 7 必须真的变成 7，否则模型会以为命令成功。`tests/pi-sbx-harness.mjs:102-106` 专门断言了这一条。

**契约三：取消信号必须被尊重。** `signal` 是宿主发来的"用户中断了"信号。处理它有两层：

```ts
					const result = await active.exec(command, effectiveTimeout + 5);
```

`+5` 这一处最容易误解：传给 daemon 的超时比客户端的超时**多 5 秒**。注释解释了原因——"daemon 在服务端强制硬截止时间；客户端的 abort 只是不再等响应"。

意思是：客户端的 `AbortController` 只切断 HTTP 等待，**不切断沙箱里的进程**。真正的截止时间由服务端执行（见 §12 的请求体上限与超时语义）。如果两边用同一个数字，客户端会在服务端之前放弃，于是你得到一个"客户端超时但其实成功了"的状态——而下一轮模型会重试，产生副作用。让客户端多等 5 秒，服务端的结果就能正常回来。

`AbortController` 的两个触发源（`signal` 与 `timer`）都注册了清理：`finally` 里 `clearTimeout(timer)` 和 `removeEventListener`。漏掉 `removeEventListener` 会累积监听器；漏掉 `clearTimeout` 会让 Node 的定时器在测试结束后仍然挂着——后者在 `pi-sbx-harness.mjs` 里表现为"进程不退出"。

### 替代方案：为什么不用包装脚本（`SBX=1 sh -c "..."`）

有个更简单的思路：不改工具，改环境——把 `bash` 换成一个包装脚本，脚本内部调 REST。没这么做的原因是它把边界放在了错误的地方：

- 包装脚本运行在**宿主机**上，它自己是宿主进程。它要读 `SBX_API_KEY`（宿主环境变量），也就是说**凭据出现在每一个被包装的 shell 的环境里**——包括 Agent 可能执行的 `env` 或 `/proc/self/environ`。
- 它无法区分"Agent 发起的命令"和"Agent 发起的命令里又启动的 shell"。嵌套会放大凭据暴露面。
- 它拿不到结构化输出：只能拼字符串，退出码和 stdout/stderr 的边界靠约定。

扩展覆写法把这些都避免了：凭据只在扩展进程的内存里（不进任何子进程环境），输出是结构化的 JSON，退出码是字段。

### 怎么验证

```bash
cd castle
sed -n '151,190p' integrations/pi/pi-sbx/index.ts
```

对照三个契约：输出回 `onData`、退出码回传、取消信号被处理（`AbortController` + `finally` 清理）。

---

## 15.4 REST 客户端：错误映射、懒获取与释放

### 问题

一个"能跑"的 REST 客户端和"能在 Agent 场景里用"的 REST 客户端，差别在三个地方：错误怎么说、沙箱什么时候建、会话结束怎么办。

### 原理：错误信息是给模型看的

`integrations/pi/pi-sbx/index.ts:34-43`：

```ts
class SandboxdError extends Error {
	status: number;
	code: string;

	constructor(status: number, code: string, message: string) {
		super(`sandboxd [${code}] (${status}): ${message}`);
		this.status = status;
		this.code = code;
	}
}
```

消息格式是 `sandboxd [code] (status): message`。为什么要把 `code` 放在最显眼的位置？因为**模型读到的是这段字符串**，它要据此决定下一步。

对比两种错误文本：

| 文本 | 模型能推出什么 |
|---|---|
| `HTTP 429` | "失败了，重试" |
| `sandboxd [rate_limited] (429): create bucket empty, retry in 20s` | "这是限流，20 秒后重试，不是我的命令错了" |

后者让模型能区分"重试"和"改命令"。前者会让模型把所有失败都当成"命令写错了"，于是它开始改命令——改一个本来正确的命令。

`code` 之所以可信，是因为服务端的错误包络里它和 status 有确定的映射（见 §12 的错误包络与 `retryable`）。客户端的职责是**把那个映射原样传达给调用方**，不要自己猜。

`request` 方法（`index.ts:59-80`）里还有一个细节值得学：

```ts
		const text = await resp.text();
		let payload: any = undefined;
		try {
			payload = text ? JSON.parse(text) : undefined;
		} catch {
			payload = { raw: text };
		}
		if (!resp.ok) {
			const err = payload?.error ?? {};
			throw new SandboxdError(resp.status, err.code ?? "unknown", err.message ?? text);
		}
```

解析失败时不是抛异常，而是把原文塞进 `{ raw: text }`。理由：**错误路径上不能产生新的错误**。如果服务端返回了一个非 JSON 的 500 页面（比如代理返回的错误页），而客户端在解析它时抛异常，那么真正的错误信息（那个 HTML）就丢了。保留原文，模型至少能看到"服务端说了什么"。

### 本项目怎么做：懒获取 + 幂等缓存

沙箱什么时候创建？两个极端都不对：

- 扩展加载时就建 —— 用户可能这次会话根本不用 bash，白建一个沙箱（占配额、占内存）；
- 每次 `exec` 都建 —— 荒谬。

`integrations/pi/pi-sbx/index.ts:132-149` 用的是**带幂等缓存的懒获取**：

```ts
export default function (pi: ExtensionAPI) {
	let client: SandboxClient | undefined;
	let starting: Promise<SandboxClient> | undefined;

	async function acquire(): Promise<SandboxClient> {
		if (client) return client;
		if (!starting) {
			starting = SandboxClient.acquire()
				.then((c) => {
					client = c;
					return c;
				})
				.finally(() => {
					starting = undefined;
				});
		}
		return starting;
	}
```

注意缓存的是 **Promise，不是结果**。这一点是关键：如果两个 `exec` 并发进来（Agent 完全可能并发调工具），缓存结果会导致两个都还没建好、于是建两次；缓存 Promise 则第二个调用会**复用第一次的进行中的创建**。

`finally` 里清空 `starting` 也是必须的：如果创建失败而 `starting` 留着，后续所有 `exec` 都会拿到同一个失败的 Promise，永久失败。清空之后，下一次调用会重新尝试。

`SandboxClient.acquire()`（`index.ts:82-103`）里有两个配置路径，对应两种用法：

```ts
	static async acquire(): Promise<SandboxClient> {
		if (!SBX_API_KEY) {
			throw new Error("SBX_API_KEY is required");
		}
		if (SBX_SANDBOX_ID) {
			const client = new SandboxClient(SBX_SANDBOX_ID);
			await client.request("GET", `/v1/sandboxes/${SBX_SANDBOX_ID}`);
			return client;
		}
		...
```

`SBX_SANDBOX_ID` 路径**先 GET 一次确认沙箱存在**。这不只是校验：它让"附加到一个已存在的沙箱"这个用法显式暴露失败。如果你不确认就直接用，第一次 `exec` 会拿到 404，而那个 404 出现在一条无关命令的上下文里，非常难排查。

创建路径（`index.ts:98-101`）用的是 `?wait=true`：

```ts
		const created = await new SandboxClient("").request("POST", "/v1/sandboxes?wait=true", {
			template_id: SBX_TEMPLATE,
			...(Object.keys(spec).length ? { spec } : {}),
		});
```

`wait=true` 意味着创建请求等到沙箱真的可用才返回（对比 §14.4 混沌测试里用 `?wait=false` 制造飞行中状态）。这里必须用 `true`，因为下一步就是 `exec`——拿到一个还在创建的沙箱，第一条命令就会失败。

`...(Object.keys(spec).length ? { spec } : {})` 这个展开的含义是：**一个字段都没设的时候，请求体里根本不带 `spec`**。这避免了发送 `{"spec":{}}` 触发服务端的"空 spec 但有内容"分支。

### 替代方案：为什么不用 WebSocket / 长连接流式 exec

本项目 v1 的 exec 是**同步的**：发一条命令，等它跑完，拿回全部输出（`index.ts:105-120`）：

```ts
	async exec(command: string, timeoutSeconds: number): Promise<{ exitCode: number; stdout: string; stderr: string }> {
		const result = await this.request("POST", `/v1/sandboxes/${this.sandboxId}/exec`, {
			command,
			timeout_ms: Math.max(1, Math.round(timeoutSeconds * 1000)),
			max_output: 1 << 20,
		});
		const decode = (b64: string | null | undefined): string => {
			if (!b64) return "";
			return Buffer.from(b64, "base64").toString("utf8");
		};
		return {
			exitCode: result.exit_code ?? 0,
			stdout: decode(result.stdout),
			stderr: decode(result.stderr),
		};
	}
```

`max_output: 1 << 20` 是 1 MiB 上限——**客户端主动限制自己愿意接收的量**。这不是服务端强制的（服务端自己的上限语义见 §12），但客户端设一个，可以防止一条 `cat /dev/urandom` 把整个扩展进程的内存吃光。

同步 exec 的代价是**输出不流式**：一个跑 30 秒的命令，前 29 秒模型什么都看不到。对 Agent 来说这会导致它以为卡住了。真正的流式需要 PTY/WebSocket，那在路线图里（`docs/RESEARCH.md:34`）。

v1 选择同步的理由是**接口形状简单**：一个 POST、一个 JSON 响应，任何语言的客户端都能写。而流式需要长连接管理、心跳、重连——对一个"先用起来"的集成来说，这个复杂度不值得。

`stdout`/`stderr` 是 base64 编码的，所以要 `decode`。为什么用 base64 而不是直接字符串？因为命令输出可能是任意字节（二进制、非法 UTF-8），JSON 字符串装不下。这个选择在 §12 的 exec 契约里有完整讨论。

### 怎么验证

```bash
cd castle
sed -n '82,130p' integrations/pi/pi-sbx/index.ts
```

对照：凭据校验 → 附加路径先确认 → 创建路径 `wait=true` → `max_output` 上限 → base64 解码。

---

## 15.5 会话结束必须释放：否则配额会漏

### 问题

Agent 会话结束时，沙箱怎么办？如果不管，它会一直活着直到 TTL 到期。这会带来三个后果：

1. **配额被占住**：租户的并发沙箱上限是有限的（见 §11），漏掉的沙箱会让后续会话创建失败；
2. **宿主资源被占住**：容器在跑，内存在占；
3. **最隐蔽的一条**：下次会话可能会碰到一个**带着上次残留文件**的沙箱——如果它是从模板建的，那倒还好；如果是复用的，Agent 会看到不属于这次会话的东西。

### 原理：释放要挂在上游的生命周期事件上

正确的做法是订阅宿主的会话结束事件，而不是靠进程退出钩子。原因：扩展进程的退出时机不由你控制，而宿主的事件是语义明确的。

`integrations/pi/pi-sbx/index.ts:199-203`：

```ts
	pi.on("session_shutdown", async () => {
		const active = client;
		client = undefined;
		if (active) await active.release();
	});
```

两行里有两处讲究：

1. **`const active = client; client = undefined;`** —— 先取出、立刻清空，再 await。顺序不能反。如果先 `await` 再清空，那么在 await 期间进来的新 `exec` 会拿到一个正在被释放的 client。先清空，新的 `exec` 会走 `acquire()` 重新建一个。

2. **`release()` 自己吞掉异常**（`index.ts:122-129`）：

```ts
	async release(): Promise<void> {
		if (SBX_KEEP) return; // leave the sandbox running (e.g. to inspect later)
		try {
			await this.request("DELETE", `/v1/sandboxes/${this.sandboxId}?force=true`);
		} catch {
			// shutdown path: daemon may already be gone; nothing to do
		}
	}
```

注释给出理由："shutdown path: daemon may already be gone"（关停路径上，daemon 可能已经不在了）。这是 §14.2 那条"清理路径上的错误必须静默"原则的又一次应用——**在关停路径上抛异常，会把一次成功的会话变成一次失败的会话**。

`?force=true` 的含义是对运行中的沙箱也强制删除（语义见 §8 的生命周期）。这里必须带，因为会话结束时沙箱几乎肯定是 `running`。

`SBX_KEEP=1` 是一个调试开关：保留沙箱，事后用 `sbxctl exec` 进去看。它的价值在集成开发阶段——你需要看到"Agent 的沙箱里到底留下了什么"。

### 本项目怎么做：`pi-sbx` 命令作为可观测入口

`integrations/pi/pi-sbx/index.ts:191-197` 注册了一个 `/sbx` 命令：

```ts
	pi.registerCommand("sbx", {
		description: "Show sandboxd sandbox status",
		handler: async (_args, ctx) => {
			const active = await acquire();
			ctx.ui.notify(`sandboxd sandbox: ${active.id} @ ${SBX_URL}${SBX_KEEP ? " (kept after session)" : ""}`, "info");
		},
	});
```

这个命令的作用不是功能性的，是**诊断性的**：它让你在会话中间确认"我现在到底在哪个沙箱里"。集成排障时最常问的问题就是"这个命令跑在哪"，而这个命令给出一个可以直接复制去 `sbxctl` 里查的 ID。

它顺便把 `SBX_KEEP` 状态显示出来——因为"沙箱会不会被保留"这件事在排障时最容易搞错：你以为沙箱还在，其实它已经被释放了。

### 替代方案：为什么不用 TTL 兜底就够了

沙箱有 TTL（`docs/RESEARCH.md` 与 §8 都讲了），到期自动回收。那为什么还要显式释放？

因为 TTL 是**安全网，不是资源管理策略**。它的量级是分钟到小时，而配额是以"并发数"计的。一个 TTL 30 分钟的沙箱，在它过期前的 30 分钟里一直占着一个并发位。一个用户开十次会话，你的配额上限就得是十。

而且 TTL 兜底有一个更难的问题：**它让泄漏变成不可见的**。你不知道自己漏了，因为最终确实回收了。等到配额告警的时候，你已经漏了一百个。

### 怎么验证

```bash
cd castle
make build
node tests/pi-sbx-harness.mjs
```

这个 harness 的最后一步（`tests/pi-sbx-harness.mjs:108-112`）专门测释放：

```js
	const before = (await api("GET", "/v1/sandboxes", undefined, TKEY)).data.count;
	await registered.handlers.session_shutdown(undefined, ui);
	await new Promise((r) => setTimeout(r, 800));
	const after = (await api("GET", "/v1/sandboxes", undefined, TKEY)).data.count;
	if (before >= 1 && after === 0) ok(`session_shutdown released the sandbox (${before} → 0)`); else bad(`sandbox not released (${before} → ${after})`);
```

注意 `before >= 1` 这个条件：**它先确认确实有沙箱存在**。这就是 §14.3 讲的正向对照——如果创建那一步静默失败了，`after === 0` 也会成立，测试会假绿。加上 `before >= 1`，这个断言才真的在测"释放"。

---

## 15.6 端到端验证：怎么在没有 LLM Key 的情况下测集成

### 问题

要测"Agent 的命令进沙箱了"，最笨的办法是开一个真的 Agent 会话，让它跑一条命令，然后看结果。这条路有三个问题：

- 需要 LLM API Key（每次跑都要花钱、要联网）；
- 模型的输出不确定（它可能不调 bash，可能调三次）；
- 跑一次要几十秒到几分钟。

一条集成测试如果依赖模型的行为，它就是一个 flaky 测试（见 §14.5）。

### 原理：测试集成，不测试模型

判据是：**被测的是"扩展把命令送到哪、把结果怎么带回来"，不是"模型决定跑什么命令"**。模型只是 `BashOperations.exec` 的调用方，而调用方可以被替换。

`tests/pi-sbx-harness.mjs:1-6` 的文件头说得很清楚：

```js
// Harness that loads the REAL pi-sbx extension (Node type-stripping), stubs
// the pi ExtensionAPI surface, and drives the bash-tool path end-to-end
// against a live sandboxd daemon. No LLM key needed: this tests the
// extension, not the model.
```

三个关键词：**REAL extension**（加载的是真实扩展代码，不是重写的简化版）、**stub ExtensionAPI**（只替换宿主的 API 表面）、**live daemon**（后端是真的）。

这个组合决定了它测到什么、测不到什么：

| 被测 | 未被测 |
|---|---|
| 扩展是否注册了 bash 覆写 | pi 宿主是否真的让后注册者覆盖先注册者 |
| 命令是否真的在沙箱里执行 | 模型是否会选择调用 bash |
| 输出/退出码是否回到调用方 | pi 的结果渲染 |
| 会话结束是否释放沙箱 | 真实 LLM 的延迟 |

第一行右侧那条"未被测"值得留意：它意味着**如果 pi 改了工具覆写的语义，这个 harness 不会发现**。这是 stub 测试的固有边界，也是为什么它在 `Makefile:31-33` 里是 `make test-e2e` 的一部分而不是全部（`Makefile:31-33`，在 Go 套件之后跑）。

### 本项目怎么做：stub 的三个层次

**第零层：harness 自己把依赖起全了。** 后面三层能跑的前提是有一个 live daemon，而它不假设你已经起了一个——`tests/pi-sbx-harness.mjs:31` 用一份临时配置 spawn 出自己的 daemon，`:51-53` 用引导 admin key（`:17`）现建一个租户和一把 Key，`:57-61` 把 Key 塞进扩展要读的环境变量里。这解释了它为什么只需要 Docker：**连租户从哪来都不依赖外部状态**。

**第一层：假造宿主的 API 对象**（`tests/pi-sbx-harness.mjs:64-79`）：

```js
	const registered = { tools: {}, commands: {}, handlers: {} };
	mod.default({
		registerTool: (t) => { registered.tools[t.name ?? "bash"] = t; },
		registerCommand: (name, c) => { registered.commands[name] = c; },
		on: (event, h) => { registered.handlers[event] = h; },
	});

	if (registered.tools.bash) ok("bash tool override registered"); else bad("bash tool not registered");
	if (registered.handlers.session_shutdown) ok("shutdown handler registered"); else bad("no shutdown handler");
```

三个注册函数都只是**记录**，不执行任何宿主逻辑。这足够验证"扩展在加载时做了正确的事"，而这正是集成最容易坏的地方（工具没注册 = 命令还在宿主机跑，且没有任何报错）。

`registerTool: (t) => { registered.tools[t.name ?? "bash"] = t; }` 里的 `?? "bash"` 是一个诚实的兜底：如果工具没有名字，就假设它是 bash。这不是掩盖问题，因为下一行立刻断言 `registered.tools.bash` 存在。

**第二层：假造 ExtensionContext 里被 bash 工具碰到的部分**（`tests/pi-sbx-harness.mjs:74-79`）：

```js
	const ui = {
		ui: { setStatus() {}, notify() {}, theme: { fg: () => "" } },
		sessionManager: { getSessionId: () => "harness-session", getSessionFile: () => "/tmp/harness-session.jsonl" },
		model: "harness-model",
	};
```

注释说明了为什么只 stub 这几片："pi's bash tool packages ops output into `result.content[].text` and throws a typed error carrying the output for non-zero exits"。也就是说，**需要先读懂官方 bash 工具会碰 context 的哪些字段**，才能 stub 得对。这是 stub 测试的隐藏成本：你依赖的是上游的实现细节。

**第三层：真的执行、真的断言**（`tests/pi-sbx-harness.mjs:84-106`）：

```js
	const result = await registered.tools.bash.execute(
		"call-1",
		{ command: "echo pi-sbx-works && uname -m" },
		undefined,
		() => {},
		ui,
	);
	const streamed = resultText(result);
	if (streamed.includes("pi-sbx-works")) ok("command output surfaced in tool result"); else bad("no output: " + JSON.stringify(result));
	if (streamed.includes("aarch64") || streamed.includes("x86_64")) ok("real execution inside the sandbox (arch visible)"); else bad("arch missing");
```

第二条断言（`uname -m` 返回真实架构）是这一组里最巧的一条：它证明命令**真的在一个真实系统上执行了**，而不是被 mock 返回的假字符串。一个把 `exec` 换成 `return "ok"` 的错误实现能通过第一条（如果它把 "pi-sbx-works" 硬编码进去的话），过不了第二条。

**第三条断言直接测那个安全决策**（`tests/pi-sbx-harness.mjs:95-100`）：

```js
	let isoText = "";
	try {
		const iso = await registered.tools.bash.execute("call-2", { command: "test -d /workspace && echo mounted || echo not-mounted" }, undefined, () => {}, ui);
		isoText = resultText(iso);
	} catch (e) { isoText = String(e?.message); }
	if (isoText.includes("not-mounted")) ok("host workspace NOT mounted (harder boundary by design)"); else bad("workspace state: " + isoText);
```

**"宿主工作目录没被挂载"这条产品决策，有一条专门的测试守着**。这是本节最重要的一点：15.2 里那个"反着来"的取舍，不是文档里的一句话，它是一条会变红的断言。

`try/catch` 包住它是因为非预期输出可能让官方 bash 工具抛异常（见上面 stub 的注释：非零退出会抛带输出的 typed error），而异常消息里也含着我们要找的字符串，所以两个分支的文本都收集。

### 替代方案：为什么不用真的 LLM 做一次冒烟测试

真实的端到端（真模型 + 真扩展 + 真 daemon）有价值，但它的定位是**演示**不是**测试**。本项目把它放在 README 里作为"已验证的端到端路径"记录（`integrations/pi/pi-sbx/README.md:51-52`），而不是放进 CI：

```text
已验证的端到端路径：GLM-5.3-flash 作为 pi 的模型，bash 工具经本扩展在
sandboxd 沙箱内执行（宿主机看不到沙箱内写入的文件）。
```

记录它，是因为**外部集成的可信度来自"真的有人跑通过"**；不放进 CI，是因为模型行为不可复现。

### 怎么验证

```bash
cd castle
make build
node tests/pi-sbx-harness.mjs
```

预期输出：

```text
  ✓ bash tool override registered
  ✓ shutdown handler registered
  ✓ command output surfaced in tool result
  ✓ real execution inside the sandbox (arch visible)
  ✓ host workspace NOT mounted (harder boundary by design)
  ✓ non-zero exit surfaced (...)
  ✓ session_shutdown released the sandbox (1 → 0)

pi-sbx harness results: pass=7 fail=0
```

它不需要 Docker 之外的任何东西，也不需要 LLM Key。它是 `make test-e2e` 的最后一步（`Makefile:33`）。

---

## 15.7 边界声明：v1 到底覆盖了什么

### 问题

一个集成最容易出问题的时间点，不是写的时候，是**使用者以为它做到了它没做到的事**的时候。

具体到 `pi-sbx`：如果使用者以为 read/write/edit 也进了沙箱，他会放心地让 Agent 改文件——而那些改动实际发生在宿主机上。

### 原理：范围声明要写在三个地方

本项目的做法是**同一个声明在三处出现**，因为使用者会从三个入口进来：

1. **README 的独立章节**（`integrations/pi/pi-sbx/README.md:65-69`）—— 人从仓库读；
2. **工具描述**（`index.ts:186-188`）—— Agent（模型）从工具列表读；
3. **部署形态表**（`integrations/pi/pi-sbx/README.md:8-12`）—— 人从"什么在哪"这张表读。

三处内容必须一致，否则使用者会挑那个对他有利的理解。

`integrations/pi/pi-sbx/README.md:65-69` 的原文：

```text
## 范围声明（诚实语义）

v1 路由 `bash` 一个工具（执行面），read/write/edit 仍在宿主机工作目录上操作。
"全部工具路由 + 工作目录挂载" 对应 gondolin 的完整模式，在路线图中
（需要 sandboxd 文件 API 之上的双向同步层）。安全边界以本文表格为准。
```

最后一句"**安全边界以本文表格为准**"是关键：当文档的不同部分看起来有冲突时，它指定了权威来源。这避免了"我觉得它应该也隔离了文件"这种推断。

### 本项目怎么做：什么在沙箱、什么不在

| 内容 | 位置 | 依据 |
|---|---|---|
| pi 进程 | 宿主机 | `integrations/pi/pi-sbx/README.md:10` |
| LLM API Key | 宿主机，且**不进沙箱进程环境** | `integrations/pi/pi-sbx/README.md:10`、`index.ts:4-6` |
| bash 命令 | 沙箱 | `integrations/pi/pi-sbx/README.md:11` |
| 命令产生的文件 | 沙箱，宿主不可见 | `integrations/pi/pi-sbx/README.md:11` |
| 宿主机工作目录 | **不进沙箱** | `integrations/pi/pi-sbx/README.md:12` |
| read/write/edit | 宿主机工作目录 | `integrations/pi/pi-sbx/README.md:67` |
| 会话结束 | 沙箱被销毁（除非 `SBX_KEEP=1`） | `index.ts:199-203` |

这张表同时也是**对官方 Gondolin 模式的两处偏离**的清单：工作目录不挂载、只路由 bash。这两处偏离都让边界更硬，代价分别是"传文件要靠 heredoc"和"文件操作仍在宿主"。第三列则是有意保留的：声明可信不是因为它写得诚恳，而是**每一条都能被翻到源码那一行去对**（这条原则 §16.3 会系统讲）。

### 替代方案：为什么不先做完整模式再说

完整的 Gondolin 模式（七种工具全路由 + 工作目录挂载）看起来更"完整"。没先做它，理由是可核对性：

- 只路由 bash 时，**边界是一句话**："除了 bash 命令本身，别的都不在沙箱里"。这句话可以被一条测试守住（`not-mounted`）。
- 全路由时，边界变成"七种工具的行为 + 双向同步的一致性 + 冲突解决"，而同步层的正确性**极难断言**（文件在两边同时被改怎么办？部分写失败怎么办？）。

先做窄而可核对的那一版，是 §16 会系统讲的"可核对性优先"原则的一次应用。

### 怎么验证

```bash
cd castle
sed -n '65,69p' integrations/pi/pi-sbx/README.md
sed -n '186,189p' integrations/pi/pi-sbx/index.ts
```

对照两处文字是否一致。如果不一致，那是一个真实的文档缺陷——因为使用者会各取所需。

---

## 15.8 平台对接的通用形态：把耦合面压到两个挂载点

### 问题

换一个 Agent 框架，`pi-sbx` 还剩多少能用？值得问，是因为**集成代码的寿命和框架的寿命不对齐**：框架可以每月发版、改扩展 API，而"送命令进沙箱、取回结果、会话结束释放"这三件事不会因为换框架而改变。

### 原理：三件套里只有两件与宿主有关

按"是否依赖 pi"把 `integrations/pi/pi-sbx/index.ts` 切开：

| 部件 | 做什么 | 依赖宿主吗 | 位置 |
|---|---|---|---|
| 工具覆写 | 让宿主的命令落到自己手里 | 依赖：要找到注册点并覆盖同名工具 | `index.ts:184-189` |
| REST 客户端 | 把命令送到运行时，把输出与退出码取回 | **不依赖**：只依赖 HTTP 契约 | `index.ts:45-130` |
| 会话生命周期挂钩 | 会话结束时释放沙箱 | 依赖：要知道事件叫什么名字 | `index.ts:199-203` |

中间那行是重点：`index.ts:45-130` 这 86 行没有一行引用 pi，它用全局 `fetch`，只认创建（`index.ts:98`）、存在性确认（`index.ts:88`）、exec（`index.ts:106`）、删除（`index.ts:125`）四个端点。整段复制到别的项目里一行不用改——换框架的工作量不是"重写一个集成"，而是**找到两个挂载点、复制一段客户端**。

### 本项目怎么做：为什么只提供 HTTP API

`docs/POSITIONING.md:144` 的劣势表承认：

```text
| 无 SDK | 只有 HTTP + CLI；竞品普遍有 Python/TS SDK |
```

这句话的另一面是**集成方必须自己写客户端**——成本实测值就是上一节那 86 行。换回三件事：客户端的形状由集成方决定（pi-sbx 只要四个端点，API 面 `README.md:283` 远不止这些）；依赖的代价留在能做取舍的人手里（本项目零第三方依赖，`docs/RESEARCH.md:25`，发 npm 包破坏的是**集成方**的承诺）；错误语义不被翻译层吃掉（`index.ts:34-43` 原样拼 `code`，而 SDK 的异常映射最容易丢 `retryable` 与 `retry_after`，见 §12.6）。

### 替代方案：让框架去调 `sbxctl` 子进程

让扩展 `spawn("sbxctl", ["sh", id, "--", cmd])` 更省事，但 `cmd/sbxctl/main.go:1-3` 自称 "thin HTTP client"——CLI 与 REST 之间**没有额外的语义层**，走 CLI 只多付一次进程启动和一次文本解析，还要自己取退出码（`cmd/sbxctl/main.go:440`）。

### 怎么验证

```bash
cd castle
sed -n '21,25p' integrations/pi/pi-sbx/index.ts
```

预期两行 import，来源都是 pi：**没有一行 import 来自 sandboxd**——协议就是 HTTP，客户端是集成方自己写的那几十行。

---

## 15.9 凭据边界的端到端取证：把一句话变成一条命令

### 问题

`integrations/pi/pi-sbx/README.md:10` 写着"LLM API Key 在宿主机（不进沙箱）"。但目前它只是文档里的一行字。而**文档描述的是设计意图，不是运行时事实**：意图可能没被实现，实现也可能在上次改动里被破坏。

### 原理：断言"没有"比断言"有"更危险

这是一个否定性的存在断言，有个专属陷阱——**很容易因为根本没检查到而通过**：路径拼错、探针语法在这个镜像里不被支持、输出被截断成空，都会让计数变成 0。

所以取证要满足两个条件：一是**探针打到真实对象**（`env` 读出当前进程派生出的环境，`/proc/self/environ` 是内核保留的初始环境块，`/proc/self/mounts` 是内核当前的挂载表，都不是进程的自我描述）；二是**断言对未知凭据也成立**——逐个变量名比对只守住你今天想到的那几个名字，稳的写法是模式匹配：任何名字含 `KEY` / `TOKEN` / `SECRET` 的变量都应该是零个。这与 §6.8 同源（读内核报告的事实，不读配置文件），差别只在作用对象（前者见 §6）。

### 本项目怎么做：机械原因写在请求体里

凭据没进沙箱，不是因为有一道过滤规则挡住了它，而是**请求里根本没有这个字段**。exec 的入参确实支持注入环境（`internal/httpapi/handlers.go:308-316`）：

```go
type execBody struct {
	Argv      []string          `json:"argv,omitempty"`
	Command   string            `json:"command,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	User      string            `json:"user,omitempty"`
	WorkDir   string            `json:"workdir,omitempty"`
	TimeoutMs int               `json:"timeout_ms,omitempty"`
	MaxOutput int               `json:"max_output,omitempty"`
}
```

这个 map 会一路走到后端变成 docker exec 的 `Env`（`internal/runtime/oci/backend.go:592`）。而 `integrations/pi/pi-sbx/index.ts:106-110` 发出去的请求体只有三项：

```ts
		const result = await this.request("POST", `/v1/sandboxes/${this.sandboxId}/exec`, {
			command,
			timeout_ms: Math.max(1, Math.round(timeoutSeconds * 1000)),
			max_output: 1 << 20,
		});
```

`env`、`user`、`workdir` 一个都没带。**没有规则，就没有规则被绕过这件事。**

另一层要补上：沙箱级的环境通道确实存在，是给"要注入配置"的场景用的（`internal/domain/sandbox.go:138`，CLI 入口 `cmd/sbxctl/main.go:256`），pi-sbx 建的沙箱里只有 CPU、内存、超时三项（`index.ts:95-97`）。

### 替代方案：为什么不做"宿主与沙箱环境对比"

把两侧 `env` 拿出来做 diff 并不成立：宿主环境本来就有几十上百个变量，沙箱是干净的容器文件系统，差集毫无意义——要断言的是"**特定的东西不在里面**"，不是"两边相同"。同理 `/v1/isolation`（`tests/e2e-enterprise-scenario.sh:194-197`）给出的是隔离机制的证据（见 §6），不是"这一次调用带了什么"的证据。

### 怎么验证

```bash
cd castle
export SBX_API_KEY=<租户Key>
export ZAI_API_KEY=<你的LLM Key>          # 宿主侧真实存在的凭据，见 integrations/pi/pi-sbx/README.md:41
TPL=$(./bin/sbxctl templates | jq -r '.templates[0].id // .id')
SBX=$(./bin/sbxctl create --template "$TPL" | jq -r .id)
./bin/sbxctl sh "$SBX" -- 'env | grep -ciE "KEY|TOKEN|SECRET"; grep -c /workspace /proc/self/mounts' | base64 -d
./bin/sbxctl delete "$SBX" --force
```

实测输出两行：

```text
0
0
```

第一行是本节的声明变成的数字：宿主那个 `ZAI_API_KEY` 没有跟进来，沙箱里此时 `env` 只有 `HOME`、`HOSTNAME`、`PATH`、`PWD`、`SHLVL` 五个；第二行顺带把 §15.2 那条"工作目录不挂载"也验了。**但这两个 0 只有在有对照时才算证据**——补对照的办法是手工带 `env`：

```bash
curl -s -X POST -H "Authorization: Bearer $SBX_API_KEY" -H 'Content-Type: application/json' \
  -d '{"command":"env | grep -ciE \"KEY|TOKEN|SECRET\"","env":{"INJECTED_API_KEY":"leaked"}}' \
  "$SBX_URL/v1/sandboxes/$SBX/exec" | jq -r '.stdout | @base64d'
```

同一个探针这次返回 `1`：探针是活的，且**请求体里的 `env` 就是凭据进入沙箱的唯一通道**。

---

## 15.10 多租户下的 Agent 集成：租户映射是一道架构题

### 问题

前面几节的部署形态都是一个人一台机器。变成"一个服务 N 个终端用户的 Agent 平台"时，`index.ts:28` 那一行就不再是一行配置。最省事的写法是共用一个租户、一把 Key，它坏起来是三处同时坏。

### 原理：判据是三件事的归属

粒度不是"越细越好"，要先问**配额归谁、限流归谁、审计归谁**。共用租户让三件事同时失效：

1. **配额互相抢**：准入统计按租户做（`internal/gov/quota.go:77-88` 只数 `sb.TenantID == t.ID` 的沙箱，机制见 §11.2）。一个用户开了十个沙箱，下一个用户的创建就撞上限拿回 402。
2. **限流互相拖**：令牌桶的键是租户 × 操作组（`internal/domain/tenant.go:25-32`，见 §11.5）。一个用户的 exec 洪水打空整个租户的 exec 桶，所有人一起拿 429。
3. **审计分不清是谁**：`Actor` 记的是 **Key ID**（`internal/domain/event.go:44`），写入接口 `Record(tenantID, sandboxID, actor, action, detail)`（`internal/gov/audit.go:28`）**没有"终端用户"这一维**；exec 事件额外带的 detail 只有一个清洗过的 `argv0`（`internal/service/lifecycle.go:249`）。

判据于是清楚了：**用租户决定"多少"，用 Key 决定"是谁"**。这两个维度在代码里本来就是分开的，所以常见正解是"一个团队一个租户、每人一把 Key"——同团队的突发本来就该共享，"谁做的"由 Key 分开记；而"一个租户一把 Key"的审计粒度是**无法区分**，不该进生产。

### 本项目怎么做：把决策推回部署层

pi-sbx 对这件事的贡献是零，而这是有意的。`SBX_API_KEY`（`index.ts:28`）是进程级环境变量，一把 Key 对应一个 pi 进程；`integrations/pi/pi-sbx/README.md:26-30` 的启动方式（先 export 再 `pi -e ...`）天然就是"每个会话一个进程、一套环境变量"。运行时侧不认识"终端用户"——`SandboxSpec`（`internal/domain/sandbox.go:128-141`）里只有资源与策略，所以映射表必须由平台自己存。配租户时还有个踩过的坑（`tests/e2e-enterprise-scenario.sh:200-205`）：

```bash
# PATCH /v1/tenants REPLACES the quota sub-object wholesale
# (`t.Quota = *body.Quota` in updateTenant) — it is not a merge. A PATCH that
# omits `rates` silently drops the tenant's custom rates and falls back to the
# hardcoded platform default (create 1/s burst 5). This script hit exactly
# that as an unexplained 429 later in the run, so every quota write below
# re-sends the full quota including rates.
```

漏发 `rates` 会把这个租户的速率悄悄打回平台默认——它的表现是"用户投诉变慢"，而配额数字看起来完全正确。

### 替代方案：为什么不直接加 `end_user` 字段

最直接的修复是让审计直接记终端用户。没做，理由在 `internal/domain/event.go:37-38`：Detail 值必须是非敏感的，而邮箱、工号是个人信息；审计流不可变（见 §11.9），写进去就永远删不掉。运行时只记 Key ID，把"Key → 人"留在平台自己的库里——那里可以删。

### 怎么验证

```bash
cd castle
sed -n '199,229p' tests/e2e-enterprise-scenario.sh
```

预期看到两个租户各自带独立配额与速率、各下发一把 Key；第五幕（`:344-354`）用租户 B 那把紧速率的 Key 连发两次创建拿到 429 与 `Retry-After`，跨租户互不影响的验收见 `Makefile:55` 的 `TestOneTenantsOverloadIsNotAnotherTenantsLatency`。审计侧跑 `./bin/sbxctl events --action sandbox.exec --limit 5`，判据是每条记录的 `actor` 能对应某一把 Key（见 §11.9）。

---

### 本章检验

1. `index.ts:166` 传给 daemon 的超时是 `effectiveTimeout + 5`。改成 `effectiveTimeout` 或 `effectiveTimeout + 300` 各自破坏什么？

2. `acquire()` 缓存的是 Promise 而不是结果。请构造一个并发时序说明缓存结果会导致什么，以及 `finally` 里清空 `starting` 防止的是哪一种永久失效。

3. §15.9 的两个 0 只有配上正向对照才算证据。请说明"探针不被支持"和"凭据真的没进来"为什么会产出同一个数字，以及它与 `/v1/isolation` 的取证对象有什么不同。

4. §15.8 把集成切成三件套。请说明换框架时哪一部分能原样复用、判据是什么（提示：看 import 列表），并解释"不做 SDK"为什么反而保住了集成方的零依赖承诺。

5. 一个平台让所有终端用户共用一个租户加一把 Key。请按"配额、限流、审计"说明它各自坏在哪、源码依据是什么，并解释为什么运行时不加 `end_user` 字段。

**动手任务**：按 §15.9 那条命令跑一次取证，记下两个数字；再把 `env: {"INJECTED_API_KEY":"leaked"}` 带进请求体重跑，看同一个探针从 0 变成 1。你会同时看到：凭据跨界不需要绕过任何机制、否定断言在缺对照时多容易假绿、以及"边界声明"与"边界证据"只隔一条命令。


# §16 工程决策复盘与延伸

## 16.1 零依赖：省下了什么，付出了什么

### 问题

一个 Go 项目要写 HTTP 服务、JSON、日志、指标、限流、容器客户端，通常的做法是引六个库：`gin`、`zap`、`prometheus/client_golang`、`golang.org/x/time/rate`、`docker/docker`、` testify`。六个库连带它们的传递依赖，进来的代码量可能是你自己代码的十倍。

代价不只是"体积大"：

- **供应链面**：任何一个依赖被投毒，你的产品就带着后门发布。2026 年这类事件的频率已经高到不能当小概率事件处理。
- **许可证面**：你需要逐个审查传递依赖的许可证，而它们的许可证可能在你引入的那个版本之后被改掉。
- **升级耦合**：Go 1.27 发布时，你要等六个库都兼容了才能升级。

### 原理：依赖的成本是"不可见成比例"的

依赖的成本不随你用到的功能比例下降——你用了 `prometheus/client_golang` 的一个 Counter，你付的是整个库的审计成本。所以真正该问的不是"这个库好不好"，而是：

> 我需要的这个功能，自己实现的复杂度是 O(多少行)？引入依赖后我要承担的面是 O(多少行)？

本项目对上面六个需求的答案分别是：

| 需求 | 自实现规模 | 依赖带来的面 |
|---|---|---|
| HTTP 路由与中间件 | ~200 行（标准库 `net/http` + `ServeMux` 的 Go 1.22 方法+路径模式） | 一个完整框架 + 它的中间件生态 |
| 结构化日志 | ~80 行（`log/slog`，标准库自带） | 一个日志库 + 它的编码器依赖 |
| Prometheus 指标 | ~150 行手写文本格式（见 §13） | client_golang + protobuf + 一堆传递依赖 |
| 令牌桶 | ~90 行（见 §11） | `golang.org/x/time`（小，但仍是外部） |
| 容器客户端 | ~600 行 HTTP + 结构体（见 §7） | `docker/docker`（**巨大**，且和引擎版本强耦合） |
| 断言 | ~50 行辅助函数 | `testify` |

每一项的右边都远大于左边。**当自己实现的行数在百量级而依赖的面在万行量级时，自己实现几乎总是对的。**

### 本项目怎么做：`go.mod` 只有三行

```go
module sandcastle.dev/sandcastle

go 1.26
```

`go.mod:1` 和 `go.mod:3`，中间一个空行。**没有任何 `require` 块**。这是一个可以被一条命令核对的事实，不需要信任 README：

```bash
cd castle && cat go.mod
```

零依赖要成立，有一个容易被忽略的前提：**不能通过 `exec.Command("docker", ...)` 把 CLI 依赖偷偷引回来**。`docs/POSITIONING.md:93-95` 把这条也写成了可核对的形式：

```markdown
3. **它是 API 客户端，不是 CLI 包装。** 生产代码通过 `/var/run/docker.sock`
   上的 HTTP 与引擎通信，**从不 `exec.Command("docker", ...)`**——
   在 `cmd/` 与 `internal/` 下 grep 该模式零命中。
```

"grep 该模式零命中"——这是一个任何人都能自己跑的核对，不是一个形容词。而且它顺带堵死了另一类问题：**命令行注入**。走 HTTP + JSON 就没有 shell 解析这一步，也就没有"命令里有个分号"这类问题。

### 替代方案：为什么不是"少量高质量依赖"

一个折中是"只引最必要的，比如只要 `x/time/rate`"。没这么做，理由是**零是一个有特殊性质的数字**：

- 依赖是 0 个时，供应链审计的工作量是 0，而且**永远**是 0——它不随时间的推移增长。
- 依赖是 1 个时，工作量是"审计这一个 + 它的传递依赖"，而且每年要重做。

也就是说，从 0 到 1 是一次**性质变化**，不是数量变化。本项目在 `docs/RESEARCH.md:25` 里把这条写成了决策：

```markdown
3. **协议合法性**：E2B(Apache-2.0)/gVisor(Apache-2.0)/pi(MIT) 可安全借鉴设计；Daytona 已闭源，**不读其代码、只对标公开功能清单**；全仓零第三方依赖（stdlib-only），供应链面为零，规避许可证与投毒双重风险。
```

注意"**不读其代码、只对标公开功能清单**"这一句——零依赖的动机不只是技术上的，也是法律上的。它是一个可以被律师审查的姿态。

### 怎么验证

```bash
cd castle
cat go.mod
grep -rn 'exec.Command' cmd/ internal/ | grep -i docker    # 预期：无输出
```

第二条命令预期**没有输出**。如果它有输出，说明 CLI 依赖被偷偷引回来了，零依赖的主张就不成立了。

---

## 16.2 诚实语义原则：做不到的就拒绝，不要假装

### 问题

假设客户要求"每个沙箱最多用 2 GiB 磁盘"。你在 API 里加一个 `disk_mib` 字段，写进 HostConfig，创建返回 201。客户看到 201，以为限制生效了。

实际上，overlayfs 上的单容器磁盘配额**无法可靠实施**。客户会在某天发现某个沙箱写了 40 GiB 把共享盘占满，然后来问你"我不是设了 2 GiB 吗"。

这个失败模式比"功能没做"糟糕得多：**功能没做，客户会找别的办法；假装做了，客户不会**。

### 原理：501 与 400 的区别是一次产品决策

同一个"我做不到"的事实，可以有两种表达：

| 表达 | 状态码 | 客户端读到的含义 |
|---|---|---|
| "你的请求格式错了" | 400 | 改请求再发一次 |
| "本节点不具备该能力" | 501 | 换节点/换部署/降级 |

`docs/POSITIONING.md:191-192` 把这个选择写明了：

```markdown
1. **`disk_mib` 与 egress 白名单返回 501，不是 400。** 语义是"本节点不具备该能力"，
   而非"请求格式错误"。客户端应据此区分重试与降级。
```

501 的信息量在于它告诉客户端**重试没有用**。一个返回 400 的实现会诱导客户端反复重试，而一个返回 501 的实现让客户端立刻走降级路径。

`docs/SECURITY-CONTRACT.md:87-105` 是一整节"明确**不**承诺的事"，六条里每一条都是这个模式的实例。挑两条最有代表性的：

```markdown
2. **不承诺磁盘配额可强制。** 在 overlayfs/ext4 上无法可靠实施单沙箱磁盘上限。请求
   `disk_mib` 的创建被 **501 拒绝**；仅当运维显式 `enforce_disk_quota=false` 时才放行
   （启动日志有 WARNING）。一个租户写满共享盘的风险由该运维决策承担。
```

注意后半句：**它给了运维一条显式放行的路，并且要求这个决定留下痕迹**（启动日志 WARNING）。这不是"不给用"，是"不给偷偷地用"。风险可以承担，但承担的动作必须是显式的、可审计的。

```markdown
5. **不承诺只读根文件系统。** `ReadonlyRootfs` 显式设为 `false`。只读根与快照模型不兼容
   （`Commit()` 捕获的是可写层，只读根会产出空快照），要与"可写 workspace overlay"
   一起重新设计。在此之前不假装拥有它。
```

第五条最值得学：它**承认了一个内部冲突**（只读根 vs 快照模型，快照机制见 §7 与 §8），并且明确说"要一起重新设计，在此之前不假装拥有它"。一个不诚实的做法是默认开启只读根，然后在文档角落里提一句"快照可能为空"。

### 本项目怎么做：把"best-effort"写进契约

诚实语义不止用在"做不到"上，也用在"做得到但不精确"上。`docs/SECURITY-CONTRACT.md:109-132` 记录了一个真实的实测结果：

```markdown
`gov.Quotas.CheckCreate` 读存储、判断、然后才写入记录，两步之间没有锁。它自己的文档注释
写明：*"It is best-effort under races (two concurrent creates can both pass the check); the
ceiling then trips on the next create instead — standard admission semantics."*

实测（20 路并发创建，租户上限 5）：

burst of 20 concurrent creates against a ceiling of 5: 20 admitted, 0 refused (overshoot 15)
```

20 路并发打一个上限 5 的配额，**全部被放行，越限 15**。这是一个会让很多人想删掉的数字。它被留下来了，而且下一节写明了为什么：

```markdown
因此可以依赖的是收敛，不是瞬时精确：

- 一次突发**可能**越限（本次实测越限 15）；
- 突发过后，**下一次创建必被拒绝**（HTTP 402 `quota_exceeded`，非 429 `rate_limited`——
  两者是不同指令："稍后重试"与"购买更多容量"）；
- 越限幅度被在途创建数封顶 ...
```

以及最关键的那一句（`docs/SECURITY-CONTRACT.md:130-132`）：

```markdown
对应测试：`TestQuotaAdmissionIsBestEffortButConverges`。它**不**断言"恰好 5 个"，
因为那是一个产品不提供的保证；它断言上面三条真实成立的保证，并把越限数字打进测试日志，
让这个数字留在记录里而不是被抹平。
```

**"它不断言'恰好 5 个'，因为那是一个产品不提供的保证"** —— 这是 §14.5 那条"断言的价值等于它能被推翻的程度"的反面应用：**不要断言一个你知道会被推翻的东西，即使它在你的机器上碰巧成立**。测试名 `TestQuotaAdmissionIsBestEffortButConverges` 本身就是契约——"best-effort but converges"（尽力但收敛）就是它承诺的全部。

402 和 429 的区分也是诚实语义的一部分：**"稍后重试"和"购买更多容量"是不同指令**。把它们混成一个"失败了"，客户端就只能盲目重试。

### 替代方案：加锁，让它精确

最直接的"修复"是在 check 和 write 之间加锁，让配额精确。没做，理由是**成本与收益不匹配**：

- 收益：突发时不越限。但越限幅度已经被在途创建数封顶，而在途创建数同时受租户 create 速率与平台 `max_concurrent_creates` 约束（见 §11）。
- 成本：每一次创建都要持有一把跨"检查 → 后端创建 → 写记录"的锁，而后端创建是**秒级的**（容器创建要拉镜像、建 overlay）。这把锁会把创建串行化。

用一个秒级的全局锁换取"突发时不越限 15"，是把一个可收敛的软问题变成了一个吞吐量的硬问题。这是 §10 里"并发闸与背压"那一节的同源决策。

### 怎么验证

```bash
cd castle
sed -n '87,132p' docs/SECURITY-CONTRACT.md
```

读完整节，然后对照测试：

```bash
SBX_REQUIRE_DOCKER=1 go test -count=1 -v -run 'TestQuotaAdmissionIsBestEffortButConverges' ./tests/
```

预期 PASS，且日志里打出越限数字。确认测试**没有**断言"恰好等于上限"。

---

## 16.3 可核对性：把边界放在能被程序读到的地方

### 问题

"我们的沙箱是隔离的"——这句话怎么核对？如果答案是"读我们的文档"，那它不是一个技术主张，是一句广告。

客户的安全评审要的不是"你说了什么"，是"我自己能查到什么"。而这两者之间有个陷阱：**文档会过期，代码会被改**。唯一不会过期的是"运行中的系统对它自己的描述"。

### 原理：三份证据要能互相印证

本项目的可核对性设计有一个三层结构，写在 `docs/SECURITY-CONTRACT.md:1-7`：

```markdown
# 容器级隔离契约（对外承诺 + 证据 + 明确不承诺）

本文是**对客户的可核对承诺**，不是设计说明。
...
一句话：**本版本提供的是容器级隔离（与宿主共享内核），不是内核级隔离。**
"共享内核"不是需要被粉饰的缺陷，而是需要被说清、被计价、被验收的**产品档位**。
```

"**需要被说清、被计价、被验收的产品档位**"——这三件事对应三种不同的核对动作：

| 层 | 谁产生 | 怎么核对 | 会过期吗 |
|---|---|---|---|
| 文档契约 | 人写 | 读 `SECURITY-CONTRACT.md` | 会（所以要可跑） |
| 运行状态 | daemon 自己 | `GET /readyz` → `.isolation` | 不会，它就是当前状态 |
| 独立证据 | 测试套件 | `GET /v1/isolation` + `TestIsolationContract` | 不会，每次跑都重算 |

第三层是关键的：**产品自己和独立测试各跑一遍，然后断言两者一致**。`docs/SECURITY-CONTRACT.md:63-65`：

```markdown
第 21 条值得单独说明：产品**自己**在启动时跑一遍同源检查，并把结论放在 `/v1/isolation`。
本套件独立跑一遍，然后断言两者一致。一个只在自检里"通过"、在独立测试里不通过的控制项，
会被这条断言抓住。
```

**"一个只在自检里通过、在独立测试里不通过的控制项，会被这条断言抓住"** —— 这就是为什么要两层。只有自检时，自检的 bug 会让所有项都通过；只有独立测试时，产品自己不知道自己是什么状态，也就无法在 `/readyz` 里如实回答。

`/readyz` 里还有一个更细的设计，`docs/SECURITY-CONTRACT.md:24-29`：

```markdown
`/readyz` 区分三种状态，且不允许互相冒充：

- `isolation_verified: true` —— 自检跑过且全项通过；
- `isolation_verified: false` + `isolation_failures` —— 自检跑过且**有项失败**；
- `isolation_verified: false` + `isolation_verified_note` —— 自检**尚未运行**，此时
  `.isolation` 描述的是配置意图，不是已验证的事实。
```

第三种状态是最容易被省掉的，而它恰恰最重要：**"自检还没跑"和"自检通过了"必须能区分**。如果只有两个状态（verified true/false），那么启动早期（自检未完成）的 `false` 会被读成"隔离有问题"，而真正的"自检失败"也被读成 `false`——两个完全不同的情况共用一个信号。

"**配置意图 vs 已验证事实**"这个区分，就是 §6 那句"配置是意图，内核事实才是证据"在 API 上的落地。

### 本项目怎么做：档位不能静默降级

`docs/SECURITY-CONTRACT.md:20-22`：

```markdown
`microVM`（每沙箱独立 guest kernel）**不在本版本内**，接口已锁定（`internal/runtime/fc`），
驱动仍是骨架。`isolation = microvm` 在本机不具备条件时**拒绝启动**，不会降级运行——
"以为在跑 microVM、实际在跑共享内核容器"的部署比明说只有容器危险得多。
```

"**不会降级运行**"——这是可核对性设计里最硬的一条规则。它和 userns 的处理是同一个模式（`docs/SECURITY-CONTRACT.md:100-101`）：

```markdown
   `userns = auto`（默认）会检测实际能力，不具备时降级为 `host` 并**写进启动日志与
   `/readyz`**；`userns = require` 在不具备时拒绝启动。
```

两个配置项、两种默认值，背后的规则是一致的：

- **默认检测并如实报告**（`auto`）——让部署先跑起来，但把真相写在能查到的地方；
- **可以强制要求**（`require`）——让在乎的运维把"不具备"变成"启动失败"。

区别在于风险性质：userns 缺失是"少一层防护"，档位降级是"你以为你在另一个产品档位"。后者更危险，所以 microvm 直接拒绝；前者给了 `auto/require` 两个选择。

### 替代方案：为什么不发一份"安全白皮书"

白皮书的问题是它的**权威性来自发布者**。而"你自己跑一遍这套测试"的权威性来自你自己。

`docs/SECURITY-CONTRACT.md:136-151` 给了三条不需要读代码就能核对的命令：

```bash
# 1. 档位与限制（未鉴权，客户可直接验）
curl -s $BASE/readyz | jq '{isolation, isolation_note, isolation_verified, isolation_userns}'

# 2. 完整隔离自检证据（需 admin）
curl -s -H "Authorization: Bearer $ADMIN_KEY" $BASE/v1/isolation | jq

# 3. 整套契约（需要本机有容器运行时）
SBX_REQUIRE_DOCKER=1 go test ./tests/ -run 'TestIsolationContract' -v -count=1
```

第一条**未鉴权**——客户在没有拿到任何凭据的情况下就能确认"这个节点报的是什么档位"。这是一个自觉的设计：档位不是机密，它是产品描述的一部分。

第三条里的 `SBX_REQUIRE_DOCKER=1` 在 §14.9 已经讲过（"静默跳过的套件和通过的套件不可区分"），这里重复出现是因为它在文档里也必须写明——否则客户跑了不带这个变量的命令，会得到一个全绿的假结果。

### 怎么验证

```bash
cd castle
make build
./bin/sandboxd -config sandboxd.conf.example &
curl -s localhost:8080/readyz | jq '{isolation, isolation_verified, isolation_note}'
```

预期看到 `isolation` 是 `container-shared-kernel`，`isolation_verified` 是布尔值（自检跑过之后应为 `true`），以及一段说明档位限制的 `isolation_note`。**如果你看到的是 `isolation_verified: false`，先确认自检是不是还没跑完**——这正是第三种状态存在的意义。

---

## 16.4 隔离谱系：从共享内核到独立内核

### 问题

"容器不够安全"这句话经常听到，但它把一个连续的谱系压成了一个二元判断。真实的谱系至少有三档，每一档的安全边界、性能代价、运维复杂度都不同。选错档位的代价，要么是过度设计，要么是被攻破。

### 原理：档位由"谁提供内核"决定

判据只有一个：**沙箱里的系统调用由谁处理**。

| 档位 | 内核归属 | 系统调用路径 | 逃逸需要什么 |
|---|---|---|---|
| 普通容器 | **共享宿主内核** | 直接陷入宿主内核 | 一个内核漏洞 + 突破 namespace/capabilities/seccomp |
| gVisor | 用户态内核（Sentry） | 陷入 Sentry，Sentry 用宿主 syscall 完成 | 突破 Sentry 的实现 + 一个宿主内核漏洞 |
| Kata / Firecracker microVM | **每沙箱独立 guest 内核** | 陷入 guest 内核，guest 经 VMM  traps 到宿主 | 突破 VMM 的隔离（这几乎就是虚拟化的边界） |

`docs/RESEARCH.md:11-12` 记录了对两个参照项目的核对：

```markdown
| [firecracker-microvm/firecracker](...) | ★36940，Rust，Apache-2.0，活跃 | 快照语义的事实标准（VM 状态+内存+盘差量）。本机无 KVM，MicroVM 后端做成可插拔骨架；快照/预热池接口按其语义设计，Linux 生产环境可替换实现 |
| [google/gvisor](...) | ★19418，Go，Apache-2.0，活跃 | 隔离强度谱系的中间档（用户态内核）。作为后端扩展位保留 |
```

`docs/POSITIONING.md:112` 给出了一句两年内没变过的排序：

```markdown
隔离强度排序在两年内没有变过：**Firecracker / Kata ≥ gVisor > 普通容器**。
```

### 本项目怎么做：选了最弱一档，并且把升级缝留在三个地方

选容器档的理由写在 `docs/POSITIONING.md:88-91`：

```markdown
1. **目标档位就是容器档。** 既然不追求 microVM，就没有理由自己实现
   namespace/cgroup/OCI 规范——那是几万行别人已经调好的代码。
2. **复用整个容器生态。** 任意 OCI 镜像、registry、overlayfs、cgroups、
   seccomp、AppArmor、user namespace 全部直接继承。
```

这是一个**目标客户决定档位**的选择。本项目的客户画像是"企业内部 AI 编程助手"（`docs/POSITIONING.md:29-37`）——代码来源是**自己的 Agent**，属于"受信任或半受信任"。而明确不该买的客户里第一条就是"直接执行外部用户提交的代码"（`docs/POSITIONING.md:40`）：

```markdown
- **直接执行外部用户提交的代码**（在线判题、SaaS 代码解释器）。这需要 microVM 级隔离。
```

**"这需要 microVM 级隔离"** —— 客户类型和档位是一一对应的。把这两个列表对照读一遍，你就知道这个产品的边界画在哪、为什么画在那。

升级缝留在三个地方，从外到内：

1. **运维层，零代码**：`docs/POSITIONING.md:96-97`：

```markdown
4. **它是一个刻意留出的替换缝。** `HostConfig.Runtime` 未写死，因此宿主把
   `default-runtime` 换成 Kata / Sysbox / gVisor 就能零代码改动升到 VM 级隔离。
```

引擎的 `default-runtime` 一改，**所有**新建的容器就走 Kata 了。这是成本最低的一条路，代价是它依赖运维的操作，产品设计不了。

2. **接口层，换实现**：`internal/runtime` 定义了 `Backend` 接口（24 个方法，见 §1），`internal/runtime/fc` 是编译通过的第二实现骨架。接口不为某一种后端设计，所以未来的 microVM 实现不用改接口。

3. **语义层，对齐快照**：`docs/RESEARCH.md:11` 提到"快照/预热池接口按其语义设计"——今天的快照是文件系统层 + 元数据（见 §8），明天的 microVM 快照是"VM 状态 + 内存 + 盘差量"。**接口按后者设计，前者是它的一个退化实现**。这样升级时上层代码（预热池、模板、恢复快路径）不用改。

### 替代方案：为什么不直接上 Firecracker

最直接的反问是"既然 microVM 更强，为什么不做"。答案有三层：

1. **本机条件不具备**：`docs/RESEARCH.md:11` 写明"本机无 KVM"。Firecracker 需要 KVM，macOS 上没有。这不是借口，是工程现实——一个你没法在本地跑的东西，你也没法为它写端到端测试（见 §14 的准入条件）。
2. **客户画像不匹配**：目标客户的代码是自己的 Agent 生成的，容器档已经够。上 microVM 要付出的代价（冷启动、内存开销、镜像格式不兼容、快照实现复杂度）不由他们受益。
3. **成本**：microVM 的快照是"VM 状态 + 内存 + 盘差量"，比 `docker commit` 复杂一个数量级。

反过来说，这也是本项目**最大的竞争劣势**，写在同一份文档的劣势清单里（`docs/POSITIONING.md:143`）：

```markdown
| 隔离弱一档 | 共享内核；E2B/CodeSandbox/Blaxel 的 Firecracker 更强 |
```

### 怎么验证

```bash
cd castle
curl -s localhost:8080/readyz | jq '.isolation'
# 预期： "container-shared-kernel"
sed -n '103,123p' docs/POSITIONING.md     # 竞争格局表
sed -n '127,147p' docs/POSITIONING.md     # 赢在哪输在哪
```

把"优势（可核对）"和"劣势（同样明确）"两张表对照读——它们的存在本身就是本节的方法论：**优势和劣势必须写在同一个文件里，且用同样的格式**。

---

## 16.5 做与不做：取舍清单

### 问题

一个项目最难回答的问题不是"你做了什么"，是"你为什么不做 X"。做了什么能靠代码清单回答；不做什么是**决策**，需要理由。

### 原理：边界要写成"排除表"

`docs/POSITIONING.md:9-21` 演示了一种值得学的写法——把产品定位里的每个限定词都翻译成"它排除了什么"：

```markdown
**单机、自托管、容器档隔离、多租户的 Agent 代码执行底座。**

拆开来看这四个限定词都不是修饰，而是**排除**：

| 限定 | 排除掉的场景 |
|---|---|
| 单机 | 需要跨节点调度、异地多活的场景 |
| 自托管 | 想直接用云 API、不想运维场景 |
| 容器档 | 直接执行不可信第三方代码的场景 |
| 多租户 | 单用户本地开发工具 |
```

"**都不是修饰，而是排除**" —— 这句话的价值在于它让每个词都可证伪。"单机"不是"我们专注单机"，是"跨节点调度的需求我们不接"。前者无法被反驳，后者可以。

`docs/RESEARCH.md:30-34` 是同一份清单的技术侧：

```markdown
## 3. 本产品不做什么（v1 边界）

- 不做多节点调度/放置（E2B 的 best-of-K 放置属于多节点范畴），单机多租户优先做扎实；
- 不做 VM 内存级快照（容器后端快照为文件系统层 + 元数据；内存级快照归 MicroVM 后端路线图）；
- 不做浏览器内 Web 终端/PTY 流（v1 提供同步 exec；PTY 列入路线图，接口预留）。
```

每一条都带了**理由**和**归属**（"归 MicroVM 后端路线图"、"接口预留"）。没有理由的"不做"是懒；有理由且指明归处的"不做"是路线图的一部分。

### 本项目怎么做：与 Docker 的分界也要列成表

`docs/POSITIONING.md:64-81` 用一张表回答"这不就是 docker run 外面包一层吗"：

```markdown
| | Docker Engine | Sandcastle |
|---|---|---|
| 租户身份 | 无此概念 | Key + scope（admin/tenant），存 SHA-256 哈希 |
| 配额 | 无 | 每租户并发沙箱/CPU/内存上限 |
| 限流 | 无 | 每租户 × 操作组（create/exec/files/snapshot/read）令牌桶 |
| 审计 | 无 | 每个状态变更一条事件 |
| 调用面 | `docker` CLI / API | HTTP REST，供 Agent 直接调用 |
| 环境复用 | `docker commit`（需自己管理） | 快照 + 引用计数 + 孤儿镜像回收 |
| 生命周期 | 需自己写 | 建/停/删/暂停/恢复/空闲超时/TTL/透明恢复 |
| 崩溃一致性 | 需自己写 | WAL + fsync + 重启后 reconciler 重建真相 |

关键区别不是"更安全"，而是**"有人在管"**：`docker run` 不区分谁在跑、
跑了多少、该不该拒绝、事后谁查得到。
```

最后那句是整张表的题眼：**不是"更安全"，是"有人在管"**。这句话把产品从"隔离工具"（和 Docker 比强弱）重新定位成"治理层"（和 Docker 比有没有）。

这个重新定位很重要，因为它改变了竞争关系：Docker 不是对手，是底座（16.1 说的"它是 API 客户端"和这里是一致的一）。

### 替代方案：为什么不做 Kubernetes Operator

一个明显的"往上走"的选择是做成 K8s Operator，这样天然获得多节点、调度、HA。没做，理由直接写在客户画像里（`docs/POSITIONING.md:32-34`）：

```markdown
- **数据主权/合规受限**：代码与产物不能出机房，因此不能用云端沙箱服务；
  又不想为几十个开发者维护一套 Kubernetes。
```

"**又不想为几十个开发者维护一套 Kubernetes**" —— 目标客户的规模是几十人，而 K8s 的运维成本在这个规模上是净负担。`docs/POSITIONING.md:145` 的劣势表也承认了这一点："单机 | 无多节点、无 HA、无选主"。

这是一个诚实的取舍：放弃大客户，换取小客户的零运维。

### 怎么验证

```bash
cd castle
sed -n '9,25p' docs/POSITIONING.md
sed -n '27,47p' docs/POSITIONING.md   # 该买 / 不该买
```

对照"该买"和"不该买"两个清单：如果你能想到一个场景同时满足两边，说明这个定位有漏洞。

---

## 16.6 已知缺口：交付时就把清单交出来

### 问题

项目交付时通常有一份"已知问题"清单，它在内部文档里，客户看不到。于是客户在三个月后自己发现其中一条，然后信任崩塌——不是因为那个问题，是因为**你早就知道却没说**。

### 原理：清单的价值在于它的时间戳

`docs/POSITIONING.md:189-200` 是"本次演练顺带确认的三个边界与缺口"，它的价值不在内容而在**它是和演练同一次产生的**：

```markdown
2. **平台默认速率硬编码（已知缺口）。** 管理面 bucket 是 `1/s burst 5`，
   `sandboxd.conf` 没有对应键；演练中连续的管理面调用确实收到过 429。
   全平台调整需要对每个租户逐个下发 `quota.rates`。
3. **`PATCH /v1/tenants` 是整对象替换，不是合并（已知缺口）。** `updateTenant`
   执行 `t.Quota = *body.Quota`，因此只带 `max_sandboxes` 的 PATCH 会把该租户的
   自定义 `rates` 一并抹掉、静默回退到平台默认。本次演练**已复现**：一次遗漏
   `rates` 的配额修改导致后续创建意外 429。修法是每次配额写入都带全 `rates`
   （演练脚本已如此处理），但语义本身值得商榷。
```

第三条值得逐句读：

- 它给出了**代码事实**（`t.Quota = *body.Quota`）；
- 它给出了**后果**（自定义 rates 被静默抹掉，回退到平台默认）；
- 它说"**本次演练已复现**"——不是"可能会"，是"发生过"；
- 它给出了**规避办法**（每次都带全 rates，演练脚本已如此处理）；
- 它仍然说"**语义本身值得商榷**"——规避办法不等于修复。

最后一条是这个清单的灵魂：**把 workaround 和 fix 区分开**。给了 workaround 就说"这是 workaround"，不要让读者以为问题解决了。

`docs/SECURITY-CONTRACT.md:155-164` 是另一份同类清单，而且它有**状态列**：

```markdown
| 项 | 状态 |
|---|---|
| 孤儿快照镜像无回收（...） | **已修复**：reconciler 增加镜像回收；实测 89 个遗留镜像归零。见 `TestOrphanedSnapshotImagesAreReaped` |
| `snapshot.go` 中"node GC 会回收未打标签镜像"的注释 | **已修正**：该说法两头都错（镜像是带标签的，且没有任何 GC 找它们） |
| 畸形 ID 返回 500 且 `retryable: true`（...） | **已修复**：改为 400 `invalid_request`、`retryable: false` |
| 运维通过配置提供的 bootstrap admin key 仍被打印到 stdout/journal | **已修复**：仅本进程**生成**的 key 才回显 |
| 平台默认租户速率是硬编码的，配置文件无对应键 | **未改**，仅能通过管理 API 按租户覆盖。需要全平台调高时这是缺口 |
| `DELETE /v1/sandboxes/{id}` 与创建共用租户 `create` 令牌桶 | **设计取舍**：租户打满 create 预算期间也无法删除。默认 1/s burst 5 下影响有限 |
```

状态有四种：**已修复 / 已修正 / 未改 / 设计取舍**。最后两种的存在是这个表格诚实的原因——它承认了"我们知道，我们没做"，并且区分了"没做是因为没空"（未改）和"没做是因为我们选了另一边"（设计取舍）。

第二条尤其少见：**一条注释是错的，也被当成缺陷修掉了**。理由在括号里："该说法两头都错"。一个错误的注释比没有注释更糟，因为它会让人不去验证。

### 本项目怎么做：演练也要声明它没证明什么

`docs/POSITIONING.md:182-185`：

```markdown
**同样重要的：本场景没有证明什么。**
没有证明隔离强度（那是 `/v1/isolation` 的事，且只覆盖已断言的那几项）；
没有证明高并发下的表现（单机顺序调用）；没有证明多节点行为（不存在）；
也没有给出任何冷启动对比数字。
```

"**本场景没有证明什么**"——一个演示材料主动列出自己的盲区。这一节的存在让前面那张"实测数字"表变得可信，因为它表明作者知道演示和证明的区别。

最后一条（"没有给出任何冷启动对比数字"）对照劣势表里的"无冷启动数字 | 未测量、也未宣称"（`docs/POSITIONING.md:147`）——**未测量就不宣称**。这条规则在本项目里没有例外。

### 替代方案：为什么不在交付前把清单清空

一个常见的冲动是"把这些修完再交付"。问题是：

- 修完的清单会变成"没有问题"，而一个说自己没有问题的产品**不可信**；
- 更重要的是，有些条目**不该修**（"设计取舍"那两条），把它们清掉等于把设计决策抹掉。

清单的正确终点不是空的，是**每一条都有状态、有理由、有归处**。

### 怎么验证

```bash
cd castle
sed -n '155,165p' docs/SECURITY-CONTRACT.md
sed -n '189,200p' docs/POSITIONING.md
```

然后逐条核对状态：标"已修复"的，去搜对应的测试名是否存在（例如 `grep -rn TestOrphanedSnapshotImagesAreReaped tests/`）。**如果一条"已修复"找不到对应测试，那这条清单就不再可信。**

---

## 16.7 外部风险：被上游挤压，以及赛道本身在动

### 问题

15.1 提到一个风险：本项目的 pi 集成依赖上游框架的扩展机制。如果 pi 自己做了执行环境，这条路径就被挤掉了。

同类风险还有：**整个赛道在动**。`docs/RESEARCH.md:13` 记录了一个信号：

```markdown
| [daytonaio/daytona](...) | **2026-06 起转闭源**，公开仓停止维护（README 顶部声明），当前 LICENSE 未再声明开源协议 | 市场信号：开源路线的头部玩家退出，自托管/可私有部署的独立运行时存在真实空档。其历史功能清单（组织/API Key/限额/审计日志/Webhook/PTY/预览代理/卷/快照）被采纳为商用功能对标清单 |
```

一个和本项目架构最接近、融了 3100 万美元的同类，**退出了**。

### 原理：对手退场是双向信号

`docs/POSITIONING.md:114-123` 给出了自己的解读，而且它同时看到了两面：

```markdown
Daytona 是与本项目**架构最接近**的同类——同样默认容器档、把 Kata/Sysbox 作为可选强化，
融资 3100 万美元。2026 年 6 月它把核心开发移入私有仓库，**2026-10-03 归档了公开仓库**
（AGPL-3.0，冻结在 v0.190.0），官网删除了自托管文档。官方给出的理由是
**"AI 辅助的漏洞挖掘正在针对公开的隔离代码"**。

这句话本身就是容器档定位的注脚：一个架构选择与本项目重合的公司，因为同一档隔离的
**实现细节**经不起公开而选择了关灯。本项目的回答不是把实现藏起来，而是把**契约**公开：
哪些是实测的、哪些明确不承诺、怎么自己核对。
```

两面的信号是：

- **机会面**：开源自托管路线出现空档，需求还在（这正是本项目的定位）；
- **风险面**：容器档隔离的**实现细节**正在被 AI 辅助的漏洞挖掘针对。一个架构重合的、有专职安全团队的公司，选择不公开自己的实现。

第二面直接命中本项目：本项目的隔离**也是容器档**，而且代码是公开的。

本项目的回答写在最后一句：**"不是把实现藏起来，而是把契约公开"**。这个回答的实质是——藏起来只能延缓被发现，而公开契约让客户能自己判断风险是否可接受。这是 16.3 那一整节的产品化理由。

### 本项目怎么做：把风险写进定位文档

`docs/POSITIONING.md:49-62` 的"威胁模型边界（买之前必须接受的三件事）"把这个风险前置到了销售环节：

```markdown
1. **沙箱与宿主共享内核。** 容器档隔离，逃逸需要同时击穿
   namespace + dropped capabilities + seccomp + no-new-privs + 只读路径屏蔽。
   正确的说法是"同时击穿每一层"，**不是**"无法逃逸"。
   ...
2. **身份不分离（当 `userns=off` 时）。** 沙箱内的 uid 0 就是宿主的 uid 0。
   一旦有内核提权漏洞，不需要第二次提权。`userns = require` 能让不具备
   user namespace 的宿主**拒绝启动**，而不是静默降级。
```

"**买之前必须接受的三件事**"——把安全风险变成购买前置条件，而不是售后问题。这个位置和 16.6 的缺口清单是同一个思路：**坏消息要说在前面，且要说在客户还能做决定的时候**。

第 1 条里的措辞也值得注意："正确的说法是'同时击穿每一层'，**不是**'无法逃逸'"。这是在教客户怎么说——因为销售环节最容易出现的失真就是"我们的沙箱绝对安全"。

### 替代方案：为什么不转向 microVM 来规避这个风险

转向 microVM 能规避"容器档实现细节被针对"这个风险，但会：

- 失去本机可测性（16.4 第 1 条），也就是失去整个 §14 的测试体系；
- 失去"零运维单机部署"这个定位（16.5）；
- 在一个没有 KVM 的开发机上无法开发。

所以本项目选择**留在容器档、公开契约、给升级缝**。这是一个承认风险存在、并把判断权交给客户的选择。

### 怎么验证

```bash
cd castle
sed -n '49,62p' docs/POSITIONING.md
sed -n '114,123p' docs/POSITIONING.md
```

读完后问自己：如果客户问"你们和 Daytona 有什么区别，它都关灯了"，你能用这两段里的哪几句回答？**答不上来就说明这一段没读懂**——因为它正是为这个问题写的。

---

### 本章检验

1. `go.mod` 里没有 `require` 块。请说出零依赖带来的**三条**具体收益，以及**两条**具体代价（提示：代价要在"自己实现了什么"里找，比如手写 Prometheus 文本格式、手写令牌桶）。

2. 磁盘配额做不到，本项目返回 501 而不是 400。请说明：如果返回 400，客户端的行为会怎样不同？这个差异为什么值一个状态码？

3. `TestQuotaAdmissionIsBestEffortButConverges` 不断言"恰好 5 个"。请说明：如果它断言了，在什么情况下会变红？那个红灯是产品缺陷还是测试缺陷？

4. `/readyz` 区分了"自检未运行"和"自检失败"两种 `isolation_verified: false`。请说明：如果只有两种状态（true/false），哪两种真实情况会被混淆？混淆的后果是什么？

5. Daytona 关灯这个事件，本项目同时读出了机会和风险。请分别说出机会和风险各是什么，并说明本项目的回答（"公开契约"）为什么是应对**风险**那一面、而不是机会那一面。

**动手任务**：挑本项目任意一条"优势"（`docs/POSITIONING.md:129-137`），找到它对应的可核对方式（一个端点、一条命令或一个测试名），亲自跑一遍。然后挑任意一条"劣势"（同一文件的 :139-147），确认它同样有具体证据（而不是一句自谦）。做完这一步，你会明白这份定位文档为什么把两张表放在一起——**它们用的是同一把尺子**。


# §17 附录：速查卡与阅读路径

本附录**只做索引与汇总，不重讲任何原理**。每一条都指向正文的一个编号，需要理解时回到那一节。

## 17.1 概念速查：这个概念在哪一节讲

| 你想知道 | 去哪 |
|---|---|
| 这个项目是什么、为什么不是 `docker run` | §1.1 / §16.5 |
| 代码目录怎么分层的 | §1 |
| 规模数字（代码行数、测试数、延迟） | §1（数字总表），各数字的推导在归属章节 |
| Go 的包、`internal`、接口、错误处理 | §2 |
| goroutine / channel / `sync` / context | §3 |
| `encoding/json`、`net/http`、`testing` | §4 |
| namespace、cgroup v2、OOM、pids limit | §5 |
| capabilities、seccomp、LSM、user namespace | §6 |
| overlayfs、镜像层、`commit`、OCI 三规范 | §7 |
| 暂停/恢复、快照、模板、预热池、TTL | §8 |
| WAL 帧格式、fsync、压实、备份 | §9 |
| 意图先行、对账、CAS 与 409、孤儿回收 | §10 |
| API Key、令牌桶、配额、审计 | §11 |
| REST 建模、中间件顺序、错误包络、分页 | §12 |
| 启动顺序、配置系统、指标、日志、停机 | §13 |
| 四层测试、正向对照、比值断言、五维验收 | §14 |
| Agent 为什么需要沙箱、pi 集成、工具覆写 | §15 |
| 零依赖、诚实语义、隔离谱系、已知缺口 | §16 |

## 17.2 术语表（固定写法，正文全程一致）

| 统一写法 | 不要写成 |
|---|---|
| 沙箱 | 容器实例、sandbox（正文） |
| 租户 | 用户、账号、组织 |
| 隔离档位 | 隔离级别、隔离等级（配置项名 `isolation` 除外） |
| 后端 | 驱动、provider |
| 意图先行 | intent-first（仅首次括注） |
| 收敛器 / 对账 | reconciler（正文）、reconcile 循环 |
| 预写日志 / WAL | 事务日志、redo log |
| 预热池 | 池子、warm pool |
| 快照 | snapshot（正文） |
| 端点 | 接口、路由（指 HTTP path 时） |
| 端到端套件 | e2e、集成测试（指 `tests/` 时） |
| 令牌桶 | token bucket（仅首次括注） |
| 命令 / 调用 | 请求（指 shell 时） |

## 17.3 三条阅读路径

| 你的情况 | 顺序 | 跳过 |
|---|---|---|
| 半天，要能讲清楚 | §1 → §5 → §6 → §9 → §10 → §16.2 | §2/§3/§4（有 Go 基础时）、§13/§14/§15 |
| 两天，要能上手改 | 全读，每章做完「本章检验」 | 无 |
| 要讲给别人听 | 每节的「替代方案」段 + §16 全部 | 各章的「本项目怎么做」细节 |

**唯一不能跳的一句**（全篇骨架，出现在卷首，也是 §6、§14.1、§16.3 的共同基础）：

> 配置是意图，内核事实才是证据。

## 17.4 实验清单（按顺序做，每个都能自己跑）

| # | 实验 | 命令 | 对应节 | 看到什么算成功 |
|---|---|---|---|---|
| 1 | 确认零依赖 | `cat go.mod`；`grep -rn 'exec.Command' cmd/ internal/ \| grep -i docker` | §16.1 | 无 `require`；第二条无输出 |
| 2 | 起服务并读档位 | `make build && ./bin/sandboxd -config sandboxd.conf.example &`；`curl -s localhost:8080/readyz \| jq` | §16.3 | `isolation` = `container-shared-kernel`，`isolation_verified` 有值 |
| 3 | 读完整隔离证据 | `curl -s -H "Authorization: Bearer $ADMIN_KEY" localhost:8080/v1/isolation \| jq` | §16.3 | 逐层事实，每条带内核读数 |
| 4 | 跑隔离契约 | `SBX_REQUIRE_DOCKER=1 go test -run TestIsolationContract -v ./tests/` | §14.1 | 22 个子场景 PASS |
| 5 | 跑遏制场景 | `SBX_REQUIRE_DOCKER=1 go test -run 'TestBlastRadiusIsTheSandbox' -v ./tests/` | §14.3 | 4 个子场景 PASS，注意第一个是正向对照 |
| 6 | 看比值断言 | `make test-commercial` 后 `grep -n atMost tests/commercial_test.go` | §14.5 | 每个边界都是表达式，没有字面毫秒数 |
| 7 | 跑崩溃恢复 | `SBX_REQUIRE_DOCKER=1 go test -run TestCrashRecovery -v ./tests/` | §14.4 | `kill -9` 之后状态收敛 |
| 8 | 跑集成 harness | `node tests/pi-sbx-harness.mjs` | §15.6 | `pass=7 fail=0`，含 `host workspace NOT mounted` |
| 9 | 复现存储层缺陷 | `go run ./poc/tornframe` | §14.7 | 观察到"声称的回滚其实是空操作"的后果 |
| 10 | 重生成派生件 | `make scenario-flow` | §14.8 | `docs/scenario-trace.jsonl` 与 `scenario-flow.html` 都被重写 |
| 11 | 验证准入条件 | 停掉容器引擎后跑 `make test-e2e` | §14.9 | **失败**（不是静默跳过） |
| 12 | 核对缺口清单 | `sed -n '155,165p' docs/SECURITY-CONTRACT.md`，再 grep 每条"已修复"的测试名 | §16.6 | 每条"已修复"都能找到对应测试 |

## 17.5 讲给别人听时的边界声明（照抄即可）

这几句是本项目反复出现的诚实语义，讲解时**必须原样说，不要软化**：

| 被问到 | 正确回答 | 依据 |
|---|---|---|
| 你的沙箱绝对安全吗 | 不。容器档共享内核，没有安全边界只有成本边界 | §16.3 / §16.7 |
| 逃逸可能吗 | 可能。正确说法是"需要同时击穿每一层"，不是"无法逃逸" | §16.7 |
| 磁盘配额生效吗 | 不生效，请求 `disk_mib` 返回 501 | §16.2 |
| 配额是精确的吗 | 不精确。best-effort 但收敛，实测 20 路并发打上限 5 时越限 15 | §16.2 |
| 冷启动多少毫秒 | 未测量、也未宣称 | §16.6 |
| 支持多节点吗 | 不支持。单机、无 HA、无选主 | §16.5 |
| 只读根文件系统呢 | 不承诺。它与快照模型冲突，要重新设计 | §16.2 |

**面试时最有价值的不是把产品说强，而是知道它在哪失效**——上面这张表就是这个判断的具体内容。

## 17.6 全篇数字索引

所有数字的**出处与推导**在归属章节，§1 有一张总表作为索引。引用规则（见 `CONCEPTS.md` 第三节）：非归属章节引用时写数字 + `见 §X.Y`，**不重新推导**。

| 数字类别 | 归属章节 |
|---|---|
| 代码行数、测试行数、比例 | §1 |
| 冷启动 727ms、首次创建 188ms、重启 166ms | §13（启动顺序） |
| exec p50 26.885ms、控制面约 10µs、`/healthz` p50 73µs | §14.5（比值断言的分母与分子） |
| 80 并发创建 → 201×10 / 429×70 | §11（令牌桶）+ §14.6 |
| 12 秒持续负载 2,968 请求、`/readyz` 47/47 | §14.6 |
| 空闲预算 60s → 1m6s 回收；TTL 35s → 36s 回收 | §8（生命周期）+ §14.6 |
| 22 项隔离断言、5 个遏制场景 | §14.1 / §14.3 |
| 34 个顶层用例、83 个子场景、228.5s 全量 | §14.9 |
| 演练 47 次调用 / 43 断言 / 24 条审计事件 | §16.6（含"没有证明什么"） |
| 配额越限 15 | §16.2 |

## 17.7 一页纸版本

如果只能记住一段，记这段：

1. **它是什么**：单机、自托管、容器档隔离、多租户的 Agent 代码执行底座（§1）。
2. **它的核心主张**：把多租户治理和容器运行时装进一个零依赖二进制，并把隔离档的真实边界放在可程序化核对的地方（§16.1、§16.3）。
3. **它的方法论**：配置是意图，内核事实才是证据（§6）；做不到的拒绝而不是假装（§16.2）；断言必须能被推翻（§14.1）。
4. **它不做**：内核级隔离、磁盘配额强制、egress 白名单、多节点、HA、SDK、计量计费（§16.2、§16.5、§16.6）。
5. **它留给你的缝**：`default-runtime` 换成 Kata/gVisor 即升档（§16.4），`Backend` 接口与 `fc` 骨架（§1）。

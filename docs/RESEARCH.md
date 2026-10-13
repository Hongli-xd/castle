# 调研纪要：Agent Sandbox / Runtime 赛道（2026-09）

调研时间：2026-09-25。方法：GitHub API 核实活跃度/协议，阅读 E2B Runtime 架构文档、
pi 官方容器化文档、Daytona README 与公告。以下结论直接影响本产品的设计决策。

## 1. 关键项目与事实

| 项目 | 状态（核实于 2026-09-25） | 对本产品的意义 |
|---|---|---|
| [e2b-dev/runtime](https://github.com/e2b-dev/runtime) | ★1618，Go，Apache-2.0，活跃（2026-09-24 有推送）。E2B 于 2026 年把云端运行时整体开源：控制面 API、每节点 orchestrator、VM 内 agent(envd)、边缘路由、模板构建器 | **首要参考**。采纳其两大核心思想：①"沙箱即快照恢复"（创建=恢复，而非冷启动）；②控制面与数据面严格分离。其 API 分组限流（每租户×操作组×Redis 预算，429+Retry-After）、envd 进程/文件 API、沙箱 URL 端口暴露、密钥元数据化（值不过控制面）均为商用必备形态 |
| [firecracker-microvm/firecracker](https://github.com/firecracker-microvm/firecracker) | ★36940，Rust，Apache-2.0，活跃 | 快照语义的事实标准（VM 状态+内存+盘差量）。本机无 KVM，MicroVM 后端做成可插拔骨架；快照/预热池接口按其语义设计，Linux 生产环境可替换实现 |
| [google/gvisor](https://github.com/google/gvisor) | ★19418，Go，Apache-2.0，活跃 | 隔离强度谱系的中间档（用户态内核）。作为后端扩展位保留 |
| [daytonaio/daytona](https://github.com/daytonaio/daytona) | **2026-06 起转闭源**，公开仓停止维护（README 顶部声明），当前 LICENSE 未再声明开源协议 | 市场信号：开源路线的头部玩家退出，自托管/可私有部署的独立运行时存在真实空档。其历史功能清单（组织/API Key/限额/审计日志/Webhook/PTY/预览代理/卷/快照）被采纳为商用功能对标清单 |
| [superradcompany/microsandbox](https://github.com/superradcompany/microsandbox) | ★8423，Rust，Apache-2.0，活跃（原 microsandbox org 迁移） | 验证"自托管 microVM 沙箱"需求；其分支(branchable)快照概念纳入路线图 |
| [kata-containers/kata-containers](https://github.com/kata-containers/kata-containers) | ★8789，Apache-2.0，活跃 | MicroVM 后端的另一种实现路径（Kata runtime 类） |
| [earendil-works/pi](https://github.com/earendil-works/pi) | ★109229，TS，MIT，极活跃（原 badlogic/pi-mono） | **集成对象**（"本地 pi agent"）。官方文档《Run Pi in an isolated environment》给出 4 种接入模式；其中 Gondolin 模式 = pi 留在宿主机、把内置工具(bash/read/write/edit/grep/find/ls)路由进隔离环境。本产品提供同款 pi 扩展，但 **v1 只路由 `bash` 一个工具**（read/write/edit 仍在宿主机；完整七工具路由在路线图）|
| [bytedance/SandboxFusion](https://github.com/bytedance/SandboxFusion) | ★1069 | 字节系代码沙箱（Python/Jupyter 向），佐证国内大厂对该基础设施的需求；本产品定位通用 Agent 运行时，非 Jupyter 替代 |
| [openai/codex](https://github.com/openai/codex) | ★126363，Rust，Apache-2.0 | 编码 Agent 端侧沙箱（Seatbelt/Landlock/seccomp）代表；其"按策略放行"思路体现在本产品的 egress 策略模型 |
| siyad01/agentbox | ★1，Go | 2026 年新出现的同方向尝试，引用 CVE-2026-25253（首个发给 agentic AI 系统的 CVE）作为隔离必要性论据；无社区规模，仅作方向佐证 |

## 2. 由调研直接得出的设计决策

1. **快照即创建（E2B 思想）**：创建沙箱优先走"模板预热池命中"或"快照恢复"路径，冷启动只发生在池未命中时。暂停/快照/模板三种产物在形状上对齐，恢复共用同一条快路径。
2. **控制面/数据面分离（E2B 思想）**：API 只决定"在哪建、记录建了没"；每节点 orchestrator 拥有"怎么建"。单二进制产品内部保持这一分层（httpapi → service → backend driver），为未来多节点保留演进空间。
3. **协议合法性**：E2B(Apache-2.0)/gVisor(Apache-2.0)/pi(MIT) 可安全借鉴设计；Daytona 已闭源，**不读其代码、只对标公开功能清单**；全仓零第三方依赖（stdlib-only），供应链面为零，规避许可证与投毒双重风险。
4. **隔离强度分级而非单点**（E2B/Firecracker/gVisor 谱系）：进程级容器（本机可测）→ MicroVM（生产推荐，骨架就位）。凡是后端无法真实强制的安全声明（egress 域名白名单、磁盘配额），一律**拒绝而非假装**，与 Claude Code 本地沙箱"安全特性≠多租户基础设施"的定位划清界限。
5. **pi 接入模式**：按 pi 官方 Gondolin 同款模式做官方扩展——pi 与 LLM 凭据留在宿主机，`bash` 工具经 REST 路由进沙箱执行（v1 仅此一个工具，read/write/edit 仍在宿主）；另提供"整个 pi 进程跑在沙箱里"的镜像配方作为第二模式——那才是 Agent 级隔离，与"工具级执行隔离"是两个不同层。
6. **商用功能对标**（来自 Daytona 功能清单 + E2B API 面）：组织(租户)、API Key、配额、速率限制、审计日志、快照、文件 API、exec、事件查询、健康/指标端点。

## 3. 本产品不做什么（v1 边界）

- 不做多节点调度/放置（E2B 的 best-of-K 放置属于多节点范畴），单机多租户优先做扎实；
- 不做 VM 内存级快照（容器后端快照为文件系统层 + 元数据；内存级快照归 MicroVM 后端路线图）；
- 不做浏览器内 Web 终端/PTY 流（v1 提供同步 exec；PTY 列入路线图，接口预留）。

## 4. 厂商调研（2026-10）与竞争力差距分析

> 数据来源：`~/Desktop/Agent Runtime 厂商调研报告.md`（2026-10-11）。它覆盖
> Anthropic / OpenAI / Google / Microsoft / AWS 与国内厂商，是一次横向比对，
> 结论与上面 2026-09 的 GitHub 开源项目调研互补。落地清单见 `docs/ROADMAP.md`。

### 4.1 三个改变游戏规则的行业事实

1. **Runtime 已经商品化。** 隔离沙箱、会话状态、凭据隔离、追踪这些能力正在变成
   所有平台都提供的"标配"，不再是差异化。差异化发生在 **Harness（agent 循环）**：
   谁决定下一步、子 agent 编排、模型路由、eval。
2. **MCP 变成无状态协议（2026-07-28）。** 一个直接后果：exec 的**幂等**从"最好有"
   变成**硬要求**——客户端收不到响应就必须重试，运行时必须保证重试不会把命令跑两遍。
3. **计费单位正在统一为资源秒。** vCPU 小时、GiB 小时、会话小时取代"每次调用"；
   且明确"内存照计，I/O/CPU 视状态"——AWS 的"内存照计"与暂停仍占内存的语义，
   要求计量必须采样**并发分配 × 墙钟时间**，而不是请求数 × 固定费率。

### 4.2 本项目的差距（用 grep 核实，非印象）

对照 Runtime 七能力（§2.3 口径）：

| 能力 | 现状 | 缺口等级 |
|---|---|---|
| 隔离沙箱 | 强（可证伪自检 + 拒绝静默降级） | 无 |
| 会话状态 | 部分（沙箱/快照/暂停恢复，无"会话"语义） | P1 |
| 跨会话记忆 | 无 | P2 |
| 权限审批 | 部分（租户/Key/配额/限流，无逐操作审批） | P1 |
| 凭据隔离 | 部分（密钥只存哈希，无 vault/占位符注入） | P1 |
| 调度 | 无（单机，刻意） | 见 ROADMAP §3 |
| 追踪 | 部分（审计事件 + 指标，无 GenAI 语义导出） | P1 |
| 计量计费 | 无 | P0 |
| exec 幂等 | 无 | P0 |
| 冷启动/吞吐数字 | 未测量、未公开 | P0 |

### 4.3 战略取舍

- **不建 Harness。** 建了会被夹在托管 agent 平台与"可自托管"两个定位之间，两头不靠。
  **把执行底座做完整、做到可核对，就是竞争力。**
- **主打叙事 = 可核对 + 数据主权。** 调研 §3.5 提到某托管平台的 agent 数据"仅存美国、
  无 ZDR（数据驻留区）"，恰好放大本项目的"自托管、数据不出机房"这个别人抄不走的点。
- **P0 三件事**（已交付）：计量 API、公开冷启动/吞吐数字、exec 幂等键——对应选型表上
  最容易被一眼淘汰的三个空白。详见 `docs/ROADMAP.md`。

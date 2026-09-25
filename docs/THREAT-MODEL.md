# sandboxd 威胁模型与安全语义

## 1. 威胁主体

| 主体 | 能力 | 动机 |
|---|---|---|
| **沙箱内代码** | 沙箱内任意计算（可能是被提示注入的 Agent 产物——参见 CVE-2026-25253，首个发给 agentic AI 系统的 CVE） | 逃逸、横向移动、耗尽宿主资源 |
| **持有租户 Key 的调用方** | 合法 API 面内的一切 | 越权访问他租户资源、超配额占用平台 |
| **网络攻击者** | 对 API 的未认证访问 | 绕过认证、注入、DoS |

明确不在模型内：宿主机 root 被攻破、内存级侧信道（容器后端的共享内核固有限制，
microVM 后端路线图缓解）、DoS 网络洪泛（由部署侧 LBS/防火墙承担）。

## 2. 沙箱内代码 → 宿主/他租户

容器后端（oci）对每个沙箱强制：

- `NetworkMode=none` —— 默认零网络；
- `CapDrop=ALL` + `no-new-privileges` —— 无提权路径；
- `PidsLimit=512` —— 防 fork 炸弹；
- `Memory` 硬上限且 `MemorySwap=Memory`（禁 swap）—— OOM 只杀死本沙箱并显式暴露
  （exit 137 / OOMKilled 会进入状态与原因）；
- `NanoCPUs` 配额；
- 重启策略 no + init 进程收僵尸。

**已知且如实声明的限制**（这是容器后端与 microVM 后端的本质差距，产品绝不掩盖）：

1. **共享内核**：容器共享宿主内核，内核漏洞构成理论逃逸面。需要内核级隔离的
   租户必须等待/部署 firecracker 后端（接口已锁定，见 internal/runtime/fc）。
2. **磁盘配额不可强制**：overlayfs/ext4 上无法可靠实施单沙箱磁盘上限。产品行为：
   请求 `disk_mib` 的创建被 501 拒绝；仅当运维显式 `enforce_disk_quota=false`
   时才放行（启动日志有 WARNING）。一个租户写满共享盘的风险由该运维决策承担。
3. **egress 白名单不可强制**：容器后端无法实施内核级出口过滤。请求
   `egress.mode=allowlist` 的创建被 501 拒绝（TestAllowlistIsRefusedNotIgnored 级别的
   测试锁死该行为）。`none`（默认）是唯一可强制的出口策略。
4. **限流的近似性**：并发创建的配额检查存在竞窗（两个同时创建可短暂越过
   软上限 1 个），上限在下一次准入时生效——标准准入控制语义，不是精确记账。

## 3. 调用方 → 平台与他租户

- **认证**：Key 仅存 SHA-256；prefix 索引查找 + 常数时间哈希比较；过期/吊销
  立即生效。明文只在创建响应中出现一次。
- **租户隔离**：所有资源访问在 service 层强制 `entity.TenantID == caller.TenantID`
  （IDOR 防线在服务层而非路由层）；跨租户访问 403 且不确认资源存在性。
- **admin 边界**：admin Key 不携带 `X-Tenant-Id` 时对一切租户资源操作 403——
  管理动作必须显式声明目标租户。
- **配额与限流**：每租户并发沙箱/CPU/内存/快照数/快照字节上限 + 每租户×操作组
  令牌桶（429 + Retry-After）。
- **输入防线**：请求体 1MiB 上限（文件上传另有 100MiB 上限）、JSON 严格解码
  （未知字段拒绝）、路径三重校验（绝对/无 `..`/无控制字符，service 与 backend 两层）、
  env 键名白名单正则、metadata 键长与总量上限、exec 输出截断（防 OOM 聚合）。
- **ReadHeaderTimeout** 防 slowloris；错误响应不含内部路径/栈。

## 4. 平台自身

- **审计不可变**：审计事件只追加；daemon 崩溃不丢已确认事件（WAL fsync）。
- **密钥卫生**：日志、错误、审计 detail 中不含 Key 明文/文件内容/env 值
  （tests/security.sh 自动验证）。
- **崩溃一致性**：状态变更 = 一条 WAL 帧；撕裂尾部重启自动截断；过渡态
  （provisioning/pausing/resuming/stopping）在重启后由 reconciler 向后端重新对账。
- **孤儿回收**：带产品标签但无记录的后端对象（崩溃残留）由 reconciler 收割；
  预热池对象按名字空间识别，采用中的对象受保护。
- **数据目录**：`flock` 防双进程共写；备份用 `sbxctl`/API 的一致性快照
  （见 docs/DEPLOYMENT.md）。

## 5. 验证矩阵

以上每一条声明都有对应测试（商用门槛，全部必须在交付前通过）：

| 声明 | 测试 |
|---|---|
| 认证拒绝/防篡改 | tests/security.sh（伪造 Key、真实前缀篡改） |
| IDOR | tests/security.sh（跨租户 read/exec/delete/snapshot/events） |
| 路径穿越/体积/env 洪水 | tests/security.sh |
| 诚实拒绝（egress/disk） | tests/security.sh（501 断言） |
| 密钥不泄漏 | tests/security.sh（日志与响应体扫描） |
| OOM 真实生效 | internal/runtime/oci 集成测试（64MiB 内堆增长被杀） |
| exec 超时真实生效 | internal/runtime/oci 集成测试 |
| 崩溃收敛 | tests/chaos.sh（kill -9 三场景） |
| 限流真实生效 | internal/httpapi HTTP 级测试 + 冒烟实测 |

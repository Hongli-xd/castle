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
   `egress.mode=allowlist` 的创建被 501 拒绝（由
   `TestIsolationContract/unenforceable_limits_are_refused_not_silently_ignored` 锁死，
   该场景同时锁死 `disk_mib` 的 501）。`none`（默认）是唯一可强制的出口策略。
4. **限流的近似性**：并发创建的配额检查存在竞窗，上限在下一次准入时生效——标准准入
   控制语义，不是精确记账。**实测幅度**：20 路并发创建对上限 5，20 个全部放行
   （越限 15），突发结束后下一次创建被 402 拒绝。越限幅度被在途创建数封顶，受
   `create_queue_timeout` 与 `max_concurrent_creates` 约束。见
   [SECURITY-CONTRACT.md](SECURITY-CONTRACT.md) §5。

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
  （`TestSecretsNeverReachTheLog` 自动验证，含"运维在配置里提供的 bootstrap key
  不再被回显到 stdout/journal"这条回归）。
- **崩溃一致性**：状态变更 = 一条 WAL 帧；撕裂尾部重启自动截断；过渡态
  （provisioning/pausing/resuming/stopping）在重启后由 reconciler 向后端重新对账。
- **孤儿回收**：带产品标签但无记录的后端对象（崩溃残留）由 reconciler 收割；
  预热池对象按名字空间识别，采用中的对象受保护。
- **数据目录**：`flock` 防双进程共写；备份用 `sbxctl`/API 的一致性快照
  （见 docs/DEPLOYMENT.md）。

## 5. 验证矩阵

以上每一条声明都有对应测试（商用门槛，全部必须在交付前通过）。全部端到端断言都在
`tests/` 下的 Go 套件里，共用一个 harness（daemon 生命周期、带类型客户端、沙箱夹具），
取代了早期六份各自重复三十行样板（写配置→pkill→rm -rf→启动→sleep→从日志里 grep 出
admin key）的 shell 脚本。

| 声明 | 测试 |
|---|---|
| 隔离逐层生效（22 项，从沙箱内部读内核事实） | `TestIsolationContract` |
| daemon 自检与本套件结论一致 | `TestIsolationContract/the_self_check_inventory_agrees_with_this_suite` |
| 认证拒绝/防篡改 | `TestAuthenticationAndScope/credential_rejections_are_all_401` |
| IDOR | `TestAuthenticationAndScope/cross_tenant_access_is_forbidden_on_every_resource` |
| 路径穿越/体积/env 洪水 | `TestQuotaAndAdmission/specs_outside_platform_limits_are_rejected_with_a_reason` |
| 诚实拒绝（egress/disk，501 断言） | `TestIsolationContract/unenforceable_limits_are_refused_not_silently_ignored` |
| 密钥不泄漏 | `TestSecretsNeverReachTheLog` |
| OOM 真实生效 | `TestMemoryBombIsContained`（64 MiB 上限下 exit 137） |
| fork 炸弹遏制 | `TestForkBombIsContained` |
| CPU 配额不饿死控制面 | `TestCPUBurnDoesNotStarveTheControlPlane` |
| 爆炸半径 = 沙箱本身 | `TestBlastRadiusIsTheSandbox`（宿主金丝雀与 `/etc/hosts` 逐字节未变） |
| exec 超时真实生效 | `internal/runtime/oci` 集成测试 |
| 崩溃收敛 | `TestCrashRecovery`（kill -9 四场景，含连续三次崩溃的 WAL 完整性） |
| 灾难恢复（PITR） | `TestPointInTimeRecovery`（备份后创建的状态必须不存在） |
| 限流真实生效 | `internal/httpapi` HTTP 级测试 + `TestQuotaAndAdmission` |
| 配额准入语义（收敛而非瞬时精确） | `TestQuotaAdmissionIsBestEffortButConverges` |
| 孤儿镜像回收 | `TestOrphanedSnapshotImagesAreReaped`、`TestSnapshotsDoNotLeakImages` |
| 后端不可达时的降级表现 | `TestBackendOutageIsDisclosedAndRecovers`（`SBX_IT_DESTRUCTIVE=1` 门控） |

按客户可核对的承诺清单、逐层证据与**明确不承诺**的边界，见
[SECURITY-CONTRACT.md](SECURITY-CONTRACT.md)。

# 容器级隔离契约（对外承诺 + 证据 + 明确不承诺）

本文是**对客户的可核对承诺**，不是设计说明。设计说明见 [DESIGN.md](DESIGN.md)，
威胁建模见 [THREAT-MODEL.md](THREAT-MODEL.md)。

一句话：**本版本提供的是容器级隔离（与宿主共享内核），不是内核级隔离。**
"共享内核"不是需要被粉饰的缺陷，而是需要被说清、被计价、被验收的**产品档位**。

---

## 1. 你买到的是哪一档

| 项 | 值 |
|---|---|
| 隔离档位 | `container-shared-kernel` |
| 内核 | **与宿主共享**。内核漏洞构成理论逃逸面 |
| 可核对方式 | 未鉴权 `GET /readyz` → `.isolation` / `.isolation_note` / `.isolation_verified` |
| 完整证据 | `GET /v1/isolation`（需 admin scope） |

`microVM`（每沙箱独立 guest kernel）**不在本版本内**，接口已锁定（`internal/runtime/fc`），
驱动仍是骨架。`isolation = microvm` 在本机不具备条件时**拒绝启动**，不会降级运行——
"以为在跑 microVM、实际在跑共享内核容器"的部署比明说只有容器危险得多。

`/readyz` 区分三种状态，且不允许互相冒充：

- `isolation_verified: true` —— 自检跑过且全项通过；
- `isolation_verified: false` + `isolation_failures` —— 自检跑过且**有项失败**；
- `isolation_verified: false` + `isolation_verified_note` —— 自检**尚未运行**，此时
  `.isolation` 描述的是配置意图，不是已验证的事实。

---

## 2. 逐层承诺（每条都有从沙箱内部读到的内核事实作证）

"读配置不是证据"是本项目的硬规矩：容器引擎会接受它无法兑现的安全字段并原样回读。
因此下表每一项都是**在运行中的真沙箱内部**读取内核状态得到的，而不是创建请求里的字段。

| # | 承诺 | 沙箱内实测 | 对应测试 |
|---|---|---|---|
| 1 | seccomp 过滤器生效 | 模式 = 2 | `seccomp_filter_is_in_force` |
| 2 | 无法重新获取特权 | `NoNewPrivs` = 1 | `privilege_cannot_be_regained` |
| 3 | 能力集为空 | `CapEff`/`CapBnd`/`CapPrm`/`CapInh`/`CapAmb` 全零 | `capability_sets_are_empty` |
| 4 | uid 0 但无权力 | `id -u` = 0 且 `CapEff` 为空 | `uid_is_root_but_powerless` |
| 5 | core dump 关闭 | `ulimit -c` = 0 | `core_dumps_are_disabled` |
| 6 | 挂载被拒 | `mount` 失败 | `mounting_is_denied` |
| 7 | 内核内存不可读 | `/proc/kcore` 读失败 | `kernel_memory_is_unreadable` |
| 8 | 内核可调参数不可写 | `/proc/sysrq-trigger` 写失败 | `kernel_tunables_are_unwritable` |
| 9 | 不能创建设备节点 | `mknod` 失败 | `device_nodes_cannot_be_created` |
| 10 | 原始设备不存在 | `/dev/sda`、`/dev/mem` 等缺席 | `raw_devices_are_absent` |
| 11 | 容器运行时套接字不可达 | `docker.sock`/`containerd.sock` 不存在 | `the_container_runtime_socket_is_not_reachable` |
| 12 | 无宿主目录挂载 | 宿主金丝雀文件不可见/不可挂载 | `no_host_directory_is_mounted` |
| 13 | `/proc` 无法用于越界 | `/proc/1/root` 指向沙箱自身 | `proc_cannot_be_used_to_step_outside` |
| 14 | PID 命名空间隔离控制面 | `/proc/*/comm` 中无 `sandboxd`；PID 1 不是它 | `pid_namespace_hides_the_control_plane` |
| 15 | UTS 命名空间独立 | hostname 与宿主不同 | `uts_namespace_differs_from_the_host` |
| 16 | 无出口路由 | 无默认路由、无 `eth0` | `there_is_no_route_off_the_box` |
| 17 | 外泄在边界处死亡 | `wget`/`nslookup` 均失败 | `exfiltration_dies_at_the_network_boundary` |
| 18 | loopback 是沙箱自己的 | 与宿主 loopback 不同 | `loopback_is_the_sandboxes_own` |
| 19 | userns 姿态与内核一致 | 与 `/proc/self/uid_map` 对照，而非与配置对照 | `user_namespace_posture_matches_the_kernel` |
| 20 | 就绪端点如实报告档位与其限制 | `/readyz` 与本文一致 | `readiness_reports_the_tier_and_its_limits` |
| 21 | 自检清单与本套件互相印证 | daemon 自己的自检结论 = 本套件的结论 | `the_self_check_inventory_agrees_with_this_suite` |
| 22 | 无法实施的限制被拒绝而非静默忽略 | 501 `unimplemented` | `unenforceable_limits_are_refused_not_silently_ignored` |

第 21 条值得单独说明：产品**自己**在启动时跑一遍同源检查，并把结论放在 `/v1/isolation`。
本套件独立跑一遍，然后断言两者一致。一个只在自检里"通过"、在独立测试里不通过的控制项，
会被这条断言抓住。

---

## 3. 资源遏制承诺（实测数字）

隔离的另一半是"一个租户的失控不会拖垮别人"。

| 承诺 | 实测（Docker 29.5.2 / darwin-arm64） | 对应测试 |
|---|---|---|
| 堆炸弹被内存上限杀死 | 64 MiB 上限下堆增长 → **exit 137**（SIGKILL） | `TestMemoryBombIsContained` |
| 内存爆炸不伤邻居 | 同租户邻居沙箱继续正常 exec | `TestMemoryBombIsContained` |
| fork 炸弹被 pids 上限吸收 | exec 超时结束（`timed_out=true`），daemon 全程可响应，沙箱仍可回收 | `TestForkBombIsContained` |
| CPU 独占不会饿死控制面 | 有人烧满一个核时：5 次健康检查 **1 ms**，无关沙箱的 exec **31 ms** | `TestCPUBurnDoesNotStarveTheControlPlane` |
| `rm -rf /` 的爆炸半径 = 该沙箱本身 | daemon 存活；宿主金丝雀文件与宿主机 `/etc/hosts` **逐字节未变**；同租户旁观沙箱与新建沙箱均正常 | `TestBlastRadiusIsTheSandbox` |
| 沙箱内写"宿主路径"不改宿主 | 沙箱内 `/etc/hosts`、`/etc/passwd` 写入**确实成功**（`rc1=0 rc2=0`，回读 1/1），宿主同名文件摘要不变 | `TestInSandboxHostPathWritesDoNotReachTheHost` |

最后一条的**正向对照**是刻意的：如果沙箱内那次写入被只读挂载挡住了，那么"宿主没变"这个
断言会通过，却什么都没证明。因此先证明写入真的落在沙箱内，再断言宿主未变。

---

## 4. 明确**不**承诺的事

写在这里的每一条都是真实存在的边界，不是免责声明。

1. **不承诺内核级隔离。** 沙箱与宿主共享内核；一个内核漏洞构成理论逃逸面。需要真隔离的
   租户必须等待/部署 microVM 后端。这是本档位与 microVM 档位的本质差距。
2. **不承诺磁盘配额可强制。** 在 overlayfs/ext4 上无法可靠实施单沙箱磁盘上限。请求
   `disk_mib` 的创建被 **501 拒绝**；仅当运维显式 `enforce_disk_quota=false` 时才放行
   （启动日志有 WARNING）。一个租户写满共享盘的风险由该运维决策承担。
3. **不承诺 egress 白名单可强制。** 容器后端无法做内核级出口过滤。请求
   `egress.mode=allowlist` 的创建被 **501 拒绝**。`none`（默认）是唯一可强制的出口策略。
4. **不承诺身份分离（除非运维配置了 userns-remap）。** 没有用户命名空间时，沙箱内 uid 0
   **就是**宿主 uid 0：一个内核漏洞直接到达宿主 root，不需要第二次提权。
   `userns = auto`（默认）会检测实际能力，不具备时降级为 `host` 并**写进启动日志与
   `/readyz`**；`userns = require` 在不具备时拒绝启动。
5. **不承诺只读根文件系统。** `ReadonlyRootfs` 显式设为 `false`。只读根与快照模型不兼容
   （`Commit()` 捕获的是可写层，只读根会产出空快照），要与"可写 workspace overlay"
   一起重新设计。在此之前不假装拥有它。
6. **不承诺精确配额记账。** 见下一节。

---

## 5. 配额准入的真实语义（一个必须说清的实测结果）

`gov.Quotas.CheckCreate` 读存储、判断、然后才写入记录，两步之间没有锁。它自己的文档注释
写明：*"It is best-effort under races (two concurrent creates can both pass the check); the
ceiling then trips on the next create instead — standard admission semantics."*

实测（20 路并发创建，租户上限 5）：

```
burst of 20 concurrent creates against a ceiling of 5: 20 admitted, 0 refused (overshoot 15)
```

**因此可以依赖的是收敛，不是瞬时精确：**

- 一次突发**可能**越限（本次实测越限 15）；
- 突发过后，**下一次创建必被拒绝**（HTTP 402 `quota_exceeded`，非 429 `rate_limited`——
  两者是不同指令："稍后重试"与"购买更多容量"）；
- 越限幅度被在途创建数封顶，而在途创建数同时受租户 `create` 速率与平台
  `max_concurrent_creates` 约束；超出的排队创建在 `create_queue_timeout` 后以
  `state: failed` + "create queue timeout — node is saturated" 显式失败，而不是无限堆积。

对应测试：`TestQuotaAdmissionIsBestEffortButConverges`。它**不**断言"恰好 5 个"，
因为那是一个产品不提供的保证；它断言上面三条真实成立的保证，并把越限数字打进测试日志，
让这个数字留在记录里而不是被抹平。

---

## 6. 怎么自己核对（不用读我们的代码）

```bash
# 1. 档位与限制（未鉴权，客户可直接验）
curl -s $BASE/readyz | jq '{isolation, isolation_note, isolation_verified, isolation_userns}'

# 2. 完整隔离自检证据（需 admin）
curl -s -H "Authorization: Bearer $ADMIN_KEY" $BASE/v1/isolation | jq

# 3. 整套契约（需要本机有容器运行时）
SBX_REQUIRE_DOCKER=1 go test ./tests/ -run 'TestIsolationContract' -v -count=1
SBX_REQUIRE_DOCKER=1 go test ./tests/ -run 'TestBlastRadius|TestForkBomb|TestMemoryBomb|TestCPUBurn|TestInSandboxHostPath' -v -count=1
```

`SBX_REQUIRE_DOCKER=1` 很关键：没有它，缺少容器运行时的机器上整套测试会**静默跳过**——
而一个静默跳过的套件与一个通过的套件在报告上长得一模一样。

---

## 7. 已知缺陷与取舍（一次交付的诚实清单）

| 项 | 状态 |
|---|---|
| 孤儿快照镜像无回收（`docker image prune` 不会删带标签的镜像） | **已修复**：reconciler 增加镜像回收；实测 89 个遗留镜像归零。见 `TestOrphanedSnapshotImagesAreReaped` |
| `snapshot.go` 中"node GC 会回收未打标签镜像"的注释 | **已修正**：该说法两头都错（镜像是带标签的，且没有任何 GC 找它们） |
| 畸形 ID 返回 500 且 `retryable: true`（一次拼写错误引发重试风暴，并被报成服务端错误） | **已修复**：改为 400 `invalid_request`、`retryable: false` |
| 运维通过配置提供的 bootstrap admin key 仍被打印到 stdout/journal | **已修复**：仅本进程**生成**的 key 才回显 |
| 平台默认租户速率是硬编码的，配置文件无对应键 | **未改**，仅能通过管理 API 按租户覆盖。需要全平台调高时这是缺口 |
| `DELETE /v1/sandboxes/{id}` 与创建共用租户 `create` 令牌桶 | **设计取舍**：租户打满 create 预算期间也无法删除。默认 1/s burst 5 下影响有限 |
| HA / 多节点 / 计量计费 / SDK | **未实现**，不在本版本内 |

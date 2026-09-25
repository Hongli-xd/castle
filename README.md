# Sandcastle（沙堡）

**单二进制、零第三方依赖、可私有部署的多租户 Agent 沙箱运行时。**

> 为 agent 而建、用完即弃的城堡：每个沙箱是一座隔离的堡内房间，
> TTL 到期潮水自会把它带走。守护进程 `sandboxd`，钥匙 `sbxctl`。

为 AI Agent 提供隔离的执行环境：秒级创建、暂停恢复、快照、预热池加速，
内置租户 / API Key / 配额 / 限流 / 审计。对标 E2B / Daytona（已闭源）的能力面，
面向企业自托管 Agent 基础设施场景。

```bash
$ ./bin/sandboxd -config sandboxd.conf        # 一个二进制，一切就绪
$ sbxctl create --template default            # 毫秒级（预热池命中）
{"id":"sbx_...","state":"running"}
```

## 核心能力

| 能力 | 说明 |
|---|---|
| 多租户 | 租户隔离、API Key（哈希落库/常数时间比较/吊销/过期）、租户级配额与速率限制 |
| 沙箱生命周期 | 创建/暂停/恢复/停止/TTL 自动回收；显式过渡态 + 崩溃对账（reconciler） |
| 快照 | 文件系统层 commit → 从快照重建沙箱；按租户配额（数量/字节）治理 |
| 预热池 | 模板预热容器，创建命中池时毫秒级交付；未命中回退冷启动 |
| 执行与文件 | 同步 exec（退出码/输出/超时/截断语义完整）、文件上传下载列表删除 |
| 治理审计 | 全部变更与生命周期事件追加式审计，可按租户/沙箱/动作过滤 |
| 运维 | /healthz /readyz /metrics（Prometheus 文本）、结构化日志、优雅停机、崩溃安全 WAL |
| 离线部署 | rootfs 目录/tar 导入为一等公民能力，气隙环境无需镜像仓库 |

## 快速开始

依赖：任意可运行 Docker 的环境（Linux 生产 / macOS Docker Desktop 或 colima 开发）。

```bash
make build                                   # 产出 bin/sandboxd + bin/sbxctl

cat > sandboxd.conf <<'EOF'
listen  = "127.0.0.1:8080"
data_dir = "./data"
bootstrap.image = "alpine:3.22"              # 本机已有该镜像则自动播种默认模板
EOF

./bin/sandboxd -config sandboxd.conf
# 首次启动会打印一次 ADMIN API KEY（仅此一次，请保存）

export SBX_URL=http://127.0.0.1:8080
export SBX_API_KEY=<打印的管理员Key>

./bin/sbxctl tenant-create demo quota.max_sandboxes=10
./bin/sbxctl key-create <tnt_id> demo-key    # 输出中包含唯一一次的租户 Key 明文
```

创建沙箱并执行命令：

```bash
export SBX_API_KEY=<租户Key>
./bin/sbxctl create --template default --cpu 500 --mem 256
./bin/sbxctl sh <sbx_id> -- "echo hello && uname -m"
./bin/sbxctl cp <sbx_id> ./file.txt /root/file.txt
./bin/sbxctl snapshot-create <sbx_id> checkpoint-1
```

与 pi coding agent 集成（把 pi 的 bash 工具路由进沙箱）见
[integrations/pi/pi-sbx/README.md](integrations/pi/pi-sbx/README.md)；
其中含 Z.ai GLM（glm-5.3-flash）模型的配置说明。

## API 概览

REST + `Authorization: Bearer sk_...`，统一错误包络
`{"error":{"code","message","retryable","request_id"}}`。完整端点见 [docs/API.md](docs/API.md)。

```
POST /v1/sandboxes?wait=true      创建（template_id | snapshot_id | base_image 三选一）
GET  /v1/sandboxes                列表（state 过滤）
POST /v1/sandboxes/{id}/pause|resume|stop|timeout
POST /v1/sandboxes/{id}/exec      {command | argv, timeout_ms, max_output}
PUT/GET/DELETE /v1/sandboxes/{id}/files?path=
POST /v1/snapshots                {sandbox_id, name}
GET  /v1/events                   审计事件（租户自动限定范围）
POST /v1/tenants  等 admin 端点   租户/Key/配额管理（admin Key）
GET  /healthz /readyz /metrics
```

## 架构与设计

- [docs/DESIGN.md](docs/DESIGN.md) —— 产品设计、领域模型、状态机、可靠性设计
- [docs/RESEARCH.md](docs/RESEARCH.md) —— 2026-09 赛道调研（E2B runtime/Daytona 闭源/Firecracker/pi）及决策依据
- [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md) —— 威胁模型与"拒绝而非假装"安全语义
- [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) —— 生产部署、气隙安装、备份恢复、运行手册

一句话架构（控制面/数据面分离思想来自 E2B，本地化为单二进制内部分层）：

```
pi/SDK/CLI ──REST──▶ httpapi(认证→限流→审计) ──▶ service(状态机/池/快照/对账)
                                                     ├─ store(WAL, fsync, 崩溃安全)
                                                     └─ runtime(oci 容器 ▸ firecracker 骨架)
```

## 工程约束（刻意的）

- **零第三方依赖**（stdlib-only Go）：供应链攻击面为零、单文件分发、可气隙构建。
- **崩溃安全**：WAL 每事务一帧 `[len][crc32][payload]` + fsync；撕裂尾部重启自动截断；
  过渡态记录一律由 reconciler 向后端重新对账，不信任本地残留。
- **诚实语义**：后端无法真实强制的策略（容器后端的 egress 白名单、磁盘配额）一律
  拒绝创建（HTTP 501），绝不产出"看起来受限实际没有"的沙箱。

## 测试

```bash
make test          # 单元 + 领域 + 存储（-race）
make test-int      # 真实容器集成测试（需 Docker）
make test-e2e      # 四轮商用测试：混沌/负载/安全/pi 扩展测试台
```

| 轮次 | 内容 | 门槛 |
|---|---|---|
| 混沌 chaos | 运行中 kill -9：创建在途/存活沙箱/连续三次崩溃 | 5 项全过，状态收敛数据零丢失 |
| 负载 load | 30 并发创建 + 200 次混合 exec | 零 5xx，限流器正常工作 |
| 安全 security | 认证/IDOR/路径穿越/体积/诚实拒绝/密钥卫生 | 21 项全过 |
| 恶意负载 malicious | 沙箱内 `rm -rf /`、fork 炸弹、装包、数据外传、OOM、CPU 烧灼、写宿主 /etc | 11 项全过，爆炸半径=沙箱本身 |
| 多用户 multiuser | 双租户并发真实工作流 + 实时越权探针 + 配额独立 + 吊销即时性 | 6 项全过 |
| 冗灾 disaster | 备份时点恢复 + 容器 VM 整机重启下的对账收敛 | 8 项全过 |
| Agent pi-sbx | 真实 pi bash 工具契约测试台 | 7 项全过 |

## 路线图

- Firecracker microVM 后端（Linux/KVM；骨架已就位，接口编译期锁定）
- egress 域名白名单（microVM 后端 + nftables，内核级强制）
- exec 流式输出（WebSocket）与 PTY
- Postgres 存储后端（Store 接口已就位）
- 事件 Webhook 订阅推送

## 许可证

Apache-2.0（见 [LICENSE](LICENSE)）。

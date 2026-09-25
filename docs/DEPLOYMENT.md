# sandboxd 部署与运维手册

## 1. 形态

单二进制 `sandboxd`（Linux amd64/arm64 生产，macOS 开发自测）+ 可选 CLI `sbxctl`。
生产建议：裸金属或完整 VM（需要 Docker/容器运行时），root 运行（管理容器 cgroup）。

## 2. 安装

```bash
# 二进制与 rootfs 产物一并分发（气隙环境无需镜像仓库）
sudo install -m 0755 sandboxd /usr/local/bin/
sudo useradd -r -d /var/lib/sandboxd -s /usr/sbin/nologin sandboxd  # 或以 root 运行
sudo mkdir -p /var/lib/sandboxd && sudo chown sandboxd:sandboxd /var/lib/sandboxd
```

配置文件 `/etc/sandboxd/sandboxd.conf`（key = value，`#` 注释；全部键见
internal/config/config.go，生产最小集）：

```ini
listen = "127.0.0.1:8080"        # 生产建议置于内网 LB/网关之后，或启用 TLS
tls_cert = "/etc/sandboxd/tls.crt"   # 可选
tls_key  = "/etc/sandboxd/tls.key"
data_dir = "/var/lib/sandboxd"
# docker_host = "unix:///var/run/docker.sock"   # 默认探测标准位置

defaults.cpu_millicores  = 1000
defaults.memory_mib      = 512
defaults.timeout_seconds = 3600

quota.max_sandboxes     = 20     # 平台默认配额（租户可覆盖）
quota.max_cpu_millicores = 8000
quota.max_memory_mib     = 8192
quota.max_snapshots      = 20

max_concurrent_creates = 8
pool.enabled = true
pool.size    = 4

# enforce_disk_quota = false    # 显式接受"磁盘配额不强制"的风险（见威胁模型）
# bootstrap_admin_key = "sk_..." # 运维轮换用；缺省则首次启动生成并打印
```

systemd 单元（`deploy/sandboxd.service`，随仓库提供）：

```ini
[Unit]
Description=sandboxd agent sandbox runtime
After=docker.service
Requires=docker.service

[Service]
ExecStart=/usr/local/bin/sandboxd -config /etc/sandboxd/sandboxd.conf
Restart=always
RestartSec=2
LimitNOFILE=65536
StateDirectory=sandboxd

[Install]
WantedBy=multi-user.target
```

首次启动：管理员 Key 会打印到 stdout 一次（journalctl 可查）——立即存入密码管理器，
然后可用它创建租户与租户 Key。

## 3. 气隙（air-gapped）安装

1. 在联网机器上准备产物：`sandboxd` 二进制 + rootfs tarball（Alpine/Ubuntu rootfs）。
2. 目标节点无需镜像仓库。启动后用 admin Key 导入 rootfs：

```bash
curl -X POST https://sbx.internal/v1/templates/import \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"name":"base","rootfs_dir":"/opt/sandboxd/rootfs/alpine","public":true}'
```

rootfs 要求：完整的用户态（含 `/usr/bin`、`/bin/sh` 及动态链接器）。
导入时产品会自动归一化 merged-/usr 布局（`/bin → usr/bin` 符号链接）。

## 4. 升级与回滚

1. `systemctl stop sandboxd`（优雅停机，在途创建被对账收敛）；
2. 替换二进制；`systemctl start sandboxd`；
3. 启动日志确认 `reconciliation complete` 且 `readyz` 200。

数据格式变更向前不兼容时会拒绝启动（而非损坏数据）——先备份（§6）再升级。

## 5. 监控与告警建议

- `/readyz` 非 200 → 后端不可达（立即告警）；
- `/metrics`：`sandboxd_creates_total{source}`（pool 占比应随负载升高）、
  `sandboxd_pool_misses_total`（持续增长=池容量不足）、
  `sandboxd_transitions_total{action}`、`sandboxd_http_requests_total{status}`（5xx 应为 0）；
- 磁盘：`data_dir` 所在卷 >80% 告警（WAL 增长与镜像存储共用卷时尤其重要）；
- 宿主内存：所有沙箱 memory 上限之和 ≤ 宿主物理内存的 80%。

## 6. 备份与恢复

- **一致性备份**：备份 = 复制 `<data_dir>`（WAL + data.json）。运行中备份请使用
  存储层的一致性导出（`Store.Backup`，将来暴露为 admin 端点/CLI）；离线（停机）
  备份直接 `cp -a`。
- **恢复**：清空数据目录后放入备份；启动即恢复全部租户/Key/记录。
  容器本体的存活由 reconciler 重新对账确认。
- **密钥前提**：恢复后的租户 Key 哈希仍在库中有效；admin Key 丢失时设置
  `bootstrap_admin_key` 启动一次即可补种（已有活跃 admin Key 则跳过）。

## 7. 常见运维场景

| 现象 | 排查 | 处置 |
|---|---|---|
| 创建全部 503 | `readyz`、docker 服务状态 | 恢复容器运行时；重试的创建由客户端退避 |
| 创建偶发 409 "exited immediately" | 事件中 `provision failed` 原因 | 多为镜像损坏/缺解释器；重新导入 rootfs |
| 沙箱"消失"，记录 stopped | 事件 `reconcile.fix` | 正常：后端真相回写（外部删除/崩溃被对账） |
| 租户报 402 | `GET /v1/tenants/{id}` 对比用量 | 调配额（PATCH + base_version） |
| 429 频发 | 租户 rates 配置 | 按业务调 rates（PATCH quota.rates） |
| 磁盘增长过快 | `docker system df`、快照用量 | 清理快照（DELETE snapshot）、定期压实（自动，10min） |

## 8. 安全基线清单

- [ ] API 端口置于内网/网关之后，或启用 TLS；
- [ ] admin Key 离线保存，日常使用租户 Key；
- [ ] `enforce_disk_quota=false` 仅在已理解威胁模型 §2.2 时设置；
- [ ] 定期备份 data_dir 并演练恢复；
- [ ] 宿主及时打内核补丁（容器共享内核的限制，见威胁模型）；
- [ ] 需要更强隔离的租户等待/部署 microVM 后端，不要用容器后端假装。

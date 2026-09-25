# sandboxd API 参考（v1）

Base URL：`http(s)://<host>:<port>/v1`。认证：`Authorization: Bearer sk_...`
或 `X-API-Key: sk_...`（兼容 E2B/Daytona 客户端习惯）。

## 通用约定

**错误包络**（所有非 2xx 响应）：

```json
{"error": {"code": "quota_exceeded", "message": "...", "retryable": false, "request_id": "9f3c..."}}
```

错误码（客户端必须以 code 分支，不要依赖 HTTP 状态码或 message 文本）：

| code | HTTP | retryable | 语义 |
|---|---|---|---|
| invalid_request | 400 | 否 | 输入被拒绝（校验失败/格式错误） |
| auth_failed | 401 | 否 | 缺失/无效/过期/吊销的 Key |
| forbidden | 403 | 否 | 已认证但越权（含跨租户访问） |
| not_found | 404 | 否 | 资源不存在 |
| conflict | 409 | 视情况 | 非法状态转换/并发写/后端冲突 |
| quota_exceeded | 402 | 否 | 租户配额（沙箱数/CPU/内存/快照） |
| rate_limited | 429 | 是 | 触发租户级限流（带 Retry-After） |
| unimplemented | 501 | 否 | 后端无法真实强制该策略，拒绝而非假装 |
| backend_unavailable | 503 | 是 | 容器/VM 后端不可达 |
| internal | 500 | 视情况 | 服务端错误（request_id 可用于排查） |

**限流头**：429 响应带 `Retry-After`（秒）与 `X-RateLimit-Limit`。限流按
租户 × 操作组（create/exec/files/snapshot/read/admin）独立预算，令牌桶实现。

**分页**：列表端点返回全量（单机内存有序），`limit` 参数仅对 /v1/events 生效。

## 沙箱

### POST /v1/sandboxes — 创建

`?wait=true`（默认）阻塞到 running/failed；`?wait=false` 立即返回 provisioning 记录。

```json
{
  "template_id": "tpl_...",       // 三选一：模板（公开或本租户）
  "snapshot_id": "snp_...",       // 三选一：从快照恢复
  "base_image": "alpine:3.22",    // 三选一：本节点已存在的镜像引用
  "spec": {
    "cpu_millicores": 500,        // 100..limits.max_cpu_millicores
    "memory_mib": 256,            // 64..limits.max_memory_mib
    "timeout_seconds": 3600,      // 30..86400；到期自动回收
    "env": {"K": "V"},            // 键名 ^[A-Za-z_][A-Za-z0-9_]*$，≤50 项
    "user": "1000",               // 可选
    "workdir": "/root",           // 可选，绝对路径
    "egress": {"mode": "none"}    // none（默认）| allowlist（容器后端拒绝，501）
  },
  "metadata": {"owner": "ci"}     // k≤64 字符 [a-zA-Z0-9._-]，≤20 项
}
```

`201` 返回沙箱记录（含 `id`/`state`/`expires_at`/`backend.ref`）。
磁盘配额（`disk_mib`）在容器后端无法强制——请求该字段将被 501 拒绝，
除非运维显式 `enforce_disk_quota=false`（文档化的接受风险）。

### 其余端点

| 方法与路径 | 说明 |
|---|---|
| `GET /v1/sandboxes?state=running` | 本租户沙箱列表 |
| `GET /v1/sandboxes/{id}` | 单个沙箱（含状态/后端引用/到期时间） |
| `POST /v1/sandboxes/{id}/pause` / `/resume` | 暂停/恢复（幂等错误返回 409 + 当前状态） |
| `POST /v1/sandboxes/{id}/stop?force=` | 停止；force=true 直接 kill |
| `POST /v1/sandboxes/{id}/timeout` | `{timeout_seconds}` 续期 TTL |
| `DELETE /v1/sandboxes/{id}?force=` | 删除；活跃沙箱需 force=true |

### POST /v1/sandboxes/{id}/exec — 执行命令

```json
{"command": "make test", "timeout_ms": 30000, "max_output": 1048576}
// 或 {"argv": ["/bin/echo", "hi"], "workdir": "/root", "user": "1000",
//     "env": {"EXTRA": "1"}}
```

`200` 返回：

```json
{"exit_code": 0, "stdout": "<base64>", "stderr": "<base64>",
 "duration_ns": 152000000, "truncated": false, "timed_out": false}
```

stdout/stderr 为 base64（支持二进制输出）。`truncated=true` 表示输出超过
max_output（默认 1MiB）被截断；`timed_out=true` 表示命中硬超时（进程被终止）。
非零退出码不构成 HTTP 错误——以 `exit_code` 为准。

### 文件 API

| 端点 | 说明 |
|---|---|
| `PUT /v1/sandboxes/{id}/files?path=/root/a.txt` | 原始 body 上传（自动建父目录）；JSON 方式：`{"path","content_b64","mode"}` |
| `GET /v1/sandboxes/{id}/files?path=...` | 下载（超出单文件上限报 quota，不截断） |
| `GET /v1/sandboxes/{id}/files/list?path=/root` | 单层目录列表 |
| `DELETE /v1/sandboxes/{id}/files?path=...` | 删除文件/目录树 |

路径必须绝对、不含 `..`、无控制字符（400 拒绝）。默认单文件上限 100MiB。

## 快照与模板

| 端点 | 说明 |
|---|---|
| `POST /v1/snapshots` | `{sandbox_id, name}`；要求沙箱 running/paused；名称租户内唯一；受配额约束，超配额回滚已生成镜像 |
| `GET /v1/snapshots` `/v1/snapshots/{id}` | 列表/详情（仅本租户） |
| `DELETE /v1/snapshots/{id}` | 删除（连带后端镜像，尽力而为） |
| `GET /v1/templates` | 可见模板（平台公开 + 本租户私有） |
| `POST /v1/templates/import` | admin：`{name, rootfs_dir, public, default_spec}` 从节点本地 rootfs 目录导入（气隙部署路径） |

## 事件（审计）

`GET /v1/events?tenant=&sandbox=&action=&actor=&limit=&before=`
租户 Key 自动限定本租户；admin Key 可用 `tenant=` 过滤全部。
动作类型：`sandbox.create/pause/resume/stop/delete/expire/extend/exec`、
`file.put/get/delete`、`snapshot.create/delete`、`key.create/revoke/auth_failed`、
`tenant.create/update`、`template.create/delete`、`reconcile.fix` 等。
Detail 只含非敏感元数据（ID/字节数/路径），永不含文件内容与密钥。

## 租户与 Key 管理（admin Key）

| 端点 | 说明 |
|---|---|
| `POST /v1/tenants` | `{name, quota{max_sandboxes, max_cpu_millicores, max_memory_mib, max_snapshots, max_snapshot_bytes, rates{group:{per_second,burst}}}}` |
| `GET /v1/tenants` `GET /v1/tenants/{id}` | 列表/详情 |
| `PATCH /v1/tenants/{id}` | `{status: "active"|"suspended", quota: {...}, base_version}`；CAS 并发保护 |
| `POST /v1/tenants/{id}/keys` | `{name, ttl_seconds?}` → 响应含 `secret`（**唯一一次**展示明文） |
| `GET /v1/tenants/{id}/keys` | 列表（只含 prefix，绝无哈希/明文） |
| `DELETE /v1/tenants/{id}/keys/{key_id}` | 吊销（幂等） |

admin Key 对沙箱资源操作必须携带 `X-Tenant-Id: tnt_...` 指定目标租户，
否则 403——管理员不会在无意识中触碰租户资源。

## 运维端点

| 端点 | 说明 |
|---|---|
| `GET /healthz` | 存活 |
| `GET /readyz` | 就绪（后端 Ping 通过） |
| `GET /metrics` | Prometheus 文本格式（HTTP 请求、创建来源、池命中、状态迁移计数） |

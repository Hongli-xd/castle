# PoC —— Sandcastle 缺陷复现集

这些程序/脚本用于把审查报告中的结论从"静态推断"变成"可运行复现"。
**它们只读代码、不改产品逻辑**：Go 程序只调用 `internal/store` 的**导出 API**，
脚本只通过 REST API 操作 daemon。

## 运行前提

```bash
cd castle
make build        # 生成 bin/sandboxd（脚本需要）
# 涉及 Docker 的脚本还需要可用的 Docker 与基础镜像：
colima start --cpu 4 --memory 6
docker tag alpine:3.22 sandboxd-test-base:alpine322
```

## 清单

| PoC | 命令 | 目标缺陷 | 缺陷位置 | 预期输出 |
|---|---|---|---|---|
| 1 WAL 竞态 | `go run ./poc/walrace` | `Put*` 在锁外写 WAL，与 `Compact()` 并发 → 已确认写入丢失 | `internal/store/store.go:276-290`（6 处）、`internal/store/wal.go:233-264` | 120/120 轮丢数据，丢失率约 4% |
| 2 撕裂帧 | `go run ./poc/tornframe` | 撕裂写的"回滚"是 `Truncate(walSize())`（空操作）→ 坏帧之后的已 fsync 事务在重启时被整体丢弃 | `internal/store/wal.go:43-53`、`62-68` | 坏帧后 2 条已确认事务消失，WAL 374→94 字节 |
| 3 全局锁 | `go run ./poc/lockleak` | `lockFile` 是包级全局变量 → 关闭 Store A 会解掉 Store B 的锁并泄漏 fd | `internal/store/wal.go:285-308` | 关闭 A 后 B 的锁被释放；残留 1 个 fd |
| 4 exec 上限 | `./poc/probe_live.sh` | `max_output` 无平台上限（`timeout` 有，`max_output` 没有） | `internal/service/lifecycle.go:222-238` | `max_output=2e9` 返回 200；单请求物化 254 MiB |
| 5 认证放大 | `./poc/probe_live.sh` | 限流在 `authenticate` 内侧 → 认证失败绕过限流；每次失败一次 fsync | `internal/httpapi/server.go:95-97, 164-195` | 400 请求 → 400 帧 + 400 fsync，**零 429** |
| 6 隔离泄漏面 | `./poc/probe_isolation.sh` | 共享内核：租户可读宿主内核版本/内存/负载/DMI | `internal/runtime/oci/backend.go:114-154`（命名空间正确，但内核共享） | 128 MiB 沙箱读到宿主 5910 MiB + `6.8.0-117-generic` |
| 7 指标标签 | `./poc/probe_live.sh` | `metrics.key()` 只用标签名拼 key → 标签值全部丢失；`/metrics` 未认证 | `internal/metrics/metrics.go:35-37, 67-79`；`internal/httpapi/server.go:52` | 带标签序列 0 个；无凭证 HTTP 200 |

> PoC 1–3 独立于 Docker，可单独运行。
> PoC 4–7 会各自启动一个**独立的临时 daemon**（端口 18123 / 18124，
> 数据目录 `/tmp/poc-*-data`），退出时通过 trap 杀掉，不触碰你正在跑的服务。

## 环境相关的两处适配

1. **`--noproxy '*'`**：本机设置了 `HTTP_PROXY`，会拦截对本机端口的请求
   （表现为莫名其妙的 307/502）。脚本内所有 curl 都显式直连。
2. **改用响应体大小而非 RSS**：本环境的 `ps` 被沙箱禁止
   （`operation not permitted: ps`）。`probe_live.sh` 因此用
   `curl -w '%{size_download}'` 度量控制面必须物化的字节数——
   要返回 N 字节 stdout，daemon 必须先在内存里收满 N 字节，结论等价。

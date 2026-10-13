# pi-sbx — 把 pi 的 bash 工具路由进 sandboxd

按 pi 官方《Run Pi in an isolated environment》的 **Gondolin 模式**实现：
pi 进程与 LLM 凭据留在宿主机，`bash` 工具的命令经 REST API 在 sandboxd 沙箱内执行。

## 部署形态与安全边界

| 内容 | 位置 |
|---|---|
| pi 进程、LLM API Key | 宿主机（不进沙箱） |
| bash 命令、命令产生的文件 | 沙箱（隔离文件系统，宿主机不可见） |
| 宿主机工作目录 | **默认不进沙箱**；可经 `SBX_WORKDIR` 只读挂载进沙箱（见下） |

默认仍不挂载宿主目录，沙箱内代码无法读写宿主机文件，边界最硬。v1.1.6 起新增**可选的
只读挂载**：把工作目录以只读方式挂进沙箱，让 bash 能 `cat`/`ls`/`python test.py`
读到真实项目，但**写不回宿主机**——这是解决"bash 里看不到项目文件"的 UX 缺陷，不是
放开写边界。写边界依然钉死：沙箱对宿主永远只读。

## 只读挂载（可选）

```bash
export SBX_WORKDIR="$PWD"     # 要挂进沙箱的宿主目录（默认即 process.cwd()）
export SBX_MOUNT_TARGET=/workspace   # 沙箱内出现的位置（默认 /workspace）
# SBX_NO_MOUNT=1 可完全关闭挂载，回到旧行为
```

**前置条件**：daemon 必须把该目录（或其父目录）写进 `mount.allow` 白名单，否则创建
沙箱会被**诚实拒绝**（403，而非"文件静默不存在"）。白名单是防「租户把 /etc 或
/root/.ssh 挂进沙箱读全盘」的边界——挂载只能读操作员显式允许的目录。

```bash
# sandboxd 配置（或环境变量 SBXD_MOUNT_ALLOW）
mount.allow = /Users/you/WorkBuddy
```

只读性由三处证据共同保证：挂载是只读 bind mount（类型层面无"可写"可传）、沙箱内写入
返回 Read-only file system、宿主机文件不变（`tests/mount_test.go` 端到端断言）。

## 安装

```bash
mkdir -p ~/.pi/agent/extensions
cp -R integrations/pi/pi-sbx ~/.pi/agent/extensions/
```

## 使用

```bash
export SBX_URL=http://127.0.0.1:8080
export SBX_API_KEY=sk_...            # 租户级 API Key
export SBX_TEMPLATE=tpl_...          # 模板 ID（sbxctl templates 获取）

pi -e ~/.pi/agent/extensions/pi-sbx
```

pi 会话中的所有 bash 命令将在沙箱内执行；会话结束时沙箱自动销毁。

## 使用 Z.ai GLM 模型（glm-5.3-flash）

pi 内置 `zai` provider（OpenAI 兼容，走 GLM Coding Plan 端点）。需要三件事：

```bash
# 1) 模型凭据：ZAI_API_KEY 指向你的 Z.ai Coding Plan Key（不要写进仓库）
export ZAI_API_KEY=<你的-key>

# 2) 本机网络差异：若 Node 报 unable to get local issuer certificate，
#    指向完整的 CA 捆绑包（macOS brew 环境实测有效）
export NODE_EXTRA_CA_CERTS="/opt/homebrew/etc/ca-certificates/cert.pem"

# 3) 指定模型启动（端点 api.z.ai/api/coding/paas/v4 为国内直连，勿挂代理）
pi --provider zai --model glm-5.3-flash -e ~/.pi/agent/extensions/pi-sbx
```

已验证的端到端路径：GLM-5.3-flash 作为 pi 的模型，bash 工具经本扩展在
sandboxd 沙箱内执行（宿主机看不到沙箱内写入的文件）。

### 排障（2026-10 实测踩过的坑）

1. **扩展冲突**：若 `~/.pi/agent/extensions/pi-sbx` 已被自动发现，又显式
   `-e <repo>/integrations/pi/pi-sbx`，会报 `Tool "bash" conflicts`。
   用 `-ne -e <path>` 只加载一份。
2. **凭据库锁残留**：报 `EEXIST ... mkdir '~/.pi/agent/auth.json.lock'` 时，
   `rm -rf ~/.pi/agent/*.lock` 再跑；或直接用 `--api-key <key>` 绕过凭据库。
3. **reasoning 模型提速**：`glm-5.3-flash` 是推理模型，非交互脚本加
   `--thinking minimal` 可少等一轮思考。

### 无头脚本的实测验证（2026-10-11）

```bash
# 起 daemon → 建租户/Key/模板 → 拿 SBX_URL / SBX_API_KEY / SBX_TEMPLATE
export SBX_KEEP=1   # 保留沙箱以便事后核对
MARKER="pi-sbx-proof-$(date +%s)"
pi --provider zai --model glm-5.3-flash --thinking minimal --api-key "$ZAI_API_KEY" \
   -ne -e integrations/pi/pi-sbx -p \
   "Use your bash tool to run this exact command and report its output verbatim: \
    echo hello-from-sandbox > /tmp/$MARKER && cat /tmp/$MARKER && echo ARCH=\$(uname -m)"

# 决定性证据：文件在沙箱内、不在宿主机
curl "$SBX_URL/v1/sandboxes/<id>/files?path=/tmp/$MARKER" -H "Authorization: Bearer $SBX_API_KEY"
#   → hello-from-sandbox
ls "/tmp/$MARKER"   # → No such file or directory（宿主机看不到）
```

该路径已实测：`exec_count` 计入计量、`sandbox.create`/`sandbox.exec` 进入审计、
文件系统边界由「沙箱内可见、宿主机不可见」证明。

## 环境变量

| 变量 | 说明 |
|---|---|
| `SBX_URL` | daemon 地址（默认 http://127.0.0.1:8080） |
| `SBX_API_KEY` | 租户 API Key（必填） |
| `SBX_TEMPLATE` | 启动沙箱所用模板（必填，除非用 SBX_SANDBOX_ID） |
| `SBX_SANDBOX_ID` | 附加到已存在的沙箱（调试/续用） |
| `SBX_CPU` / `SBX_MEM` / `SBX_TTL` | 规格覆盖：毫核 / MiB / 秒 |
| `SBX_WORKDIR` | 只读挂进沙箱的宿主目录（默认 `process.cwd()`） |
| `SBX_MOUNT_TARGET` | 沙箱内挂载点（默认 `/workspace`） |
| `SBX_NO_MOUNT=1` | 关闭工作目录挂载，回到旧行为 |
| `SBX_KEEP=1` | 会话结束后保留沙箱（配合 `sbxctl exec` 事后排查） |

## 范围声明（诚实语义）

**这是「执行隔离」，不是「Agent 隔离」。** 两句话把边界钉死：

- 被隔离的是**命令执行**（`bash` 工具在沙箱内跑，隔离可自证、可计量审计）；
  pi 进程、LLM 凭据、以及 `read/write/edit/ls/grep/find` 六个文件工具**仍在宿主机**。
- 所以"防模型生成的代码作恶"成立；"把 Agent 整个关进沙箱"**不成立**——Agent 仍能
  经文件工具直接读写你的项目文件。

v1 只路由 `bash` 一个工具（执行面），`read/write/edit` 仍在宿主机工作目录上操作。
**这不是漏配，是刻意的架构取舍**：把文件工具也路由进 sandboxd，补不上"Agent 隔离"——
pi 进程还在宿主，模型照样能经 pi 进程本身或其它扩展读写宿主；而要让文件工具在沙箱里
操作宿主项目，就必须把宿主目录挂进沙箱，那会**反向削弱**"沙箱读不到宿主文件"这条边界。

v1.1.6 的工作目录只读挂载是**对这一边界的窄幅、显式、操作员授权的例外**：只读、只挂
白名单目录、默认关闭（`no_host_directory_is_mounted` 金丝雀保持默认通过）。它解决的是
"bash 看不到项目"的可用性，不是把写边界放开——沙箱仍**永远写不回宿主机**。

若客户要 Agent 级隔离，正解是**把整个 pi 进程跑进沙箱**（第二模式，见
`docs/RESEARCH.md` §2.5），而不是往本扩展里塞文件工具。安全边界以本文表格为准。

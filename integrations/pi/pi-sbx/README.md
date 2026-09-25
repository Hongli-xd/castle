# pi-sbx — 把 pi 的 bash 工具路由进 sandboxd

按 pi 官方《Run Pi in an isolated environment》的 **Gondolin 模式**实现：
pi 进程与 LLM 凭据留在宿主机，`bash` 工具的命令经 REST API 在 sandboxd 沙箱内执行。

## 部署形态与安全边界

| 内容 | 位置 |
|---|---|
| pi 进程、LLM API Key | 宿主机（不进沙箱） |
| bash 命令、命令产生的文件 | 沙箱（隔离文件系统，宿主机不可见） |
| 宿主机工作目录 | **不进沙箱**（与 gondolin 的 /workspace 挂载不同；需要传文件时用 heredoc） |

这是刻意的取舍：不挂载宿主目录，沙箱内代码无法读写宿主机文件，边界更硬。

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

## 环境变量

| 变量 | 说明 |
|---|---|
| `SBX_URL` | daemon 地址（默认 http://127.0.0.1:8080） |
| `SBX_API_KEY` | 租户 API Key（必填） |
| `SBX_TEMPLATE` | 启动沙箱所用模板（必填，除非用 SBX_SANDBOX_ID） |
| `SBX_SANDBOX_ID` | 附加到已存在的沙箱（调试/续用） |
| `SBX_CPU` / `SBX_MEM` / `SBX_TTL` | 规格覆盖：毫核 / MiB / 秒 |
| `SBX_KEEP=1` | 会话结束后保留沙箱（配合 `sbxctl exec` 事后排查） |

## 范围声明（诚实语义）

v1 路由 `bash` 一个工具（执行面），read/write/edit 仍在宿主机工作目录上操作。
"全部工具路由 + 工作目录挂载" 对应 gondolin 的完整模式，在路线图中
（需要 sandboxd 文件 API 之上的双向同步层）。安全边界以本文表格为准。

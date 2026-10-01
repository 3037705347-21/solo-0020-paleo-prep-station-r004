# 古生物修复制备台

古生物修复制备台是一个使用 Go 编写的后端基线，用于记录化石标本、审批修复方案、回填制备进度，并在全部工序完成后办理研究借阅放行。

该服务只使用进程内仓储，便于在本机重复启动和检查。它不依赖数据库、远端接口或其他在线资源。

## 目录

- `cmd/paleo-prep-station`：HTTP 服务入口，监听地址、各类超时和关闭期限全部来自配置。
- `cmd/workflowcheck`：针对一个**已在运行**的服务、通过真实公共 HTTP 路由执行四条业务链路的本地验证命令；它自身不再启动任何监听器。
- `internal/config`：服务端与探针的统一配置面（命令行标志 > 环境变量 > 内置默认值），含资源边界校验。
- `internal/httpapi`：路由、请求解码、状态码映射和 JSON 输出。
- `internal/station`：跨领域对象的编排、事务边界和业务流程。
- `internal/domain`：标本、方案、进度和放行记录的状态与不变量。
- `internal/repository`：互斥保护的内存仓储和原子提交。
- `internal/contracts`：稳定错误模型和公共请求结构。
- `scripts/verify-lifecycle.sh`：可重复的生命周期验证脚本（构建、配置校验、启动、四条工作流、并发探针、主动停止、端口冲突）。
- `config/`：运行配置样例（`paleo-prep-station.env` 为历史默认值，`paleo-prep-station.ci.env` 为不同地址、更紧边界的备用配置）。

## 构建与运行

```bash
go build ./...

# 不提供任何配置时，等价于历史内置默认值
go run ./cmd/paleo-prep-station

# 使用配置文件
set -a; . ./config/paleo-prep-station.env; set +a
go run ./cmd/paleo-prep-station

# 或用命令行标志逐项覆盖（标志优先于环境变量）
go run ./cmd/paleo-prep-station \
  --addr 127.0.0.1:18133 \
  --read-header-timeout 5s --read-timeout 15s --write-timeout 15s \
  --idle-timeout 60s --shutdown-timeout 5s
```

服务先在前台绑定监听端口再开始服务，因此端口占用会在 LISTEN 阶段直接失败（退出码 1）。收到 SIGINT/SIGTERM 后，在 `--shutdown-timeout` 宽限期内排空在途请求，随后进程退出码 0 并释放监听端口；宽限期耗尽则强制关闭连接并以非零码退出。

服务启动后，`GET /healthz` 返回 `{"status":"ok"}`。

## 配置项

所有字段的优先级均为：命令行标志 > 环境变量 > 默认值。服务端：

| 标志 | 环境变量 | 默认值 | 含义 |
| --- | --- | --- | --- |
| `--addr` | `PPS_ADDR` | `127.0.0.1:18133` | 监听地址（host:port） |
| `--read-header-timeout` | `PPS_READ_HEADER_TIMEOUT` | `5s` | 读请求头上限 |
| `--read-timeout` | `PPS_READ_TIMEOUT` | `15s` | 读完整请求上限（须 ≥ header 超时） |
| `--write-timeout` | `PPS_WRITE_TIMEOUT` | `15s` | 写响应上限 |
| `--idle-timeout` | `PPS_IDLE_TIMEOUT` | `60s` | keep-alive 空闲上限 |
| `--shutdown-timeout` | `PPS_SHUTDOWN_TIMEOUT` | `5s` | 收到停止信号后的排空宽限期 |

探针（workflowcheck）：

| 标志 | 环境变量 | 默认值 | 含义 |
| --- | --- | --- | --- |
| `--workflow` | — | 必填 | 四条工作流之一 |
| `--base-url` | `PPS_PROBE_BASE_URL` | 必填 | 已运行服务的绝对 http(s) 地址；探针不会自建监听 |
| `--request-timeout` | `PPS_PROBE_REQUEST_TIMEOUT` | `5s` | 单次探针请求上限，必须为正 |
| `--catalog-number` | `PPS_PROBE_CATALOG_NUMBER` | `PPS-2026-001` | 探针登记的目录号；并发探针必须各自不同 |

任何超时为零或负、header 超时大于读超时、地址不是 host:port、base-url 缺失或不是绝对 http(s) URL 等，都会在 CONFIG 阶段被拒绝，退出码为 2，且不会打开任何套接字。配置调整只能改变资源边界，不能关闭边界（例如用 0 放宽超时）。

## 本地验证

先启动服务，再针对它运行检查。检查不再自建临时 HTTP 服务：

```bash
set -a; . ./config/paleo-prep-station.env; set +a
go run ./cmd/paleo-prep-station &

for w in register-specimen approve-prep-plan log-preparation release-for-study; do
  go run ./cmd/workflowcheck --workflow "$w" \
    --base-url "$PPS_PROBE_BASE_URL" \
    --catalog-number "PPS-2026-$w"
done
```

并发探针必须使用互不相同的目录号（目录号唯一是业务不变量，重复登记返回 409）：

```bash
for i in 1 2 3 4; do
  go run ./cmd/workflowcheck --workflow log-preparation \
    --base-url http://127.0.0.1:18133 \
    --catalog-number "PPS-CONC-$i" &
done; wait
```

### 一键生命周期验证

```bash
scripts/verify-lifecycle.sh
```

该脚本依次验证并在每个失败点给出具名阶段：构建 → 非法配置拒绝（CONFIG，退出码 2）→ 正常启动与 `/healthz` → 四条工作流状态结果 → 四个并发探针 → 重复目录号仍返回 409 → SIGTERM 干净退出且端口释放（STOP）→ 端口占用在 LISTEN 阶段失败（退出码 1）→ 标志/环境变量优先级 → 在不同地址、更紧超时的备用配置上完整重跑一轮。每轮结束都校验进程与监听全部归零。

四条工作流断言的状态结果（`registered` / 方案 `approved` 且标本 `preparing` / 方案 `completed` 且标本 `stabilized` / 标本 `available`）不因配置调整而改变；脚本同时用 409 用例防止"通过放宽唯一约束或绕过工作流让检查变绿"。

## 主要接口

- `GET /specimens` 与 `POST /specimens`
- `GET /specimens/{id}`
- `POST /specimens/{id}/plans`
- `GET /plans/{id}`
- `POST /plans/{id}/approval`
- `POST /plans/{id}/progress`
- `POST /specimens/{id}/release`

## 测试边界

本基线暂不包含 `go test` 单元测试。四条生产级 HTTP 链路检查与 `scripts/verify-lifecycle.sh` 共同作为端到端烟雾与生命周期验证；后续任务阶段补充红绿验证测试时，保留这些检查作为端到端烟雾路径。

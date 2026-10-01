# 古生物修复制备台

古生物修复制备台是一个使用 Go 编写的后端基线，用于记录化石标本、审批修复方案、回填制备进度，并在全部工序完成后办理研究借阅放行。

该服务只使用进程内仓储，便于在本机重复启动和检查。它不依赖数据库、远端接口或其他在线资源。

## 目录

- `cmd/paleo-prep-station`：HTTP 服务入口和监听参数。
- `cmd/workflowcheck`：通过真实 HTTP 处理器执行四条业务链路的本地验证命令。
- `internal/httpapi`：路由、请求解码、状态码映射和 JSON 输出。
- `internal/station`：跨领域对象的编排、事务边界和业务流程。
- `internal/domain`：标本、方案、进度和放行记录的状态与不变量。
- `internal/repository`：互斥保护的内存仓储和原子提交。
- `internal/contracts`：稳定错误模型和公共请求结构。

## 构建与运行

```bash
go build ./...
go run ./cmd/paleo-prep-station
```

服务启动后，`GET /healthz` 返回就绪状态。

## 运行配置

监听地址、读写与空闲超时、关闭期限，以及验证探针的路径、超时、并发度和就绪等待，全部从 `internal/config` 的显式配置进入，取值优先级为 **flag > `PPS_*` 环境变量 > 内置默认值**。非法取值（含零超时、零并发）在 config 阶段被拒绝，进程以退出码 1 退出，不会静默回退。

| 配置项 | flag | 环境变量 | 默认值 |
| --- | --- | --- | --- |
| 监听地址 | `--addr` | `PPS_ADDR` | `127.0.0.1:18133` |
| 读头超时 | `--read-header-timeout` | `PPS_READ_HEADER_TIMEOUT` | `5s` |
| 读超时 | `--read-timeout` | `PPS_READ_TIMEOUT` | `15s` |
| 写超时 | `--write-timeout` | `PPS_WRITE_TIMEOUT` | `15s` |
| 空闲超时 | `--idle-timeout` | `PPS_IDLE_TIMEOUT` | `60s` |
| 关闭期限 | `--shutdown-timeout` | `PPS_SHUTDOWN_TIMEOUT` | `5s` |
| 探针路径 | `--probe-path` | `PPS_PROBE_PATH` | `/healthz` |
| 探针单请求超时 | `--probe-timeout` | `PPS_PROBE_TIMEOUT` | `5s` |
| 探针并发度 | `--probe-concurrency` | `PPS_PROBE_CONCURRENCY` | `8` |
| 就绪等待预算 | `--probe-ready-timeout` | `PPS_PROBE_READY_TIMEOUT` | `5s` |

启动时会输出一行 `effective config: ...`，可据此核对当轮实际生效的取值。示例：

```bash
PPS_READ_TIMEOUT=8s PPS_SHUTDOWN_TIMEOUT=2s \
  go run ./cmd/paleo-prep-station --addr 127.0.0.1:19000
```

## 主要接口

- `GET /specimens` 与 `POST /specimens`
- `GET /specimens/{id}`
- `POST /specimens/{id}/plans`
- `GET /plans/{id}`
- `POST /plans/{id}/approval`
- `POST /plans/{id}/progress`
- `POST /specimens/{id}/release`

## 本地验证

每条业务链路都可以独立执行：

```bash
go run ./cmd/workflowcheck --workflow register-specimen
go run ./cmd/workflowcheck --workflow approve-prep-plan
go run ./cmd/workflowcheck --workflow log-preparation
go run ./cmd/workflowcheck --workflow release-for-study
# 或一次执行全部（固定顺序，输出可重复对比）
go run ./cmd/workflowcheck --workflow all
```

这些检查不再使用 `httptest`，而是在与生产入口相同的资源边界内，通过 `internal/config` + `internal/server` 起真实的 TCP 监听：监听 → 就绪轮询 → 并发探针 → 工作流请求 → 排空客户端连接池 → 优雅关闭。临时监听默认 `127.0.0.1:0`，探针配置通过同一套 flag / `PPS_*` 进入。

### 可重复的运行验证流程

```bash
scripts/verify-runtime.sh [轮数]   # 默认 1 轮，例如 scripts/verify-runtime.sh 3
```

每轮依次覆盖：build（`go build` + `go vet`，`CGO_ENABLED=0`）、正常启动（flag 覆盖环境变量、就绪）、并发探针（8 路全 200）、主动停止（SIGTERM 退出码 0 并归零）、端口冲突（第二实例退出码 2、标记 `stage listen`）、配置边界（零超时/零/非法取值退出码 1、标记 `stage config`）、四条工作流在默认与调优两套配置下状态结果逐字节一致，以及每轮结束的归零审计。任何失败都带阶段标签并非零退出。

退出码按阶段固定：

| 退出码 | 阶段 |
| --- | --- |
| 0 | 成功 |
| 1 | config（配置非法） |
| 2 | listen（含端口冲突） |
| 3 | serve |
| 4 | shutdown（关闭期限内未优雅退出） |
| 5 | ready（探针未就绪） |
| 6 | probe（并发探针失败） |
| 7 | workflow（业务状态不符合预期） |

配置调整只改变资源边界，不能改变四条工作流的状态结果，也不能通过放宽超时或绕过工作流让检查变绿——`Validate` 会拒绝零或负值，验证脚本会对比两套配置下的 `workflow-status` 输出。

## 环境变量

服务无外部依赖，不需要任何外部服务；所有运行参数都通过上表中的 `PPS_*` 环境变量或同名 flag 配置。

## 测试边界

本基线按要求暂不生成单元测试、测试夹具或端到端测试文件。后续任务阶段会补充红绿验证测试，届时再替换或扩展当前的生产级链路检查。

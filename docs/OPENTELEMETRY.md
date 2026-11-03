# OpenTelemetry 链路追踪与日志上报（自定义功能）

sub2api 可以通过 OTLP 把**链路追踪和日志**上报到 SigNoz（或任何兼容 OTLP 的后端），并接上调用方的 trace：

- 调用方发来的 W3C `traceparent` 会被识别，sub2api 的 span 挂在调用方的 trace 下面。
- 请求处理过程中写的日志带有同一条 trace 的上下文，在 SigNoz 里能从 trace 直接跳到对应日志。

默认关闭，全部通过标准的 `OTEL_*` 环境变量配置，不需要改 `config.yaml`。

## 启用

只要设置了 OTLP endpoint，trace 和日志就会一起启用：

```bash
# SigNoz OTLP HTTP（默认协议 http/protobuf）
OTEL_EXPORTER_OTLP_ENDPOINT=http://signoz-otel-collector:4318

# 或者用 gRPC
OTEL_EXPORTER_OTLP_ENDPOINT=http://signoz-otel-collector:4317
OTEL_EXPORTER_OTLP_PROTOCOL=grpc

# 自建 collector 前面有反向代理做 Bearer 认证时（空格要写成 %20）
OTEL_EXPORTER_OTLP_ENDPOINT=https://<你的 SigNoz 域名>
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer%20<token>

# SigNoz Cloud 需要带 ingestion key
OTEL_EXPORTER_OTLP_ENDPOINT=https://ingest.<region>.signoz.cloud:443
OTEL_EXPORTER_OTLP_HEADERS=signoz-ingestion-key=<key>
```

启动日志里出现 `opentelemetry enabled`（带 `traces`、`logs` 两个开关字段）即表示生效。配置有误（比如不支持的协议）时会打印 `OpenTelemetry disabled: ...`，服务照常启动，只是不上报。

| 变量 | 说明 |
| --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | 同时启用 trace 和日志 |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` | 只为某一类单独指定地址 |
| `OTEL_TRACES_EXPORTER=none` / `OTEL_LOGS_EXPORTER=none` | 单独关闭 trace 或日志上报 |
| `OTEL_EXPORTER_OTLP_PROTOCOL`（及 `_TRACES_`、`_LOGS_` 变体） | `http/protobuf`（默认）或 `grpc` |
| `OTEL_EXPORTER_OTLP_HEADERS`、`_TIMEOUT`、`_INSECURE`、`_CERTIFICATE`、`_COMPRESSION` | 由 OpenTelemetry SDK 直接读取 |
| `OTEL_SERVICE_NAME` | 默认 `ai-gateway`（与 `log.service_name` 无关） |
| `OTEL_RESOURCE_ATTRIBUTES` | 额外的资源属性，可覆盖默认值 |
| `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG` | 采样策略，默认 `parentbased_always_on` |
| `OTEL_BSP_*` / `OTEL_BLRP_*` | trace / 日志的批量上报参数 |
| `OTEL_SDK_DISABLED=true` | 全部关闭 |

默认的资源属性：`service.name`（`ai-gateway`）、`service.version`（sub2api 版本号）、`deployment.environment`（取 `log.env`；用的是旧 key，与我们 SigNoz 里其他服务保持一致）、`host.name`。

`log.env` 的默认值是 `production`，每个环境都要设置 `log.env`（或 `LOG_ENV`），或者用 `OTEL_RESOURCE_ATTRIBUTES=deployment.environment=staging` 覆盖，否则所有环境都会被标成 `production`。

## 调用方需要做什么

只需要在请求里带上 W3C `traceparent` 头。用 OpenTelemetry 埋点的 HTTP 客户端（各语言 SDK 的自动埋点）会自动带上，不用改代码。

不支持 OTel 的客户端也可以手动带：

```
traceparent: 00-<32 位十六进制 trace_id>-<16 位十六进制 span_id>-01
```

## 会产生哪些 span

**入口 span**（Server），每个匹配到路由的请求一个，名称形如 `POST /v1/messages`：

- `http.request.method`、`http.route`、`url.path`、`http.response.status_code`、`client.address`、`user_agent.original`
- `sub2api.account_id`、`sub2api.user_id`、`sub2api.platform`、`gen_ai.request.model`：最终命中的账号、用户、平台和模型
- 5xx 响应标记为错误
- 流式（SSE）和 WebSocket 请求的 span 持续到连接结束

**上游 span**（Client），每次请求上游都会记一个，名称形如 `upstream POST api.anthropic.com`：

- 换账号重试时，每次尝试是一个单独的 span，带各自的 `sub2api.account_id`，能直接看出重试了几次、每次耗时和失败原因
- 流式响应的 span 持续到响应体读完；`first_byte` 事件标出首字节时间
- `url.full` 不包含 query string（有的上游会把 API key 放在 query 里）
- 4xx/5xx 和网络错误标记为错误

**不会记录的请求**：`/health`、`/setup/status`、没有匹配到路由的请求（前端静态资源、404），以及不在入口请求内的后台上游调用（定时任务等）。

## 日志

### 上报

开启后，所有写到 stdout/文件的日志会同时通过 OTLP 上报（配置加载完成之前的少量启动日志除外），级别与 `log.level` 一致（运行时调整级别也同步生效）。实现用的是 OpenTelemetry 官方的 zap 桥接 [`otelzap`](https://pkg.go.dev/go.opentelemetry.io/contrib/bridges/otelzap)，不需要额外部署日志采集。

请求处理过程中写的日志（通过请求级 logger）会带上这条请求的 trace 上下文，SigNoz 日志的 `trace_id`、`span_id` 列会被填上，能从 trace 直接跳到日志。即使请求没有被采样，日志里也有 `trace_id`，能和调用方的 trace 对上。

OpenTelemetry SDK 自身的错误（比如上报失败）只写本地 stdout/文件，不会再经过 OTLP 上报，避免上报失败的错误日志又被送进同一个失败的导出器里形成循环。

### 本地输出格式

| 构建方式 | 默认格式 |
| --- | --- |
| 打包构建：CI 发版的二进制、Docker 镜像（`BuildType=release`） | `json` |
| 本地开发：`go run`、`go build`、`make build`（`BuildType=source`） | `console` |

显式配置的 `log.format` 或 `LOG_FORMAT` 始终优先。JSON 输出使用 zap 自带的 `JSONEncoder`，每行一个 JSON 对象，请求级日志带 `trace_id`、`span_id` 字段。

日志里的 `service` 字段取 `log.service_name`（默认 `sub2api`），会作为普通属性一起上报。想和 trace 的服务名保持一致，可以设置 `log.service_name: ai-gateway`（或 `LOG_SERVICE_NAME=ai-gateway`）。

## 安全边界

- **不会把 trace 上下文透传给上游**：发给 Anthropic、OpenAI 等上游的请求里没有 `traceparent`、`tracestate`、`baggage`，不会额外暴露客户端特征。
- 不设置 OpenTelemetry 的全局 TracerProvider、LoggerProvider 和 Propagator。依赖里自带 OTel 埋点的第三方库（例如 `otelhttp`）保持不生效，不会产生数据，也不会往它们自己的出站请求里注入 trace 头。
- 默认采样策略 `parentbased_always_on` 会遵循调用方的采样标记。sub2api 是公网网关，外部客户端也可以带 `traceparent`。如果要控制上报量，建议用 `OTEL_TRACES_SAMPLER=parentbased_traceidratio` 配合 `OTEL_TRACES_SAMPLER_ARG=0.1`：带 `traceparent` 的请求仍然跟随调用方的采样决定，不带的请求按 10% 采样。日志不受采样影响，全部上报。

## 暂未覆盖

- 上游 WebSocket 连接（OpenAI Responses WebSocket）内部的消息没有单独的上游 span，耗时包含在入口 span 里
- 指标（metrics）不通过 OTLP 上报

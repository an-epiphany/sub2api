## 自定义功能

### OpenAI 403 处理策略

- `rate_limit.openai_403_ignore`：设为 `true` 时跳过 OpenAI 403 的全部账号副作用，不计数、不临时下线、不永久禁用，账号保持可用，并将原始 403 响应直接透传给客户端。
- `rate_limit.openai_403_disable_threshold`：连续 403 次数达到该阈值时才永久禁用账号；未达到阈值时仅临时下线。设为小于或等于 `0` 时使用内置默认值 `3`。

### OpenTelemetry

集成 OpenTelemetry，默认关闭，通过环境变量配置：

- `OTEL_EXPORTER_OTLP_ENDPOINT`：OTLP 上报地址，设置后启用
- `OTEL_EXPORTER_OTLP_HEADERS`：上报请求头，如认证信息
- `OTEL_EXPORTER_OTLP_PROTOCOL`：`http/protobuf`（默认）或 `grpc`
- `OTEL_SERVICE_NAME`：服务名，默认 `ai-gateway`
- `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG`：采样策略
- `LOG_ENV`：环境标识，上报为 `deployment.environment`，默认 `production`

### 打包构建默认 JSON 日志

- CI 发版的二进制和 Docker 镜像默认 `log.format=json`，本地源码构建保持 `console`；显式配置的 `log.format` / `LOG_FORMAT` 优先。

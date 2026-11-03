## 自定义功能

### OpenAI 403 处理策略

- `rate_limit.openai_403_ignore`：设为 `true` 时跳过 OpenAI 403 的全部账号副作用，不计数、不临时下线、不永久禁用，账号保持可用，并将原始 403 响应直接透传给客户端。
- `rate_limit.openai_403_disable_threshold`：连续 403 次数达到该阈值时才永久禁用账号；未达到阈值时仅临时下线。设为小于或等于 `0` 时使用内置默认值 `3`。

### OpenTelemetry 链路追踪与日志上报

- 设置 `OTEL_EXPORTER_OTLP_ENDPOINT` 即启用，通过 OTLP 同时上报 trace 和日志（如 SigNoz），默认关闭，只用标准 `OTEL_*` 环境变量配置。
- 识别调用方的 W3C `traceparent`，网关的 span 挂在调用方的 trace 下；每次请求上游（含换账号重试）各有一个 span，流式响应持续到读完。
- 请求级日志带 trace 上下文，可在 SigNoz 里从 trace 跳到日志。
- 不向上游透传 trace 头，不记录 URL query。默认服务名 `ai-gateway`，环境取 `log.env`。详见 `docs/OPENTELEMETRY.md`。

### 打包构建默认 JSON 日志

- CI 发版的二进制和 Docker 镜像默认 `log.format=json`，本地源码构建保持 `console`；显式配置的 `log.format` / `LOG_FORMAT` 优先。

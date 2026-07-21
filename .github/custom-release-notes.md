## 自定义功能

### OpenAI 403 处理策略

- `rate_limit.openai_403_ignore`：设为 `true` 时跳过 OpenAI 403 的全部账号副作用，不计数、不临时下线、不永久禁用，账号保持可用，并将原始 403 响应直接透传给客户端。
- `rate_limit.openai_403_disable_threshold`：连续 403 次数达到该阈值时才永久禁用账号；未达到阈值时仅临时下线。设为小于或等于 `0` 时使用内置默认值 `3`。

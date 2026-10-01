# AgentRouter WAF 直连适配（header_override wire image）

> AgentRouter（agentrouter.org，AI Coding 公益站）的 Aliyun WAF 会校验请求的 **Stainless SDK 专属 header 组合**。只有携带完整 wire image 的请求才能通过；否则返回 `401 unauthorized client detected`。
>
> 结论：**new-api 渠道用 header_override 直连即可，无需任何中转代理**（curl / Go 客户端均可通过，WAF 不校验 TLS 指纹）。

## 渠道配置

| 配置项 | 值 |
|--------|-----|
| 渠道类型 | **Anthropic Claude**（type=14，走 `/v1/messages`） |
| Base URL | `https://agentrouter.org`（**不带** `/v1`） |
| 模型 | 该 key 计划仅支持 `claude-opus-5`（后端实际模型 `anthropic/claude-opus-5-ps-aws-dst`） |
| 密钥 | AgentRouter 控制台获取的 `sk-...`，支持多 key 轮询 |

## header_override（完整 wire image）

在渠道编辑 → 渠道设置中配置：

```json
{
  "User-Agent": "claude-cli/2.1.158 (external, sdk-cli)",
  "anthropic-version": "2023-06-01",
  "anthropic-beta": "claude-code-20250219,interleaved-thinking-2025-05-14,effort-2025-11-24,redact-thinking-2026-02-12",
  "anthropic-dangerous-direct-browser-access": "true",
  "accept": "application/json, text/event-stream",
  "x-app": "cli",
  "x-stainless-lang": "js",
  "x-stainless-package-version": "0.74.1",
  "x-stainless-os": "Linux",
  "x-stainless-arch": "arm64",
  "x-stainless-runtime": "node",
  "x-stainless-runtime-version": "22.14.0",
  "x-stainless-helper-method": "messages",
  "x-stainless-timeout": "600"
}
```

`Authorization` / `x-api-key` 由 new-api 按渠道密钥自动填充，不需要 override。

## 排障

| 现象 | 原因 | 处理 |
|------|------|------|
| `401 unauthorized client detected` | header 组合不完整（尤其缺 `anthropic-beta` 或 `x-stainless-*`） | 按上表补全 wire image |
| `401 无效的令牌 / invalid key` | wire image 已通过，key 被 AgentRouter 拒绝 | 到 agentrouter.org 控制台换 key |
| `503 no available channel` | WAF 已通过，上游模型池容量不足 | 换模型或稍后重试 |
| `400 content-blocked` | 请求含 `thinking`/`output_config` 等非常规字段，或该 key 计划不支持此模型 | 精简请求体 / 换支持的模型 |

## 参考

- AgentRouter 官方接入文档：https://agentrouter.org/docs/claude-code.html（Claude Code 直连配置）
- 注意：GitHub 上部分项目声称"必须用 Python 同步 anthropic SDK 的 TLS 指纹才能过 WAF"——**实测不成立**，完整 header 组合即可通过（curl 实测 200）。

# 二次开发文档（Fork Docs）

> 本目录只放**本 fork 的二次开发内容**。上游（QuantumNous/new-api）原有的文档在 `docs/` 根目录与各子目录，**保持原样、不修改、不混入**。
>
> 上游文档与 fork 文档的界限：凡是本 fork 新增的功能、适配、部署方式，文档都在本目录；上游功能看上游文档。

## 功能清单与文档映射

| # | 功能 | 代码位置 | 文档 |
|---|------|---------|------|
| 1 | 渠道守卫：模型级/key 级限速排队 + 熔断 + LOCAL_MODE | `pkg/upstream_guard/`、`controller/relay.go`、`middleware/` | [upstream-guard.md](./upstream-guard.md)（含部署配置清单 §9） |
| 2 | 用户额度字段 int→int64（修复 32 位平台溢出） | `model/user.go`、`model/user_cache.go`、`service/quota.go` 等 | 见下方变更记录 0554b71 |
| 3 | Token API key 可编辑 + 错误信息中文化 | `controller/token.go`、`model/token.go`、`i18n/` | 见下方变更记录 |
| 4 | 渠道设置独立保存 API | `controller/channel.go`（`PUT /api/channel/:id/setting`）、前端 `EditChannelModal` | [upstream-guard.md](./upstream-guard.md) |
| 5 | ARMv7/ARM64 交叉编译 + Release CI | `.github/workflows/build-binaries.yml` | 主 README「裸机部署 (ARMv7/ARM64)」章节 |
| 6 | AgentRouter WAF 直连适配（header_override wire image） | 渠道配置（无代码改动） | [agentrouter-wire-image.md](./agentrouter-wire-image.md) |
| 7 | 备份与迁移（data.db + .env） | 运维操作 | [backup-migration.md](./backup-migration.md)（含内网信息，**仅本地，不入库**） |
| 8 | Mock 上游测试服务器 | `scripts/mock_llm_server/` | [upstream-guard.md](./upstream-guard.md) 测试章节 |

## 关键运行依赖（速查）

完整说明见 [upstream-guard.md §9](./upstream-guard.md)，最常忘的三条：

- **429 换 key/换渠道重试**：前提 `RetryTimes > 0`（设置 → 运营设置 → 失败重试次数）
- **HTTP 部署登录被踢**：`COOKIE_SECURE=false`
- **单机自用免 2FA/操作限流**：`LOCAL_MODE=true`

## 变更记录（主要 commit）

| commit | 内容 |
|--------|------|
| `0554b71` | 用户额度字段 int→int64，修复 armv7 大额额度溢出为负数 |
| `f9484df` | README 增加 ARMv7/ARM64 裸机部署、systemd 开机自启与卸载说明 |
| `29d9e31` | upstream-guard 部署配置清单与开关依赖关系 |
| 更早 | upstream_guard 全套（限速排队/熔断/key 轮换/LOCAL_MODE）、i18n 中文化、token key 编辑、渠道设置保存 API、Release CI |

## 维护约定

1. **上游文档不动**：`docs/` 根目录及 `installation/`、`openapi/`、`channel/` 等属于上游，同步上游代码时可直接覆盖，不会丢我们的内容。
2. **fork 文档只进本目录**：新增二次开发功能时，文档写到 `docs/fork/` 并在上面的表格登记。
3. **含内网 IP/密钥的文档不入库**（如 backup-migration.md），由 `.gitignore` 拦截。
4. 本文件是索引，功能细节看对应文档。

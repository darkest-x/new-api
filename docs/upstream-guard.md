# 上游保护：模型级/key级限速 + 熔断 + 本地模式

本文档对应一次二次开发，为 new-api 增加「渠道+模型」「渠道+key+模型」两个粒度的
限速（排队，不报错）与熔断（连续失败后跳过、到时自动恢复），并新增 `LOCAL_MODE`
本地模式开关与一个 mock 上游测试服务器。默认行为保持不变，所有新能力都通过
「渠道 setting JSON」或环境变量显式开启/覆盖。

---

## 1. 改动文件 / 函数清单

### 新增文件
| 文件 | 说明 |
| --- | --- |
| `pkg/upstream_guard/limiter.go` | 滑动窗口均匀摊派限流器（前 burst 个立即、之后摊派，纯标准库） |
| `pkg/upstream_guard/guard.go` | 模型限速、模型熔断、key 熔断三组内存态 + 公开 API |
| `pkg/upstream_guard/guard_test.go` | 限速/熔断单元测试 |
| `scripts/mock_llm_server/main.go` | OpenAI 兼容 mock 上游（限流/5xx/延迟/流式） |
| `docs/upstream-guard.md` | 本文档 |

### 修改文件
| 文件 | 函数/位置 | 改动 |
| --- | --- | --- |
| `dto/channel_settings.go` | `ChannelSettings` | 新增 `RateLimit` / `CircuitBreaker` 两个 per-model 字段 |
| `dto/channel_settings.go` | 新增 `ChannelModelRateLimit`、`ChannelModelCircuitBreaker` | 规则结构体 |
| `common/constants.go` | 新增一组全局默认变量 + `LocalMode` | 见 §3 |
| `common/init.go` | `InitEnv` | 从环境变量读取上述默认值 |
| `constant/context_key.go` | 新增 `ContextKeyRetrySameChannelId` | key 级 429 换 key 重试标记 |
| `model/channel.go` | `GetNextEnabledKey` → 拆出 `GetNextEnabledKeyForModel(model)` | 多 key 选择时跳过该模型下已熔断的 key |
| `middleware/distributor.go` | `SetupContextForSelectedChannel` | 改调用 `GetNextEnabledKeyForModel` |
| `model/channel_cache.go` | `GetRandomSatisfiedChannel` | 选道时过滤已熔断的 (渠道,模型) |
| `model/ability.go` | `GetChannel`（无内存缓存 DB 路径） | 选道时过滤已熔断的 (渠道,模型) |
| `controller/relay.go` | `Relay` 重试循环 | 限速排队等待；成功/失败记录熔断；429 同渠道换 key；429 错误提示 |
| `controller/relay.go` | `getChannel` | 支持「同渠道换 key」短路 |
| `controller/relay.go` | 新增 `shouldTripBreaker` | 判断 429/5xx |
| `controller/relay.go` | `Relay` 的 defer | 429 时追加自助提示文案 |
| `middleware/secure_verification.go` | `SecureVerificationRequired` | `LOCAL_MODE` 下跳过 2FA/Passkey 验证 |
| `middleware/rate-limit.go` | `CriticalRateLimit` | `LOCAL_MODE` 下关闭关键操作限流 |

---

## 2. 配置存储设计

### 2.1 渠道+模型精细化规则 → 渠道 `setting` JSON（`channels.setting` 列）

复用 `dto.ChannelSettings`，新增两个字段（带 `omitempty`，向后兼容，旧渠道不受影响）：

```json
{
  "rate_limit": {
    "agnes-2.5-flash": { "rpm": 40, "burst": 20, "max_wait_seconds": 120 }
  },
  "circuit_breaker": {
    "agnes-2.5-flash": { "threshold": 5, "mode": "fixed", "cooldown_minutes": 5 },
    "gpt-4o": { "threshold": 3, "mode": "daily", "recover_at": "08:00" }
  }
}
```

- `rate_limit.<model>.rpm`：每分钟最大请求数，`0`/缺省 = 不限。
  **粒度是「每个 key」**——上游的每分钟限额是 per-key 的，多 key 渠道下每个 key 独立限速，
  互不影响（轮询/随机都不会串）。
- `rate_limit.<model>.burst`：每分钟窗口内「立即放行」的请求数，`0`/缺省 = `rpm/2`
  （前一半不限制、达到一半后按剩余时间均匀摊派——这正是「前半分钟不冲光额度」的关键）。
- `rate_limit.<model>.max_wait_seconds`：排队上限，`0`/缺省 = 用全局 `MODEL_RATE_LIMIT_MAX_WAIT_SECONDS`。
- `circuit_breaker.<model>.threshold`：连续 429/5xx 达到该次数即熔断，`0`/缺省 = 用全局默认阈值。
- `circuit_breaker.<model>.mode`：`fixed`（固定冷却）或 `daily`（每天固定时间恢复）。
- `circuit_breaker.<model>.cooldown_minutes`：`fixed` 模式冷却分钟数。
- `circuit_breaker.<model>.recover_at`：`daily` 模式恢复时间 `HH:MM`（服务器本地时区）。

写入方式：后台「渠道编辑 → 设置（高级 JSON）」直接粘贴，或 `PUT /api/channel/`（`setting`
字段）。模型名匹配与选道一致（先精确、再 `FormatMatchingModelName` 归一化）。

### 2.2 key 级熔断 → 内存态（无持久化）

key+模型熔断用全局阈值（环境变量），无需 per-key 配置。key 已随 `channels.channel_info`
持久化（多 key 状态），熔断态为运行时内存态，重启即恢复——符合「个人内网、单二进制」。

### 2.3 熔断/限速运行时状态 → 进程内 `sync.Map`（`pkg/upstream_guard`）

单实例部署下无需 Redis/DB。多实例共享是后续扩展点（把 `sync.Map` 换成 Redis 键即可）。

---

## 3. 新配置项：默认值 + 向后兼容

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `MODEL_RATE_LIMIT_MAX_WAIT_SECONDS` | `60` | 模型级限速排队上限（秒） |
| `CIRCUIT_BREAKER_DEFAULT_THRESHOLD` | `5` | 模型级熔断默认连续失败阈值 |
| `CIRCUIT_BREAKER_DEFAULT_COOLDOWN_MINUTES` | `5` | 模型级熔断默认冷却（分钟） |
| `KEY_CIRCUIT_BREAKER_THRESHOLD` | `3` | key+模型熔断默认连续失败阈值 |
| `KEY_CIRCUIT_BREAKER_COOLDOWN_MINUTES` | `5` | key+模型熔断默认冷却（分钟） |
| `LOCAL_MODE` | `false` | 本地模式：关闭 2FA/Passkey 安全验证 + 关键操作限流 |

**向后兼容**：以上全部为新增，未设置时行为与改动前一致（模型级熔断默认阈值 `5`，
如需完全关闭全局熔断，设 `CIRCUIT_BREAKER_DEFAULT_THRESHOLD=0`，此时仅 per-channel
显式配置了 `threshold` 的模型会熔断）。`LOCAL_MODE` 默认 `false`，生产安全行为不变。

---

## 4. 功能实现要点

### 4.1 模型级限速（排队，不报错）
- 位置：`controller/relay.go` 重试循环内、发上游请求前。限速/熔断配置与多 key 标记从
  **context** 读取（`SetupContextForSelectedChannel` 已写入 `ContextKeyChannelSetting` /
  `ContextKeyChannelIsMultiKey`），因为 relay 循环阶段 `ChannelMeta` 尚未初始化、缓存对象不可靠。
- 粒度：**渠道+key+模型**。上游的每分钟限额是 per-key 的，多 key 渠道每个 key 独立限速窗口，
  轮询/随机互不串扰（N 个 key = N×rpm 总容量）。
- 算法：**60 秒滑动窗口 + 均匀摊派**。每个 key 窗口内前 `burst`（默认 `rpm/2`）个请求立即放行、
  只计数不限速；达到一半后，剩余请求按窗口剩余时间均匀摊派（第 k 个请求排在
  `窗口起点 + 60s·(k+1)/rpm`），客户端保持连接、只是响应变慢；窗口名额用尽则等到下一分钟。
  严格保证每分钟 ≤ `rpm`，不会「前半分钟冲光额度、后半分钟报错」。
- 低流量（< rpm/分钟）时名额充足，摊派时刻早于当前时间，请求不排队、无感知。
- 排队超过 `max_wait_seconds` 或客户端取消 → 返回明确错误，不伪装成 429。

### 4.2 模型级熔断（429 按 key、5xx 按渠道）
- 429（限流）：只记 **key+模型** 熔断（`RecordKeyFailure`），单 key 渠道才记渠道+模型——多 key 渠道
  下一个 key 被上游限流不会连累同渠道其他 key。
- 5xx（故障）：记 **渠道+模型** 熔断（`RecordModelFailure`），视为渠道整体故障。
- 成功：`RecordModelSuccess` / `RecordKeySuccess` 同时清零。
- 选道：`GetRandomSatisfiedChannel`（内存）与 `GetChannel`（DB）在加权随机前过滤掉已熔断渠道，
  被熔断渠道当作不存在，优先级自然下探到下一档——熔断只影响该模型，其他模型照常；
  `GetNextEnabledKeyForModel` 跳过已熔断的 key，key 的 A 模型限流不影响 B 模型。
- 恢复：`fixed` 按冷却分钟；`daily` 按每天 `HH:MM`；`IsModelOpen`/`IsKeyOpen` 惰性判断到点自动关闭。

### 4.3 key 级轮询/随机/故障转移
- 随机/轮询：复用已有 `MultiKeyMode`（`random`/`polling`），本次仅补齐「换 key」。
- key 429 → `GetNextEnabledKeyForModel` 跳过该模型下已熔断 key；重试循环设置
  `ContextKeyRetrySameChannelId`，让下一次重试仍选同一渠道从而换到其它 key。
- key 熔断按 key+模型粒度：key 的 A 模型限流不影响 B 模型（`IsKeyOpen` 按 `key|model` 隔离）。

### 4.4 渠道级 auto_ban
- 未改动 `service.DisableChannel` / `model.UpdateChannelStatus` 逻辑，仅在其外层新增模型/key 熔断。
- 多 key 渠道单个 key 失败时 auto_ban 仍只禁用该 key，全部 key 失败才禁用渠道——原样保留。

### 4.5 LOCAL_MODE
- `SecureVerificationRequired`：`common.LocalMode` 为真时 `c.Next()` 直通（跳过查看渠道密钥的 2FA/Passkey）。
- `CriticalRateLimit`：`common.LocalMode` 为真时返回 no-op（覆盖登录/OAuth/充值/查看 key 等关键限流）。

---

## 5. 交叉编译（linux/arm64，ImmortalWrt）

本项目 SQLite 走 `github.com/glebarez/sqlite`（纯 Go，基于 `modernc.org/sqlite`），
**默认无需 CGO**，直接静态交叉编译：

```bash
# 在开发机（x86_64，已装 Go 1.25+）执行
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o new-api-linux-arm64 .
```

可选：用 `go env -w GOOS=linux GOARCH=arm64` 或 Docker 的 `golang:1.25` 镜像构建。

若确需 CGO（例如自行替换了 CGO 版 SQLite 驱动），推荐 zig 作为交叉 C 工具链，避免手装
arm64 gcc：

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
  CC="zig cc -target aarch64-linux-musl" \
  go build -o new-api-linux-arm64 .
```

> 说明：本环境未安装 Go，代码未做本地编译验证。交付后请在装有 Go 的开发机上先
> `go build ./...` 与 `go test ./pkg/upstream_guard/...` 跑通再交叉编译。

---

## 6. 验证方法

### 6.1 单元测试
```bash
go test ./pkg/upstream_guard/... -v
```
覆盖：令牌桶放行/阻塞、模型熔断开合/重置、全局默认阈值、key 熔断按模型隔离、恢复时间计算。

### 6.2 mock 端到端测试场景清单

启动 mock（本地开发机）：
```bash
go run ./scripts/mock_llm_server -addr :8090 -limit-model agnes-2.5-flash -limit-success 3
```
把 new-api 某渠道 `BaseURL` 指向 `http://127.0.0.1:8090`、模型填 `agnes-2.5-flash`。

| # | 场景 | mock 配置 | 预期行为 |
| --- | --- | --- | --- |
| 1 | 限速排队不报错 | `-limit-model agnes-2.5-flash -limit-success 1000`（不限流），渠道 setting 配 `rate_limit.agnes-2.5-flash.rpm=6` | 并发 >6 请求时，超出的请求排队等待而非 429；观测响应时间被拉长 |
| 2 | 排队超时报错 | 同上，`max_wait_seconds=2`，压并发到远超 rpm | 部分请求在 ~2s 后返回「排队等待超时」明确错误 |
| 3 | 延迟后仍限流提示 | `-limit-model agnes-2.5-flash -limit-success 3 -retry-after`，且不配模型限速 | 前 3 次成功，第 4 次起返回 429，最终错误带「请到后台调小该渠道该模型的每分钟请求数」 |
| 4 | 模型级熔断粒度 | 两个模型 A、B 指向同一渠道，只对 A 配 `circuit_breaker.A.threshold=3`；mock 只限 A（`-limit-model A -limit-success 0`） | A 连续 3 次 429 后进入熔断、选道跳过它；B 照常走该渠道不受影响 |
| 5 | 熔断恢复计时（fixed） | A 配 `threshold=2, mode=fixed, cooldown_minutes=1` | 熔断后 1 分钟内 A 请求走其它渠道/报无渠道；1 分钟后自动恢复 |
| 6 | 熔断恢复计时（daily） | A 配 `threshold=2, mode=daily, recover_at=HH:MM`（设为 2 分钟后） | 熔断后到 `HH:MM` 才恢复（可把 HH:MM 设近一点观察） |
| 7 | key 429 换 key | 渠道多 key（2 个），`multi_key_mode=polling`，mock 只对其中一个 key 限流（可给两 key 不同 base 或看日志） | 单 key 429 后重试换到另一 key；连续失败达 `KEY_CIRCUIT_BREAKER_THRESHOLD` 后该 key 被跳过 |
| 8 | key 熔断按模型隔离 | 同上，但 key 对 A 模型限流、对 B 模型正常 | 同一 key：A 模型熔断被跳过，B 模型继续可用 |
| 9 | 5xx 熔断 | `-inject-500-every 1`，模型配 `threshold=3` | 连续 500 触发熔断，走其它渠道 |
| 10 | auto_ban 不受影响 | 保持渠道 `auto_ban=1`，上游返回 401（默认禁用码） | 渠道/key 仍按原 auto_ban 逻辑禁用，无回归 |

---

## 7. 部署替换步骤（ImmortalWrt 路由器）

1. 开发机编译：`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o new-api-linux-arm64 .`
2. 上传到路由器（scp 到 `/usr/bin/new-api` 或自定路径）。
3. 备份旧二进制：`mv /usr/bin/new-api /usr/bin/new-api.bak`
4. 替换：`install -m 0755 new-api-linux-arm64 /usr/bin/new-api`
5. 配置环境变量（`/etc/init.d/new-api` 或 systemd unit 的 Environment 里）：
   ```ini
   LOCAL_MODE=true
   CIRCUIT_BREAKER_DEFAULT_THRESHOLD=5
   CIRCUIT_BREAKER_DEFAULT_COOLDOWN_MINUTES=5
   KEY_CIRCUIT_BREAKER_THRESHOLD=3
   KEY_CIRCUIT_BREAKER_COOLDOWN_MINUTES=5
   MODEL_RATE_LIMIT_MAX_WAIT_SECONDS=60
   ```
6. 重启服务并验证 `GET /api/status` 正常。
7. 回滚：`mv /usr/bin/new-api.bak /usr/bin/new-api` 再重启。

---

## 8. 已知边界与后续扩展点

- 熔断/限速为进程内内存态，重启即清空；多实例需接 Redis（把 `sync.Map` 换成 Redis 键）。
- 熔断/限速为进程内内存态；修改 `rate_limit.rpm`/`burst` 后限速器自动重建、即时生效（无需重启）；熔断态重启清空。
- 未覆盖 task/mj 异步任务的限速等待与 key 换 key（选道熔断过滤仍生效）。
- 后台渠道「设置」目前为 JSON 文本框；如需图形化界面可后续加前端表单。

---

## 9. 部署配置清单（哪些开关配合才生效）

### 9.1 关键环境变量

| 变量 | 默认 | 作用 | 什么时候必须配 |
|---|---|---|---|
| `SESSION_SECRET` | 随机 | session 签名 | **必填**（不设启动即 fatal） |
| `LOCAL_MODE` | `false` | true 关闭「查看密钥」2FA 与登录/关键操作限流 | 内网 HTTP 自用建议 `true`；公网务必 `false` |
| `COOKIE_SECURE` | `true`（Secure cookie） | cookie 是否仅 HTTPS 发送 | **HTTP 部署必须 `false`**，否则登录成功但点菜单被弹回登录页（实测踩坑） |
| `MEMORY_CACHE_ENABLED` | `false` | true 选道走内存缓存（多 key 轮询建议开启） | 多 key 轮询/低延迟选道 |
| `CIRCUIT_BREAKER_DEFAULT_THRESHOLD` | `5` | 模型级熔断默认阈值（0=仅渠道显式配置才熔断） | 想全局默认熔断时 |
| `CIRCUIT_BREAKER_DEFAULT_COOLDOWN_MINUTES` | `5` | 熔断默认冷却分钟 | 同上 |
| `KEY_CIRCUIT_BREAKER_THRESHOLD` | `3` | key 级熔断默认阈值 | 多 key 渠道 429 轮换 |
| `MODEL_RATE_LIMIT_MAX_WAIT_SECONDS` | `60` | 限速排队上限 | 客户端超时较短时调小 |

### 9.2 开关依赖关系（重点，容易忘）

1. **限速排队**：渠道 setting 配 `rate_limit.<模型>.rpm` 即生效，**不需要任何开关**。每 key+模型独立。
2. **熔断**：`circuit_breaker.<模型>.threshold` 配了就生效；不配则用全局默认（`CIRCUIT_BREAKER_DEFAULT_THRESHOLD`，默认 5）。429 只熔 key（多 key 渠道）、5xx 熔渠道。
3. **429 换渠道 / 换 key 重试**：**前提是 `RetryTimes > 0`**（系统设置里的 RetryTimes，DB 配置，默认 0=不重试）。限速排队后上游仍 429，靠它换 key/换渠道兜底。**这是最容易漏的**——不配 RetryTimes，429 直接报给客户端。
4. **auto_ban（渠道自动禁用）**：渠道 `auto_ban=1` + 系统设置 `AutomaticDisableChannelEnabled`（默认关）+ `AutomaticDisableStatusCodes`（默认仅 401）。
5. **多 key 轮询**：渠道开启多 key + `multi_key_mode=polling`，每 key 独立限速/熔断；单实例无需 Redis。
6. **CPU/内存保护**：`performance_setting.monitor_enabled`（默认 true）超 `monitor_cpu_threshold`（默认 90）会拒绝 `/v1` 请求——**单机自用建议关闭**，否则本机负载稍高就 503 `system cpu overloaded`。

### 9.3 部署实测提醒

- 建/改渠道后**重启一次**再发请求（选道缓存刷新；或等 60s 自动同步），否则可能报「无可用渠道」。
- 错误提示默认中文（`i18n` 默认语言已改为 zh-CN）；无效 API Key 会提示「请求中的密钥与任何令牌都不匹配」。
- 令牌 key 明文存储，编辑令牌可自定义（留空不修改）。

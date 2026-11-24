# 渠道校验失败（400 / code 11128）自动重试 — 设计文档

- 日期：2026-11-24
- 影响文件：`workbuddy.go`、`main.go`、`reasoning_guard.go`、`cmd/fakeupstream/main.go`、`channel_retry_test.go`、`reasoning_guard_test.go`、`README.md`、`relay-retry-proxy.env.example`
- 版本基线：`d66112e`（合并 `03c8bd4` 空思考守卫后的 state），部署前备份见 `backup-relay-retry-proxy-20261124-2230/`

## 1. 需求与现象

Trae CN 通过 `relay-retry-proxy`（127.0.0.1:8443）请求 `deepseek-v4.1-flash`，
随机（按时间窗）收到：

```json
{"code":11128,"msg":"Illegal API invocation from an unapproved channel",
 "retry_policy":{"retry":false}}
```

客户端侧错误码 `4028`，提示「这次调用没通过渠道校验」。

## 2. 现场证据（抓包 + 回放）

抓包（`tcpdump -i any 'tcp port 8443 or tcp port 4000'`）得到一次完整失败请求：

| 方向 | 路径 | Body | Authorization 指纹 |
|---|---|---|---|
| Trae → 代理 :8443 | `/v1/responses` | 1,114,399 B | `ce093955d49e` |
| 代理 → New API :4000 | `/v1/responses` | 1,132,101 B | `ce093955d49e`（同一个 key） |

出站头已被正确替换为官方 WorkBuddy 桌面端身份：

```
User-Agent: WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0
Origin/Referer: https://www.workbuddy.ai/
X-Agent-Type: main / X-Agent-Intent: craft / X-Domain: www.workbuddy.ai
X-IDE-Type: WorkBuddy / X-IDE-Version: 5.9.4 / X-Product: SaaS / X-Private-Data: true
X-Conversation-* / X-Root-Request-ID / X-B3-* 全套重建
trae 的 X-Model-Provider / Acl-Token / X-Bridge-Transport / UA: hertz 已清除
```

结论：**头替换本身没有问题**，11128 不是头缺失导致的。

回放实验结果（同一 key、同一模型、同一上游）：

| 用例 | 结果 |
|---|---|
| 1.1 MB Trae 原始 body → 代理 | 400 / 11128（可复现） |
| 代理出站 body 直接打到 New API（带全套官方头） | 400 / 11128 |
| 60 KB 合成 body → 代理 | 200 |
| 2.4 MB 合成 body → 代理 | 200 |
| **同一最小 body 连打 20 次** | **20/20 全 200** |
| 错误窗内任意 body（含 9 连发、8 连发） | 全部 400 |

即 11128 与 body 大小、内容、头完全无关，而是**按时间窗出现的渠道侧拒绝**（上游
New API v1.0.0-rc.49 在该模型上存在多条上游渠道，其中 WorkBuddy 渠道在窗口内一律拒绝）。
窗口过后同一条请求 200。

## 2.1 真正的根因：提示词指纹（补记，22:37）

抓包比对后进一步确认，11128 与请求头无关，而是**首条 system 消息的客户端签名**：

| 首条 system 文本 | 结果 |
|---|---|
| `You are an interactive agent in TraeCode` | 400 / 11128 |
| `...in TraeCode that ...` | 400 / 11128 |
| `...in TraeCodx` | 200 |
| `AYou are an interactive agent in TraeCode` | 200（前缀被打断） |
| `...in CodeBuddy` | 200 |
| 直接删掉该 system 消息 | 200 |

同一个 1.1 MB body，仅把 `TraeCode` 改为中性名就 200；官方头在两种情况下完全一致。
因此方案里增加「提示词指纹清洗」，并把重试作为抖动兜底。

## 3. 方案

沿用代理已有的「重试 + 对冲」框架，把「渠道校验拒绝」纳入可重试错误：

1. `workbuddy.go`：新增 `wbChannelRejectCode(body)`，从上游错误体里识别
   `code=11128|4028` 或消息含 `unapproved channel` / `Illegal API invocation`；
   兼容「裸网关体」「外层再包一层 error.message」「双重转义字符串」三种形态。
2. `main.go`：在 5xx 重试分支之后、正常透传之前加入 400 判定分支：
   - 命中 → 计入 `wbChannelRejected`、按指数退避重试；
   - 用尽尝试 → 把上游原始 400 体原样透传，不伪造错误；
   - 未命中（普通 400，如 `input is required`）→ 把已读出的 body 放回，照常透传。
3. 开关：`WB_RETRY_CHANNEL`（env）/ `-wb-retry-channel`（flag），默认开启。
4. `/__health` 增加 `wbRetryChannel`、`wbChannelRejected`、`wbSanitize`。
5. `workbuddy.go`：新增 `wbSanitizeBody`，只改写 `instructions`、
   `input[].content`、`messages[].content` 里的指纹文本（`TraeCode` → `CodeBuddy`），
   工具名/参数/图片/用户内容不动；开关 `WB_SANITIZE`（默认开）。
6. 顺带修复 `wbResponsesToChat` 丢掉 `type=message` 普通消息的缺陷：真实客户端
   的 system 提示词原本会被整条丢弃（WB_API_MODE=auto 时）。

### 顺带修掉一个真实缺陷

`03c8bd4` 的空思考守卫存在**流重复**：`peekVerdict` 用 `buffered()` 返回缓存副本，
而缓存并未从 `peekReader` 取走，随后 `replayReader(cached, peek)` 又把同一批字节
通过 `peek` 再吐一遍 —— 客户端会收到重复的 SSE 块（同一段正文出现两次）。

修复：新增 `peekReader.takeBuffered()`，在放行时取走并清空缓存；`peekVerdict`
的三个返回点改用 `takeBuffered()`。

## 4. 异常路径

| 场景 | 行为 |
|---|---|
| 渠道拒绝后重试成功 | 客户端只看到一次正常 200 流 |
| 渠道持续拒绝 | 重试至 `MAX_ATTEMPTS`，透传上游 400 原文 + `X-Proxy-Attempt` |
| 普通参数错误 400 | 不重试，原样透传 |
| `WB_RETRY_CHANNEL=0` | 完全不进入该分支，行为与旧版一致 |
| 客户端断开 | 现有 `rootCtx` 取消逻辑，取消所有在途尝试 |

## 5. 测试用例

单元：

- `TestWBChannelRejectCode`：裸体 / 4028 / 包一层 / 双重转义 / 普通 400 / 空体 / 非 JSON
- `TestWBChannelRejectNotConfusedWithUsageError`
- `TestPeekReplayNoDuplicate`：重放流必须与原始流逐字节一致且正文只出现一次

集成（`httptest` 假上游，走真实 `proxy()`）：

- `TestProxyRetriesChannelReject`：2 次 11128 + 第 3 次 200 → 客户端 200，上游被调 3 次
- `TestProxyChannelRejectGivesUp`：一直 11128 + `maxAttempts=3` → 透传 400，调用 3 次
- `TestProxyChannelRejectRetryDisabled`：开关关闭 → 只调 1 次
- `TestProxyPassesThroughNormal400`：普通 400 → 只调 1 次
- `TestProxyStreamNotDuplicated`：端到端确认 SSE 块不重复

手工：`FAKE_REJECT_N=2` 的 `fakeupstream` + 独立端口代理，验证日志与调用次数。

## 6. 验收标准

1. `go vet ./...`、`go test ./...` 全绿。
2. 假上游连拒两次后成功时，客户端拿到完整 200 流。
3. 渠道持续拒绝时按尝试次数收敛。
4. 普通 400 不重试。
5. 生产 8443 端口重启后 `/__health` 带新字段，真实 Trae 请求 1.1 MB body 恢复正常。

## 7. 回滚

```bash
systemctl stop relay-retry-proxy
install -m0755 /root/backup-relay-retry-proxy-20261124-2230/relay-retry-proxy.bin /usr/local/bin/relay-retry-proxy
cp /root/backup-relay-retry-proxy-20261124-2230/env /etc/relay-retry-proxy/env
systemctl start relay-retry-proxy
```

源码回滚：`git reset --hard d66112e`（或 `git checkout <上一个 commit>`）。

# relay-retry-proxy

给 Trae CN（以及任何 OpenAI 兼容客户端）用的**重试代理**。转发到中转站，
在上游抖动时自动重试，流式响应不缓冲。

## 它解决什么

Trae 报的两类错：

| 报错 | 含义 | 本代理能否解决 |
|---|---|---|
| `Connection timeout (HTTP Status: 500) (4028)` | 上游 500 / 连接超时 | ✅ 自动重试，客户端只看到最终成功的那次 |
| `模型请求失败，请稍后重试 (4054)` | Trae 侧模型网关把上游失败包装后的提示 | ✅ 若根因是上游抖动，重试后不再出现 |

重试触发条件：

- 传输层错误（dial 失败、TLS 失败、`timeout awaiting response headers`、连接重置）
- 上游 `429`、`408`、`5xx`
- 间隔：`base-delay × 2^(n-1)`，±25% 抖动，上限 `max-delay`

不重试的情况：上游返回 4xx（除 429/408）——参数错误重试也没用；以及已经开始
向客户端吐数据之后的失败（避免重复内容）。


## 一键安装（其他 Linux 机器）

```bash
tar -xzf relay-retry-proxy-1.0.0-linux.tar.gz
cd relay-retry-proxy-1.0.0-linux 2>/dev/null || true
sudo ./install.sh --upstream http://your-relay:4000
```

安装脚本做的事：按架构自动选二进制（amd64/arm64）→ 装到 `/usr/local/bin`
→ 写 `/etc/relay-retry-proxy/env` → 写 systemd 单元并 `enable --now`
→ 配 logrotate → 等 2 秒做一次 `/__health` 健康检查。

常用参数：

```bash
# 给局域网/别的机器用（默认只监听 127.0.0.1）
sudo ./install.sh -u http://10.0.0.5:4000 -l 0.0.0.0:8443

# 重试更凶
sudo ./install.sh -u http://10.0.0.5:4000 -a 8 --base-delay 1s --max-delay 20s

# 多上游故障切换（逗号分隔，按尝试轮转）
sudo ./install.sh -u http://a:4000,http://b:4000

# 先预演，不动系统
./install.sh --dry-run -u http://10.0.0.5:4000

# 卸载（保留配置和日志）
sudo ./install.sh --uninstall
```

完整参数：`./install.sh -h`。已有配置默认**不覆盖**，要覆盖加 `--force`。

安装后：

```bash
systemctl status relay-retry-proxy
journalctl -u relay-retry-proxy -n 50 --no-pager
curl -s http://127.0.0.1:8443/__health
```

## 部署形态

```
Trae / 任意客户端 → 127.0.0.1:8443 (本代理) → 中转站 http://your-relay:4000
```

本机已部署为 systemd 服务：

```bash
systemctl status  relay-retry-proxy
systemctl restart relay-retry-proxy
curl -s http://127.0.0.1:8443/__health
```

配置：`/etc/relay-retry-proxy/env`（改完 `systemctl restart relay-retry-proxy`）
日志：`/var/log/relay-retry-proxy/proxy.log`

## Trae 里怎么接

打开 Trae 的模型服务商设置，选择 **自定义 / OpenAI 兼容** 类型，把 Base URL 改成：

- **请求在远程服务器侧发出** → `http://127.0.0.1:8443/v1`
- **请求在 Windows 客户端侧发出** → 见下面两种做法：
  1. 把 `relay-retry-proxy-windows-amd64.exe` 拷到 Windows 跑：
     ```powershell
     .\relay-retry-proxy-windows-amd64.exe -listen 127.0.0.1:8443 `
       -upstream http://your-relay:4000 -max-attempts 4 -head-timeout 180s
     ```
     Base URL 填 `http://127.0.0.1:8443/v1`
  2. 或者走 SSH 隧道（把服务器上的代理映射到 Windows 本地）：
     ```powershell
     ssh -N -L 8443:127.0.0.1:8443 root@你的服务器
     ```
     Base URL 同样填 `http://127.0.0.1:8443/v1`

API Key 照原样填中转站的 key —— 代理逐字节透传 `Authorization` 头，不解析、不落盘。


## 对冲重试（hedge）：应对"客户端等不起"

单纯"失败后重试"救不了这种情况：上游卡住 90 秒不返回响应头，而 Trae 自己的
超时只有 ~90 秒，等第一次尝试超时再重试，客户端早就断开了（日志里表现为
`status=200 bytes=0 copyErr=write: broken pipe`）。

所以代理默认开启对冲：

- 第一次请求发出后，若 `HEDGE_AFTER`（默认 10s）内仍未拿到响应头
- 立即并行再发一次（轮转到下一个上游）
- 谁先返回有效响应就用谁，另一条取消
- 客户端断开时（Trae 关掉窗口/超时放弃），立刻中断所有在途尝试，不白烧请求

实测：第一条上游卡死、第二条正常时，客户端 3.9 秒拿到 200（未对冲前要等 90 秒超时）。

配置项：

```ini
HEDGE_AFTER=10s    # 0 = 关闭对冲，退回串行重试
HEAD_TIMEOUT=45s   # 单次尝试等响应头的上限
MAX_ATTEMPTS=6     # 含对冲与重试的总尝试数
```

注意：对冲会让上游收到双份请求（仅在前 10 秒没响应时）。如果你的中转站按请求
计费且首字延迟常年 >10s，把 `HEDGE_AFTER` 调大或设 0。


## WebUI 看板

代理自带一个只读看板（Vue 3 + 原生打包，**单个二进制内嵌，无 Node/Python 依赖**）：

```
http://<本机>:9090/ui
```

内容：总请求 / 成功 / 失败 / 重试次数 / 上游 5xx / 成功率、上游列表、当前参数、
最近 600 条请求明细（时间、request id、路径、第几次尝试、状态码、字节、耗时、备注），
SSE 实时推送 + 5 秒兜底刷新，可按路径/状态/ID 过滤，可只看重试与失败。

参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `-admin-listen` | `127.0.0.1:9090` | 看板监听地址，逗号分隔多个；`0` = 不启用 |
| `ADMIN_LISTEN` | 同上 | 配置文件里的等价项 |

要点：**看板监听与代理监听是两个独立端口**。看板端口只提供 UI 与只读 API
（`/api/health`、`/api/stats`、`/api/config`、`/api/requests`、`/api/requests/stream`），
**不转发模型请求**，所以把它开到内网也不会变成开放代理。

想让远程的 Windows 直接打开（零信任网段内）：

```ini
ADMIN_LISTEN=127.0.0.1:9090,YOUR-SERVER-IP:9090
```

然后 Windows 浏览器访问 `http://YOUR-SERVER-IP:9090/ui`。


## 断流排查：dump 落盘

怀疑"回答做一半停了"时，打开 dump 就能分清是**上游提前结束**还是**客户端/代理截断**：

```ini
DUMP_DIR=/var/log/relay-retry-proxy/dumps
DUMP_MAX_KB=2048     # 单个文件上限
DUMP_KEEP=200        # 每个方向保留多少个文件（自动轮转）
DUMP_BODY=0          # 1=请求体也落盘（默认 0，避免把 prompt 写进磁盘）
```

或者命令行：`-dump-dir /path -dump-max-kb 2048 -dump-keep 200 -dump-body=false`

每次请求生成两个文件（`0600`，目录 `0700`）：

```
20261123-231540.296-O9cTJd0JmZ-req.txt    请求行、脱敏后的头、body（默认省略）
20261123-231545.108-O9cTJd0JmZ-resp.txt   响应原文尾部（含 SSE 分片），到上限截断
```

分辨方法：

| 现象 | 结论 |
|---|---|
| `resp.txt` 里能看到 `finish_reason` / `[DONE]` / 完整结尾 | 上游正常结束，停是客户端/agent 行为 |
| `resp.txt` 中途被切断，且日志有 `client gone` | 客户端断开（Trae 自己放弃） |
| `resp.txt` 为空且日志有 `gave up` | 上游确实失败，重试也救不回 |

脱敏规则：任何名字里含 `authorization` / `token` / `auth` / `key` / `cookie` / `secret` /
`session` / `credential` 的请求头一律打成 `***REDACTED***`；`X-Forwarded-For`、`X-Real-IP`
也打码；请求体默认不落盘。


## Token 统计与持久化

看板顶部新增四张卡片：今日输入 / 今日输出 / 今日均速(tok/s) / 累计 tokens；
下方"用量统计"面板给出**按模型**的请求数、输入、输出、tok/s，以及**近 24 小时**输出柱状图；
请求明细表新增 `模型 / 输入 / 输出 / tok/s` 四列。

数据来源（与 new-api 之类的中转站口径一致）：

1. **优先取上游返回的 usage**：兼容
   - `chat/completions`：`usage.prompt_tokens` / `completion_tokens` / `total_tokens`
   - `responses`：`usage.input_tokens` / `output_tokens` / `total_tokens`
   代理在流式响应里扫最后一个 `usage` 对象（保留尾部 256 KB），不做缓冲、不改写响应。
2. **上游没给 usage 时按字节估算**：约 3 字节/token，记录里标 `est:true`，看板显示"其中 N 条为估算"。
3. **4xx/5xx 不计 token**：错误响应体不是模型输出。

**速度指标（两套口径）**

| 指标 | 算法 | 用途 |
|---|---|---|
| `tok/s`（总） | output_tokens ÷ 整个请求耗时 | 端到端体验 |
| `gen_tps`（生成） | output_tokens ÷ (耗时 − TTFT) | 模型真实吐字速度 |
| `TTFT` | 从发起请求到**首个响应字节**到达的毫秒数 | 反映 prefill 有多慢 |
| 实时 tok/s | 在途请求每 700ms 推一次：已收估算 tokens ÷ 已用时间 | 看板顶部实时条 |

`cached_tokens`（prompt 缓存命中）与 `reasoning_tokens`（思考 token）也会从
`prompt_tokens_details` / `completion_tokens_details` 里解析并落盘。

持久化：默认写在日志目录下的 `usage/usage-YYYY-MM-DD.jsonl`（每天一个文件，`0600`）：

```json
{"ts":1795447841,"id":"7a002642","path":"/v1/chat/completions","model":"deepseek-v4.1-flash",
 "status":200,"in":3416309,"out":3204,"total":3419513,"dur_ms":90000,"tps":35.6}
```

启动时扫描最近 30 天文件重建聚合，**重启不丢数据**（实测：重启后 today/models 与重启前一致）。
配置项：`USAGE_DIR` / `-usage-dir`（默认 `<日志目录>/usage`）。

## 配置文件

可执行文件同目录放一个 `relay-retry-proxy.env`，启动时自动读取（不用带任何参数）：

```ini
LISTEN=127.0.0.1:8443
UPSTREAM=http://your-relay:4000
MAX_ATTEMPTS=5
BASE_DELAY=800ms
MAX_DELAY=15s
HEAD_TIMEOUT=90s
DIAL_TIMEOUT=10s
```

优先级：**命令行参数 > 配置文件 > 环境变量 > 内置默认值**。
内置默认上游是 `http://127.0.0.1:4000`（本机占位值），部署时请用 `--upstream` 指向真实中转站。

Windows 打包版：`relay-retry-proxy-windows.zip`
（内含 `relay-retry-proxy-windows-amd64.exe` + `relay-retry-proxy.env` + `start.bat`，解压后双击 `start.bat`）。

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-listen` | `127.0.0.1:8443` | 监听地址；Windows 想被局域网访问可写 `0.0.0.0:8443` |
| `-upstream` | `http://your-relay:4000` | 上游；逗号分隔可配多个，按尝试轮转做故障切换 |
| `-max-attempts` | `4` | 含首次的总尝试次数 |
| `-base-delay` | `700ms` | 首次重试间隔 |
| `-max-delay` | `12s` | 间隔上限 |
| `-head-timeout` | `90s` | 等上游响应头的最长时间；模型思考久就调大 |
| `-dial-timeout` | `10s` | TCP 建连超时；ZeroTier 链路抖动时尽早失败转入重试 |
| `-hedge-after` | `10s` | 超时未拿到响应头就并行再发一次；`0` 关闭对冲 |
| `-admin-listen` | `127.0.0.1:9090` | WebUI 监听地址（只读，不含代理）；`0` 关闭 |
| `-log` | 空 | 日志文件，空则 stdout |
| `-max-body-mb` | `64` | 请求体上限 |

## 自测

`test/stub_upstream.py` 是一个会先回 500、再故意拖到超时、第三次才正常流式输出的假上游：

```bash
python3 test/stub_upstream.py 19001 &
./relay-retry-proxy-linux-amd64 -listen 127.0.0.1:18443 -upstream http://127.0.0.1:19001 \
  -max-attempts 4 -base-delay 300ms -head-timeout 2s -log /tmp/proxy-test.log
curl -N -H 'Content-Type: application/json' -d '{"stream":true,"messages":[]}' \
  http://127.0.0.1:18443/v1/chat/completions
```

预期：客户端拿到 `X-Proxy-Attempt: 3` 的 200 流，stub 记到 3 次请求，
日志里能看到 500 → header timeout → ok 三段。

## 为什么不是 Trae 扩展

VS Code 扩展 API 里没有"拦截模型请求"的钩子：`vscode.lm` 只能发起请求，不能
劫持 Trae 自己的模型调用；而 Trae 的模型请求由 `harness-server`（原生二进制，
独立进程）发出，扩展宿主进程碰不到。任何"Trae++"式扩展都只能改 UI/命令，
改不到这一层——所以重试只能在网络层做。这也是本方案用 HTTP 代理而不是扩展的原因。

## 构建

```bash
export PATH=/usr/local/go1.27/bin:$PATH
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o relay-retry-proxy-linux-amd64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o relay-retry-proxy-windows-amd64.exe .
```

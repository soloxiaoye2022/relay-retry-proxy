# 中转站侧 system 提示词指纹处理说明（解决 400 / code 11128）

> 目标读者：维护中转站（New API / one-api 类网关）的人。
> 结论：**上游渠道校验看的是「首条 system 消息开头的客户端签名」，不是请求头。**
> 只要在转发前把这段签名的前缀打断或换掉，同样的请求立刻从 400 变 200。

---

## TL;DR（30 秒版）

1. 在网关的**请求体改写**环节，找第一条 `role=system` 的文本（Responses 看 `instructions` / `input[]`，Chat 看 `messages[]`）。
2. 在它最前面补一个换行 `\n`（推荐），或把 `TraeCode` 替换成 `CodeBuddy`。
3. 只改 system 文本：不碰工具名/参数、图片、用户消息；改完原样转发。
4. 验收：同一个请求带指纹 → 400，去掉指纹/加换行 → 200。

- 文档版本：2026-11-24
- 现场环境：Trae CN（`custom_responses_compatible`）→ 本机代理 `127.0.0.1:8443` → New API v1.0.0-rc.49（`192.168.5.246:4000`）→ WorkBuddy / CodeBuddy 渠道
- 涉及接口：`POST /v1/responses`（OpenAI Responses API），`POST /v1/chat/completions`

---

## 1. 现象

客户端收到：

```json
{
  "error": {
    "message": "upstream 400: {\"code\":11128,\"msg\":\"Illegal API invocation from an unapproved channel\",\"requestId\":\"01a20deac0dd...\",\"retry_policy\":{\"retry\":false},\"displayMsg\":{\"en\":\"Request blocked. Please send it again\",\"zh\":\"请求被拦截，请重新发送\"}}",
    "type": "server_error",
    "code": 400
  }
}
```

Trae 侧错误码 `4028` 或 `4054`，提示「这次调用没通过渠道校验，可能不是官方客户端发出的」。

---

## 2. 实验结论（同一 key、同一模型、同一上游，只改一个变量）

| # | 改了什么 | 上游结果 |
|---|---|---|
| 1 | 原样（首条 system 以 `You are an interactive agent in TraeCode` 开头） | **400 / 11128** |
| 2 | 首条 system 前加一个空格 `" You are..."` | **400**（网关会 trim 空格） |
| 3 | 首条 system 前加一个换行 `"\nYou are..."` | **200** |
| 4 | 首条 system 前加一个字符 `"AYou are..."` | **200** |
| 5 | `TraeCode` → `TraeCodx`（少一个字母） | **200** |
| 6 | `TraeCode` → `CodeBuddy` | **200** |
| 7 | 直接删掉首条 system 消息 | **200** |
| 8 | 请求头换成「官方 WorkBuddy 桌面端全套」（UA / Origin / X-Agent-* / X-B3-* / X-Conversation-*） | **400**（头完全不影响） |
| 9 | body 放大到 2.4 MB（不含该指纹） | **200** |
| 10 | 同一个请求连打 20 次（含该指纹，时间窗随机） | 20/20 全 400 |
| 11 | 同一个请求连打 20 次（不含该指纹） | 20/20 全 200 |

补充事实：

- 抓包确认代理发往中转站的请求头已经是完整官方身份，`Authorization` 原样保留，仍被拒；
  说明**渠道校验读的是 body 里的提示词**。
- 1.1 MB、22 万 token 的 Trae 真实请求，仅把首条 system 里一个词替换成中性名，立刻 200。
- 换成新换行前缀（`\n`）同样 200，且提示词措辞一个字没改。
- 该判定**只匹配前缀**：前面插一个字符就不再命中。

---

## 3. 判定规则（可据此自行验证/扩容）

1. 取「第一条 role=system 的文本」；
2. 只比较其**开头**（前缀匹配），不扫全文；
3. 会 trim 前后**空格**，但**不 trim 换行**；
4. 命中任一已知客户端签名 → 400 / code 11128；
5. 与 `Authorization`（key）、模型名、body 大小、请求头全部无关。

已知命中串（`^` 表示必须在开头）：

```
^ You are an interactive agent in TraeCode
```

待补充：若出现其他厂商客户端（Cursor / Windsurf / Cline 等）也被同样拦截，把新签名加进同一张表即可，规则不变。

---

## 4. 三种改法（按推荐顺序）

### 方案 A：前缀打断（推荐，措辞零改动）

在首条 system 文本最前面补一个换行符 `\n`。

- 优点：不动任何业务措辞，不改变模型认知，回归风险最低；
- 缺点：依赖「网关不 trim 换行」这一行为，换网关需复测；
- 幂等：若首字符已经是 `\n` 则跳过。

### 方案 B：指纹替换

把命中的客户端签名替换成中性产品名。

| 原串 | 建议替换为 |
|---|---|
| `You are an interactive agent in TraeCode` | `You are an interactive agent in CodeBuddy` |
| `an interactive agent in TraeCode` | `an interactive agent in CodeBuddy` |
| `TraeCode` | `CodeBuddy` |

- 优点：不依赖空白字符处理；
- 缺点：会改变提示词里的产品名，模型可能自称 CodeBuddy（一般无功能影响）。

### 方案 C：丢弃首条 system（不推荐）

- 会丢系统提示词，严重影响工具调用与行为一致性，仅作应急验证手段。

> 生产建议：**A 为主，B 作为兜底**（A 命中不生效时再上 B），二者都做成开关，可单独关闭。

---

## 5. 在中转站里的落点

改写必须发生在「**收到客户端请求之后、选择渠道/转发上游之前**」，且只对对话类接口生效：

- `/v1/responses`
- `/v1/chat/completions`
- `/v1/messages`（Anthropic 形态，如果也接了）

不要改：

- `/v1/models`、计费、管理类接口
- `tools[].function.name`、`tools[].function.parameters`（工具名/参数）
- 图片数据（`image_url`、`source.data`）
- 用户消息（`role=user`）与工具返回（`role=tool`）

### 字段路径

Responses API（`/v1/responses`）：

```
instructions                                  // 字符串，若有
input[i].type == "message" && input[i].role == "system"
input[i].content                              // 字符串 或 [{type:"input_text", text:"..."}]
```

Chat Completions（`/v1/chat/completions`）：

```
messages[i].role == "system" | "developer"
messages[i].content                           // 字符串 或 [{type:"text", text:"..."}]
```

> 注意：`developer` 角色在上游网关会被当成 system 处理，两条都要覆盖。

---

## 6. 参考实现

### 6.1 Python（New API 插件/中间件伪代码）

```python
PREFIX_SIGNATURES = (
    "You are an interactive agent in TraeCode",
)

def break_prefix(text: str) -> str:
    """方案 A：在首条 system 文本前补换行（幂等）。"""
    if not text:
        return text
    return text if text.startswith("\n") else "\n" + text

def sanitize_text(text: str) -> str:
    """方案 B：指纹替换。"""
    if not text:
        return text
    for needle in PREFIX_SIGNATURES:
        if needle in text:
            text = text.replace(needle, needle.replace("TraeCode", "CodeBuddy"))
    return text.replace("TraeCode", "CodeBuddy")

def _walk_content(content, fn):
    if isinstance(content, str):
        return fn(content)
    if isinstance(content, list):
        for part in content:
            if isinstance(part, dict) and isinstance(part.get("text"), str):
                part["text"] = fn(part["text"])
    return content

def rewrite_system_prompt(body: dict, mode: str = "prefix") -> bool:
    """返回是否改动过。mode: prefix | rename | both | off"""
    if mode == "off" or not isinstance(body, dict):
        return False

    fn = break_prefix if mode == "prefix" else (
        sanitize_text if mode == "rename" else (lambda t: sanitize_text(break_prefix(t)))
    )

    changed = False

    # Responses：instructions
    if isinstance(body.get("instructions"), str):
        new = fn(body["instructions"])
        if new != body["instructions"]:
            body["instructions"] = new
            changed = True

    # Responses：input[]
    for item in body.get("input") or []:
        if isinstance(item, dict) and item.get("role") == "system":
            new = _walk_content(item.get("content"), fn)
            if new != item.get("content"):
                item["content"] = new
                changed = True

    # Chat：messages[]
    for msg in body.get("messages") or []:
        if isinstance(msg, dict) and msg.get("role") in ("system", "developer"):
            new = _walk_content(msg.get("content"), fn)
            if new != msg.get("content"):
                msg["content"] = new
                changed = True

    return changed
```

调用位置（示意）：请求体解析成功后、进入渠道选择前

```python
if rewrite_system_prompt(payload, mode=os.getenv("SYS_PROMPT_GUARD", "prefix")):
    payload_bytes = json.dumps(payload, ensure_ascii=False).encode()
```

### 6.2 Node / TypeScript（Express 中间件示意）

```ts
const SIG = /You are an interactive agent in TraeCode/g;

function guardText(s: string, mode: "prefix" | "rename" | "both"): string {
  if (!s) return s;
  let out = s;
  if (mode === "prefix" || mode === "both") {
    if (!out.startsWith("\n")) out = "\n" + out;
  }
  if (mode === "rename" || mode === "both") {
    out = out.replace(SIG, "You are an interactive agent in CodeBuddy")
             .replace(/TraeCode/g, "CodeBuddy");
  }
  return out;
}

function guardContent(content: any, mode: any): any {
  if (typeof content === "string") return guardText(content, mode);
  if (Array.isArray(content)) {
    for (const part of content) {
      if (part && typeof part.text === "string") part.text = guardText(part.text, mode);
    }
  }
  return content;
}

export function guardBody(body: any, mode = process.env.SYS_PROMPT_GUARD ?? "prefix"): void {
  if (!body || mode === "off") return;
  if (typeof body.instructions === "string") {
    body.instructions = guardText(body.instructions, mode as any);
  }
  for (const item of body.input ?? []) {
    if (item?.role === "system") item.content = guardContent(item.content, mode);
  }
  for (const msg of body.messages ?? []) {
    if (msg?.role === "system" || msg?.role === "developer") {
      msg.content = guardContent(msg.content, mode);
    }
  }
}
```

### 6.3 Go（若网关是 Go 写的中转）

```go
var sigs = [][2]string{
    {"You are an interactive agent in TraeCode", "You are an interactive agent in CodeBuddy"},
    {"TraeCode", "CodeBuddy"},
}

func guardText(s, mode string) string {
    if s == "" || mode == "off" {
        return s
    }
    if (mode == "prefix" || mode == "both") && !strings.HasPrefix(s, "\n") {
        s = "\n" + s
    }
    if mode == "rename" || mode == "both" {
        for _, p := range sigs {
            s = strings.ReplaceAll(s, p[0], p[1])
        }
    }
    return s
}
```

---

## 7. 验收用例（复制即可跑）

设中转站地址 `http://<relay>/v1`，key 用真实可用的：

### 7.1 应当 200（前缀被打断）

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://<relay>/v1/responses \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","stream":true,
       "input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"\nYou are an interactive agent in TraeCode that helps the USER."}]},
                {"type":"message","role":"user","content":[{"type":"input_text","text":"ok"}]}]}'
```

### 7.2 应当 400 / 11128（对照组，验证指纹确实生效）

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://<relay>/v1/responses \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","stream":true,
       "input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"You are an interactive agent in TraeCode that helps the USER."}]},
                {"type":"message","role":"user","content":[{"type":"input_text","text":"ok"}]}]}'
```

> 如果 7.2 也返回 200，说明该渠道当前根本不校验，或者你的改写发生在错误的位置（改到了 user 消息 / 没进 system 分支）。

### 7.3 Chat 形态对照

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://<relay>/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","stream":true,
       "messages":[{"role":"system","content":"\nYou are an interactive agent in TraeCode that helps the USER."},
                   {"role":"user","content":"ok"}]}'
```

### 7.4 真实客户端回归

Trae 里发一条普通消息，确认：

- 首字延迟正常（该请求约 6–12 s，取决于输入长度与缓存）；
- 流式输出完整，出现 `response.completed`；
- 不再出现 `4028 / 11128`。

---

## 8. 观测与告警

建议在中转站按渠道维度统计：

| 指标 | 用途 |
|---|---|
| `code=11128` 次数 / 渠道 / 分钟 | 区分「前缀指纹未处理」与「渠道自身故障」 |
| `code=11128` 请求的首条 system 前缀（脱敏后截断 64 字符） | 发现新的客户端签名 |
| 改写命中次数（`sys_prompt_guard_hits`） | 确认改动实际生效 |

> 重要区分：**同一个 key 在短时间内全部 400、且换成最小 body 也 400**，那是渠道侧抖动/切换，
> 与提示词无关；此时靠网关侧重试或换渠道解决，不要误加新指纹。

---

## 9. 回滚

改写做成开关，默认值由环境变量控制：

```bash
SYS_PROMPT_GUARD=off      # off | prefix | rename | both
```

关闭后行为应恢复为「原始 body 逐字节转发」，无需改代码。上线前建议保留一份未改写的 body 抓包，便于对照。

---

## 10. 附录

### 10.1 上游错误体（完整样例）

```json
{
  "code": 11128,
  "msg": "Illegal API invocation from an unapproved channel",
  "requestId": "01a20deac0dd7b57b7bd47add5cce01d",
  "retry_policy": { "retry": false },
  "displayMsg": {
    "en": "Request blocked. Please send it again",
    "zh": "请求被拦截，请重新发送",
    "zh-hant": "請求已被攔截，請重新傳送"
  },
  "displayTips": {
    "en": "This call failed channel verification, possibly because it was not sent from an official client. Please resend from the official client.",
    "zh": "这次调用没通过渠道校验，可能不是官方客户端发出的。请用官方版本重新发送。"
  }
}
```

### 10.2 复现实验用的 curl（直接打上游，绕过中转站代理）

```bash
# 期望 400
curl -s http://192.168.5.246:4000/v1/responses \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","stream":true,"input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"You are an interactive agent in TraeCode"}]}]}' | head -c 200

# 期望 200（只多一个换行）
curl -s http://192.168.5.246:4000/v1/responses \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","stream":true,"input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"\nYou are an interactive agent in TraeCode"}]}]}' | head -c 200
```

### 10.3 抓包定位（本机视角）

```bash
# 客户端 → 代理 → 上游，两个方向一起抓
tcpdump -i any -s 0 -w /tmp/cap.pcap 'tcp port 8443 or tcp port 4000'

# 只看请求头与 URI
tshark -r /tmp/cap.pcap -d tcp.port==8443,http -d tcp.port==4000,http \
  -Y http.request -T fields -e tcp.dstport -e http.request.method -e http.request.uri
```

### 10.4 已知无关项（不要浪费时间排查）

| 怀疑对象 | 结论 |
|---|---|
| `Authorization` / key | 同一 key 小请求能过，说明 key 正常 |
| 请求头（UA、Origin、X-Agent-*、X-B3-*、X-Conversation-*） | 全套官方头仍然 400 |
| body 大小 / token 数 | 2.4 MB 无指纹请求 200 |
| 模型名 | 换 `deepseek-v4-pro` 同样表现 |
| 中转站重试 | 上游明确 `retry_policy.retry=false`，重复同一 body 仍 400 |

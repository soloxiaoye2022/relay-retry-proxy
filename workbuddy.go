// workbuddy.go：把 OpenAI 兼容的入站请求改写成 WorkBuddy / CodeBuddy 上游请求。
//
// 上游渠道会按「客户端身份」做校验（400 / code 11128 / Illegal API invocation
// from an unapproved channel）。中转站在前面重打包请求时往往丢掉了官方桌面端
// 的那套身份头，于是被网关判成非官方客户端。这一步做的就是把身份头按官方
// 抓包对齐重新拼回去，同时保留中转站自己的鉴权头（Authorization / API Key）。
//
// 设计要点：
//   - 只在 WB_MODE 打开时生效，默认关闭，老行为不变。
//   - 出站头会先清掉暴露中转站/客户端身份的残留头，再按官方顺序重建。
//   - 不吞掉调用方的 Authorization：它是中转站/账号的 key，必须原样带上去。
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// workbuddyConfig 是模式开关与身份参数，全部来自配置文件 / 命令行。
type workbuddyConfig struct {
	Mode          string // off | desktop | vscode | cli
	Realm         string // cn | intl
	UpstreamUA    string // 覆盖默认 UA（空=按身份/realm 现算）
	IdeVersion    string // 覆盖 X-IDE-Version
	Domain        string // 覆盖 X-Domain
	ConverPath    bool   // 是否把 /v1/chat/completions 改写到 /v2/chat/completions
	PrivateData   string // true | false | 空(auto)
	APIMode       string // auto | chat | responses
	RetryChannel  bool   // 上游以「渠道校验」拒绝（400 + code 11128）时是否重试
	Sanitize      bool   // 是否处理提示词里的客户端指纹（网关按首条 system 前缀判定来源）
	SanitizeStyle string // prefix=只在首条 system 前加换行打断指纹（默认，不改措辞）| rename=改名 | both
}

// wbChannelRejectCodes 是上游网关「渠道校验失败」的错误码。
// 11128 是 WorkBuddy 的原始码；4028 是客户端把它转成 SSE 错误时用的马甲。
var wbChannelRejectCodes = map[int]bool{11128: true, 4028: true}

// wbChannelRejectCode 从上游错误体里识别「渠道校验失败」。
// 命中返回错误码与原始消息，未命中返回 0。
//
// 错误体有两种形态：
//   - 裸的网关体：{"code":11128,"msg":"Illegal API invocation ..."}
//   - 被别的层包过一层：{"error":{"message":"upstream 400: {\"code\":11128...}}"}}
//
// 所以先按 JSON 找 code，再用特征串兜底。
func wbChannelRejectCode(body []byte) (int, string) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return 0, ""
	}

	var probe struct {
		Code  *json.Number `json:"code"`
		Msg   string       `json:"msg"`
		Error struct {
			Code    *json.Number `json:"code"`
			Message string       `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &probe); err == nil {
		for _, c := range []*json.Number{probe.Code, probe.Error.Code} {
			if c == nil {
				continue
			}
			if n, err := c.Int64(); err == nil && wbChannelRejectCodes[int(n)] {
				msg := probe.Msg
				if msg == "" {
					msg = probe.Error.Message
				}
				return int(n), msg
			}
		}
	}

	if strings.Contains(trimmed, "unapproved channel") ||
		strings.Contains(trimmed, "Illegal API invocation") {
		for code := range wbChannelRejectCodes {
			if strings.Contains(trimmed, strconv.Itoa(code)) {
				return code, "channel verification failed"
			}
		}
		return 4028, "channel verification failed"
	}
	return 0, ""
}

// 官方客户端版本（来自 CN 5.8.3 / INTL 5.9.4 实跑与解包）
const (
	wbDesktopVersionCN   = "5.8.3"
	wbDesktopVersionIntl = "5.9.4"
	wbDesktopCLICN       = "2.161.0"
	wbDesktopCLIIntl     = "2.165.0"
	wbVSCODEPlatform     = "Visual Studio Code"
	wbVSCODEIdeVersion   = "4.12.39997873"
	wbCLIVersion         = "2.168.2"
)

// wbEndpoint 是某身份在某区域的固定端点与 X-Domain
type wbEndpoint struct {
	Base   string
	Domain string
}

func wbEndpointFor(realm, product string) wbEndpoint {
	if realm == "cn" {
		if product == "vscode" {
			return wbEndpoint{"https://www.workbuddy.cn", "www.workbuddy.cn"}
		}
		return wbEndpoint{"https://copilot.tencent.com", "copilot.tencent.com"}
	}
	return wbEndpoint{"https://www.workbuddy.ai", "www.workbuddy.ai"}
}

// wbDefaultUA 按身份 + 区域算默认 UA，公式与官方 buildUserAgent 一致。
func wbDefaultUA(realm, product string) string {
	switch product {
	case "cli":
		return fmt.Sprintf("CLI/%s CodeBuddy/%s", wbCLIVersion, wbCLIVersion)
	case "vscode":
		return fmt.Sprintf("%s/%s %s/%s", wbVSCODEPlatform, wbVSCODEIdeVersion,
			wbVSCODEPlatform, wbVSCODEIdeVersion)
	}
	if realm == "cn" {
		return fmt.Sprintf("WorkBuddy/%s WorkBuddy/%s CLI/%s",
			wbDesktopVersionCN, wbDesktopVersionCN, wbDesktopCLICN)
	}
	return fmt.Sprintf("WorkBuddy/%s workbuddy-ai/%s CLI/%s",
		wbDesktopVersionIntl, wbDesktopVersionIntl, wbDesktopCLIIntl)
}

func wbDefaultIdeVersion(realm, product string) string {
	switch product {
	case "cli":
		return wbCLIVersion
	case "vscode":
		return wbVSCODEIdeVersion
	}
	if realm == "cn" {
		return wbDesktopVersionCN
	}
	return wbDesktopVersionIntl
}

// ---------------------------------------------------------------------------
// 标识生成：官方用的都是 UUIDv4 / UUIDv7 去连字符，这里保持同形状
// ---------------------------------------------------------------------------

func wbRandHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		ns := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(ns >> uint(i%8*8))
		}
	}
	return hex.EncodeToString(b)
}

func wbUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return wbFormatUUID(b)
}

func wbUUIDv7() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return wbFormatUUID(b)
}

// wbFormatUUID 把 16 字节按 8-4-4-4-12 格式化成带连字符的 UUID。
func wbFormatUUID(b [16]byte) string {
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func wbHex32() string { return wbRandHex(16) }

// ---------------------------------------------------------------------------
// 请求体改写
// ---------------------------------------------------------------------------

// wbClientSignatures 是上游网关用来判定「请求是不是官方客户端发的」的提示词指纹。
//
// 实测（2026-11-24，New API v1.0.0-rc.49 → WorkBuddy 渠道）：首条 system 消息以
// "You are an interactive agent in TraeCode" 开头时，网关稳定回 400 / code 11128；
// 只要把这几个字换掉（或在前面加一个字），同样的请求立刻 200。说明它是按提示词
// 开头的客户端签名做渠道校验，而不是看请求头。
//
// 这里只改文本内容里的指纹，不动工具定义、工具参数和图片数据。
var wbClientSignatures = [][2]string{
	{"You are an interactive agent in TraeCode", "You are an interactive agent in CodeBuddy"},
	{"an interactive agent in TraeCode", "an interactive agent in CodeBuddy"},
	{"TraeCode", "CodeBuddy"},
}

// wbSanitizeTextReport 替换指纹并报告是否真的改过。
func wbSanitizeTextReport(s string) (string, bool) {
	changed := false
	for _, pair := range wbClientSignatures {
		if strings.Contains(s, pair[0]) {
			s = strings.ReplaceAll(s, pair[0], pair[1])
			changed = true
		}
	}
	return s, changed
}

func wbSanitizeText(s string) string {
	out, _ := wbSanitizeTextReport(s)
	return out
}

// wbSanitizeContent 处理 content：字符串直接换，分块内容只换 text 片段。
func wbSanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		return wbSanitizeTextReport(c)
	case []any:
		changed := false
		for i, part := range c {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if txt, ok := pm["text"].(string); ok {
				if nv, ch := wbSanitizeTextReport(txt); ch {
					pm["text"] = nv
					c[i] = pm
					changed = true
				}
			}
		}
		return c, changed
	}
	return v, false
}

// wbPrefixBreak 在首条 system 文本最前面补一个换行，打断网关的前缀指纹匹配。
//
// 实测：网关会 trim 掉空格但不 trim 换行 —— " You are..." 仍然被拒，
// "\nYou are..." 直接 200。这比把产品名改掉更保守：提示词措辞一个字没变，
// 只动了一个空白字符。
func wbPrefixBreak(m map[string]any) bool {
	if v, ok := m["instructions"].(string); ok && v != "" {
		if !strings.HasPrefix(v, "\n") {
			m["instructions"] = "\n" + v
			return true
		}
	}
	for _, key := range []string{"input", "messages"} {
		arr, ok := m[key].([]any)
		if !ok {
			continue
		}
		for i, raw := range arr {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := item["role"].(string); role != "system" {
				continue
			}
			switch c := item["content"].(type) {
			case string:
				if c != "" && !strings.HasPrefix(c, "\n") {
					item["content"] = "\n" + c
					arr[i] = item
					return true
				}
			case []any:
				for j, part := range c {
					pm, ok := part.(map[string]any)
					if !ok {
						continue
					}
					txt, ok := pm["text"].(string)
					if !ok || txt == "" || strings.HasPrefix(txt, "\n") {
						continue
					}
					pm["text"] = "\n" + txt
					c[j] = pm
					item["content"] = c
					arr[i] = item
					return true
				}
			}
			return false
		}
	}
	return false
}

// wbSanitizeBody 处理 body 里会被网关当成「非官方客户端」的提示词指纹。
// style：prefix（默认，只在首条 system 前加换行）/ rename（改产品名）/ both。
// 只触碰 instructions / input[].content / messages[].content，工具与图片不动。
func wbSanitizeBody(body []byte, style string) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}
	changed := false
	if style == "prefix" || style == "both" {
		if wbPrefixBreak(m) {
			changed = true
		}
	}
	if style == "rename" || style == "both" {
		if v, ok := m["instructions"].(string); ok {
			if nv, ch := wbSanitizeTextReport(v); ch {
				m["instructions"] = nv
				changed = true
			}
		}
		walk := func(key string) {
			arr, ok := m[key].([]any)
			if !ok {
				return
			}
			for i, raw := range arr {
				item, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if c, ok := item["content"]; ok {
					if nc, ch := wbSanitizeContent(c); ch {
						item["content"] = nc
						arr[i] = item
						changed = true
					}
				}
			}
		}
		walk("input")
		walk("messages")
	}
	if !changed {
		return body, nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body, nil
	}
	return out, nil
}

// wbRewriteBody 把 OpenAI 兼容 body 收敛成 WorkBuddy 能接受的形态：
//   - stream 强制 true + stream_options.include_usage
//   - developer 角色降级为 system（官方网关不收 developer）
//   - 去掉网关不认的私有字段
//   - deepseek 系列补 thinking / reasoning_effort
func wbRewriteBody(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}

	if msgs, ok := m["messages"].([]any); ok {
		for i, raw := range msgs {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := msg["role"].(string); role == "developer" {
				msg["role"] = "system"
				msgs[i] = msg
			}
		}
	}

	m["stream"] = true
	if _, ok := m["stream_options"]; !ok {
		m["stream_options"] = map[string]any{"include_usage": true}
	}

	for _, k := range []string{"metadata", "store", "service_tier",
		"prediction", "modalities", "audio", "logprobs", "top_logprobs",
		"seed", "logit_bias", "user"} {
		delete(m, k)
	}

	if model, _ := m["model"].(string); strings.HasPrefix(strings.ToLower(model), "deepseek") {
		if th, ok := m["thinking"].(map[string]any); ok {
			if t, _ := th["type"].(string); t == "disabled" {
				delete(m, "reasoning_effort")
				delete(m, "reasoningEffort")
			}
		} else if _, ok := m["thinking"]; !ok {
			m["thinking"] = map[string]any{"type": "enabled"}
		}
		if _, has := m["reasoning_effort"]; !has {
			if _, has2 := m["reasoningEffort"]; !has2 {
				m["reasoning_effort"] = "high"
			}
		}
	}

	if msgs, ok := m["messages"].([]any); ok {
		hasReasoning := false
		for _, raw := range msgs {
			if msg, ok := raw.(map[string]any); ok {
				if _, ok := msg["reasoning_content"]; ok {
					hasReasoning = true
				}
				if r, ok := msg["reasoning"].(string); ok && r != "" {
					hasReasoning = true
				}
			}
		}
		if hasReasoning {
			for _, raw := range msgs {
				msg, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if role, _ := msg["role"].(string); role == "assistant" {
					if _, ok := msg["reasoning_content"]; !ok {
						if r, ok := msg["reasoning"].(string); ok {
							msg["reasoning_content"] = r
						} else {
							msg["reasoning_content"] = ""
						}
					}
				}
			}
		}
	}

	return json.Marshal(m)
}

// ---------------------------------------------------------------------------
// 出站头
// ---------------------------------------------------------------------------

// 会泄露中转站/客户端身份、或网关不认的入站头，一律先清掉。
var wbStripHeaders = []string{
	"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
	"X-Real-Ip", "Forwarded", "Via", "X-Proxy-Attempt",
	"X-Stainless-Os", "X-Stainless-Arch", "X-Stainless-Lang",
	"X-Stainless-Package-Version", "X-Stainless-Runtime",
	"X-Stainless-Runtime-Version", "X-Stainless-Retry-Count",
	"X-Stainless-Timeout", "X-Stainless-Helper-Method",
	"X-Hub-Spot-Ray-Id", "Cf-Ray", "Cf-Connecting-Ip",
	"X-Cloudide-Token", "X-Ide-Token", "X-Tt-Token",
	"X-App-Id", "X-App-Version", "X-App-Version-Code",
	"X-Ide-Version-Code", "X-Plugin-Channel", "X-Tt-Env", "X-Use-Ppe",
	"X-Trae-Authorized-Services", "X-Custom-Trace-Id",
	"X-Device-Brand", "X-Device-Cpu", "X-Os-Version", "X-Device-Type",
	"X-Machine-Id", "X-Machine-ID", "X-Web-Id", "X-Session-Id",
	"X-Session-ID", "X-Model-Id",
	"Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest",
	"Sec-Ch-Ua", "Sec-Ch-Ua-Mobile", "Sec-Ch-Ua-Platform",
}

// wbApplyHeaders 按官方顺序重建出站头。
// 保留并复用：调用方 Authorization（中转站 key）。
func wbApplyHeaders(dst http.Header, cfg workbuddyConfig, product, model string) {
	for _, k := range wbStripHeaders {
		dst.Del(k)
	}

	auth := dst.Get("Authorization")
	if auth == "" {
		if k := dst.Get("X-Api-Key"); k != "" {
			auth = "Bearer " + strings.TrimSpace(k)
		}
	}

	token := strings.TrimPrefix(auth, "Bearer ")
	apiKey := ""
	if strings.HasPrefix(token, "ck_") || strings.HasPrefix(token, "pt_") {
		apiKey = token
	}

	uid := dst.Get("X-User-Id")
	if uid == "" {
		uid = wbUIDFromToken(token)
	}
	if uid == "" {
		uid = "anonymous"
	}

	ep := wbEndpointFor(cfg.Realm, product)
	domain := orDefault(cfg.Domain, ep.Domain)

	conv := wbUUIDv7()
	reqID := wbUUIDv7()
	msgID := wbUUIDv7()
	trace := wbRandHex(16)

	dst.Set("Content-Type", "application/json")
	dst.Set("Accept", "application/json, text/plain, */*")

	switch product {
	case "cli":
		dst.Set("Accept", "application/json")
		dst.Set("X-Agent-Intent", "craft")
		dst.Set("X-Agent-Purpose", "conversation")
		dst.Set("X-Agent-Type", "main")
		dst.Set("X-Agent-Runtime", "cli")
		dst.Set("X-IDE-Type", "CLI")
		dst.Set("X-IDE-Name", "CLI")
		dst.Set("X-IDE-Version", orDefault(cfg.IdeVersion, wbCLIVersion))
		dst.Set("X-Product", "SaaS")
		dst.Set("X-Domain", domain)
		dst.Set("User-Agent", orDefault(cfg.UpstreamUA, wbDefaultUA(cfg.Realm, "cli")))
		dst.Set("X-User-Id", uid)
		dst.Set("Authorization", orDefault(auth, "Bearer "))
		dst.Set("X-Conversation-ID", wbHex32())
		dst.Set("X-Conversation-Request-ID", reqID)
		dst.Set("X-Root-Request-ID", reqID)
		dst.Set("X-Conversation-Message-ID", msgID)
		dst.Set("X-Request-ID", msgID)
		dst.Set("X-Private-Data", orDefault(cfg.PrivateData, "false"))
		dst.Set("X-Trace-ID", trace)
		dst.Set("X-B3-TraceId", trace)
		dst.Set("X-B3-SpanId", msgID[:16])
		dst.Set("X-B3-Sampled", "1")

	case "vscode":
		dst.Set("X-Requested-With", "XMLHttpRequest")
		dst.Set("Origin", wbOriginFor(cfg.Realm))
		dst.Set("Referer", wbOriginFor(cfg.Realm)+"/")
		dst.Set("X-CodeBuddy-Request", "1")
		dst.Set("Accept-Language", "en-US")
		dst.Set("X-Agent-Intent", "craft")
		dst.Set("X-Agent-Type", "main")
		dst.Set("X-IDE-Type", wbVSCODEPlatform)
		dst.Set("X-IDE-Name", wbVSCODEPlatform)
		dst.Set("X-IDE-Version", orDefault(cfg.IdeVersion, wbVSCODEIdeVersion))
		dst.Set("X-Product-Version", wbVSCODEIdeVersion)
		dst.Set("X-Product", "SaaS")
		dst.Set("X-Domain", domain)
		dst.Set("User-Agent", orDefault(cfg.UpstreamUA, wbDefaultUA(cfg.Realm, "vscode")))
		dst.Set("X-User-Id", uid)
		dst.Set("Authorization", orDefault(auth, "Bearer "))
		dst.Set("X-Request-Trace-Id", wbUUIDv4())
		dst.Set("X-Conversation-ID", wbHex32())
		dst.Set("X-Conversation-Request-ID", reqID)
		dst.Set("X-Conversation-Message-ID", msgID)
		dst.Set("X-Request-ID", reqID)
		if model != "" {
			now := fmt.Sprintf("%d", time.Now().UnixMilli())
			dst.Set("X-Model-Id", model)
			dst.Set("monitor_promptPrepareStartTime", now)
			dst.Set("monitor_httpSendTime", now)
		}
		if cfg.Realm != "cn" {
			dst.Set("X-Private-Data", orDefault(cfg.PrivateData, "true"))
		} else if cfg.PrivateData != "" {
			dst.Set("X-Private-Data", cfg.PrivateData)
		}

	default: // desktop
		dst.Set("X-Requested-With", "XMLHttpRequest")
		dst.Set("Origin", wbOriginFor(cfg.Realm))
		dst.Set("Referer", wbOriginFor(cfg.Realm)+"/")
		dst.Set("X-CodeBuddy-Request", "1")
		dst.Set("Accept-Language", "en-US")
		dst.Set("X-Agent-Intent", "craft")
		dst.Set("X-Agent-Purpose", "conversation")
		dst.Set("X-Agent-Type", "main")
		dst.Set("X-IDE-Name", "WorkBuddy")
		dst.Set("X-IDE-Type", "WorkBuddy")
		dst.Set("X-IDE-Version", orDefault(cfg.IdeVersion, wbDefaultIdeVersion(cfg.Realm, "desktop")))
		dst.Set("X-Private-Data", orDefault(cfg.PrivateData, "true"))
		dst.Set("X-Product", "SaaS")
		dst.Set("X-Domain", domain)
		dst.Set("User-Agent", orDefault(cfg.UpstreamUA, wbDefaultUA(cfg.Realm, "desktop")))
		dst.Set("X-User-Id", uid)
		dst.Set("Authorization", orDefault(auth, "Bearer "))
		dst.Set("X-Conversation-ID", conv)
		dst.Set("X-Conversation-Request-ID", reqID)
		dst.Set("X-Root-Request-ID", reqID)
		dst.Set("X-Conversation-Message-ID", msgID)
		dst.Set("X-Request-ID", msgID)
		dst.Set("X-Trace-ID", trace)
		dst.Set("X-B3-TraceId", trace)
		dst.Set("X-B3-SpanId", msgID[:16])
		dst.Set("X-B3-Sampled", "1")
	}

	if apiKey != "" {
		dst.Set("X-Api-Key", apiKey)
	}
	if dst.Get("Authorization") == "" {
		dst.Set("Authorization", "Bearer ")
	}
}

func wbOriginFor(realm string) string {
	if realm == "cn" {
		return "https://www.codebuddy.cn"
	}
	return "https://www.workbuddy.ai"
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

// wbUIDFromToken 从 JWT 的 payload.data.id / user_id 取用户 id。
func wbUIDFromToken(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return ""
	}
	seg := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
	switch len(seg) % 4 {
	case 2:
		seg += "=="
	case 3:
		seg += "="
	}
	raw, err := base64.StdEncoding.DecodeString(seg)
	if err != nil {
		return ""
	}
	var payload struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	if payload.Data.ID != "" {
		return payload.Data.ID
	}
	return payload.UserID
}

// ---------------------------------------------------------------------------
// 路径改写
// ---------------------------------------------------------------------------

// 入站协议路径 → 上游唯一的 chat 路径。上游只认 /v2/chat/completions，
// 所以 chat / responses / messages 三条入口都收敛到它。
var wbPathAliases = map[string]string{
	"/v1/chat/completions": "/v2/chat/completions",
	"/chat/completions":    "/v2/chat/completions",
	"/v2/chat/completions": "/v2/chat/completions",
	"/v1/messages":         "/v2/chat/completions",
	"/v1/responses":        "/v2/chat/completions",
	"/responses":           "/v2/chat/completions",
}

// wbTargetPath 把入站路径映射到上游真实路径。
func wbTargetPath(in string, cfg workbuddyConfig) string {
	if !cfg.ConverPath {
		return in
	}
	if p, ok := wbPathAliases[in]; ok {
		return p
	}
	return in
}

// wbIsChatPath 判断这条请求是否要走身份改写（chat / responses / messages）。
func wbIsChatPath(p string) bool {
	switch {
	case strings.HasSuffix(p, "/chat/completions"):
		return true
	case strings.HasSuffix(p, "/messages"):
		return true
	case strings.HasSuffix(p, "/responses"):
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 统计
// ---------------------------------------------------------------------------

var wbRewritten atomic.Int64

func wbRewriteCount() int64 { return wbRewritten.Load() }

// parseWorkbuddyConfig 解析 WB_* 配置。
// 优先级：命令行 > 配置文件 > 环境变量 > 默认值（pick 已按此顺序取值）。
func parseWorkbuddyConfig(
	kv map[string]string,
	explicit map[string]bool,
	pick func(key, cur string, isExplicit bool) string,
	mode, realm, ua, ideVer, domain, privateData, retryChannel, sanitize, sanitizeStyle string,
) workbuddyConfig {
	cfg := workbuddyConfig{
		Mode:        strings.ToLower(strings.TrimSpace(pick("WB_MODE", mode, explicit["wb-mode"]))),
		Realm:       strings.ToLower(strings.TrimSpace(pick("WB_REALM", realm, explicit["wb-realm"]))),
		UpstreamUA:  pick("WB_UA", ua, explicit["wb-ua"]),
		IdeVersion:  pick("WB_IDE_VERSION", ideVer, explicit["wb-ide-version"]),
		Domain:      pick("WB_DOMAIN", domain, explicit["wb-domain"]),
		PrivateData: strings.ToLower(strings.TrimSpace(pick("WB_PRIVATE_DATA", privateData, explicit["wb-private-data"]))),
	}
	switch cfg.Mode {
	case "desktop", "vscode", "cli":
	case "off", "", "0", "false", "no":
		cfg.Mode = ""
	default:
		cfg.Mode = ""
	}
	if cfg.Realm != "cn" {
		cfg.Realm = "intl"
	}
	if cfg.PrivateData != "true" && cfg.PrivateData != "false" {
		cfg.PrivateData = ""
	}
	raw := strings.ToLower(strings.TrimSpace(pick("WB_CONVERT_PATH", "1", explicit["wb-convert-path"])))
	cfg.ConverPath = !(raw == "0" || raw == "false" || raw == "off" || raw == "no")
	rawRetry := strings.ToLower(strings.TrimSpace(pick("WB_RETRY_CHANNEL", retryChannel, explicit["wb-retry-channel"])))
	cfg.RetryChannel = !(rawRetry == "0" || rawRetry == "false" || rawRetry == "off" || rawRetry == "no")
	rawSan := strings.ToLower(strings.TrimSpace(pick("WB_SANITIZE", "1", explicit["wb-sanitize"])))
	cfg.Sanitize = !(rawSan == "0" || rawSan == "false" || rawSan == "off" || rawSan == "no")
	style := strings.ToLower(strings.TrimSpace(pick("WB_SANITIZE_STYLE", sanitizeStyle, explicit["wb-sanitize-style"])))
	switch style {
	case "prefix", "rename", "both":
		cfg.SanitizeStyle = style
	default:
		cfg.SanitizeStyle = "prefix"
	}
	cfg.APIMode = strings.ToLower(strings.TrimSpace(pick("WB_API_MODE", "auto", explicit["wb-api-mode"])))
	if cfg.APIMode != "chat" && cfg.APIMode != "responses" {
		cfg.APIMode = "auto"
	}
	return cfg
}

// ---------------------------------------------------------------------------
// /v1/responses 与 /v1/messages 的入站形态
//
// 两种额外协议都要收敛成上游唯一的 chat 形态。这里只做结构转换，不改语义：
//   - responses：input 数组 + instructions，转成 messages
//   - messages（Anthropic）：system + messages，转成 messages
// 上游只会说 chat，所以两条路径最终都落到 /v2/chat/completions。
// ---------------------------------------------------------------------------

// wbResponsesToChat 把 OpenAI Responses API 的 body 转成 Chat Completions body。
func wbResponsesToChat(raw []byte) ([]byte, bool) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return raw, false
	}
	if _, ok := in["input"]; !ok {
		return raw, false
	}

	out := map[string]any{}
	for _, k := range []string{"model", "temperature", "top_p", "stream",
		"max_output_tokens", "reasoning", "text"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	if v, ok := out["max_output_tokens"]; ok {
		out["max_tokens"] = v
		delete(out, "max_output_tokens")
	}
	delete(out, "text")
	if _, ok := in["stream"]; !ok {
		out["stream"] = true
	}

	var msgs []any
	if sys, ok := in["instructions"].(string); ok && strings.TrimSpace(sys) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}

	appendItem := func(item map[string]any) {
		role, _ := item["role"].(string)
		typ, _ := item["type"].(string)
		switch {
		// Responses 的普通消息既可能是 {role,content}，也可能显式带
		// type=message；两种都要收，否则真实客户端（Trae）的 system 提示词
		// 会被整条丢掉。
		case (typ == "" || typ == "message") && role != "":
			msgs = append(msgs, map[string]any{
				"role":    role,
				"content": wbContentToChat(item["content"]),
			})
		case typ == "function_call_output":
			msgs = append(msgs, map[string]any{
				"role":         "tool",
				"tool_call_id": item["call_id"],
				"content":      wbStringify(item["output"]),
			})
		case typ == "function_call":
			msgs = append(msgs, map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":   item["call_id"],
					"type": "function",
					"function": map[string]any{
						"name":      item["name"],
						"arguments": wbStringify(item["arguments"]),
					},
				}},
			})
		}
	}

	switch v := in["input"].(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": v})
	case []any:
		for _, it := range v {
			if s, ok := it.(string); ok {
				msgs = append(msgs, map[string]any{"role": "user", "content": s})
				continue
			}
			if m, ok := it.(map[string]any); ok {
				appendItem(m)
			}
		}
	}
	if len(msgs) == 0 {
		return raw, false
	}
	out["messages"] = msgs

	// 工具定义
	if tools, ok := in["tools"].([]any); ok && len(tools) > 0 {
		var conv []any
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			// responses 形态：{type:"function", name, description, parameters}
			// chat 形态：{type:"function", function:{name, description, parameters}}
			if _, hasFn := tm["function"]; hasFn {
				conv = append(conv, tm)
				continue
			}
			fn := map[string]any{}
			for _, k := range []string{"name", "description", "parameters"} {
				if v, ok := tm[k]; ok {
					fn[k] = v
				}
			}
			conv = append(conv, map[string]any{"type": "function", "function": fn})
		}
		if len(conv) > 0 {
			out["tools"] = conv
			if tc, ok := in["tool_choice"]; ok {
				out["tool_choice"] = tc
			}
		}
	}

	raw2, err := json.Marshal(out)
	if err != nil {
		return raw, false
	}
	return raw2, true
}

// wbMessagesToChat 把 Anthropic Messages 的 body 转成 Chat Completions body。
func wbMessagesToChat(raw []byte) ([]byte, bool) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return raw, false
	}
	if _, ok := in["messages"]; !ok {
		return raw, false
	}
	if _, ok := in["max_tokens"]; !ok {
		// Anthropic 必有 max_tokens；缺了多半已经是 chat 形态
		return raw, false
	}

	out := map[string]any{"model": in["model"], "stream": true}
	if v, ok := in["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := in["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := in["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := in["stop_sequences"]; ok {
		out["stop"] = v
	}

	var msgs []any
	if sys, ok := in["system"]; ok {
		if s, ok := sys.(string); ok && strings.TrimSpace(s) != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": s})
		} else if arr, ok := sys.([]any); ok {
			msgs = append(msgs, map[string]any{"role": "system", "content": wbContentToChat(arr)})
		}
	}
	if arr, ok := in["messages"].([]any); ok {
		for _, it := range arr {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			msgs = append(msgs, map[string]any{
				"role":    m["role"],
				"content": wbContentToChat(m["content"]),
			})
		}
	}
	out["messages"] = msgs

	if tools, ok := in["tools"].([]any); ok && len(tools) > 0 {
		var conv []any
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fn := map[string]any{"name": tm["name"]}
			if v, ok := tm["description"]; ok {
				fn["description"] = v
			}
			if v, ok := tm["input_schema"]; ok {
				fn["parameters"] = v
			}
			conv = append(conv, map[string]any{"type": "function", "function": fn})
		}
		if len(conv) > 0 {
			out["tools"] = conv
		}
	}

	raw2, err := json.Marshal(out)
	if err != nil {
		return raw, false
	}
	return raw2, true
}

// wbContentToChat 把多模态/分块内容压成 chat 的 content 形态。
func wbContentToChat(v any) any {
	switch c := v.(type) {
	case string:
		return c
	case nil:
		return ""
	case []any:
		var parts []any
		hasImage := false
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				if s, ok := p.(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": s})
				}
				continue
			}
			switch pm["type"] {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": pm["text"]})
			case "image":
				hasImage = true
				if src, ok := pm["source"].(map[string]any); ok {
					if src["type"] == "base64" {
						parts = append(parts, map[string]any{
							"type": "image_url",
							"image_url": map[string]any{
								"url": "data:" + wbStringify(src["media_type"]) +
									";base64," + wbStringify(src["data"]),
							},
						})
					} else if u := wbStringify(src["url"]); u != "" {
						parts = append(parts, map[string]any{
							"type":      "image_url",
							"image_url": map[string]any{"url": u},
						})
					}
				}
			case "image_url":
				hasImage = true
				parts = append(parts, pm)
			case "tool_result":
				return wbStringify(pm["content"])
			}
		}
		if len(parts) == 1 && !hasImage {
			if t, ok := parts[0].(map[string]any)["text"].(string); ok {
				return t
			}
		}
		if len(parts) > 0 {
			return parts
		}
		return ""
	}
	return wbStringify(v)
}

// wbStringify 把任意 JSON 值转成字符串（数字不带引号、对象走 JSON）。
func wbStringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%v", x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// wbNormalizeInbound 按入站协议把 body 收敛成上游 chat 形态。
// 返回转换后的 body 与命中的协议名（""=本来就是 chat）。
func wbNormalizeInbound(path string, body []byte, cfg workbuddyConfig) ([]byte, string) {
	if cfg.APIMode == "chat" {
		return body, ""
	}
	if strings.HasSuffix(path, "/responses") {
		if b, ok := wbResponsesToChat(body); ok {
			return b, "responses"
		}
		return body, ""
	}
	if strings.HasSuffix(path, "/messages") {
		if b, ok := wbMessagesToChat(body); ok {
			return b, "messages"
		}
		return body, ""
	}
	if cfg.APIMode == "auto" {
		// 兜底：即便路径是 chat，也识别一次 responses 形态的裸 body
		if b, ok := wbResponsesToChat(body); ok {
			return b, "responses"
		}
	}
	return body, ""
}

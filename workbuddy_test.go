package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRewriteBodyDeveloperRole(t *testing.T) {
	in := `{"model":"glm-5.3","messages":[{"role":"developer","content":"be terse"},{"role":"user","content":"hi"}],"stream":false,"metadata":{"a":1}}`
	out, err := wbRewriteBody([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["stream"] != true {
		t.Errorf("stream should be forced true, got %v", m["stream"])
	}
	if _, ok := m["metadata"]; ok {
		t.Error("metadata should be stripped")
	}
	msgs := m["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("developer should map to system, got %v", first["role"])
	}
	if _, ok := m["stream_options"]; !ok {
		t.Error("stream_options should be added")
	}
}

func TestRewriteBodyDeepseekThinking(t *testing.T) {
	in := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	out, _ := wbRewriteBody([]byte(in))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	th, ok := m["thinking"].(map[string]any)
	if !ok || th["type"] != "enabled" {
		t.Errorf("thinking should be enabled, got %v", m["thinking"])
	}
	if m["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort should default high, got %v", m["reasoning_effort"])
	}
}

func TestResponsesToChat(t *testing.T) {
	in := `{"model":"glm-5.3","instructions":"sys","input":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"max_output_tokens":128}`
	out, ok := wbResponsesToChat([]byte(in))
	if !ok {
		t.Fatal("conversion should trigger")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expect system+user, got %d", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Error("instructions should become system")
	}
	if m["max_tokens"] != float64(128) {
		t.Errorf("max_output_tokens should map to max_tokens, got %v", m["max_tokens"])
	}
}

func TestMessagesToChat(t *testing.T) {
	in := `{"model":"glm-5.3","max_tokens":64,"system":"you are helpful","messages":[{"role":"user","content":"hi"}]}`
	out, ok := wbMessagesToChat([]byte(in))
	if !ok {
		t.Fatal("conversion should trigger")
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("expect system+user, got %v", msgs)
	}
}

func TestApplyHeadersDesktopIntl(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer ck_abc")
	cfg := workbuddyConfig{Mode: "desktop", Realm: "intl"}
	wbApplyHeaders(h, cfg, "desktop", "glm-5.3")

	want := map[string]string{
		"X-Ide-Type":          "WorkBuddy",
		"X-Ide-Name":          "WorkBuddy",
		"X-Ide-Version":       "5.9.4",
		"X-Agent-Intent":      "craft",
		"X-Agent-Purpose":     "conversation",
		"X-Agent-Type":        "main",
		"X-Product":           "SaaS",
		"X-Domain":            "www.workbuddy.ai",
		"X-Codebuddy-Request": "1",
		"User-Agent":          "WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if h.Get("Authorization") != "Bearer ck_abc" {
		t.Errorf("Authorization must be preserved, got %q", h.Get("Authorization"))
	}
	if h.Get("X-Api-Key") != "ck_abc" {
		t.Errorf("static API key should also be sent as X-Api-Key, got %q", h.Get("X-Api-Key"))
	}
	if h.Get("X-Conversation-Id") == "" || h.Get("X-Request-Id") == "" {
		t.Error("conversation / request ids must be generated")
	}
}

func TestApplyHeadersStripsLeaks(t *testing.T) {
	h := http.Header{}
	h.Set("X-Forwarded-For", "1.2.3.4")
	h.Set("X-Cloudide-Token", "leak")
	h.Set("X-App-Id", "leak")
	h.Set("Authorization", "Bearer x")
	wbApplyHeaders(h, workbuddyConfig{Mode: "desktop", Realm: "intl"}, "desktop", "")
	for _, k := range []string{"X-Forwarded-For", "X-Cloudide-Token", "X-App-Id"} {
		if h.Get(k) != "" {
			t.Errorf("%s should be stripped, got %q", k, h.Get(k))
		}
	}
}

func TestApplyHeadersCN(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer k")
	wbApplyHeaders(h, workbuddyConfig{Mode: "desktop", Realm: "cn"}, "desktop", "")
	if h.Get("X-Domain") != "copilot.tencent.com" {
		t.Errorf("CN domain wrong: %q", h.Get("X-Domain"))
	}
	if !strings.Contains(h.Get("User-Agent"), "WorkBuddy/5.8.3") {
		t.Errorf("CN UA wrong: %q", h.Get("User-Agent"))
	}
}

func TestCLIHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer k")
	wbApplyHeaders(h, workbuddyConfig{Mode: "cli", Realm: "intl"}, "cli", "")
	if h.Get("X-Ide-Type") != "CLI" || h.Get("X-Ide-Name") != "CLI" {
		t.Error("cli identity should be CLI/CLI")
	}
	if h.Get("Origin") != "" {
		t.Error("cli identity must not send browser Origin")
	}
}

func TestTargetPath(t *testing.T) {
	cfg := workbuddyConfig{ConverPath: true}
	cases := map[string]string{
		"/v1/chat/completions": "/v2/chat/completions",
		"/chat/completions":    "/v2/chat/completions",
		"/v1/messages":         "/v2/chat/completions",
		"/v1/models":           "/v1/models",
	}
	for in, want := range cases {
		if got := wbTargetPath(in, cfg); got != want {
			t.Errorf("wbTargetPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUIDFromToken(t *testing.T) {
	// {"data":{"id":"123"},"user_id":"999"}
	tok := "h.eyJkYXRhIjp7ImlkIjoiMTIzIn0sInVzZXJfaWQiOiI5OTkifQ.s"
	if got := wbUIDFromToken(tok); got != "123" {
		t.Errorf("uid = %q, want 123", got)
	}
	if got := wbUIDFromToken("garbage"); got != "" {
		t.Errorf("garbage token should yield empty, got %q", got)
	}
}

func TestWBChannelRejectCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
	}{
		{"裸网关体", `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, 11128},
		{"客户端马甲码", `{"code":4028,"msg":"Request blocked"}`, 4028},
		{"外层再包一层", `{"error":{"message":"upstream 400: {\"code\":11128,\"msg\":\"Illegal API invocation from an unapproved channel\"}"}}`, 11128},
		{"双重转义字符串", `{"error":{"message":"{\"code\":11128,\"msg\":\"Illegal API invocation from an unapproved channel\"}"}}`, 11128},
		{"普通参数错误", `{"error":{"message":"input is required","type":"new_api_error"}}`, 0},
		{"空体", ``, 0},
		{"非 JSON", `gateway timeout`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _ := wbChannelRejectCode([]byte(c.body))
			if code != c.code {
				t.Fatalf("code=%d 期望 %d（体：%s）", code, c.code, c.body)
			}
		})
	}
}

// 渠道校验错误不能被当成普通响应放过去：它是可重试信号。
func TestWBChannelRejectNotConfusedWithUsageError(t *testing.T) {
	if code, _ := wbChannelRejectCode([]byte(`{"code":200,"msg":"ok"}`)); code != 0 {
		t.Fatalf("正常体被误判成渠道错误：code=%d", code)
	}
}

// Responses 里显式带 type=message 的普通消息不能被丢掉（Trae 真实请求就是这种形态）。
func TestWBResponsesToChatKeepsTypedMessages(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"message","role":"system","content":[{"type":"input_text","text":"sys prompt"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	out, ok := wbResponsesToChat(body)
	if !ok {
		t.Fatalf("转换失败")
	}
	var got struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("解析转换结果失败：%v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("期望 2 条消息，实际 %d：%s", len(got.Messages), out)
	}
	if got.Messages[0].Role != "system" || got.Messages[1].Role != "user" {
		t.Fatalf("角色顺序不对：%s", out)
	}
}

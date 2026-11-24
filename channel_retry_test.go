package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const channelRejectBody = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel",` +
	`"requestId":"01a20daf8a9d7f3f9a82640fb10be380","retry_policy":{"retry":false}}`

func testProxyConfig(upstream string, attempts int) config {
	return config{
		upstreams:   []string{upstream},
		maxAttempts: attempts,
		baseDelay:   10 * time.Millisecond,
		maxDelay:    20 * time.Millisecond,
		headTimeout: 5 * time.Second,
		dialTimeout: 2 * time.Second,
		maxBody:     1 << 20,
		guard:       defaultReasoningGuard(),
		wb:          workbuddyConfig{Mode: "desktop", Realm: "intl", RetryChannel: true},
	}
}

func doProxy(t *testing.T, cfg config, client *http.Client) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"deepseek-v4.1-flash","input":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	proxy(cfg, client, rec, req)
	return rec
}

// 渠道校验失败（400 + code 11128）必须被当作可重试错误：前两次拒绝，
// 第三次成功时客户端应当拿到 200 的完整流，而不是那个 400。
func TestProxyRetriesChannelReject(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n <= 2 {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(channelRejectBody))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"}}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer up.Close()

	usageStore = newUsageStore(t.TempDir())
	rec := doProxy(t, testProxyConfig(up.URL, 5), up.Client())

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("客户端没拿到完整 SSE 流：%s", rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("期望上游被调用 3 次（2 次拒绝 + 1 次成功），实际 %d", got)
	}
}

// 渠道一直拒绝时必须收敛：用尽尝试次数后把原始 400 体透传，不无限重试。
func TestProxyChannelRejectGivesUp(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(channelRejectBody))
	}))
	defer up.Close()

	usageStore = newUsageStore(t.TempDir())
	rec := doProxy(t, testProxyConfig(up.URL, 3), up.Client())

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望透传 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "11128") {
		t.Fatalf("响应体应当包含上游原始错误：%s", rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("期望用尽 3 次尝试，实际 %d", got)
	}
}

// 关闭开关后不得重试，直接透传 400。
func TestProxyChannelRejectRetryDisabled(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(channelRejectBody))
	}))
	defer up.Close()

	usageStore = newUsageStore(t.TempDir())
	cfg := testProxyConfig(up.URL, 3)
	cfg.wb.RetryChannel = false
	rec := doProxy(t, cfg, up.Client())

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", rec.Code)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("关闭开关后不应重试，实际调用 %d 次", got)
	}
}

// 普通 400（参数错误）不能被误判成渠道问题，必须原样透传且不重试。
func TestProxyPassesThroughNormal400(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"input is required","type":"new_api_error"}}`))
	}))
	defer up.Close()

	usageStore = newUsageStore(t.TempDir())
	rec := doProxy(t, testProxyConfig(up.URL, 3), up.Client())

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", rec.Code)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("普通 400 不应重试，实际调用 %d 次", got)
	}
}

// 端到端确认：经过空思考守卫的前瞻与重放，客户端只会收到一份正文块。
func TestProxyStreamNotDuplicated(t *testing.T) {
	const chunk = `data: {"choices":[{"delta":{"content":"ok"}}]}` + "\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chunk + "data: [DONE]\n\n"))
	}))
	defer up.Close()

	usageStore = newUsageStore(t.TempDir())
	rec := doProxy(t, testProxyConfig(up.URL, 3), up.Client())

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	if n := strings.Count(rec.Body.String(), `"content":"ok"`); n != 1 {
		t.Fatalf("正文块出现 %d 次，应为 1 次：\n%s", n, rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), "[DONE]"); n != 1 {
		t.Fatalf("[DONE] 出现 %d 次，应为 1 次", n)
	}
}

// buildRequest 必须在 WB 模式下把 system 指纹清掉，其余结构保持原样。
func TestBuildRequestSanitizesFingerprint(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","input":[{"type":"message","role":"system",` +
		`"content":[{"type":"input_text","text":"You are an interactive agent in TraeCode that helps the USER."}]}]}`)
	orig := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	orig.Header.Set("Authorization", "Bearer sk-test")
	orig.Header.Set("User-Agent", "hertz")

	cfg := workbuddyConfig{Mode: "desktop", Realm: "intl", Sanitize: true, RetryChannel: true}
	req, err := buildRequest(orig, body, "http://192.168.5.246:4000", context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildRequest 失败：%v", err)
	}
	sent, _ := io.ReadAll(req.Body)
	if strings.Contains(string(sent), "in TraeCode") {
		t.Fatalf("出站 body 仍带 Trae 指纹：%s", sent)
	}
	if ua := req.Header.Get("User-Agent"); !strings.HasPrefix(ua, "WorkBuddy/") {
		t.Fatalf("出站 UA 没被替换成官方身份：%q", ua)
	}
	if req.Header.Get("X-Model-Provider") != "" {
		t.Fatalf("客户端私有头泄漏到上游：%v", req.Header)
	}
}

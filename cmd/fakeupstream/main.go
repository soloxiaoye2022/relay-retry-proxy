package main

// fakeupstream 是手工验证用的假上游：把收到的 path / headers / body 落盘，
// 然后按 mode 回 SSE。mode=reason 时只回 reasoning（用于验证空思考守卫）；
// FAKE_REJECT_N=N 时前 N 个请求回 WorkBuddy 的渠道校验 400（验证渠道重试）。
import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

type capture struct {
	Path    string              `json:"path"`
	Query   string              `json:"query"`
	Method  string              `json:"method"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

const sseChunk = `data: {"id":"x","choices":[{"delta":{"content":"ok"}}]}` + "\n\n"
const sseDone = `data: [DONE]` + "\n\n"
const sseReason = `data: {"choices":[{"delta":{"reasoning_content":"thinking hard"}}]}` + "\n\n" +
	`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	`data: [DONE]` + "\n\n"

// wbRejectBody 是 WorkBuddy 网关「渠道校验失败」的真实错误体，
// 用于验证代理是否会把 400 当成可重试错误（FAKE_REJECT_N=前几次请求回这个）。
const wbRejectBody = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel",` +
	`"requestId":"01a20daf8a9d7f3f9a82640fb10be380","retry_policy":{"retry":false}}`

func main() {
	out := os.Getenv("FAKE_OUT")
	if out == "" {
		out = "fake-capture.jsonl"
	}
	addr := os.Getenv("FAKE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:15000"
	}
	mode := os.Getenv("FAKE_MODE")
	f, _ := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	defer f.Close()
	var hits int64
	rejectN, _ := strconv.Atoi(os.Getenv("FAKE_REJECT_N"))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := capture{r.URL.Path, r.URL.RawQuery, r.Method, r.Header, string(body)}
		b, _ := json.Marshal(c)
		_, _ = f.Write(append(b, 0x0a))
		n := atomic.AddInt64(&hits, 1)

		if rejectN > 0 && n <= int64(rejectN) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, wbRejectBody)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		emit := func(s string) {
			fmt.Fprint(w, s)
			if fl != nil {
				fl.Flush()
			}
		}

		// mode=reason-always：每次都只回 reasoning（验证耗尽重试后报错）
		// mode=reason-then-ok：第一次只回 reasoning，第二次回正文（验证重试成功）
		switch mode {
		case "reason-always":
			emit(sseReason)
		case "reason-then-ok":
			if n == 1 {
				emit(sseReason)
			} else {
				emit(sseChunk)
				time.Sleep(20 * time.Millisecond)
				emit(sseDone)
			}
		default:
			emit(sseChunk)
			time.Sleep(20 * time.Millisecond)
			emit(sseDone)
		}
	})
	_ = http.ListenAndServe(addr, nil)
}

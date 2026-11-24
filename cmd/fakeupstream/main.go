package main

// fakeupstream 是手工验证用的假上游：把收到的 path / headers / body 落盘，
// 然后按 mode 回 SSE。mode=reason 时只回 reasoning（用于验证空思考守卫）。
import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := capture{r.URL.Path, r.URL.RawQuery, r.Method, r.Header, string(body)}
		b, _ := json.Marshal(c)
		_, _ = f.Write(append(b, 0x0a))
		n := atomic.AddInt64(&hits, 1)

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

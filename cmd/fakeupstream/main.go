package main

// fakeupstream 是手工验证用的假上游：把收到的 path / headers / body 落盘，
// 然后回一段最小 SSE。仅用于本地联调。
import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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

func main() {
	out := os.Getenv("FAKE_OUT")
	if out == "" {
		out = "fake-capture.jsonl"
	}
	addr := os.Getenv("FAKE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:15000"
	}
	f, _ := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	defer f.Close()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := capture{r.URL.Path, r.URL.RawQuery, r.Method, r.Header, string(body)}
		b, _ := json.Marshal(c)
		_, _ = f.Write(append(b, 0x0a))

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, sseChunk)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, sseDone)
		if fl != nil {
			fl.Flush()
		}
	})
	_ = http.ListenAndServe(addr, nil)
}

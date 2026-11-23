package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web
var webFS embed.FS

var startedAt = time.Now()

// RequestRecord 是写到 UI 上的一行记录。
type RequestRecord struct {
	Seq          int64   `json:"seq"`
	Time         string  `json:"time"`
	ID           string  `json:"id"`
	Method       string  `json:"method"`
	Path         string  `json:"path"`
	Attempt      int     `json:"attempt"`
	Status       int     `json:"status"`
	Bytes        int64   `json:"bytes"`
	DurationMs   int64   `json:"duration_ms"`
	Note         string  `json:"note"`
	Error        string  `json:"error,omitempty"`
	Model        string  `json:"model,omitempty"`
	InTokens     int64   `json:"in_tokens"`
	OutTokens    int64   `json:"out_tokens"`
	TPS          float64 `json:"tps"`
	TokensEst    bool    `json:"tokens_estimated,omitempty"`
	TTFTMs       int64   `json:"ttft_ms,omitempty"`
	GenTPS       float64 `json:"gen_tps,omitempty"`
	CachedTokens int64   `json:"cached_tokens,omitempty"`
	ReasonTokens int64   `json:"reasoning_tokens,omitempty"`
	RequestDump  string  `json:"request_dump,omitempty"`
	ResponseDump string  `json:"response_dump,omitempty"`
}

type ring struct {
	mu   sync.Mutex
	buf  []RequestRecord
	head int
	size int
	cap  int
	seq  int64
}

func newRing(capacity int) *ring {
	return &ring{buf: make([]RequestRecord, capacity), cap: capacity}
}

func (r *ring) push(rec RequestRecord) RequestRecord {
	r.mu.Lock()
	r.seq++
	rec.Seq = r.seq
	r.buf[r.head] = rec
	r.head = (r.head + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
	r.mu.Unlock()
	return rec
}

// snapshot 返回按时间倒序（最新在前）的记录。
func (r *ring) snapshot(limit int) []RequestRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > r.size {
		limit = r.size
	}
	out := make([]RequestRecord, 0, limit)
	for i := 1; i <= limit; i++ {
		idx := (r.head - i + r.cap) % r.cap
		out = append(out, r.buf[idx])
	}
	return out
}

// hub 负责把新记录推给所有 SSE 订阅者。
type hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newHub() *hub { return &hub{subs: map[chan []byte]struct{}{}} }

func (h *hub) add() chan []byte {
	ch := make(chan []byte, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) del(ch chan []byte) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *hub) broadcast(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default: // 慢客户端直接丢，不阻塞主链路
		}
	}
}

var (
	reqLog = newRing(600)
	uiHub  = newHub()
)

var usageStore *UsageStore

var (
	dumpCounter int64
	dumpDirCfg  string
	dumpKeepCfg int
)

func recordRequest(rec RequestRecord) {
	if dumpDirCfg != "" && atomic.AddInt64(&dumpCounter, 1)%25 == 0 {
		pruneDumps(dumpDirCfg, dumpKeepCfg)
	}
	saved := reqLog.push(rec)
	uiHub.broadcast("record", saved)
	uiHub.broadcast("stats", statsSnapshot())
}

type statsPayload struct {
	Total     int64 `json:"total"`
	Succeeded int64 `json:"succeeded"`
	Failed    int64 `json:"failed"`
	Retried   int64 `json:"retried"`
	Upstream5 int64 `json:"upstream5xx"`
}

func statsSnapshot() statsPayload {
	return statsPayload{
		Total:     st.total.Load(),
		Succeeded: st.succeeded.Load(),
		Failed:    st.failed.Load(),
		Retried:   st.retried.Load(),
		Upstream5: atomic.LoadInt64(&st.upstream5),
	}
}

// initUI 把 API + 静态资源挂到 mux 上，其余路径交给代理。
func initUI(mux *http.ServeMux, proxyFn http.HandlerFunc, cfg config) {
	staticFS, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"ok":          true,
			"version":     version,
			"upstreams":   cfg.upstreams,
			"started_at":  startedAt.Format(time.RFC3339),
			"listen":      cfg.listen,
			"now":         time.Now().Format(time.RFC3339),
			"max_attempt": cfg.maxAttempts,
		})
	})

	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, statsSnapshot())
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"max_attempts": cfg.maxAttempts,
			"base_delay":   cfg.baseDelay.String(),
			"max_delay":    cfg.maxDelay.String(),
			"head_timeout": cfg.headTimeout.String(),
			"dial_timeout": cfg.dialTimeout.String(),
			"hedge_after":  cfg.hedgeAfter.String(),
			"max_body_mb":  cfg.maxBody >> 20,
		})
	})

	mux.HandleFunc("/api/usage", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, usageStore.Summary())
	})

	mux.HandleFunc("/api/requests", func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		writeJSON(w, map[string]any{"records": reqLog.snapshot(limit)})
	})

	mux.HandleFunc("/api/requests/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		fmt.Fprintf(w, "event: hello\ndata: {\"ok\":true,\"version\":%q}\n\n", version)
		flusher.Flush()

		ch := uiHub.add()
		defer uiHub.del(ch)
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				if _, err := w.Write(msg); err != nil {
					return
				}
				flusher.Flush()
			case <-ping.C:
				if _, err := w.Write([]byte(": ping\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})

	mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, r, staticFS, "index.html")
	})

	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		if name == "" || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		serveAsset(w, r, staticFS, name)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			http.Redirect(w, r, "/ui", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/api/"):
			http.NotFound(w, r)
		default:
			proxyFn(w, r)
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// serveAsset 直接从 embed FS 输出单个文件（不经过 FileServer，避免 index.html 被重定向）。
func serveAsset(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// startAdminServers 启动只读 WebUI/API 监听（不含代理转发，避免变成开放代理）。
func startAdminServers(cfg config) {
	if cfg.adminListen == "" || cfg.adminListen == "0" {
		return
	}
	staticFS, err := fs.Sub(webFS, "web")
	if err != nil {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui", http.StatusFound)
			return
		}
		if r.URL.Path == "/ui" {
			serveAsset(w, r, staticFS, "index.html")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			name := strings.TrimPrefix(r.URL.Path, "/assets/")
			if name == "" || strings.Contains(name, "..") {
				http.NotFound(w, r)
				return
			}
			serveAsset(w, r, staticFS, name)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"ok": true, "version": version, "upstreams": cfg.upstreams,
			"started_at": startedAt.Format(time.RFC3339), "listen": cfg.listen,
			"now": time.Now().Format(time.RFC3339), "max_attempt": cfg.maxAttempts,
		})
	})
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, statsSnapshot())
	})
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"max_attempts": cfg.maxAttempts, "base_delay": cfg.baseDelay.String(),
			"max_delay": cfg.maxDelay.String(), "head_timeout": cfg.headTimeout.String(),
			"dial_timeout": cfg.dialTimeout.String(), "hedge_after": cfg.hedgeAfter.String(),
			"max_body_mb": cfg.maxBody >> 20,
		})
	})
	mux.HandleFunc("/api/usage", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, usageStore.Summary())
	})

	mux.HandleFunc("/api/requests", func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		writeJSON(w, map[string]any{"records": reqLog.snapshot(limit)})
	})
	mux.HandleFunc("/api/requests/stream", streamRecords)

	for _, addr := range strings.Split(cfg.adminListen, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		s := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func(a string, srv *http.Server) {
			log.Printf("[admin] WebUI 监听 http://%s/ui", a)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("[admin] %s 启动失败: %v", a, err)
			}
		}(addr, s)
	}
}

// streamRecords 抽出 SSE 处理，供两个监听复用。
func streamRecords(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprintf(w, "event: hello\ndata: {\"ok\":true,\"version\":%q}\n\n", version)
	flusher.Flush()

	ch := uiHub.add()
	defer uiHub.del(ch)
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// relay-retry-proxy：OpenAI 兼容的重试代理。
// 上游（中转站）出现连接超时 / 429 / 5xx / 空响应时自动重试；
// 正常响应按 SSE 流式透传，边收边发，不缓冲完整内容。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const version = "1.4.0"

// shutdownGrace 与应用层一致：给在途流式请求留足时间，避免重启把用户请求打断
const shutdownGrace = 150 * time.Second

type config struct {
	configFile  string
	listen      string
	upstreams   []string
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
	headTimeout time.Duration
	dialTimeout time.Duration
	hedgeAfter  time.Duration
	adminListen string
	dumpDir     string
	dumpMax     int64
	dumpKeep    int
	dumpBody    bool
	usageDir    string
	priceFile   string
	logPath     string
	maxBody     int64
}

type counters struct {
	total     atomic.Int64
	retried   atomic.Int64
	succeeded atomic.Int64
	failed    atomic.Int64
	upstream5 int64
}

var st counters

func main() {
	cfg := parseFlags()

	if cfg.logPath != "" {
		f, err := os.OpenFile(cfg.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("open log: %v", err)
		}
		defer f.Close()
		log.SetOutput(f)
	}

	dumpDirCfg, dumpKeepCfg = cfg.dumpDir, cfg.dumpKeep
	priceFileCfg = cfg.priceFile
	prices.load(cfg.priceFile)
	usageStore = newUsageStore(cfg.usageDir)
	defer usageStore.Close()
	log.Printf("[usage] 持久化目录 %s（已恢复历史 %d 条聚合）", cfg.usageDir, usageStore.totalB.Reqs)
	client := newHTTPClient(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("/__health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":          true,
			"version":     version,
			"upstreams":   cfg.upstreams,
			"total":       st.total.Load(),
			"retried":     st.retried.Load(),
			"succeeded":   st.succeeded.Load(),
			"failed":      st.failed.Load(),
			"upstream5xx": atomic.LoadInt64(&st.upstream5),
		})
	})
	initUI(mux, func(w http.ResponseWriter, r *http.Request) {
		proxy(cfg, client, w, r)
	}, cfg)

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}

	startAdminServers(cfg)

	go func() {
		log.Printf("[start] listen=%s upstreams=%s attempts=%d base=%s max=%s headTimeout=%s dialTimeout=%s hedgeAfter=%s dumpDir=%s",
			cfg.listen, strings.Join(cfg.upstreams, ","), cfg.maxAttempts, cfg.baseDelay, cfg.maxDelay, cfg.headTimeout, cfg.dialTimeout, cfg.hedgeAfter, cfg.dumpDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	// 优雅退出：不再接受新连接，等在途请求（可能正在流式输出上百秒）跑完
	log.Printf("[drain] 收到退出信号，停止接受新请求，等待在途请求完成（最长 %s）", shutdownGrace)
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[drain] 超时未完成，强制退出：%v", err)
	}
	log.Printf("[stop] total=%d retried=%d ok=%d fail=%d", st.total.Load(), st.retried.Load(), st.succeeded.Load(), st.failed.Load())
}

func parseFlags() config {
	var (
		cfg        config
		configPath = flag.String("config", "", "配置文件路径（默认同目录 relay-retry-proxy.env）")
		showVer    = flag.Bool("version", false, "打印版本后退出")
		adminAddr  = flag.String("admin-listen", "127.0.0.1:9090", "WebUI 监听地址，逗号分隔可多个；0 或空=不启用")
		dumpDir    = flag.String("dump-dir", "", "把请求/响应尾部落盘到该目录（空=不落盘），用于排查断流")
		dumpMaxKB  = flag.Int64("dump-max-kb", 2048, "单个 dump 文件上限（KB）")
		dumpKeep   = flag.Int("dump-keep", 400, "每个方向保留的文件数")
		dumpBody   = flag.Bool("dump-body", false, "dump 里是否包含请求体（默认 false，避免落盘 prompt）")
		usageDir   = flag.String("usage-dir", "", "用量数据持久化目录（默认：日志目录下的 usage/）")
		priceFile  = flag.String("prices", "", "模型单价文件 prices.json（默认：配置目录下）")
		tokModel   = flag.String("tokenize", "", "从 stdin 读文本，按指定模型词表打印 token 数后退出（自测用）")
		listen     = flag.String("listen", "127.0.0.1:8443", "监听地址")
		upstreams  = flag.String("upstream", "http://127.0.0.1:4000", "上游 base URL，逗号分隔可配多个做故障切换")
		maxAtt     = flag.Int("max-attempts", 5, "最大尝试次数（含首次）")
		baseDelay  = flag.Duration("base-delay", 800*time.Millisecond, "首次重试间隔")
		maxDelay   = flag.Duration("max-delay", 15*time.Second, "重试间隔上限")
		headTO     = flag.Duration("head-timeout", 90*time.Second, "等待上游响应头的最长时间")
		dialTO     = flag.Duration("dial-timeout", 10*time.Second, "TCP 建连超时")
		hedgeTO    = flag.Duration("hedge-after", 8*time.Second, "首次尝试迟迟没响应时，并行再发一次（0=关闭）")
		logPath    = flag.String("log", "", "日志文件，空则 stdout")
		maxBodyMB  = flag.Int64("max-body-mb", 64, "请求体上限（MB）")
	)
	flag.Parse()
	if *tokModel != "" {
		in, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatalf("read stdin: %v", err)
		}
		fmt.Printf("%d\n", CountTokens(*tokModel, string(in)))
		os.Exit(0)
	}
	if *showVer {
		fmt.Printf("relay-retry-proxy %s\n", version)
		os.Exit(0)
	}

	// 配置优先级：命令行 > 配置文件 relay-retry-proxy.env > 内置默认值
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	cfg.configFile = *configPath
	if cfg.configFile == "" {
		cfg.configFile = findConfigFile()
	}
	kv := loadEnvFile(cfg.configFile)
	pick := func(key string, cur string, isExplicit bool) string {
		if isExplicit {
			return cur
		}
		if v, ok := kv[key]; ok && v != "" {
			return v
		}
		if v := os.Getenv(key); v != "" {
			return v
		}
		return cur
	}

	cfg.listen = pick("LISTEN", *listen, explicit["listen"])
	up := pick("UPSTREAM", *upstreams, explicit["upstream"])
	for _, u := range strings.Split(up, ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, err := url.Parse(u); err != nil {
			log.Fatalf("bad upstream %q: %v", u, err)
		}
		cfg.upstreams = append(cfg.upstreams, strings.TrimRight(u, "/"))
	}
	if len(cfg.upstreams) == 0 {
		log.Fatal("at least one upstream is required")
	}

	parseInt := func(s string, def int) int {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			return def
		}
		return n
	}
	parseDur := func(s string, def time.Duration) time.Duration {
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil || d <= 0 {
			return def
		}
		return d
	}

	cfg.maxAttempts = parseInt(pick("MAX_ATTEMPTS", strconv.Itoa(*maxAtt), explicit["max-attempts"]), 5)
	cfg.baseDelay = parseDur(pick("BASE_DELAY", baseDelay.String(), explicit["base-delay"]), 800*time.Millisecond)
	cfg.maxDelay = parseDur(pick("MAX_DELAY", maxDelay.String(), explicit["max-delay"]), 15*time.Second)
	cfg.headTimeout = parseDur(pick("HEAD_TIMEOUT", headTO.String(), explicit["head-timeout"]), 90*time.Second)
	cfg.dialTimeout = parseDur(pick("DIAL_TIMEOUT", dialTO.String(), explicit["dial-timeout"]), 10*time.Second)
	cfg.hedgeAfter = parseDur(pick("HEDGE_AFTER", hedgeTO.String(), explicit["hedge-after"]), 8*time.Second)
	cfg.adminListen = pick("ADMIN_LISTEN", *adminAddr, explicit["admin-listen"])
	cfg.dumpDir = pick("DUMP_DIR", *dumpDir, explicit["dump-dir"])
	cfg.dumpMax = int64(parseInt(pick("DUMP_MAX_KB", strconv.FormatInt(*dumpMaxKB, 10), explicit["dump-max-kb"]), 2048)) << 10
	cfg.dumpKeep = parseInt(pick("DUMP_KEEP", strconv.Itoa(*dumpKeep), explicit["dump-keep"]), 400)
	cfg.dumpBody = *dumpBody
	if v, ok := kv["DUMP_BODY"]; ok && !explicit["dump-body"] {
		cfg.dumpBody = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}
	cfg.logPath = pick("LOG", *logPath, explicit["log"])
	cfg.usageDir = pick("USAGE_DIR", *usageDir, explicit["usage-dir"])
	cfg.priceFile = pick("PRICES_FILE", *priceFile, explicit["prices"])
	if cfg.priceFile == "" {
		for _, cand := range []string{"/etc/relay-retry-proxy/prices.json", filepath.Join(filepath.Dir(cfg.usageDir), "prices.json")} {
			if _, err := os.Stat(cand); err == nil {
				cfg.priceFile = cand
				break
			}
		}
	}
	if cfg.usageDir == "" {
		base := filepath.Dir(cfg.logPath)
		if cfg.logPath == "" {
			base = "."
		}
		cfg.usageDir = filepath.Join(base, "usage")
	}
	cfg.maxBody = int64(parseInt(pick("MAX_BODY_MB", strconv.FormatInt(*maxBodyMB, 10), explicit["max-body-mb"]), 64)) << 20
	if cfg.configFile != "" {
		log.Printf("[config] 使用配置文件 %s", cfg.configFile)
	}
	return cfg
}

func newHTTPClient(cfg config) *http.Client {
	dialer := &net.Dialer{Timeout: cfg.dialTimeout, KeepAlive: 60 * time.Second}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   cfg.dialTimeout,
		ExpectContinueTimeout: 2 * time.Second,
		ResponseHeaderTimeout: cfg.headTimeout,
	}
	return &http.Client{Transport: tr}
}

var hopHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true,
	"Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

// proxy 采用「对冲重试」：第一次请求若在 hedgeAfter 内还没拿到响应头，
// 就并行再发一次到下一个上游；谁先给出有效响应就用谁，其余取消。
// 这样即使客户端自己的超时很紧（Trae 大约 60~90s），也来得及在窗口内拿到结果。
func proxy(cfg config, client *http.Client, w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := requestID(r)
	st.total.Add(1)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cfg.maxBody))
	if err != nil {
		log.Printf("[%s] read body: %v", reqID, err)
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	type result struct {
		attempt int
		resp    *http.Response
		err     error
	}

	model := extractModel(body)

	rec := RequestRecord{
		Time:   time.Now().Format("2006-01-02 15:04:05"),
		ID:     reqID,
		Method: r.Method,
		Path:   r.URL.Path,
	}
	if cfg.dumpDir != "" {
		rec.RequestDump = writeRequestDump(cfg.dumpDir, reqID, r.Method, r.URL.Path, r.Header, body, cfg.dumpMax, cfg.dumpBody)
	}
	hedgedAt := 0
	defer func() {
		rec.DurationMs = time.Since(start).Milliseconds()
		if rec.Note == "" {
			rec.Note = "ok"
		}
		recordRequest(rec)
	}()

	rootCtx := r.Context()
	results := make(chan result, cfg.maxAttempts)
	cancels := map[int]context.CancelFunc{}
	nextAttempt := 0
	var lastErr string

	launch := func() bool {
		if nextAttempt >= cfg.maxAttempts {
			return false
		}
		nextAttempt++
		a := nextAttempt
		up := cfg.upstreams[(a-1)%len(cfg.upstreams)]
		actx, cancel := context.WithCancel(rootCtx)
		cancels[a] = cancel
		if a > 1 {
			st.retried.Add(1)
			log.Printf("[%s] launching attempt %d/%d -> %s", reqID, a, cfg.maxAttempts, up)
		}
		go func() {
			req, err := buildRequest(r, body, up, actx)
			if err != nil {
				results <- result{a, nil, err}
				return
			}
			resp, err := client.Do(req)
			results <- result{a, resp, err}
		}()
		return true
	}

	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	launch()

	var (
		retryTimer *time.Timer
		retryC     <-chan time.Time
	)
	scheduleRetry := func(d time.Duration) {
		if retryTimer != nil {
			retryTimer.Stop()
		}
		retryTimer = time.NewTimer(d)
		retryC = retryTimer.C
	}
	defer func() {
		if retryTimer != nil {
			retryTimer.Stop()
		}
	}()

	var timer *time.Timer
	var hedgeC <-chan time.Time
	if cfg.hedgeAfter > 0 {
		timer = time.NewTimer(cfg.hedgeAfter)
		defer timer.Stop()
		hedgeC = timer.C
	}

	for {
		select {
		case res := <-results:
			if res.err != nil {
				if errors.Is(res.err, context.Canceled) || errors.Is(rootCtx.Err(), context.Canceled) {
					log.Printf("[%s] attempt %d canceled (client gone)", reqID, res.attempt)
					return
				}
				lastErr = res.err.Error()
				if nextAttempt >= cfg.maxAttempts {
					log.Printf("[%s] attempt %d failed: %v （已用尽尝试次数）", reqID, res.attempt, res.err)
					fail(w, reqID, start, lastErr, &rec)
					return
				}
				d := backoff(cfg, res.attempt)
				log.Printf("[%s] attempt %d failed: %v → %s 后重试", reqID, res.attempt, res.err, d)
				scheduleRetry(d)
				continue
			}

			if shouldRetryStatus(res.resp.StatusCode) {
				atomic.AddInt64(&st.upstream5, 1)
				snippet, _ := io.ReadAll(io.LimitReader(res.resp.Body, 2048))
				_ = res.resp.Body.Close()
				lastErr = fmt.Sprintf("upstream %d: %s", res.resp.StatusCode, truncate(string(snippet), 300))
				log.Printf("[%s] attempt %d got %s", reqID, res.attempt, lastErr)
				if nextAttempt >= cfg.maxAttempts {
					copyHeaders(w.Header(), res.resp.Header)
					w.Header().Set("X-Proxy-Attempt", strconv.Itoa(res.attempt))
					w.WriteHeader(res.resp.StatusCode)
					_, _ = w.Write(snippet)
					st.failed.Add(1)
					uiHub.broadcast("usage", usageStore.Summary())
					rec.Attempt = res.attempt
					rec.Status = res.resp.StatusCode
					rec.Note = "upstream 5xx (耗尽尝试)"
					rec.Error = lastErr
					return
				}
				scheduleRetry(backoff(cfg, res.attempt))
				continue
			}

			if retryTimer != nil {
				retryTimer.Stop()
			}
			// 拿到有效响应：取消同批次其它尝试，开始流式透传
			for a, c := range cancels {
				if a != res.attempt {
					c()
				}
			}
			copyHeaders(w.Header(), res.resp.Header)
			w.Header().Set("X-Proxy-Attempt", strconv.Itoa(res.attempt))
			w.WriteHeader(res.resp.StatusCode)
			sink := newDumpSink(cfg.dumpDir, reqID, cfg.dumpMax)
			tap := newTailBuf(256 << 10)
			stopMon := make(chan struct{})
			go monitorProgress(reqID, tap, start, stopMon)
			n, copyErr := streamCopy(w, res.resp.Body, sink, tap)
			close(stopMon)
			_ = res.resp.Body.Close()
			if sink != nil {
				rec.ResponseDump = sink.Close()
			}
			st.succeeded.Add(1)
			inTok, outTok, totTok, hasUsage := parseUsage(tap.buf)
			est := false
			srcTok := "usage"
			// 4xx/5xx 的响应体是错误信息，不是模型输出，不计 token
			if res.resp.StatusCode >= 400 {
				inTok, outTok, totTok, hasUsage = 0, 0, 0, true
			} else if !hasUsage {
				// 本地 BPE 分词（近似 tiktoken），比字节估算准得多
				srcTok = "tiktoken"
				if v, ok := CountRequestTokens(model, body); ok && v > 0 {
					inTok = int64(v)
				} else {
					inTok = estimateTokensFromBytes(body)
					srcTok = "bytes"
				}
				if txt := extractStreamText(tap.buf); txt != "" {
					outTok = int64(CountTokens(model, txt))
				} else {
					outTok = atomic.LoadInt64(&tap.estTok)
					srcTok = "bytes"
				}
				totTok = inTok + outTok
				est = srcTok != "tiktoken"
			} else {
				srcTok = "usage"
			}
			cost := prices.cost(model, inTok, outTok)
			durMs := time.Since(start).Milliseconds()
			tps := 0.0
			if outTok > 0 && durMs > 0 {
				tps = float64(outTok) / (float64(durMs) / 1000.0)
			}
			ttftMs := int64(0)
			if fa := atomic.LoadInt64(&tap.firstAt); fa > 0 {
				ttftMs = (fa - start.UnixNano()) / 1e6
			}
			genMs := durMs - ttftMs
			genTps := 0.0
			if outTok > 0 && genMs > 0 {
				genTps = float64(outTok) / (float64(genMs) / 1000.0)
			}
			rec.TTFTMs = ttftMs
			rec.GenTPS = genTps
			rec.Cost = cost
			rec.TokenSrc = srcTok
			rec.CachedTokens = lastCached
			rec.ReasonTokens = lastReason
			rec.Model = model
			rec.InTokens = inTok
			rec.OutTokens = outTok
			rec.TPS = tps
			rec.TokensEst = est
			usageStore.Add(UsageRecord{
				Ts: time.Now().Unix(), ID: reqID, Path: r.URL.Path, Model: model,
				Status: res.resp.StatusCode, In: inTok, Out: outTok, Total: totTok,
				DurMs: durMs, TPS: tps, Est: est, Retries: res.attempt - 1,
				TTFTMs: ttftMs, GenTPS: genTps, Cached: lastCached, Reason: lastReason,
				Cost: cost, Src: srcTok,
			})
			rec.Attempt = res.attempt
			rec.Status = res.resp.StatusCode
			rec.Bytes = n
			if copyErr != nil {
				rec.Note = "client gone"
				rec.Error = copyErr.Error()
			} else if hedgedAt > 0 {
				rec.Note = fmt.Sprintf("ok · hedge@#%d", hedgedAt)
			} else {
				rec.Note = "ok"
			}
			log.Printf("[%s] ok attempt=%d status=%d bytes=%d dur=%s copyErr=%v",
				reqID, res.attempt, res.resp.StatusCode, n, time.Since(start).Round(time.Millisecond), copyErr)
			return

		case <-retryC:
			retryC = nil
			if rootCtx.Err() != nil {
				return
			}
			if launch() {
				log.Printf("[%s] 退避结束，发起 attempt %d/%d", reqID, nextAttempt, cfg.maxAttempts)
			}

		case <-hedgeC:
			if rootCtx.Err() != nil {
				return
			}
			if launch() {
				hedgedAt = nextAttempt
				log.Printf("[%s] hedge: no headers after %s, fired parallel attempt %d", reqID, cfg.hedgeAfter, nextAttempt)
			}
			if nextAttempt >= cfg.maxAttempts {
				hedgeC = nil
			} else {
				timer.Reset(cfg.hedgeAfter)
			}

		case <-rootCtx.Done():
			log.Printf("[%s] client disconnected, aborting in-flight attempts", reqID)
			rec.Note = "client gone"
			return
		}
	}
}

func fail(w http.ResponseWriter, reqID string, start time.Time, lastErr string, rec *RequestRecord) {
	rec.Status = http.StatusBadGateway
	rec.Note = "gave up"
	rec.Error = lastErr
	st.failed.Add(1)
	log.Printf("[%s] gave up in %s: %s", reqID, time.Since(start).Round(time.Millisecond), lastErr)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    "proxy_upstream_unavailable",
			"message": "upstream failed after retries: " + lastErr,
		},
	})
}

// streamCopy 边读边写，每块都 flush，保证 SSE 实时到达客户端。
func streamCopy(w http.ResponseWriter, src io.Reader, sink *dumpSink, tap *tailBuf) (int64, error) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	var total int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if sink != nil {
				_, _ = sink.Write(buf[:n])
			}
			if tap != nil {
				_, _ = tap.Write(buf[:n])
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, nil
			}
			return total, rerr
		}
	}
}

func buildRequest(orig *http.Request, body []byte, upstream string, ctx context.Context) (*http.Request, error) {
	target := upstream + orig.URL.Path
	if orig.URL.RawQuery != "" {
		target += "?" + orig.URL.RawQuery
	}
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, orig.Method, target, rdr)
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, orig.Header)
	req.Header.Del("Accept-Encoding")
	req.ContentLength = int64(len(body))
	return req, nil
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func shouldRetryStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code >= 500
}

func backoff(cfg config, n int) time.Duration {
	d := cfg.baseDelay
	for i := 1; i < n; i++ {
		d *= 2
		if d >= cfg.maxDelay {
			d = cfg.maxDelay
			break
		}
	}
	jitter := time.Duration(float64(d) * 0.25 * (rand.Float64()*2 - 1))
	d += jitter
	if d < 100*time.Millisecond {
		d = 100 * time.Millisecond
	}
	return d
}

func requestID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" {
		return v
	}
	return strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 16)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// findConfigFile 依次在可执行文件目录、当前目录查找 relay-retry-proxy.env
func findConfigFile() string {
	names := []string{"relay-retry-proxy.env"}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, n := range names {
			p := filepath.Join(dir, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	for _, n := range names {
		if st, err := os.Stat(n); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(n)
			return abs
		}
	}
	return ""
}

// loadEnvFile 解析 KEY=VALUE 形式的配置，忽略 # 注释与空行
func loadEnvFile(path string) map[string]string {
	kv := map[string]string{}
	if path == "" {
		return kv
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[config] 读取失败 %s: %v", path, err)
		return kv
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		kv[strings.ToUpper(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return kv
}

// monitorProgress 每 700ms 把在途请求的估算速度推给 WebUI，实现"实时 tok/s"。
func monitorProgress(reqID string, tap *tailBuf, start time.Time, stop <-chan struct{}) {
	tk := time.NewTicker(700 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			uiHub.broadcast("progress", map[string]any{
				"id": reqID, "done": true, "elapsed_ms": time.Since(start).Milliseconds(),
				"bytes": atomic.LoadInt64(&tap.total),
			})
			return
		case <-tk.C:
			elapsed := time.Since(start).Milliseconds()
			tok := atomic.LoadInt64(&tap.estTok)
			tps := 0.0
			if elapsed > 0 {
				tps = float64(tok) / (float64(elapsed) / 1000.0)
			}
			uiHub.broadcast("progress", map[string]any{
				"id": reqID, "done": false, "elapsed_ms": elapsed,
				"bytes": atomic.LoadInt64(&tap.total), "tokens": tok, "tps": tps,
			})
		}
	}
}

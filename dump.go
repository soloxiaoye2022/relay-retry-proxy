package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// dumpSink 把响应体尾部（最多 limit 字节）落盘，用于事后判断
// "是模型提前结束" 还是 "客户端/代理截断"。
type dumpSink struct {
	f     *os.File
	path  string
	limit int64
	wrote int64
}

func newDumpFile(dir, reqID, suffix string) (*os.File, string, error) {
	if dir == "" {
		return nil, "", nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	name := fmt.Sprintf("%s-%s-%s", time.Now().Format("20060102-150405.000"), reqID, suffix)
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

func newDumpSink(dir, reqID string, limit int64) *dumpSink {
	if dir == "" || limit <= 0 {
		return nil
	}
	f, path, err := newDumpFile(dir, reqID, "resp.txt")
	if err != nil {
		return nil
	}
	return &dumpSink{f: f, path: path, limit: limit}
}

func (d *dumpSink) Write(p []byte) (int, error) {
	if d == nil || d.f == nil {
		return len(p), nil
	}
	room := d.limit - atomic.LoadInt64(&d.wrote)
	if room <= 0 {
		return len(p), nil
	}
	n := int64(len(p))
	if n > room {
		p = p[:room]
		n = room
	}
	if _, err := d.f.Write(p); err != nil {
		return len(p), nil
	}
	atomic.AddInt64(&d.wrote, n)
	return len(p), nil
}

func (d *dumpSink) Close() string {
	if d == nil || d.f == nil {
		return ""
	}
	if _, err := d.f.WriteString(fmt.Sprintf("\n\n=== dumped %d bytes, capped at %d ===\n", atomic.LoadInt64(&d.wrote), d.limit)); err == nil {
		_ = d.f.Sync()
	}
	_ = d.f.Close()
	return d.path
}

// writeRequestDump 记录入站请求：方法、路径、脱敏后的头、请求体。
// isSensitiveHeader 名字里出现这些词的头部一律打码。
func isSensitiveHeader(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"authorization", "token", "auth", "api-key", "apikey", "key", "cookie", "secret", "session", "credential"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

func writeRequestDump(dir, reqID string, method, path string, header map[string][]string, body []byte, limit int64, withBody bool) string {
	if dir == "" {
		return ""
	}
	f, p, err := newDumpFile(dir, reqID, "req.txt")
	if err != nil {
		return ""
	}
	defer f.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", method, path)
	fmt.Fprintf(&b, "time: %s\n", time.Now().Format(time.RFC3339Nano))
	for k, vs := range header {
		if isSensitiveHeader(k) {
			b.WriteString(k + ": ***REDACTED***\n")
			continue
		}
		// 客户端真实 IP 也算隐私，打码
		if strings.EqualFold(k, "X-Forwarded-For") || strings.EqualFold(k, "X-Real-IP") {
			b.WriteString(k + ": ***REDACTED***\n")
			continue
		}
		for _, v := range vs {
			b.WriteString(k + ": " + v + "\n")
		}
	}
	if !withBody {
		fmt.Fprintf(&b, "\n--- body omitted (%d bytes, 未落盘) ---\n", len(body))
		_, _ = f.WriteString(b.String())
		return p
	}
	b.WriteString("\n--- body ---\n")
	if int64(len(body)) > limit {
		b.Write(body[:limit])
		fmt.Fprintf(&b, "\n... (truncated, total %d bytes)\n", len(body))
	} else {
		b.Write(body)
	}
	_, _ = f.WriteString(b.String())
	return p
}

// pruneDumps 只保留每个后缀最新的 keep 个文件。
func pruneDumps(dir string, keep int) {
	if dir == "" || keep <= 0 {
		return
	}
	for _, suffix := range []string{"req.txt", "resp.txt"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		var files []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
				files = append(files, e.Name())
			}
		}
		if len(files) <= keep {
			continue
		}
		sort.Strings(files) // 文件名以时间戳开头，字典序即时间序
		for _, name := range files[:len(files)-keep] {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

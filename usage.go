package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- 用量记录与持久化 ----------

// UsageRecord 是一条持久化的用量记录（写入 usage-YYYY-MM-DD.jsonl）。
type UsageRecord struct {
	Ts      int64   `json:"ts"`   // 完成时刻 unix 秒
	ID      string  `json:"id"`   // request id
	Path    string  `json:"path"` // /v1/responses 等
	Model   string  `json:"model,omitempty"`
	Status  int     `json:"status"`
	In      int64   `json:"in"`  // 输入 token
	Out     int64   `json:"out"` // 输出 token
	Total   int64   `json:"total"`
	DurMs   int64   `json:"dur_ms"`
	TPS     float64 `json:"tps"`
	Est     bool    `json:"est,omitempty"` // true = 上游没给 usage，按字节估算
	Retries int     `json:"retries,omitempty"`
	TTFTMs  int64   `json:"ttft_ms,omitempty"` // 首字节/首个 token 时间
	GenTPS  float64 `json:"gen_tps,omitempty"` // 纯生成阶段 tok/s（扣掉 TTFT）
	Cached  int64   `json:"cached_tokens,omitempty"`
	Reason  int64   `json:"reasoning_tokens,omitempty"`
	Cost    float64 `json:"cost,omitempty"`
	Src     string  `json:"src,omitempty"` // usage / tiktoken / bytes
}

type bucket struct {
	In      int64   `json:"in"`
	Out     int64   `json:"out"`
	Total   int64   `json:"total"`
	Reqs    int64   `json:"reqs"`
	DurMs   int64   `json:"dur_ms"`
	Errors  int64   `json:"errors"`
	EstReqs int64   `json:"est_reqs"`
	TTFTSum int64   `json:"ttft_ms_sum"`
	GenOut  int64   `json:"gen_out"` // 用于算生成阶段均速
	GenMs   int64   `json:"gen_ms"`
	Cost    float64 `json:"cost"`
	Cached  int64   `json:"cached"`
	Reason  int64   `json:"reasoning"`
}

func (b *bucket) add(r UsageRecord) {
	b.In += r.In
	b.Out += r.Out
	b.Total += r.Total
	b.Reqs++
	b.DurMs += r.DurMs
	if r.Status >= 400 {
		b.Errors++
	}
	if r.Est {
		b.EstReqs++
	}
	b.Cost += r.Cost
	b.Cached += r.Cached
	b.Reason += r.Reason
	if r.TTFTMs > 0 {
		b.TTFTSum += r.TTFTMs
	}
	if r.GenTPS > 0 && r.Out > 0 {
		b.GenOut += r.Out
		b.GenMs += int64(float64(r.Out) / r.GenTPS * 1000)
	}
}

type hourSlot struct {
	Hour string `json:"hour"`
	In   int64  `json:"in"`
	Out  int64  `json:"out"`
}

// UsageStore 负责落盘与内存聚合（重启后从文件恢复）。
type UsageStore struct {
	mu      sync.Mutex
	dir     string
	today   string
	todayB  bucket
	totalB  bucket
	byModel map[string]*bucket
	hourly  map[string]*[2]int64 // "20060102-15" -> {in, out}
	curFile *os.File
}

func newUsageStore(dir string) *UsageStore {
	s := &UsageStore{
		dir:     dir,
		byModel: map[string]*bucket{},
		hourly:  map[string]*[2]int64{},
	}
	if dir != "" {
		_ = os.MkdirAll(dir, 0o700)
		s.reload()
	}
	s.today = time.Now().Format("2006-01-02")
	return s
}

// reload 扫最近 30 天的 jsonl 重建聚合。
func (s *UsageStore) reload() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "usage-") && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) > 30 {
		names = names[len(names)-30:]
	}
	today := time.Now().Format("2006-01-02")
	for _, name := range names {
		f, err := os.Open(filepath.Join(s.dir, name))
		if err != nil {
			continue
		}
		dec := json.NewDecoder(f)
		for {
			var r UsageRecord
			if err := dec.Decode(&r); err != nil {
				break
			}
			s.applyLocked(r, today)
		}
		_ = f.Close()
	}
}

func (s *UsageStore) applyLocked(r UsageRecord, today string) {
	// 历史记录可能没有 cost 字段（早期版本写入），用当前价格补算
	if r.Cost == 0 && (r.In > 0 || r.Out > 0) {
		r.Cost = prices.cost(r.Model, r.In, r.Out)
	}
	s.totalB.add(r)
	if r.Ts > 0 {
		day := time.Unix(r.Ts, 0).Format("2006-01-02")
		if day == today {
			s.todayB.add(r)
		}
		hourKey := time.Unix(r.Ts, 0).Format("20060102-15")
		slot, ok := s.hourly[hourKey]
		if !ok {
			slot = &[2]int64{}
			s.hourly[hourKey] = slot
		}
		slot[0] += r.In
		slot[1] += r.Out
	}
	key := r.Model
	if key == "" {
		key = "unknown"
	}
	b, ok := s.byModel[key]
	if !ok {
		b = &bucket{}
		s.byModel[key] = b
	}
	b.add(r)
}

// Add 落盘 + 更新聚合。落盘失败不影响代理主链路。
func (s *UsageStore) Add(r UsageRecord) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		day := time.Now().Format("2006-01-02")
		if day != s.today || s.curFile == nil {
			if s.curFile != nil {
				_ = s.curFile.Close()
			}
			path := filepath.Join(s.dir, "usage-"+day+".jsonl")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err == nil {
				s.curFile = f
				s.today = day
			}
		}
		if s.curFile != nil {
			if b, err := json.Marshal(r); err == nil {
				_, _ = s.curFile.Write(append(b, '\n'))
			}
		}
	}
	s.applyLocked(r, time.Now().Format("2006-01-02"))
}

func (b bucket) ttft() float64 {
	if b.Reqs == 0 {
		return 0
	}
	return float64(b.TTFTSum) / float64(b.Reqs)
}

func (b bucket) genTps() float64 {
	if b.GenMs <= 0 {
		return 0
	}
	return float64(b.GenOut) / (float64(b.GenMs) / 1000.0)
}

func (b bucket) tps() float64 {
	if b.DurMs <= 0 {
		return 0
	}
	return float64(b.Out) / (float64(b.DurMs) / 1000.0)
}

type modelStat struct {
	Model string  `json:"model"`
	In    int64   `json:"in"`
	Out   int64   `json:"out"`
	Reqs  int64   `json:"reqs"`
	TPS   float64 `json:"tps"`
	Cost  float64 `json:"cost"`
}

// Summary 返回给 WebUI 的用量视图。
func (s *UsageStore) Summary() map[string]any {
	if s == nil {
		return map[string]any{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	today := time.Now().Format("2006-01-02")

	// 最近 24 小时分桶
	now := time.Now()
	series := make([]hourSlot, 0, 24)
	for i := 23; i >= 0; i-- {
		t := now.Add(-time.Duration(i) * time.Hour)
		key := t.Format("20060102-15")
		slot := s.hourly[key]
		hs := hourSlot{Hour: t.Format("15:00")}
		if slot != nil {
			hs.In, hs.Out = slot[0], slot[1]
		}
		series = append(series, hs)
	}

	models := make([]modelStat, 0, len(s.byModel))
	for m, b := range s.byModel {
		models = append(models, modelStat{Model: m, In: b.In, Out: b.Out, Reqs: b.Reqs, TPS: b.tps(), Cost: b.Cost})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Out > models[j].Out })
	if len(models) > 12 {
		models = models[:12]
	}

	return map[string]any{
		"today": map[string]any{
			"in": s.todayB.In, "out": s.todayB.Out, "total": s.todayB.Total,
			"reqs": s.todayB.Reqs, "errors": s.todayB.Errors,
			"tps": s.todayB.tps(), "est_reqs": s.todayB.EstReqs,
			"gen_tps": s.todayB.genTps(), "ttft_ms": s.todayB.ttft(),
			"cached": s.todayB.Cached, "reasoning": s.todayB.Reason,
			"cost": s.todayB.Cost, "currency": prices.currencyCode(),
		},
		"total": map[string]any{
			"in": s.totalB.In, "out": s.totalB.Out, "total": s.totalB.Total,
			"reqs": s.totalB.Reqs, "errors": s.totalB.Errors,
			"tps": s.totalB.tps(), "est_reqs": s.totalB.EstReqs,
			"gen_tps": s.totalB.genTps(), "ttft_ms": s.totalB.ttft(),
			"cached": s.totalB.Cached, "reasoning": s.totalB.Reason,
			"cost": s.totalB.Cost, "currency": prices.currencyCode(),
		},
		"models":  models,
		"hourly":  series,
		"day":     today,
		"storage": s.dir,
	}
}

// Close 关闭当前文件句柄。
func (s *UsageStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.curFile != nil {
		_ = s.curFile.Sync()
		_ = s.curFile.Close()
		s.curFile = nil
	}
}

// ---------- 从响应流里抽取 usage ----------

// tailBuf 只保留最后 N 字节用于找 usage，同时累计总字节、首个数据到达时刻与估算 token 数。
type tailBuf struct {
	buf     []byte
	max     int
	total   int64
	estTok  int64
	firstAt int64 // unix nano，0 表示还没收到数据
}

func newTailBuf(max int) *tailBuf { return &tailBuf{max: max} }

func (t *tailBuf) Write(p []byte) (int, error) {
	if atomic.LoadInt64(&t.firstAt) == 0 {
		atomic.StoreInt64(&t.firstAt, time.Now().UnixNano())
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	atomic.AddInt64(&t.total, int64(len(p)))
	atomic.AddInt64(&t.estTok, estimateTokensFromBytes(p))
	return len(p), nil
}

// parseUsage 在响应尾部里找最后一个可解析的 usage 对象。
// 兼容 chat/completions 的 prompt_tokens/completion_tokens 与 responses 的 input_tokens/output_tokens。
var lastCached, lastReason int64

func parseUsage(b []byte) (in, out, total int64, ok bool) {
	lastCached, lastReason = 0, 0
	for i := len(b) - 1; i >= 0; i-- {
		if i > 0 && !(b[i] == '"' && i+7 < len(b) && string(b[i:i+7]) == `"usage"`) {
			continue
		}
		if i+7 >= len(b) {
			continue
		}
		j := i + 7
		for j < len(b) && (b[j] == ':' || b[j] == ' ' || b[j] == '\t') {
			j++
		}
		if j >= len(b) || b[j] != '{' {
			continue
		}
		var u map[string]any
		dec := json.NewDecoder(strings.NewReader(string(b[j:])))
		if err := dec.Decode(&u); err != nil {
			continue
		}
		pick := func(keys ...string) int64 {
			for _, k := range keys {
				if v, exists := u[k]; exists {
					if f, isNum := v.(float64); isNum {
						return int64(f)
					}
				}
			}
			return 0
		}
		in = pick("input_tokens", "prompt_tokens")
		out = pick("output_tokens", "completion_tokens")
		total = pick("total_tokens")
		var cached, reasoning int64
		if pd, ok := u["prompt_tokens_details"].(map[string]any); ok {
			if v, ok := pd["cached_tokens"].(float64); ok {
				cached = int64(v)
			}
		}
		if cd, ok := u["completion_tokens_details"].(map[string]any); ok {
			if v, ok := cd["reasoning_tokens"].(float64); ok {
				reasoning = int64(v)
			}
		}
		if in == 0 && out == 0 {
			continue
		}
		if total == 0 {
			total = in + out
		}
		lastCached, lastReason = cached, reasoning
		return in, out, total, true
	}
	return 0, 0, 0, false
}

// estimateTokens 上游没给 usage 时的兜底估算：
// 先用字节数算一个上界，再按"非 ASCII 字符单独计 1 token"的思路修正。
func estimateTokensFromBytes(b []byte) int64 {
	var ascii, wide int64
	for _, r := range string(b) {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	// ASCII 约 4 字节/token；CJK 等宽字符约 1 字/token
	return ascii/4 + wide
}

// extractModel 从请求体里取 model 字段。
func extractModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.Model
}

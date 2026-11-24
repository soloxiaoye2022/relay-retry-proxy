// reasoning_guard.go：识别「整轮烧在思考上、没有正文」的空响应并触发重试。
//
// 背景：DeepSeek 系模型偶尔把整轮预算全花在 reasoning 上，思考完直接结束，
// 正文一个字都不吐。如果照常按 SSE 结束返回，客户端会认为这一轮正常完成了
// （只是模型没说话），于是停在那里等用户再输入——用户看到的就是「转了半天
// 什么都没出」。
//
// 这里的做法是「先看后交」：在写响应头、开始往客户端吐字节之前，先读一小段
// 上游数据做判定。
//   - 见到正文（content / tool_calls / 结构化输出）→ 正常流式透传，零延迟损失
//   - 只见到 reasoning 且流已结束 → 判定为 empty_reasoning_only，交给重试循环
//   - 超时仍未定性 → 按正常处理（不拖延用户的正常请求）
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
)

// reasoningGuardConfig 控制前瞻窗口的行为。
type reasoningGuardConfig struct {
	Enabled  bool          // 是否启用
	MaxBytes int           // 前瞻最多缓存多少字节
	MaxWait  time.Duration // 最长等多久必须做决定
}

func defaultReasoningGuard() reasoningGuardConfig {
	return reasoningGuardConfig{
		Enabled:  true,
		MaxBytes: 256 << 10,
		MaxWait:  3 * time.Second,
	}
}

// peekReader 是一个可以「先读后放」的 reader：前瞻阶段把读到的字节缓存下来，
// 决定放行之后再按原顺序吐出去。
type peekReader struct {
	src    io.Reader
	mu     sync.Mutex
	buf    []byte
	eof    bool
	err    error
	peeked bool
}

func newPeekReader(src io.Reader) *peekReader {
	return &peekReader{src: src}
}

// Read 先从缓存吐，缓存空了再读源。
func (p *peekReader) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) > 0 {
		n := copy(b, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}
	if p.err != nil {
		return 0, p.err
	}
	if p.eof {
		return 0, io.EOF
	}
	return p.src.Read(b)
}

// peekOnce 读一批进缓存，返回是否已到流末尾。
func (p *peekReader) peekOnce(size int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.eof || p.err != nil {
		return true
	}
	if len(p.buf) >= size {
		return false
	}
	tmp := make([]byte, size)
	n, err := p.src.Read(tmp)
	if n > 0 {
		p.buf = append(p.buf, tmp[:n]...)
	}
	if err != nil {
		if err == io.EOF {
			p.eof = true
		} else {
			p.err = err
		}
		return true
	}
	return false
}

// buffered 返回当前已缓存内容（副本）。
func (p *peekReader) buffered() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.buf...)
}

// verdict 是前瞻判定的结果。
type verdict int

const (
	verdictContent verdict = iota // 有正文，正常放行
	verdictEmpty                  // 只有 reasoning，没有任何正文 → 可重试
	verdictUnknown                // 没看清（超时/非 SSE/截断），按正常放行
)

// sseSignals 汇总一批 SSE 字节里观察到的信号。
type sseSignals struct {
	sawContent   bool // 正文
	sawReasoning bool // reasoning
	sawToolCall  bool // 工具调用
	sawDone      bool // 收到 [DONE] 或明确的结束事件
	sawFinish    bool // finish_reason 非空
	finishLength bool // 因 max_tokens 截断
	sawError     bool // 流内 error 事件
}

// analyzeSSE 逐行扫描 SSE，提取判定所需的信号。
func analyzeSSE(raw []byte) sseSignals {
	var s sseSignals
	text := string(raw)
	if text == "" {
		return s
	}
	// 不是 SSE 的（例如普通 JSON 错误体），交给调用方按 unknown 处理
	if !strings.Contains(text, "data:") && !strings.HasPrefix(strings.TrimSpace(text), "{") {
		return s
	}

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		payload := line
		if strings.HasPrefix(line, "event:") {
			if strings.Contains(line, "error") {
				s.sawError = true
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if payload == "[DONE]" {
			s.sawDone = true
			continue
		}
		if !strings.HasPrefix(payload, "{") {
			continue
		}

		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}

		// 流内错误
		if _, ok := obj["error"]; ok {
			s.sawError = true
		}
		if t, _ := obj["type"].(string); t == "error" || strings.HasSuffix(t, ".failed") {
			s.sawError = true
		}

		// OpenAI chat 形态
		if choices, ok := obj["choices"].([]any); ok {
			for _, c := range choices {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
					s.sawFinish = true
					if fr == "length" {
						s.finishLength = true
					}
				}
				delta, ok := cm["delta"].(map[string]any)
				if !ok {
					if msg, ok2 := cm["message"].(map[string]any); ok2 {
						delta = msg
					}
				}
				if delta == nil {
					continue
				}
				if hasText(delta["content"]) {
					s.sawContent = true
				}
				if hasText(delta["reasoning_content"]) || hasText(delta["reasoning"]) {
					s.sawReasoning = true
				}
				if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
					s.sawToolCall = true
				}
				if _, ok := delta["function_call"]; ok {
					s.sawToolCall = true
				}
			}
		}

		// Responses API 形态
		if t, _ := obj["type"].(string); t != "" {
			// 顺序要紧：reasoning_summary_text.delta 里也含 "text.delta"，
			// 必须先判 reasoning，否则思考流会被误当成正文。
			switch {
			case strings.Contains(t, "reasoning"):
				s.sawReasoning = true
			case strings.Contains(t, "output_text.delta"):
				if hasText(obj["delta"]) {
					s.sawContent = true
				}
			case strings.HasSuffix(t, "text.delta"):
				if hasText(obj["delta"]) {
					s.sawContent = true
				}
			case strings.Contains(t, "function_call_arguments.delta"),
				strings.Contains(t, "output_item.added"):
				if item, ok := obj["item"].(map[string]any); ok {
					if it, _ := item["type"].(string); it == "function_call" {
						s.sawToolCall = true
					}
				}
				s.sawToolCall = true
			}
			if strings.HasSuffix(t, ".completed") {
				s.sawDone = true
			}
			if obj["status"] == "incomplete" {
				if d, ok := obj["incomplete_details"].(map[string]any); ok {
					if r, _ := d["reason"].(string); r == "max_output_tokens" {
						s.finishLength = true
					}
				}
			}
		}
	}
	return s
}

// hasText 判断一个 JSON 值是否含有非空正文。
func hasText(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(x) != ""
	case []any:
		for _, it := range x {
			if hasText(it) {
				return true
			}
			if m, ok := it.(map[string]any); ok {
				if t, _ := m["type"].(string); t == "text" && hasText(m["text"]) {
					return true
				}
			}
		}
	}
	return false
}

// decide 根据已观察到的信号给出判定。
// decided=false 表示还需要继续读。
func decide(s sseSignals, atEOF bool) (verdict, bool) {
	// 任何正文信号都是终局：正常放行
	if s.sawContent || s.sawToolCall {
		return verdictContent, true
	}
	// 流内错误不在这里处理（交给上层按错误路径走）
	if s.sawError {
		return verdictUnknown, true
	}
	// 被 max_tokens 截断，不算「空想」
	if s.finishLength {
		return verdictUnknown, true
	}
	if atEOF {
		// 流已经结束：有 reasoning 没正文 = 空想
		if s.sawReasoning {
			return verdictEmpty, true
		}
		return verdictUnknown, true
	}
	if s.sawDone && s.sawFinish {
		if s.sawReasoning {
			return verdictEmpty, true
		}
		return verdictUnknown, true
	}
	return verdictUnknown, false
}

// peekVerdict 在前瞻窗口内反复读，直到能下结论或用尽预算。
// 返回判定结果与已缓存字节（供放行后重放）。
func peekVerdict(p *peekReader, cfg reasoningGuardConfig) (verdict, []byte) {
	if !cfg.Enabled {
		return verdictUnknown, nil
	}
	deadline := time.Now().Add(cfg.MaxWait)
	for {
		atEOF := p.peekOnce(4096)
		buf := p.buffered()
		var s sseSignals
		if len(buf) > 0 {
			s = analyzeSSE(buf)
		}
		if v, ok := decide(s, atEOF); ok {
			return v, buf
		}
		if len(buf) >= cfg.MaxBytes || atEOF {
			return verdictUnknown, buf
		}
		if time.Now().After(deadline) {
			// 超时：不拖延用户，按正常放行
			return verdictContent, buf
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// replayReader 把已缓存的字节先吐出去，再接原始 reader。
func replayReader(cached []byte, rest io.Reader) io.Reader {
	if len(cached) == 0 {
		return rest
	}
	return io.MultiReader(bytes.NewReader(cached), rest)
}

package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

const sseReasonOnly = `data: {"choices":[{"delta":{"reasoning_content":"let me think about this"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

const sseWithContent = `data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}

data: {"choices":[{"delta":{"content":"hello"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

const sseToolCall = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"ls","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

const sseLength = `data: {"choices":[{"delta":{"reasoning_content":"thinking a lot"}}]}

data: {"choices":[{"delta":{},"finish_reason":"length"}]}

data: [DONE]

`

const sseResponsesEmpty = `event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","delta":"pondering"}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed"}}

`

func TestAnalyzeSSEReasonOnly(t *testing.T) {
	s := analyzeSSE([]byte(sseReasonOnly))
	if !s.sawReasoning {
		t.Error("should detect reasoning")
	}
	if s.sawContent {
		t.Error("should not detect content")
	}
	if !s.sawDone {
		t.Error("should detect [DONE]")
	}
	if v, ok := decide(s, true); !ok || v != verdictEmpty {
		t.Errorf("reason-only stream should be verdictEmpty, got %v ok=%v", v, ok)
	}
}

func TestAnalyzeSSEWithContent(t *testing.T) {
	s := analyzeSSE([]byte(sseWithContent))
	if !s.sawContent {
		t.Error("should detect content")
	}
	if v, ok := decide(s, true); !ok || v != verdictContent {
		t.Errorf("stream with content should be verdictContent, got %v", v)
	}
}

func TestAnalyzeSSEToolCall(t *testing.T) {
	s := analyzeSSE([]byte(sseToolCall))
	if !s.sawToolCall {
		t.Error("should detect tool call")
	}
	if v, _ := decide(s, true); v != verdictContent {
		t.Errorf("tool call stream should be verdictContent, got %v", v)
	}
}

func TestAnalyzeSSELengthNotEmpty(t *testing.T) {
	s := analyzeSSE([]byte(sseLength))
	if !s.finishLength {
		t.Error("should detect length finish")
	}
	if v, _ := decide(s, true); v == verdictEmpty {
		t.Error("max_tokens truncation must not count as empty reasoning")
	}
}

func TestAnalyzeSSEResponsesEmpty(t *testing.T) {
	s := analyzeSSE([]byte(sseResponsesEmpty))
	if !s.sawReasoning {
		t.Error("should detect responses reasoning delta")
	}
	if s.sawContent {
		t.Error("should not detect content")
	}
}

func TestDecideNeedsMoreData(t *testing.T) {
	partial := `data: {"choices":[{"delta":{"reasoning_content":"hmm"}}]}`
	s := analyzeSSE([]byte(partial))
	if _, ok := decide(s, false); ok {
		t.Error("partial stream should not be decided yet")
	}
}

func TestPeekVerdictReasonOnly(t *testing.T) {
	p := newPeekReader(strings.NewReader(sseReasonOnly))
	v, cached := peekVerdict(p, reasoningGuardConfig{Enabled: true, MaxBytes: 64 << 10, MaxWait: time.Second})
	if v != verdictEmpty {
		t.Errorf("expected verdictEmpty, got %v", v)
	}
	if len(cached) == 0 {
		t.Error("cached bytes should be returned")
	}
}

func TestPeekVerdictContent(t *testing.T) {
	p := newPeekReader(strings.NewReader(sseWithContent))
	v, _ := peekVerdict(p, reasoningGuardConfig{Enabled: true, MaxBytes: 64 << 10, MaxWait: time.Second})
	if v != verdictContent {
		t.Errorf("expected verdictContent, got %v", v)
	}
}

func TestPeekVerdictDisabled(t *testing.T) {
	p := newPeekReader(strings.NewReader(sseReasonOnly))
	v, _ := peekVerdict(p, reasoningGuardConfig{Enabled: false})
	if v != verdictUnknown {
		t.Errorf("disabled guard should return unknown, got %v", v)
	}
}

func TestPeekReaderReplay(t *testing.T) {
	src := "abcdefghij"
	p := newPeekReader(strings.NewReader(src))
	p.peekOnce(4)
	got := make([]byte, len(src))
	n, err := io.ReadFull(p, got)
	if err != nil && err.Error() != "unexpected EOF" {
		t.Fatalf("read: %v", err)
	}
	if string(got[:n]) != src {
		t.Errorf("replay mismatch: %q != %q", string(got[:n]), src)
	}
}

func TestReplayReader(t *testing.T) {
	r := replayReader([]byte("abc"), strings.NewReader("def"))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "abcdef" {
		t.Errorf("got %q, want %q", string(got), "abcdef")
	}
}

func TestHasText(t *testing.T) {
	cases := []struct {
		v    any
		want bool
	}{
		{"hi", true},
		{"  ", false},
		{nil, false},
		{[]any{map[string]any{"type": "text", "text": "yo"}}, true},
		{[]any{map[string]any{"type": "text", "text": ""}}, false},
	}
	for _, c := range cases {
		if got := hasText(c.v); got != c.want {
			t.Errorf("hasText(%v) = %v, want %v", c.v, got, c.want)
		}
	}
}

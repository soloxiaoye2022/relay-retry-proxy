// sse_filter.go：流式转发时的两处「保命」处理。
//
// 背景（2026-11-24 现场）：Trae 的 custom model 隧道走 WebSocket 到云端，
// 云端在高负载/大帧时会直接用 1001 关连接（日志：server_disconnected），
// 客户端随即取消发往代理的 HTTP 流，整轮对话作废。
//
// 我们改不了云端的策略，但能减轻两条最可疑的诱因：
//  1. 上游在 response.created / response.in_progress 里把整个请求原样回显
//     （实测 54KB + 51KB，包含 tools 和 22 万 token 的 input）。这两个事件对
//     客户端只有 id/status 有用，删掉回显能把单帧压到几百字节。
//  2. 模型思考期间上游可能长时间不吐字节，中间层容易把空闲连接判死。
//     空闲时补一个 SSE 注释帧 `: keepalive`，按 SSE 规范解析器必须忽略。
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// echoDropKeys 是 response.created / response.in_progress 回显里可以安全丢弃的字段。
var echoDropKeys = []string{
	"tools", "tool_choice", "parallel_tool_calls",
	"instructions", "input", "output",
	"reasoning", "text", "truncation",
	"temperature", "top_p", "max_output_tokens", "max_tool_calls",
	"previous_response_id", "store", "background", "include",
	"metadata", "service_tier", "user", "safety_identifier",
	"prompt_cache_key", "conversation",
}

// trimEchoEvent 只在事件是 response.created / response.in_progress 时瘦身回显，
// 其它事件原样返回（字节不动，避免影响 tool_calls 之类对格式敏感的事件）。
func trimEchoEvent(ev []byte) []byte {
	if len(ev) == 0 {
		return ev
	}
	lines := strings.Split(string(ev), "\n")
	eventName := ""
	dataIdx := -1
	for i, ln := range lines {
		ln = strings.TrimSuffix(ln, "\r")
		if strings.HasPrefix(ln, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(ln, "event:"))
		}
		if strings.HasPrefix(ln, "data:") && dataIdx < 0 {
			dataIdx = i
		}
	}
	if dataIdx < 0 || (eventName != "response.created" && eventName != "response.in_progress") {
		return ev
	}

	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(lines[dataIdx], "\r"), "data:"))
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ev
	}
	resp, ok := payload["response"].(map[string]any)
	if !ok {
		return ev
	}
	dropped := false
	for _, k := range echoDropKeys {
		if _, has := resp[k]; has {
			delete(resp, k)
			dropped = true
		}
	}
	if !dropped {
		return ev
	}
	if _, has := resp["output"]; !has {
		resp["output"] = []any{}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ev
	}
	tail := ""
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		tail = "\n"
	}
	lines[dataIdx] = "data: " + string(body)
	_ = tail
	return []byte(strings.Join(lines, "\n"))
}

// sseFilter 负责把字节流切成完整事件、按需瘦身，并保留半截事件等下一次写入。
type sseFilter struct {
	pending []byte
}

// feed 处理一段上游字节，把可以发出去的事件交给 emit。返回是否产生过输出。
func (f *sseFilter) feed(chunk []byte, emit func([]byte)) bool {
	if len(chunk) == 0 {
		return false
	}
	f.pending = append(f.pending, chunk...)
	wrote := false
	for {
		idx := bytes.Index(f.pending, []byte("\n\n"))
		if idx < 0 {
			break
		}
		ev := f.pending[:idx+2]
		f.pending = f.pending[idx+2:]
		if out := trimEchoEvent(ev); len(out) > 0 {
			emit(out)
			wrote = true
		}
	}
	// 单事件过大又不带分隔：攒到 256KB 就强制放行，避免无限缓存
	if len(f.pending) > 256<<10 {
		if out := trimEchoEvent(f.pending); len(out) > 0 {
			emit(out)
			wrote = true
		}
		f.pending = nil
	}
	return wrote
}

// flush 把残留的半截事件发出去（流结束时调用）。
func (f *sseFilter) flush(emit func([]byte)) {
	if len(f.pending) == 0 {
		return
	}
	if out := trimEchoEvent(f.pending); len(out) > 0 {
		emit(out)
	}
	f.pending = nil
}

// keepaliveFrame 是 SSE 注释帧：按规范解析器必须忽略，不会污染数据。
var keepaliveFrame = []byte(": keepalive\n\n")

const keepaliveInterval = 5 * time.Second

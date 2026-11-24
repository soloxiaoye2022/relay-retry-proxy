package main

import (
	"strings"
	"testing"
)

const createdEvent = `event: response.created
data: {"type":"response.created","sequence_number":1,"response":{"id":"resp_1","object":"response","model":"deepseek-v4.1-flash","status":"in_progress","tools":[{"type":"function","function":{"name":"shell"}}],"input":[{"role":"user","content":"hi"}],"output":[]}}` + "\n\n"

const deltaEvent = `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hello"}` + "\n\n"

// created / in_progress 的大回显要被削掉，其余事件必须逐字节保留。
func TestTrimEchoEvent(t *testing.T) {
	out := string(trimEchoEvent([]byte(createdEvent)))
	if strings.Contains(out, `"tools"`) || strings.Contains(out, `"input"`) {
		t.Fatalf("回显没被削掉：%s", out)
	}
	if !strings.Contains(out, `"id":"resp_1"`) || !strings.Contains(out, `"model":"deepseek-v4.1-flash"`) {
		t.Fatalf("关键字段被误删：%s", out)
	}
	if !strings.HasPrefix(out, "event: response.created") {
		t.Fatalf("事件头被破坏：%s", out)
	}

	if got := string(trimEchoEvent([]byte(deltaEvent))); got != deltaEvent {
		t.Fatalf("非目标事件被改动：\n got=%q\nwant=%q", got, deltaEvent)
	}
}

// 事件被拆成任意小段喂进来，输出必须与整体处理一致，且普通事件不改字节。
func TestSSEFilterHandlesSplitChunks(t *testing.T) {
	var out strings.Builder
	var f sseFilter
	emit := func(b []byte) { out.Write(b) }

	stream := createdEvent + deltaEvent
	for i := 0; i < len(stream); i += 7 {
		end := i + 7
		if end > len(stream) {
			end = len(stream)
		}
		f.feed([]byte(stream[i:end]), emit)
	}
	f.flush(emit)

	got := out.String()
	if strings.Contains(got, `"tools"`) {
		t.Fatalf("分片处理后回显仍在：%s", got)
	}
	if !strings.Contains(got, deltaEvent) {
		t.Fatalf("普通事件在分片处理中被破坏：%s", got)
	}
}

// 没有命中瘦身条件时，整条流必须逐字节不变。
func TestSSEFilterPassthrough(t *testing.T) {
	stream := deltaEvent
	var out strings.Builder
	var f sseFilter
	f.feed([]byte(stream), func(b []byte) { out.Write(b) })
	f.flush(func(b []byte) { out.Write(b) })
	if out.String() != stream {
		t.Fatalf("passthrough 被改动：%q", out.String())
	}
}

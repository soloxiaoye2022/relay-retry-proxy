package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

//go:embed tokenizer/cl100k_base.tiktoken tokenizer/o200k_base.tiktoken
var tokenizerFS embed.FS

type bpe struct {
	ranks map[string]int
	cache sync.Map // piece -> token count
}

var (
	bpeOnce sync.Once
	encCl   *bpe
	encO2   *bpe
)

func loadBPE(name string) *bpe {
	f, err := tokenizerFS.Open("tokenizer/" + name)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := &bpe{ranks: make(map[string]int, 200000)}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for sc.Scan() {
		line := sc.Text()
		sp := strings.IndexByte(line, ' ')
		if sp <= 0 {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(line[:sp])
		if err != nil {
			continue
		}
		rank, err := strconv.Atoi(line[sp+1:])
		if err != nil {
			continue
		}
		b.ranks[string(raw)] = rank
	}
	return b
}

func initTokenizers() {
	bpeOnce.Do(func() {
		encCl = loadBPE("cl100k_base.tiktoken")
		encO2 = loadBPE("o200k_base.tiktoken")
	})
}

// pickEncoder 按模型名猜词表：gpt-4o/o1/o3/gpt-5 系用 o200k，其余用 cl100k。
func pickEncoder(model string) *bpe {
	initTokenizers()
	m := strings.ToLower(model)
	for _, k := range []string{"gpt-4o", "gpt-4.1", "gpt-5", "o1", "o3", "o4", "chatgpt-4o"} {
		if strings.Contains(m, k) {
			return encO2
		}
	}
	if encCl != nil {
		return encCl
	}
	return encO2
}

// splitPieces 复刻 tiktoken 的预切分规则（cl100k / o200k 的主干部分）：
//
//  1. 字母串（允许前面带一个非字母数字换行的字符，如空格或标点）
//  2. 1~3 位数字
//  3. 空白 + 标点串（可选前导空格）及其后的换行
//  4. 其余空白串
func splitPieces(s string) []string {
	rs := []rune(s)
	n := len(rs)
	out := make([]string, 0, len(rs)/3+4)
	isSpace := func(r rune) bool { return unicode.IsSpace(r) }
	isLetter := func(r rune) bool { return unicode.IsLetter(r) }
	isDigit := func(r rune) bool { return unicode.IsDigit(r) }
	isPunct := func(r rune) bool { return !isSpace(r) && !isLetter(r) && !isDigit(r) }

	i := 0
	for i < n {
		start := i
		r := rs[i]

		switch {
		case isLetter(r):
			for i < n && isLetter(rs[i]) {
				i++
			}

		case isDigit(r):
			c := 0
			for i < n && isDigit(rs[i]) && c < 3 {
				i++
				c++
			}

		case isPunct(r):
			// 规则 1：单个标点 + 后面紧跟的字母串（如 ".Hello"）
			if i+1 < n && isLetter(rs[i+1]) {
				i++
				for i < n && isLetter(rs[i]) {
					i++
				}
				break
			}
			// 规则 3：标点串 + 尾部换行
			for i < n && isPunct(rs[i]) {
				i++
			}
			for i < n && (rs[i] == '\n' || rs[i] == '\r') {
				i++
			}

		default: // 空白
			// 规则 1：空格 + 字母
			if i+1 < n && isLetter(rs[i+1]) {
				i++
				for i < n && isLetter(rs[i]) {
					i++
				}
				break
			}
			// 规则 3：空格 + 标点串 + 换行
			if i+1 < n && isPunct(rs[i+1]) {
				i++
				for i < n && isPunct(rs[i]) {
					i++
				}
				for i < n && (rs[i] == '\n' || rs[i] == '\r') {
					i++
				}
				break
			}
			// 规则 3b：含换行的空白串
			j := i
			hasNL := false
			for j < n && isSpace(rs[j]) {
				if rs[j] == '\n' || rs[j] == '\r' {
					hasNL = true
				}
				j++
			}
			if hasNL && j < n && !isSpace(rs[j]) {
				i = j - 1 // 把最后一个换行留给下一个 piece
			} else {
				i = j
			}
			if i <= start {
				i = start + 1
			}
		}
		piece := string(rs[start:i])
		// 缩略词后缀合并：'s 't 're 've 'm 'll 'd 紧贴前一个词
		if len(out) > 0 && strings.HasPrefix(piece, "'") && len(piece) <= 3 {
			switch strings.ToLower(strings.TrimPrefix(piece, "'")) {
			case "s", "t", "re", "ve", "m", "ll", "d":
				out[len(out)-1] += piece
				continue
			}
		}
		out = append(out, piece)
	}
	return out
}

// encodePiece 用标准 BPE 合并算法把一个 piece 切成 token 数。
func (b *bpe) encodePiece(piece string) int {
	if b == nil || piece == "" {
		return 0
	}
	if v, ok := b.cache.Load(piece); ok {
		return v.(int)
	}
	parts := make([]string, 0, len(piece))
	for _, r := range piece {
		parts = append(parts, string(r))
	}
	// 单字节为单位更贴近 tiktoken：先把 UTF-8 拆成字节串
	if len(parts) > 0 && parts[0] != piece {
		parts = parts[:0]
		for i := 0; i < len(piece); i++ {
			parts = append(parts, string([]byte{piece[i]}))
		}
	} else if parts[0] == piece && len(piece) > 1 {
		parts = parts[:0]
		for i := 0; i < len(piece); i++ {
			parts = append(parts, string([]byte{piece[i]}))
		}
	}
	for len(parts) > 1 {
		bestRank := int(^uint(0) >> 1)
		bestIdx := -1
		for i := 0; i < len(parts)-1; i++ {
			if r, ok := b.ranks[parts[i]+parts[i+1]]; ok && r < bestRank {
				bestRank = r
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			break
		}
		merged := parts[bestIdx] + parts[bestIdx+1]
		parts = append(parts[:bestIdx], append([]string{merged}, parts[bestIdx+2:]...)...)
	}
	n := len(parts)
	b.cache.Store(piece, n)
	return n
}

// CountTokens 返回文本的 token 数。
func CountTokens(model, text string) int {
	if text == "" {
		return 0
	}
	enc := pickEncoder(model)
	if enc == nil {
		return len([]rune(text)) / 2
	}
	total := 0
	for _, p := range splitPieces(text) {
		total += enc.encodePiece(p)
	}
	return total
}

// jsonTexts 递归收集请求体里有语义的字符串（跳过 role/model/type 等短标识）。
func jsonTexts(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, x := range t {
			jsonTexts(x, out)
		}
	case map[string]any:
		// 按固定顺序遍历，避免 map 随机序影响结果
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch k {
			case "model", "role", "type", "id", "name", "tool_choice", "response_format":
				continue
			}
			jsonTexts(t[k], out)
		}
	}
}

// CountRequestTokens 估算请求体 token 数：只对文本内容计数，不含 JSON 结构。
func CountRequestTokens(model string, body []byte) (int, bool) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return 0, false
	}
	var texts []string
	jsonTexts(v, &texts)
	if len(texts) == 0 {
		return 0, false
	}
	return CountTokens(model, strings.Join(texts, "\n")), true
}

// extractStreamText 从 SSE 原始字节里抽取 content/delta 文本，用于没有 usage 时的输出计数。
func extractStreamText(buf []byte) string {
	var sb strings.Builder
	for _, line := range bytes.Split(buf, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var ev map[string]any
		if json.Unmarshal(payload, &ev) != nil {
			continue
		}
		collectDelta(ev, &sb)
	}
	return sb.String()
}

func collectDelta(v any, sb *strings.Builder) {
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			collectDelta(x, sb)
		}
	case map[string]any:
		for k, x := range t {
			switch k {
			case "content", "text", "output_text", "reasoning_content":
				if s, ok := x.(string); ok {
					sb.WriteString(s)
				}
			case "delta", "message", "choices", "output", "item", "response":
				collectDelta(x, sb)
			}
		}
	}
}

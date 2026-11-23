package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// PriceEntry 单价：每 100 万 token 的价格（与 new-api 口径一致）。
type PriceEntry struct {
	In  float64 `json:"in"`
	Out float64 `json:"out"`
}

type priceTable struct {
	mu        sync.RWMutex
	path      string
	defaultIn float64
	defOut    float64
	models    map[string]PriceEntry
	modTime   time.Time
	currency  string
}

func logf(format string, args ...any) { log.Printf(format, args...) }

var prices = &priceTable{models: map[string]PriceEntry{}, currency: "USD"}

// loadPrices 读取 prices.json；结构：
//
//	{
//	  "currency": "USD",
//	  "default": {"in": 0.15, "out": 0.6},
//	  "models": { "deepseek-v4.1-flash": {"in": 0.14, "out": 0.28} }
//	}
func (p *priceTable) load(path string) {
	if path == "" {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	p.mu.RLock()
	same := !st.ModTime().After(p.modTime) && p.path == path
	p.mu.RUnlock()
	if same {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var f struct {
		Currency string                `json:"currency"`
		Default  *PriceEntry           `json:"default"`
		Models   map[string]PriceEntry `json:"models"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		logf("prices: 解析失败 %s: %v", path, err)
		return
	}
	p.mu.Lock()
	p.path = path
	p.modTime = st.ModTime()
	if f.Currency != "" {
		p.currency = f.Currency
	}
	p.models = f.Models
	if p.models == nil {
		p.models = map[string]PriceEntry{}
	}
	if f.Default != nil {
		p.defaultIn, p.defOut = f.Default.In, f.Default.Out
	}
	p.mu.Unlock()
	logf("prices: 已加载 %d 个模型单价（%s）", len(f.Models), p.currency)
}

// cost 按模型计算费用。
func (p *priceTable) cost(model string, in, out int64) float64 {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	e, ok := p.models[model]
	if !ok {
		e = PriceEntry{In: p.defaultIn, Out: p.defOut}
	}
	p.mu.RUnlock()
	return float64(in)/1e6*e.In + float64(out)/1e6*e.Out
}

func (p *priceTable) currencyCode() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.currency == "" {
		return "USD"
	}
	return p.currency
}

func (p *priceTable) snapshot() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return map[string]any{
		"path":     p.path,
		"currency": p.currency,
		"default":  PriceEntry{In: p.defaultIn, Out: p.defOut},
		"models":   p.models,
	}
}

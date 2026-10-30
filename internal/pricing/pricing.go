// Package pricing supplies offline-first USD per-million-token quotes.
package pricing

import (
	"embed"
	"encoding/json"
	"math"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

//go:embed *.json
var snapshots embed.FS

type Price struct {
	Input             float64 `json:"i"`
	Output            float64 `json:"o"`
	CacheRead         float64 `json:"cr"`
	CacheWrite        float64 `json:"cw"`
	Context           uint64  `json:"ctx"`
	Label             string  `json:"l"`
	Source            string  `json:"source"`
	SourceModel       string  `json:"source_model"`
	MissingCacheRead  bool    `json:"missing_cache_read"`
	MissingCacheWrite bool    `json:"missing_cache_write"`
	Tiers             []Tier  `json:"tiers"`
}
type Tier struct {
	Above             uint64  `json:"above"`
	Input             float64 `json:"i"`
	Output            float64 `json:"o"`
	CacheRead         float64 `json:"cr"`
	CacheWrite        float64 `json:"cw"`
	MissingCacheRead  bool    `json:"missing_cache_read"`
	MissingCacheWrite bool    `json:"missing_cache_write"`
}

func (p Price) ForPrompt(prompt float64) Price {
	var best uint64
	for _, t := range p.Tiers {
		if prompt > float64(t.Above) && t.Above >= best {
			best = t.Above
			p.Input = t.Input
			p.Output = t.Output
			p.CacheRead = t.CacheRead
			p.CacheWrite = t.CacheWrite
			p.MissingCacheRead = t.MissingCacheRead
			p.MissingCacheWrite = t.MissingCacheWrite
		}
	}
	return p
}
func (p Price) SupportsCacheUsage(read, write float64) bool {
	return (read <= 0 || !p.MissingCacheRead) && (write <= 0 || !p.MissingCacheWrite)
}
func (p Price) Cost(input, output, cached, write float64) float64 {
	p = p.ForPrompt(input + cached + write)
	return (input*p.Input + output*p.Output + cached*p.CacheRead + write*p.CacheWrite) / 1e6
}
func (p Price) Total(input, output, cached, write float64) float64 {
	return p.Cost(input, output, cached, write)
}

var mu sync.RWMutex
var api, pro, legacy map[string]Price
var catalogRevision uint64

func init() {
	api = embedded("models-dev-pricing.json")
	pro = embedded("devin-pro-pricing.json")
	legacy = embedded("devin-model-pricing.json")
	if !disabled() {
		if b, e := os.ReadFile(cachePath("DEVIN_USAGE_MODELS_PATH", "models-dev.json")); e == nil {
			if m, e := parseCatalog(b); e == nil {
				api = m
			}
		}
		if b, e := os.ReadFile(cachePath("DEVIN_USAGE_PRO_PRICING_PATH", "devin-pro-models.md")); e == nil {
			if m, e := parsePro(b); e == nil {
				pro = m
			}
		}
	}
}
func embedded(name string) map[string]Price {
	b, _ := snapshots.ReadFile(name)
	var m map[string]Price
	_ = json.Unmarshal(b, &m)
	return m
}

var dateSuffix = regexp.MustCompile(`(-[0-9]{8}|-[0-9]{4}-[0-9]{2}-[0-9]{2})$`)
var gptVersion = regexp.MustCompile(`^gpt-([0-9]+)-([0-9]+)(.*)$`)
var fireworksP = regexp.MustCompile(`([a-z0-9])p([0-9])`)

func bare(s string) string { return s[strings.LastIndex(s, "/")+1:] }
func normalize(s string) string {
	for _, prefix := range []string{"accounts/fireworks/models/", "fireworks/", "fireworks-ai/"} {
		if strings.HasPrefix(s, prefix) {
			return "fireworks-ai/" + fireworksP.ReplaceAllString(strings.TrimPrefix(s, prefix), "${1}.${2}")
		}
	}
	id := bare(s)
	prefix := strings.TrimSuffix(s, id)
	if strings.HasPrefix(id, "gk-") {
		return prefix + "grok-" + strings.TrimPrefix(id, "gk-")
	}
	return prefix + gptVersion.ReplaceAllString(id, "gpt-${1}.${2}${3}")
}
func effort(s string) bool {
	for _, v := range []string{"-low", "-medium", "-high", "-xhigh", "-max", "-thinking", "-think", " low", " medium", " high", " xhigh", " max", " low thinking", " medium thinking", " high thinking", " xhigh thinking", " max thinking"} {
		if s == v {
			return true
		}
	}
	if strings.HasPrefix(s, "-thinking:") {
		s = strings.TrimPrefix(s, "-thinking:")
		return s != "" && strings.Trim(s, "0123456789") == ""
	}
	return false
}
func lookup(m map[string]Price, s string) (Price, bool) {
	if p, ok := m[s]; ok {
		return p, true
	}
	s = normalize(s)
	if p, ok := m[s]; ok {
		return p, true
	}
	s = dateSuffix.ReplaceAllString(s, "")
	if p, ok := m[s]; ok {
		return p, true
	}
	for i := len(s) - 1; i > 0; i-- {
		if effort(s[i:]) {
			b := bare(s[:i])
			if strings.HasPrefix(b, "gpt-") || strings.HasPrefix(b, "claude-") {
				if p, ok := m[s[:i]]; ok {
					return p, true
				}
			}
		}
	}
	return Price{}, false
}

// Lookup inspects quotes, including legacy Devin labels. Cost always selects the billing channel explicitly.
func Lookup(name string) (price Price, found bool) {
	mu.RLock()
	defer mu.RUnlock()
	defer func() { price.Tiers = append([]Tier(nil), price.Tiers...) }()
	s := strings.ToLower(strings.TrimSpace(name))
	if p, ok := legacy[s]; ok {
		return p, true
	}
	if p, ok := lookup(api, s); ok {
		return p, true
	}
	return labelLookup(legacy, s)
}
func labelLookup(m map[string]Price, s string) (Price, bool) {
	var result Price
	best := -1
	for _, p := range m {
		l := strings.ToLower(p.Label)
		if l != "" && (s == l || (strings.HasPrefix(s, l) && effort(s[len(l):]))) && len(l) > best {
			result = p
			best = len(l)
		}
	}
	return result, best >= 0
}
func devin(s string) (Price, bool) {
	s = bare(s)
	if p, ok := pro[s]; ok {
		return p, true
	}
	if p, ok := pro[strings.ReplaceAll(s, ".", "-")]; ok {
		return p, true
	}
	return labelLookup(pro, s)
}
func valid(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

// Cost prices one request; valid recorded billing (including zero) takes precedence.
func Cost(t model.Turn, fallbackModel string) (float64, bool) {
	if t.RecordedCost != nil && valid(*t.RecordedCost) {
		return *t.RecordedCost, true
	}
	p, ok := resolve(t.Agent, costModel(t, fallbackModel))
	return costWithPrice(t, p, ok)
}

func costModel(t model.Turn, fallback string) string {
	if t.Model != "" {
		return t.Model
	}
	return fallback
}

func resolve(agent model.Agent, name string) (Price, bool) {
	mu.RLock()
	defer mu.RUnlock()
	return resolveLocked(agent, name)
}

// Caller holds mu; catalog replacement and lookup share the same lock.
func resolveLocked(agent model.Agent, name string) (Price, bool) {
	s := strings.ToLower(strings.TrimSpace(name))
	var p Price
	var ok bool
	if agent == model.Devin {
		p, ok = devin(s)
	} else {
		p, ok = lookup(api, s)
	}
	if !ok && agent == model.Antigravity {
		p, ok = antigravityAlias(s)
	}
	return p, ok
}

// antigravityFamilies bill any unknown variant as the base model, except
// variants that are separate products with their own prices.
var antigravityFamilies = []struct {
	base       string
	restricted []string
}{
	{"gemini-3.8-flash", []string{"lite", "tts", "cyber"}},
	{"gemini-4-argon", nil},
}

// antigravityAlias prices Antigravity's internal labels. It tags builds with
// -trial / -exp and one-letter rollout arms (gemini-4-argon-c,
// gemini-3-flash-a, gemini-3.7-flash-exp-a); those bill as the base model.
// Google lists some of them only as -preview (gemini-3-flash-preview).
// Caller holds mu.
func antigravityAlias(s string) (Price, bool) {
	find := func(base string) (Price, bool) {
		if p, ok := lookup(api, base); ok {
			return p, true
		}
		return lookup(api, base+"-preview")
	}
	if p, ok := find(s); ok {
		return p, true
	}
	for base := s; ; {
		i := strings.LastIndexByte(base, '-')
		if i <= 0 || !antigravityTag(base[i+1:]) {
			break
		}
		base = base[:i]
		if p, ok := find(base); ok {
			return p, true
		}
	}
	for _, f := range antigravityFamilies {
		suffix, ok := strings.CutPrefix(s, f.base+"-")
		if !ok || suffix == "" {
			continue
		}
		restricted := false
		for _, r := range f.restricted {
			restricted = restricted || suffix == r || strings.HasPrefix(suffix, r+"-")
		}
		if !restricted {
			return lookup(api, f.base)
		}
	}
	return Price{}, false
}

func antigravityTag(s string) bool {
	switch s {
	case "trial", "exp", "preview":
		return true
	}
	return len(s) == 1 && s[0] >= 'a' && s[0] <= 'z'
}

func costWithPrice(t model.Turn, p Price, ok bool) (float64, bool) {
	if !ok {
		return 0, false
	}
	i, o, r, w := t.InputTokens, t.OutputTokens, t.CacheReadTokens, t.CacheCreationTokens
	if t.Agent == model.Claude && (t.CacheCreation5mTokens > 0 || t.CacheCreation1hTokens > 0) {
		p = p.ForPrompt(i + r + t.CacheCreation5mTokens + t.CacheCreation1hTokens)
		if !p.SupportsCacheUsage(r, t.CacheCreation5mTokens) {
			return 0, false
		}
		return (i*p.Input + o*p.Output + r*p.CacheRead + t.CacheCreation5mTokens*p.CacheWrite + t.CacheCreation1hTokens*p.Input*2) / 1e6, true
	}
	p = p.ForPrompt(i + r + w)
	// Google quotes no cache-write price for Gemini; estimate first processing
	// at the input rate rather than leaving the turn unpriced.
	if t.Agent == model.Antigravity && p.MissingCacheWrite && p.Source == "google" {
		i += w
		w = 0
	}
	if t.Agent != model.Devin && !p.SupportsCacheUsage(r, w) {
		return 0, false
	}
	return p.Cost(i, o, r, w), true
}

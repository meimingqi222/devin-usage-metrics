package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var refreshMu sync.Mutex

func disabled() bool {
	s := os.Getenv("DEVIN_USAGE_DISABLE_MODELS_FETCH")
	return s == "1" || s == "true"
}
func cachePath(env, name string) string {
	if p := os.Getenv(env); strings.TrimSpace(p) != "" {
		return p
	}
	d, _ := os.UserCacheDir()
	return filepath.Join(d, "devin-usage-metrics", name)
}
func fresh(path string) bool {
	s, e := os.Stat(path)
	return e == nil && time.Since(s.ModTime()) >= 0 && time.Since(s.ModTime()) < 24*time.Hour
}
func atomicSave(path string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".pricing-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, e = f.Write(b)
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), path)
}

// Refresh checks both official catalogs. Failure preserves the last good quotes;
// force bypasses the 24-hour TTL and one-hour failure backoff, not offline mode.
func Refresh(ctx context.Context, force bool) bool {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	if disabled() {
		return false
	}
	changed := false
	for _, c := range []struct {
		env, path, urlEnv, url string
		pro                    bool
	}{
		{"DEVIN_USAGE_MODELS_PATH", "models-dev.json", "DEVIN_USAGE_MODELS_URL", "https://models.opencode.ai/api.json", false},
		{"DEVIN_USAGE_PRO_PRICING_PATH", "devin-pro-models.md", "DEVIN_USAGE_PRO_PRICING_URL", "https://docs.devin.ai/desktop/models.md", true},
	} {
		path := cachePath(c.env, c.path)
		parse := parseCatalog
		if c.pro {
			parse = parsePro
		}
		b, _ := os.ReadFile(path)
		cached, e := parse(b)
		if e == nil {
			mu.Lock()
			if c.pro {
				pro = cached
			} else {
				api = cached
			}
			catalogRevision++
			mu.Unlock()
		}
		if !force {
			if e == nil && fresh(path) {
				continue
			}
			marker, _ := os.ReadFile(path + ".retry")
			at, err := strconv.ParseInt(strings.TrimSpace(string(marker)), 10, 64)
			if err == nil && time.Now().Unix()-at < 3600 {
				continue
			}
		}
		url := os.Getenv(c.urlEnv)
		if url == "" {
			url = c.url
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		var table map[string]Price
		if err == nil {
			req.Header.Set("User-Agent", "devin-usage-metrics")
			client := http.Client{Timeout: 10 * time.Second}
			resp, re := client.Do(req)
			err = re
			if err == nil {
				if resp.StatusCode != 200 {
					err = fmt.Errorf("HTTP %d", resp.StatusCode)
				} else {
					b, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
					if err == nil {
						table, err = parse(b)
					}
				}
				resp.Body.Close()
			}
		}
		if err != nil {
			_ = atomicSave(path+".retry", []byte(strconv.FormatInt(time.Now().Unix(), 10)))
			continue
		}
		mu.Lock()
		if c.pro {
			pro = table
		} else {
			api = table
		}
		catalogRevision++
		mu.Unlock()
		_ = atomicSave(path, b)
		_ = os.Remove(path + ".retry")
		changed = true
	}
	return changed
}

func parseCatalog(b []byte) (map[string]Price, error) {
	var flat map[string]Price
	if json.Unmarshal(b, &flat) == nil && len(flat) > 0 {
		var raw map[string]map[string]json.RawMessage
		_ = json.Unmarshal(b, &raw)
		ok := true
		for id, p := range flat {
			ok = ok && p.Source != "" && p.SourceModel != "" && valid(p.Input) && valid(p.Output) && valid(p.CacheRead) && valid(p.CacheWrite)
			for _, key := range []string{"i", "o"} {
				v, exists := raw[id][key]
				ok = ok && exists && string(v) != "null"
			}
			for _, tier := range p.Tiers {
				ok = ok && tier.Above > 0 && valid(tier.Input) && valid(tier.Output) && valid(tier.CacheRead) && valid(tier.CacheWrite)
			}
		}
		if ok {
			return flat, nil
		}
	}
	var providers map[string]struct {
		Models map[string]struct {
			Cost  map[string]json.RawMessage `json:"cost"`
			Limit struct {
				Context uint64 `json:"context"`
			} `json:"limit"`
			Canonical string `json:"canonical_model_id"`
		} `json:"models"`
	}
	if e := json.Unmarshal(b, &providers); e != nil {
		return nil, e
	}
	type quote struct {
		p     Price
		owner string
	}
	aliases := map[string][]quote{}
	out := map[string]Price{}
	for provider, v := range providers {
		provider = strings.ToLower(provider)
		for id, m := range v.Models {
			id = strings.ToLower(id)
			p, ok := apiPrice(m.Cost)
			if !ok {
				continue
			}
			p.Context = m.Limit.Context
			p.Source = provider
			p.SourceModel = id
			out[provider+"/"+id] = p
			if provider == "fireworks-ai" && strings.HasPrefix(id, "accounts/fireworks/models/") {
				out[normalize(id)] = p
			}
			owner := ""
			if strings.Contains(m.Canonical, "/") {
				owner = strings.ToLower(strings.SplitN(m.Canonical, "/", 2)[0])
			}
			aliases[bare(id)] = append(aliases[bare(id)], quote{p, owner})
		}
	}
	for alias, qs := range aliases {
		if _, ok := out[alias]; ok {
			continue
		}
		sort.Slice(qs, func(i, j int) bool { return qs[i].p.Source+qs[i].p.SourceModel < qs[j].p.Source+qs[j].p.SourceModel })
		var auth []quote
		for _, q := range qs {
			if q.p.Source == origin(alias) {
				auth = append(auth, q)
			}
		}
		if len(auth) == 0 {
			for _, q := range qs {
				for _, other := range qs {
					if other.owner == q.p.Source {
						auth = append(auth, q)
						break
					}
				}
			}
		}
		hasOrigin := len(auth) > 0
		if hasOrigin {
			qs = auth
		}
		p := qs[0].p
		equal := true
		for _, q := range qs {
			a, z := p, q.p
			a.Source = ""
			a.SourceModel = ""
			a.Context = 0
			z.Source = ""
			z.SourceModel = ""
			z.Context = 0
			equal = equal && reflect.DeepEqual(a, z)
		}
		if equal && (hasOrigin || p.Input != 0 || p.Output != 0 || p.CacheRead != 0 || p.CacheWrite != 0) {
			out[alias] = p
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty catalog")
	}
	return out, nil
}
func origin(s string) string {
	for _, v := range []struct{ provider, prefixes string }{{"anthropic", "claude-"}, {"openai", "gpt-,chatgpt-,o1,o3,o4,text-embedding-"}, {"google", "gemini-,gemma-"}, {"xai", "grok-,gk-"}, {"deepseek", "deepseek-"}, {"moonshotai", "kimi-,moonshot-"}, {"minimax", "minimax-"}, {"zai", "glm-"}, {"alibaba", "qwen"}, {"mistral", "mistral-,open-mistral-,magistral-,codestral-,devstral-,ministral-,pixtral-"}, {"cohere", "command-,embed-"}, {"xiaomi", "mimo-"}} {
		for _, p := range strings.Split(v.prefixes, ",") {
			if strings.HasPrefix(s, p) {
				return v.provider
			}
		}
	}
	return ""
}
func apiPrice(cost map[string]json.RawMessage) (Price, bool) {
	rate := func(key string, base float64, required bool) (float64, bool) {
		b, ok := cost[key]
		if !ok {
			return base, !required
		}
		var f float64
		e := json.Unmarshal(b, &f)
		return f, e == nil && string(b) != "null" && valid(f)
	}
	i, a := rate("input", 0, true)
	o, b := rate("output", 0, true)
	r, c := rate("cache_read", 0, false)
	w, d := rate("cache_write", 0, false)
	if !a || !b || !c || !d {
		return Price{}, false
	}
	_, cr := cost["cache_read"]
	_, cw := cost["cache_write"]
	p := Price{Input: i, Output: o, CacheRead: r, CacheWrite: w, MissingCacheRead: !cr, MissingCacheWrite: !cw}
	if raw, ok := cost["tiers"]; ok {
		var tiers []map[string]json.RawMessage
		if json.Unmarshal(raw, &tiers) != nil || string(raw) == "null" {
			return Price{}, false
		}
		for _, t := range tiers {
			var def struct {
				Type string `json:"type"`
				Size uint64 `json:"size"`
			}
			if json.Unmarshal(t["tier"], &def) != nil || def.Type != "context" || def.Size == 0 {
				return Price{}, false
			}
			cost = t
			ti, a := rate("input", i, false)
			to, b := rate("output", o, false)
			tr, c := rate("cache_read", r, false)
			tw, d := rate("cache_write", w, false)
			if !a || !b || !c || !d {
				return Price{}, false
			}
			_, rPresent := t["cache_read"]
			_, wPresent := t["cache_write"]
			p.Tiers = append(p.Tiers, Tier{def.Size, ti, to, tr, tw, !cr && !rPresent, !cw && !wPresent})
		}
		sort.Slice(p.Tiers, func(i, j int) bool { return p.Tiers[i].Above < p.Tiers[j].Above })
		for j := 1; j < len(p.Tiers); j++ {
			if p.Tiers[j].Above == p.Tiers[j-1].Above {
				return Price{}, false
			}
		}
	} else if _, ok := cost["context_over_200k"]; ok {
		return Price{}, false
	}
	return p, true
}

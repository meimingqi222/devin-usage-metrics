// Package compaction replays historical contexts and manages local thresholds.
package compaction

import (
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/pricing"
	"math"
	"sort"
	"strings"
)

const CodexGroup = "Codex · all models"
const CompactReserve uint64 = 33000

// CompactionDrift charges every additional replayed compaction a share of the
// baseline simulated cost. Replay prices tokens, not the context a compaction
// discards, so dollar savings alone would overstate the benefit of compacting
// more often.
const CompactionDrift = .01

type Point struct {
	Threshold   uint64
	Cost        float64
	Compactions int
}
type Report struct {
	Recommendations []Recommendation
	Skipped         int
}
type Recommendation struct {
	DeviceID, DeviceName, Model                                                        string
	Calls, Streams, Compactions                                                        int
	Growth, After, Rework, Observed, Current, Window, Recommended, Optimal, Candidate  uint64
	BaselineCost, RecommendedCost                                                      float64
	BaselineCompactions, CandidateCompactions, RecommendedCompactions, CompactionLimit int
	OptimalCompactions                                                                 int
	OverShare, CandidateSaving, CandidateCost, CandidateNetSaving                      float64
	OptimalCost, OptimalSaving, OptimalNetSaving, CautiousSaving, SavingBar            float64
	SensitivityFailed, BaselineInferred, Uncalibrated                                  bool
	PricedCalls                                                                        int
	Curve                                                                              []Point
	Unpriced                                                                           int
	Status                                                                             string
}

func (r Recommendation) SavingsFraction() float64 { return saving(r.BaselineCost, r.RecommendedCost) }
func saving(a, b float64) float64 {
	if a > 0 {
		return math.Max(0, (a-b)/a)
	}
	return 0
}
func prompt(t model.Turn) uint64 {
	v := t.InputTokens + t.CacheReadTokens + t.CacheCreationTokens
	if v >= float64(^uint64(0)) {
		return ^uint64(0)
	}
	return uint64(v)
}
func sub(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return 0
}
func cut(a, b uint64) bool { return a >= 20000 && float64(b) < float64(a)*.6 }
func median(v []uint64) uint64 {
	if len(v) == 0 {
		return 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return v[len(v)/2]
}
func valid(t model.Turn) bool {
	if t.ContextStream == "" || strings.TrimSpace(t.Model) == "" || t.CreatedAt <= 0 {
		return false
	}
	for _, v := range []float64{t.InputTokens, t.CacheReadTokens, t.CacheCreationTokens, t.OutputTokens} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return false
		}
	}
	return prompt(t) > 0
}
func Analyze(data model.Data, agent model.Agent, device string, start, end int64, current map[[2]string]uint64) Report {
	r := Report{}
	if agent != model.Claude && agent != model.Codex {
		return r
	}
	streams := map[[3]string][]model.Turn{}
	for _, t := range data.Turns {
		if t.Agent == agent && (device == "" || device == t.DeviceID) && t.CreatedAt >= start && t.CreatedAt < end {
			k := [3]string{t.DeviceID, t.SessionKey, t.ContextStream}
			streams[k] = append(streams[k], t)
		}
	}
	groups := map[[2]string][][]model.Turn{}
	streamKeys := make([][3]string, 0, len(streams))
	for k := range streams {
		streamKeys = append(streamKeys, k)
	}
	sort.Slice(streamKeys, func(i, j int) bool {
		for n := 0; n < 3; n++ {
			if streamKeys[i][n] != streamKeys[j][n] {
				return streamKeys[i][n] < streamKeys[j][n]
			}
		}
		return false
	})
	for _, key := range streamKeys {
		s := streams[key]
		sort.SliceStable(s, func(i, j int) bool {
			if s[i].CreatedAt == s[j].CreatedAt {
				return s[i].ContextOrder < s[j].ContextOrder
			}
			return s[i].CreatedAt < s[j].CreatedAt
		})
		var seg []model.Turn
		flush := func() {
			if len(seg) > 0 {
				label := strings.ToLower(strings.TrimSpace(seg[0].Model))
				if agent == model.Codex {
					label = CodexGroup
				}
				k := [2]string{seg[0].DeviceID, label}
				groups[k] = append(groups[k], seg)
				seg = nil
			}
		}
		for _, t := range s {
			boundary := false
			if len(seg) > 0 {
				p := seg[len(seg)-1]
				boundary = strings.ToLower(strings.TrimSpace(p.Model)) != strings.ToLower(strings.TrimSpace(t.Model)) || (prompt(t) < prompt(p) && !cut(prompt(p), prompt(t)))
			}
			if boundary || !valid(t) {
				flush()
			}
			if valid(t) {
				seg = append(seg, t)
			} else {
				r.Skipped++
			}
		}
		flush()
	}
	keys := make([][2]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] == keys[j][0] {
			return keys[i][1] < keys[j][1]
		}
		return keys[i][0] < keys[j][0]
	})
	for _, k := range keys {
		v, ok := current[k]
		if !ok && agent == model.Claude {
			if m := native.FindStringSubmatch(k[1]); m != nil {
				name := "claude-" + m[1] + "-" + m[2]
				if m[3] != "" {
					name += "-" + m[3]
				}
				v, ok = current[[2]string{k[0], name}]
				if !ok {
					v, ok = current[[2]string{k[0], ""}]
				}
			}
		}
		r.Recommendations = append(r.Recommendations, recommend(agent, k, groups[k], v, ok))
	}
	sort.SliceStable(r.Recommendations, func(i, j int) bool {
		rank := func(s string) int {
			if s == "recommend" {
				return 0
			}
			if s == "keep" {
				return 1
			}
			return 2
		}
		a, b := r.Recommendations[i], r.Recommendations[j]
		if rank(a.Status) != rank(b.Status) {
			return rank(a.Status) < rank(b.Status)
		}
		return a.Calls > b.Calls
	})
	return r
}

// CalibrationTolerance bounds how far replay at the baseline may drift from
// the observed compaction count before the verdict is withheld.
const CalibrationTolerance = 1.5

// calibrate infers an unconfigured threshold as the one whose replay
// reproduces the observed compaction count. The median compaction point is
// biased low: manual compactions land early, and streams that climbed past it
// without compacting are invisible to it. Ties prefer the observed median.
func calibrate(r Recommendation, run func(threshold, rework uint64) replayResult) uint64 {
	observed := min(r.Observed, r.Window)
	candidates := append([]Point{{observed, 0, run(observed, r.Rework).compactions}}, r.Curve...)
	best := candidates[0]
	gap := func(p Point) (int, uint64) {
		d := p.Compactions - r.Compactions
		if d < 0 {
			d = -d
		}
		return d, max(p.Threshold, observed) - min(p.Threshold, observed)
	}
	for _, p := range candidates[1:] {
		dp, tp := gap(p)
		db, tb := gap(best)
		if dp < db || (dp == db && tp < tb) {
			best = p
		}
	}
	return best.Threshold
}

type replayResult struct {
	cost              float64
	compactions, over int
}

func replay(s []model.Turn, threshold, after, base, rework uint64, agent model.Agent, p pricing.Price) replayResult {
	r := replayResult{}
	summary := float64(sub(after, base))
	write := func(t model.Turn, p pricing.Price) float64 {
		if agent != model.Claude {
			return p.Input
		}
		share := 0.
		if t.CacheCreationTokens > 0 {
			share = math.Max(0, math.Min(1, t.CacheCreation1hTokens/t.CacheCreationTokens))
		}
		return p.CacheWrite*(1-share) + p.Input*2*share
	}
	ctx := prompt(s[0])
	q := p.ForPrompt(float64(ctx))
	r.cost = float64(ctx)*write(s[0], q) + s[0].OutputTokens*q.Output
	for i := 1; i < len(s); i++ {
		t, prev := s[i], s[i-1]
		growth := uint64(0)
		if !cut(prompt(prev), prompt(t)) {
			growth = sub(prompt(t), prompt(prev))
		}
		past := ctx+growth > threshold
		if past && ctx > after {
			q = p.ForPrompt(float64(ctx))
			r.cost += float64(ctx)*q.CacheRead + summary*q.Output + (summary+float64(rework))*write(t, q)
			ctx = after + rework
			r.compactions++
		}
		if past {
			r.over++
		}
		ttl := int64(300)
		if t.CacheCreation1hTokens > t.CacheCreation5mTokens {
			ttl = 3600
		}
		q = p.ForPrompt(float64(ctx + growth))
		w := write(t, q)
		read := q.CacheRead
		if agent == model.Claude && t.CreatedAt-prev.CreatedAt > ttl {
			read = w
		}
		r.cost += float64(ctx)*read + float64(growth)*w + t.OutputTokens*q.Output
		ctx += growth
	}
	r.cost /= 1e6
	return r
}
func recommend(agent model.Agent, k [2]string, all [][]model.Turn, current uint64, configured bool) Recommendation {
	r := Recommendation{DeviceID: k[0], Model: k[1], DeviceName: all[0][0].DeviceName, Streams: len(all), BaselineInferred: !configured, Status: "not_enough_calls"}
	var streams [][]model.Turn
	var prices []pricing.Price
	for _, s := range all {
		r.Calls += len(s)
		p, ok := pricing.Lookup(s[0].Model)
		ok = ok && p.Input > 0
		for _, t := range s {
			ok = ok && p.ForPrompt(float64(prompt(t))).SupportsCacheUsage(t.CacheReadTokens, t.CacheCreation5mTokens)
		}
		if ok {
			streams = append(streams, s)
			prices = append(prices, p)
			r.PricedCalls += len(s)
		} else {
			r.Unpriced++
		}
	}
	var peaks, afters, growths, reworks, bases []uint64
	for _, s := range streams {
		bases = append(bases, prompt(s[0]))
		var gs []uint64
		for i := 1; i < len(s); i++ {
			a, b := prompt(s[i-1]), prompt(s[i])
			if !cut(a, b) {
				gs = append(gs, sub(b, a))
			}
		}
		g := median(gs)
		for i := 1; i < len(s); i++ {
			a, b := prompt(s[i-1]), prompt(s[i])
			if cut(a, b) {
				peaks = append(peaks, a)
				afters = append(afters, b)
				if i+10 < len(s) {
					ok := true
					for j := i + 1; j <= i+10; j++ {
						ok = ok && !cut(prompt(s[j-1]), prompt(s[j]))
					}
					if ok {
						reworks = append(reworks, sub(prompt(s[i+10]), b+10*g))
					}
				}
			} else {
				growths = append(growths, sub(b, a))
			}
		}
	}
	r.Compactions = len(peaks)
	r.Growth = median(growths)
	r.After = median(afters)
	r.Rework = median(reworks)
	r.Observed = median(peaks)
	if len(streams) == 0 {
		r.Status = "unknown_pricing"
		return r
	}
	if r.PricedCalls < 50 {
		return r
	}
	if r.Compactions < 3 {
		r.Status = "not_enough_compactions"
		return r
	}
	if r.Compactions < 5 || len(reworks) == 0 {
		r.Rework = max(r.Rework, 10000)
	}
	minimum, baseWindow, hard := uint64(50000), uint64(272000), uint64(1000000)
	if agent == model.Claude {
		minimum, baseWindow, hard = 67000, 200000, 967000
	}
	floor := (max(r.After+r.Rework+max(r.After/2, 20*r.Growth), minimum) + 9999) / 10000 * 10000
	r.Window = hard
	for i, s := range streams {
		reached := uint64(0)
		for _, t := range s {
			reached = max(reached, prompt(t))
		}
		w := prices[i].Context
		if w == 0 {
			w = baseWindow
		}
		r.Window = min(r.Window, max(w, reached))
	}
	run := func(threshold, rework uint64) replayResult {
		sum := replayResult{}
		for i, s := range streams {
			p := replay(s, threshold, r.After, median(append([]uint64(nil), bases...)), rework, agent, prices[i])
			sum.cost += p.cost
			sum.compactions += p.compactions
			sum.over += p.over
		}
		return sum
	}
	for t := floor; t <= r.Window; t += 10000 {
		v := run(t, r.Rework)
		r.Curve = append(r.Curve, Point{t, v.cost, v.compactions})
	}
	if configured {
		r.Current = min(current, r.Window)
	} else {
		r.Current = calibrate(r, run)
	}
	baseline := run(r.Current, r.Rework)
	r.BaselineCost = baseline.cost
	r.BaselineCompactions = baseline.compactions
	r.Curve = append(r.Curve, Point{r.Current, baseline.cost, baseline.compactions})
	low := math.Inf(1)
	for _, p := range r.Curve {
		low = math.Min(low, p.Cost)
	}
	// Pure cost minimisation often lands next to the safety floor: it can
	// recommend tripling compactions for a few dollars. Context discarded by
	// a compaction is not priced, so cap the additional compactions at half
	// the baseline (with a floor of ten for small samples). The cap is only a
	// safety bound: within it, the chosen point minimises simulated cost plus
	// CompactionDrift for every additional compaction. Candidate below stays
	// the cheapest cap-compliant point for comparison.
	limit := r.BaselineCompactions + max(10, r.BaselineCompactions/2)
	r.CompactionLimit = limit
	best := -1
	for i, p := range r.Curve {
		if p.Cost <= low*1.01 && p.Compactions <= limit && (best < 0 || p.Threshold > r.Curve[best].Threshold) {
			best = i
		}
	}
	if best < 0 {
		for i, p := range r.Curve {
			if p.Compactions <= limit && (best < 0 || p.Cost < r.Curve[best].Cost) {
				best = i
			}
		}
	}
	if best < 0 {
		for i, p := range r.Curve {
			if best < 0 || p.Cost < r.Curve[best].Cost {
				best = i
			}
		}
	}
	r.Candidate = r.Curve[best].Threshold
	r.CandidateCost = r.Curve[best].Cost
	r.CandidateCompactions = r.Curve[best].Compactions
	r.CandidateSaving = saving(r.BaselineCost, r.CandidateCost)
	r.CandidateNetSaving = r.CandidateSaving - float64(max(0, r.CandidateCompactions-r.BaselineCompactions))*CompactionDrift

	optimal, adjusted := -1, 0.
	for i, p := range r.Curve {
		if p.Compactions > limit {
			continue
		}
		cost := p.Cost + float64(max(0, p.Compactions-r.BaselineCompactions))*CompactionDrift*r.BaselineCost
		if optimal < 0 || cost < adjusted-1e-9 || (math.Abs(cost-adjusted) <= 1e-9 && p.Threshold > r.Curve[optimal].Threshold) {
			optimal, adjusted = i, cost
		}
	}
	if optimal < 0 {
		optimal = best
	}
	r.Optimal = r.Curve[optimal].Threshold
	r.OptimalCost = r.Curve[optimal].Cost
	r.OptimalCompactions = r.Curve[optimal].Compactions
	r.OptimalSaving = saving(r.BaselineCost, r.OptimalCost)
	r.OptimalNetSaving = r.OptimalSaving - float64(max(0, r.OptimalCompactions-r.BaselineCompactions))*CompactionDrift
	a := run(r.Optimal, r.Rework)
	r.OverShare = float64(a.over) / float64(r.PricedCalls)
	bar := .03
	if r.Optimal < r.Current {
		bar = .05
	}
	r.SavingBar = bar
	cautious := max(r.Rework*2, r.Rework+10000)
	r.CautiousSaving = saving(run(r.Current, cautious).cost, run(r.Optimal, cautious).cost)
	r.SensitivityFailed = r.OptimalSaving >= bar && r.CautiousSaving < bar
	r.Recommended = r.Optimal
	r.RecommendedCost = r.OptimalCost
	r.RecommendedCompactions = r.OptimalCompactions
	// A baseline whose replay is far from the observed compaction count does
	// not describe this history, so savings measured against it are not
	// evidence. This happens when the threshold changed within the range or
	// many compactions were manual.
	lo, hi := min(r.Compactions, r.BaselineCompactions), max(r.Compactions, r.BaselineCompactions)
	r.Uncalibrated = hi >= lo+3 && float64(hi) > float64(lo)*CalibrationTolerance
	r.Status = "recommend"
	if r.OptimalNetSaving < bar || r.SensitivityFailed || r.Uncalibrated {
		r.Status = "keep"
		r.Recommended = r.Current
		r.RecommendedCost = r.BaselineCost
		r.RecommendedCompactions = r.BaselineCompactions
	}
	sort.Slice(r.Curve, func(i, j int) bool { return r.Curve[i].Threshold < r.Curve[j].Threshold })
	curve := r.Curve[:0]
	for _, p := range r.Curve {
		if len(curve) == 0 || curve[len(curve)-1].Threshold != p.Threshold {
			curve = append(curve, p)
		}
	}
	r.Curve = curve
	return r
}

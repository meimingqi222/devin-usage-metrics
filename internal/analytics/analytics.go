// Package analytics groups request usage into calendar-aligned local periods.
package analytics

import (
	"fmt"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/pricing"
	"time"
)

type Period string

const (
	Day   Period = "day"
	Week  Period = "week"
	Month Period = "month"
)

type ModelUsage struct {
	Input, Output, Cached, CacheWrite, Cost float64
	Turns, Sessions                         int
	Priced                                  bool
}

func (m ModelUsage) Total() float64 { return m.Input + m.Output + m.Cached + m.CacheWrite }

type Bucket struct {
	Label                                   string
	Start, End                              int64
	Input, Output, Cached, CacheWrite, Cost float64
	Turns, Sessions                         int
	ByModel                                 map[string]ModelUsage
	// TokenCost is Cost for the turns priced from tokens; UncachedCost prices
	// the same turns as if cache reads were new input. Recorded billing has
	// no token prices to compare, so it is in neither.
	TokenCost, UncachedCost float64
}

// CacheSavings is the share of the uncached price that cache reads saved.
func (b Bucket) CacheSavings() float64 {
	if b.UncachedCost <= 0 {
		return 0
	}
	return 1 - b.TokenCost/b.UncachedCost
}

func (b Bucket) Total() float64 { return b.Input + b.Output + b.Cached + b.CacheWrite }
func count(p Period) int {
	if p == Day {
		return 15
	}
	return 12
}
func shift(t time.Time, p Period, n int) time.Time {
	switch p {
	case Month:
		return t.AddDate(0, n, 0)
	case Week:
		return t.AddDate(0, 0, 7*n)
	default:
		return t.AddDate(0, 0, n)
	}
}

// Window returns a half-open natural-calendar window. Negative pages mean the current page.
func Window(p Period, page int, now time.Time) (start, end time.Time) {
	if page < 0 {
		page = 0
	}
	anchor := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if p == Month {
		anchor = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	} else if p == Week {
		anchor = anchor.AddDate(0, 0, -(int(now.Weekday())+6)%7)
	}
	n := count(p)
	return shift(anchor, p, -(n-1)-page*n), shift(anchor, p, 1-page*n)
}

type identity struct {
	agent       model.Agent
	device, key string
}

// Build counts turns, not session summary totals, and uses session metadata only as a fallback.
func Build(data model.Data, p Period, page int, agent model.Agent, device string, now time.Time) []Bucket {
	start, _ := Window(p, page, now)
	bs := make([]Bucket, count(p))
	sets := make([]map[identity]bool, len(bs))
	models := make([]map[string]map[identity]bool, len(bs))
	for i := range bs {
		s := shift(start, p, i)
		e := shift(start, p, i+1)
		label := s.Format("01-02")
		if p == Month {
			label = s.Format("2006-01")
		} else if p == Week {
			_, w := s.ISOWeek()
			label = fmt.Sprintf("W%02d", w)
		}
		bs[i] = Bucket{Label: label, Start: s.Unix(), End: e.Unix(), ByModel: map[string]ModelUsage{}}
		sets[i] = map[identity]bool{}
		models[i] = map[string]map[identity]bool{}
	}
	type sessionInfo struct {
		model    string
		adaptive bool
	}
	sessions := make(map[identity]sessionInfo, len(data.Sessions))
	for _, s := range data.Sessions {
		sessions[identity{s.Agent, s.DeviceID, s.Key}] = sessionInfo{s.DisplayModel(), s.SelectedModel == "adaptive"}
	}
	var calculator pricing.Calculator
	for _, t := range data.Turns {
		if (agent != "" && t.Agent != agent) || (device != "" && t.DeviceID != device) {
			continue
		}
		for j := range bs {
			b := &bs[j]
			if t.CreatedAt < b.Start || t.CreatedAt >= b.End {
				continue
			}
			id := identity{t.Agent, t.DeviceID, t.SessionKey}
			s := sessions[id]
			name := t.Model
			if name == "" {
				name = s.model
				if name == "" {
					name = "unknown"
				}
			}
			label := name
			if s.adaptive && name != "unknown" && name != "compactor" {
				label += "(adaptive)"
			}
			cost, ok := calculator.Cost(t, name)
			b.Input += t.InputTokens
			b.Output += t.OutputTokens
			b.Cached += t.CacheReadTokens
			b.CacheWrite += t.CacheCreationTokens
			b.Cost += cost
			if ok && t.RecordedCost == nil {
				u := t
				u.InputTokens += u.CacheReadTokens
				u.CacheReadTokens = 0
				uncached, _ := calculator.Cost(u, name)
				b.TokenCost += cost
				b.UncachedCost += uncached
			}
			b.Turns++
			sets[j][id] = true
			b.Sessions = len(sets[j])
			m := b.ByModel[label]
			m.Input += t.InputTokens
			m.Output += t.OutputTokens
			m.Cached += t.CacheReadTokens
			m.CacheWrite += t.CacheCreationTokens
			m.Cost += cost
			m.Turns++
			m.Priced = m.Priced || ok
			if models[j][label] == nil {
				models[j][label] = map[identity]bool{}
			}
			models[j][label][id] = true
			m.Sessions = len(models[j][label])
			b.ByModel[label] = m
			break
		}
	}
	return bs
}

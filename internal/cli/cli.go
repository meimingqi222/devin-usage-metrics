// Package cli implements the local usage reporting command.
package cli

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/meimingqi222/devin-usage-metrics/internal/data"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/pricing"
	"github.com/meimingqi222/devin-usage-metrics/internal/updater"
)

const help = `Agent Usage Metrics CLI

Usage:
  devin-usage-metrics --cli [options]

Options:
  --agent <name>       Agent or all; default: claude
  --days <n>           Most recent N days; default: 30
  --since <date>       Start date (YYYY-MM-DD or YYYYMMDD)
  --until <date>       Inclusive end date
  --format table|csv   Output format; default: table
  --by-agent           Include per-agent rows
  --refresh            Ignore cache and reload local data
  -h, --help           Show help
  --version            Show the app version
`

type options struct {
	agent                 model.Agent
	since, until          time.Time
	days                  int64
	csv, byAgent, refresh bool
}

func parse(args []string) (options, error) {
	o := options{agent: model.Claude, days: 30}
	if len(args) > 0 && args[0] == "--cli" {
		args = args[1:]
	}
	for i := 0; i < len(args); i++ {
		key := args[i]
		switch key {
		case "--refresh":
			o.refresh = true
			continue
		case "--by-agent":
			o.byAgent = true
			continue
		case "--agent", "--since", "--until", "--days", "--format":
		default:
			return o, fmt.Errorf("Unknown argument: %s", key)
		}
		i++
		if i == len(args) || strings.HasPrefix(args[i], "--") {
			return o, fmt.Errorf("%s requires a value", key)
		}
		v := args[i]
		switch key {
		case "--agent":
			a := strings.ReplaceAll(strings.ToLower(v), "_", "-")
			aliases := map[string]model.Agent{"all": "", "claude-code": model.Claude, "grok-build": model.Grok, "z-code": model.ZCode, "open-code": model.OpenCode, "pi-agent": model.Pi, "mimo-code": model.MimoCode, "mimo": model.MimoCode, "work-buddy": model.WorkBuddy, "wb": model.WorkBuddy, "codebuddy": model.WorkBuddy, "code-buddy": model.WorkBuddy}
			for _, agent := range model.Agents {
				aliases[string(agent)] = agent
			}
			var ok bool
			o.agent, ok = aliases[a]
			if !ok {
				return o, fmt.Errorf("Unsupported agent: %s", v)
			}
		case "--since", "--until":
			d, e := time.ParseInLocation("2006-01-02", v, time.Local)
			if e != nil {
				d, e = time.ParseInLocation("20060102", v, time.Local)
			}
			if e != nil || d.Year() < 1 || (d.Format("2006-01-02") != v && d.Format("20060102") != v) {
				return o, fmt.Errorf("Invalid date: %s; expected YYYY-MM-DD or YYYYMMDD", v)
			}
			if key == "--since" {
				o.since = d
			} else {
				o.until = d
			}
		case "--days":
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil {
				return o, fmt.Errorf("Invalid --days value: %s", v)
			}
			if n <= 0 {
				return o, fmt.Errorf("--days must be positive and within the supported date range")
			}
			o.days = n
		case "--format":
			if v != "table" && v != "csv" {
				return o, fmt.Errorf("Unsupported format: %s", v)
			}
			o.csv = v == "csv"
		}
	}
	if o.until.IsZero() {
		now := time.Now()
		o.until = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	}
	if o.since.IsZero() {
		if o.days > 3660000 {
			return o, fmt.Errorf("--days must be positive and within the supported date range")
		}
		o.since = o.until.AddDate(0, 0, -int(o.days-1))
		if o.since.Year() < 1 {
			return o, fmt.Errorf("--days must be positive and within the supported date range")
		}
	}
	if o.until.Before(o.since) {
		return o, fmt.Errorf("--until %s cannot be earlier than --since %s", o.until.Format("2006-01-02"), o.since.Format("2006-01-02"))
	}
	return o, nil
}

type usage struct {
	models                            map[string]bool
	input, output, create, read, cost float64
}

func (u *usage) merge(v *usage) {
	if u.models == nil {
		u.models = map[string]bool{}
	}
	for m := range v.models {
		u.models[m] = true
	}
	u.input += v.input
	u.output += v.output
	u.create += v.create
	u.read += v.read
	u.cost += v.cost
}
func sorted(m map[string]bool) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}
func numbers(u *usage) []float64 {
	return []float64{u.input, u.output, u.create, u.read, u.input + u.output + u.create + u.read, u.cost}
}

// Run writes a report to out and nonfatal source/pricing warnings to errOut.
// Argument and writer failures are returned to the caller, without printing a fatal error.
func Run(args []string, out, errOut io.Writer) error {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			_, e := io.WriteString(out, help)
			return e
		}
		if a == "--version" {
			_, e := fmt.Fprintln(out, "Agent Usage Metrics "+updater.CurrentVersion)
			return e
		}
	}
	o, e := parse(args)
	if e != nil {
		return e
	}
	pricing.Refresh(context.Background(), false)
	start, end := o.since.Unix(), o.until.AddDate(0, 0, 1).Unix()
	var loaded model.Data
	if o.agent == "" {
		loaded = data.LoadAll(start, end, o.refresh)
	} else {
		loaded = data.Load(o.agent, start, end, o.refresh)
	}
	rows := map[string]*usage{}
	per := map[string]map[model.Agent]*usage{}
	unpriced := map[string]bool{}
	sessions := map[string]string{}
	for _, s := range loaded.Sessions {
		if o.agent == "" || s.Agent == o.agent {
			sessions[string(s.Agent)+"\x00"+s.Key] = s.DisplayModel()
		}
	}
	for _, t := range loaded.Turns {
		if (o.agent != "" && t.Agent != o.agent) || t.CreatedAt < start || t.CreatedAt >= end {
			continue
		}
		if t.InputTokens+t.OutputTokens+t.CacheCreationTokens+t.CacheReadTokens == 0 && (t.RecordedCost == nil || *t.RecordedCost == 0) {
			continue
		}
		m := t.Model
		if m == "" {
			m = sessions[string(t.Agent)+"\x00"+t.SessionKey]
			if m == "" {
				m = "unknown"
			}
		}
		cost, ok := pricing.Cost(t, m)
		if !ok {
			unpriced[m] = true
			cost = 0
		}
		u := &usage{map[string]bool{m: true}, t.InputTokens, t.OutputTokens, t.CacheCreationTokens, t.CacheReadTokens, cost}
		d := time.Unix(t.CreatedAt, 0).In(time.Local).Format("2006-01-02")
		if rows[d] == nil {
			rows[d] = &usage{}
			per[d] = map[model.Agent]*usage{}
		}
		rows[d].merge(u)
		if per[d][t.Agent] == nil {
			per[d][t.Agent] = &usage{}
		}
		per[d][t.Agent].merge(u)
	}
	var buf strings.Builder
	headers := []string{"Date", "Models", "Input", "Output", "Cache Create", "Cache Read", "Total Tokens", "Cost (USD)"}
	if o.csv {
		headers = []string{"date", "models", "input", "output", "cache_creation", "cache_read", "total_tokens", "cost_usd"}
	}
	if o.byAgent {
		name := "Agent"
		if o.csv {
			name = "agent"
		}
		headers = append(headers[:1], append([]string{name}, headers[1:]...)...)
	}
	var body [][]string
	add := func(date, agent string, u *usage, total bool) {
		models := strings.Join(sorted(u.models), ", ")
		if total && !o.byAgent {
			models = ""
		}
		cells := []string{date}
		if o.byAgent {
			cells = append(cells, agent)
		}
		cells = append(cells, models)
		for i, n := range numbers(u) {
			if o.csv {
				if i == 5 {
					cells = append(cells, fmt.Sprintf("%.8f", n))
				} else {
					cells = append(cells, fmt.Sprintf("%.0f", n))
				}
			} else if i == 5 {
				cells = append(cells, fmt.Sprintf("$%.2f", n))
			} else {
				cells = append(cells, integer(n))
			}
		}
		body = append(body, cells)
	}
	dates := map[string]bool{}
	for d := range rows {
		dates[d] = true
	}
	total := &usage{}
	all := "All"
	if o.csv {
		all = "all"
	}
	for _, d := range sorted(dates) {
		total.merge(rows[d])
		add(d, all, rows[d], false)
		if o.byAgent {
			for _, a := range model.Agents {
				if u := per[d][a]; u != nil {
					label := a.Label()
					if o.csv {
						label = string(a)
					}
					add("", label, u, false)
				}
			}
		}
	}
	add("Total", all, total, true)
	if o.csv {
		buf.WriteString(strings.Join(headers, ",") + "\n")
		for _, row := range body {
			for i := 1; i < len(row)-6; i++ {
				row[i] = "\"" + strings.ReplaceAll(row[i], "\"", "\"\"") + "\""
			}
			buf.WriteString(strings.Join(row, ",") + "\n")
		}
	} else {
		grid(&buf, headers, body)
	}
	if _, e = io.WriteString(out, buf.String()); e != nil {
		return e
	}
	if len(unpriced) > 0 {
		if _, e = fmt.Fprintf(errOut, "Warning: no pricing found for these models; Cost excludes them: %s\n", strings.Join(sorted(unpriced), ", ")); e != nil {
			return e
		}
	}
	for _, warning := range loaded.Errors {
		if _, e = fmt.Fprintf(errOut, "Data source warning: %s\n", warning.Message); e != nil {
			return e
		}
	}
	return nil
}

func integer(n float64) string {
	s := fmt.Sprintf("%.0f", math.RoundToEven(n))
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return sign + s
}

// Approximate terminal cell widths without introducing a Unicode dependency.
func width(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == '\u200d' {
			continue
		}
		n++
		if r >= 0x1100 && (r <= 0x115f || r >= 0x2e80 && r <= 0xa4cf || r >= 0xac00 && r <= 0xd7a3 || r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe6f || r >= 0xff01 && r <= 0xff60 || r >= 0x1f300) {
			n++
		}
	}
	return n
}
func grid(w io.Writer, headers []string, body [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = width(h)
	}
	for _, row := range body {
		for i, s := range row {
			if width(s) > widths[i] {
				widths[i] = width(s)
			}
		}
	}
	parts := make([]string, len(headers))
	for i, n := range widths {
		parts[i] = strings.Repeat("-", n+2)
	}
	sep := strings.Join(parts, "+") + "\n"
	printRow := func(row []string) {
		for i, s := range row {
			pad := strings.Repeat(" ", widths[i]-width(s))
			if i >= len(headers)-6 {
				parts[i] = " " + pad + s + " "
			} else {
				parts[i] = " " + s + pad + " "
			}
		}
		fmt.Fprintln(w, strings.Join(parts, "|"))
	}
	io.WriteString(w, sep)
	printRow(headers)
	io.WriteString(w, sep)
	for _, row := range body {
		printRow(row)
	}
	io.WriteString(w, sep)
}

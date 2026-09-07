// reporter aggregates Walker results and produces the final report.
package reporter

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/f33d3r/aethyr-walker/internal/validator"
)

type Report struct {
	StartedAt time.Time
	Results   []validator.Result
	pass      int
	warn      int
	fail      int
}

func New() *Report {
	return &Report{StartedAt: time.Now()}
}

func (r *Report) Add(res validator.Result) {
	r.Results = append(r.Results, res)
	if res.Pass && !res.Warn { r.pass++ } else if res.Warn { r.warn++ } else { r.fail++ }
}

func (r *Report) Print() {
	sections := map[string][]validator.Result{}
	for _, res := range r.Results {
		// Extract section from label (first word before space)
		parts := strings.SplitN(res.Label, "/", 2)
		section := "General"
		if len(parts) > 1 { section = parts[0] }
		sections[section] = append(sections[section], res)
	}

	keys := make([]string, 0, len(sections))
	for k := range sections { keys = append(keys, k) }
	sort.Strings(keys)

	elapsed := time.Since(r.StartedAt).Round(time.Millisecond)

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║          AETHYR WALKER — E2E VALIDATION REPORT           ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Printf("  Ran in %s\n\n", elapsed)

	for _, section := range keys {
		results := sections[section]
		fmt.Printf("── %s %s\n", section, strings.Repeat("─", max(0, 54-len(section))))
		for _, res := range results {
			label := strings.TrimPrefix(res.Label, section+"/")
			line := fmt.Sprintf("  %s %-42s", res.Symbol(), label)
			if res.Latency > 0 {
				line += fmt.Sprintf(" %s", res.Latency.Round(time.Millisecond))
			}
			if !res.Pass && res.Error != "" {
				line += fmt.Sprintf("\n      └─ %s", res.Error)
			}
			fmt.Println(line)
		}
		fmt.Println()
	}

	fmt.Println("══════════════════════════════════════════════════════════")
	fmt.Printf("  ✅ %d passed   ⚠️  %d warnings   ❌ %d failed\n",
		r.pass, r.warn, r.fail)
	fmt.Printf("  Coverage: %d endpoints tested\n", len(r.Results))

	if r.fail == 0 && r.warn == 0 {
		fmt.Println("  🚀 ALL SYSTEMS GO — Aethyr Network fully operational")
	} else if r.fail == 0 {
		fmt.Println("  ⚡ Network operational with warnings — review above")
	} else {
		fmt.Println("  🔥 Failures detected — see above for details")
	}
	fmt.Println("══════════════════════════════════════════════════════════")
}

func (r *Report) HasFailures() bool { return r.fail > 0 }

func max(a, b int) int {
	if a > b { return a }
	return b
}

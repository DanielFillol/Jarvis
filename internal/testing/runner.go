package testing

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// RunnerService is the minimal interface the test runner needs from the app.Service.
// Keeping it here avoids a circular import between internal/testing and internal/app.
type RunnerService interface {
	HandleMessageDirect(ctx context.Context, channel, threadTs, originTs, question, senderUserID string) (string, error)
}

// TestResult holds the outcome of a single prompt test.
type TestResult struct {
	Section  string
	Name     string
	Passed   bool
	Failures []string
	Duration time.Duration
}

// RunAll executes all prompt tests against svc and returns the results.
// channel and threadTs are used as context for the bot calls; originTs may be empty.
func RunAll(ctx context.Context, svc RunnerService, tests []PromptTest, channel, threadTs string) []TestResult {
	results := make([]TestResult, 0, len(tests))
	for _, t := range tests {
		start := time.Now()
		log.Printf("[TEST] running %q (%s)", t.Name, t.Section)

		answer, err := svc.HandleMessageDirect(ctx, channel, threadTs, "", t.Question, "")
		dur := time.Since(start)

		res := TestResult{
			Section:  t.Section,
			Name:     t.Name,
			Duration: dur,
		}

		if err != nil {
			res.Passed = false
			res.Failures = []string{fmt.Sprintf("erro ao executar: %v", err)}
			results = append(results, res)
			log.Printf("[TEST] FAIL %q err=%v dur=%s", t.Name, err, dur)
			continue
		}

		validators := SelectValidators(t.Question)
		for _, v := range validators {
			passed, reason := v(answer)
			if !passed {
				res.Failures = append(res.Failures, reason)
			}
		}
		res.Passed = len(res.Failures) == 0
		if res.Passed {
			log.Printf("[TEST] PASS %q dur=%s", t.Name, dur)
		} else {
			log.Printf("[TEST] FAIL %q failures=%v dur=%s", t.Name, res.Failures, dur)
		}
		results = append(results, res)
	}
	return results
}

// FormatSummary returns a Slack-formatted summary of test results.
func FormatSummary(results []TestResult) string {
	passed, failed := 0, 0
	var totalDur time.Duration
	for _, r := range results {
		totalDur += r.Duration
		if r.Passed {
			passed++
		} else {
			failed++
		}
	}

	var b strings.Builder
	if failed == 0 {
		b.WriteString(fmt.Sprintf("✅ *%d/%d passaram* | ⏱ %s\n", passed, len(results), totalDur.Round(time.Second)))
	} else {
		b.WriteString(fmt.Sprintf("✅ %d passaram  |  ❌ %d falharam  |  ⏱ %s\n\n", passed, failed, totalDur.Round(time.Second)))
		b.WriteString("*Falhas:*\n")
		for _, r := range results {
			if !r.Passed {
				b.WriteString(fmt.Sprintf("• *%s*: %s\n", r.Name, strings.Join(r.Failures, "; ")))
			}
		}
	}
	return strings.TrimSpace(b.String())
}

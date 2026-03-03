// internal/llm/retry.go
package llm

import (
	"errors"
	"math"
	"math/rand"
	"strings"
	"time"
)

// CallOpenAIWithModel delegates to Client.Chat with the same signature.
// It exists as a named alias for callers that want an explicit model parameter.
func (c *Client) CallOpenAIWithModel(messages []OpenAIMessage, model string, temperature float64, maxTokens int) (string, error) {
	return c.Chat(messages, model, temperature, maxTokens)
}

// AnswerWithModel generates an answer using a specific model, bypassing the
// primary/fallback selection in AnswerWithRetry.
func (c *Client) AnswerWithModel(question, threadHistory, slackCtx, jiraCtx, model string) (string, error) {
	return c.answerWithModel(question, threadHistory, slackCtx, jiraCtx, "", "", nil, model)
}

// AnswerWithRetry generates an answer using primaryModel, retrying on transient
// failures, then falls back to lesserModel when configured and different.
// This makes answer generation resilient to flaky networking, 429s, and 5xxs.
func (c *Client) AnswerWithRetry(
	question, threadHistory, slackCtx, jiraCtx, dbCtx, fileCtx string,
	images []ImageAttachment,
	primaryModel, lesserModel string,
	maxAttempts int,
	baseDelay time.Duration,
) (string, error) {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if baseDelay <= 0 {
		baseDelay = 400 * time.Millisecond
	}

	// Try primary first.
	out, err := c.answerWithRetrySingleModel(question, threadHistory, slackCtx, jiraCtx, dbCtx, fileCtx, images, primaryModel, maxAttempts, baseDelay)
	if err == nil && strings.TrimSpace(out) != "" {
		return out, nil
	}

	// Fall back to the lesser model if configured and different from primary.
	if lesserModel != "" && lesserModel != primaryModel {
		out2, err2 := c.answerWithRetrySingleModel(question, threadHistory, slackCtx, jiraCtx, dbCtx, fileCtx, images, lesserModel, maxAttempts, baseDelay)
		if err2 == nil && strings.TrimSpace(out2) != "" {
			return out2, nil
		}
		if err2 != nil {
			return "", err2
		}
		return "", errors.New("empty content from openai (lesser model fallback)")
	}

	if err != nil {
		return "", err
	}
	return "", errors.New("empty content from openai")
}

func (c *Client) answerWithRetrySingleModel(
	question, threadHistory, slackCtx, jiraCtx, dbCtx, fileCtx string,
	images []ImageAttachment,
	model string,
	maxAttempts int,
	baseDelay time.Duration,
) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, err := c.answerWithModel(question, threadHistory, slackCtx, jiraCtx, dbCtx, fileCtx, images, model)
		if err == nil && strings.TrimSpace(out) != "" {
			return out, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = errors.New("empty content from openai")
		}

		// backoff with jitter
		if attempt < maxAttempts {
			sleep := backoffWithJitter(baseDelay, attempt)
			time.Sleep(sleep)
		}
	}
	return "", lastErr
}

func backoffWithJitter(base time.Duration, attempt int) time.Duration {
	// exponential backoff: base * 2^(attempt-1), capped
	mult := math.Pow(2, float64(attempt-1))
	d := time.Duration(float64(base) * mult)
	const capDelay = 6 * time.Second
	if d > capDelay {
		d = capDelay
	}
	// full jitter in [0.7..1.3]
	j := 0.7 + rand.Float64()*0.6
	return time.Duration(float64(d) * j)
}

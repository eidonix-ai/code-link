package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codelink/internal/config"
)

// CheckResult is a Test button's answer.
type CheckResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// CheckOCR: any HTTP answer means the server is there (a layout-parsing
// endpoint answers 405/422 to a bare HEAD); only a connection failure is
// "unreachable".
func CheckOCR(ctx context.Context, client *http.Client, rawURL string) CheckResult {
	if !ValidURL(rawURL) {
		return CheckResult{false, "The URL must be http:// or https://."}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, strings.TrimSpace(rawURL), nil)
	if err != nil {
		return CheckResult{false, err.Error()}
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return CheckResult{false, describeNetError(err)}
	}
	resp.Body.Close()
	return CheckResult{true, fmt.Sprintf("Reachable (HTTP %d, %d ms)", resp.StatusCode, time.Since(start).Milliseconds())}
}

// SearchRequest tests a search key with a one-result query.
func SearchRequest(ctx context.Context, p config.SearchProvider, key string) *http.Request {
	if p == config.Brave {
		q := url.Values{"q": {"test"}, "count": {"1"}}
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.search.brave.com/res/v1/web/search?"+q.Encode(), nil)
		r.Header.Set("X-Subscription-Token", key)
		r.Header.Set("Accept", "application/json")
		return r
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", strings.NewReader(`{"query":"test","max_results":1}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

// SearchResult words a search test's HTTP status.
func SearchResult(status int) CheckResult {
	switch {
	case status >= 200 && status < 300:
		return CheckResult{true, "Key accepted"}
	case status == 401 || status == 403:
		return CheckResult{false, "Key rejected"}
	case status == 429:
		return CheckResult{false, "Rate limited (the key is valid)"}
	}
	return CheckResult{false, fmt.Sprintf("HTTP %d", status)}
}

// CheckSearch tests a web search key.
func CheckSearch(ctx context.Context, client *http.Client, p config.SearchProvider, key string) CheckResult {
	if strings.TrimSpace(key) == "" {
		return CheckResult{false, "Enter an API key first."}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.Do(SearchRequest(ctx, p, strings.TrimSpace(key)))
	if err != nil {
		return CheckResult{false, describeNetError(err)}
	}
	resp.Body.Close()
	return SearchResult(resp.StatusCode)
}

func describeNetError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "No answer within the time limit"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

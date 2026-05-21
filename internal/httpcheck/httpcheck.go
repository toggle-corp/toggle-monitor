// Package httpcheck performs a single HTTP probe per monitor
// configuration and returns a typed result.
package httpcheck

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Config describes a single check probe.
type Config struct {
	URL                 string
	Method              string
	AcceptedStatusCodes []int
	Timeout             time.Duration
	FollowRedirects     bool
	UserAgent           string
}

// Result is the outcome of one probe.
type Result struct {
	StatusCode   int
	Duration     time.Duration
	Error        string // empty iff the request returned a status code in AcceptedStatusCodes
	ResponseBody []byte
}

// Check performs one HTTP probe according to cfg. The caller is
// responsible for in-cycle retries (the scheduler collapses retries
// into a single tick before handing the final Result to the alert
// state machine).
func Check(ctx context.Context, cfg Config) Result {
	client := &http.Client{
		Timeout: cfg.Timeout,
	}
	if !cfg.FollowRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, nil)
	if err != nil {
		return Result{Error: fmt.Sprintf("invalid request: %v", err)}
	}
	if cfg.UserAgent != "" {
		req.Header.Set("User-Agent", cfg.UserAgent)
	}

	start := time.Now()
	resp, err := client.Do(req)
	dur := time.Since(start)
	if err != nil {
		return Result{Error: err.Error(), Duration: dur}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	res := Result{
		StatusCode:   resp.StatusCode,
		Duration:     dur,
		ResponseBody: body,
	}
	if !accepted(resp.StatusCode, cfg.AcceptedStatusCodes) {
		res.Error = fmt.Sprintf("unexpected status %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return res
}

func accepted(code int, list []int) bool {
	for _, c := range list {
		if c == code {
			return true
		}
	}
	return false
}

package netboxclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Response body caps. NetBox error bodies are small JSON or HTML pages, so
// 1 MiB is generous. A 1000-row page of full device objects is a few MiB;
// 32 MiB leaves headroom while still bounding what one page can buffer.
const (
	DefaultMaxErrorBodyBytes int64 = 1 << 20
	DefaultMaxPageBodyBytes  int64 = 32 << 20
)

// ErrBodyTooLarge is returned when a response body exceeds its cap.
var ErrBodyTooLarge = errors.New("response body too large")

// LimitBody returns a reader that yields at most n bytes of r and fails with
// ErrBodyTooLarge if r has more, so an oversized body is reported as such
// rather than as a truncated-JSON decode error.
func LimitBody(r io.Reader, n int64) io.Reader {
	return &limitedBody{r: io.LimitReader(r, n+1), n: n}
}

type limitedBody struct {
	r    io.Reader
	n    int64
	read int64
}

func (l *limitedBody) Read(p []byte) (int, error) {
	k, err := l.r.Read(p)
	l.read += int64(k)
	if l.read > l.n {
		return k - int(l.read-l.n), fmt.Errorf("%w (over %d bytes)", ErrBodyTooLarge, l.n)
	}
	return k, err
}

// withRetry runs fetch up to retryCount+1 times with exponential backoff.
// A 401/403 (AuthError) is returned at once: it cannot fix itself within a
// poll. what names the fetch in the retry log line.
func withRetry[T any](ctx context.Context, c *Client, what string, fetch func(context.Context) (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt <= c.retryCount; attempt++ {
		if attempt > 0 {
			delay := c.retryDelay(attempt)
			slog.Warn("retrying "+what,
				"attempt", attempt, "max_retries", c.retryCount, "delay", delay, "err", lastErr)
			if c.RetryCounter != nil {
				c.RetryCounter.Inc()
			}
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(delay):
			}
		}
		out, err := fetch(ctx)
		if err == nil {
			return out, nil
		}
		if isAuthError(err) {
			return zero, err
		}
		lastErr = err
	}
	return zero, lastErr
}

// pageFunc fetches the page at offset with the given limit, returning the
// total count NetBox reports and the page's items.
type pageFunc[T any] func(ctx context.Context, offset, limit int) (count int, items []T, err error)

// fetchAllPages probes for the total count, then fetches every page with
// bounded concurrency. Results keep page order, and any page error fails the
// whole fetch, so a partial result never reaches zone generation.
func fetchAllPages[T any](ctx context.Context, c *Client, page pageFunc[T]) ([]T, error) {
	total, _, err := page(ctx, 0, 1)
	if err != nil {
		return nil, fmt.Errorf("probe request failed: %w", err)
	}
	if total == 0 {
		return nil, nil
	}
	totalPages := int(math.Ceil(float64(total) / float64(c.pageSize)))

	sem := make(chan struct{}, c.maxConcurrency)
	results := make([][]T, totalPages)
	errsCh := make(chan error, totalPages)

	var wg sync.WaitGroup
	for p := 0; p < totalPages; p++ {
		wg.Add(1)
		go func(pageNum, offset int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			_, items, err := page(ctx, offset, c.pageSize)
			if err != nil {
				errsCh <- fmt.Errorf("fetch page offset=%d: %w", offset, err)
				return
			}
			results[pageNum] = items
		}(p, p*c.pageSize)
	}
	wg.Wait()
	close(errsCh)

	if err := <-errsCh; err != nil {
		return nil, err
	}

	var all []T
	for _, items := range results {
		all = append(all, items...)
	}
	return all, nil
}

// getPage GETs apiPath with query and decodes the JSON body into out. Error
// and success bodies are both read through their caps.
func (c *Client) getPage(ctx context.Context, apiPath string, query url.Values, out any) error {
	reqURL := fmt.Sprintf("%s%s?%s", c.baseURL, apiPath, query.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", AuthHeader(c.token))
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return statusError(resp, apiPath, c.maxErrorBodyBytes)
	}
	if err := json.NewDecoder(LimitBody(resp.Body, c.maxPageBodyBytes)).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

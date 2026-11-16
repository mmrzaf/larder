package warm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ExecuteOptions controls plan execution.
type ExecuteOptions struct {
	BaseURL     string
	Mounts      map[string]string
	Concurrency int
	Retries     int
	FailFast    bool
	Token       string
	Client      *http.Client
}

// ItemResult is the outcome of one request.
type ItemResult struct {
	Item
	Status int    `json:"status"`
	Cache  string `json:"cache,omitempty"`
	Bytes  int64  `json:"bytes"`
	Error  string `json:"error,omitempty"`
}

// Summary reports one plan execution.
type Summary struct {
	Planned   int          `json:"planned"`
	Cached    int          `json:"cached"`
	Fetched   int          `json:"fetched"`
	Failed    int          `json:"failed"`
	Pinned    int          `json:"pinned"`
	Bytes     int64        `json:"bytes_fetched"`
	ElapsedMS int64        `json:"elapsed_ms"`
	Results   []ItemResult `json:"results,omitempty"`
}

// Execute fetches every planned item through the server.
func Execute(ctx context.Context, plan Plan, opts ExecuteOptions) Summary {
	summary := Summary{Planned: len(plan.Items), Results: make([]ItemResult, len(plan.Items))}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.Retries < 0 {
		opts.Retries = 0
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	base := strings.TrimSuffix(opts.BaseURL, "/")
	if base == "" {
		for i := range summary.Results {
			summary.Results[i] = ItemResult{Item: plan.Items[i], Error: "server URL is required"}
		}
		summary.Failed = len(plan.Items)
		return summary
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := false
	start := time.Now()
	for worker := 0; worker < opts.Concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				item := plan.Items[index]
				result := fetchItem(ctx, client, requestURL(base, opts.Mounts[item.Repo], item.Path), opts, item)
				mu.Lock()
				summary.Results[index] = result
				switch {
				case result.Error != "":
					summary.Failed++
					if opts.FailFast && !failed {
						failed = true
						cancel()
					}
				case result.Cache == "MISS" || result.Cache == "BYPASS" || result.Cache == "":
					summary.Fetched++
					summary.Bytes += result.Bytes
				default:
					summary.Cached++
				}
				mu.Unlock()
			}
		}()
	}
	for index := range plan.Items {
		select {
		case <-ctx.Done():
			break
		case jobs <- index:
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	for index := range summary.Results {
		if summary.Results[index].Item.Path == "" && summary.Results[index].Error == "" {
			summary.Results[index] = ItemResult{Item: plan.Items[index], Error: "cancelled"}
			summary.Failed++
		}
	}
	summary.ElapsedMS = time.Since(start).Milliseconds()
	return summary
}

func fetchItem(ctx context.Context, client *http.Client, target string, opts ExecuteOptions, item Item) ItemResult {
	result := ItemResult{Item: item}
	var lastErr error
	for attempt := 0; attempt <= opts.Retries; attempt++ {
		if err := ctx.Err(); err != nil {
			result.Error = err.Error()
			return result
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		if opts.Token != "" {
			request.Header.Set("X-Larder-Capture", opts.Token)
		}
		response, err := client.Do(request)
		if err != nil {
			lastErr = err
			if attempt < opts.Retries {
				sleepRetry(ctx, attempt)
				continue
			}
			break
		}
		body, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		result.Status = response.StatusCode
		result.Cache = response.Header.Get("X-Larder-Cache")
		result.Bytes = body
		if readErr != nil {
			lastErr = readErr
		} else if closeErr != nil {
			lastErr = closeErr
		} else if response.StatusCode >= 200 && response.StatusCode < 400 {
			return result
		} else if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("status %d", response.StatusCode)
		} else {
			result.Error = fmt.Sprintf("status %d", response.StatusCode)
			return result
		}
		if attempt < opts.Retries {
			sleepRetry(ctx, attempt)
		}
	}
	if lastErr != nil {
		result.Error = lastErr.Error()
	} else if result.Error == "" {
		result.Error = fmt.Sprintf("status %d", result.Status)
	}
	return result
}

func sleepRetry(ctx context.Context, attempt int) {
	delay := time.Duration(attempt+1) * 100 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func requestURL(base, mount, path string) string {
	mount = strings.Trim(mount, "/")
	path = strings.TrimPrefix(path, "/")
	if mount == "" {
		if path == "" {
			return base + "/"
		}
		return base + "/" + path
	}
	if path == "" {
		return base + "/" + mount
	}
	return base + "/" + mount + "/" + path
}

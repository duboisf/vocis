package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// lemonadeURL canonicalizes baseURL (TrimRight trailing slash, reject
// empty) and concatenates it with path. `path` is expected to start
// with `/`. Returns the same "base_url is empty" error all callers
// surface so the user sees consistent diagnostics regardless of which
// endpoint tripped the check.
func lemonadeURL(baseURL, path string) (string, error) {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		return "", fmt.Errorf("lemonade base_url is empty")
	}
	return base + path, nil
}

// httpBodyExcerpt reads up to 1024 bytes from resp.Body and returns the
// trimmed result. Errors are swallowed because the caller is already
// formatting an error response — what we surface is whatever bytes the
// server actually wrote. The cap bounds error-message size so a runaway
// HTML 500 page doesn't blow up the session log.
func httpBodyExcerpt(resp *http.Response) string {
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return strings.TrimSpace(string(excerpt))
}

// postJSON marshals `body` to JSON and POSTs it to `url` with
// `Content-Type: application/json`. Returns the live *http.Response —
// caller must Close it and is responsible for status-code checking
// (typically via httpBodyExcerpt for the error path). If client is
// nil, http.DefaultClient is used.
//
// Errors from marshal/build/transport are wrapped with the method and
// URL so log readers can identify which Lemonade endpoint failed.
func postJSON(ctx context.Context, client *http.Client, url string, body any) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	return resp, nil
}

// getJSON GETs `url` and decodes the response body as JSON into `out`.
// Non-2xx responses become an error including a body excerpt; decode
// failures are wrapped with the URL. Uses http.DefaultClient — the
// Lemonade GET endpoints (/health, /models) don't need a custom client.
func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, httpBodyExcerpt(resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("GET %s: decode response: %w", url, err)
	}
	return nil
}

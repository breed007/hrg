// Package notify sends push notifications about drift, failures, and
// staleness. The wire format is ntfy-compatible (plain-text body, Title
// header), which also works as a generic webhook: point notify_url at
// https://ntfy.sh/your-topic, a self-hosted ntfy, or anything that accepts
// a POST.
package notify

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Send POSTs one notification. Failures are the caller's to log — a dead
// notification endpoint must never break a collection cycle.
func Send(ctx context.Context, url, title, body string) error {
	if url == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", title)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify %s: %s", url, resp.Status)
	}
	return nil
}

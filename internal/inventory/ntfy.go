package inventory

import (
	"bytes"
	"context"
	"net/http"
	"time"
)

// pingNtfyEvent fires a fire-and-forget low-priority ntfy push so the
// operator sees stock-sync events (orphan SKUs, sync stalls) on their
// phone without it being a wake-up alarm. Runs in its own goroutine
// with a 5-second timeout so it never blocks the sync loop.
//
// Empty topic disables the helper — Phase 1 setups without NTFY_TOPIC
// configured continue to log only, no behaviour change.
func pingNtfyEvent(topic, title, body string) {
	if topic == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://ntfy.sh/"+topic, bytes.NewBufferString(body))
		if err != nil {
			return
		}
		req.Header.Set("Title", title)
		req.Header.Set("Priority", "default")
		req.Header.Set("Tags", "rectella,event")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
	}()
}

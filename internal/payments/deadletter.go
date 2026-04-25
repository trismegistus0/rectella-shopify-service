package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DeadLetterDir defaults if not configured. Sits alongside pg_dump
// backups so operators have one root to reason about.
const DefaultDeadLetterDir = "/home/bast/backups/rectella/missed-reports"

// writeDeadLetter persists a failed-to-send CSV to disk so the operator
// can resend manually via cmd/send-report. Returns the absolute path
// or an error if the write itself failed (rare — disk full, permissions).
//
// Filename pattern: YYYY-MM-DD-{kind}.csv. If today's slot is already
// occupied (a previous send failed for the same date earlier today)
// we append a -HHMMSS suffix so we don't clobber the prior failure.
func writeDeadLetter(dir, kind string, date time.Time, csv []byte) (string, error) {
	if dir == "" {
		dir = DefaultDeadLetterDir
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("mkdir dead-letter dir: %w", err)
	}
	datePart := date.Format("2006-01-02")
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.csv", datePart, kind))
	if _, err := os.Stat(path); err == nil {
		// Pre-existing dead-letter for the same date — keep both.
		path = filepath.Join(dir, fmt.Sprintf("%s-%s-%s.csv",
			datePart, kind, time.Now().Format("150405")))
	}
	if err := os.WriteFile(path, csv, 0o600); err != nil {
		return "", fmt.Errorf("writing dead-letter: %w", err)
	}
	return path, nil
}

// pingNtfyDeadLetter posts a brief alert to ntfy with the dead-letter
// path so the operator gets a phone notification. Best-effort — we
// don't propagate ntfy errors, the disk write is the reliable record.
func pingNtfyDeadLetter(ctx context.Context, topic, kind string, date time.Time, path string, sendErr error) {
	if topic == "" {
		return
	}
	body := fmt.Sprintf("Daily %s report send failed for %s.\nCSV preserved at: %s\nReason: %v\n\nResend with:\n  go run ./cmd/send-report --type=%s --date=%s",
		kind, date.Format("2006-01-02"), path, sendErr, kind, date.Format("2006-01-02"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://ntfy.sh/"+topic, bytes.NewBufferString(body))
	if err != nil {
		return
	}
	req.Header.Set("Title", "Rectella daily report failed")
	req.Header.Set("Priority", "high")
	req.Header.Set("Tags", "warning,rectella")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// pingHealthcheck POSTs success to a Healthchecks.io URL after a
// successful daily-report send. Best-effort, fail-quiet — the report
// reaching the recipient is the actual deliverable, the heartbeat is
// just for the operator's "did it run today" question.
func pingHealthcheck(ctx context.Context, url string) {
	if url == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// archiveSentCSV writes a copy of every successfully-sent CSV to disk.
// Audit trail independent of the recipient's mailbox — if Liz deletes
// the email, finance can still reconcile what was sent. Best-effort:
// errors are logged by the caller, never propagate to the user-facing
// "send" return code.
func archiveSentCSV(dir, kind string, date time.Time, csv []byte) (string, error) {
	if dir == "" {
		return "", nil // disabled
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("mkdir archive dir: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.csv", date.Format("2006-01-02"), kind))
	if err := os.WriteFile(path, csv, 0o600); err != nil {
		return "", fmt.Errorf("writing archive: %w", err)
	}
	return path, nil
}

// archiveSentCSVRange variant for backfill bulk CSVs.
func archiveSentCSVRange(dir, kind string, start, end time.Time, csv []byte) (string, error) {
	if dir == "" {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("mkdir archive dir: %w", err)
	}
	endLabel := end.AddDate(0, 0, -1).Format("2006-01-02")
	path := filepath.Join(dir, fmt.Sprintf("%s-to-%s-%s.csv", start.Format("2006-01-02"), endLabel, kind))
	if err := os.WriteFile(path, csv, 0o600); err != nil {
		return "", fmt.Errorf("writing archive: %w", err)
	}
	return path, nil
}

// readLastSent / writeLastSent persist the most recent successful send
// date to a tiny per-report file so operator restarts mid-fire-window
// don't re-fire the same day's report. Format: a single ISO-8601 date
// line. Missing or unreadable file returns time zero (legitimate first-
// boot or first-run state).
func readLastSent(dir, kind string) time.Time {
	if dir == "" {
		return time.Time{}
	}
	path := filepath.Join(dir, fmt.Sprintf("last-send-%s.date", kind))
	b, err := os.ReadFile(path) // #nosec G304 — path constructed from operator-controlled dir + fixed kind suffix

	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}
	}
	return t
}

func writeLastSent(dir, kind string, date time.Time) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("last-send-%s.date", kind))
	return os.WriteFile(path, []byte(date.UTC().Format("2006-01-02")+"\n"), 0o600)
}

// noopJSONStub keeps `encoding/json` referenced if no other helper in
// this file needs it later — placeholder for the operator-resend
// envelope metadata. Remove if not extended within a couple of weeks.
var _ = json.Marshal

package payments

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteDeadLetter_HappyPath(t *testing.T) {
	dir := t.TempDir()
	date := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	csv := []byte("col1,col2\nval1,val2\n")
	path, err := writeDeadLetter(dir, "cash", date, csv)
	if err != nil {
		t.Fatalf("writeDeadLetter: %v", err)
	}
	want := filepath.Join(dir, "2026-04-24-cash.csv")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	got, err := os.ReadFile(path) // #nosec G304 — test reads a path it just wrote into a t.TempDir
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != string(csv) {
		t.Errorf("body mismatch")
	}
}

func TestWriteDeadLetter_CollisionGetsTimestampSuffix(t *testing.T) {
	dir := t.TempDir()
	date := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	first, err := writeDeadLetter(dir, "cash", date, []byte("first"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := writeDeadLetter(dir, "cash", date, []byte("second"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first == second {
		t.Errorf("expected distinct paths on collision, got %q twice", first)
	}
	if !strings.HasPrefix(filepath.Base(second), "2026-04-24-cash-") {
		t.Errorf("collision path not suffixed with timestamp: %q", second)
	}
}

func TestArchiveSentCSV_Disabled(t *testing.T) {
	path, err := archiveSentCSV("", "cash", time.Now(), []byte("x"))
	if err != nil {
		t.Errorf("err on disabled archive: %v", err)
	}
	if path != "" {
		t.Errorf("path should be empty when disabled, got %q", path)
	}
}

func TestArchiveSentCSV_Writes(t *testing.T) {
	dir := t.TempDir()
	date := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	path, err := archiveSentCSV(dir, "cash", date, []byte("hello"))
	if err != nil {
		t.Fatalf("archiveSentCSV: %v", err)
	}
	if !strings.HasSuffix(path, "2026-04-24-cash.csv") {
		t.Errorf("unexpected path: %q", path)
	}
	body, _ := os.ReadFile(path) // #nosec G304 — test reads a path it just wrote into a t.TempDir
	if string(body) != "hello" {
		t.Errorf("body wrong: %q", body)
	}
}

func TestArchiveSentCSVRange_Writes(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 26, 0, 0, 0, 0, time.UTC) // exclusive end → label is 04-25
	path, err := archiveSentCSVRange(dir, "cash", start, end, []byte("hello"))
	if err != nil {
		t.Fatalf("archiveSentCSVRange: %v", err)
	}
	if !strings.HasSuffix(path, "2026-04-01-to-2026-04-25-cash.csv") {
		t.Errorf("unexpected path: %q", path)
	}
}

func TestReadWriteLastSent_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	date := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	if err := writeLastSent(dir, "cash", date); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readLastSent(dir, "cash")
	if !got.Equal(date) {
		t.Errorf("round-trip mismatch: got %v want %v", got, date)
	}
}

func TestReadLastSent_MissingFile(t *testing.T) {
	dir := t.TempDir()
	got := readLastSent(dir, "cash")
	if !got.IsZero() {
		t.Errorf("missing file should return zero time, got %v", got)
	}
}

func TestReadLastSent_DisabledDir(t *testing.T) {
	if got := readLastSent("", "cash"); !got.IsZero() {
		t.Error("empty dir should return zero time")
	}
}

func TestPingHealthcheck_FiresWhenURLSet(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	pingHealthcheck(t.Context(), srv.URL)
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

func TestPingHealthcheck_NoOpWhenURLEmpty(t *testing.T) {
	// Should not panic, should not block.
	pingHealthcheck(t.Context(), "")
}

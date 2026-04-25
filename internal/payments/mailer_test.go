package payments

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// graphFakeServer stands in for both the OAuth token endpoint and the
// Graph sendMail endpoint. One mux, two routes, configurable failures.
type graphFakeServer struct {
	srv            *httptest.Server
	tokenCalls     atomic.Int32
	sendMailCalls  atomic.Int32
	tokenStatus    int
	tokenResponse  map[string]any
	sendStatuses   []int        // popped per call; if empty, always 202
	lastAuthBearer atomic.Value // string — last bearer header seen on sendMail
	lastPayload    atomic.Value // []byte — last sendMail body
}

func newGraphFakeServer() *graphFakeServer {
	f := &graphFakeServer{
		tokenStatus: 200,
		tokenResponse: map[string]any{
			"access_token": "tok-abc",
			"expires_in":   3600,
			"token_type":   "Bearer",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/tenant-xyz/oauth2/v2.0/token", f.handleToken)
	mux.HandleFunc("/v1.0/users/shopify-service@rectella.com/sendMail", f.handleSendMail)
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *graphFakeServer) close() { f.srv.Close() }

func (f *graphFakeServer) handleToken(w http.ResponseWriter, r *http.Request) {
	f.tokenCalls.Add(1)
	// Test fake for the Microsoft OAuth token endpoint. Body is a hardcoded
	// JSON map under our control, consumed by our Mailer's HTTP client — not
	// a browser — so the XSS rule triggered by "writing to ResponseWriter"
	// does not apply. Suppress the generic scan warning on the line below.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.tokenStatus)
	// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter
	_ = json.NewEncoder(w).Encode(f.tokenResponse)
}

func (f *graphFakeServer) handleSendMail(w http.ResponseWriter, r *http.Request) {
	n := f.sendMailCalls.Add(1)
	f.lastAuthBearer.Store(r.Header.Get("Authorization"))
	body, _ := io.ReadAll(r.Body)
	f.lastPayload.Store(body)

	status := http.StatusAccepted
	if int(n) <= len(f.sendStatuses) {
		status = f.sendStatuses[n-1]
	}
	w.WriteHeader(status)
}

func newTestMailer(t *testing.T, f *graphFakeServer) *Mailer {
	t.Helper()
	return NewMailer(MailerConfig{
		TenantID:      "tenant-xyz",
		ClientID:      "client-abc",
		ClientSecret:  "secret-shhh",
		SenderMailbox: "shopify-service@rectella.com",
		GraphBaseURL:  f.srv.URL + "/v1.0",
		TokenBaseURL:  f.srv.URL,
	})
}

func TestMailer_Send_HappyPath(t *testing.T) {
	f := newGraphFakeServer()
	defer f.close()
	m := newTestMailer(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	att := &Attachment{
		Filename:    "report.csv",
		ContentType: "text/csv",
		Body:        []byte("header\nrow\n"),
	}
	if err := m.Send(ctx, []string{"creditcontrol@example.com"}, "Daily Cash Receipts", "Totals enclosed.", att); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := f.sendMailCalls.Load(); got != 1 {
		t.Errorf("sendMail calls = %d, want 1", got)
	}
	if got := f.tokenCalls.Load(); got != 1 {
		t.Errorf("token calls = %d, want 1", got)
	}
	if bearer, _ := f.lastAuthBearer.Load().(string); bearer != "Bearer tok-abc" {
		t.Errorf("Authorization header = %q, want Bearer tok-abc", bearer)
	}

	raw, _ := f.lastPayload.Load().([]byte)
	var env struct {
		Message struct {
			Subject      string            `json:"subject"`
			Body         map[string]string `json:"body"`
			ToRecipients []struct {
				EmailAddress struct {
					Address string `json:"address"`
				} `json:"emailAddress"`
			} `json:"toRecipients"`
			Attachments []struct {
				ODataType    string `json:"@odata.type"`
				Name         string `json:"name"`
				ContentType  string `json:"contentType"`
				ContentBytes string `json:"contentBytes"`
			} `json:"attachments"`
		} `json:"message"`
		SaveToSentItems bool `json:"saveToSentItems"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal payload: %v\nraw: %s", err, raw)
	}
	if env.Message.Subject != "Daily Cash Receipts" {
		t.Errorf("subject = %q", env.Message.Subject)
	}
	if env.Message.Body["contentType"] != "Text" {
		t.Errorf("contentType = %q, want Text", env.Message.Body["contentType"])
	}
	if env.Message.Body["content"] != "Totals enclosed." {
		t.Errorf("content = %q", env.Message.Body["content"])
	}
	if len(env.Message.ToRecipients) != 1 || env.Message.ToRecipients[0].EmailAddress.Address != "creditcontrol@example.com" {
		t.Errorf("toRecipients = %+v", env.Message.ToRecipients)
	}
	if len(env.Message.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(env.Message.Attachments))
	}
	a := env.Message.Attachments[0]
	if a.ODataType != "#microsoft.graph.fileAttachment" || a.Name != "report.csv" || a.ContentType != "text/csv" {
		t.Errorf("attachment meta = %+v", a)
	}
	decoded, err := base64.StdEncoding.DecodeString(a.ContentBytes)
	if err != nil {
		t.Fatalf("attachment base64: %v", err)
	}
	if string(decoded) != "header\nrow\n" {
		t.Errorf("attachment body = %q", decoded)
	}
}

func TestMailer_Send_HTMLBodyDetected(t *testing.T) {
	f := newGraphFakeServer()
	defer f.close()
	m := newTestMailer(t, f)

	err := m.Send(context.Background(), []string{"ops@example.com"}, "Intake", "<p>Hi</p>", nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	raw, _ := f.lastPayload.Load().([]byte)
	var env struct {
		Message struct {
			Body map[string]string `json:"body"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Message.Body["contentType"] != "HTML" {
		t.Errorf("contentType = %q, want HTML", env.Message.Body["contentType"])
	}
}

func TestMailer_Send_TokenCached(t *testing.T) {
	f := newGraphFakeServer()
	defer f.close()
	m := newTestMailer(t, f)

	for i := 0; i < 3; i++ {
		if err := m.Send(context.Background(), []string{"x@example.com"}, "s", "b", nil); err != nil {
			t.Fatalf("Send #%d: %v", i, err)
		}
	}
	if got := f.tokenCalls.Load(); got != 1 {
		t.Errorf("token calls = %d across 3 sends, want 1 (cached)", got)
	}
	if got := f.sendMailCalls.Load(); got != 3 {
		t.Errorf("sendMail calls = %d, want 3", got)
	}
}

func TestMailer_Send_401TriggersRefresh(t *testing.T) {
	f := newGraphFakeServer()
	defer f.close()
	f.sendStatuses = []int{401, 202} // first send 401, second 202
	m := newTestMailer(t, f)

	if err := m.Send(context.Background(), []string{"x@example.com"}, "s", "b", nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := f.sendMailCalls.Load(); got != 2 {
		t.Errorf("sendMail calls = %d, want 2 (1 failed + 1 retry)", got)
	}
	if got := f.tokenCalls.Load(); got != 2 {
		t.Errorf("token calls = %d, want 2 (initial + refresh after 401)", got)
	}
}

func TestMailer_Send_TokenEndpointFailure(t *testing.T) {
	f := newGraphFakeServer()
	defer f.close()
	f.tokenStatus = 400
	f.tokenResponse = map[string]any{"error": "invalid_client"}
	m := newTestMailer(t, f)

	err := m.Send(context.Background(), []string{"x@example.com"}, "s", "b", nil)
	if err == nil {
		t.Fatal("expected error from token endpoint failure")
	}
	if !strings.Contains(err.Error(), "token endpoint") {
		t.Errorf("error = %v, want token endpoint failure", err)
	}
	if got := f.sendMailCalls.Load(); got != 0 {
		t.Errorf("sendMail calls = %d, want 0 (should short-circuit on token failure)", got)
	}
}

func TestMailer_Send_GraphFailurePropagates(t *testing.T) {
	f := newGraphFakeServer()
	defer f.close()
	f.sendStatuses = []int{500}
	m := newTestMailer(t, f)

	err := m.Send(context.Background(), []string{"x@example.com"}, "s", "b", nil)
	if err == nil {
		t.Fatal("expected error for 500 from Graph")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %v, want HTTP 500", err)
	}
}

func TestMailer_Send_Validation(t *testing.T) {
	m := NewMailer(MailerConfig{TenantID: "t", ClientID: "c", ClientSecret: "s", SenderMailbox: "a@b"})
	if err := m.Send(context.Background(), nil, "s", "b", nil); err == nil {
		t.Error("want error for empty recipients")
	}

	m2 := NewMailer(MailerConfig{})
	if err := m2.Send(context.Background(), []string{"x@example.com"}, "s", "b", nil); err == nil {
		t.Error("want error for incomplete Graph config")
	}
}

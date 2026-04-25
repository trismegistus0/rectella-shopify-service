package payments

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Graph endpoints. Overrideable for tests via MailerConfig.GraphBaseURL /
// TokenBaseURL so the test server can stand in for both.
const (
	defaultGraphBaseURL = "https://graph.microsoft.com/v1.0"
	defaultTokenBaseURL = "https://login.microsoftonline.com"
)

// MailerConfig bundles Microsoft Graph credentials for the outbound mailer.
// All fields required. Send() rejects any attempt to run with an incomplete
// config rather than letting the HTTP call fail half-configured.
//
// The app registration (`SysPro Shopify Graph API App`, provisioned by NCS
// on 2026-04-23) has Mail.Send application permission scoped via an
// ApplicationAccessPolicy to exactly one mailbox — SenderMailbox must match
// that scoping or Graph returns 403.
type MailerConfig struct {
	TenantID       string
	ClientID       string
	ClientSecret   string
	SenderMailbox  string        // e.g. "shopify-service@rectella.com"
	GraphBaseURL   string        // optional — defaults to https://graph.microsoft.com/v1.0
	TokenBaseURL   string        // optional — defaults to https://login.microsoftonline.com
	RequestTimeout time.Duration // optional — default 30s
}

// Mailer sends a single email with one optional attachment via the
// Microsoft Graph sendMail endpoint. Narrow scope: the daily reports are
// the only callers. Tokens are cached in-memory with a small safety margin
// and refreshed lazily; a 401 from Graph triggers a forced refresh + one
// retry.
type Mailer struct {
	cfg    MailerConfig
	client *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// NewMailer constructs a production mailer.
func NewMailer(cfg MailerConfig) *Mailer {
	timeout := cfg.RequestTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Mailer{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

// Attachment is a single file attached to an email.
type Attachment struct {
	Filename    string
	ContentType string
	Body        []byte
}

// Send transmits an email with the given recipients, subject, body, and
// optional attachment. Body is sent as HTML if it contains a `<` character,
// Text otherwise — good enough for the two reports we emit today without
// demanding callers pick a content type.
func (m *Mailer) Send(ctx context.Context, to []string, subject, body string, att *Attachment) error {
	if len(to) == 0 {
		return errors.New("mailer: no recipients")
	}
	if m.cfg.TenantID == "" || m.cfg.ClientID == "" || m.cfg.ClientSecret == "" || m.cfg.SenderMailbox == "" {
		return errors.New("mailer: incomplete Graph config")
	}

	payload, err := buildGraphPayload(to, subject, body, att)
	if err != nil {
		return fmt.Errorf("building payload: %w", err)
	}

	// First attempt with the cached token (may refresh). On 401, force a
	// refresh and retry exactly once — covers the case where the token was
	// revoked or the service-principal password rolled without the service
	// noticing.
	if err := m.postSendMail(ctx, payload, false); err != nil {
		if errors.Is(err, errUnauthorised) {
			return m.postSendMail(ctx, payload, true)
		}
		return err
	}
	return nil
}

var errUnauthorised = errors.New("graph: unauthorised")

func (m *Mailer) postSendMail(ctx context.Context, payload []byte, forceRefresh bool) error {
	token, err := m.getToken(ctx, forceRefresh)
	if err != nil {
		return fmt.Errorf("acquiring token: %w", err)
	}

	base := m.cfg.GraphBaseURL
	if base == "" {
		base = defaultGraphBaseURL
	}
	endpoint := fmt.Sprintf("%s/users/%s/sendMail", base, url.PathEscape(m.cfg.SenderMailbox))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST sendMail: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	// Graph returns 202 Accepted on success; anything 2xx is acceptable.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorised
	}
	return fmt.Errorf("graph sendMail returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// getToken returns a cached bearer or fetches a new one. Refreshes ~60s
// before expiry to avoid racing the clock. Caller passes force=true to
// bypass the cache after a 401.
func (m *Mailer) getToken(ctx context.Context, force bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !force && m.token != "" && time.Now().Before(m.tokenExp) {
		return m.token, nil
	}

	base := m.cfg.TokenBaseURL
	if base == "" {
		base = defaultTokenBaseURL
	}
	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token", base, url.PathEscape(m.cfg.TenantID))

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {m.cfg.ClientID},
		"client_secret": {m.cfg.ClientSecret},
		"scope":         {"https://graph.microsoft.com/.default"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("token endpoint returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("token response had empty access_token")
	}
	// Subtract 60s so we refresh before the token actually expires.
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl < 2*time.Minute {
		ttl = 2 * time.Minute // be generous if tenant hands back an unusually short TTL
	}
	m.token = tr.AccessToken
	m.tokenExp = time.Now().Add(ttl - 60*time.Second)
	return m.token, nil
}

// buildGraphPayload marshals the sendMail JSON body. Text bodies are sent
// as contentType=Text; anything containing a `<` is sent as HTML so
// simple `<br>`-bearing templates render correctly without callers
// needing a separate field.
func buildGraphPayload(to []string, subject, body string, att *Attachment) ([]byte, error) {
	contentType := "Text"
	if strings.Contains(body, "<") {
		contentType = "HTML"
	}

	type recipient struct {
		EmailAddress struct {
			Address string `json:"address"`
		} `json:"emailAddress"`
	}
	type attachmentJSON struct {
		ODataType    string `json:"@odata.type"`
		Name         string `json:"name"`
		ContentType  string `json:"contentType,omitempty"`
		ContentBytes string `json:"contentBytes"`
	}
	type messageJSON struct {
		Subject      string            `json:"subject"`
		Body         map[string]string `json:"body"`
		ToRecipients []recipient       `json:"toRecipients"`
		Attachments  []attachmentJSON  `json:"attachments,omitempty"`
	}
	type envelope struct {
		Message         messageJSON `json:"message"`
		SaveToSentItems bool        `json:"saveToSentItems"`
	}

	rcpts := make([]recipient, 0, len(to))
	for _, addr := range to {
		var r recipient
		r.EmailAddress.Address = addr
		rcpts = append(rcpts, r)
	}

	msg := messageJSON{
		Subject: subject,
		Body: map[string]string{
			"contentType": contentType,
			"content":     body,
		},
		ToRecipients: rcpts,
	}
	if att != nil && len(att.Body) > 0 {
		ct := att.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		msg.Attachments = []attachmentJSON{{
			ODataType:    "#microsoft.graph.fileAttachment",
			Name:         att.Filename,
			ContentType:  ct,
			ContentBytes: base64.StdEncoding.EncodeToString(att.Body),
		}}
	}

	return json.Marshal(envelope{Message: msg, SaveToSentItems: true})
}

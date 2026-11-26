// Package notify prepares and pushes operation outcomes to a webhook — the journals are
// write-only forensics; this is the page. One generic JSON POST per finished
// mutating verb, carrying both structured fields and a human "text" line
// (Slack-compatible; Discord/ntfy/generic consumers read the fields).
//
// Fail-open is the contract: a dead webhook returns an error for the caller
// to WARN on, and must never block, fail, or slow an operation beyond the
// send timeout. Error strings are the same redaction-safe strings printed to
// the terminal — secret content never travels, only hashes.
//
// A notification that names signing_secret_env is signed with Standard
// Webhooks (https://www.standardwebhooks.com): webhook-id, webhook-timestamp
// and webhook-signature headers over the exact body bytes. A signed send is
// retried, because a receiver that verifies signatures is usually one that
// records the event rather than a chat channel, within a total bound that
// keeps the fail-open contract. An unsigned send is one attempt, as before.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
)

const timeout = 5 * time.Second

// A signed send makes at most SignedAttempts attempts, each bounded by the
// per-request timeout, and all of them within SignedBudget. The budget is the
// whole cost a signed notification can add to an operation.
const (
	SignedAttempts = 3
	SignedBudget   = 15 * time.Second
)

// signedBackoff is the pause before each retry. A variable so tests do not
// wait for it.
var signedBackoff = []time.Duration{500 * time.Millisecond, time.Second}

// secretPrefix is the Standard Webhooks marker on a symmetric secret.
const secretPrefix = "whsec_"

// Payload is the notification: what ran, where, and how it ended.
type Payload struct {
	App      string `json:"app"`
	Env      string `json:"env,omitempty"`
	Host     string `json:"host,omitempty"`
	Verb     string `json:"verb"`
	DeployID string `json:"deploy_id,omitempty"`
	Status   string `json:"status"` // ok | fail
	Error    string `json:"error,omitempty"`
	Operator string `json:"operator,omitempty"`
	TS       string `json:"ts"`
	// Skipped marks work that did not run rather than work that failed. It
	// routes with the failures, because that is where an operator watches,
	// but it must not be announced as one: a scheduled job that stood aside
	// for a deploy did nothing wrong, and saying otherwise trains people to
	// ignore the channel.
	Skipped bool   `json:"skipped,omitempty"`
	Text    string `json:"text"` // human line, filled by Send
}

// Request is the stable HTTP representation of one selected notification.
// Preparing it separately lets host-fired work reuse the exact payload contract
// without requiring a resident Onebox process.
type Request struct {
	Body        []byte
	ContentType string
	Title       string
}

// event maps a payload to the config's `on` vocabulary.
func (p Payload) event() string {
	if p.Status == "ok" {
		return "success"
	}
	return "failure"
}

func (p Payload) text() string {
	id := p.DeployID
	if id != "" {
		id = " " + id
	}
	if p.Status == "ok" {
		return fmt.Sprintf("✅ %s: %s%s succeeded on %s", p.App, p.Verb, id, p.Host)
	}
	if p.Skipped {
		return fmt.Sprintf("⏭️ %s: %s%s did not run on %s — %s", p.App, p.Verb, id, p.Host, p.Error)
	}
	// The id belongs on the failure line most of all: a text webhook sends
	// this sentence and nothing else, so without it the one notification an
	// operator acts on cannot name the run they should go and read.
	return fmt.Sprintf("🚨 %s: %s%s FAILED on %s — %s", p.App, p.Verb, id, p.Host, p.Error)
}

// Prepare renders a selected payload without sending it. An unset webhook and
// a filtered outcome return nil so every caller shares the same event policy.
func Prepare(cfg app.Notification, p Payload) (*Request, error) {
	if cfg.Webhook == "" {
		return nil, nil
	}
	selected := false
	for _, on := range cfg.On {
		if on == p.event() {
			selected = true
			break
		}
	}
	if !selected {
		return nil, nil
	}
	if p.TS == "" {
		p.TS = time.Now().UTC().Format(time.RFC3339)
	}
	// Diagnostic errors may contain provider output, query text, or credentials.
	// Notifications cross the host trust boundary, so they carry only the stable
	// outcome; operators use the trusted local diagnostics for details.
	if p.Status != "ok" && p.Error != "" {
		p.Error = "operation failed; inspect trusted local diagnostics"
		if p.Skipped {
			// The line already says the run did not happen, so this is only
			// where to look for why.
			p.Error = "inspect trusted host diagnostics"
		}
	}
	p.Text = p.text()
	contentType := "application/json"
	var body []byte
	if cfg.Format == "text" {
		// ntfy-style topic endpoints render the body verbatim — send the
		// human line only, with the title as a header
		body, contentType = []byte(p.Text), "text/plain"
	} else {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		body = b
	}
	return &Request{
		Body: body, ContentType: contentType,
		Title: strings.TrimSpace(p.App + " " + p.Verb),
	}, nil
}

// SigningKey resolves cfg's signing secret from the local environment. It
// returns nil for an unsigned notification. Errors name the variable and never
// carry its value.
func SigningKey(cfg app.Notification) ([]byte, error) {
	if cfg.SigningSecretEnv == "" {
		return nil, nil
	}
	value := os.Getenv(cfg.SigningSecretEnv)
	if value == "" {
		return nil, fmt.Errorf("signing secret env var %s is empty; export the Standard Webhooks secret (whsec_...) to sign this notification", cfg.SigningSecretEnv)
	}
	key, err := ParseSigningSecret(value)
	if err != nil {
		return nil, fmt.Errorf("signing secret env var %s: %w", cfg.SigningSecretEnv, err)
	}
	return key, nil
}

// ParseSigningSecret decodes a Standard Webhooks secret, whsec_ followed by
// standard base64, into the HMAC key. Errors describe the shape only.
func ParseSigningSecret(secret string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(secret, secretPrefix)
	if !ok {
		return nil, errors.New("not a Standard Webhooks secret: it must start with whsec_")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) == 0 {
		return nil, errors.New("not a Standard Webhooks secret: the part after whsec_ must be non-empty base64")
	}
	return key, nil
}

// Signature is the webhook-signature header value for one attempt: version 1,
// an HMAC-SHA256 over "id.timestamp.body".
func Signature(key []byte, id string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// newMessageID is the webhook-id for one logical notification: random, made
// once, and reused by every retry so a receiver can drop the duplicates.
func newMessageID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "msg_" + hex.EncodeToString(b[:]), nil
}

// Send fires the webhook if the payload's outcome is selected by cfg.On. An
// unset webhook and a filtered outcome are silent no-ops. Callers treat a
// returned error as a warning — never as the operation's result.
func Send(ctx context.Context, cfg app.Notification, p Payload) error {
	prepared, err := Prepare(cfg, p)
	if err != nil || prepared == nil {
		return err
	}
	if cfg.SigningSecretEnv == "" {
		_, err := post(ctx, cfg.Webhook, prepared, nil)
		return err
	}
	key, err := SigningKey(cfg)
	if err != nil {
		return err
	}
	id, err := newMessageID()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, SignedBudget)
	defer cancel()
	var lastErr error
	for attempt := 1; attempt <= SignedAttempts; attempt++ {
		now := time.Now().Unix()
		headers := map[string]string{
			"webhook-id":        id,
			"webhook-timestamp": strconv.FormatInt(now, 10),
			"webhook-signature": Signature(key, id, now, prepared.Body),
		}
		retry, err := post(ctx, cfg.Webhook, prepared, headers)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry || attempt == SignedAttempts || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(signedBackoff[attempt-1]):
		}
		if ctx.Err() != nil {
			break
		}
	}
	if errors.Is(lastErr, context.DeadlineExceeded) && ctx.Err() != nil {
		return fmt.Errorf("webhook not delivered within %s: %w", SignedBudget, lastErr)
	}
	return lastErr
}

// post makes one attempt. retry reports whether the failure is one a retry
// could fix: a network error, a 429, or a 5xx. A 4xx is the receiver refusing
// this request, and the same request will be refused again.
func post(ctx context.Context, webhook string, prepared *Request, headers map[string]string) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(prepared.Body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", prepared.ContentType)
	req.Header.Set("X-Title", prepared.Title)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: timeout}
	res, err := client.Do(req)
	if err != nil {
		return true, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500,
			fmt.Errorf("webhook returned %d", res.StatusCode)
	}
	return false, nil
}

// Package notify pushes operation outcomes to a webhook — the journals are
// write-only forensics; this is the page. One generic JSON POST per finished
// mutating verb, carrying both structured fields and a human "text" line
// (Slack-compatible; Discord/ntfy/generic consumers read the fields).
//
// Fail-open is the contract: a dead webhook returns an error for the caller
// to WARN on, and must never block, fail, or slow an operation beyond the
// send timeout. Error strings are the same redaction-safe strings printed to
// the terminal — secrets content never travels, only hashes (design §07).
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/yeet/internal/config"
)

const timeout = 5 * time.Second

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
	Text     string `json:"text"` // human line, filled by Send
}

// event maps a payload to the config's `on` vocabulary.
func (p Payload) event() string {
	if p.Status == "ok" {
		return "success"
	}
	return "failure"
}

func (p Payload) text() string {
	if p.Status == "ok" {
		id := p.DeployID
		if id != "" {
			id = " " + id
		}
		return fmt.Sprintf("✅ %s: %s%s succeeded on %s", p.App, p.Verb, id, p.Host)
	}
	return fmt.Sprintf("🚨 %s: %s FAILED on %s — %s", p.App, p.Verb, p.Host, p.Error)
}

// Send fires the webhook if the payload's outcome is selected by cfg.On.
// nil cfg and filtered outcomes are silent no-ops. Callers treat a returned
// error as a warning — never as the operation's result.
func Send(cfg *config.Notify, p Payload) error {
	if cfg == nil || cfg.Webhook == "" {
		return nil
	}
	selected := false
	for _, on := range cfg.On {
		if on == p.event() {
			selected = true
			break
		}
	}
	if !selected {
		return nil
	}
	if p.TS == "" {
		p.TS = time.Now().UTC().Format(time.RFC3339)
	}
	p.Text = p.text()
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: timeout}
	res, err := client.Post(cfg.Webhook, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("webhook returned %d", res.StatusCode)
	}
	return nil
}

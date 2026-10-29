package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
)

// webhooks delivers job completion events. Payloads are signed with
// HMAC-SHA256 over "<timestamp>.<body>" (header X-Cleads-Signature), so the
// receiver can verify origin and reject replays.
type webhooks struct {
	secret []byte
	client *http.Client
	log    *slog.Logger
	wg     sync.WaitGroup
}

func newWebhooks(secret string, log *slog.Logger) *webhooks {
	return &webhooks{secret: []byte(secret), client: source.NewPublicHTTPClient(10*time.Second, 4), log: log}
}

type webhookEvent struct {
	Event string     `json:"event"`
	Job   domain.Job `json:"job"`
}

func (w *webhooks) send(j domain.Job) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		body, _ := json.Marshal(webhookEvent{Event: "job." + string(j.Status), Job: j})
		for attempt := 0; attempt < 4; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(1<<attempt) * time.Second)
			}
			if err := w.post(j.Spec.WebhookURL, body); err != nil {
				w.log.Warn("webhook delivery failed", "job", j.ID, "attempt", attempt+1, "err", err)
				continue
			}
			return
		}
	}()
}

func (w *webhooks) post(url string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CLEADS-Webhooks/1.0")
	req.Header.Set("X-Cleads-Timestamp", ts)
	if len(w.secret) > 0 {
		req.Header.Set("X-Cleads-Signature", "sha256="+Sign(w.secret, ts, body))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}

// Sign computes the webhook signature.
func Sign(secret []byte, ts string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts))
	m.Write([]byte{'.'})
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func (w *webhooks) wait(d time.Duration) {
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

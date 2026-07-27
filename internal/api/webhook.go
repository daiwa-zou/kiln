package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/daiwa-zou/kiln/internal/store"
)

// GitHub webhook ingress. This is the door that keeps wikis fresh
// unattended: a push event maps to the connectors watching that repository
// and enqueues a run. It sits outside auth.Wrap — GitHub cannot carry a kiln
// token — so its own defenses are strict: HMAC signature over the raw body,
// a payload size cap, and a dedicated rate limit, all before any parsing
// beyond the envelope.

// HookStore is what webhook handling needs from persistence.
type HookStore interface {
	WebhookConnectors(ctx context.Context) ([]store.ConnectorRow, error)
	LastRunFinishedAt(ctx context.Context, workspaceID string) (time.Time, error)
	EnqueueRunOpts(ctx context.Context, workspaceID, trigger, connectorID string, opts store.EnqueueOptions) (string, bool, error)
	UpsertGitHubInstallationEvent(ctx context.Context, installationID int64, accountLogin, accountType string, suspended bool) error
	RemoveGitHubInstallation(ctx context.Context, installationID int64) error
}

// maxWebhookBytes caps the payload before the HMAC is even computed. Push
// payloads run tens of kilobytes; a megabyte is generous. Delivery rate is
// bounded by the shared token-bucket limiter (per remote host here, since
// webhooks carry no identity): GitHub retries politely, so only a hostile
// sender ever sees the 429.
const maxWebhookBytes = 1 << 20

// handleGitHubWebhook verifies and dispatches one delivery.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}
	if len(body) > maxWebhookBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload too large"})
		return
	}

	// The signature covers the raw body with the shared webhook secret. An
	// invalid or absent signature is a 401 and nothing else happens — this
	// is the M3 verification case.
	if !validSignature(s.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
	case "push":
		s.handlePushEvent(w, r.Context(), body)
	case "installation":
		s.handleInstallationEvent(w, r.Context(), body)
	default:
		// Unknown events are acknowledged, not errored: GitHub disables hooks
		// that keep failing, and new event types must not break ingestion.
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "event": event})
	}
}

func validSignature(secret []byte, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}

func (s *Server) handlePushEvent(w http.ResponseWriter, ctx context.Context, body []byte) {
	var push struct {
		After      string `json:"after"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &push); err != nil || push.Repository.FullName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed push payload"})
		return
	}

	connectors, err := s.Hooks.WebhookConnectors(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}

	enqueued := 0
	for _, c := range connectors {
		remoteURL, _ := c.Config["url"].(string)
		if !repoMatches(remoteURL, push.Repository.FullName) {
			continue
		}

		// Completion cooldown, expressed as scheduling rather than skipping:
		// the push always enqueues (the active-run index collapses storms),
		// but a run created inside the cooldown carries not_before so it
		// waits out the quiet period. Nothing is dropped — a debounced push
		// advances the waiting run's ref_to, and the range base derives from
		// the last build, so late pushes cannot punch holes in the diff.
		opts := store.EnqueueOptions{RefTo: push.After}
		if s.WebhookCooldown > 0 {
			finished, err := s.Hooks.LastRunFinishedAt(ctx, c.WorkspaceID)
			if err != nil {
				s.fail(w, err)
				return
			}
			if !finished.IsZero() && time.Since(finished) < s.WebhookCooldown {
				opts.NotBefore = finished.Add(s.WebhookCooldown)
			}
		}

		if _, _, err := s.Hooks.EnqueueRunOpts(ctx, c.WorkspaceID, "webhook", c.ID, opts); err != nil {
			s.fail(w, err)
			return
		}
		enqueued++
	}

	// 202 whether or not anything matched: a webhook pointed at the wrong
	// kiln must not learn which repositories this one watches.
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "enqueued": enqueued})
}

// repoMatches reports whether a connector's remote URL names the pushed
// repository, comparing the URL path against owner/name with an optional
// .git suffix.
func repoMatches(remoteURL, fullName string) bool {
	if remoteURL == "" || fullName == "" {
		return false
	}
	path := remoteURL
	if i := strings.Index(path, "://"); i >= 0 {
		path = path[i+3:]
	}
	if i := strings.IndexByte(path, '/'); i >= 0 {
		path = path[i+1:]
	} else {
		return false
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	return strings.EqualFold(path, fullName)
}

func (s *Server) handleInstallationEvent(w http.ResponseWriter, ctx context.Context, body []byte) {
	var ev struct {
		Action       string `json:"action"`
		Installation struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"account"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &ev); err != nil || ev.Installation.ID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed installation payload"})
		return
	}

	var err error
	switch ev.Action {
	case "deleted":
		// The mirror row goes; user links cascade. Workspaces and pages stay
		// — destruction of content is always a human decision.
		err = s.Hooks.RemoveGitHubInstallation(ctx, ev.Installation.ID)
	case "suspend":
		err = s.Hooks.UpsertGitHubInstallationEvent(ctx, ev.Installation.ID,
			ev.Installation.Account.Login, ev.Installation.Account.Type, true)
	default: // created, unsuspend, new_permissions_accepted
		err = s.Hooks.UpsertGitHubInstallationEvent(ctx, ev.Installation.ID,
			ev.Installation.Account.Login, ev.Installation.Account.Type, false)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "action": ev.Action})
}

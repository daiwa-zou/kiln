package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/daiwa-zou/kiln/internal/jobs"
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
// payloads run tens of kilobytes; a megabyte is generous.
const maxWebhookBytes = 1 << 20

// Rate limiting a webhook is a different problem from limiting a person, and
// the usual key (the remote address) is wrong for it. Behind an ingress or
// load balancer every delivery arrives from the proxy's address, so one bucket
// would be shared by every repository GitHub talks to about, and GitHub does
// not redeliver a failed hook: a 429 there is a push that never builds.
//
// So the two cases are limited on different keys:
//
//   - A delivery that fails verification has no identity worth trusting, so it
//     is limited by the peer address (never X-Forwarded-For, which the sender
//     writes). Behind a proxy that address is shared, which is acceptable here:
//     it can only slow down other unauthenticated requests, never a signed one.
//   - A delivery whose HMAC verified came from the holder of the webhook
//     secret, so its repository (or installation) is trustworthy, and that is
//     the key. One noisy repository can then only exhaust its own bucket.
//
// A signed delivery never consults the address bucket, so a flood of garbage
// through the same ingress cannot cost a real push its build.

// handleGitHubWebhook verifies and dispatches one delivery.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes+1))
	if err != nil {
		s.rejectHook(w, r, http.StatusBadRequest, "unreadable body")
		return
	}
	if len(body) > maxWebhookBytes {
		s.rejectHook(w, r, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}

	// The signature covers the raw body with the shared webhook secret. An
	// invalid or absent signature is a 401 and nothing else happens — this
	// is the M3 verification case.
	if !validSignature(s.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		s.rejectHook(w, r, http.StatusUnauthorized, "invalid signature")
		return
	}

	// Only now is the payload's own account of who sent it believable.
	if !s.hookLimit.take(hookSubject(r.Header.Get("X-GitHub-Event"), body)) {
		w.Header().Set("Retry-After", "3")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "webhook rate limit exceeded for this repository"})
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

// rejectHook answers a delivery that failed before verification, charging it to
// the peer address's bucket. Once that bucket is empty the answer becomes a
// 429, which is what bounds an unauthenticated flood.
func (s *Server) rejectHook(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if !s.hookRejectLimit.take(limiterKey(r)) {
		w.Header().Set("Retry-After", "3")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "webhook rate limit exceeded"})
		return
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

// hookSubject names what a verified delivery is about, for rate limiting: the
// repository for repository-scoped events, else the App installation, else the
// event type. Callers must have verified the signature first; the body is
// otherwise attacker-chosen.
func hookSubject(event string, body []byte) string {
	var env struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	// A body that does not parse falls through to the event key: it is signed,
	// so it is not hostile, merely not something we can attribute.
	_ = json.Unmarshal(body, &env)
	switch {
	case env.Repository.FullName != "":
		// Repository names are case-insensitive on GitHub.
		return "repo:" + strings.ToLower(env.Repository.FullName)
	case env.Installation.ID != 0:
		return "installation:" + strconv.FormatInt(env.Installation.ID, 10)
	}
	return "event:" + event
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
	var push pushEvent
	if err := json.Unmarshal(body, &push); err != nil || push.Repository.FullName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed push payload"})
		return
	}

	connectors, err := s.Hooks.WebhookConnectors(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}

	enqueued, skipped := 0, 0
	for _, c := range connectors {
		remoteURL, _ := c.Config["url"].(string)
		if !repoMatches(remoteURL, push.Repository.FullName) {
			continue
		}

		// A wiki describes one branch. Building it for a push to a feature
		// branch would regenerate the tracked branch's pages from a tree that
		// did not change and spend budget on a push it does not read.
		if !push.targets(connectorBranch(c.Config)) {
			skipped++
			continue
		}

		// Completion cooldown, expressed as scheduling rather than skipping:
		// the push always enqueues (the active-run index collapses storms),
		// but a run created inside the cooldown carries not_before so it
		// waits out the quiet period. Nothing is dropped — a push that lands
		// on a waiting build advances its ref_to, and one that finds the bench
		// busy with research or a running build is recorded and queued as a
		// build when that finishes. The range base derives from the last
		// build, so late pushes cannot punch holes in the diff.
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

		if _, _, err := s.Hooks.EnqueueRunOpts(ctx, c.WorkspaceID, jobs.TriggerWebhook, c.ID, opts); err != nil {
			s.fail(w, err)
			return
		}
		enqueued++
	}

	// Always 2xx, and never an error for a push we chose not to build: GitHub
	// does not redeliver failures, and a hook that keeps failing is disabled.
	// An unmatched repository still reads as plain "accepted", so a webhook
	// pointed at the wrong kiln cannot learn which repositories this one watches.
	if enqueued == 0 && skipped > 0 {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":   "ignored",
			"enqueued": 0,
			"message":  "push to " + push.Ref + " ignored: connectors build their tracked branch only",
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "enqueued": enqueued})
}

// pushEvent is the part of a push payload ingress reads.
type pushEvent struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`

	Repository struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
}

// targets reports whether the push updated the branch a connector builds:
// branch when the connector names one, else the repository's default branch.
// Tag pushes, branch deletions, and every other branch are not builds.
func (p pushEvent) targets(branch string) bool {
	if p.Deleted {
		return false
	}
	pushed, ok := strings.CutPrefix(p.Ref, "refs/heads/")
	if !ok || pushed == "" {
		return false
	}
	if branch == "" {
		branch = p.Repository.DefaultBranch
	}
	// Branch names are case-sensitive on GitHub.
	return branch != "" && pushed == branch
}

// connectorBranch is the branch a git connector asked to track, or "" for the
// repository's default branch. The clone reads the same key (see
// gitconn.CloneOptions.Branch), so what is built and what triggers a build
// cannot disagree.
func connectorBranch(cfg map[string]any) string {
	b, _ := cfg["branch"].(string)
	return strings.TrimSpace(b)
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

package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/secret"
)

// WorkspaceState is the cached snapshot of a recent auth.test sweep.
// Surfaced to the UI so operators can spot a revoked or misconfigured
// bot token without trawling logs.
type WorkspaceState struct {
	LastChecked time.Time
	Healthy     bool
	TeamID      string // shared team_id across every configured token, empty when no consensus reached
	TeamName    string
	Error       string // human-readable; non-empty iff Healthy=false
}

// WorkspaceWatcher periodically verifies every configured Slack bot
// token with auth.test, enforces the single-workspace invariant, and
// exposes the latest state to the UI.
type WorkspaceWatcher struct {
	client *Client
	// tokens maps tokenEnv name → resolved token value. Multiple
	// channels can share a tokenEnv (we treat that as one token).
	tokens map[string]secret.SecretString
	log    *slog.Logger

	mu    sync.RWMutex
	state WorkspaceState
}

// NewWorkspaceWatcher builds a watcher over the given token set.
func NewWorkspaceWatcher(tokens map[string]secret.SecretString, client *Client, log *slog.Logger) *WorkspaceWatcher {
	if log == nil {
		log = slog.Default()
	}
	return &WorkspaceWatcher{client: client, tokens: tokens, log: log}
}

// ErrMultiWorkspace is returned by VerifyOnce when distinct tokens
// resolve to different Slack workspace team_ids — v1 is
// single-workspace only and the binary refuses to start.
var ErrMultiWorkspace = errors.New("slack tokens span multiple workspaces")

// VerifyOnce calls auth.test for every distinct token and updates
// cached state. Returns ErrMultiWorkspace if responses succeed but
// disagree on team_id. Transient auth.test failures do NOT return an
// error — they are surfaced via State() so the UI shows them while
// startup proceeds.
func (w *WorkspaceWatcher) VerifyOnce(ctx context.Context) error {
	keys := w.sortedTokenKeys()
	if len(keys) == 0 {
		w.setState(WorkspaceState{LastChecked: time.Now(), Healthy: true})
		return nil
	}

	teamID := ""
	teamName := ""
	var firstErr error
	for _, k := range keys {
		tok := w.tokens[k]
		res, err := w.client.AuthTest(ctx, tok)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("token %s: %w", k, err)
			}
			w.log.Warn("auth.test failed", "tokenEnv", k, "error", err)
			continue
		}
		if teamID == "" {
			teamID = res.TeamID
			teamName = res.Team
			continue
		}
		if res.TeamID != teamID {
			err := fmt.Errorf("%w: %s → %s, %s → %s", ErrMultiWorkspace, keys[0], teamID, k, res.TeamID)
			w.setState(WorkspaceState{LastChecked: time.Now(), Healthy: false, Error: err.Error()})
			return err
		}
	}

	now := time.Now()
	if firstErr != nil {
		// Transient failure: keep the most-recent successful team info if any.
		w.setState(WorkspaceState{
			LastChecked: now,
			Healthy:     false,
			TeamID:      teamID,
			TeamName:    teamName,
			Error:       firstErr.Error(),
		})
		return nil
	}
	w.setState(WorkspaceState{
		LastChecked: now,
		Healthy:     true,
		TeamID:      teamID,
		TeamName:    teamName,
	})
	return nil
}

// Run kicks off the periodic re-check loop. Returns once ctx is
// cancelled. Errors from VerifyOnce other than ErrMultiWorkspace are
// logged and otherwise ignored — by definition the periodic loop only
// runs after startup successfully verified single-workspace, so
// subsequent multi-workspace would be a config change requiring a
// restart anyway.
func (w *WorkspaceWatcher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = w.VerifyOnce(ctx)
		}
	}
}

// State returns a copy of the cached workspace state.
func (w *WorkspaceWatcher) State() WorkspaceState {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.state
}

func (w *WorkspaceWatcher) setState(s WorkspaceState) {
	w.mu.Lock()
	w.state = s
	w.mu.Unlock()
}

// sortedTokenKeys returns the token-env keys in deterministic order so
// error messages and tests are stable.
func (w *WorkspaceWatcher) sortedTokenKeys() []string {
	keys := make([]string, 0, len(w.tokens))
	for k := range w.tokens {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

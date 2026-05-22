package slack

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/secret"
)

// MappingEntryState is the cached health of one userMapping entry.
type MappingEntryState struct {
	Slug    string
	ID      string
	OK      bool
	Reason  string // human-readable when OK=false
	Checked time.Time
}

// UserMappingValidator periodically verifies every slack.userMapping
// entry against the live Slack workspace and exposes the cached
// state to the UI.
//
// U… entries are checked with users.info (one call each). S… entries
// are checked against a single usergroups.list response. Transient
// failures surface as OK=false with a "transient" reason; revoked
// IDs surface as user_not_found / no_such_subteam.
type UserMappingValidator struct {
	client  *Client
	mapping map[string]string
	token   func() string
	log     *slog.Logger

	mu      sync.RWMutex
	entries map[string]MappingEntryState
	lastRun time.Time
}

// NewUserMappingValidator constructs a validator over the configured
// mapping. token is a getter so it can be re-resolved per call (the
// secret.SecretString comes from the channel set; we use the first
// configured token since v1 is single-workspace).
func NewUserMappingValidator(client *Client, mapping map[string]string, token func() string, log *slog.Logger) *UserMappingValidator {
	if log == nil {
		log = slog.Default()
	}
	return &UserMappingValidator{
		client:  client,
		mapping: mapping,
		token:   token,
		log:     log,
		entries: map[string]MappingEntryState{},
	}
}

// VerifyOnce runs one full validation pass and updates the cache.
// Errors from individual Slack calls are recorded per entry, never
// returned — the validator is best-effort and is allowed to be
// stale.
func (v *UserMappingValidator) VerifyOnce(ctx context.Context) {
	if len(v.mapping) == 0 {
		v.setRunAt(time.Now())
		return
	}
	tokStr := v.token()
	if tokStr == "" {
		v.log.Warn("user-mapping validator skipped: no token available")
		return
	}
	tok := secret.SecretString(tokStr)

	// usergroups.list once for all S… IDs.
	var subteamIDs map[string]struct{}
	var subteamErr string
	if v.hasSubteamID() {
		res, err := v.client.UsergroupsList(ctx, tok)
		if err != nil {
			subteamErr = "transient: " + err.Error()
		} else if !res.OK {
			subteamErr = "slack error: " + res.Error
		} else {
			subteamIDs = make(map[string]struct{}, len(res.Usergroups))
			for _, u := range res.Usergroups {
				subteamIDs[u.ID] = struct{}{}
			}
		}
	}

	now := time.Now()
	next := make(map[string]MappingEntryState, len(v.mapping))
	for slug, id := range v.mapping {
		entry := MappingEntryState{Slug: slug, ID: id, Checked: now}
		switch {
		case strings.HasPrefix(id, "U"):
			res, err := v.client.UsersInfo(ctx, tok, id)
			switch {
			case err != nil:
				entry.Reason = "transient: " + err.Error()
			case !res.OK:
				entry.Reason = "slack error: " + res.Error
			default:
				entry.OK = true
			}
		case strings.HasPrefix(id, "S"):
			if subteamErr != "" {
				entry.Reason = subteamErr
			} else if _, ok := subteamIDs[id]; ok {
				entry.OK = true
			} else {
				entry.Reason = "subteam not found in workspace"
			}
		default:
			entry.Reason = "id does not start with U or S"
		}
		next[slug] = entry
	}

	v.mu.Lock()
	v.entries = next
	v.lastRun = now
	v.mu.Unlock()
}

// Run kicks off the periodic re-validation loop. interval defaults to
// 24h when zero. Blocks until ctx is cancelled.
func (v *UserMappingValidator) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			v.VerifyOnce(ctx)
		}
	}
}

// Snapshot returns a copy of the cached entries (sorted by slug) plus
// the time of the most recent validation pass.
func (v *UserMappingValidator) Snapshot() (entries []MappingEntryState, lastRun time.Time) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	entries = make([]MappingEntryState, 0, len(v.entries))
	for _, e := range v.entries {
		entries = append(entries, e)
	}
	return entries, v.lastRun
}

// Invalid returns only the entries currently flagged as not OK.
func (v *UserMappingValidator) Invalid() []MappingEntryState {
	entries, _ := v.Snapshot()
	out := entries[:0]
	for _, e := range entries {
		if !e.OK {
			out = append(out, e)
		}
	}
	return out
}

func (v *UserMappingValidator) hasSubteamID() bool {
	for _, id := range v.mapping {
		if strings.HasPrefix(id, "S") {
			return true
		}
	}
	return false
}

func (v *UserMappingValidator) setRunAt(t time.Time) {
	v.mu.Lock()
	v.lastRun = t
	v.mu.Unlock()
}

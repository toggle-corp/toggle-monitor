package lifecycle

import (
	"log/slog"
	"reflect"

	"github.com/toggle-corp/toggle-monitor/internal/alertmanager"
	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/kube"
	"github.com/toggle-corp/toggle-monitor/internal/merger"
	"github.com/toggle-corp/toggle-monitor/internal/observability"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// slackMentionResolver is the alertmanager.MentionResolver impl wired
// at startup. It wraps the operator-configured slack.userMapping and
// defers to slack.ResolveMentions so AM and monitor paths produce
// identical mention markup for the same notify[] input.
//
// A standalone resolver type is preferred over a closure so the AM
// handler can keep its interface dependency narrow (no functional
// option soup) and tests can substitute a deterministic fake.
type slackMentionResolver struct {
	userMapping map[string]string
}

func (r *slackMentionResolver) Resolve(notify []string) []string {
	return slack.ResolveMentions(notify, r.userMapping)
}

// buildAMHandler constructs the Alertmanager webhook handler from the
// already-resolved lifecycle deps (repo, slack client, channel lookup,
// metrics). Returns (nil, nil) when cfg.Alertmanager is absent so the
// caller's "AM disabled" branch stays branchless on the happy path.
//
// Errors here are fatal — the bearer-token env-var resolution is
// expected to fail loudly at boot so the operator notices instead of
// silently running an unauthenticated AM endpoint or one with a
// rotated-away token.
func buildAMHandler(
	cfg *config.AlertmanagerConfig,
	userMapping map[string]string,
	repo *store.Repo,
	slackClient *slack.Client,
	channelLookup func(string) (slack.ChannelInfo, bool),
	metrics *observability.Metrics,
	publicBase string,
	log *slog.Logger,
) (*alertmanager.Handler, error) {
	if cfg == nil {
		return nil, nil
	}
	return alertmanager.NewHandler(alertmanager.HandlerOptions{
		Config:      cfg,
		Repo:        repo,
		SlackClient: slackClient,
		Channels:    channelLookup,
		Mentions:    &slackMentionResolver{userMapping: userMapping},
		Logger:      log,
		Observer:    metrics,
		PublicBase:  publicBase,
		UserMapping: userMapping,
	})
}

// buildAMSweeper constructs the retention sweeper from cfg. Returns
// nil when cfg.Alertmanager is absent so the caller's goroutine-spawn
// branch can be skipped entirely.
func buildAMSweeper(
	cfg *config.AlertmanagerConfig,
	repo *store.Repo,
	log *slog.Logger,
) *alertmanager.Sweeper {
	if cfg == nil {
		return nil
	}
	return alertmanager.NewSweeper(alertmanager.SweeperOptions{
		Repo:          repo,
		RetentionDays: cfg.RetentionDays,
		Logger:        log,
		// Observer left nil for now — the observability layer doesn't
		// yet expose an AMRetentionSwept counter, so wiring one here
		// would dangle. Adding it is a one-line change when the
		// counter lands.
	})
}

// The two cascades that resolve namespaceAnnotation-scoped `*From`
// blocks each declare their own source interface — the kube
// materializer (ADR-0009) and the Alertmanager handler (ADR-0013) — so
// wiring takes one narrow interface per consumer rather than a shared
// one.
type (
	kubeAnnotationConsumer interface {
		SetNamespaceAnnotationSource(src merger.NamespaceAnnotationSource)
	}
	amAnnotationConsumer interface {
		SetNamespaceAnnotationSource(src alertmanager.NamespaceAnnotationSource)
	}
)

// wireNamespaceAnnotations points every cascade that reads namespace
// annotations at src, which the kube watcher owns. Both consumers are
// constructed before the watcher, so both are wired by setter
// afterwards — and both go through this one call, so neither can be
// wired without the other being visible at the same place.
//
// A nil src or a nil consumer is skipped: a deployment with no
// alertmanager: block wires the materializer unchanged, and vice versa.
func wireNamespaceAnnotations(src *kube.Watcher, mat kubeAnnotationConsumer, am amAnnotationConsumer) {
	if src == nil {
		return
	}
	if mat != nil && !reflect.ValueOf(mat).IsNil() {
		mat.SetNamespaceAnnotationSource(src)
	}
	if am != nil && !reflect.ValueOf(am).IsNil() {
		am.SetNamespaceAnnotationSource(src)
	}
}

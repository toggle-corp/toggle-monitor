package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/secret"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// newSlackCmd is the `slack` subcommand group. Currently only `test`
// lives under it.
func newSlackCmd() *cobra.Command {
	g := &cobra.Command{
		Use:   "slack",
		Short: "Slack utilities",
	}
	g.AddCommand(newSlackTestCmd())
	return g
}

// newSlackTestCmd groups the uptime + ssl workflow runners.
func newSlackTestCmd() *cobra.Command {
	g := &cobra.Command{
		Use:   "test",
		Short: "Post a fake alert workflow to a real Slack channel (sanity check)",
		Long: "Posts a simulated incident lifecycle to the channel " +
			"identified by --channel so you can eyeball the message " +
			"shape, colors, and threading without waiting for a real " +
			"monitor to flap. Uses real chat.postMessage and chat.update " +
			"calls against the configured bot token — pick a low-traffic " +
			"channel.",
	}
	g.AddCommand(newSlackTestUptimeCmd())
	g.AddCommand(newSlackTestSSLCmd())
	return g
}

// slackTestFlags is the common flag set for both subcommands.
type slackTestFlags struct {
	ConfigPath  string
	ChannelSlug string
	Name        string
	Reminders   int
	Interval    time.Duration
	NoPrompt    bool
	Notify      []string // userMapping slugs and/or raw Slack markup
	Dependents  []string // simulated dependent monitor slugs (uptime only)
}

func addSlackTestFlags(cmd *cobra.Command, f *slackTestFlags) {
	cmd.Flags().StringVar(&f.ConfigPath, "config", "deploy/local/config.yaml", "path to the YAML config")
	cmd.Flags().StringVar(&f.ChannelSlug, "channel", "", "Slack channel slug from config.slack.channels (required)")
	cmd.Flags().StringVar(&f.Name, "name", "test-monitor", "friendly name used in the simulated messages")
	cmd.Flags().IntVar(&f.Reminders, "reminders", 2, "number of reminder messages to post in-thread")
	cmd.Flags().DurationVar(&f.Interval, "interval", time.Second, "delay between simulated events")
	cmd.Flags().BoolVar(&f.NoPrompt, "no-prompt", false, "skip the 'press Enter to resolve' prompt; resolve immediately")
	cmd.Flags().StringSliceVar(&f.Notify, "notify", nil,
		"mentions to attach to the parent message — userMapping slug (e.g. oncall) or raw Slack markup "+
			"(e.g. '<!here>', '<@U123>'). Repeatable / comma-separated.")
	cmd.Flags().StringSliceVar(&f.Dependents, "dependents", nil,
		"simulate cascading effect: dependent monitor slugs that would be paused (down) / resumed (resolve) "+
			"while this monitor is down. Repeatable / comma-separated. Renders a small dim note above the footer.")
	_ = cmd.MarkFlagRequired("channel")
}

func newSlackTestUptimeCmd() *cobra.Command {
	var f slackTestFlags
	cmd := &cobra.Command{
		Use:   "uptime",
		Short: "Post a Down → reminders → Resolve uptime workflow",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSlackTestUptime(cmd.Context(), cmd.OutOrStdout(), cmd.InOrStdin(), f)
		},
	}
	addSlackTestFlags(cmd, &f)
	return cmd
}

func newSlackTestSSLCmd() *cobra.Command {
	var f slackTestFlags
	cmd := &cobra.Command{
		Use:   "ssl",
		Short: "Post an SSL expiring → reminders → Renewed workflow",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSlackTestSSL(cmd.Context(), cmd.OutOrStdout(), cmd.InOrStdin(), f)
		},
	}
	addSlackTestFlags(cmd, &f)
	return cmd
}

// resolvedTarget bundles what the test workflow needs from the config:
// the destination channel ID, the bot token, and the userMapping (so
// --notify values can resolve to real Slack markup).
type resolvedTarget struct {
	ChannelID   string
	Token       secret.SecretString
	UserMapping map[string]string
}

// resolveTarget loads the config, finds the channel by slug, and
// returns everything the workflow runners need. Errors are
// user-actionable (missing channel, missing env).
func resolveTarget(cfgPath, slug string) (resolvedTarget, error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return resolvedTarget{}, fmt.Errorf("read config %q: %w", cfgPath, err)
	}
	cfg, err := config.Load(data)
	if err != nil {
		return resolvedTarget{}, fmt.Errorf("load config: %w", err)
	}
	for _, ch := range cfg.Slack.Channels {
		if ch.Slug != slug {
			continue
		}
		raw := os.Getenv(ch.TokenEnv)
		if raw == "" {
			return resolvedTarget{}, fmt.Errorf("env var %q (tokenEnv for channel %q) is empty", ch.TokenEnv, slug)
		}
		return resolvedTarget{
			ChannelID:   ch.ChannelID,
			Token:       secret.SecretString(raw),
			UserMapping: cfg.Slack.UserMapping,
		}, nil
	}
	known := make([]string, 0, len(cfg.Slack.Channels))
	for _, ch := range cfg.Slack.Channels {
		known = append(known, ch.Slug)
	}
	return resolvedTarget{}, fmt.Errorf("channel slug %q not found in config (known: %v)", slug, known)
}

// promptResolve blocks until the user presses Enter, unless NoPrompt
// is set. Output goes to out so we can be redirected in tests.
func promptResolve(in io.Reader, out io.Writer, noPrompt bool) {
	if noPrompt {
		return
	}
	_, _ = fmt.Fprintln(out, "→ press Enter to post the resolve message…")
	_, _ = bufio.NewReader(in).ReadString('\n')
}

func runSlackTestUptime(ctx context.Context, out io.Writer, in io.Reader, f slackTestFlags) error {
	target, err := resolveTarget(f.ConfigPath, f.ChannelSlug)
	if err != nil {
		return err
	}
	mentions := slack.ResolveMentions(f.Notify, target.UserMapping)
	if len(f.Notify) > 0 {
		_, _ = fmt.Fprintf(out, "  notify: %v → resolved: %v\n", f.Notify, mentions)
	}
	client := slack.NewClient(slack.WithHTTPClient(&http.Client{Timeout: 10 * time.Second}))

	openedAt := time.Now().UTC()
	downIn := slack.DownInput{
		FriendlyName: f.Name,
		Group:        "slack-test",
		URL:          "https://example.invalid/health",
		Mentions:     mentions,
		StatusCode:   503,
		StatusText:   "Service Unavailable",
		FailureAt:    openedAt,
		LastError:    "simulated failure (slack test cli)",
		BodyMaxChars: 500,
		Note:         renderDependentsNote("⏸ Pauses dependents", f.Dependents),
	}

	_, _ = fmt.Fprintf(out, "▸ posting :red_circle: parent to channel %s …\n", target.ChannelID)
	parent, err := client.PostMessage(ctx, target.Token, slack.PostMessageInput{
		ChannelID:   target.ChannelID,
		Attachments: slack.BuildDownParent(downIn),
	})
	if err != nil {
		return fmt.Errorf("post parent: %w", err)
	}
	_, _ = fmt.Fprintf(out, "  parent ts=%s\n", parent.TS)

	for i := 1; i <= f.Reminders; i++ {
		if !sleep(ctx, f.Interval) {
			return ctx.Err()
		}
		_, _ = fmt.Fprintf(out, "▸ posting reminder %d/%d…\n", i, f.Reminders)
		_, err := client.PostMessage(ctx, target.Token, slack.PostMessageInput{
			ChannelID: target.ChannelID,
			ThreadTS:  parent.TS,
			Blocks: slack.BuildReminderReply(slack.ReminderInput{
				DownDuration:  time.Since(openedAt),
				LastCheckedAt: time.Now().UTC(),
				LastError:     "still down (simulated)",
			}),
		})
		if err != nil {
			return fmt.Errorf("post reminder %d: %w", i, err)
		}
	}

	promptResolve(in, out, f.NoPrompt)

	resolveAt := time.Now().UTC()
	resolveDownIn := downIn
	resolveDownIn.Note = renderDependentsNote("▶ Resumes dependents", f.Dependents)
	resolveIn := slack.ResolveInput{
		DownInput: resolveDownIn,
		ResolveAt: resolveAt,
		Downtime:  resolveAt.Sub(openedAt),
	}
	_, _ = fmt.Fprintf(out, "▸ editing parent → :large_green_circle: resolved …\n")
	if err := client.UpdateMessage(ctx, target.Token, slack.UpdateMessageInput{
		ChannelID:   target.ChannelID,
		TS:          parent.TS,
		Attachments: slack.BuildResolveEdit(resolveIn),
	}); err != nil {
		return fmt.Errorf("edit parent on resolve: %w", err)
	}
	_, _ = fmt.Fprintf(out, "▸ posting resolve reply in thread…\n")
	if _, err := client.PostMessage(ctx, target.Token, slack.PostMessageInput{
		ChannelID: target.ChannelID,
		ThreadTS:  parent.TS,
		Blocks:    slack.BuildResolveReply(resolveIn),
	}); err != nil {
		return fmt.Errorf("post resolve reply: %w", err)
	}
	_, _ = fmt.Fprintln(out, "✓ uptime workflow complete")
	return nil
}

func runSlackTestSSL(ctx context.Context, out io.Writer, in io.Reader, f slackTestFlags) error {
	target, err := resolveTarget(f.ConfigPath, f.ChannelSlug)
	if err != nil {
		return err
	}
	mentions := slack.ResolveMentions(f.Notify, target.UserMapping)
	if len(f.Notify) > 0 {
		_, _ = fmt.Fprintf(out, "  notify: %v → resolved: %v\n", f.Notify, mentions)
	}
	client := slack.NewClient(slack.WithHTTPClient(&http.Client{Timeout: 10 * time.Second}))

	expiresAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	sslIn := slack.SSLDownInput{
		FriendlyName:  f.Name,
		Group:         "slack-test",
		URL:           "https://example.invalid/",
		Mentions:      mentions,
		ExpiresAt:     expiresAt,
		Issuer:        "CN=Let's Encrypt Authority X3",
		Subject:       "CN=example.invalid",
		DaysRemaining: 7,
		DetectedAt:    time.Now().UTC(),
	}

	_, _ = fmt.Fprintf(out, "▸ posting :warning: SSL parent to channel %s …\n", target.ChannelID)
	parent, err := client.PostMessage(ctx, target.Token, slack.PostMessageInput{
		ChannelID:   target.ChannelID,
		Attachments: slack.BuildSSLParent(sslIn),
	})
	if err != nil {
		return fmt.Errorf("post ssl parent: %w", err)
	}
	_, _ = fmt.Fprintf(out, "  parent ts=%s\n", parent.TS)

	for i := 1; i <= f.Reminders; i++ {
		if !sleep(ctx, f.Interval) {
			return ctx.Err()
		}
		_, _ = fmt.Fprintf(out, "▸ posting ssl reminder %d/%d…\n", i, f.Reminders)
		_, err := client.PostMessage(ctx, target.Token, slack.PostMessageInput{
			ChannelID: target.ChannelID,
			ThreadTS:  parent.TS,
			Blocks:    slack.BuildSSLReminderReply(sslIn),
		})
		if err != nil {
			return fmt.Errorf("post ssl reminder %d: %w", i, err)
		}
	}

	promptResolve(in, out, f.NoPrompt)

	newExpiry := time.Now().UTC().Add(90 * 24 * time.Hour)
	resolveIn := slack.SSLResolveInput{
		SSLDownInput: sslIn,
		NewExpiresAt: newExpiry,
		RenewedAt:    time.Now().UTC(),
	}
	_, _ = fmt.Fprintf(out, "▸ editing parent → :large_green_circle: renewed …\n")
	if err := client.UpdateMessage(ctx, target.Token, slack.UpdateMessageInput{
		ChannelID:   target.ChannelID,
		TS:          parent.TS,
		Attachments: slack.BuildSSLResolveEdit(resolveIn),
	}); err != nil {
		return fmt.Errorf("edit ssl parent on resolve: %w", err)
	}
	_, _ = fmt.Fprintf(out, "▸ posting ssl resolve reply in thread…\n")
	if _, err := client.PostMessage(ctx, target.Token, slack.PostMessageInput{
		ChannelID: target.ChannelID,
		ThreadTS:  parent.TS,
		Blocks:    slack.BuildSSLResolveReply(resolveIn),
	}); err != nil {
		return fmt.Errorf("post ssl resolve reply: %w", err)
	}
	_, _ = fmt.Fprintln(out, "✓ ssl workflow complete")
	return nil
}

// renderDependentsNote mirrors Notifier.dependentsNote: builds the
// "<prefix>: `a`, `b`" line for the small dim note above the footer.
// Returns "" when no dependents were supplied.
func renderDependentsNote(prefix string, slugs []string) string {
	if len(slugs) == 0 {
		return ""
	}
	parts := make([]string, len(slugs))
	for i, s := range slugs {
		parts[i] = "`" + s + "`"
	}
	return prefix + ": " + strings.Join(parts, ", ")
}

// sleep returns false if the context is cancelled before the duration
// elapses (caller should propagate ctx.Err()).
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

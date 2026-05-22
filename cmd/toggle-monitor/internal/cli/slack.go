package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
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
}

func addSlackTestFlags(cmd *cobra.Command, f *slackTestFlags) {
	cmd.Flags().StringVar(&f.ConfigPath, "config", "deploy/local/config.yaml", "path to the YAML config")
	cmd.Flags().StringVar(&f.ChannelSlug, "channel", "", "Slack channel slug from config.slack.channels (required)")
	cmd.Flags().StringVar(&f.Name, "name", "test-monitor", "friendly name used in the simulated messages")
	cmd.Flags().IntVar(&f.Reminders, "reminders", 2, "number of reminder messages to post in-thread")
	cmd.Flags().DurationVar(&f.Interval, "interval", time.Second, "delay between simulated events")
	cmd.Flags().BoolVar(&f.NoPrompt, "no-prompt", false, "skip the 'press Enter to resolve' prompt; resolve immediately")
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

// resolveChannel loads the config, finds the channel by slug, and
// returns the channel ID + bot token (resolved from the channel's
// tokenEnv). Errors are user-actionable (missing channel, missing env).
func resolveChannel(cfgPath, slug string) (channelID string, token secret.SecretString, err error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", "", fmt.Errorf("read config %q: %w", cfgPath, err)
	}
	cfg, err := config.Load(data)
	if err != nil {
		return "", "", fmt.Errorf("load config: %w", err)
	}
	for _, ch := range cfg.Slack.Channels {
		if ch.Slug != slug {
			continue
		}
		raw := os.Getenv(ch.TokenEnv)
		if raw == "" {
			return "", "", fmt.Errorf("env var %q (tokenEnv for channel %q) is empty", ch.TokenEnv, slug)
		}
		return ch.ChannelID, secret.SecretString(raw), nil
	}
	known := make([]string, 0, len(cfg.Slack.Channels))
	for _, ch := range cfg.Slack.Channels {
		known = append(known, ch.Slug)
	}
	return "", "", fmt.Errorf("channel slug %q not found in config (known: %v)", slug, known)
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
	channelID, token, err := resolveChannel(f.ConfigPath, f.ChannelSlug)
	if err != nil {
		return err
	}
	client := slack.NewClient(slack.WithHTTPClient(&http.Client{Timeout: 10 * time.Second}))

	openedAt := time.Now().UTC()
	downIn := slack.DownInput{
		FriendlyName: f.Name,
		Group:        "slack-test",
		URL:          "https://example.invalid/health",
		StatusCode:   503,
		StatusText:   "Service Unavailable",
		FailureAt:    openedAt,
		LastError:    "simulated failure (slack test cli)",
		BodyMaxChars: 500,
	}

	_, _ = fmt.Fprintf(out, "▸ posting :red_circle: parent to channel %s …\n", channelID)
	parent, err := client.PostMessage(ctx, token, slack.PostMessageInput{
		ChannelID:   channelID,
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
		_, err := client.PostMessage(ctx, token, slack.PostMessageInput{
			ChannelID: channelID,
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
	resolveIn := slack.ResolveInput{
		DownInput: downIn,
		ResolveAt: resolveAt,
		Downtime:  resolveAt.Sub(openedAt),
	}
	_, _ = fmt.Fprintf(out, "▸ editing parent → :large_green_circle: resolved …\n")
	if err := client.UpdateMessage(ctx, token, slack.UpdateMessageInput{
		ChannelID:   channelID,
		TS:          parent.TS,
		Attachments: slack.BuildResolveEdit(resolveIn),
	}); err != nil {
		return fmt.Errorf("edit parent on resolve: %w", err)
	}
	_, _ = fmt.Fprintf(out, "▸ posting resolve reply in thread…\n")
	if _, err := client.PostMessage(ctx, token, slack.PostMessageInput{
		ChannelID: channelID,
		ThreadTS:  parent.TS,
		Blocks:    slack.BuildResolveReply(resolveIn),
	}); err != nil {
		return fmt.Errorf("post resolve reply: %w", err)
	}
	_, _ = fmt.Fprintln(out, "✓ uptime workflow complete")
	return nil
}

func runSlackTestSSL(ctx context.Context, out io.Writer, in io.Reader, f slackTestFlags) error {
	channelID, token, err := resolveChannel(f.ConfigPath, f.ChannelSlug)
	if err != nil {
		return err
	}
	client := slack.NewClient(slack.WithHTTPClient(&http.Client{Timeout: 10 * time.Second}))

	expiresAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	sslIn := slack.SSLDownInput{
		FriendlyName:  f.Name,
		Group:         "slack-test",
		URL:           "https://example.invalid/",
		ExpiresAt:     expiresAt,
		Issuer:        "CN=Let's Encrypt Authority X3",
		Subject:       "CN=example.invalid",
		DaysRemaining: 7,
	}

	_, _ = fmt.Fprintf(out, "▸ posting :warning: SSL parent to channel %s …\n", channelID)
	parent, err := client.PostMessage(ctx, token, slack.PostMessageInput{
		ChannelID:   channelID,
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
		_, err := client.PostMessage(ctx, token, slack.PostMessageInput{
			ChannelID: channelID,
			ThreadTS:  parent.TS,
			Blocks:    slack.BuildSSLReminderReply(sslIn),
		})
		if err != nil {
			return fmt.Errorf("post ssl reminder %d: %w", i, err)
		}
	}

	promptResolve(in, out, f.NoPrompt)

	newExpiry := time.Now().UTC().Add(90 * 24 * time.Hour)
	resolveIn := slack.SSLResolveInput{SSLDownInput: sslIn, NewExpiresAt: newExpiry}
	_, _ = fmt.Fprintf(out, "▸ editing parent → :large_green_circle: renewed …\n")
	if err := client.UpdateMessage(ctx, token, slack.UpdateMessageInput{
		ChannelID:   channelID,
		TS:          parent.TS,
		Attachments: slack.BuildSSLResolveEdit(resolveIn),
	}); err != nil {
		return fmt.Errorf("edit ssl parent on resolve: %w", err)
	}
	_, _ = fmt.Fprintf(out, "▸ posting ssl resolve reply in thread…\n")
	if _, err := client.PostMessage(ctx, token, slack.PostMessageInput{
		ChannelID: channelID,
		ThreadTS:  parent.TS,
		Blocks:    slack.BuildSSLResolveReply(resolveIn),
	}); err != nil {
		return fmt.Errorf("post ssl resolve reply: %w", err)
	}
	_, _ = fmt.Fprintln(out, "✓ ssl workflow complete")
	return nil
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

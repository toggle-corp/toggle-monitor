package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/coalesce"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// digestPoster adapts *slack.Client to coalesce.Poster. It resolves each
// channel slug to its ChannelInfo (ID + bot token) via the same lookup
// the notifier uses, then posts/edits/replies to the digest message.
type digestPoster struct {
	client   *slack.Client
	channels func(slug string) (slack.ChannelInfo, bool)
}

func (p *digestPoster) PostDigest(ctx context.Context, channelSlug string, msg slack.ParentMessage) (string, string, error) {
	ch, ok := p.channels(channelSlug)
	if !ok {
		return "", "", fmt.Errorf("slack channel slug %q is not registered", channelSlug)
	}
	res, err := p.client.PostMessage(ctx, ch.Token, slack.PostMessageInput{
		ChannelID:   ch.ID,
		Blocks:      msg.Blocks,
		Attachments: msg.Attachments,
	})
	if err != nil {
		return "", "", err
	}
	return res.Channel, res.TS, nil
}

func (p *digestPoster) UpdateDigest(ctx context.Context, channelSlug, ts string, msg slack.ParentMessage) error {
	ch, ok := p.channels(channelSlug)
	if !ok {
		return fmt.Errorf("slack channel slug %q is not registered", channelSlug)
	}
	return p.client.UpdateMessage(ctx, ch.Token, slack.UpdateMessageInput{
		ChannelID:   ch.ID,
		TS:          ts,
		Blocks:      msg.Blocks,
		Attachments: msg.Attachments,
	})
}

func (p *digestPoster) Reply(ctx context.Context, channelSlug, ts string, blocks []slack.Block) error {
	ch, ok := p.channels(channelSlug)
	if !ok {
		return fmt.Errorf("slack channel slug %q is not registered", channelSlug)
	}
	_, err := p.client.PostMessage(ctx, ch.Token, slack.PostMessageInput{
		ChannelID: ch.ID,
		ThreadTS:  ts,
		Blocks:    blocks,
	})
	return err
}

// groupSinkAdapter adapts *coalesce.Manager to scheduler.GroupSink,
// translating the scheduler's GroupMember into coalesce.MemberInfo.
type groupSinkAdapter struct{ m *coalesce.Manager }

func (a groupSinkAdapter) Down(ctx context.Context, channel string, gm scheduler.GroupMember, at time.Time) {
	a.m.Down(ctx, channel, coalesce.MemberInfo{
		Slug:         gm.Slug,
		FriendlyName: gm.FriendlyName,
		Mentions:     gm.Mentions,
	}, at)
}

func (a groupSinkAdapter) Up(ctx context.Context, channel, slug string, at time.Time) {
	a.m.Up(ctx, channel, slug, at)
}

func (a groupSinkAdapter) Pause(ctx context.Context, channel, slug string, at time.Time) {
	a.m.Pause(ctx, channel, slug, at)
}

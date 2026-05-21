package slack_test

import (
	"reflect"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

func TestResolveMentions_userAndSubteamSlugs(t *testing.T) {
	mapping := map[string]string{
		"alice":    "U01ABCDEF12",
		"ops-team": "S02GHIJKL34",
	}
	got := slack.ResolveMentions([]string{"alice", "ops-team"}, mapping)
	want := []string{"<@U01ABCDEF12>", "<!subteam^S02GHIJKL34>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveMentions_passesThroughRawMarkup(t *testing.T) {
	got := slack.ResolveMentions([]string{"<!here>", "<!channel>"}, nil)
	want := []string{"<!here>", "<!channel>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveMentions_dedupsUnion(t *testing.T) {
	mapping := map[string]string{"alice": "U01ABC"}
	// union from group + monitor lists; same slug appears twice.
	got := slack.ResolveMentions([]string{"alice", "<!here>", "alice"}, mapping)
	want := []string{"<@U01ABC>", "<!here>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveMentions_dropsUnknown(t *testing.T) {
	// Config validation should have caught this; the resolver is
	// defensive and just drops anything it can't classify.
	got := slack.ResolveMentions([]string{"unknown-slug", "<!here>"}, nil)
	want := []string{"<!here>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

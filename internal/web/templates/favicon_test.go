package templates_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/web/templates"
)

// Every page goes through Layout, so the icon links belong there and
// nowhere else. The .ico link points at the root path because that is
// the one browsers request unprompted.
func TestLayout_declaresTheFavicons(t *testing.T) {
	var buf bytes.Buffer
	if err := templates.Layout("Monitors").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render layout: %v", err)
	}
	head := buf.String()
	for _, want := range []string{
		`<link rel="icon" href="/static/favicon.svg" type="image/svg+xml">`,
		`<link rel="alternate icon" href="/favicon.ico"`,
		`<link rel="apple-touch-icon" href="/static/apple-touch-icon.png">`,
	} {
		if !strings.Contains(head, want) {
			t.Errorf("layout head missing %s", want)
		}
	}
}

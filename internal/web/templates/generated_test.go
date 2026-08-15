package templates_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/web/templates"
)

// templ only parses `@Component(...)` when the call starts an expression
// node. Written after text on the same line — `Last checked @Timestamp(t)`
// — it is swallowed into the surrounding text run and the generator bakes
// the source into the page as literal characters, which then ship to
// operators verbatim.
//
// The mistake is invisible in the .templ source and produces no build
// error, so the generated output is where it has to be caught. Scanning
// the static strings the generator emits covers every page at once,
// including ones no test renders.
var componentCallInText = regexp.MustCompile(`@[A-Za-z_][A-Za-z0-9_.]*\(`)

func TestGenerated_noComponentCallLeftAsLiteralText(t *testing.T) {
	files, err := filepath.Glob("*_templ.go")
	if err != nil {
		t.Fatalf("glob generated templates: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no *_templ.go found — this guard would pass vacuously")
	}

	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			// Only the emitted literals matter; ordinary Go in the file
			// legitimately mentions component names.
			if !strings.Contains(line, "templruntime.WriteString") {
				continue
			}
			if m := componentCallInText.FindString(line); m != "" {
				t.Errorf("%s:%d emits %q as literal page text — the component call was not parsed:\n\t%s",
					f, i+1, m, strings.TrimSpace(line))
			}
		}
	}
}

// The guard above proves the call is no longer literal text; this
// proves it resolves to the stamp an operator is meant to read.
func TestIssuesPage_rendersMappingLastRunAsATimestamp(t *testing.T) {
	lastRun := time.Date(2026, 8, 15, 2, 25, 19, 0, time.UTC)
	view := templates.IssuesView{
		Mapping: templates.MappingHealth{
			Invalid: []templates.MappingEntry{{Slug: "alice", ID: "U0000000000", Reason: "not found"}},
			LastRun: lastRun,
		},
	}

	var buf bytes.Buffer
	if err := templates.IssuesPage(view).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()

	if want := lastRun.Format(time.RFC3339); !strings.Contains(got, want) {
		t.Errorf("page should carry the mapping check time %q", want)
	}
	if strings.Contains(got, "@Timestamp(") {
		t.Error("page carries the unrendered component call as text")
	}
}

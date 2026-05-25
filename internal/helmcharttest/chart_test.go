// Package helmcharttest validates that the bundled Helm chart renders
// a ConfigMap whose payload is accepted by the binary's config loader.
//
// The point is to catch schema drift between deploy/helm/toggle-monitor
// and internal/config the moment a chart change or a binary schema
// change desynchronises the two.
package helmcharttest_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// chartDir resolves to deploy/helm/toggle-monitor relative to the repo
// root. The test file lives in internal/helmcharttest/, so two parents
// up gets us to the module root.
func chartDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(here), "..", "..", "deploy", "helm", "toggle-monitor")
}

func TestChart_ExamplesRenderValidConfig(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}

	cases := []struct {
		name       string
		valuesFile string   // relative to chart dir; "" = no extra values
		sets       []string // --set k=v overrides
		envStubs   map[string]string
		expectCM   bool
	}{
		{
			// The default values.yaml must render a binary-valid ConfigMap
			// once REPLACE_ME entries are filled in. The chart isn't
			// expected to ship a fully-working config out of the box —
			// REPLACE_ME signals "operator must edit this" — so the test
			// overrides those fields via --set.
			name:       "default values.yaml with REPLACE_ME overridden",
			valuesFile: "",
			sets: []string{
				`config.inline.database.host=stub-pg.svc.cluster.local`,
				// Helm's --set replaces list entries wholesale instead
				// of merging, so we re-supply slug/tokenEnv alongside the
				// channelId override.
				`config.inline.slack.channels[0].slug=ops-alerts`,
				`config.inline.slack.channels[0].channelId=C0123ABCD`,
				`config.inline.slack.channels[0].tokenEnv=SLACK_BOT_TOKEN`,
			},
			expectCM: true,
		},
		{
			name:       "examples/values-cnpg.yaml",
			valuesFile: "examples/values-cnpg.yaml",
			envStubs: map[string]string{
				"DB_HOST":         "my-cnpg-rw.cnpg-system.svc.cluster.local",
				"DB_PORT":         "5432",
				"DB_USER":         "toggle_monitor",
				"DB_NAME":         "toggle_monitor",
				"DB_PASSWORD":     "stub",
				"SLACK_BOT_TOKEN": "xoxb-stub",
			},
			expectCM: true,
		},
		{
			// External-CM path: chart must NOT render its own ConfigMap.
			// The values file points at a CM the operator manages
			// out-of-band, so this test only verifies that helm template
			// succeeds and that the rendered output contains no chart-owned
			// ConfigMap.
			name:       "examples/values-external-cm.yaml",
			valuesFile: "examples/values-external-cm.yaml",
			expectCM:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rendered, err := helmTemplate(t, chartDir(t), tc.valuesFile, tc.sets, tc.envStubs)
			if err != nil {
				t.Fatalf("helm template failed: %v\n--- output ---\n%s", err, rendered)
			}

			cmData, found := extractChartConfigMap(t, rendered)
			switch {
			case tc.expectCM && !found:
				t.Fatalf("expected the chart-rendered ConfigMap, found none\n--- rendered ---\n%s", rendered)
			case !tc.expectCM && found:
				t.Fatalf("expected NO chart-rendered ConfigMap, found one\n--- rendered ---\n%s", rendered)
			case !tc.expectCM:
				return
			}

			for k, v := range tc.envStubs {
				t.Setenv(k, v)
			}
			if _, err := config.Load([]byte(cmData)); err != nil {
				t.Fatalf("config.Load rejected the rendered ConfigMap:\n%v\n\n--- ConfigMap data ---\n%s",
					err, cmData)
			}
		})
	}
}

// helmTemplate shells out to `helm template` with the given values file
// and --set overrides. envStubs are set on the helm process so any
// ${VAR} in the values file resolves at render time (though our chart
// passes ${VAR} literals through verbatim — the env stubs primarily
// matter at config.Load time later).
func helmTemplate(
	t *testing.T,
	chartDir, valuesFile string,
	sets []string,
	envStubs map[string]string,
) (string, error) {
	t.Helper()

	args := []string{"template", "r", chartDir}
	if valuesFile != "" {
		args = append(args, "-f", filepath.Join(chartDir, valuesFile))
	}
	for _, s := range sets {
		args = append(args, "--set", s)
	}

	cmd := exec.Command("helm", args...)
	env := os.Environ()
	for k, v := range envStubs {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String() + "\n" + stderr.String(), err
	}
	return stdout.String(), nil
}

// extractChartConfigMap scans a multi-doc YAML stream for the chart's
// own ConfigMap (Kind=ConfigMap, label app.kubernetes.io/name=toggle-monitor)
// and returns the value of data."config.yaml" (or whichever key is set
// in the chart). Returns (data, true) on hit, ("", false) on miss.
func extractChartConfigMap(t *testing.T, manifest string) (string, bool) {
	t.Helper()

	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Data map[string]string `yaml:"data"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return "", false
		}
		if err != nil {
			t.Fatalf("yaml decode: %v", err)
		}
		if doc.Kind != "ConfigMap" {
			continue
		}
		if doc.Metadata.Labels["app.kubernetes.io/name"] != "toggle-monitor-helm" {
			continue
		}
		for _, v := range doc.Data {
			return v, true
		}
		return "", true // ConfigMap with no data — odd but treat as found
	}
}

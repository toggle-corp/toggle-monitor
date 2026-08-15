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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/observability"
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

// The alert expressions match on toggle_monitor_issues' `source` label
// values, which internal/lifecycle owns. A rename on either side
// silently stops the alert from ever firing — nothing else would catch
// that, because a PromQL selector matching no series is not an error.
func TestChart_PrometheusRuleMatchesTheGaugeSourceLabels(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}

	rendered, err := helmTemplate(t, chartDir(t), "", []string{"prometheusRule.enabled=true"}, nil)
	if err != nil {
		t.Fatalf("helm template failed: %v\n--- output ---\n%s", err, rendered)
	}
	if !strings.Contains(rendered, "kind: PrometheusRule") {
		t.Fatalf("prometheusRule.enabled=true rendered no PrometheusRule\n--- rendered ---\n%s", rendered)
	}
	for _, source := range observability.IssueSources {
		want := `toggle_monitor_issues{source="` + source + `"} > 0`
		if !strings.Contains(rendered, want) {
			t.Errorf("no alert expression for source %q (want %s)", source, want)
		}
	}
	// The gauge is only published while the process is up, so the
	// scrape-health rule is what covers every other rule's blind spot.
	if !strings.Contains(rendered, "alert: ToggleMonitorDown") {
		t.Error("scrape-health rule missing; nothing covers toggle-monitor being down")
	}
}

// Shipping alert rules that fire by default in a deployment that never
// asked for them would be hostile.
func TestChart_PrometheusRuleIsOffByDefault(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}

	rendered, err := helmTemplate(t, chartDir(t), "", nil, nil)
	if err != nil {
		t.Fatalf("helm template failed: %v\n--- output ---\n%s", err, rendered)
	}
	if strings.Contains(rendered, "kind: PrometheusRule") {
		t.Error("PrometheusRule rendered without prometheusRule.enabled")
	}
}

// Namespace annotations feed namespaceAnnotation-scoped *From blocks
// (ADR-0009); without this verb the informer cache never syncs and the
// pod fails to start.
func TestChart_ClusterRoleGrantsNamespaceRead(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}

	rendered, err := helmTemplate(t, chartDir(t), "", nil, nil)
	if err != nil {
		t.Fatalf("helm template failed: %v\n--- output ---\n%s", err, rendered)
	}
	if !strings.Contains(rendered, `resources: ["namespaces"]`) {
		t.Errorf("ClusterRole does not grant namespaces read\n--- rendered ---\n%s", rendered)
	}
}

// AM value-source rejections have no /issues section to alert on — the
// counter and the warn log are the whole surface (ADR-0013) — so the
// shipped rule is the only thing that surfaces them unprompted.
func TestChart_PrometheusRuleAlertsOnAMValueSourceRejections(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}

	rendered, err := helmTemplate(t, chartDir(t), "", []string{"prometheusRule.enabled=true"}, nil)
	if err != nil {
		t.Fatalf("helm template failed: %v\n--- output ---\n%s", err, rendered)
	}
	if !strings.Contains(rendered, "alert: ToggleMonitorAMValueSourceRejections") {
		t.Fatal("no alert for toggle_monitor_am_value_source_rejections_total")
	}
	// increase(), not rate(): rejections are events that arrive only as
	// often as Alertmanager re-delivers the offending alert, so a rate
	// over a short window is zero most of the time.
	if !strings.Contains(rendered, "increase(toggle_monitor_am_value_source_rejections_total[") {
		t.Error("alert expression should use increase() over the counter")
	}
	// Without `by`, the alert cannot say which field or reason fired.
	if !strings.Contains(rendered, "sum by (field, reason)") {
		t.Error("alert expression should preserve the field/reason labels")
	}

	// The alert can only ever fire if the series stays non-zero for the
	// whole `for` window, and increase() holds it up for exactly
	// `window`. for >= window is a silently dead rule.
	window, forDur := amValueSourceWindows(t, rendered)
	if forDur >= window {
		t.Errorf("for (%s) must be shorter than the increase window (%s), or the alert can never fire", forDur, window)
	}
}

// amValueSourceWindows extracts the rejection alert's increase() range
// and its `for:` from the rendered chart.
func amValueSourceWindows(t *testing.T, rendered string) (window, forDur time.Duration) {
	t.Helper()
	idx := strings.Index(rendered, "alert: ToggleMonitorAMValueSourceRejections")
	if idx < 0 {
		t.Fatal("rejection alert not found in rendered chart")
	}
	block := rendered[idx:]
	if end := strings.Index(block, "- alert: "); end > 0 {
		block = block[:end]
	}
	w := regexp.MustCompile(`increase\(toggle_monitor_am_value_source_rejections_total\[([^\]]+)\]\)`).FindStringSubmatch(block)
	f := regexp.MustCompile(`(?m)^\s*for:\s*(\S+)\s*$`).FindStringSubmatch(block)
	if w == nil || f == nil {
		t.Fatalf("could not read window/for from:\n%s", block)
	}
	var err error
	if window, err = time.ParseDuration(w[1]); err != nil {
		t.Fatalf("increase window %q: %v", w[1], err)
	}
	if forDur, err = time.ParseDuration(f[1]); err != nil {
		t.Fatalf("for %q: %v", f[1], err)
	}
	return window, forDur
}

// Every rule in the block is individually toggleable.
func TestChart_AMValueSourceRuleIsToggleable(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}

	rendered, err := helmTemplate(t, chartDir(t), "",
		[]string{"prometheusRule.enabled=true", "prometheusRule.amValueSources.enabled=false"}, nil)
	if err != nil {
		t.Fatalf("helm template failed: %v\n--- output ---\n%s", err, rendered)
	}
	if strings.Contains(rendered, "ToggleMonitorAMValueSourceRejections") {
		t.Error("rule rendered despite amValueSources.enabled=false")
	}
	if !strings.Contains(rendered, "ToggleMonitorDown") {
		t.Error("disabling one rule should not disable the others")
	}
}

// Helm runs pre-install hooks before every normal resource in the
// release, so anything a hook pod references must not be a normal
// resource of that same release. The chart's ArgoCD path expresses the
// ordering with sync waves; the Helm path has no equivalent, and a
// dangling reference here is not a template error — it is a fresh
// install that hangs until it times out.
func TestChart_migrateHookReferencesNothingHelmCreatesLater(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := chartDir(t)

	cases := []struct {
		name string
		sets []string
	}{
		{
			// The quickstart path: the chart renders its own ConfigMap.
			name: "chart-managed config",
			sets: []string{
				`migrate.argoCDPreSyncHook=false`,
				`config.inline.database.host=stub-pg.svc.cluster.local`,
				`config.inline.slack.channels[0].slug=ops-alerts`,
				`config.inline.slack.channels[0].channelId=C0123ABCD`,
				`config.inline.slack.channels[0].tokenEnv=SLACK_BOT_TOKEN`,
			},
		},
		{
			// The operator-managed path: the ConfigMap is external, so
			// only the ServiceAccount reference is in question.
			name: "existing config map",
			sets: []string{
				`migrate.argoCDPreSyncHook=false`,
				`config.existingConfigMap.name=external-config`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest, err := helmTemplate(t, dir, "", tc.sets, nil)
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, manifest)
			}
			docs := decodeManifest(t, manifest)

			job := findMigrateJob(t, docs)
			if _, ok := job.Metadata.Annotations["helm.sh/hook"]; !ok {
				t.Fatal("migrate Job is not a Helm hook — this test asserts the wrong thing")
			}

			if sa := job.Spec.Template.Spec.ServiceAccountName; sa != "" {
				if d := findByKindName(docs, "ServiceAccount", sa); d != nil && !isHelmHook(*d) {
					t.Errorf("migrate hook runs as ServiceAccount %q, which the chart creates as a normal resource — the hook pod cannot be admitted", sa)
				}
			}

			for _, v := range job.Spec.Template.Spec.Volumes {
				if v.ConfigMap.Name == "" {
					continue
				}
				d := findByKindName(docs, "ConfigMap", v.ConfigMap.Name)
				if d == nil {
					continue // externally managed; the operator owns its lifecycle
				}
				if !isHelmHook(*d) {
					t.Errorf("migrate hook mounts ConfigMap %q, which the chart creates as a normal resource — the hook pod stays pending", v.ConfigMap.Name)
					continue
				}
				if hookWeight(t, *d) >= hookWeight(t, job) {
					t.Errorf("ConfigMap %q has hook-weight %d, not below the Job's %d — Helm may create it after the hook runs",
						v.ConfigMap.Name, hookWeight(t, *d), hookWeight(t, job))
				}
			}
		})
	}
}

// manifestDoc is the slice of a rendered document these ordering
// assertions need.
type manifestDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				ServiceAccountName string `yaml:"serviceAccountName"`
				Volumes            []struct {
					Name      string `yaml:"name"`
					ConfigMap struct {
						Name string `yaml:"name"`
					} `yaml:"configMap"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func decodeManifest(t *testing.T, manifest string) []manifestDoc {
	t.Helper()
	var docs []manifestDoc
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var d manifestDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("yaml decode: %v", err)
		}
		if d.Kind != "" {
			docs = append(docs, d)
		}
	}
}

func findMigrateJob(t *testing.T, docs []manifestDoc) manifestDoc {
	t.Helper()
	for _, d := range docs {
		if d.Kind == "Job" && strings.Contains(d.Metadata.Name, "migrate") {
			return d
		}
	}
	t.Fatal("no migrate Job in the rendered manifest")
	return manifestDoc{}
}

func findByKindName(docs []manifestDoc, kind, name string) *manifestDoc {
	for i, d := range docs {
		if d.Kind == kind && d.Metadata.Name == name {
			return &docs[i]
		}
	}
	return nil
}

func isHelmHook(d manifestDoc) bool {
	_, ok := d.Metadata.Annotations["helm.sh/hook"]
	return ok
}

// hookWeight returns the resource's hook weight; Helm treats an absent
// weight as 0.
func hookWeight(t *testing.T, d manifestDoc) int {
	t.Helper()
	raw, ok := d.Metadata.Annotations["helm.sh/hook-weight"]
	if !ok {
		return 0
	}
	w, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("hook-weight %q on %s/%s is not an integer: %v", raw, d.Kind, d.Metadata.Name, err)
	}
	return w
}

// The ArgoCD path orders the same resources with sync waves. Helm hook
// annotations there would take the ConfigMap out of the release Argo
// tracks, so they must appear only under the Helm-hook path.
func TestChart_argoCDPathLeavesTheConfigMapANormalResource(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}

	manifest, err := helmTemplate(t, chartDir(t), "", []string{
		`migrate.argoCDPreSyncHook=true`,
		`config.inline.database.host=stub-pg.svc.cluster.local`,
		`config.inline.slack.channels[0].slug=ops-alerts`,
		`config.inline.slack.channels[0].channelId=C0123ABCD`,
		`config.inline.slack.channels[0].tokenEnv=SLACK_BOT_TOKEN`,
	}, nil)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, manifest)
	}

	for _, d := range decodeManifest(t, manifest) {
		if d.Kind == "ConfigMap" && isHelmHook(d) {
			t.Errorf("ConfigMap %q carries helm.sh/hook under the ArgoCD path", d.Metadata.Name)
		}
	}
}

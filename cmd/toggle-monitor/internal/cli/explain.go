package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/merger"
	"github.com/toggle-corp/toggle-monitor/internal/slug"
)

// newExplainCmd builds the `toggle-monitor explain` subcommand.
//
// Two modes:
//
//   - Live:        explain --ingress ns/name [--host h]
//   - Hypothetical: explain --namespace ns --labels k=v,k2=v2 --host h
//
// Both modes resolve against the same merger.Resolve helper the daemon
// uses to materialize monitors, so the CLI cannot drift from production
// cascade semantics. Output is human-readable YAML; --json and
// --from-file are deferred per ADR-0002.
func newExplainCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Resolve the kube.match cascade for a live or hypothetical Ingress",
		Long: "Walks the cascading kube.match[] tree for either a live " +
			"Ingress (--ingress ns/name) or a synthetic one " +
			"(--namespace + --labels/--annotations + --host) and prints the resolved " +
			"monitor config plus the rule chain that produced it.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			ingressRef, _ := cmd.Flags().GetString("ingress")
			namespace, _ := cmd.Flags().GetString("namespace")
			labelsFlag, _ := cmd.Flags().GetString("labels")
			annotationsFlag, _ := cmd.Flags().GetString("annotations")
			nsAnnotationsFlag, _ := cmd.Flags().GetString("namespace-annotations")
			hostFlag, _ := cmd.Flags().GetString("host")
			kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
			return runExplainCLI(cmd.Context(), explainOpts{
				configPath:        cfgPath,
				ingressRef:        ingressRef,
				namespace:         namespace,
				labelsFlag:        labelsFlag,
				annotationsFlag:   annotationsFlag,
				nsAnnotationsFlag: nsAnnotationsFlag,
				host:              hostFlag,
				kubeconfig:        kubeconfig,
				out:               cmd.OutOrStdout(),
				clientFor:         defaultClientFor,
			})
		},
	}
	cmd.Flags().String("config", "/etc/toggle-monitor/config.yaml", "path to the YAML config")
	cmd.Flags().String("ingress", "", "live mode: <namespace>/<name> of an Ingress to fetch from the cluster")
	cmd.Flags().String("namespace", "", "hypothetical mode: namespace of the synthetic Ingress")
	cmd.Flags().String("labels", "", "hypothetical mode: comma-separated key=value pairs for the synthetic Ingress labels (empty allowed)")
	cmd.Flags().String("annotations", "", "hypothetical mode: comma-separated key=value pairs for the synthetic Ingress annotations (empty allowed)")
	cmd.Flags().String("namespace-annotations", "", "hypothetical mode: comma-separated key=value pairs for the synthetic Namespace annotations (empty allowed)")
	cmd.Flags().String("host", "", "hypothetical: host on the synthetic Ingress; live: filter to one host on the fetched Ingress")
	cmd.Flags().String("kubeconfig", "",
		"live mode: path to a kubeconfig file; "+
			"empty defaults to in-cluster ServiceAccount, then $KUBECONFIG, then $HOME/.kube/config")
	return cmd
}

// explainOpts bundles the inputs to runExplainCLI so the test harness
// can drive it without going through the cobra flag layer.
type explainOpts struct {
	configPath string
	ingressRef string
	namespace  string
	labelsFlag string
	// annotationsFlag / nsAnnotationsFlag feed the two `when:` annotation
	// scopes (ADR-0014). Live mode reads both off the cluster instead.
	annotationsFlag   string
	nsAnnotationsFlag string
	host              string
	kubeconfig        string
	out               io.Writer

	// clientFor is the seam tests use to inject a fake clientset. nil
	// in production wires defaultClientFor (in-cluster → kubeconfig →
	// $KUBECONFIG → $HOME/.kube/config).
	clientFor func(kubeconfigPath string) (kubernetes.Interface, error)
}

// runExplainCLI dispatches on flag combination and produces the YAML
// report. Live and hypothetical modes share the same renderer; only
// the Ingress construction differs.
func runExplainCLI(ctx context.Context, opts explainOpts) error {
	if opts.ingressRef != "" && (opts.namespace != "" || opts.labelsFlag != "" ||
		opts.annotationsFlag != "" || opts.nsAnnotationsFlag != "") {
		return fmt.Errorf("--ingress is mutually exclusive with --namespace/--labels/--annotations/--namespace-annotations (live vs hypothetical modes)")
	}
	if opts.ingressRef == "" && opts.namespace == "" {
		return fmt.Errorf("provide either --ingress ns/name (live) or --namespace + --host (hypothetical)")
	}

	cfg, err := loadExplainConfig(opts.configPath)
	if err != nil {
		return err
	}

	if opts.ingressRef != "" {
		return runExplainLive(ctx, cfg, opts)
	}
	return runExplainHypothetical(cfg, opts)
}

// loadExplainConfig is a small wrapper that surfaces a consistent
// error for missing/invalid config files. Mirrors what serve.go and
// validate.go do.
func loadExplainConfig(path string) (*config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg, err := config.Load(data)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if cfg.Kube == nil {
		return nil, fmt.Errorf("config has no kube: block; nothing for explain to resolve against")
	}
	return &cfg, nil
}

// runExplainHypothetical constructs a synthetic Ingress and prints
// the resolved cascade. Host is required: without it the (Ingress,
// host) pair is incomplete and the cascade has no host-dimension
// input to discriminate on.
func runExplainHypothetical(cfg *config.Config, opts explainOpts) error {
	if opts.host == "" {
		return fmt.Errorf("hypothetical mode requires --host")
	}
	labels, err := parsePairsFlag("--labels", opts.labelsFlag)
	if err != nil {
		return err
	}
	annotations, err := parsePairsFlag("--annotations", opts.annotationsFlag)
	if err != nil {
		return err
	}
	nsAnnotations, err := parsePairsFlag("--namespace-annotations", opts.nsAnnotationsFlag)
	if err != nil {
		return err
	}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   opts.namespace,
			Name:        "(hypothetical)",
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: opts.host}},
		},
	}
	return writeExplainReport(opts.out, cfg, ing, opts.host, nsAnnotations)
}

// runExplainLive fetches the named Ingress from the live cluster and
// prints one report per (Ingress, host) pair. With --host the output
// is filtered to a single host; an unknown host fails loudly so the
// operator notices a typo instead of getting silent empty output.
func runExplainLive(ctx context.Context, cfg *config.Config, opts explainOpts) error {
	ns, name, err := splitIngressRef(opts.ingressRef)
	if err != nil {
		return err
	}
	clientFor := opts.clientFor
	if clientFor == nil {
		clientFor = defaultClientFor
	}
	cs, err := clientFor(opts.kubeconfig)
	if err != nil {
		return fmt.Errorf("resolve kube client: %w", err)
	}
	ing, err := cs.NetworkingV1().Ingresses(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get ingress %s/%s: %w", ns, name, err)
	}

	hosts := uniqueExplainHosts(ing)
	if opts.host != "" {
		found := false
		for _, h := range hosts {
			if h == opts.host {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("ingress %s/%s has no rule with host %q (hosts: %v)", ns, name, opts.host, hosts)
		}
		hosts = []string{opts.host}
	}
	if len(hosts) == 0 {
		return fmt.Errorf("ingress %s/%s has no rule with a non-empty host", ns, name)
	}

	// Namespace annotations feed namespaceAnnotation-scoped *From
	// blocks. A failed read is not fatal: those blocks fall back to
	// their defaults, which is also what the daemon does when its
	// informer has no entry, and the rest of the report is still
	// worth printing.
	var nsAnnotations map[string]string
	if nsObj, nerr := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); nerr == nil {
		nsAnnotations = nsObj.Annotations
	} else {
		fmt.Fprintf(os.Stderr, "warning: could not read namespace %q annotations (%v); namespaceAnnotation sources will use their defaults\n", ns, nerr)
	}

	for i, h := range hosts {
		if i > 0 {
			if _, err := io.WriteString(opts.out, "---\n"); err != nil {
				return err
			}
		}
		if err := writeExplainReport(opts.out, cfg, ing, h, nsAnnotations); err != nil {
			return err
		}
	}
	return nil
}

// splitIngressRef parses "<namespace>/<name>". Either part empty is
// rejected with a clear message — operators shouldn't have to guess
// which side the typo is on.
func splitIngressRef(ref string) (namespace, name string, err error) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("--ingress must be <namespace>/<name>, got %q", ref)
	}
	return parts[0], parts[1], nil
}

// parsePairsFlag accepts "" (empty map), "k=v", or "k=v,k2=v2"
// with whitespace tolerated around keys and values. Anything that
// doesn't parse as key=value is a hard error — silently dropping
// malformed pairs would hide typos in selector tests. name is the flag
// the pairs came from, so the error says which one to fix.
func parsePairsFlag(name, flag string) (map[string]string, error) {
	out := map[string]string{}
	flag = strings.TrimSpace(flag)
	if flag == "" {
		return out, nil
	}
	for _, raw := range strings.Split(flag, ",") {
		pair := strings.TrimSpace(raw)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("%s entry %q is not key=value", name, pair)
		}
		k := strings.TrimSpace(pair[:eq])
		v := strings.TrimSpace(pair[eq+1:])
		if k == "" {
			return nil, fmt.Errorf("%s entry %q has empty key", name, pair)
		}
		out[k] = v
	}
	return out, nil
}

// uniqueExplainHosts mirrors kube.uniqueHosts. Duplicated here to
// avoid pulling the entire kube package (with its informer wiring)
// into the CLI binary's hypothetical-mode path. Order-preserving.
func uniqueExplainHosts(ing *networkingv1.Ingress) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" {
			continue
		}
		if _, dup := seen[rule.Host]; dup {
			continue
		}
		seen[rule.Host] = struct{}{}
		out = append(out, rule.Host)
	}
	return out
}

// defaultClientFor implements the kubeconfig precedence documented on
// the --kubeconfig flag: in-cluster ServiceAccount first (so the same
// binary works when run via `kubectl exec` into the pod), then the
// explicit --kubeconfig path, then $KUBECONFIG, then $HOME/.kube/config.
func defaultClientFor(kubeconfigPath string) (kubernetes.Interface, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(cfg)
	}
	candidates := []string{kubeconfigPath, os.Getenv("KUBECONFIG")}
	if home := os.Getenv("HOME"); home != "" {
		candidates = append(candidates, home+"/.kube/config")
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		cfg, err := clientcmd.BuildConfigFromFlags("", p)
		if err != nil {
			continue
		}
		return kubernetes.NewForConfig(cfg)
	}
	return nil, fmt.Errorf("no usable kube credentials: not running in-cluster and no kubeconfig found (tried --kubeconfig, $KUBECONFIG, $HOME/.kube/config)")
}

// explainReport is the YAML-serializable shape printed for one
// (Ingress, host) pair. Field ordering is intentional — operators
// read top-to-bottom: identity → which rules fired → what config
// they produced → what the daemon would do with that config.
type explainReport struct {
	Ingress   explainIngress     `yaml:"ingress"`
	RuleChain []string           `yaml:"ruleChain"`
	Resolved  *config.KubeConfig `yaml:"resolved,omitempty"`
	Outcome   string             `yaml:"outcome"`
	Slug      string             `yaml:"slug,omitempty"`
	Invalid   string             `yaml:"invalid,omitempty"`

	// Provenance names every field an annotation supplied, and
	// Warnings every annotation value that was rejected. Both are
	// omitted for a cascade that used only literals — ADR-0009 makes
	// them required output, because without them "why does this
	// monitor have these settings?" has an invisible second input.
	Provenance []string `yaml:"provenance,omitempty"`
	Warnings   []string `yaml:"warnings,omitempty"`
}

// explainIngress is the identity block at the top of the report.
// Labels are emitted sorted so the output is byte-stable across runs
// (map iteration order would otherwise produce diff noise on
// repeated invocations).
type explainIngress struct {
	Namespace string            `yaml:"namespace"`
	Name      string            `yaml:"name"`
	Host      string            `yaml:"host"`
	Labels    map[string]string `yaml:"labels,omitempty"`
}

// writeExplainReport runs the merger's Resolve helper and serializes
// the result. Three outcomes are possible per ADR-0002:
//
//   - no-match: tree has no root or every rule's when missed (caller-
//     constructed configs only; production validation forbids this).
//   - ignored: deepest ignore: directive resolved to true.
//   - invalid: resolved config failed checkResolved (e.g. timeout >=
//     interval, sslAlertThreshold <= escalation).
//   - materialized: would produce the named monitor slug.
func writeExplainReport(out io.Writer, cfg *config.Config, ing *networkingv1.Ingress, host string, nsAnnotations map[string]string) error {
	res := merger.Resolve(cfg.Kube.Match, ing, host, explainEnv(cfg, nsAnnotations))
	resolved := res.Config

	report := explainReport{
		Ingress: explainIngress{
			Namespace: ing.Namespace,
			Name:      ing.Name,
			Host:      host,
			Labels:    sortedLabels(ing.Labels),
		},
		RuleChain: res.Chain,
	}
	for _, p := range res.Provenance {
		report.Provenance = append(report.Provenance, p.String())
	}
	for _, w := range res.Warnings {
		report.Warnings = append(report.Warnings, w.String())
	}

	switch {
	case !res.Matched:
		report.Outcome = "no-match"
	case res.Ignored:
		report.Outcome = "ignored"
	case res.Err != nil:
		report.Outcome = "invalid"
		report.Invalid = res.Err.Error()
		// Show the resolved (but invalid) config too — operators
		// usually want to see *which* field went out of bounds, not
		// just the error message.
		report.Resolved = &resolved
	default:
		report.Outcome = "materialized"
		report.Resolved = &resolved
		s, err := slug.SanitizeKubeDiscovered(ing.Namespace, ing.Name, host)
		if err != nil {
			// Slug failure is itself a materialization blocker; the
			// daemon would emit kube-invalid here. Match that by
			// flipping the outcome rather than letting the report
			// claim "materialized" with an empty slug.
			report.Outcome = "invalid"
			report.Invalid = "slug generation failed: " + err.Error()
		} else {
			report.Slug = s
		}
	}

	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	defer func() { _ = enc.Close() }()
	return enc.Encode(report)
}

// explainEnv assembles the annotation environment from the loaded
// config plus the namespace annotations the caller fetched. Hypothetical
// mode passes nil annotations — a *From block then resolves to its
// default, which is exactly what the daemon would do for a namespace
// carrying no annotations.
func explainEnv(cfg *config.Config, nsAnnotations map[string]string) merger.Env {
	channels := make(map[string]struct{}, len(cfg.Slack.Channels))
	for _, ch := range cfg.Slack.Channels {
		channels[ch.Slug] = struct{}{}
	}
	return merger.Env{
		NamespaceAnnotations: nsAnnotations,
		UserMapping:          cfg.Slack.UserMapping,
		SlackChannels:        channels,
	}
}

// sortedLabels copies the input map with keys in stable order so the
// YAML encoder produces deterministic output. Returns nil when the
// input is empty so the omitempty tag drops the labels: line entirely
// for label-free ingresses.
func sortedLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(in))
	for _, k := range keys {
		out[k] = in[k]
	}
	return out
}

// Package kube loads kubeconfig files, resolves contexts and builds clients.
package kube

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/flux-iac/tofu-controller/api/v1alpha2"
)

// DefaultKubeconfig is used when neither --kube nor $KUBECONFIG is set.
const DefaultKubeconfig = "~/.kube/lynqtech/config"

// Client-side rate limit. kubectl uses QPS 50, burst 300.
const (
	clientQPS   = 50
	clientBurst = 100
)

// Config wraps the loaded kubeconfig.
type Config struct {
	rules    *clientcmd.ClientConfigLoadingRules
	contexts []string
	current  string
}

// KubeconfigPaths returns the kubeconfig files to load. The explicit flag
// wins over $KUBECONFIG, which wins over DefaultKubeconfig. Colon-separated
// lists are supported for both the flag and the environment variable.
func KubeconfigPaths(flag string, env string) []string {
	raw := flag
	if raw == "" {
		raw = env
	}
	if raw == "" {
		raw = DefaultKubeconfig
	}
	var paths []string
	for _, p := range filepath.SplitList(raw) {
		if p == "" {
			continue
		}
		paths = append(paths, expandHome(p))
	}
	return paths
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Load reads the kubeconfig files and lists their contexts.
func Load(paths []string) (*Config, error) {
	rules := &clientcmd.ClientConfigLoadingRules{Precedence: paths}
	raw, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %s: %w", strings.Join(paths, ":"), err)
	}
	if len(raw.Contexts) == 0 {
		return nil, fmt.Errorf("no Kubernetes contexts found in %s, check your kubeconfig", strings.Join(paths, ":"))
	}
	contexts := make([]string, 0, len(raw.Contexts))
	for name := range raw.Contexts {
		contexts = append(contexts, name)
	}
	sort.Strings(contexts)
	return &Config{rules: rules, contexts: contexts, current: raw.CurrentContext}, nil
}

// Contexts returns all context names, sorted.
func (c *Config) Contexts() []string { return c.contexts }

// CurrentContext returns the current-context of the kubeconfig.
func (c *Config) CurrentContext() string { return c.current }

// ResolveContexts turns the user's selection into a list of existing contexts.
//
//   - all: every context.
//   - requested empty: the current context.
//   - otherwise each entry is used as exact name if it exists, or as substring
//     filter (e.g. "stage" selects every context containing "stage").
//
// Entries that match nothing are returned as errors; resolution continues.
func ResolveContexts(available []string, current string, requested []string, all bool) ([]string, []error) {
	if all {
		return available, nil
	}
	if len(requested) == 0 {
		if current == "" {
			return nil, []error{fmt.Errorf("no current context set, use --context or --all")}
		}
		return []string{current}, nil
	}

	exists := make(map[string]bool, len(available))
	for _, c := range available {
		exists[c] = true
	}

	var (
		result []string
		errs   []error
		seen   = map[string]bool{}
	)
	add := func(c string) {
		if !seen[c] {
			seen[c] = true
			result = append(result, c)
		}
	}
	for _, r := range requested {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if exists[r] {
			add(r)
			continue
		}
		matched := false
		for _, c := range available {
			if strings.Contains(c, r) {
				add(c)
				matched = true
			}
		}
		if !matched {
			errs = append(errs, fmt.Errorf("context '%s' does not exist. Available contexts are: %s", r, strings.Join(available, ", ")))
		}
	}
	return result, errs
}

// Scheme returns a scheme with core and tofu-controller types registered.
func Scheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := infrav1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// Client builds a controller-runtime client for the given context.
func (c *Config) Client(context string, scheme *runtime.Scheme) (client.Client, error) {
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		c.rules, &clientcmd.ConfigOverrides{CurrentContext: context},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building client config for context %s: %w", context, err)
	}
	// The client-go defaults (5 QPS, burst 10) throttle runs over many stacks.
	restCfg.QPS = clientQPS
	restCfg.Burst = clientBurst
	cl, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("creating client for context %s: %w", context, err)
	}
	return cl, nil
}

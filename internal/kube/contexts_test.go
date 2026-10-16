package kube

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var available = []string{"clientA-prod", "clientA-stage", "clientB-prod", "clientB-stage"}

func TestResolveContexts(t *testing.T) {
	tests := []struct {
		name      string
		requested []string
		all       bool
		want      []string
		wantErrs  int
	}{
		{name: "current context by default", want: []string{"clientA-stage"}},
		{name: "all", all: true, want: available},
		{name: "exact list", requested: []string{"clientB-prod", "clientA-prod"}, want: []string{"clientB-prod", "clientA-prod"}},
		{name: "substring", requested: []string{"stage"}, want: []string{"clientA-stage", "clientB-stage"}},
		{name: "deduplicated", requested: []string{"clientA-stage", "stage"}, want: []string{"clientA-stage", "clientB-stage"}},
		{name: "unknown reported, rest kept", requested: []string{"nope", "clientA-prod"}, want: []string{"clientA-prod"}, wantErrs: 1},
		{name: "blank entries ignored", requested: []string{"", " clientA-prod "}, want: []string{"clientA-prod"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, errs := ResolveContexts(available, "clientA-stage", tt.requested, tt.all)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("contexts = %v, want %v", got, tt.want)
			}
			if len(errs) != tt.wantErrs {
				t.Errorf("errors = %v, want %d", errs, tt.wantErrs)
			}
		})
	}
}

func TestKubeconfigPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	tests := []struct {
		name, flag, env string
		want            []string
	}{
		{name: "default", want: []string{filepath.Join(home, ".kube/lynqtech/config")}},
		{name: "env", env: "/a:/b", want: []string{"/a", "/b"}},
		{name: "flag wins", flag: "~/x", env: "/a", want: []string{filepath.Join(home, "x")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KubeconfigPaths(tt.flag, tt.env); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("KubeconfigPaths() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveContextsReportsSubstringMatches(t *testing.T) {
	avail := []string{"prod", "preprod", "prod-eu"}
	tests := []struct {
		name      string
		requested []string
		want      []string
		wantSub   map[string]string
	}{
		{"exact is silent", []string{"prod"}, []string{"prod"}, map[string]string{}},
		{"substring lists every match", []string{"eu"}, []string{"prod-eu"}, map[string]string{"prod-eu": "eu"}},
		{"mixed", []string{"prod", "pre"}, []string{"prod", "preprod"}, map[string]string{"preprod": "pre"}},
		{"already selected is not marked", []string{"prod-eu", "eu"}, []string{"prod-eu"}, map[string]string{}},
		{"no entry", nil, []string{"prod"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, sub, errs := ResolveContexts(avail, "prod", tt.requested, false)
			if len(errs) != 0 {
				t.Fatalf("errors = %v", errs)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("contexts = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(sub, tt.wantSub) {
				t.Errorf("substring = %v, want %v", sub, tt.wantSub)
			}
		})
	}
}

func TestResolveContextsWithoutCurrentContext(t *testing.T) {
	got, _, errs := ResolveContexts(available, "", nil, false)
	if len(got) != 0 {
		t.Errorf("contexts = %v, want none", got)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "--context or --all") {
		t.Errorf("errors = %v, want a hint to use --context or --all", errs)
	}
}

func TestResolveContextsUnknownNamesAvailable(t *testing.T) {
	_, _, errs := ResolveContexts(available, "clientA-stage", []string{"nope"}, false)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "clientA-prod") {
		t.Errorf("errors = %v, want the list of available contexts", errs)
	}
}

func TestKubeconfigPathsEdgeCases(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	tests := []struct {
		name, flag, env string
		want            []string
	}{
		{name: "empty elements skipped", flag: "/a::/b:", want: []string{"/a", "/b"}},
		{name: "bare tilde", flag: "~", want: []string{home}},
		{name: "tilde in later element", flag: "/a:~/b", want: []string{"/a", filepath.Join(home, "b")}},
		{name: "tilde of other user left alone", flag: "~other/config", want: []string{"~other/config"}},
		{name: "relative path kept", flag: "config", want: []string{"config"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KubeconfigPaths(tt.flag, tt.env); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("KubeconfigPaths() = %v, want %v", got, tt.want)
			}
		})
	}
}

const testKubeconfig = `apiVersion: v1
kind: Config
current-context: clientA-stage
clusters:
- name: c1
  cluster:
    server: https://127.0.0.1:1
users:
- name: u1
  user:
    token: placeholder-token
contexts:
- name: clientB-prod
  context: {cluster: c1, user: u1}
- name: clientA-stage
  context: {cluster: c1, user: u1}
`

func writeKubeconfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	cfg, err := Load([]string{writeKubeconfig(t, testKubeconfig)})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"clientA-stage", "clientB-prod"}; !reflect.DeepEqual(cfg.Contexts(), want) {
		t.Errorf("Contexts() = %v, want %v (sorted)", cfg.Contexts(), want)
	}
	if cfg.CurrentContext() != "clientA-stage" {
		t.Errorf("CurrentContext() = %q, want clientA-stage", cfg.CurrentContext())
	}
}

func TestLoadMergesFiles(t *testing.T) {
	second := strings.NewReplacer("clientB-prod", "clientC-prod", "clientA-stage", "clientD-stage").Replace(testKubeconfig)
	cfg, err := Load([]string{writeKubeconfig(t, testKubeconfig), writeKubeconfig(t, second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Contexts()) != 4 {
		t.Errorf("Contexts() = %v, want the contexts of both files", cfg.Contexts())
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{"unreadable syntax", writeKubeconfig(t, "contexts: [this is: not valid"), "loading kubeconfig"},
		{"no contexts", writeKubeconfig(t, "apiVersion: v1\nkind: Config\n"), "no Kubernetes contexts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]string{tt.path})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tt.path) {
				t.Errorf("err = %v, want it to name the kubeconfig %s", err, tt.path)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	// A kubeconfig that does not exist is skipped by client-go, which leaves
	// no contexts. The user must get the hint, not a stack trace.
	_, err := Load([]string{filepath.Join(t.TempDir(), "missing")})
	if err == nil || !strings.Contains(err.Error(), "no Kubernetes contexts") {
		t.Errorf("err = %v, want the no contexts hint", err)
	}
}

func TestScheme(t *testing.T) {
	s, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"Terraform", "ConfigMap", "Secret"} {
		found := false
		for gvk := range s.AllKnownTypes() {
			if gvk.Kind == kind {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("scheme does not know %s", kind)
		}
	}
}

func TestClient(t *testing.T) {
	cfg, err := Load([]string{writeKubeconfig(t, testKubeconfig)})
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := cfg.Client("clientB-prod", scheme); err != nil {
		t.Errorf("Client(existing context) = %v", err)
	}
	if _, err := cfg.Client("does-not-exist", scheme); err == nil || !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("Client(unknown context) err = %v, want it to name the context", err)
	}
}

func TestCompleteContexts(t *testing.T) {
	tests := []struct {
		name       string
		toComplete string
		want       []string
	}{
		{"empty offers all", "", available},
		{"prefix", "clientA", []string{"clientA-prod", "clientA-stage"}},
		{"no match", "nope", nil},
		{"exact name still offered", "clientA-prod", []string{"clientA-prod"}},
		{"second entry keeps the list prefix", "clientA-prod,clientB", []string{"clientA-prod,clientB-prod", "clientA-prod,clientB-stage"}},
		{"trailing comma offers the rest", "clientA-prod,", []string{"clientA-prod,clientA-stage", "clientA-prod,clientB-prod", "clientA-prod,clientB-stage"}},
		{"already selected is skipped", "clientA-prod,clientA", []string{"clientA-prod,clientA-stage"}},
		{"substring selector in the list is not a context", "stage,clientA", []string{"stage,clientA-prod", "stage,clientA-stage"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CompleteContexts(available, tt.toComplete); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CompleteContexts(%q) = %v, want %v", tt.toComplete, got, tt.want)
			}
		})
	}
}

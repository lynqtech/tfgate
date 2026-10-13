package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tfgate/internal/app"
	"tfgate/internal/tf"
	"tfgate/internal/ui"
)

const kubeconfigWithCurrent = `apiVersion: v1
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

// TestRunRejectsInvalidInvocations covers flag parsing and validation. None of
// these may get as far as touching a kubeconfig or a cluster.
func TestRunRejectsInvalidInvocations(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"--nope"}},
		{"positional argument", []string{"stack-a"}},
		{"replan and suspend", []string{"--replan", "--suspend"}},
		{"suspend and resume", []string{"--suspend", "--resume"}},
		{"fix-stuck and suspend", []string{"--fix-stuck", "--suspend"}},
		{"all and context", []string{"--all", "-c", "stage"}},
		{"zero stuck threshold", []string{"--stuck-threshold", "0"}},
		{"malformed stuck threshold", []string{"--stuck-threshold", "soon"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := run(tt.args); code != 1 {
				t.Errorf("run(%v) = %d, want 1", tt.args, code)
			}
		})
	}
}

func TestRunVersionAndHelpSucceed(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"--help"}} {
		if code := run(args); code != 0 {
			t.Errorf("run(%v) = %d, want 0", args, code)
		}
	}
}

func TestRunWithBrokenKubeconfigFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if code := run([]string{"--kube", missing}); code != 1 {
		t.Errorf("run = %d, want 1", code)
	}
}

func newOpts(kubeconfig string) app.Options {
	return app.Options{Kubeconfig: kubeconfig, StuckThreshold: tf.DefaultStuckThreshold}
}

func TestExecuteKubeconfigErrors(t *testing.T) {
	tests := []struct {
		name       string
		kubeconfig string
		opts       func(*app.Options)
		wantOutput string
	}{
		{"missing kubeconfig", filepath.Join(t.TempDir(), "missing"), func(*app.Options) {}, "no Kubernetes contexts"},
		{"unknown context", writeKubeconfig(t, kubeconfigWithCurrent), func(o *app.Options) { o.Contexts = []string{"nope"} }, "context 'nope' does not exist"},
		{"no current context", writeKubeconfig(t, strings.Replace(kubeconfigWithCurrent, "current-context: clientA-stage\n", "", 1)), func(*app.Options) {}, "--context or --all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			opts := newOpts(tt.kubeconfig)
			tt.opts(&opts)

			code := execute(context.Background(), ui.NewWithIO(strings.NewReader(""), out, out), opts)
			if code != 1 {
				t.Errorf("execute = %d, want 1, output:\n%s", code, out)
			}
			if !strings.Contains(out.String(), tt.wantOutput) {
				t.Errorf("output does not contain %q:\n%s", tt.wantOutput, out)
			}
		})
	}
}

// TestExecuteReportsUnreachableCluster runs the whole path against a
// kubeconfig whose server refuses connections: the failure must be reported
// with its context and turn into exit code 1.
func TestExecuteReportsUnreachableCluster(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := &bytes.Buffer{}

	code := execute(ctx, ui.NewWithIO(strings.NewReader(""), out, out), newOpts(writeKubeconfig(t, kubeconfigWithCurrent)))
	if code != 1 {
		t.Errorf("execute = %d, want 1, output:\n%s", code, out)
	}
	if !strings.Contains(out.String(), "clientA-stage") {
		t.Errorf("output does not name the context:\n%s", out)
	}
}

// TestExecuteUnknownContextDoesNotHideOtherContexts pins that a bad name is an
// error, yet the valid contexts of the same selection are still processed.
func TestExecuteUnknownContextDoesNotHideOtherContexts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := &bytes.Buffer{}
	opts := newOpts(writeKubeconfig(t, kubeconfigWithCurrent))
	opts.Contexts = []string{"nope", "clientA-stage"}

	code := execute(ctx, ui.NewWithIO(strings.NewReader(""), out, out), opts)
	if code != 1 {
		t.Errorf("execute = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "context 'nope' does not exist") || !strings.Contains(out.String(), "Iterating over all TF stacks in context: clientA-stage") {
		t.Errorf("want both the error and the valid context processed, output:\n%s", out)
	}
}

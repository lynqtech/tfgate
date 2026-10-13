package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/flux-iac/tofu-controller/api/plan"
	infrav1 "github.com/flux-iac/tofu-controller/api/v1alpha2"

	"tfgate/internal/flux"
	"tfgate/internal/kube"
	"tfgate/internal/tf"
	"tfgate/internal/ui"
)

var now = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func newTerraform(name, pending string) *infrav1.Terraform {
	obj := &infrav1.Terraform{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tf.Namespace, UID: types.UID(name + "-uid")},
		Spec:       infrav1.TerraformSpec{StoreReadablePlan: "human"},
	}
	obj.Status.Plan.Pending = pending
	return obj
}

func planConfigMaps(t *testing.T, obj *infrav1.Terraform, text string) []client.Object {
	t.Helper()
	p, err := plan.NewFromBytes(obj.Name, obj.Namespace, obj.WorkspaceName(), string(obj.UID), obj.Status.Plan.Pending, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	cms, err := p.ToConfigMap("")
	if err != nil {
		t.Fatal(err)
	}
	var objs []client.Object
	for _, cm := range cms {
		objs = append(objs, cm)
	}
	return objs
}

func newApp(t *testing.T, opts Options, input string, objs ...client.Object) (*App, client.Client, *bytes.Buffer) {
	t.Helper()
	return newAppWithInterceptor(t, opts, input, interceptor.Funcs{}, objs...)
}

func newAppWithInterceptor(t *testing.T, opts Options, input string, funcs interceptor.Funcs, objs ...client.Object) (*App, client.Client, *bytes.Buffer) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	oci := &unstructured.Unstructured{}
	oci.SetGroupVersionKind(flux.OCIRepositoryGVK)
	oci.SetName("stack-a-iac")
	oci.SetNamespace(tf.Namespace)
	objs = append(objs, oci)

	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&infrav1.Terraform{}).WithInterceptorFuncs(funcs).Build()
	out := &bytes.Buffer{}
	if opts.StuckThreshold == 0 {
		opts.StuckThreshold = tf.DefaultStuckThreshold
	}
	a := &App{
		UI:        ui.NewWithIO(strings.NewReader(input), out, out),
		Opts:      opts,
		NewClient: func(string) (client.Client, error) { return kubeClient, nil },
		Now:       func() time.Time { return now },
	}
	return a, kubeClient, out
}

func get(t *testing.T, c client.Client, name string) *infrav1.Terraform {
	t.Helper()
	obj := &infrav1.Terraform{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: tf.Namespace, Name: name}, obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestApprovePendingPlan(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123")
	objs := append([]client.Object{obj}, planConfigMaps(t, obj, "Plan: 1 to add, 0 to change, 0 to destroy.")...)
	a, c, out := newApp(t, Options{}, "y\n", objs...)

	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 0 {
		t.Fatalf("Run errors = %d, output:\n%s", errs, out)
	}
	if got := get(t, c, "stack-a").Spec.ApprovePlan; got != "plan-main-abc123" {
		t.Errorf("approvePlan = %q, want plan-main-abc123", got)
	}
	if !strings.Contains(out.String(), "Plan: 1 to add") {
		t.Errorf("plan not shown, output:\n%s", out)
	}
}

// TestPlanTerminalEscapesAreNotPrinted makes sure a plan cannot rewrite what
// the user sees before approving: cursor movement, line erase and carriage
// returns from the stored plan must not reach the terminal.
func TestPlanTerminalEscapesAreNotPrinted(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123")
	text := "+ create harmless\r\x1b[2K\x1b[1A- destroy production\x1b]0;title\x07"
	objs := append([]client.Object{obj}, planConfigMaps(t, obj, text)...)
	a, _, out := newApp(t, Options{}, "n\n", objs...)

	a.Run(context.Background(), []string{"ctx"})
	for _, bad := range []string{"\r", "\x1b[2K", "\x1b[1A", "\x1b]0;"} {
		if strings.Contains(out.String(), bad) {
			t.Errorf("output contains %q, output:\n%q", bad, out)
		}
	}
	if !strings.Contains(out.String(), "+ create harmless- destroy production") {
		t.Errorf("plan text not shown, output:\n%s", out)
	}
}

func TestSkipLeavesObjectUnchanged(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123")
	objs := append([]client.Object{obj}, planConfigMaps(t, obj, "plan")...)
	a, c, out := newApp(t, Options{}, "n\n", objs...)

	a.Run(context.Background(), []string{"ctx"})
	if got := get(t, c, "stack-a").Spec.ApprovePlan; got != "" {
		t.Errorf("approvePlan = %q, want empty", got)
	}
	if !strings.Contains(out.String(), "skipping") {
		t.Errorf("expected skipping, output:\n%s", out)
	}
}

func TestDryRunDoesNotApprove(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123")
	objs := append([]client.Object{obj}, planConfigMaps(t, obj, "plan")...)
	a, c, out := newApp(t, Options{DryRun: true}, "y\n", objs...)

	a.Run(context.Background(), []string{"ctx"})
	if got := get(t, c, "stack-a").Spec.ApprovePlan; got != "" {
		t.Errorf("approvePlan = %q, want empty in dry-run", got)
	}
	if !strings.Contains(out.String(), `[dry-run] would merge-patch terraform flux-system/stack-a: {"metadata":{"resourceVersion":"999"},"spec":{"approvePlan":"plan-main-abc123"}}`) {
		t.Errorf("missing dry-run output:\n%s", out)
	}
}

func TestNoApprovalWhenPlanUnreadable(t *testing.T) {
	// No plan ConfigMaps exist, so "y" must not approve.
	a, c, out := newApp(t, Options{}, "y\n", newTerraform("stack-a", "plan-main-abc123"))

	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 1 {
		t.Errorf("Run errors = %d, want 1", errs)
	}
	if got := get(t, c, "stack-a").Spec.ApprovePlan; got != "" {
		t.Errorf("approvePlan = %q, want empty", got)
	}
	if !strings.Contains(out.String(), "could not be shown") {
		t.Errorf("expected unreadable plan prompt, output:\n%s", out)
	}
}

func TestApproveRefusesChangedPlan(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-new")
	objs := append([]client.Object{obj}, planConfigMaps(t, obj, "plan")...)
	_, c, _ := newApp(t, Options{}, "", objs...)

	tc := &tf.Client{Kube: c, Out: &bytes.Buffer{}}
	if err := tc.Approve(context.Background(), "stack-a", "plan-main-old", "plan"); err == nil {
		t.Fatal("expected error for changed pending plan")
	}
}

func TestFixStuckReplansOnlyStuckStacks(t *testing.T) {
	stuck := newTerraform("stack-a", "plan-main-abc123")
	stuck.Status.LastPlannedRevision = "main@sha1:abc"
	stuck.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionUnknown, Reason: "Progressing",
		LastTransitionTime: metav1.NewTime(now.Add(-3 * time.Hour)),
	}}
	healthy := newTerraform("stack-b", "")
	a, c, out := newApp(t, Options{FixStuck: true}, "", stuck, healthy)

	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 0 {
		t.Fatalf("Run errors = %d, output:\n%s", errs, out)
	}
	got := get(t, c, "stack-a")
	if got.Status.Plan.Pending != "" || got.Status.LastPlannedRevision != "" {
		t.Errorf("status not cleared: pending=%q lastPlanned=%q", got.Status.Plan.Pending, got.Status.LastPlannedRevision)
	}
	if got.Annotations["reconcile.fluxcd.io/requestedAt"] == "" {
		t.Error("reconcile annotation missing")
	}
	if ann := get(t, c, "stack-b").Annotations["reconcile.fluxcd.io/requestedAt"]; ann != "" {
		t.Error("healthy stack must not be replanned")
	}
	if !strings.Contains(out.String(), "Service stack stack-b not stuck") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSuspendAndResume(t *testing.T) {
	a, c, _ := newApp(t, Options{Suspend: true, Stack: "stack-a", Yes: true}, "", newTerraform("stack-a", ""))
	a.Run(context.Background(), []string{"ctx"})
	if !get(t, c, "stack-a").Spec.Suspend {
		t.Error("expected suspended")
	}
	a.Opts = Options{Resume: true, Stack: "stack-a", StuckThreshold: tf.DefaultStuckThreshold}
	a.UI = ui.NewWithIO(strings.NewReader("Y\n"), &bytes.Buffer{}, &bytes.Buffer{})
	a.Run(context.Background(), []string{"ctx"})
	if get(t, c, "stack-a").Spec.Suspend {
		t.Error("expected resumed")
	}
}

func TestOCIReconcile(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123")
	objs := append([]client.Object{obj}, planConfigMaps(t, obj, "plan")...)
	a, c, out := newApp(t, Options{}, "o\n", objs...)

	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 0 {
		t.Fatalf("Run errors = %d, output:\n%s", errs, out)
	}
	oci := &unstructured.Unstructured{}
	oci.SetGroupVersionKind(flux.OCIRepositoryGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: tf.Namespace, Name: "stack-a-iac"}, oci); err != nil {
		t.Fatal(err)
	}
	if oci.GetAnnotations()["reconcile.fluxcd.io/requestedAt"] != now.Format(time.RFC3339Nano) {
		t.Errorf("annotations = %v", oci.GetAnnotations())
	}
}

func TestUnknownStackReportsError(t *testing.T) {
	a, _, out := newApp(t, Options{Stack: "missing"}, "")
	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 1 {
		t.Errorf("Run errors = %d, want 1, output:\n%s", errs, out)
	}
}

func TestValidate(t *testing.T) {
	ok := Options{StuckThreshold: time.Hour}
	tests := []struct {
		name    string
		mod     func(*Options)
		wantErr bool
	}{
		{name: "defaults", mod: func(*Options) {}},
		{name: "replan and suspend", mod: func(o *Options) { o.Replan, o.Suspend = true, true }, wantErr: true},
		{name: "suspend and resume", mod: func(o *Options) { o.Suspend, o.Resume = true, true }, wantErr: true},
		{name: "fix-stuck and replan", mod: func(o *Options) { o.FixStuck, o.Replan = true, true }},
		{name: "fix-stuck and suspend", mod: func(o *Options) { o.FixStuck, o.Suspend = true, true }, wantErr: true},
		{name: "all and context", mod: func(o *Options) { o.All, o.Contexts = true, []string{"x"} }, wantErr: true},
		{name: "zero threshold", mod: func(o *Options) { o.StuckThreshold = 0 }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := ok
			tt.mod(&o)
			if err := o.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFullRunUsesFewAPICalls(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	var objs []client.Object
	for _, name := range []string{"stack-a", "stack-b", "stack-c"} {
		obj := newTerraform(name, "plan-main-"+name)
		objs = append(objs, obj)
		objs = append(objs, planConfigMaps(t, obj, "Plan for "+name)...)
	}
	objs = append(objs, newTerraform("stack-d", ""))

	calls := map[string]int{}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				calls["get"]++
				return c.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				calls["list"]++
				return c.List(ctx, list, opts...)
			},
		}).Build()

	out := &bytes.Buffer{}
	a := &App{
		UI:        ui.NewWithIO(strings.NewReader("n\nn\nn\n"), out, out),
		Opts:      Options{StuckThreshold: tf.DefaultStuckThreshold},
		NewClient: func(string) (client.Client, error) { return kubeClient, nil },
		Now:       func() time.Time { return now },
	}
	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 0 {
		t.Fatalf("Run errors = %d, output:\n%s", errs, out)
	}
	// One list of terraforms, one list of plan configmaps, no per-stack gets.
	if calls["list"] != 2 || calls["get"] != 0 {
		t.Errorf("calls = %v, want 2 lists and 0 gets", calls)
	}
	for _, name := range []string{"stack-a", "stack-b", "stack-c"} {
		if !strings.Contains(out.String(), "Plan for "+name) {
			t.Errorf("plan of %s not shown", name)
		}
	}
}

func TestLegacyUnlabeledPlanConfigMap(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123")
	legacy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tfplan-default-stack-a", Namespace: tf.Namespace,
			Annotations: map[string]string{"savedPlan": "plan-main-abc123"},
		},
		Data: map[string]string{"tfplan": "Plan: 2 to add, 0 to change, 0 to destroy."},
	}
	a, c, out := newApp(t, Options{}, "y\n", obj, legacy)

	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 0 {
		t.Fatalf("Run errors = %d, output:\n%s", errs, out)
	}
	if !strings.Contains(out.String(), "Plan: 2 to add") {
		t.Errorf("legacy plan not shown, output:\n%s", out)
	}
	if got := get(t, c, "stack-a").Spec.ApprovePlan; got != "plan-main-abc123" {
		t.Errorf("approvePlan = %q", got)
	}
}

func TestStalePlanIsNotApprovable(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-new")
	stale := newTerraform("stack-a", "plan-main-old")
	objs := append([]client.Object{obj}, planConfigMaps(t, stale, "old plan")...)
	a, c, out := newApp(t, Options{}, "y\n", objs...)

	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 1 {
		t.Errorf("Run errors = %d, want 1", errs)
	}
	if strings.Contains(out.String(), "old plan") {
		t.Error("stale plan must not be shown")
	}
	if got := get(t, c, "stack-a").Spec.ApprovePlan; got != "" {
		t.Errorf("approvePlan = %q, want empty", got)
	}
}

// writeRecorder returns interceptor funcs that record every write to the
// cluster and pass it through.
func writeRecorder(writes *[]string) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			*writes = append(*writes, "create "+obj.GetName())
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			*writes = append(*writes, "update "+obj.GetName())
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			*writes = append(*writes, "patch "+obj.GetName())
			return c.Patch(ctx, obj, p, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			*writes = append(*writes, "delete "+obj.GetName())
			return c.Delete(ctx, obj, opts...)
		},
		DeleteAllOf: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteAllOfOption) error {
			*writes = append(*writes, "deleteallof")
			return c.DeleteAllOf(ctx, obj, opts...)
		},
		SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj client.Object, body client.Object, opts ...client.SubResourceCreateOption) error {
			*writes = append(*writes, "create/"+sub+" "+obj.GetName())
			return c.SubResource(sub).Create(ctx, obj, body, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			*writes = append(*writes, "update/"+sub+" "+obj.GetName())
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			*writes = append(*writes, "patch/"+sub+" "+obj.GetName())
			return c.SubResource(sub).Patch(ctx, obj, p, opts...)
		},
	}
}

// TestDryRunNeverWrites runs every write path of the CLI with --dry-run and
// asserts that nothing reaches the API server. A new write path that forgets
// the DryRun check fails here once a scenario for it is added.
func TestDryRunNeverWrites(t *testing.T) {
	pendingWithPlan := func(t *testing.T) []client.Object {
		obj := newTerraform("stack-a", "plan-main-abc123")
		return append([]client.Object{obj}, planConfigMaps(t, obj, "plan")...)
	}
	stuck := func(t *testing.T) []client.Object {
		obj := newTerraform("stack-a", "plan-main-abc123")
		obj.Status.Conditions = []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionUnknown, Reason: "Progressing",
			LastTransitionTime: metav1.NewTime(now.Add(-3 * time.Hour)),
		}}
		return []client.Object{obj}
	}
	plain := func(*testing.T) []client.Object { return []client.Object{newTerraform("stack-a", "")} }

	tests := []struct {
		name  string
		opts  Options
		input string
		objs  func(*testing.T) []client.Object
	}{
		{"approve", Options{}, "y\n", pendingWithPlan},
		{"replan from prompt", Options{}, "r\n", pendingWithPlan},
		{"oci reconcile from prompt", Options{}, "o\n", pendingWithPlan},
		{"replan flag", Options{Replan: true, Yes: true}, "", plain},
		{"suspend flag", Options{Suspend: true, Yes: true}, "", plain},
		{"resume flag", Options{Resume: true, Yes: true}, "", plain},
		{"fix-stuck", Options{FixStuck: true}, "", stuck},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var writes []string
			tt.opts.DryRun = true
			a, _, out := newAppWithInterceptor(t, tt.opts, tt.input, writeRecorder(&writes), tt.objs(t)...)

			if errs := a.Run(context.Background(), []string{"ctx"}); errs != 0 {
				t.Fatalf("Run errors = %d, output:\n%s", errs, out)
			}
			if len(writes) != 0 {
				t.Errorf("dry-run wrote to the cluster: %v", writes)
			}
			// Guards against a scenario that passes because it never reached the write.
			if !strings.Contains(out.String(), "[dry-run]") {
				t.Errorf("scenario did not reach a write path, output:\n%s", out)
			}
		})
	}
}

// TestYesNeverSkipsApprovalPrompt checks that -y only skips the confirmation
// of replan, suspend and resume, and never approves a plan on its own.
func TestYesNeverSkipsApprovalPrompt(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantApproved bool
	}{
		{"answer no", "n\n", false},
		{"answer enter", "\n", false},
		{"stdin closed", "", false},
		{"answer yes", "y\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := newTerraform("stack-a", "plan-main-abc123")
			objs := append([]client.Object{obj}, planConfigMaps(t, obj, "plan")...)
			a, c, out := newApp(t, Options{Yes: true}, tt.input, objs...)

			a.Run(context.Background(), []string{"ctx"})

			if !strings.Contains(out.String(), "Should it be applied?") {
				t.Errorf("approval prompt was not shown, output:\n%s", out)
			}
			approved := get(t, c, "stack-a").Spec.ApprovePlan == "plan-main-abc123"
			if approved != tt.wantApproved {
				t.Errorf("approved = %v, want %v", approved, tt.wantApproved)
			}
		})
	}
}

// TestYesSkipsConfirmationOfReplan documents the other half of -y: replan,
// suspend and resume run without asking.
func TestYesSkipsConfirmationOfReplan(t *testing.T) {
	a, c, out := newApp(t, Options{Replan: true, Yes: true}, "", newTerraform("stack-a", "plan-main-abc123"))

	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 0 {
		t.Fatalf("Run errors = %d, output:\n%s", errs, out)
	}
	if strings.Contains(out.String(), "[Y=yes") {
		t.Errorf("-y must not prompt for replan, output:\n%s", out)
	}
	if got := get(t, c, "stack-a").Status.Plan.Pending; got != "" {
		t.Errorf("pending = %q, want replanned", got)
	}
}

func stuckTerraform(name string) *infrav1.Terraform {
	obj := newTerraform(name, "")
	obj.Status.LastPlannedRevision = "main@sha1:abc"
	obj.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionUnknown, Reason: "Progressing",
		LastTransitionTime: metav1.NewTime(now.Add(-3 * time.Hour)),
	}}
	return obj
}

func TestClientErrorIsReportedAndRunContinues(t *testing.T) {
	obj := newTerraform("stack-a", "")
	a, _, out := newApp(t, Options{}, "", obj)
	good := a.NewClient
	a.NewClient = func(kubeContext string) (client.Client, error) {
		if kubeContext == "bad" {
			return nil, errors.New("no credentials for bad")
		}
		return good(kubeContext)
	}

	if errs := a.Run(context.Background(), []string{"bad", "good"}); errs != 1 {
		t.Errorf("Run errors = %d, want 1", errs)
	}
	if !strings.Contains(out.String(), "no credentials for bad") {
		t.Errorf("client error not shown, output:\n%s", out)
	}
	if !strings.Contains(out.String(), "-> good / stack-a") {
		t.Errorf("run did not continue with the next context, output:\n%s", out)
	}
}

func TestErrorsAreSummedOverContexts(t *testing.T) {
	// The pending plan has no stored plan, which is one error per context.
	a, _, _ := newApp(t, Options{}, "n\nn\n", newTerraform("stack-a", "plan-main-abc123"))
	if errs := a.Run(context.Background(), []string{"one", "two"}); errs != 2 {
		t.Errorf("Run errors = %d, want 2", errs)
	}
}

func TestListErrorIsReported(t *testing.T) {
	a, _, out := newAppWithInterceptor(t, Options{}, "", interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			if _, ok := l.(*infrav1.TerraformList); ok {
				return errors.New("api down")
			}
			return c.List(ctx, l, opts...)
		},
	})

	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 1 {
		t.Errorf("Run errors = %d, want 1", errs)
	}
	if !strings.Contains(out.String(), "context ctx: ") || !strings.Contains(out.String(), "api down") {
		t.Errorf("list error not shown with its context, output:\n%s", out)
	}
}

func TestSingleStackMode(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123")
	other := newTerraform("stack-b", "plan-main-def456")
	objs := append([]client.Object{obj, other}, planConfigMaps(t, obj, "Plan for stack-a")...)
	a, c, out := newApp(t, Options{Stack: "stack-a"}, "y\n", objs...)

	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 0 {
		t.Fatalf("Run errors = %d, output:\n%s", errs, out)
	}
	if got := get(t, c, "stack-a").Spec.ApprovePlan; got != "plan-main-abc123" {
		t.Errorf("approvePlan = %q, want plan-main-abc123", got)
	}
	if strings.Contains(out.String(), "stack-b") {
		t.Errorf("other stacks must not be touched or shown, output:\n%s", out)
	}
	if strings.Contains(out.String(), "Iterating over all TF stacks") {
		t.Errorf("single stack mode must not announce iterating, output:\n%s", out)
	}
}

func TestDeclinedConfirmationChangesNothing(t *testing.T) {
	suspended := newTerraform("stack-a", "")
	suspended.Spec.Suspend = true
	tests := []struct {
		name string
		opts Options
		obj  *infrav1.Terraform
	}{
		{"replan", Options{Replan: true}, newTerraform("stack-a", "plan-main-abc123")},
		{"suspend", Options{Suspend: true}, newTerraform("stack-a", "")},
		{"resume", Options{Resume: true}, suspended},
	}
	for _, tt := range tests {
		for _, answer := range []string{"n\n", "\n", ""} {
			t.Run(tt.name, func(t *testing.T) {
				var writes []string
				a, _, out := newAppWithInterceptor(t, tt.opts, answer, writeRecorder(&writes), tt.obj.DeepCopy())

				if errs := a.Run(context.Background(), []string{"ctx"}); errs != 0 {
					t.Fatalf("Run errors = %d, output:\n%s", errs, out)
				}
				if len(writes) != 0 {
					t.Errorf("declined %s wrote to the cluster: %v", tt.name, writes)
				}
				if !strings.Contains(out.String(), "skipping") {
					t.Errorf("expected skipping, output:\n%s", out)
				}
			})
		}
	}
}

func TestStuckStackIsOnlyReportedWithoutFixStuck(t *testing.T) {
	var writes []string
	a, _, out := newAppWithInterceptor(t, Options{}, "", writeRecorder(&writes), stuckTerraform("stack-a"))

	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 0 {
		t.Fatalf("Run errors = %d, output:\n%s", errs, out)
	}
	if len(writes) != 0 {
		t.Errorf("reporting a stuck stack wrote to the cluster: %v", writes)
	}
	for _, want := range []string{"Service stack stack-a is stuck since 2026-10-13T09:00:00Z", "rerun with --fix-stuck"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

func TestStackWithoutPendingPlanAsksNothing(t *testing.T) {
	a, c, out := newApp(t, Options{}, "y\n", newTerraform("stack-a", ""))

	if errs := a.Run(context.Background(), []string{"ctx"}); errs != 0 {
		t.Fatalf("Run errors = %d, output:\n%s", errs, out)
	}
	if !strings.Contains(out.String(), tf.NoPlanPending) {
		t.Errorf("expected %q, output:\n%s", tf.NoPlanPending, out)
	}
	if strings.Contains(out.String(), "Should it be applied?") {
		t.Errorf("must not prompt without a pending plan, output:\n%s", out)
	}
	if got := get(t, c, "stack-a").Spec.ApprovePlan; got != "" {
		t.Errorf("approvePlan = %q, want empty", got)
	}
}

func TestReplanFromReviewPrompt(t *testing.T) {
	tests := []struct {
		name string
		objs func(t *testing.T, obj *infrav1.Terraform) []client.Object
	}{
		{"readable plan", func(t *testing.T, obj *infrav1.Terraform) []client.Object {
			return append([]client.Object{obj}, planConfigMaps(t, obj, "plan")...)
		}},
		{"unreadable plan", func(_ *testing.T, obj *infrav1.Terraform) []client.Object { return []client.Object{obj} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := newTerraform("stack-a", "plan-main-abc123")
			a, c, out := newApp(t, Options{}, "r\n", tt.objs(t, obj)...)

			if errs := a.Run(context.Background(), []string{"ctx"}); errs != 0 {
				t.Fatalf("Run errors = %d, output:\n%s", errs, out)
			}
			if got := get(t, c, "stack-a").Status.Plan.Pending; got != "" {
				t.Errorf("pending = %q, want it cleared by the replan", got)
			}
			if !strings.Contains(out.String(), "Replan requested for flux-system/stack-a") {
				t.Errorf("replan not confirmed, output:\n%s", out)
			}
		})
	}
}

func TestFailedWritesAreReported(t *testing.T) {
	denied := errors.New("write denied")
	failPatches := interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return denied
		},
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return denied
		},
	}
	pending := newTerraform("stack-a", "plan-main-abc123")
	withPlan := append([]client.Object{pending}, planConfigMaps(t, pending, "plan")...)
	orphan := newTerraform("stack-b", "plan-main-def456") // its OCIRepository stack-b-iac does not exist
	orphanWithPlan := append([]client.Object{orphan}, planConfigMaps(t, orphan, "plan")...)

	tests := []struct {
		name        string
		opts        Options
		input       string
		objs        []client.Object
		wantOutput  string
		notInOutput string
	}{
		{"approve", Options{}, "y\n", withPlan, "write denied", "Approved"},
		{"replan", Options{Replan: true, Yes: true}, "", []client.Object{newTerraform("stack-a", "")}, "write denied", "Replan requested"},
		{"suspend", Options{Suspend: true, Yes: true}, "", []client.Object{newTerraform("stack-a", "")}, "write denied", ""},
		{"oci reconcile", Options{}, "o\n", orphanWithPlan, "stack-b-iac", "Reconcile requested"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _, out := newAppWithInterceptor(t, tt.opts, tt.input, failPatches, tt.objs...)

			if errs := a.Run(context.Background(), []string{"ctx"}); errs != 1 {
				t.Errorf("Run errors = %d, want 1, output:\n%s", errs, out)
			}
			if !strings.Contains(out.String(), tt.wantOutput) {
				t.Errorf("output does not contain %q:\n%s", tt.wantOutput, out)
			}
			if tt.notInOutput != "" && strings.Contains(out.String(), tt.notInOutput) {
				t.Errorf("output must not claim success (%q):\n%s", tt.notInOutput, out)
			}
		})
	}
}

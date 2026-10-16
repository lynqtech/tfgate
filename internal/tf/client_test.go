package tf

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/flux-iac/tofu-controller/api/v1alpha2"

	"tfgate/internal/kube"
)

var clientNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// TestReplanMirrorsTfctl pins the exact writes of Replan to what `tfctl
// replan` does: a status patch with field manager tf-controller first, then
// the reconcile annotation.
func TestReplanMirrorsTfctl(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	obj := newTerraform("stack-a", "plan-main-abc123", "human")
	obj.Status.LastPlannedRevision = "main@sha1:abc"
	obj.Status.LastAttemptedRevision = "main@sha1:def"
	apimeta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type: meta.ReadyCondition, Status: metav1.ConditionUnknown, Reason: "Progressing",
	})

	var order []string
	var fieldManager string
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj).
		WithStatusSubresource(&infrav1.Terraform{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
				order = append(order, "status-patch")
				po := &client.SubResourcePatchOptions{}
				po.ApplyOptions(opts)
				fieldManager = po.FieldManager
				return c.SubResource(sub).Patch(ctx, o, p, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
				order = append(order, "metadata-patch")
				return c.Patch(ctx, o, p, opts...)
			},
		}).Build()

	tc := &Client{Kube: kubeClient, Out: &bytes.Buffer{}, Now: func() time.Time { return clientNow }}
	if err := tc.Replan(context.Background(), "stack-a"); err != nil {
		t.Fatal(err)
	}

	if len(order) != 2 || order[0] != "status-patch" || order[1] != "metadata-patch" {
		t.Errorf("write order = %v, want [status-patch metadata-patch]", order)
	}
	if fieldManager != "tf-controller" {
		t.Errorf("status patch field manager = %q, want tf-controller", fieldManager)
	}

	got, err := tc.Get(context.Background(), "stack-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Plan.Pending != "" || got.Status.LastPlannedRevision != "" || got.Status.LastAttemptedRevision != "" {
		t.Errorf("status not cleared: pending=%q lastPlanned=%q lastAttempted=%q",
			got.Status.Plan.Pending, got.Status.LastPlannedRevision, got.Status.LastAttemptedRevision)
	}
	ready := apimeta.FindStatusCondition(got.Status.Conditions, meta.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ReplanRequested" {
		t.Errorf("Ready condition = %+v, want False/ReplanRequested", ready)
	}
	if want := clientNow.Format(time.RFC3339Nano); got.Annotations[meta.ReconcileRequestAnnotation] != want {
		t.Errorf("%s = %q, want %q", meta.ReconcileRequestAnnotation, got.Annotations[meta.ReconcileRequestAnnotation], want)
	}
}

// TestReplanRetriesOnConflict checks that the status patch is optimistically
// locked: when the controller updates the object between Replan's read and its
// patch, the API server must answer with a conflict, and the retry must read
// again so that the controller's condition is not overwritten.
func TestReplanRetriesOnConflict(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123", "human")
	attempts := 0
	tc := newClientWith(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			attempts++
			if attempts == 1 {
				// The controller adds a condition before the patch arrives.
				cur := &infrav1.Terraform{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(o), cur); err != nil {
					return err
				}
				apimeta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
					Type: "Other", Status: metav1.ConditionTrue, Reason: "FromController",
				})
				if err := c.Status().Update(ctx, cur); err != nil {
					return err
				}
			}
			return c.SubResource(sub).Patch(ctx, o, p, opts...)
		},
	}, obj)

	if err := tc.Replan(context.Background(), "stack-a"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Errorf("status patch attempts = %d, want 2 (conflict, then success)", attempts)
	}
	got, err := tc.Get(context.Background(), "stack-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Plan.Pending != "" {
		t.Errorf("pending = %q, want it cleared", got.Status.Plan.Pending)
	}
	if ready := apimeta.FindStatusCondition(got.Status.Conditions, meta.ReadyCondition); ready == nil || ready.Reason != "ReplanRequested" {
		t.Errorf("Ready condition = %+v, want ReplanRequested", ready)
	}
	if other := apimeta.FindStatusCondition(got.Status.Conditions, "Other"); other == nil {
		t.Error("condition added by the controller was overwritten")
	}
	if got.Annotations[meta.ReconcileRequestAnnotation] == "" {
		t.Errorf("%s not set", meta.ReconcileRequestAnnotation)
	}
}

// TestReplanGivesUpOnPersistentConflict makes sure the retry loop ends and
// that no reconcile is requested when the status could not be cleared.
func TestReplanGivesUpOnPersistentConflict(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123", "human")
	conflicts := 0
	metaPatches := 0
	tc := newClientWith(t, interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string, o client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			conflicts++
			return apierrors.NewConflict(infrav1.GroupVersion.WithResource("terraforms").GroupResource(), o.GetName(), errors.New("stale"))
		},
		Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
			metaPatches++
			return c.Patch(ctx, o, p, opts...)
		},
	}, obj)

	err := tc.Replan(context.Background(), "stack-a")
	if !apierrors.IsConflict(err) {
		t.Fatalf("Replan error = %v, want a conflict", err)
	}
	if conflicts < 2 {
		t.Errorf("status patch attempts = %d, want retries", conflicts)
	}
	if metaPatches != 0 {
		t.Errorf("%d annotation patches after a failed status patch, want 0", metaPatches)
	}
}

// TestApproveRefusesWhenObjectChangesBetweenReadAndWrite covers the window
// between Approve's re-read and its patch: the controller replans right after
// the pending check, so the approval must be rejected by the API server.
func TestApproveRefusesWhenObjectChangesBetweenReadAndWrite(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	obj := newTerraform("stack-a", "plan-main-old", "human")
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(withPlan(t, obj, "plan-main-old", "plan text")...).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
				// The controller writes a new plan before the approval arrives.
				cur := &infrav1.Terraform{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(o), cur); err != nil {
					return err
				}
				cur.Status.Plan.Pending = "plan-main-new"
				if err := c.Update(ctx, cur); err != nil {
					return err
				}
				return c.Patch(ctx, o, p, opts...)
			},
		}).Build()

	tc := &Client{Kube: kubeClient, Out: &bytes.Buffer{}}
	err = tc.Approve(context.Background(), "stack-a", "plan-main-old", "plan text")
	if !apierrors.IsConflict(err) {
		t.Fatalf("Approve error = %v, want a conflict", err)
	}
	if got, _ := tc.Get(context.Background(), "stack-a"); got.Spec.ApprovePlan != "" {
		t.Errorf("approvePlan = %q, want empty", got.Spec.ApprovePlan)
	}
}

func TestApproveRefusesWhenPendingChangedDuringRun(t *testing.T) {
	// The user saw plan-main-old, but the controller produced a new plan
	// before the approval was sent. The write must not happen.
	obj := newTerraform("stack-a", "plan-main-new", "human")
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
				writes++
				return c.Patch(ctx, o, p, opts...)
			},
		}).Build()

	tc := &Client{Kube: kubeClient, Out: &bytes.Buffer{}}
	if err := tc.Approve(context.Background(), "stack-a", "plan-main-old", "plan text"); err == nil {
		t.Fatal("expected error")
	}
	if writes != 0 {
		t.Errorf("Approve wrote %d times despite the changed pending plan", writes)
	}
	if got, _ := tc.Get(context.Background(), "stack-a"); got.Spec.ApprovePlan != "" {
		t.Errorf("approvePlan = %q, want empty", got.Spec.ApprovePlan)
	}
}

// TestApproveRefusesWhenPlanContentChanged covers a replan of the same
// revision: the plan ID only reflects the revision, so it is identical while
// the plan content differs from what the user reviewed.
func TestApproveRefusesWhenPlanContentChanged(t *testing.T) {
	writes := 0
	obj := newTerraform("stack-a", "plan-main-abc123", "human")
	tc := newClientWith(t, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
			writes++
			return c.Patch(ctx, o, p, opts...)
		},
	}, withPlan(t, obj, "plan-main-abc123", "Plan: 1 to destroy.")...)

	err := tc.Approve(context.Background(), "stack-a", "plan-main-abc123", "Plan: 1 to add.")
	if err == nil || !strings.Contains(err.Error(), "changed since it was shown") {
		t.Fatalf("Approve error = %v, want a refusal because the plan content changed", err)
	}
	if writes != 0 {
		t.Errorf("Approve wrote %d times for a plan the user did not see", writes)
	}
}

func TestApproveRefusesWhenPlanCannotBeRead(t *testing.T) {
	writes := 0
	tc := newClientWith(t, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
			writes++
			return c.Patch(ctx, o, p, opts...)
		},
	}, newTerraform("stack-a", "plan-main-abc123", "human"))

	if err := tc.Approve(context.Background(), "stack-a", "plan-main-abc123", "Plan: 1 to add."); err == nil {
		t.Fatal("expected error when the stored plan is missing")
	}
	if writes != 0 {
		t.Errorf("Approve wrote %d times without a readable plan", writes)
	}
}

// failingWrites makes every kind of write fail and counts how many reached the
// client, so tests can assert both error handling and dry-run behavior.
func failingWrites(writes *int) interceptor.Funcs {
	fail := func() error {
		*writes++
		return errors.New("write denied")
	}
	return interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return fail()
		},
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return fail()
		},
	}
}

// withPlan returns obj together with the stored plan of its pending plan ID.
func withPlan(t *testing.T, obj *infrav1.Terraform, storedID, text string) []client.Object {
	t.Helper()
	return append([]client.Object{obj}, planConfigMaps(t, obj, storedID, text)...)
}

func newClientWith(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *Client {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&infrav1.Terraform{}).WithInterceptorFuncs(funcs).Build()
	return &Client{Kube: kubeClient, Out: &bytes.Buffer{}, Now: func() time.Time { return clientNow }}
}

func TestListSortsByName(t *testing.T) {
	tc := newClientWith(t, interceptor.Funcs{},
		newTerraform("stack-c", "", "human"), newTerraform("stack-a", "", "human"), newTerraform("stack-b", "", "human"))

	got, err := tc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range got {
		names = append(names, o.Name)
	}
	if want := "stack-a stack-b stack-c"; strings.Join(names, " ") != want {
		t.Errorf("names = %v, want %s", names, want)
	}
}

func TestListIgnoresOtherNamespaces(t *testing.T) {
	other := newTerraform("stack-x", "", "human")
	other.Namespace = "other"
	tc := newClientWith(t, interceptor.Funcs{}, newTerraform("stack-a", "", "human"), other)

	got, err := tc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "stack-a" {
		t.Errorf("List returned %d objects, want only stack-a", len(got))
	}
}

func TestListError(t *testing.T) {
	tc := newClientWith(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("api down")
		},
	})
	if _, err := tc.List(context.Background()); err == nil || !strings.Contains(err.Error(), "api down") {
		t.Errorf("err = %v, want it to wrap the API error", err)
	}
}

func TestGetNotFound(t *testing.T) {
	tc := newClientWith(t, interceptor.Funcs{})
	if _, err := tc.Get(context.Background(), "missing"); !apierrors.IsNotFound(err) {
		t.Errorf("err = %v, want a not found error", err)
	}
}

func TestSetSuspend(t *testing.T) {
	tc := newClientWith(t, interceptor.Funcs{}, newTerraform("stack-a", "", "human"))

	for _, suspend := range []bool{true, false} {
		if err := tc.SetSuspend(context.Background(), "stack-a", suspend); err != nil {
			t.Fatal(err)
		}
		got, err := tc.Get(context.Background(), "stack-a")
		if err != nil {
			t.Fatal(err)
		}
		if got.Spec.Suspend != suspend {
			t.Errorf("spec.suspend = %v, want %v", got.Spec.Suspend, suspend)
		}
	}
}

// TestWriteOperationsDryRunAndErrors runs each write operation twice: with
// DryRun nothing may reach the API and the intended change is printed, without
// it a failing write must surface as an error.
func TestWriteOperationsDryRunAndErrors(t *testing.T) {
	ops := []struct {
		name       string
		run        func(tc *Client) error
		wantDryRun string
	}{
		{"approve", func(tc *Client) error {
			return tc.Approve(context.Background(), "stack-a", "plan-main-abc123", "plan text")
		},
			`[dry-run] would merge-patch terraform flux-system/stack-a: {"metadata":{"resourceVersion":"999"},"spec":{"approvePlan":"plan-main-abc123"}}`},
		{"suspend", func(tc *Client) error { return tc.SetSuspend(context.Background(), "stack-a", true) },
			`[dry-run] would merge-patch terraform flux-system/stack-a: {"spec":{"suspend":true}}`},
		{"resume", func(tc *Client) error { return tc.SetSuspend(context.Background(), "stack-a", false) },
			`[dry-run] would merge-patch terraform flux-system/stack-a: {"spec":{"suspend":false}}`},
		{"request reconcile", func(tc *Client) error { return tc.RequestReconcile(context.Background(), "stack-a") },
			`[dry-run] would merge-patch terraform flux-system/stack-a: {"metadata":{"annotations":{"reconcile.fluxcd.io/requestedAt":"2026-10-13T12:00:00Z"}}}`},
		{"replan", func(tc *Client) error { return tc.Replan(context.Background(), "stack-a") },
			`[dry-run] would clear status.plan.pending`},
	}
	for _, op := range ops {
		t.Run(op.name+" dry-run", func(t *testing.T) {
			writes := 0
			tc := newClientWith(t, failingWrites(&writes), withPlan(t, newTerraform("stack-a", "plan-main-abc123", "human"), "plan-main-abc123", "plan text")...)
			tc.DryRun = true
			out := &bytes.Buffer{}
			tc.Out = out

			if err := op.run(tc); err != nil {
				t.Fatalf("dry-run returned an error: %v", err)
			}
			if writes != 0 {
				t.Errorf("dry-run attempted %d writes", writes)
			}
			if !strings.Contains(out.String(), op.wantDryRun) {
				t.Errorf("output = %q, want it to contain %q", out, op.wantDryRun)
			}
		})
		t.Run(op.name+" write error", func(t *testing.T) {
			writes := 0
			tc := newClientWith(t, failingWrites(&writes), withPlan(t, newTerraform("stack-a", "plan-main-abc123", "human"), "plan-main-abc123", "plan text")...)

			err := op.run(tc)
			if err == nil || !strings.Contains(err.Error(), "write denied") {
				t.Errorf("err = %v, want it to wrap the write error", err)
			}
		})
		t.Run(op.name+" missing object", func(t *testing.T) {
			tc := newClientWith(t, interceptor.Funcs{})
			if err := op.run(tc); !apierrors.IsNotFound(err) {
				t.Errorf("err = %v, want a not found error", err)
			}
		})
	}
}

func TestNowDefaultsToWallClock(t *testing.T) {
	tc := &Client{}
	before := time.Now()
	got := tc.now()
	if got.Before(before) || got.After(time.Now()) {
		t.Errorf("now() = %v, want the current time", got)
	}
}

func TestSummary(t *testing.T) {
	created := metav1.NewTime(clientNow.Add(-50 * time.Hour))

	t.Run("pending plan, ready, suspended", func(t *testing.T) {
		obj := newTerraform("stack-a", "plan-main-abc123", "human")
		obj.CreationTimestamp = created
		obj.Spec.Suspend = true
		apimeta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type: meta.ReadyCondition, Status: metav1.ConditionTrue, Reason: "Succeeded", Message: "Applied successfully",
		})

		want := "NAME:      stack-a\n" +
			"READY:     True\n" +
			"MESSAGE:   Applied successfully\n" +
			"PENDING:   plan-main-abc123\n" +
			"SUSPENDED: true\n" +
			"AGE:       2d"
		if got := Summary(obj, clientNow); got != want {
			t.Errorf("Summary =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("no ready condition, no pending plan", func(t *testing.T) {
		obj := newTerraform("stack-b", "", "human")
		obj.CreationTimestamp = created

		got := Summary(obj, clientNow)
		for _, want := range []string{"READY:     Unknown\n", "MESSAGE:   \n", "PENDING:   -\n", "SUSPENDED: false\n"} {
			if !strings.Contains(got, want) {
				t.Errorf("Summary = %q, want it to contain %q", got, want)
			}
		}
	})
}

func TestAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0m"},
		{-2 * time.Minute, "0m"},
		{30 * time.Second, "0m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{23*time.Hour + 59*time.Minute, "23h"},
		{24 * time.Hour, "1d"},
		{49 * time.Hour, "2d"},
	}
	for _, tt := range tests {
		if got := age(tt.d); got != tt.want {
			t.Errorf("age(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

package flux

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"tfgate/internal/kube"
)

var fluxNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

const reconcileAnnotation = "reconcile.fluxcd.io/requestedAt"

func newOCIRepository(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(OCIRepositoryGVK)
	obj.SetName(name)
	obj.SetNamespace("flux-system")
	return obj
}

func getOCIRepository(t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(OCIRepositoryGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "flux-system", Name: name}, obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func newFake(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func TestReconcileOCISetsAnnotation(t *testing.T) {
	c := newFake(t, interceptor.Funcs{}, newOCIRepository("stack-a-iac"))

	if err := ReconcileOCI(context.Background(), c, "flux-system", "stack-a-iac", fluxNow, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got := getOCIRepository(t, c, "stack-a-iac").GetAnnotations()[reconcileAnnotation]
	if want := fluxNow.Format(time.RFC3339Nano); got != want {
		t.Errorf("%s = %q, want %q", reconcileAnnotation, got, want)
	}
}

// TestReconcileOCIPatchesWithoutReading pins that a merge patch needs no
// prior read: the object is addressed by name only.
func TestReconcileOCIPatchesWithoutReading(t *testing.T) {
	gets, patches := 0, 0
	c := newFake(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gets++
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			patches++
			return c.Patch(ctx, obj, p, opts...)
		},
	}, newOCIRepository("stack-a-iac"))

	if err := ReconcileOCI(context.Background(), c, "flux-system", "stack-a-iac", fluxNow, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if gets != 0 || patches != 1 {
		t.Errorf("gets = %d, patches = %d, want 0 gets and 1 patch", gets, patches)
	}
}

func TestReconcileOCIMissingRepository(t *testing.T) {
	c := newFake(t, interceptor.Funcs{})

	err := ReconcileOCI(context.Background(), c, "flux-system", "stack-a-iac", fluxNow, false, &bytes.Buffer{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want a not found error", err)
	}
	if !strings.Contains(err.Error(), "stack-a-iac") {
		t.Errorf("error should name the repository, got: %v", err)
	}
}

func TestReconcileOCIDryRunDoesNotWrite(t *testing.T) {
	writes := 0
	c := newFake(t, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			writes++
			return c.Patch(ctx, obj, p, opts...)
		},
	}, newOCIRepository("stack-a-iac"))
	out := &bytes.Buffer{}

	if err := ReconcileOCI(context.Background(), c, "flux-system", "stack-a-iac", fluxNow, true, out); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Errorf("dry-run wrote %d times", writes)
	}
	want := `[dry-run] would merge-patch ocirepository flux-system/stack-a-iac: {"metadata":{"annotations":{"reconcile.fluxcd.io/requestedAt":"2026-10-13T12:00:00Z"}}}`
	if !strings.Contains(out.String(), want) {
		t.Errorf("output = %q, want it to contain %q", out, want)
	}
	if _, ok := getOCIRepository(t, c, "stack-a-iac").GetAnnotations()[reconcileAnnotation]; ok {
		t.Error("annotation set in dry-run")
	}
}

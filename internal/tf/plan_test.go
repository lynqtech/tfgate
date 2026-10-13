package tf

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/flux-iac/tofu-controller/api/plan"
	infrav1 "github.com/flux-iac/tofu-controller/api/v1alpha2"

	"tfgate/internal/kube"
)

func newTerraform(name, pending, storeReadablePlan string) *infrav1.Terraform {
	obj := &infrav1.Terraform{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, UID: types.UID(name + "-uid")},
		Spec:       infrav1.TerraformSpec{StoreReadablePlan: storeReadablePlan},
	}
	obj.Status.Plan.Pending = pending
	return obj
}

func newFake(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// planSecrets builds the Secrets tofu-controller stores for the readable plan
// of obj with storeReadablePlan: json, with the plan ID taken from storedID.
// The controller stores the JSON plan under the ".json" name suffix, see
// runner/server_save_tfplan.go.
func planSecrets(t *testing.T, obj *infrav1.Terraform, storedID, text string) []*corev1.Secret {
	t.Helper()
	return planSecretsWithSuffix(t, obj, storedID, text, ".json")
}

// binaryPlanSecrets builds the Secrets holding the binary plan file, which the
// controller stores for every stack next to the readable plan.
func binaryPlanSecrets(t *testing.T, obj *infrav1.Terraform, storedID string) []*corev1.Secret {
	t.Helper()
	return planSecretsWithSuffix(t, obj, storedID, "binary planfile, not readable", "")
}

func planSecretsWithSuffix(t *testing.T, obj *infrav1.Terraform, storedID, text, suffix string) []*corev1.Secret {
	t.Helper()
	p, err := plan.NewFromBytes(obj.Name, obj.Namespace, obj.WorkspaceName(), string(obj.UID), storedID, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := p.ToSecret(suffix)
	if err != nil {
		t.Fatal(err)
	}
	return secrets
}

func asObjects[T client.Object](in []T) []client.Object {
	out := make([]client.Object, 0, len(in))
	for _, o := range in {
		out = append(out, o)
	}
	return out
}

func TestReadableJSONSecret(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		name := "single"
		if bulk {
			name = "bulk"
		}
		t.Run(name, func(t *testing.T) {
			obj := newTerraform("stack-a", "plan-main-abc123", "json")
			other := newTerraform("stack-b", "plan-main-def456", "json")
			objs := append(asObjects(planSecrets(t, obj, "plan-main-abc123", "Plan for stack-a")),
				asObjects(planSecrets(t, other, "plan-main-def456", "Plan for stack-b"))...)
			p := NewPlans(newFake(t, objs...), bulk)

			got, err := p.Readable(context.Background(), obj)
			if err != nil {
				t.Fatal(err)
			}
			if got != "Plan for stack-a" {
				t.Errorf("Readable = %q, want the plan of stack-a only", got)
			}
		})
	}
}

func TestReadableJSONIgnoresBinaryPlanSecret(t *testing.T) {
	// The binary planfile Secret carries the same savedPlan annotation as the
	// JSON one. Reading it would pass the plan ID check and print garbage.
	for _, bulk := range []bool{false, true} {
		name := "single"
		if bulk {
			name = "bulk"
		}
		t.Run(name, func(t *testing.T) {
			obj := newTerraform("stack-a", "plan-main-abc123", "json")
			objs := append(asObjects(binaryPlanSecrets(t, obj, "plan-main-abc123")),
				asObjects(planSecrets(t, obj, "plan-main-abc123", "Plan: 1 to add."))...)

			got, err := NewPlans(newFake(t, objs...), bulk).Readable(context.Background(), obj)
			if err != nil {
				t.Fatal(err)
			}
			if got != "Plan: 1 to add." {
				t.Errorf("Readable = %q, want the JSON plan", got)
			}
		})
	}
}

func TestReadableJSONFailsWithOnlyBinaryPlanSecret(t *testing.T) {
	// Without the JSON Secret there is nothing readable to show, so the plan
	// must not be offered for approval.
	obj := newTerraform("stack-a", "plan-main-abc123", "json")
	p := NewPlans(newFake(t, asObjects(binaryPlanSecrets(t, obj, "plan-main-abc123"))...), false)

	got, err := p.Readable(context.Background(), obj)
	if err == nil {
		t.Fatalf("expected error, got plan %q", got)
	}
}

func TestReadableJSONSecretChunked(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123", "json")
	secrets := planSecrets(t, obj, "plan-main-abc123", "Plan: 1 to add, 0 to change, 0 to destroy.")
	if len(secrets) != 1 {
		t.Fatalf("expected one secret for a small plan, got %d", len(secrets))
	}

	// Split the gzip stream into two chunks, stored out of order.
	whole := secrets[0]
	data := whole.Data[plan.TFPlanName]
	half := len(data) / 2
	chunk := func(idx, suffix string, part []byte) client.Object {
		c := whole.DeepCopy()
		c.Name = whole.Name + suffix
		c.ResourceVersion = ""
		c.Data = map[string][]byte{plan.TFPlanName: part}
		c.Annotations[plan.TFPlanChunkAnnotation] = idx
		return c
	}
	objs := []client.Object{chunk("1", "-1", data[half:]), chunk("0", "-0", data[:half])}

	got, err := NewPlans(newFake(t, objs...), true).Readable(context.Background(), obj)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Plan: 1 to add, 0 to change, 0 to destroy." {
		t.Errorf("Readable = %q", got)
	}
}

func TestReadableJSONSecretMissingChunk(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123", "json")
	secrets := planSecrets(t, obj, "plan-main-abc123", "plan")
	c := secrets[0].DeepCopy()
	c.Annotations[plan.TFPlanChunkAnnotation] = "1" // chunk 0 is missing
	c.ResourceVersion = ""

	if _, err := NewPlans(newFake(t, c), false).Readable(context.Background(), obj); err == nil {
		t.Fatal("expected error for a missing chunk")
	}
}

func TestReadableRefusesStalePlan(t *testing.T) {
	// The stored plan belongs to plan-main-old but plan-main-new is pending.
	// This must fail for both storage formats, otherwise the user would
	// review one plan and approve another.
	tests := []struct {
		name  string
		store string
		build func(t *testing.T, obj *infrav1.Terraform) []client.Object
	}{
		{"json secret", "json", func(t *testing.T, obj *infrav1.Terraform) []client.Object {
			return asObjects(planSecrets(t, obj, "plan-main-old", "old plan"))
		}},
		{"human configmap", "human", func(t *testing.T, obj *infrav1.Terraform) []client.Object {
			p, err := plan.NewFromBytes(obj.Name, obj.Namespace, obj.WorkspaceName(), string(obj.UID), "plan-main-old", []byte("old plan"))
			if err != nil {
				t.Fatal(err)
			}
			cms, err := p.ToConfigMap("")
			if err != nil {
				t.Fatal(err)
			}
			return asObjects(cms)
		}},
	}
	for _, tt := range tests {
		for _, bulk := range []bool{false, true} {
			t.Run(tt.name, func(t *testing.T) {
				obj := newTerraform("stack-a", "plan-main-new", tt.store)
				got, err := NewPlans(newFake(t, tt.build(t, obj)...), bulk).Readable(context.Background(), obj)
				if err == nil {
					t.Fatalf("expected error, got plan %q", got)
				}
				if got != "" {
					t.Errorf("plan text %q must not be returned with an error", got)
				}
				if !strings.Contains(err.Error(), "plan-main-old") || !strings.Contains(err.Error(), "plan-main-new") {
					t.Errorf("error should name both plan IDs, got: %v", err)
				}
			})
		}
	}
}

func TestReadableErrors(t *testing.T) {
	tests := []struct {
		name    string
		obj     *infrav1.Terraform
		wantErr string
		want    string
	}{
		{"no pending plan", newTerraform("stack-a", "", "json"), "", NoPlanPending},
		{"json without secrets", newTerraform("stack-a", "plan-main-abc123", "json"), "no plan secrets found", ""},
		{"human without configmaps", newTerraform("stack-a", "plan-main-abc123", "human"), "no plan configmaps found", ""},
		{"storeReadablePlan unset", newTerraform("stack-a", "plan-main-abc123", ""), "storeReadablePlan", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewPlans(newFake(t), false).Readable(context.Background(), tt.obj)
			if tt.wantErr == "" {
				if err != nil || got != tt.want {
					t.Errorf("Readable = %q, %v, want %q, nil", got, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestReadableLegacyConfigMapErrors(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123", "human")
	legacy := func(annotations map[string]string, data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "tfplan-default-stack-a", Namespace: Namespace, Annotations: annotations},
			Data:       data,
		}
	}
	tests := []struct {
		name    string
		cm      *corev1.ConfigMap
		wantErr string
	}{
		{"stale", legacy(map[string]string{"savedPlan": "plan-main-old"}, map[string]string{"tfplan": "old"}), "plan-main-old"},
		{"missing key", legacy(map[string]string{"savedPlan": "plan-main-abc123"}, nil), "missing key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewPlans(newFake(t, tt.cm), false).Readable(context.Background(), obj)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func planConfigMaps(t *testing.T, obj *infrav1.Terraform, storedID, text string) []client.Object {
	t.Helper()
	p, err := plan.NewFromBytes(obj.Name, obj.Namespace, obj.WorkspaceName(), string(obj.UID), storedID, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	cms, err := p.ToConfigMap("")
	if err != nil {
		t.Fatal(err)
	}
	return asObjects(cms)
}

// chunkedConfigMaps stores a plan of size bytes, which tofu-controller splits
// into chunks of 1 MiB.
func chunkedConfigMaps(t *testing.T, obj *infrav1.Terraform, storedID string, size int) []*corev1.ConfigMap {
	t.Helper()
	p, err := plan.NewFromBytes(obj.Name, obj.Namespace, obj.WorkspaceName(), string(obj.UID), storedID, []byte(strings.Repeat("x", size)))
	if err != nil {
		t.Fatal(err)
	}
	cms, err := p.ToConfigMap("")
	if err != nil {
		t.Fatal(err)
	}
	return cms
}

func TestReadableHumanChunkedComplete(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123", "human")
	cms := chunkedConfigMaps(t, obj, "plan-main-abc123", maxChunkSize+10)
	if len(cms) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(cms))
	}

	got, err := NewPlans(newFake(t, asObjects(cms)...), false).Readable(context.Background(), obj)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxChunkSize+10 {
		t.Errorf("plan has %d bytes, want %d", len(got), maxChunkSize+10)
	}
}

func TestReadableRefusesTruncatedChunks(t *testing.T) {
	// A List during a rewrite of the plan can return only the first chunks.
	// The reassembled plan would silently end early, maybe before the destroy
	// section, so it must not be shown for approval.
	obj := newTerraform("stack-a", "plan-main-abc123", "human")
	cms := chunkedConfigMaps(t, obj, "plan-main-abc123", 2*maxChunkSize+10)
	if len(cms) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(cms))
	}

	for _, bulk := range []bool{false, true} {
		got, err := NewPlans(newFake(t, asObjects(cms[:2])...), bulk).Readable(context.Background(), obj)
		if err == nil {
			t.Fatalf("bulk=%v: expected error for a truncated plan, got %d bytes", bulk, len(got))
		}
		if !strings.Contains(err.Error(), "incomplete") {
			t.Errorf("bulk=%v: error should say the plan is incomplete, got: %v", bulk, err)
		}
	}
}

func TestReadableRefusesMixedChunks(t *testing.T) {
	// Chunk 0 is already the new plan, chunk 1 is still the old one. Only the
	// first object used to be checked, so this was shown as one plan.
	obj := newTerraform("stack-a", "plan-main-new", "human")
	fresh := chunkedConfigMaps(t, obj, "plan-main-new", maxChunkSize+10)
	stale := chunkedConfigMaps(t, obj, "plan-main-old", maxChunkSize+10)

	_, err := NewPlans(newFake(t, fresh[0], stale[1]), false).Readable(context.Background(), obj)
	if err == nil {
		t.Fatal("expected error for chunks of different plans")
	}
	if !strings.Contains(err.Error(), "plan-main-old") || !strings.Contains(err.Error(), "plan-main-new") {
		t.Errorf("error should name both plan IDs, got: %v", err)
	}
}

func TestCheckStoredPlanSecretsOfFullSizeLastChunk(t *testing.T) {
	// Secret chunks are gzip data of 1 MiB each, the same size rule applies.
	obj := newTerraform("stack-a", "plan-main-abc123", "json")
	part := func(chunk string, size int) storedPart {
		return storedPart{
			annotations: map[string]string{plan.TFPlanSavedAnnotation: "plan-main-abc123", plan.TFPlanChunkAnnotation: chunk},
			size:        size,
		}
	}
	if err := checkStoredPlan(obj, []storedPart{part("0", maxChunkSize), part("1", 5)}); err != nil {
		t.Errorf("complete chunk set rejected: %v", err)
	}
	if err := checkStoredPlan(obj, []storedPart{part("0", maxChunkSize), part("1", maxChunkSize)}); err == nil {
		t.Error("full-size last chunk accepted, the next chunk may be missing")
	}
}

func TestReadableHumanConfigMap(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		name := "single"
		if bulk {
			name = "bulk"
		}
		t.Run(name, func(t *testing.T) {
			obj := newTerraform("stack-a", "plan-main-abc123", "human")
			other := newTerraform("stack-b", "plan-main-def456", "human")
			objs := append(planConfigMaps(t, obj, "plan-main-abc123", "Plan for stack-a"),
				planConfigMaps(t, other, "plan-main-def456", "Plan for stack-b")...)
			p := NewPlans(newFake(t, objs...), bulk)

			got, err := p.Readable(context.Background(), obj)
			if err != nil {
				t.Fatal(err)
			}
			if got != "Plan for stack-a" {
				t.Errorf("Readable = %q, want the plan of stack-a only", got)
			}
		})
	}
}

func TestReadableLegacyConfigMap(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123", "human")
	legacy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tfplan-default-stack-a", Namespace: Namespace,
			Annotations: map[string]string{plan.TFPlanSavedAnnotation: "plan-main-abc123"},
		},
		Data: map[string]string{plan.TFPlanName: "legacy plan text"},
	}
	for _, bulk := range []bool{false, true} {
		got, err := NewPlans(newFake(t, legacy), bulk).Readable(context.Background(), obj)
		if err != nil || got != "legacy plan text" {
			t.Errorf("bulk=%v: Readable = %q, %v, want the legacy plan", bulk, got, err)
		}
	}
}

func TestReadableLegacyConfigMapGetError(t *testing.T) {
	obj := newTerraform("stack-a", "plan-main-abc123", "human")
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("api down")
		},
	}).Build()

	_, err = NewPlans(kubeClient, false).Readable(context.Background(), obj)
	if err == nil || !strings.Contains(err.Error(), "api down") || !strings.Contains(err.Error(), "tfplan-default-stack-a") {
		t.Errorf("err = %v, want it to name the configmap and wrap the API error", err)
	}
}

func TestReadableListErrors(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	failing := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("api down")
		},
	}).Build()

	for _, store := range []string{"human", "json"} {
		for _, bulk := range []bool{false, true} {
			obj := newTerraform("stack-a", "plan-main-abc123", store)
			_, err := NewPlans(failing, bulk).Readable(context.Background(), obj)
			if err == nil || !strings.Contains(err.Error(), "api down") {
				t.Errorf("store=%s bulk=%v: err = %v, want it to wrap the API error", store, bulk, err)
			}
		}
	}
}

// TestBulkModeListsOnce pins the constant API call count: in bulk mode all
// stacks are served from a single list, in single mode each lookup lists.
func TestBulkModeListsOnce(t *testing.T) {
	for _, store := range []string{"human", "json"} {
		for _, bulk := range []bool{false, true} {
			t.Run(store, func(t *testing.T) {
				stacks := []*infrav1.Terraform{
					newTerraform("stack-a", "plan-main-a", store),
					newTerraform("stack-b", "plan-main-b", store),
					newTerraform("stack-c", "plan-main-c", store),
				}
				var objs []client.Object
				for _, s := range stacks {
					id := s.Status.Plan.Pending
					if store == "human" {
						objs = append(objs, planConfigMaps(t, s, id, "Plan for "+s.Name)...)
					} else {
						objs = append(objs, asObjects(planSecrets(t, s, id, "Plan for "+s.Name))...)
					}
				}
				scheme, err := kube.Scheme()
				if err != nil {
					t.Fatal(err)
				}
				lists := 0
				kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
					List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
						lists++
						return c.List(ctx, l, opts...)
					},
				}).Build()

				p := NewPlans(kubeClient, bulk)
				for _, s := range stacks {
					got, err := p.Readable(context.Background(), s)
					if err != nil || got != "Plan for "+s.Name {
						t.Fatalf("Readable(%s) = %q, %v", s.Name, got, err)
					}
				}
				want := len(stacks)
				if bulk {
					want = 1
				}
				if lists != want {
					t.Errorf("bulk=%v: %d list calls, want %d", bulk, lists, want)
				}
			})
		}
	}
}

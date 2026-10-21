// Package flux triggers reconciliation of Flux sources.
package flux

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OCIRepositoryGVK is the OCIRepository version used in lqt-configuration-store.
var OCIRepositoryGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "OCIRepository"}

// ReconcileOCI does what `flux reconcile source oci <name>` triggers: it sets
// the reconcile request annotation. It does not wait for the result.
func ReconcileOCI(ctx context.Context, c client.Client, namespace, name string, now time.Time, dryRun bool, out io.Writer) error {
	data, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		meta.ReconcileRequestAnnotation: now.Format(time.RFC3339Nano),
	}}})
	if err != nil {
		return err
	}
	if dryRun {
		_, _ = fmt.Fprintf(out, "[dry-run] would merge-patch ocirepository %s/%s: %s\n", namespace, name, data)
		return nil
	}
	// A merge patch only needs the identity of the object, no prior read.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(OCIRepositoryGVK)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	if err := c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, data)); err != nil {
		return fmt.Errorf("annotating ocirepository %s: %w", name, err)
	}
	return nil
}

// Package tf reads and modifies tofu-controller Terraform objects.
package tf

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/flux-iac/tofu-controller/api/v1alpha2"
)

// Namespace is where all Terraform objects live.
const Namespace = "flux-system"

// Terraform is the tofu-controller Terraform object.
type Terraform = infrav1.Terraform

// Client performs operations on Terraform objects in one cluster. With
// DryRun set, write operations print the intended change instead of
// applying it.
type Client struct {
	Kube   client.Client
	DryRun bool
	Out    io.Writer
	Now    func() time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: Namespace, Name: name}
}

// List returns all Terraform objects, sorted by name.
func (c *Client) List(ctx context.Context) ([]Terraform, error) {
	list := &infrav1.TerraformList{}
	if err := c.Kube.List(ctx, list, client.InNamespace(Namespace)); err != nil {
		return nil, fmt.Errorf("listing terraforms: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items, nil
}

// Get returns the Terraform object with the given name.
func (c *Client) Get(ctx context.Context, name string) (*infrav1.Terraform, error) {
	tf := &infrav1.Terraform{}
	if err := c.Kube.Get(ctx, key(name), tf); err != nil {
		return nil, err
	}
	return tf, nil
}

// Approve sets spec.approvePlan to pending. It re-reads the object first and
// refuses to approve if the pending plan changed in the meantime.
//
// The plan ID only reflects the source revision, so the same ID can come with
// different content, for example after a replan. Approve therefore also reads
// the stored plan again and refuses if it differs from reviewed, the text the
// user was shown. The patch carries the resourceVersion of the first read, so
// the API server rejects it with a conflict if the object changed after that,
// which includes a new plan being stored and made pending.
func (c *Client) Approve(ctx context.Context, name, pending, reviewed string) error {
	tf, err := c.Get(ctx, name)
	if err != nil {
		return err
	}
	if tf.Status.Plan.Pending != pending {
		return fmt.Errorf("pending plan of %s changed from %q to %q, not approving", name, pending, tf.Status.Plan.Pending)
	}
	current, err := NewPlans(c.Kube, false).Readable(ctx, tf)
	if err != nil {
		return fmt.Errorf("reading plan of %s again, not approving: %w", name, err)
	}
	if current != reviewed {
		return fmt.Errorf("plan of %s changed since it was shown, not approving", name)
	}
	return c.mergePatch(ctx, tf, map[string]any{
		"metadata": map[string]any{"resourceVersion": tf.ResourceVersion},
		"spec":     map[string]any{"approvePlan": pending},
	})
}

// SetSuspend sets spec.suspend.
func (c *Client) SetSuspend(ctx context.Context, name string, suspend bool) error {
	tf, err := c.Get(ctx, name)
	if err != nil {
		return err
	}
	return c.mergePatch(ctx, tf, map[string]any{"spec": map[string]any{"suspend": suspend}})
}

// Replan does what `tfctl replan` does: clear the pending plan in the status,
// mark the object as not ready and request a reconciliation.
//
// A JSON merge patch replaces status.conditions as a whole, so the patch
// carries the resourceVersion of the read. If the controller updated the object
// in between, the API server answers with a conflict and the retry re-reads
// instead of overwriting the controller's condition changes.
func (c *Client) Replan(ctx context.Context, name string) error {
	if c.DryRun {
		_, _ = fmt.Fprintf(c.Out, "[dry-run] would clear status.plan.pending, status.lastPlannedRevision, status.lastAttemptedRevision, set Ready=False (ReplanRequested) and annotate %s=<now> on terraform %s/%s\n",
			meta.ReconcileRequestAnnotation, Namespace, name)
		return nil
	}
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		tf, err := c.Get(ctx, name)
		if err != nil {
			return err
		}
		patch := client.MergeFromWithOptions(tf.DeepCopy(), client.MergeFromWithOptimisticLock{})
		apimeta.SetStatusCondition(&tf.Status.Conditions, metav1.Condition{
			Type:    meta.ReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  "ReplanRequested",
			Message: "Replan requested",
		})
		tf.Status.Plan.Pending = ""
		tf.Status.LastPlannedRevision = ""
		tf.Status.LastAttemptedRevision = ""
		return c.Kube.Status().Patch(ctx, tf, patch, &client.SubResourcePatchOptions{
			PatchOptions: client.PatchOptions{FieldManager: "tf-controller"},
		})
	})
	if err != nil {
		return fmt.Errorf("clearing plan status of %s: %w", name, err)
	}
	return c.RequestReconcile(ctx, name)
}

// RequestReconcile sets the Flux reconcile annotation on the object.
func (c *Client) RequestReconcile(ctx context.Context, name string) error {
	tf, err := c.Get(ctx, name)
	if err != nil {
		return err
	}
	return c.mergePatch(ctx, tf, map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		meta.ReconcileRequestAnnotation: c.now().Format(time.RFC3339Nano),
	}}})
}

func (c *Client) mergePatch(ctx context.Context, obj client.Object, body map[string]any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if c.DryRun {
		_, _ = fmt.Fprintf(c.Out, "[dry-run] would merge-patch terraform %s/%s: %s\n", obj.GetNamespace(), obj.GetName(), data)
		return nil
	}
	if err := c.Kube.Patch(ctx, obj, client.RawPatch(types.MergePatchType, data)); err != nil {
		return fmt.Errorf("patching %s: %w", obj.GetName(), err)
	}
	return nil
}

// Summary returns a one-line status like `tfctl get` prints per object.
func Summary(tf *infrav1.Terraform, now time.Time) string {
	ready := apimeta.FindStatusCondition(tf.Status.Conditions, meta.ReadyCondition)
	status, message := "Unknown", ""
	if ready != nil {
		status, message = string(ready.Status), ready.Message
	}
	pending := tf.Status.Plan.Pending
	if pending == "" {
		pending = "-"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "NAME:      %s\n", tf.Name)
	fmt.Fprintf(&b, "READY:     %s\n", status)
	fmt.Fprintf(&b, "MESSAGE:   %s\n", message)
	fmt.Fprintf(&b, "PENDING:   %s\n", pending)
	fmt.Fprintf(&b, "SUSPENDED: %t\n", tf.Spec.Suspend)
	fmt.Fprintf(&b, "AGE:       %s", age(now.Sub(tf.CreationTimestamp.Time)))
	return b.String()
}

func age(d time.Duration) string {
	if d < 0 {
		// Local clock behind the cluster; do not print negative ages.
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

package tf

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/flux-iac/tofu-controller/api/plan"
	infrav1 "github.com/flux-iac/tofu-controller/api/v1alpha2"
)

// NoPlanPending is printed when the object has no pending plan, matching tfctl.
const NoPlanPending = "There is no plan pending."

// jsonPlanSuffix is appended to the stack name in the plan labels of the Secret
// that holds the JSON plan (storeReadablePlan: json).
const jsonPlanSuffix = ".json"

// maxChunkSize is the size at which tofu-controller splits a stored plan into
// chunks. It is not exported by the plan package.
const maxChunkSize = 1024 * 1024

type planKey struct{ name, workspace string }

// Plans reads human-readable plans the same way `tfctl show plan` does: from
// ConfigMaps (storeReadablePlan: human) or Secrets (storeReadablePlan: json),
// selected by plan labels and reassembled from chunks.
//
// In bulk mode, the first lookup lists the plan ConfigMaps (or Secrets) of
// all stacks in one call and later lookups are served from memory. Use bulk
// mode when iterating over many stacks, and per-stack mode for a single one.
type Plans struct {
	kube       client.Client
	bulk       bool
	configMaps map[planKey][]corev1.ConfigMap
	secrets    map[planKey][]corev1.Secret
}

// NewPlans returns a plan reader. See Plans for the meaning of bulk.
func NewPlans(kube client.Client, bulk bool) *Plans {
	return &Plans{kube: kube, bulk: bulk}
}

// Readable returns the human-readable plan of tf.
func (p *Plans) Readable(ctx context.Context, tf *infrav1.Terraform) (string, error) {
	if tf.Status.Plan.Pending == "" {
		return NoPlanPending, nil
	}
	k := planKey{name: plan.SafeLabelValue(tf.Name), workspace: plan.SafeLabelValue(tf.WorkspaceName())}
	uuid := string(tf.GetUID())

	switch tf.Spec.StoreReadablePlan {
	case "human":
		items, err := p.planConfigMaps(ctx, tf.Namespace, k)
		if err != nil {
			return "", err
		}
		if len(items) == 0 {
			return p.legacyConfigMapPlan(ctx, tf)
		}
		parts := make([]storedPart, len(items))
		for i, cm := range items {
			parts[i] = storedPart{annotations: cm.Annotations, size: len(cm.Data[plan.TFPlanName])}
		}
		if err := checkStoredPlan(tf, parts); err != nil {
			return "", err
		}
		pl, err := plan.NewFromConfigMaps(tf.Name, tf.Namespace, uuid, items)
		if err != nil {
			return "", err
		}
		return pl.ToString(), nil
	case "json":
		// The controller stores the JSON plan under "<name>.json". The label
		// without the suffix belongs to the binary planfile Secret, which has
		// the same savedPlan annotation but is not readable.
		k.name = plan.SafeLabelValue(tf.Name + jsonPlanSuffix)
		items, err := p.planSecrets(ctx, tf.Namespace, k)
		if err != nil {
			return "", err
		}
		if len(items) == 0 {
			return "", fmt.Errorf("no plan secrets found for %s", tf.Name)
		}
		parts := make([]storedPart, len(items))
		for i, s := range items {
			parts[i] = storedPart{annotations: s.Annotations, size: len(s.Data[plan.TFPlanName])}
		}
		if err := checkStoredPlan(tf, parts); err != nil {
			return "", err
		}
		pl, err := plan.NewFromSecrets(tf.Name, tf.Namespace, uuid, items)
		if err != nil {
			return "", err
		}
		return pl.ToString(), nil
	default:
		return "", fmt.Errorf("%s has no readable plan, set spec.storeReadablePlan to 'human' or 'json'", tf.Name)
	}
}

// checkPlanID makes sure the stored plan belongs to the pending plan, so the
// user never reviews one plan and approves another.
func checkPlanID(tf *infrav1.Terraform, annotations map[string]string) error {
	if id := annotations[plan.TFPlanSavedAnnotation]; id != tf.Status.Plan.Pending {
		return fmt.Errorf("stored plan of %s is %q but pending is %q, replan to refresh it", tf.Name, id, tf.Status.Plan.Pending)
	}
	return nil
}

// storedPart is what checkStoredPlan needs to know about one stored ConfigMap
// or Secret of a plan.
type storedPart struct {
	annotations map[string]string
	size        int
}

// checkStoredPlan checks that all stored objects of a plan belong to the
// pending plan and that no chunk is missing at the end. The plan package only
// checks for gaps between chunks, and no annotation records how many chunks
// there are. tofu-controller fills every chunk but the last one completely, so
// a full-size last chunk means that the following chunk may not have been
// listed, for example because the controller was rewriting the plan. A plan
// whose size is an exact multiple of the chunk size is refused as well, which
// is the safe direction.
func checkStoredPlan(tf *infrav1.Terraform, parts []storedPart) error {
	chunked, last, lastSize := false, -1, 0
	for _, part := range parts {
		if err := checkPlanID(tf, part.annotations); err != nil {
			return err
		}
		raw := part.annotations[plan.TFPlanChunkAnnotation]
		if raw == "" {
			continue
		}
		chunked = true
		idx, err := strconv.Atoi(raw)
		if err != nil {
			continue // rejected by the plan package when reassembling
		}
		if idx > last {
			last, lastSize = idx, part.size
		}
	}
	if chunked && lastSize >= maxChunkSize {
		return fmt.Errorf("stored plan of %s may be incomplete, chunk %d is full size but no later chunk was found, replan to refresh it", tf.Name, last)
	}
	return nil
}

// legacyConfigMapPlan reads a plan ConfigMap written by older controller
// versions: a single unlabeled ConfigMap named tfplan-<workspace>-<name>.
func (p *Plans) legacyConfigMapPlan(ctx context.Context, tf *infrav1.Terraform) (string, error) {
	name := fmt.Sprintf("tfplan-%s-%s", tf.WorkspaceName(), tf.Name)
	cm := &corev1.ConfigMap{}
	if err := p.kube.Get(ctx, client.ObjectKey{Namespace: tf.Namespace, Name: name}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("no plan configmaps found for %s", tf.Name)
		}
		return "", fmt.Errorf("getting plan configmap %s: %w", name, err)
	}
	if err := checkPlanID(tf, cm.Annotations); err != nil {
		return "", err
	}
	text, ok := cm.Data[plan.TFPlanName]
	if !ok {
		return "", fmt.Errorf("configmap %s missing key %s", name, plan.TFPlanName)
	}
	return text, nil
}

// listOptions selects the plan objects of one stack, or with k == nil the
// plan objects of all stacks.
func listOptions(namespace string, k *planKey) []client.ListOption {
	if k == nil {
		return []client.ListOption{client.InNamespace(namespace), client.HasLabels{plan.TFPlanNameLabel}}
	}
	return []client.ListOption{client.InNamespace(namespace), client.MatchingLabels{
		plan.TFPlanNameLabel:      k.name,
		plan.TFPlanWorkspaceLabel: k.workspace,
	}}
}

func keyOf(labels map[string]string) planKey {
	return planKey{name: labels[plan.TFPlanNameLabel], workspace: labels[plan.TFPlanWorkspaceLabel]}
}

func (p *Plans) planConfigMaps(ctx context.Context, namespace string, k planKey) ([]corev1.ConfigMap, error) {
	if !p.bulk {
		list := &corev1.ConfigMapList{}
		if err := p.kube.List(ctx, list, listOptions(namespace, &k)...); err != nil {
			return nil, fmt.Errorf("listing plan configmaps: %w", err)
		}
		return list.Items, nil
	}
	if p.configMaps == nil {
		list := &corev1.ConfigMapList{}
		if err := p.kube.List(ctx, list, listOptions(namespace, nil)...); err != nil {
			return nil, fmt.Errorf("listing plan configmaps: %w", err)
		}
		p.configMaps = map[planKey][]corev1.ConfigMap{}
		for _, cm := range list.Items {
			p.configMaps[keyOf(cm.Labels)] = append(p.configMaps[keyOf(cm.Labels)], cm)
		}
	}
	return p.configMaps[k], nil
}

func (p *Plans) planSecrets(ctx context.Context, namespace string, k planKey) ([]corev1.Secret, error) {
	if !p.bulk {
		list := &corev1.SecretList{}
		if err := p.kube.List(ctx, list, listOptions(namespace, &k)...); err != nil {
			return nil, fmt.Errorf("listing plan secrets: %w", err)
		}
		return list.Items, nil
	}
	if p.secrets == nil {
		list := &corev1.SecretList{}
		if err := p.kube.List(ctx, list, listOptions(namespace, nil)...); err != nil {
			return nil, fmt.Errorf("listing plan secrets: %w", err)
		}
		p.secrets = map[planKey][]corev1.Secret{}
		for _, s := range list.Items {
			p.secrets[keyOf(s.Labels)] = append(p.secrets[keyOf(s.Labels)], s)
		}
	}
	return p.secrets[k], nil
}

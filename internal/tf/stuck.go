package tf

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultStuckThreshold is how long a plan may stay in Progressing/Unknown
// before it counts as stuck.
const DefaultStuckThreshold = 2 * time.Hour

// Stuck reports whether any condition with reason "Progressing" and status
// "Unknown" exists whose latest lastTransitionTime is older than threshold.
// It returns that latest transition time. Objects without conditions are
// never stuck.
func Stuck(conditions []metav1.Condition, now time.Time, threshold time.Duration) (bool, time.Time) {
	var latest time.Time
	found := false
	for _, c := range conditions {
		if c.Reason != "Progressing" || c.Status != metav1.ConditionUnknown {
			continue
		}
		if c.LastTransitionTime.IsZero() {
			continue
		}
		t := c.LastTransitionTime.Time
		if !found || t.After(latest) {
			latest = t
			found = true
		}
	}
	if !found {
		return false, time.Time{}
	}
	return latest.Before(now.Add(-threshold)), latest
}

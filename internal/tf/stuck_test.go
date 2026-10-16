package tf

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStuck(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	cond := func(reason string, status metav1.ConditionStatus, ago time.Duration) metav1.Condition {
		return metav1.Condition{Type: "Ready", Reason: reason, Status: status, LastTransitionTime: metav1.NewTime(now.Add(-ago))}
	}

	tests := []struct {
		name      string
		conds     []metav1.Condition
		wantStuck bool
		wantSince time.Time
	}{
		{name: "no conditions"},
		{name: "progressing but recent", conds: []metav1.Condition{cond("Progressing", metav1.ConditionUnknown, time.Hour)}, wantSince: now.Add(-time.Hour)},
		{name: "progressing and old", conds: []metav1.Condition{cond("Progressing", metav1.ConditionUnknown, 3*time.Hour)}, wantStuck: true, wantSince: now.Add(-3 * time.Hour)},
		{name: "exactly at threshold", conds: []metav1.Condition{cond("Progressing", metav1.ConditionUnknown, 2*time.Hour)}, wantSince: now.Add(-2 * time.Hour)},
		{name: "old but status true", conds: []metav1.Condition{cond("Progressing", metav1.ConditionTrue, 5*time.Hour)}},
		{name: "old but other reason", conds: []metav1.Condition{cond("PlannedWithChanges", metav1.ConditionUnknown, 5*time.Hour)}},
		{
			name: "latest progressing entry counts",
			conds: []metav1.Condition{
				cond("Progressing", metav1.ConditionUnknown, 5*time.Hour),
				cond("Progressing", metav1.ConditionUnknown, 30*time.Minute),
			},
			wantSince: now.Add(-30 * time.Minute),
		},
		{name: "zero timestamp ignored", conds: []metav1.Condition{{Reason: "Progressing", Status: metav1.ConditionUnknown}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stuck, since := Stuck(tt.conds, now, DefaultStuckThreshold)
			if stuck != tt.wantStuck || !since.Equal(tt.wantSince) {
				t.Errorf("Stuck() = %v, %v; want %v, %v", stuck, since, tt.wantStuck, tt.wantSince)
			}
		})
	}
}

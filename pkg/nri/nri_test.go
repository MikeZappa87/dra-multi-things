package nri

import (
	"testing"
)

func TestTracker_AddAndConsumePending(t *testing.T) {
	tr := NewRDMANetnsTracker()

	tr.AddPending("claim-1", "mlx5_0")
	tr.AddPending("claim-2", "mlx5_1")

	// Consume only claim-1.
	moves := tr.ConsumePendingForClaims([]string{"claim-1", "claim-3"})
	if len(moves) != 1 {
		t.Fatalf("expected 1 move, got %d", len(moves))
	}
	if moves[0].IBDev != "mlx5_0" {
		t.Errorf("expected ibdev mlx5_0, got %s", moves[0].IBDev)
	}

	// claim-1 should be gone, claim-2 still pending.
	moves = tr.ConsumePendingForClaims([]string{"claim-1"})
	if len(moves) != 0 {
		t.Fatalf("expected 0 moves after consumption, got %d", len(moves))
	}

	moves = tr.ConsumePendingForClaims([]string{"claim-2"})
	if len(moves) != 1 {
		t.Fatalf("expected 1 move for claim-2, got %d", len(moves))
	}
}

func TestTracker_RemovePending(t *testing.T) {
	tr := NewRDMANetnsTracker()

	tr.AddPending("claim-1", "mlx5_0")
	tr.RemovePending("claim-1")

	moves := tr.ConsumePendingForClaims([]string{"claim-1"})
	if len(moves) != 0 {
		t.Fatalf("expected 0 moves after RemovePending, got %d", len(moves))
	}
}

func TestTracker_ActiveLifecycle(t *testing.T) {
	tr := NewRDMANetnsTracker()

	tr.MarkActive("claim-1", "pod-uid-1", "mlx5_0")
	tr.MarkActive("claim-2", "pod-uid-1", "mlx5_1")
	tr.MarkActive("claim-3", "pod-uid-2", "mlx5_2")

	// GetActiveForPod should find the right entries.
	pod1 := tr.GetActiveForPod("pod-uid-1")
	if len(pod1) != 2 {
		t.Fatalf("expected 2 active for pod-uid-1, got %d", len(pod1))
	}

	pod2 := tr.GetActiveForPod("pod-uid-2")
	if len(pod2) != 1 {
		t.Fatalf("expected 1 active for pod-uid-2, got %d", len(pod2))
	}

	// RemoveActive by claim.
	m, ok := tr.RemoveActive("claim-1")
	if !ok {
		t.Fatal("expected RemoveActive to find claim-1")
	}
	if m.IBDev != "mlx5_0" || m.PodUID != "pod-uid-1" {
		t.Errorf("unexpected active move: %+v", m)
	}

	// Double remove returns false.
	_, ok = tr.RemoveActive("claim-1")
	if ok {
		t.Fatal("expected RemoveActive to return false for already-removed claim")
	}

	// RemoveActiveForPod removes remaining entries for pod-uid-1.
	removed := tr.RemoveActiveForPod("pod-uid-1")
	if len(removed) != 1 { // claim-2 still there
		t.Fatalf("expected 1 removed for pod-uid-1, got %d", len(removed))
	}
	if removed[0].IBDev != "mlx5_1" {
		t.Errorf("expected mlx5_1, got %s", removed[0].IBDev)
	}

	// pod-uid-2 should still be there.
	pod2 = tr.GetActiveForPod("pod-uid-2")
	if len(pod2) != 1 {
		t.Fatalf("expected 1 active for pod-uid-2, got %d", len(pod2))
	}
}

func TestTracker_String(t *testing.T) {
	tr := NewRDMANetnsTracker()
	tr.AddPending("c1", "mlx5_0")
	tr.MarkActive("c2", "p1", "mlx5_1")

	s := tr.String()
	if s != "RDMANetnsTracker{pending=1, active=1}" {
		t.Errorf("unexpected String(): %s", s)
	}
}

func TestExtractUUIDs(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"empty", "", 0},
		{"no-uuid", "hello world", 0},
		{"single", "uid=aabbccdd-1234-5678-abcd-1234567890ab", 1},
		{"two", "aabbccdd-1234-5678-abcd-1234567890ab,eeff0011-2233-4455-6677-8899aabbccdd", 2},
		{"embedded", `{"uid":"aabbccdd-1234-5678-abcd-1234567890ab"}`, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractUUIDs(tc.input)
			if len(got) != tc.want {
				t.Errorf("extractUUIDs(%q) returned %d uuids, want %d: %v", tc.input, len(got), tc.want, got)
			}
		})
	}
}

func TestIsUUID(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"aabbccdd-1234-5678-abcd-1234567890ab", true},
		{"AABBCCDD-1234-5678-ABCD-1234567890AB", true},
		{"aabbccdd12345678abcd1234567890ab", false},      // no dashes
		{"aabbccdd-1234-5678-abcd-1234567890a", false},   // too short
		{"aabbccdd-1234-5678-abcd-1234567890abc", false}, // too long
		{"ggbbccdd-1234-5678-abcd-1234567890ab", false},  // non-hex
		{"", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := isUUID(tc.input)
			if got != tc.want {
				t.Errorf("isUUID(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestExtractClaimUIDs(t *testing.T) {
	annotations := map[string]string{
		"resource.kubernetes.io/my-container": "aabbccdd-1234-5678-abcd-1234567890ab",
		"other-annotation":                    "eeff0011-2233-4455-6677-8899aabbccdd", // not resource.kubernetes.io prefix
	}

	uids := extractClaimUIDs(annotations)
	if len(uids) != 1 {
		t.Fatalf("expected 1 claim UID, got %d: %v", len(uids), uids)
	}
	if uids[0] != "aabbccdd-1234-5678-abcd-1234567890ab" {
		t.Errorf("unexpected UID: %s", uids[0])
	}
}

func TestExtractClaimUIDs_Dedup(t *testing.T) {
	annotations := map[string]string{
		"resource.kubernetes.io/c1": "aabbccdd-1234-5678-abcd-1234567890ab",
		"resource.kubernetes.io/c2": "aabbccdd-1234-5678-abcd-1234567890ab", // same UID
	}

	uids := extractClaimUIDs(annotations)
	if len(uids) != 1 {
		t.Fatalf("expected 1 deduplicated UID, got %d: %v", len(uids), uids)
	}
}

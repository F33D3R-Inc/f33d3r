package gnosis

import "testing"

// verifiedSet builds a predicate marking the listed PIALs as verified adults.
func verifiedSet(verified ...string) func(string) bool {
	m := map[string]bool{}
	for _, v := range verified {
		m[v] = true
	}
	return func(pid string) bool { return m[pid] }
}

func TestModeForParticipants(t *testing.T) {
	cases := []struct {
		name     string
		pials    []string
		verified []string
		want     string
	}{
		{"both verified adults → sealed", []string{"a", "b"}, []string{"a", "b"}, ModeSealed},
		{"one unverified → plain", []string{"a", "b"}, []string{"a"}, ModePlain},
		{"none verified → plain", []string{"a", "b"}, nil, ModePlain},
		{"solo verified → sealed", []string{"a"}, []string{"a"}, ModeSealed},
		{"group all verified → sealed", []string{"a", "b", "c"}, []string{"a", "b", "c"}, ModeSealed},
		{"group one minor → plain", []string{"a", "b", "c"}, []string{"a", "b"}, ModePlain},
		{"empty participants → plain", nil, nil, ModePlain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := modeFor(tc.pials, verifiedSet(tc.verified...))
			if got != tc.want {
				t.Fatalf("modeFor(%v) = %q, want %q", tc.pials, got, tc.want)
			}
		})
	}
}

func TestGuardAddMember(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		canAdd  bool
		wantErr bool
	}{
		{"add verified to sealed → ok", ModeSealed, true, false},
		{"add unverified to sealed → blocked", ModeSealed, false, true},
		{"add unverified to plain → ok", ModePlain, false, false},
		{"add verified to plain → ok", ModePlain, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := guardAddMember(tc.mode, tc.canAdd)
			if tc.wantErr && err != ErrCannotDowngradeSealed {
				t.Fatalf("expected ErrCannotDowngradeSealed, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

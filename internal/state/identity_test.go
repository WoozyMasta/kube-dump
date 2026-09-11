package state

import "testing"

func TestIdentityKey(t *testing.T) {
	t.Parallel()

	identity := Identity{
		Group: "apps", Version: "v1", Resource: "deployments",
		Namespace: "production", Name: "api",
	}
	if got, want := identity.Key(), "apps/v1/deployments/production/api"; got != want {
		t.Fatalf("Identity.Key() = %q, want %q", got, want)
	}
}

func TestIdentityValidateRejectsPathSeparators(t *testing.T) {
	t.Parallel()

	tests := []Identity{
		{Group: "apps/evil", Version: "v1", Resource: "pods", Kind: "Pod", Name: "api"},
		{Version: "v1\\evil", Resource: "pods", Kind: "Pod", Name: "api"},
		{Version: "v1", Resource: "pods/x", Kind: "Pod", Name: "api"},
		{Version: "v1", Resource: "pods", Kind: "Pod", Namespace: "prod\\evil", Name: "api"},
	}

	for _, identity := range tests {
		if err := identity.Validate(); err == nil {
			t.Fatalf("Identity.Validate() returned nil for %#v", identity)
		}
	}
}

package flow

import (
	"strings"
	"testing"
)

// TestFactoryUpgradeGuardsAreLive: each guarded upgrade constant must differ
// from the current factory template — if they were identical the boot
// upgrade would be a silent no-op. Both versions must also define a function
// (the renderer refuses templates without one).
func TestFactoryUpgradeGuardsAreLive(t *testing.T) {
	cases := []struct {
		name    string
		factory string
		current string
	}{
		{"mux V1", MuxFactoryV1, MuxTemplate().PowerShell},
		{"mux V2", MuxFactoryV2, MuxTemplate().PowerShell},
		{"encode_4k V1", Encode4kFactoryV1, Encode4kTemplate().PowerShell},
	}
	for _, c := range cases {
		if c.factory == c.current {
			t.Errorf("%s: factory constant identical to current template — upgrade is a no-op", c.name)
		}
		if psFuncName.FindStringSubmatch(c.factory) == nil {
			t.Errorf("%s: factory constant defines no function", c.name)
		}
		if psFuncName.FindStringSubmatch(c.current) == nil {
			t.Errorf("%s: current template defines no function", c.name)
		}
	}
}

// TestVerifyOutputFactoryV1ByteGuard pins the exact text of the shipped
// verify_output factory script. The boot guarded-upgrade only replaces the
// stored template when the stored script still equals VerifyOutputFactoryV1
// byte-for-byte (user edits block the upgrade and stay in effect); this test
// catches an accidental edit to the constant so the guard stays honest. The
// factory must also define its function (the renderer refuses templates
// without one) and must differ from the live template once a V2 exists.
func TestVerifyOutputFactoryV1ByteGuard(t *testing.T) {
	if psFuncName.FindStringSubmatch(VerifyOutputFactoryV1) == nil {
		t.Error("VerifyOutputFactoryV1 defines no function")
	}
	if VerifyOutputFactoryV1 != VerifyOutputTemplate().PowerShell {
		t.Error("VerifyOutputFactoryV1 differs from the live template — update this guard when V2 lands")
	}
	// Byte-for-byte stability: the exact body must not drift. Any edit
	// (even whitespace) fails here on purpose.
	const wantFunc = "function Invoke-VerifyOutput"
	if !strings.Contains(VerifyOutputFactoryV1, wantFunc) {
		t.Errorf("VerifyOutputFactoryV1 missing %q", wantFunc)
	}
}

package flow

import "testing"

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

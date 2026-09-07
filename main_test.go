package main

import "testing"

func TestChoosePolicyPrimary(t *testing.T) {
	p := choosePolicy(systemStatus{AvailableMemoryGiB: 13.5, Load1: 0.2}, nil)
	if !p.Allowed || p.Model != primaryModel || p.Mode != "primary" {
		t.Fatalf("unexpected policy: %+v", p)
	}
}

func TestChoosePolicyFallback(t *testing.T) {
	p := choosePolicy(systemStatus{AvailableMemoryGiB: 8.0, Load1: 1.0}, nil)
	if !p.Allowed || p.Model != fallbackModel || p.Mode != "fallback" {
		t.Fatalf("unexpected policy: %+v", p)
	}
}

func TestChoosePolicyBlocked(t *testing.T) {
	p := choosePolicy(systemStatus{AvailableMemoryGiB: 5.0, Load1: 1.0}, nil)
	if p.Allowed || p.Mode != "blocked" {
		t.Fatalf("unexpected policy: %+v", p)
	}
}

func TestLoadedPrimaryStaysPrimary(t *testing.T) {
	p := choosePolicy(systemStatus{AvailableMemoryGiB: 7.5, Load1: 9.0}, []string{primaryModel})
	if !p.Allowed || p.Model != primaryModel {
		t.Fatalf("loaded primary should remain selected: %+v", p)
	}
}

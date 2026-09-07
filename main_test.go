package main

import "testing"

func TestChooseModelPrimary(t *testing.T) {
	got := chooseModel(SystemStatus{AvailableMemoryGiB: 13, Load1: 0.5})
	if !got.Allowed || got.Mode != "primary" || got.Model != primaryModel {
		t.Fatalf("expected primary model, got %+v", got)
	}
}

func TestChooseModelFallbackForMemory(t *testing.T) {
	got := chooseModel(SystemStatus{AvailableMemoryGiB: 8.5, Load1: 1})
	if !got.Allowed || got.Mode != "fallback" || got.Model != fallbackModel {
		t.Fatalf("expected fallback model, got %+v", got)
	}
}

func TestChooseModelFallbackForLoad(t *testing.T) {
	got := chooseModel(SystemStatus{AvailableMemoryGiB: 13, Load1: 9})
	if !got.Allowed || got.Mode != "fallback" || got.Model != fallbackModel {
		t.Fatalf("expected fallback model, got %+v", got)
	}
}

func TestChooseModelBlocked(t *testing.T) {
	got := chooseModel(SystemStatus{AvailableMemoryGiB: 5.5, Load1: 1})
	if got.Allowed || got.Mode != "blocked" {
		t.Fatalf("expected blocked policy, got %+v", got)
	}
}

package main

import (
	"fmt"
	"strings"
	"testing"
)

func Test8BInferenceProfileOptions(t *testing.T) {
	tests := []struct {
		name                  string
		wantThread, wantBatch int
		wantOverrides         bool
	}{
		{name: inferenceProfileBalanced, wantThread: 6, wantBatch: 128, wantOverrides: true},
		{name: inferenceProfileCool, wantThread: 4, wantBatch: 64, wantOverrides: true},
		{name: inferenceProfileUltraCool, wantThread: 3, wantBatch: 32, wantOverrides: true},
		{name: inferenceProfileDefault},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile, valid := parse8BInferenceProfile(test.name)
			if !valid {
				t.Fatalf("profile %q was not accepted", test.name)
			}
			a := &app{eightBProfile: profile}
			options := a.ollamaRequestOptions(primaryModel, map[string]any{"num_ctx": 8192, "num_predict": 320})
			thread, hasThread := options["num_thread"]
			batch, hasBatch := options["num_batch"]
			if hasThread != test.wantOverrides || hasBatch != test.wantOverrides {
				t.Fatalf("profile=%s options=%v", test.name, options)
			}
			if test.wantOverrides && (int(number(thread)) != test.wantThread || int(number(batch)) != test.wantBatch) {
				t.Fatalf("profile=%s num_thread=%v num_batch=%v", test.name, thread, batch)
			}
			if options["num_ctx"] != 8192 || options["num_predict"] != 320 {
				t.Fatalf("existing options changed: %v", options)
			}
		})
	}
}

func TestEmptyAndInvalid8BProfilesUseCoolSafely(t *testing.T) {
	empty, valid := parse8BInferenceProfile("")
	if !valid || empty.Name != inferenceProfileCool || empty.NumThread != 4 || empty.NumBatch != 64 {
		t.Fatalf("empty profile=%+v valid=%v", empty, valid)
	}

	var logged string
	invalid := configured8BInferenceProfile("do-not-log-this-value", func(format string, args ...any) {
		logged = fmt.Sprintf(format, args...)
	})
	if invalid.Name != inferenceProfileCool || invalid.NumThread != 4 || invalid.NumBatch != 64 {
		t.Fatalf("invalid fallback=%+v", invalid)
	}
	if !strings.Contains(logged, "invalid MINIAI_8B_PROFILE") || !strings.Contains(logged, "using cool") ||
		strings.Contains(logged, "do-not-log-this-value") {
		t.Fatalf("unsafe or unclear fallback log: %q", logged)
	}
}

func Test4BRequestsAreUnaffectedBy8BProfile(t *testing.T) {
	ultraCool, _ := parse8BInferenceProfile(inferenceProfileUltraCool)
	a := &app{eightBProfile: ultraCool}
	options := a.ollamaRequestOptions(fallbackModel, map[string]any{"num_ctx": 8192, "num_predict": 320})
	if _, ok := options["num_thread"]; ok {
		t.Fatalf("4B num_thread override added: %v", options)
	}
	if _, ok := options["num_batch"]; ok {
		t.Fatalf("4B num_batch override added: %v", options)
	}
	metadata := a.withInferenceProfileMetadata(fallbackModel, map[string]any{"model": fallbackModel})
	if _, ok := metadata["inference_profile"]; ok {
		t.Fatalf("4B profile metadata added: %v", metadata)
	}
}

func TestUltraCoolInferenceProfileMetadataAndLogging(t *testing.T) {
	ultraCool, valid := parse8BInferenceProfile(inferenceProfileUltraCool)
	if !valid {
		t.Fatal("ultra_cool profile was not accepted")
	}
	a := &app{eightBProfile: ultraCool}
	metadata := a.withInferenceProfileMetadata(primaryModel, map[string]any{"model": primaryModel})
	if metadata["inference_profile"] != inferenceProfileUltraCool || int(number(metadata["num_thread"])) != 3 ||
		int(number(metadata["num_batch"])) != 32 {
		t.Fatalf("metadata=%v", metadata)
	}

	var logged string
	log8BInferenceProfile(ultraCool, func(format string, args ...any) {
		logged = fmt.Sprintf(format, args...)
	})
	for _, want := range []string{"model=" + primaryModel, "inference_profile=ultra_cool", "num_thread=3", "num_batch=32"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("startup profile log %q missing %q", logged, want)
		}
	}
}

package main

import (
	"context"
	"testing"
	"time"
)

type platformPipelineExecutor struct{ calls int }

func (executor *platformPipelineExecutor) Execute(_ context.Context, name string, _ map[string]any) (any, string, error) {
	executor.calls++
	return reactorLabOverview{
		CollectedAt: time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC),
		System: reactorLabSection[reactorLabSystemSnapshot]{Available: true, Data: &reactorLabSystemSnapshot{
			CPU: reactorLabCPUState{UsagePercent: 10}, Memory: reactorLabMemoryState{UsagePercent: 20},
			Disk: reactorLabDiskState{UsagePercent: 30}, Temperature: reactorLabTemperatureState{Celsius: 50},
		}},
		Recovery:      reactorLabSection[reactorLabRecovery]{Available: true},
		Deployments:   reactorLabSection[reactorLabDeploymentList]{Available: true},
		Databases:     reactorLabSection[reactorLabDatabaseList]{Available: true},
		Observability: reactorLabSection[reactorLabObservabilityState]{Available: true},
	}, "safe overview", nil
}

func TestPhase2BPipelineRunsCompleteShadowFlowWithoutModel(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	fake := &platformPipelineExecutor{}
	pipeline := NewPhase2BPipeline(phase0CapabilityRegistry(), fake, func() time.Time { return now })
	result, err := pipeline.Run(t.Context(), "Is the Dell platform healthy now?", ShadowRouterContext{Now: now, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if result.Route.ID != RouteCurrentPlatformHealth || len(result.Plan.Requests) != 1 || len(result.Results) != 1 || len(result.Evidence.Items) != 1 || len(result.PacketJSON) == 0 || fake.calls != 1 {
		t.Fatalf("pipeline result=%+v calls=%d", result, fake.calls)
	}
	if result.Evidence.Confidence.Level != ConfidenceHigh || len(result.PacketJSON) > evidencePacketHardLimitBytes {
		t.Fatalf("confidence/packet=%+v/%d", result.Evidence.Confidence, len(result.PacketJSON))
	}
}

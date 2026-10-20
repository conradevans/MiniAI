package main

import (
	"errors"
	"fmt"
)

type Phase2BPipelineStage string

const (
	Phase2BStageEvidenceExecution Phase2BPipelineStage = "evidence_execution"
	Phase2BStageEvidenceReduction Phase2BPipelineStage = "evidence_reduction"
	Phase2BStagePacketBuild       Phase2BPipelineStage = "packet_build"
	Phase2BStagePacketMarshal     Phase2BPipelineStage = "packet_marshal"
)

type Phase2BPipelineErrorCategory string

const (
	Phase2BPipelineErrorExecutionFailed Phase2BPipelineErrorCategory = "evidence_execution_failed"
	Phase2BPipelineErrorReductionFailed Phase2BPipelineErrorCategory = "evidence_reduction_failed"
	Phase2BPipelineErrorPacketBuild     Phase2BPipelineErrorCategory = "packet_build_failed"
	Phase2BPipelineErrorPacketMarshal   Phase2BPipelineErrorCategory = "packet_marshal_failed"
	Phase2BPipelineErrorPacketTooLarge  Phase2BPipelineErrorCategory = "packet_too_large"
)

// Phase2BPipelineError carries only safe stage metadata. The wrapped error is
// retained for errors.Is/errors.As inside the process and is never serialized
// directly to a client.
type Phase2BPipelineError struct {
	Stage        Phase2BPipelineStage
	Category     Phase2BPipelineErrorCategory
	PacketBytes  int
	PacketTarget int
	PacketLimit  int
	Err          error
}

func (err Phase2BPipelineError) Error() string {
	return fmt.Sprintf("phase2b %s failed (%s)", err.Stage, err.Category)
}

func (err Phase2BPipelineError) Unwrap() error { return err.Err }

func newPhase2BPipelineError(stage Phase2BPipelineStage, category Phase2BPipelineErrorCategory, err error) error {
	out := Phase2BPipelineError{Stage: stage, Category: category, Err: err}
	if stage == Phase2BStagePacketBuild || stage == Phase2BStagePacketMarshal {
		out.PacketTarget = evidencePacketTargetBytes
		out.PacketLimit = evidencePacketHardLimitBytes
		var sizeErr EvidencePacketSizeError
		if errors.As(err, &sizeErr) {
			out.Category = Phase2BPipelineErrorPacketTooLarge
			out.PacketBytes = sizeErr.Bytes
			out.PacketLimit = sizeErr.Limit
		}
	}
	return out
}

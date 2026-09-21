package main

// evidenceKind separates what MiniAI directly observed from deterministic
// derivations and model-generated inferences. Phase 0 defines this contract but
// does not persist an evidence ledger.
type evidenceKind string

const (
	evidenceObserved  evidenceKind = "observed"
	evidenceDerived   evidenceKind = "derived"
	evidenceInference evidenceKind = "inference"
)

type evidenceSourceReference struct {
	Capability string
	Resource   string
}

type evidenceRecord struct {
	Kind      evidenceKind
	Statement string
	Source    evidenceSourceReference
}

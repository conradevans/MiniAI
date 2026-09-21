package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRepositoryEvidenceCarriesInternalObservedSourceContract(t *testing.T) {
	search := repoSearchResponse{Hits: []repoSearchHit{{Path: "backend/routes/schedule.go", Line: 1, Text: "handler"}}}
	file := repoFileResponse{Path: "backend/routes/schedule.go", Content: "handler"}
	evidence, ok := compactRepositoryLocationEvidence(search, file)
	if !ok {
		t.Fatal("expected repository evidence")
	}
	if evidence.Contract.Kind != evidenceObserved || evidence.Contract.Source.Capability != "read_repository_file" ||
		evidence.Contract.Source.Resource != file.Path {
		t.Fatalf("contract=%+v", evidence.Contract)
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Contract") || strings.Contains(string(encoded), "observed") {
		t.Fatalf("internal evidence contract leaked into current JSON: %s", encoded)
	}
}

func TestEvidenceContractDistinguishesFactAndInferenceKinds(t *testing.T) {
	if evidenceObserved == evidenceDerived || evidenceObserved == evidenceInference || evidenceDerived == evidenceInference {
		t.Fatal("evidence kinds must remain distinct")
	}
}

package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

func (a *app) finalizeRequestHistory(ctx context.Context, entry HistoryEntry, capture *sseCaptureWriter) {
	if a == nil || a.store == nil || capture == nil || entry.ID == "" {
		return
	}
	status := historyStatusForCapture(ctx, capture)
	metadata := capture.metadata
	if status == HistoryStatusCanceled {
		metadata.ErrorCategory = "client_canceled"
	} else if status == HistoryStatusFailed && metadata.ErrorCategory == "" {
		if capture.statusCode >= 400 {
			metadata.ErrorCategory = "http_" + strconv.Itoa(capture.statusCode)
		} else {
			metadata.ErrorCategory = "execution_incomplete"
		}
	}
	reads := capture.capturedHistoryReads(status)
	resultText := capture.answer.String()
	persistAssistant := capture.done && strings.TrimSpace(resultText) != "" && status != HistoryStatusCanceled
	if status == HistoryStatusCanceled {
		resultText = ""
	}
	input := historyFinalizeInput{
		Kind:              historyKindForCapture(metadata, reads),
		Status:            status,
		ResultText:        resultText,
		AssistantText:     capture.answer.String(),
		PersistAssistant:  persistAssistant,
		AssistantEvidence: capture.evidence,
		CompletedAt:       time.Now().UTC(),
		Metadata:          metadata,
		EvidenceReads:     reads,
		Targets:           deriveHistoryTargets(metadata, reads),
	}
	if _, err := a.store.finalizeHistoryEntry(entry.ID, input); err != nil {
		log.Printf("finalize MiniAI execution history %s: %s", entry.ID, safeHistoryPersistenceError(err))
	}
}

func historyStatusForCapture(ctx context.Context, capture *sseCaptureWriter) HistoryStatus {
	if ctx != nil && ctx.Err() != nil {
		return HistoryStatusCanceled
	}
	if capture.done {
		if historyErrorCategoryIsFailure(capture.metadata.ErrorCategory) {
			return HistoryStatusFailed
		}
		return HistoryStatusSucceeded
	}
	return HistoryStatusFailed
}

func historyErrorCategoryIsFailure(category string) bool {
	switch strings.TrimSpace(category) {
	case "", "unsupported_local_evidence":
		return false
	default:
		return true
	}
}

func safeHistoryPersistenceError(err error) string {
	if err == nil {
		return ""
	}
	if err == errHistoryNotFound || err == errHistoryAlreadyFinalized {
		return err.Error()
	}
	return fmt.Sprintf("persistence failure (%T)", err)
}

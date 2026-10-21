package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	adminActionRequestMaxBytes = 16 << 10
	adminActionCancelMaxBytes  = 1 << 10
)

type adminActionCreateRequest struct {
	Kind      ActionKind      `json:"kind"`
	Target    ActionTarget    `json:"target"`
	Arguments json.RawMessage `json:"arguments"`
}

type adminActionConfirmRequest struct {
	Confirm            bool `json:"confirm"`
	AcknowledgeWarning bool `json:"acknowledge_warning"`
}

type adminActionTargetResponse struct {
	Type ActionTargetType `json:"type"`
	ID   string           `json:"id"`
}

type adminActionResponse struct {
	ActionID             ActionID                  `json:"action_id"`
	Kind                 ActionKind                `json:"kind"`
	Origin               ActionOrigin              `json:"origin"`
	SafetyClass          ActionSafetyClass         `json:"safety_class"`
	Target               adminActionTargetResponse `json:"target"`
	Arguments            any                       `json:"arguments"`
	State                ActionState               `json:"state"`
	ConfirmationRequired bool                      `json:"confirmation_required"`
	WarningRequired      bool                      `json:"warning_required"`
	SupportsExecution    bool                      `json:"supports_execution"`
	CreatedAt            time.Time                 `json:"created_at"`
	UpdatedAt            time.Time                 `json:"updated_at"`
	HistoryEntryID       string                    `json:"history_id,omitempty"`
}

type adminActionEventResponse struct {
	EventID   string          `json:"event_id"`
	EventType ActionEventType `json:"event_type"`
	FromState *ActionState    `json:"from_state"`
	ToState   ActionState     `json:"to_state"`
	Ordinal   int             `json:"ordinal"`
	Timestamp time.Time       `json:"timestamp"`
}

type adminActionEventsResponse struct {
	ActionID ActionID                   `json:"action_id"`
	Events   []adminActionEventResponse `json:"events"`
}

func (a *app) registerAdminActionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/admin/actions", a.handleCreateAdminAction)
	mux.HandleFunc("GET /api/admin/actions/{action_id}", a.handleGetAdminAction)
	mux.HandleFunc("GET /api/admin/actions/{action_id}/events", a.handleGetAdminActionEvents)
	mux.HandleFunc("POST /api/admin/actions/{action_id}/confirm", a.handleConfirmAdminAction)
	mux.HandleFunc("POST /api/admin/actions/{action_id}/cancel", a.handleCancelAdminAction)
}

func (a *app) handleCreateAdminAction(w http.ResponseWriter, r *http.Request) {
	var request adminActionCreateRequest
	if err := decodeStrictAdminActionJSON(w, r, &request, adminActionRequestMaxBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid action request")
		return
	}

	arguments, err := decodeAdminActionArguments(request.Kind, request.Arguments)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid action request")
		return
	}
	record, err := a.store.createActionProposal(actionProposalCreateInput{
		Kind:      request.Kind,
		Origin:    ActionOriginDirectUser,
		Target:    request.Target,
		Arguments: arguments,
	})
	if err != nil {
		if invalidAdminActionCreateInput(err) {
			writeError(w, http.StatusBadRequest, "invalid action request")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not create action")
		return
	}

	response, err := newAdminActionResponse(record)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read action")
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (a *app) handleGetAdminAction(w http.ResponseWriter, r *http.Request) {
	record, err := a.store.getActionProposal(ActionID(r.PathValue("action_id")))
	if err != nil {
		writeAdminActionLoadError(w, err)
		return
	}
	response, err := newAdminActionResponse(record)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read action")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) handleGetAdminActionEvents(w http.ResponseWriter, r *http.Request) {
	id := ActionID(r.PathValue("action_id"))
	events, err := a.store.listActionEvents(id)
	if err != nil {
		writeAdminActionLoadError(w, err)
		return
	}
	response := adminActionEventsResponse{
		ActionID: id,
		Events:   make([]adminActionEventResponse, 0, len(events)),
	}
	for _, event := range events {
		response.Events = append(response.Events, adminActionEventResponse{
			EventID:   event.ID,
			EventType: event.Type,
			FromState: event.FromState,
			ToState:   event.ToState,
			Ordinal:   event.Ordinal,
			Timestamp: event.CreatedAt.UTC(),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) handleConfirmAdminAction(w http.ResponseWriter, r *http.Request) {
	var request adminActionConfirmRequest
	if err := decodeStrictAdminActionJSON(w, r, &request, adminActionRequestMaxBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid confirmation request")
		return
	}
	if !request.Confirm {
		writeError(w, http.StatusBadRequest, "explicit confirmation is required")
		return
	}

	id := ActionID(r.PathValue("action_id"))
	current, err := a.store.getActionProposal(id)
	if err != nil {
		writeAdminActionLoadError(w, err)
		return
	}
	if current.Proposal.State() != ActionStateAwaitingConfirmation {
		writeError(w, http.StatusConflict, "action cannot be confirmed in its current state")
		return
	}
	if current.Proposal.WarningRequired() && !request.AcknowledgeWarning {
		writeError(w, http.StatusBadRequest, "warning acknowledgement is required")
		return
	}

	record, err := a.store.confirmAction(id)
	if err != nil {
		writeAdminActionTransitionError(w, err, "confirmed")
		return
	}
	response, err := newAdminActionResponse(record)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read action")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) handleCancelAdminAction(w http.ResponseWriter, r *http.Request) {
	if err := requireEmptyAdminActionBody(w, r); err != nil {
		writeError(w, http.StatusBadRequest, "cancellation request body must be empty")
		return
	}

	record, err := a.store.cancelAction(ActionID(r.PathValue("action_id")))
	if err != nil {
		writeAdminActionTransitionError(w, err, "cancelled")
		return
	}
	response, err := newAdminActionResponse(record)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read action")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func decodeStrictAdminActionJSON(
	w http.ResponseWriter,
	r *http.Request,
	destination any,
	maxBytes int64,
) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func decodeAdminActionArguments(
	kind ActionKind,
	raw json.RawMessage,
) (ActionArguments, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, errInvalidActionArguments
	}

	var destination any
	switch kind {
	case ActionKindRestartApplication:
		destination = &RestartApplicationArguments{}
	case ActionKindRollbackDeployment:
		destination = &RollbackDeploymentArguments{}
	case ActionKindDeployRepository:
		destination = &DeployRepositoryArguments{}
	default:
		return nil, errUnknownActionKind
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return nil, errInvalidActionArguments
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, errInvalidActionArguments
	}

	switch arguments := destination.(type) {
	case *RestartApplicationArguments:
		return *arguments, nil
	case *RollbackDeploymentArguments:
		return *arguments, nil
	case *DeployRepositoryArguments:
		return *arguments, nil
	default:
		return nil, errInvalidActionArguments
	}
}

func requireEmptyAdminActionBody(w http.ResponseWriter, r *http.Request) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, adminActionCancelMaxBytes))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) != 0 {
		return errors.New("unexpected cancellation request body")
	}
	return nil
}

func invalidAdminActionCreateInput(err error) bool {
	return errors.Is(err, errUnknownActionKind) ||
		errors.Is(err, errInvalidActionTarget) ||
		errors.Is(err, errInvalidActionArguments)
}

func writeAdminActionLoadError(w http.ResponseWriter, err error) {
	if errors.Is(err, errActionNotFound) {
		writeError(w, http.StatusNotFound, "action not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "could not read action")
}

func writeAdminActionTransitionError(w http.ResponseWriter, err error, operation string) {
	if errors.Is(err, errActionNotFound) {
		writeError(w, http.StatusNotFound, "action not found")
		return
	}
	if errors.Is(err, errActionConfirmationNotAllowed) ||
		errors.Is(err, errActionCancellationNotAllowed) ||
		errors.Is(err, errActionPersistenceConflict) {

		writeError(w, http.StatusConflict, "action could not be "+operation)
		return
	}
	writeError(w, http.StatusInternalServerError, "could not update action")
}

func newAdminActionResponse(record storedActionProposal) (adminActionResponse, error) {
	proposal := record.Proposal
	definition, ok := phase4ActionCatalog().lookup(proposal.Kind())
	if !ok {
		return adminActionResponse{}, errUnknownActionKind
	}
	decision, err := phase4ActionCatalog().evaluate(proposal)
	if err != nil {
		return adminActionResponse{}, err
	}
	arguments, err := safeAdminActionArguments(proposal.Kind(), proposal.Arguments())
	if err != nil {
		return adminActionResponse{}, err
	}
	target := proposal.Target()
	return adminActionResponse{
		ActionID:             proposal.ID(),
		Kind:                 proposal.Kind(),
		Origin:               proposal.Origin(),
		SafetyClass:          definition.SafetyClass,
		Target:               adminActionTargetResponse{Type: target.Type, ID: target.ID},
		Arguments:            arguments,
		State:                proposal.State(),
		ConfirmationRequired: decision.ConfirmationRequired,
		WarningRequired:      decision.WarningRequired,
		SupportsExecution:    definition.SupportsExecution,
		CreatedAt:            proposal.CreatedAt().UTC(),
		UpdatedAt:            record.UpdatedAt.UTC(),
		HistoryEntryID:       record.HistoryEntryID,
	}, nil
}

func safeAdminActionArguments(kind ActionKind, value ActionArguments) (any, error) {
	switch kind {
	case ActionKindRestartApplication:
		arguments, ok := value.(RestartApplicationArguments)
		if !ok {
			return nil, errInvalidActionArguments
		}
		return arguments, nil
	case ActionKindRollbackDeployment:
		arguments, ok := value.(RollbackDeploymentArguments)
		if !ok {
			return nil, errInvalidActionArguments
		}
		return arguments, nil
	case ActionKindDeployRepository:
		arguments, ok := value.(DeployRepositoryArguments)
		if !ok {
			return nil, errInvalidActionArguments
		}
		return arguments, nil
	default:
		return nil, errUnknownActionKind
	}
}

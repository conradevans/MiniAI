package main

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestPhase4ActionCatalogIsClosedAndExecutionIsRestartOnly(t *testing.T) {
	catalog := phase4ActionCatalog()
	if catalogType := reflect.TypeOf(catalog); catalogType.NumField() != 0 {
		t.Fatalf("action catalog exposes mutable fields: %v", catalogType)
	}

	want := []struct {
		kind              ActionKind
		safetyClass       ActionSafetyClass
		targetType        ActionTargetType
		supportsExecution bool
	}{
		{
			kind: ActionKindRestartApplication, safetyClass: ActionSafetyRecoverable,
			targetType: ActionTargetApplication, supportsExecution: true,
		},
		{
			kind: ActionKindRollbackDeployment, safetyClass: ActionSafetyIrreversible,
			targetType: ActionTargetApplication,
		},
		{
			kind: ActionKindDeployRepository, safetyClass: ActionSafetyCompensatable,
			targetType: ActionTargetRepository,
		},
	}
	definitions := catalog.definitionsCopy()
	if len(definitions) != len(want) {
		t.Fatalf("definitions=%d want %d", len(definitions), len(want))
	}
	for index, expected := range want {
		definition := definitions[index]
		if definition.Kind != expected.kind ||
			definition.SafetyClass != expected.safetyClass ||
			definition.TargetType != expected.targetType ||
			definition.SupportsExecution != expected.supportsExecution {

			t.Fatalf("definition %d=%+v want=%+v", index, definition, expected)
		}
	}

	definitions[0] = ActionDefinition{
		Kind:              "invented_action",
		SafetyClass:       ActionSafetyIrreversible,
		TargetType:        ActionTargetRepository,
		SupportsExecution: true,
	}
	definition, ok := catalog.lookup(ActionKindRestartApplication)
	if !ok ||
		definition.Kind != ActionKindRestartApplication ||
		definition.SafetyClass != ActionSafetyRecoverable ||
		definition.TargetType != ActionTargetApplication ||
		!definition.SupportsExecution {

		t.Fatalf("returned copy modified authoritative catalog: %+v", definition)
	}
}

func TestPhase4ActionCatalogRejectsUnknownAndArbitraryCommandKinds(t *testing.T) {
	catalog := phase4ActionCatalog()
	for _, kind := range []ActionKind{
		"run_command",
		"shell",
		"exec",
		"arbitrary_http",
	} {
		if _, ok := catalog.lookup(kind); ok {
			t.Fatalf("unsafe generic action %q is registered", kind)
		}
	}
	_, err := catalog.propose(ActionProposalInput{
		ID:        "action_0123456789abcdef0123456789abcdef",
		Kind:      ActionKind("invented_action"),
		Origin:    ActionOriginDirectUser,
		Target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
		Arguments: RestartApplicationArguments{},
		CreatedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
	})
	if !errors.Is(err, errUnknownActionKind) {
		t.Fatalf("unknown kind error=%v", err)
	}
}

func TestPhase4RollbackCatalogClassificationAndPolicy(t *testing.T) {
	catalog := phase4ActionCatalog()
	for _, origin := range []ActionOrigin{
		ActionOriginDirectUser,
		ActionOriginAssistantSuggested,
	} {
		t.Run(string(origin), func(t *testing.T) {
			proposal, err := catalog.propose(ActionProposalInput{
				ID:        "action_0123456789abcdef0123456789abcdef",
				Kind:      ActionKindRollbackDeployment,
				Origin:    origin,
				Target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
				Arguments: RollbackDeploymentArguments{},
				CreatedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
			})
			if err != nil {
				t.Fatal(err)
			}
			if proposal.SafetyClass() != ActionSafetyIrreversible ||
				!proposal.ConfirmationRequired() ||
				!proposal.WarningRequired() ||
				proposal.State() != ActionStateAwaitingConfirmation ||
				proposal.PolicyReason() != ActionPolicyIrreversibleWarning {

				t.Fatalf(
					"rollback class=%q state=%q origin=%q",
					proposal.SafetyClass(),
					proposal.State(),
					proposal.Origin(),
				)
			}
			decision, err := catalog.evaluate(proposal)
			if err != nil || !decision.ConfirmationRequired ||
				!decision.WarningRequired ||
				decision.InitialState != ActionStateAwaitingConfirmation {

				t.Fatalf("rollback evaluation=%+v err=%v", decision, err)
			}
		})
	}
}

func TestPhase4ActionProposalUsesCatalogSafetyAndPolicy(t *testing.T) {
	createdAt := time.Date(
		2026, 10, 20, 12, 30, 0, 123456789,
		time.FixedZone("test", -5*60*60),
	)
	proposal, err := phase4ActionCatalog().propose(ActionProposalInput{
		ID:        "action_0123456789abcdef0123456789abcdef",
		Kind:      ActionKindRestartApplication,
		Origin:    ActionOriginDirectUser,
		Target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
		Arguments: RestartApplicationArguments{},
		CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.ID() != "action_0123456789abcdef0123456789abcdef" ||
		proposal.Kind() != ActionKindRestartApplication ||
		proposal.Origin() != ActionOriginDirectUser ||
		proposal.SafetyClass() != ActionSafetyRecoverable ||
		proposal.Target() != (ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"}) ||
		proposal.State() != ActionStateAuthorized ||
		proposal.ConfirmationRequired() ||
		proposal.WarningRequired() ||
		proposal.PolicyReason() != ActionPolicyDirectUserNoConfirmation ||
		!proposal.CreatedAt().Equal(createdAt) ||
		proposal.CreatedAt().Location() != time.UTC {

		t.Fatalf("unexpected proposal getters")
	}
	if _, ok := proposal.Arguments().(RestartApplicationArguments); !ok {
		t.Fatalf("arguments type=%T", proposal.Arguments())
	}
}

func TestPhase4ActionProposalHasOpaqueAuthorityBoundary(t *testing.T) {
	proposalType := reflect.TypeOf(ActionProposal{})
	if proposalType.PkgPath() != "miniai/internal/actiondomain" {
		t.Fatalf("proposal authority package=%q", proposalType.PkgPath())
	}
	for index := 0; index < proposalType.NumField(); index++ {
		field := proposalType.Field(index)
		if field.IsExported() {
			t.Fatalf("ActionProposal exposes mutable field %s", field.Name)
		}
	}
	for _, forbidden := range []string{
		"SetOrigin",
		"SetState",
		"SetSafetyClass",
		"SetSupportsExecution",
	} {
		if _, ok := proposalType.MethodByName(forbidden); ok {
			t.Fatalf("ActionProposal exposes forbidden method %s", forbidden)
		}
	}

	catalog := phase4ActionCatalog()
	var zero ActionProposal
	if _, err := catalog.evaluate(zero); !errors.Is(err, errInvalidActionProposal) {
		t.Fatalf("zero proposal evaluation error=%v", err)
	}
	if _, err := transitionActionProposal(
		zero,
		ActionStateAuthorized,
	); !errors.Is(err, errInvalidActionProposal) {
		t.Fatalf("zero proposal transition error=%v", err)
	}

	proposal, err := catalog.propose(ActionProposalInput{
		ID:        "action_abcdef0123456789abcdef0123456789",
		Kind:      ActionKindRestartApplication,
		Origin:    ActionOriginAssistantSuggested,
		Target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
		Arguments: RestartApplicationArguments{},
		CreatedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	copied := proposal
	if copied.Origin() != ActionOriginAssistantSuggested ||
		copied.State() != ActionStateAwaitingConfirmation {

		t.Fatalf("copied proposal origin=%q state=%q", copied.Origin(), copied.State())
	}
	if _, err := transitionActionProposal(
		copied,
		ActionStateExecuting,
	); !errors.Is(err, errInvalidActionStateTransition) {
		t.Fatalf("awaiting -> executing error=%v", err)
	}
	if proposal.State() != ActionStateAwaitingConfirmation ||
		copied.State() != ActionStateAwaitingConfirmation {

		t.Fatalf("failed transition mutated copy or original")
	}

	authorized, err := transitionActionProposal(copied, ActionStateAuthorized)
	if err != nil {
		t.Fatal(err)
	}
	if authorized.Origin() != ActionOriginAssistantSuggested ||
		authorized.State() != ActionStateAuthorized ||
		proposal.State() != ActionStateAwaitingConfirmation {

		t.Fatalf(
			"authorized origin=%q state=%q original=%q",
			authorized.Origin(),
			authorized.State(),
			proposal.State(),
		)
	}
}

func TestPhase4ActionProposalRejectsInvalidIdentityOriginTargetsAndArguments(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	validRestart := ActionProposalInput{
		ID:        "action_0123456789abcdef0123456789abcdef",
		Kind:      ActionKindRestartApplication,
		Origin:    ActionOriginDirectUser,
		Target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
		Arguments: RestartApplicationArguments{},
		CreatedAt: now,
	}
	validDeploy := ActionProposalInput{
		ID:     "action_0123456789abcdef0123456789abcdef",
		Kind:   ActionKindDeployRepository,
		Origin: ActionOriginDirectUser,
		Target: ActionTarget{Type: ActionTargetRepository, ID: "owner/repository"},
		Arguments: DeployRepositoryArguments{
			Provider: RepositoryProviderGitHub, ContainerPort: 3000,
			HealthPath: "/health",
		},
		CreatedAt: now,
	}
	validDeployProposal, err := phase4ActionCatalog().propose(validDeploy)
	if err != nil {
		t.Fatalf("valid typed deploy proposal: %v", err)
	}
	if validDeployProposal.SafetyClass() != ActionSafetyCompensatable ||
		!validDeployProposal.ConfirmationRequired() ||
		validDeployProposal.WarningRequired() ||
		validDeployProposal.State() != ActionStateAwaitingConfirmation {

		t.Fatalf("unexpected valid typed deploy proposal")
	}
	tests := []struct {
		name  string
		input ActionProposalInput
		want  error
	}{
		{
			name: "invalid id",
			input: func() ActionProposalInput {
				value := validRestart
				value.ID = "user-supplied"
				return value
			}(),
			want: errInvalidActionID,
		},
		{
			name: "zero timestamp",
			input: func() ActionProposalInput {
				value := validRestart
				value.CreatedAt = time.Time{}
				return value
			}(),
			want: errInvalidActionCreatedAt,
		},
		{
			name: "invalid origin",
			input: func() ActionProposalInput {
				value := validRestart
				value.Origin = "inferred_from_prose"
				return value
			}(),
			want: errInvalidActionOrigin,
		},
		{
			name: "wrong target type",
			input: func() ActionProposalInput {
				value := validRestart
				value.Target.Type = ActionTargetRepository
				return value
			}(),
			want: errInvalidActionTarget,
		},
		{
			name: "malformed application",
			input: func() ActionProposalInput {
				value := validRestart
				value.Target.ID = "My Scheduler"
				return value
			}(),
			want: errInvalidActionTarget,
		},
		{
			name: "wrong typed arguments",
			input: func() ActionProposalInput {
				value := validRestart
				value.Arguments = RollbackDeploymentArguments{}
				return value
			}(),
			want: errInvalidActionArguments,
		},
		{
			name: "missing arguments",
			input: func() ActionProposalInput {
				value := validRestart
				value.Arguments = nil
				return value
			}(),
			want: errInvalidActionArguments,
		},
		{
			name: "malformed repository",
			input: func() ActionProposalInput {
				value := validDeploy
				value.Target.ID = "https://github.com/owner/repository"
				return value
			}(),
			want: errInvalidActionTarget,
		},
		{
			name: "unsupported provider",
			input: func() ActionProposalInput {
				value := validDeploy
				value.Arguments = DeployRepositoryArguments{Provider: "arbitrary"}
				return value
			}(),
			want: errInvalidActionArguments,
		},
		{
			name: "invalid port",
			input: func() ActionProposalInput {
				value := validDeploy
				value.Arguments = DeployRepositoryArguments{
					Provider:      RepositoryProviderGitHub,
					ContainerPort: 65536,
				}
				return value
			}(),
			want: errInvalidActionArguments,
		},
		{
			name: "invalid health path",
			input: func() ActionProposalInput {
				value := validDeploy
				value.Arguments = DeployRepositoryArguments{
					Provider:   RepositoryProviderGitHub,
					HealthPath: "health",
				}
				return value
			}(),
			want: errInvalidActionArguments,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := phase4ActionCatalog().propose(test.input); !errors.Is(err, test.want) {
				t.Fatalf("error=%v want %v", err, test.want)
			}
		})
	}
}

func TestPhase4ActionKindArgumentCoupling(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	deployArguments := DeployRepositoryArguments{
		Provider: RepositoryProviderGitHub, ContainerPort: 3000, HealthPath: "/health",
	}
	tests := []struct {
		name      string
		kind      ActionKind
		target    ActionTarget
		arguments ActionArguments
	}{
		{
			name: "restart with rollback", kind: ActionKindRestartApplication,
			target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
			arguments: RollbackDeploymentArguments{},
		},
		{
			name: "restart with deploy", kind: ActionKindRestartApplication,
			target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
			arguments: deployArguments,
		},
		{
			name: "rollback with restart", kind: ActionKindRollbackDeployment,
			target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
			arguments: RestartApplicationArguments{},
		},
		{
			name: "rollback with deploy", kind: ActionKindRollbackDeployment,
			target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
			arguments: deployArguments,
		},
		{
			name: "deploy with restart", kind: ActionKindDeployRepository,
			target:    ActionTarget{Type: ActionTargetRepository, ID: "owner/repository"},
			arguments: RestartApplicationArguments{},
		},
		{
			name: "deploy with rollback", kind: ActionKindDeployRepository,
			target:    ActionTarget{Type: ActionTargetRepository, ID: "owner/repository"},
			arguments: RollbackDeploymentArguments{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := phase4ActionCatalog().propose(ActionProposalInput{
				ID:        "action_0123456789abcdef0123456789abcdef",
				Kind:      test.kind,
				Origin:    ActionOriginDirectUser,
				Target:    test.target,
				Arguments: test.arguments,
				CreatedAt: now,
			})
			if !errors.Is(err, errInvalidActionArguments) {
				t.Fatalf("cross-kind arguments error=%v", err)
			}
		})
	}
}

func phase4DeployProposalInput(
	repository string,
	containerPort int,
	healthPath string,
) ActionProposalInput {
	return ActionProposalInput{
		ID:     "action_0123456789abcdef0123456789abcdef",
		Kind:   ActionKindDeployRepository,
		Origin: ActionOriginDirectUser,
		Target: ActionTarget{
			Type: ActionTargetRepository,
			ID:   repository,
		},
		Arguments: DeployRepositoryArguments{
			Provider:      RepositoryProviderGitHub,
			ContainerPort: containerPort,
			HealthPath:    healthPath,
		},
		CreatedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
	}
}

func TestPhase4DeployRepositoryPortValidation(t *testing.T) {
	tests := []struct {
		name    string
		port    int
		wantErr bool
	}{
		{name: "zero", port: 0, wantErr: true},
		{name: "negative", port: -1, wantErr: true},
		{name: "minimum", port: 1},
		{name: "maximum", port: 65535},
		{name: "above maximum", port: 65536, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := phase4ActionCatalog().propose(
				phase4DeployProposalInput("owner/repository", test.port, "/health"),
			)
			if test.wantErr && !errors.Is(err, errInvalidActionArguments) {
				t.Fatalf("port %d error=%v", test.port, err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("port %d: %v", test.port, err)
			}
		})
	}
}

func TestPhase4DeployRepositoryHealthPathValidation(t *testing.T) {
	tests := []struct {
		path    string
		wantErr bool
	}{
		{path: "/"},
		{path: "/health"},
		{path: "/api/health"},
		{path: "/health?ready=1"},
		{path: "", wantErr: true},
		{path: "//attacker.example/health", wantErr: true},
		{path: "http://attacker.example/health", wantErr: true},
		{path: "https://attacker.example/health", wantErr: true},
		{path: "attacker.example/health", wantErr: true},
		{path: "javascript:alert(1)", wantErr: true},
		{path: "/health\r\nX-Injected: true", wantErr: true},
		{path: "/probe/http://attacker.example", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			_, err := phase4ActionCatalog().propose(
				phase4DeployProposalInput("owner/repository", 3000, test.path),
			)
			if test.wantErr && !errors.Is(err, errInvalidActionArguments) {
				t.Fatalf("health path %q error=%v", test.path, err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("health path %q: %v", test.path, err)
			}
		})
	}
}

func TestPhase4DeployRepositoryGitHubIdentityValidation(t *testing.T) {
	tests := []struct {
		repository string
		wantErr    bool
	}{
		{repository: "owner/repository"},
		{repository: "OpenAI/ReactorLab"},
		{repository: "owner/.github"},
		{repository: "../repository", wantErr: true},
		{repository: "owner/..", wantErr: true},
		{repository: "./repository", wantErr: true},
		{repository: "owner/.", wantErr: true},
		{repository: "/repository", wantErr: true},
		{repository: "owner/", wantErr: true},
		{repository: "owner/repo/extra", wantErr: true},
		{repository: "owner//repo", wantErr: true},
		{repository: "https://github.com/owner/repository", wantErr: true},
		{repository: "/tmp/repository", wantErr: true},
		{repository: "C:/repository", wantErr: true},
		{repository: "owner/repository?ref=main", wantErr: true},
		{repository: "owner/repository#fragment", wantErr: true},
		{repository: "user:token/repository", wantErr: true},
		{repository: "_owner/repository", wantErr: true},
		{repository: "owner-/repository", wantErr: true},
		{repository: "owner--name/repository", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.repository, func(t *testing.T) {
			_, err := phase4ActionCatalog().propose(
				phase4DeployProposalInput(test.repository, 3000, "/health"),
			)
			if test.wantErr && !errors.Is(err, errInvalidActionTarget) {
				t.Fatalf("repository %q error=%v", test.repository, err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("repository %q: %v", test.repository, err)
			}
		})
	}
}

func TestPhase4ActionLifecycleAcceptsOnlyDefinedTransitions(t *testing.T) {
	valid := [][2]ActionState{
		{ActionStateProposed, ActionStateAwaitingConfirmation},
		{ActionStateProposed, ActionStateAuthorized},
		{ActionStateAwaitingConfirmation, ActionStateAuthorized},
		{ActionStateAwaitingConfirmation, ActionStateCancelled},
		{ActionStateAuthorized, ActionStateExecuting},
		{ActionStateExecuting, ActionStateSucceeded},
		{ActionStateExecuting, ActionStateFailed},
	}
	for _, transition := range valid {
		if err := validateActionStateTransition(transition[0], transition[1]); err != nil {
			t.Errorf("valid transition %q -> %q rejected: %v", transition[0], transition[1], err)
		}
	}

	invalid := [][2]ActionState{
		{ActionStateProposed, ActionStateExecuting},
		{ActionStateProposed, ActionStateSucceeded},
		{ActionStateAwaitingConfirmation, ActionStateExecuting},
		{ActionStateAwaitingConfirmation, ActionStateSucceeded},
		{ActionStateCancelled, ActionStateProposed},
		{ActionStateCancelled, ActionStateAuthorized},
		{ActionStateCancelled, ActionStateExecuting},
		{ActionStateFailed, ActionStateAuthorized},
		{ActionStateSucceeded, ActionStateExecuting},
		{ActionStateAuthorized, ActionStateSucceeded},
		{ActionStateProposed, ActionStateProposed},
	}
	for _, transition := range invalid {
		if err := validateActionStateTransition(transition[0], transition[1]); !errors.Is(
			err,
			errInvalidActionStateTransition,
		) {
			t.Errorf("invalid transition %q -> %q error=%v", transition[0], transition[1], err)
		}
	}
	if err := validateActionStateTransition(
		ActionState("invented"),
		ActionStateAuthorized,
	); !errors.Is(err, errInvalidActionState) {
		t.Fatalf("invalid state error=%v", err)
	}

	direct, err := phase4ActionCatalog().propose(ActionProposalInput{
		ID:        "action_abcdef0123456789abcdef0123456789",
		Kind:      ActionKindRestartApplication,
		Origin:    ActionOriginDirectUser,
		Target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
		Arguments: RestartApplicationArguments{},
		CreatedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if direct.State() != ActionStateAuthorized {
		t.Fatalf("direct restart state=%q", direct.State())
	}
	executing, err := transitionActionProposal(direct, ActionStateExecuting)
	if err != nil {
		t.Fatal(err)
	}
	succeeded, err := transitionActionProposal(executing, ActionStateSucceeded)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := transitionActionProposal(executing, ActionStateFailed)
	if err != nil {
		t.Fatal(err)
	}
	for _, terminal := range []ActionProposal{succeeded, failed} {
		if _, err := transitionActionProposal(
			terminal,
			ActionStateExecuting,
		); !errors.Is(err, errInvalidActionStateTransition) {
			t.Fatalf("terminal %q transition error=%v", terminal.State(), err)
		}
	}
	if direct.State() != ActionStateAuthorized ||
		executing.State() != ActionStateExecuting ||
		succeeded.State() != ActionStateSucceeded ||
		failed.State() != ActionStateFailed {

		t.Fatalf("value transitions mutated prior proposal states")
	}
}

func TestPhase4ActionProposalCreationIsPureAndHasClosedExecutionPolicy(t *testing.T) {
	catalog := phase4ActionCatalog()
	for _, definition := range catalog.definitionsCopy() {
		wantExecution := definition.Kind == ActionKindRestartApplication
		if definition.SupportsExecution != wantExecution {
			t.Fatalf("action %q execution support=%t want=%t", definition.Kind, definition.SupportsExecution, wantExecution)
		}
	}
	input := ActionProposalInput{
		ID:        "action_abcdef0123456789abcdef0123456789",
		Kind:      ActionKindRollbackDeployment,
		Origin:    ActionOriginAssistantSuggested,
		Target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
		Arguments: RollbackDeploymentArguments{},
		CreatedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
	}
	first, err := catalog.propose(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.propose(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("proposal creation is not deterministic")
	}
	firstDecision, err := catalog.evaluate(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDecision, err := catalog.evaluate(first)
	if err != nil {
		t.Fatal(err)
	}
	if firstDecision != secondDecision {
		t.Fatalf("proposal evaluation is not deterministic")
	}
}

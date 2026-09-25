package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func testShadowRouterContext() ShadowRouterContext {
	return ShadowRouterContext{
		Now: time.Date(2026, 9, 22, 16, 30, 0, 0, time.UTC),
		Entities: []RouterEntity{
			{
				Subject: EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"},
				Aliases: []string{"my scheduler", "myscheduler"},
			},
			{
				Subject: EvidenceSubject{Kind: SubjectApplication, ID: "golf-mullet", Name: "Golf Mullet"},
				Aliases: []string{"golf mullet", "golf-mullet"},
			},
			{
				Subject: EvidenceSubject{Kind: SubjectApplication, ID: "minideploy", Name: "MiniDeploy"},
				Aliases: []string{"mini deploy", "minideploy"},
			},
		},
	}
}

func TestShadowRouterCurrentHealthParaphrasesShareRoute(t *testing.T) {
	for _, question := range []string{
		"Is the Dell healthy now?",
		"How is the Dell doing?",
		"What is the current health of the Dell?",
		"Is the Dell in a normal condition?",
	} {
		route := buildShadowInvestigationRoute(question, testShadowRouterContext())
		if route.ID != RouteCurrentPlatformHealth {
			t.Fatalf("question=%q route=%q frame=%+v", question, route.ID, route.Frame)
		}
		if route.Frame.Goal != GoalCurrentAssessment || route.Frame.Exactness != ExactnessJudgment {
			t.Fatalf("question=%q goal/exactness=%q/%q", question, route.Frame.Goal, route.Frame.Exactness)
		}
	}
}

func TestCurrentHealthRequirementsStayNarrow(t *testing.T) {
	route := buildShadowInvestigationRoute("Is the Dell healthy?", testShadowRouterContext())
	assertRequirementTypes(t, route, []EvidenceType{EvidenceTypeCurrentPlatformState})
	for _, requirement := range route.Requirements {
		if requirement.Type == EvidenceTypeDatabaseBackups ||
			requirement.Type == EvidenceTypeRepositorySearch ||
			requirement.Type == EvidenceTypeRepositoryContent ||
			requirement.Type == EvidenceTypeRuntimeFailures ||
			requirement.Type == EvidenceTypeDeploymentFailures {
			t.Fatalf("current health included unrelated requirement: %+v", requirement)
		}
		if requirement.Freshness.Defined {
			t.Fatalf("current freshness invented an SLA: %+v", requirement.Freshness)
		}
	}
}

func TestThermalInvestigationRequirements(t *testing.T) {
	route := buildShadowInvestigationRoute("Does the Dell look like it is overheating today?", testShadowRouterContext())
	if route.ID != RouteThermalInvestigation {
		t.Fatalf("route=%q frame=%+v", route.ID, route.Frame)
	}
	assertRequirementTypes(t, route, []EvidenceType{
		EvidenceTypeCurrentPlatformState,
		EvidenceTypeHostHistory,
		EvidenceTypeThermalHistory,
	})
}

func TestRestartExplanationRequirements(t *testing.T) {
	route := buildShadowInvestigationRoute("Why did the Dell restart yesterday?", testShadowRouterContext())
	if route.ID != RouteRestartInvestigation {
		t.Fatalf("route=%q frame=%+v", route.ID, route.Frame)
	}
	assertRequirementTypes(t, route, []EvidenceType{
		EvidenceTypeActivityTimeline,
		EvidenceTypeHostHistory,
		EvidenceTypeInfrastructureEvents,
		EvidenceTypeRecoveryTimeline,
		EvidenceTypeThermalHistory,
	})
	for _, requirement := range route.Requirements {
		if requirement.Window == nil || requirement.Window.Kind != TemporalYesterday {
			t.Fatalf("requirement did not preserve yesterday scope: %+v", requirement)
		}
	}
}

func TestDeploymentCorrelationRequirements(t *testing.T) {
	route := buildShadowInvestigationRoute(
		"Did the latest deployment cause Golf Mullet to slow down in the last 24 hours?",
		testShadowRouterContext(),
	)
	if route.ID != RouteDeploymentCorrelation {
		t.Fatalf("route=%q frame=%+v", route.ID, route.Frame)
	}
	assertRequirementTypes(t, route, []EvidenceType{
		EvidenceTypeActivityTimeline,
		EvidenceTypeApplicationHistory,
		EvidenceTypeCurrentApplication,
		EvidenceTypeDeploymentTimeline,
		EvidenceTypeInfrastructureEvents,
	})
	for _, requirement := range route.Requirements {
		if requirement.Subject.Kind == SubjectApplication && requirement.Subject.ID != "golf-mullet" {
			t.Fatalf("application subject=%+v", requirement.Subject)
		}
	}
}

func TestExactFactIsDistinctFromAssessment(t *testing.T) {
	exact := buildShadowInvestigationRoute("What commit is Golf Mullet running?", testShadowRouterContext())
	assessment := buildShadowInvestigationRoute("Is Golf Mullet healthy?", testShadowRouterContext())
	if exact.ID != RouteExactCurrentFact || exact.Frame.Goal != GoalExactFact || exact.Frame.Exactness != ExactnessExact {
		t.Fatalf("exact route=%+v", exact)
	}
	assertRequirementTypes(t, exact, []EvidenceType{EvidenceTypeCurrentDeployment})
	if assessment.ID != RouteApplicationCurrent || assessment.Frame.Goal != GoalCurrentAssessment || assessment.Frame.Exactness != ExactnessJudgment {
		t.Fatalf("assessment route=%+v", assessment)
	}
	if exact.ID == assessment.ID || exact.Frame.Exactness == assessment.Frame.Exactness {
		t.Fatalf("fact and judgment collapsed: exact=%+v assessment=%+v", exact, assessment)
	}
}

func TestAmbiguousSubjectsRemainExplicit(t *testing.T) {
	route := buildShadowInvestigationRoute("Is Golf Mullet or MiniDeploy healthy?", testShadowRouterContext())
	if !route.Frame.Ambiguity.Ambiguous || route.Frame.Ambiguity.Reason != "multiple_subjects" {
		t.Fatalf("ambiguity=%+v", route.Frame.Ambiguity)
	}
	if len(route.Frame.Ambiguity.Candidates) != 2 || route.ID != RouteUnresolved ||
		route.Resolution != RouteResolutionAmbiguous || len(route.Requirements) != 0 {

		t.Fatalf("ambiguous route=%+v", route)
	}
	unresolved := buildShadowInvestigationRoute("Is this app healthy?", ShadowRouterContext{Now: testShadowRouterContext().Now})
	if !unresolved.Frame.Ambiguity.Ambiguous || unresolved.Frame.Ambiguity.Reason != "unresolved_application_subject" || unresolved.ID != RouteUnresolved {
		t.Fatalf("unresolved application was not explicit: %+v", unresolved)
	}
}

func TestMultiSubjectComparisonIsExplicitlyUnsupported(t *testing.T) {
	for _, question := range []string{
		"Compare MyScheduler and Golf Mullet",
		"What is the difference between MyScheduler and Golf Mullet?",
	} {
		route := buildShadowInvestigationRoute(question, testShadowRouterContext())
		if route.Frame.Goal != GoalComparison || route.Frame.Ambiguity.Ambiguous {
			t.Fatalf("question=%q comparison frame=%+v", question, route.Frame)
		}
		if len(route.Frame.Subjects) != 2 ||
			route.Frame.Subjects[0].ID != "golf-mullet" ||
			route.Frame.Subjects[1].ID != "myscheduler" {

			t.Fatalf("question=%q comparison subjects=%+v", question, route.Frame.Subjects)
		}
		if route.ID != RouteUnresolved || route.Resolution != RouteResolutionUnsupported ||
			route.Reason != "comparison_not_supported" || len(route.Requirements) != 0 {

			t.Fatalf("question=%q comparison silently collapsed: %+v", question, route)
		}
	}
}

func TestUnsupportedApplicationTrendDoesNotDowngradeToCurrentState(t *testing.T) {
	route := buildShadowInvestigationRoute(
		"Show Golf Mullet application history",
		testShadowRouterContext(),
	)
	if route.Frame.Goal != GoalTrend || route.ID != RouteUnresolved ||
		route.Resolution != RouteResolutionUnsupported ||
		route.Reason != "trend_not_supported_for_domains" ||
		len(route.Requirements) != 0 {

		t.Fatalf("unsupported trend downgraded: %+v", route)
	}
}

func TestShadowRouterUsesOnlyBoundedConversationSubjects(t *testing.T) {
	context := ShadowRouterContext{
		Now: testShadowRouterContext().Now,
		ConversationSubjects: []EvidenceSubject{
			{Kind: SubjectApplication, ID: "golf-mullet", Name: "Golf Mullet"},
		},
	}
	route := buildShadowInvestigationRoute("Is it healthy?", context)
	if route.ID != RouteApplicationCurrent || len(route.Frame.Subjects) != 1 || route.Frame.Subjects[0].ID != "golf-mullet" {
		t.Fatalf("bounded conversation subject was not resolved: %+v", route)
	}

	context.ConversationSubjects = append(context.ConversationSubjects,
		EvidenceSubject{Kind: SubjectApplication, ID: "minideploy", Name: "MiniDeploy"},
	)
	ambiguous := buildShadowInvestigationRoute("Is it healthy?", context)
	if !ambiguous.Frame.Ambiguity.Ambiguous || ambiguous.ID != RouteUnresolved {
		t.Fatalf("multiple conversation subjects were guessed: %+v", ambiguous)
	}
}

func TestTemporalScopeParserDistinguishesSupportedWindows(t *testing.T) {
	now := testShadowRouterContext().Now
	cases := []struct {
		question  string
		kind      TemporalScopeKind
		rangeName string
	}{
		{"right now", TemporalNow, ""},
		{"today", TemporalToday, ""},
		{"yesterday", TemporalYesterday, ""},
		{"during the last hour", TemporalNamedWindow, "1h"},
		{"during the last 15 minutes", TemporalNamedWindow, "15m"},
		{"during the last 6 hours", TemporalNamedWindow, "6h"},
		{"during the last 24 hours", TemporalNamedWindow, "24h"},
		{"during the last 7 days", TemporalNamedWindow, "7d"},
		{"from 2026-09-21T10:00:00Z to 2026-09-21T12:00:00Z", TemporalExplicitWindow, ""},
		{"around incident 2026-09-21T11:00:00Z", TemporalIncidentCentered, ""},
	}
	for _, test := range cases {
		scope := parseTemporalScope(test.question, now, time.UTC)
		if scope.Kind != test.kind || scope.NamedRange != test.rangeName || !scope.Valid {
			t.Fatalf("question=%q scope=%+v want kind=%q range=%q", test.question, scope, test.kind, test.rangeName)
		}
	}
	tooLong := parseTemporalScope(
		"from 2026-09-01T00:00:00Z to 2026-09-09T00:00:00Z",
		now,
		time.UTC,
	)
	if tooLong.Kind != TemporalExplicitWindow || tooLong.Valid {
		t.Fatalf("over-retention window accepted: %+v", tooLong)
	}
	futureExplicit := parseTemporalScope(
		"from 2026-09-22T16:00:00Z to 2026-09-22T17:00:00Z",
		now,
		time.UTC,
	)
	if futureExplicit.Kind != TemporalExplicitWindow || futureExplicit.Valid {
		t.Fatalf("future explicit window accepted: %+v", futureExplicit)
	}
	nearNow := parseTemporalScope(
		"around incident 2026-09-22T16:00:00Z",
		now,
		time.UTC,
	)
	if !nearNow.Valid || nearNow.To == nil || !nearNow.To.Equal(now) ||
		nearNow.From == nil || !nearNow.From.Before(*nearNow.To) || nearNow.To.After(now) {

		t.Fatalf("near-now incident was not safely clamped: %+v", nearNow)
	}
	futureIncident := parseTemporalScope(
		"around incident 2026-09-22T17:00:00Z",
		now,
		time.UTC,
	)
	if futureIncident.Kind != TemporalIncidentCentered || futureIncident.Valid {
		t.Fatalf("future incident became historical evidence: %+v", futureIncident)
	}
	futureIncidentRoute := buildShadowInvestigationRoute(
		"Why did the Dell restart around incident 2026-09-22T17:00:00Z?",
		testShadowRouterContext(),
	)
	if futureIncidentRoute.ID != RouteUnresolved ||
		futureIncidentRoute.Resolution != RouteResolutionUnsupported ||
		futureIncidentRoute.Reason != "invalid_temporal_scope" ||
		len(futureIncidentRoute.Requirements) != 0 {

		t.Fatalf("future incident produced historical requirements: %+v", futureIncidentRoute)
	}
}

func TestSemanticRouteProfilesAreCompleteAndDeterministic(t *testing.T) {
	first := phase2SemanticRouteProfiles()
	second := phase2SemanticRouteProfiles()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("route profiles are not deterministic:\n%+v\n%+v", first, second)
	}
	want := []InvestigationRouteID{
		RouteExactCurrentFact,
		RouteCurrentPlatformHealth,
		RouteThermalInvestigation,
		RouteRestartInvestigation,
		RouteApplicationCurrent,
		RouteApplicationPerformance,
		RouteDeploymentCorrelation,
		RouteDatabaseInvestigation,
		RouteRepositoryInvestigation,
	}
	got := make([]InvestigationRouteID, 0, len(first))
	for _, profile := range first {
		got = append(got, profile.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("profiles=%v want %v", got, want)
	}
}

func TestShadowRouterIsAbsentFromProductionRequestPaths(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") || path == "investigation_router.go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			identifier, ok := call.Fun.(*ast.Ident)
			if ok && identifier.Name == "buildShadowInvestigationRoute" {
				t.Errorf("production path %s invokes Phase 2A shadow routing", path)
			}
			return true
		})
	}
}

func assertRequirementTypes(t *testing.T, route ShadowRouteDecision, want []EvidenceType) {
	t.Helper()
	got := make([]EvidenceType, 0, len(route.Requirements))
	for _, requirement := range route.Requirements {
		got = append(got, requirement.Type)
		if requirement.ID == "" || requirement.Round != InvestigationRoundInitial {
			t.Fatalf("incomplete requirement: %+v", requirement)
		}
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("route=%q requirements=%v want %v", route.ID, got, want)
	}
}

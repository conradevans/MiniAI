package main

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

const maximumHistoricalWindow = 7 * 24 * time.Hour

type InvestigationGoal string

const (
	GoalExactFact              InvestigationGoal = "exact_fact"
	GoalCurrentAssessment      InvestigationGoal = "current_assessment"
	GoalTrend                  InvestigationGoal = "trend"
	GoalIncidentExplanation    InvestigationGoal = "incident_explanation"
	GoalComparison             InvestigationGoal = "comparison"
	GoalCausalAssessment       InvestigationGoal = "causal_assessment"
	GoalImplementationLocation InvestigationGoal = "implementation_source_location"
	GoalUnresolved             InvestigationGoal = "unresolved"
)

type EvidenceDomain string

const (
	DomainPlatform       EvidenceDomain = "platform"
	DomainThermal        EvidenceDomain = "thermal"
	DomainRecovery       EvidenceDomain = "recovery"
	DomainApplication    EvidenceDomain = "application"
	DomainPerformance    EvidenceDomain = "performance"
	DomainDeployment     EvidenceDomain = "deployment"
	DomainInfrastructure EvidenceDomain = "infrastructure"
	DomainDatabase       EvidenceDomain = "database"
	DomainRepository     EvidenceDomain = "repository"
	DomainRuntime        EvidenceDomain = "runtime"
)

type QuestionExactness string

const (
	ExactnessExact       QuestionExactness = "exact"
	ExactnessJudgment    QuestionExactness = "judgment"
	ExactnessUnspecified QuestionExactness = "unspecified"
)

type RequestedDetail string

const (
	DetailSummary  RequestedDetail = "summary"
	DetailDetailed RequestedDetail = "detailed"
)

type TemporalScopeKind string

const (
	TemporalUnspecified      TemporalScopeKind = "unspecified"
	TemporalNow              TemporalScopeKind = "now"
	TemporalNamedWindow      TemporalScopeKind = "named_window"
	TemporalToday            TemporalScopeKind = "today"
	TemporalYesterday        TemporalScopeKind = "yesterday"
	TemporalExplicitWindow   TemporalScopeKind = "explicit_window"
	TemporalIncidentCentered TemporalScopeKind = "incident_centered"
)

type TemporalScope struct {
	Kind       TemporalScopeKind `json:"kind"`
	NamedRange string            `json:"namedRange,omitempty"`
	At         *time.Time        `json:"at,omitempty"`
	From       *time.Time        `json:"from,omitempty"`
	To         *time.Time        `json:"to,omitempty"`
	Valid      bool              `json:"valid"`
	Reason     string            `json:"reason,omitempty"`
}

type QuestionAmbiguity struct {
	Ambiguous  bool              `json:"ambiguous"`
	Reason     string            `json:"reason,omitempty"`
	Candidates []EvidenceSubject `json:"candidates,omitempty"`
}

type QuestionFrame struct {
	Goal      InvestigationGoal `json:"goal"`
	Subjects  []EvidenceSubject `json:"subjects"`
	Domains   []EvidenceDomain  `json:"domains"`
	Temporal  TemporalScope     `json:"temporal"`
	Exactness QuestionExactness `json:"exactness"`
	Detail    RequestedDetail   `json:"detail"`
	Ambiguity QuestionAmbiguity `json:"ambiguity"`
}

type RouterEntity struct {
	Subject EvidenceSubject
	Aliases []string
}

type ShadowRouterContext struct {
	Now                  time.Time
	Location             *time.Location
	Entities             []RouterEntity
	ConversationSubjects []EvidenceSubject
}

type InvestigationRound string

const (
	InvestigationRoundInitial  InvestigationRound = "initial"
	InvestigationRoundFollowup InvestigationRound = "followup"
)

type EvidenceCardinality string

const (
	CardinalityOne      EvidenceCardinality = "one"
	CardinalityOptional EvidenceCardinality = "optional"
	CardinalityMany     EvidenceCardinality = "many"
)

type FreshnessPolicy struct {
	Defined bool           `json:"defined"`
	MaxAge  *time.Duration `json:"maxAge,omitempty"`
	Reason  string         `json:"reason,omitempty"`
}

type EvidenceRequirement struct {
	ID          string                 `json:"id"`
	Type        EvidenceType           `json:"type"`
	Subject     EvidenceSubject        `json:"subject"`
	Window      *TemporalScope         `json:"window,omitempty"`
	Criticality RequirementCriticality `json:"criticality"`
	Freshness   FreshnessPolicy        `json:"freshness"`
	Cardinality EvidenceCardinality    `json:"cardinality"`
	Round       InvestigationRound     `json:"round"`
}

func (requirement EvidenceRequirement) WithStableID(routeID string) EvidenceRequirement {
	requirement.ID = stableContractID("requirement", struct {
		RouteID     string
		Type        EvidenceType
		Subject     EvidenceSubject
		Window      *TemporalScope
		Criticality RequirementCriticality
		Freshness   FreshnessPolicy
		Cardinality EvidenceCardinality
		Round       InvestigationRound
	}{routeID, requirement.Type, requirement.Subject, requirement.Window, requirement.Criticality, requirement.Freshness, requirement.Cardinality, requirement.Round})
	return requirement
}

type InvestigationRouteID string

const (
	RouteExactCurrentFact        InvestigationRouteID = "exact_current_fact"
	RouteCurrentPlatformHealth   InvestigationRouteID = "current_platform_health"
	RouteThermalInvestigation    InvestigationRouteID = "thermal_investigation"
	RouteRestartInvestigation    InvestigationRouteID = "restart_recovery_investigation"
	RouteApplicationCurrent      InvestigationRouteID = "application_current_status"
	RouteApplicationPerformance  InvestigationRouteID = "application_performance_investigation"
	RouteDeploymentCorrelation   InvestigationRouteID = "deployment_correlation_investigation"
	RouteDatabaseInvestigation   InvestigationRouteID = "database_backup_investigation"
	RouteRepositoryInvestigation InvestigationRouteID = "repository_source_investigation"
	RouteUnresolved              InvestigationRouteID = "unresolved"
)

type RouteResolution string

const (
	RouteResolutionSupported   RouteResolution = "supported"
	RouteResolutionAmbiguous   RouteResolution = "ambiguous"
	RouteResolutionUnsupported RouteResolution = "unsupported"
)

// InvestigationRoute is the canonical route description stored with gathered
// evidence. Requirements live only on InvestigationEvidence once gathering
// begins, so the evidence graph cannot contain two authoritative copies.
type InvestigationRoute struct {
	ID         InvestigationRouteID `json:"id"`
	Resolution RouteResolution      `json:"resolution"`
	Reason     string               `json:"reason,omitempty"`
	Frame      QuestionFrame        `json:"frame"`
}

type requirementSubject string

const (
	requirementHost        requirementSubject = "host"
	requirementApplication requirementSubject = "application"
	requirementDatabase    requirementSubject = "database"
	requirementRepository  requirementSubject = "repository"
)

type requirementWindow string

const (
	windowCurrent     requirementWindow = "current"
	windowQuestion    requirementWindow = "question"
	windowQuestion24h requirementWindow = "question_or_24h"
	windowQuestion7d  requirementWindow = "question_or_7d"
)

type evidenceRequirementTemplate struct {
	Type        EvidenceType
	Subject     requirementSubject
	Window      requirementWindow
	Criticality RequirementCriticality
	Cardinality EvidenceCardinality
}

type SemanticRouteProfile struct {
	ID           InvestigationRouteID
	Requirements []evidenceRequirementTemplate
}

// phase2SemanticRouteProfiles declaratively maps supported semantic routes to bounded evidence requirements.
func phase2SemanticRouteProfiles() []SemanticRouteProfile {
	return []SemanticRouteProfile{
		{ID: RouteExactCurrentFact},
		{ID: RouteCurrentPlatformHealth, Requirements: []evidenceRequirementTemplate{
			{EvidenceTypeCurrentPlatformState, requirementHost, windowCurrent, CriticalityCritical, CardinalityOne},
		}},
		{ID: RouteThermalInvestigation, Requirements: []evidenceRequirementTemplate{
			{EvidenceTypeCurrentPlatformState, requirementHost, windowCurrent, CriticalityCritical, CardinalityOne},
			{EvidenceTypeThermalHistory, requirementHost, windowQuestion24h, CriticalityCritical, CardinalityMany},
			{EvidenceTypeHostHistory, requirementHost, windowQuestion24h, CriticalityRelevant, CardinalityMany},
		}},
		{ID: RouteRestartInvestigation, Requirements: []evidenceRequirementTemplate{
			{EvidenceTypeRecoveryTimeline, requirementHost, windowQuestion24h, CriticalityCritical, CardinalityMany},
			{EvidenceTypeInfrastructureEvents, requirementHost, windowQuestion24h, CriticalityCritical, CardinalityMany},
			{EvidenceTypeActivityTimeline, requirementHost, windowQuestion24h, CriticalityRelevant, CardinalityMany},
			{EvidenceTypeHostHistory, requirementHost, windowQuestion24h, CriticalityRelevant, CardinalityMany},
			{EvidenceTypeThermalHistory, requirementHost, windowQuestion24h, CriticalityRelevant, CardinalityMany},
		}},
		{ID: RouteApplicationCurrent, Requirements: []evidenceRequirementTemplate{
			{EvidenceTypeCurrentApplication, requirementApplication, windowCurrent, CriticalityCritical, CardinalityOne},
		}},
		{ID: RouteApplicationPerformance, Requirements: []evidenceRequirementTemplate{
			{EvidenceTypeCurrentApplication, requirementApplication, windowCurrent, CriticalityCritical, CardinalityOne},
			{EvidenceTypeApplicationHistory, requirementApplication, windowQuestion24h, CriticalityCritical, CardinalityMany},
			// ReactorLab service history is platform-service evidence, not an
			// application-scoped series. Keep the subject honest until an API can
			// resolve deployed application services to observability service IDs.
			{EvidenceTypeServiceHistory, requirementHost, windowQuestion24h, CriticalityRelevant, CardinalityMany},
			{EvidenceTypeHostHistory, requirementHost, windowQuestion24h, CriticalitySupporting, CardinalityMany},
		}},
		{ID: RouteDeploymentCorrelation, Requirements: []evidenceRequirementTemplate{
			{EvidenceTypeCurrentApplication, requirementApplication, windowCurrent, CriticalityCritical, CardinalityOne},
			{EvidenceTypeDeploymentTimeline, requirementApplication, windowQuestion7d, CriticalityCritical, CardinalityMany},
			{EvidenceTypeApplicationHistory, requirementApplication, windowQuestion7d, CriticalityCritical, CardinalityMany},
			{EvidenceTypeInfrastructureEvents, requirementHost, windowQuestion7d, CriticalityRelevant, CardinalityMany},
			{EvidenceTypeActivityTimeline, requirementHost, windowQuestion7d, CriticalityRelevant, CardinalityMany},
		}},
		{ID: RouteDatabaseInvestigation, Requirements: []evidenceRequirementTemplate{
			{EvidenceTypeCurrentDatabase, requirementDatabase, windowCurrent, CriticalityCritical, CardinalityMany},
			{EvidenceTypeDatabaseBackups, requirementDatabase, windowQuestion7d, CriticalityRelevant, CardinalityMany},
		}},
		{ID: RouteRepositoryInvestigation, Requirements: []evidenceRequirementTemplate{
			{EvidenceTypeRepositorySearch, requirementRepository, windowQuestion, CriticalityCritical, CardinalityMany},
			{EvidenceTypeRepositoryContent, requirementRepository, windowQuestion, CriticalityRelevant, CardinalityMany},
		}},
	}
}

type ShadowRouteDecision struct {
	ID           InvestigationRouteID  `json:"id"`
	Resolution   RouteResolution       `json:"resolution"`
	Reason       string                `json:"reason,omitempty"`
	Frame        QuestionFrame         `json:"frame"`
	Requirements []EvidenceRequirement `json:"requirements"`
}

func (decision ShadowRouteDecision) RouteDescriptor() InvestigationRoute {
	return InvestigationRoute{
		ID:         decision.ID,
		Resolution: decision.Resolution,
		Reason:     decision.Reason,
		Frame:      decision.Frame,
	}
}

func buildShadowInvestigationRoute(question string, context ShadowRouterContext) ShadowRouteDecision {
	now := context.Now
	if now.IsZero() {
		now = time.Unix(0, 0).UTC()
	}
	location := context.Location
	if location == nil {
		location = time.UTC
	}

	normalized := normalizeQuestionText(question)
	frame := QuestionFrame{
		Goal:     classifyInvestigationGoal(normalized),
		Domains:  classifyEvidenceDomains(normalized),
		Temporal: parseTemporalScope(question, now, location),
		Detail:   classifyRequestedDetail(normalized),
	}
	frame.Subjects, frame.Ambiguity = resolveShadowSubjects(normalized, context)
	frame.Domains = addSubjectDomains(frame.Domains, frame.Subjects)
	if frame.Goal == GoalImplementationLocation && !hasDomain(frame, DomainRepository) {
		frame.Domains = append(frame.Domains, DomainRepository)
		sort.Slice(frame.Domains, func(i, j int) bool { return frame.Domains[i] < frame.Domains[j] })
	}
	frame.Exactness = classifyQuestionExactness(frame.Goal)
	if !isEvidenceSeekingGoal(frame.Goal) {
		frame.Ambiguity = QuestionAmbiguity{}
	}
	if !frame.Ambiguity.Ambiguous && requiresResolvedApplication(frame) && !hasApplicationSubject(frame.Subjects) {
		frame.Ambiguity = QuestionAmbiguity{Ambiguous: true, Reason: "unresolved_application_subject"}
	}

	resolution := RouteResolutionSupported
	reason := ""
	routeID := RouteUnresolved
	unsupported := unsupportedReason(frame)
	switch {
	case frame.Ambiguity.Ambiguous:
		resolution = RouteResolutionAmbiguous
		reason = frame.Ambiguity.Reason
	case !frame.Temporal.Valid:
		resolution = RouteResolutionUnsupported
		reason = "invalid_temporal_scope"
	case unsupported != "":
		resolution = RouteResolutionUnsupported
		reason = unsupported
	default:
		routeID = selectSemanticRoute(frame)
		if routeID == RouteUnresolved {
			resolution = RouteResolutionUnsupported
			reason = "unsupported_goal_domain"
		}
	}
	requirements := requirementsForRoute(routeID, frame)
	for index := range requirements {
		requirements[index] = requirements[index].WithStableID(string(routeID))
	}
	return ShadowRouteDecision{
		ID:           routeID,
		Resolution:   resolution,
		Reason:       reason,
		Frame:        frame,
		Requirements: requirements,
	}
}

// routePhase2BQuestion is the deterministic semantic routing seam shared by tests and production.
func routePhase2BQuestion(question string, context ShadowRouterContext) ShadowRouteDecision {
	return buildShadowInvestigationRoute(question, context)
}

func classifyInvestigationGoal(question string) InvestigationGoal {
	switch {
	case containsAnyConcept(question, "where is", "where does", "implemented", "implementation", "source code", "which file", "which function"):
		return GoalImplementationLocation
	case containsAnyConcept(question, "did the deploy", "did the deployment", "cause", "caused", "responsible", "correlate", "likely caused"):
		return GoalCausalAssessment
	case containsAnyConcept(question, "compare", "versus", "difference between"):
		return GoalComparison
	case containsAnyConcept(question, "why", "what happened", "diagnose", "investigate") ||
		(containsConcept(question, "explain") && containsAnyConcept(question,
			"outage", "failure", "failed", "failing", "error", "crash", "restart", "reboot",
			"incident", "broken", "issue", "problem", "what changed")):
		return GoalIncidentExplanation
	case containsAnyConcept(question, "healthy", "health", "normal", "overheating", "too hot", "doing", "condition", "okay", "ok", "armed") ||
		isCurrentPerformanceAssessment(question):
		return GoalCurrentAssessment
	case containsAnyConcept(question, "trend", "over time", "history", "historical",
		"last hour", "last 24 hours", "last day", "last week", "yesterday", "earlier", "during"):
		return GoalTrend
	case containsAnyConcept(question, "what commit", "which commit", "what sha", "which sha", "what branch", "which branch", "how many", "is active", "current status", "exact"):
		return GoalExactFact
	default:
		return GoalUnresolved
	}
}

func isCurrentPerformanceAssessment(question string) bool {
	if !containsAnyConcept(question, "slow", "latency", "performance") {
		return false
	}
	return containsAnyConcept(question, "now", "today", "current", "currently") ||
		strings.HasPrefix(question, "is ") || strings.HasPrefix(question, "are ") ||
		strings.HasPrefix(question, "has ") || strings.HasPrefix(question, "have ")
}

func isEvidenceSeekingGoal(goal InvestigationGoal) bool {
	switch goal {
	case GoalExactFact, GoalCurrentAssessment, GoalTrend, GoalIncidentExplanation,
		GoalComparison, GoalCausalAssessment, GoalImplementationLocation:
		return true
	default:
		return false
	}
}

// isEvidenceSeekingQuestion is a pure semantic preclassification. It must not
// perform evidence reads because production uses it to choose one architecture.
func isEvidenceSeekingQuestion(question string) bool {
	normalized := normalizeQuestionText(question)
	if isEvidenceSeekingGoal(classifyInvestigationGoal(normalized)) {
		return true
	}
	return containsAnyConcept(normalized, "check", "inspect", "show", "read", "list", "look up", "lookup") &&
		len(classifyEvidenceDomains(normalized)) > 0
}

func classifyQuestionExactness(goal InvestigationGoal) QuestionExactness {
	if goal == GoalExactFact || goal == GoalImplementationLocation {
		return ExactnessExact
	}
	if goal == GoalCurrentAssessment || goal == GoalIncidentExplanation || goal == GoalCausalAssessment || goal == GoalComparison || goal == GoalTrend {
		return ExactnessJudgment
	}
	return ExactnessUnspecified
}

func classifyRequestedDetail(question string) RequestedDetail {
	if containsAnyConcept(question, "detail", "detailed", "explain", "investigate", "breakdown") {
		return DetailDetailed
	}
	return DetailSummary
}

func classifyEvidenceDomains(question string) []EvidenceDomain {
	ontology := []struct {
		domain EvidenceDomain
		terms  []string
	}{
		{DomainPlatform, []string{"dell", "host", "platform", "system", "server"}},
		{DomainThermal, []string{"thermal", "temperature", "temperatures", "overheat", "overheating", "too hot"}},
		{DomainRecovery, []string{"restart", "restarted", "reboot", "rebooted", "recovery", "watchdog", "rtc", "outage"}},
		{DomainApplication, []string{"app", "application", "service"}},
		{DomainPerformance, []string{"performance", "slow", "latency", "cpu", "memory", "load", "resources"}},
		{DomainDeployment, []string{"deploy", "deployed", "deployment", "release", "rollout", "commit", "sha", "branch", "version"}},
		{DomainInfrastructure, []string{"infrastructure", "event", "events", "activity", "incident"}},
		{DomainDatabase, []string{"database", "databases", "backup", "backups", "postgres"}},
		{DomainRepository, []string{"repository", "repo", "source code", "implementation", "which file", "which function"}},
		{DomainRuntime, []string{"runtime", "log", "logs", "error", "errors", "failure", "failures"}},
	}
	var domains []EvidenceDomain
	for _, entry := range ontology {
		if containsAnyConcept(question, entry.terms...) {
			domains = append(domains, entry.domain)
		}
	}
	return domains
}

func resolveShadowSubjects(question string, context ShadowRouterContext) ([]EvidenceSubject, QuestionAmbiguity) {
	questionKey := normalizeMatch(question)
	entities := append([]RouterEntity{{
		Subject: EvidenceSubject{Kind: SubjectHost, ID: "dell", Name: "Dell"},
		Aliases: []string{"dell", "host", "server"},
	}}, context.Entities...)
	byKey := map[string]EvidenceSubject{}
	for _, entity := range entities {
		aliases := append([]string{entity.Subject.ID, entity.Subject.Name}, entity.Aliases...)
		for _, alias := range aliases {
			aliasKey := normalizeMatch(alias)
			if alias == "" || (!containsConcept(question, normalizeQuestionText(alias)) &&
				(aliasKey == "" || !strings.Contains(questionKey, aliasKey))) {
				continue
			}
			key := string(entity.Subject.Kind) + ":" + entity.Subject.ID
			byKey[key] = entity.Subject
		}
	}
	if len(byKey) == 0 && (containsAnyConcept(question, "it", "this app", "that app", "this deployment") ||
		shouldInheritBareThisApplication(question)) {
		for _, subject := range context.ConversationSubjects {
			byKey[string(subject.Kind)+":"+subject.ID] = subject
		}
	}

	subjects := make([]EvidenceSubject, 0, len(byKey))
	for _, subject := range byKey {
		subjects = append(subjects, subject)
	}
	sort.Slice(subjects, func(i, j int) bool {
		left := string(subjects[i].Kind) + ":" + subjects[i].ID
		right := string(subjects[j].Kind) + ":" + subjects[j].ID
		return left < right
	})
	ambiguity := QuestionAmbiguity{}
	if len(subjects) > 1 && classifyInvestigationGoal(question) != GoalComparison {
		ambiguity = QuestionAmbiguity{Ambiguous: true, Reason: "multiple_subjects", Candidates: subjects}
	}
	return subjects, ambiguity
}

func addSubjectDomains(domains []EvidenceDomain, subjects []EvidenceSubject) []EvidenceDomain {
	seen := map[EvidenceDomain]bool{}
	for _, domain := range domains {
		seen[domain] = true
	}
	for _, subject := range subjects {
		var domain EvidenceDomain
		switch subject.Kind {
		case SubjectHost, SubjectPlatform:
			domain = DomainPlatform
		case SubjectApplication, SubjectService:
			domain = DomainApplication
		case SubjectDatabase:
			domain = DomainDatabase
		case SubjectRepository:
			domain = DomainRepository
		}
		if domain != "" && !seen[domain] {
			domains = append(domains, domain)
			seen[domain] = true
		}
	}
	sort.Slice(domains, func(i, j int) bool { return domains[i] < domains[j] })
	return domains
}

func selectSemanticRoute(frame QuestionFrame) InvestigationRouteID {
	if frame.Ambiguity.Ambiguous || unsupportedReason(frame) != "" || !isEvidenceSeekingGoal(frame.Goal) {
		return RouteUnresolved
	}
	switch {
	case frame.Goal == GoalImplementationLocation && hasDomain(frame, DomainRepository):
		return RouteRepositoryInvestigation
	case frame.Goal == GoalExactFact && len(frame.Domains) > 0:
		return RouteExactCurrentFact
	case frame.Goal == GoalCausalAssessment && hasDomain(frame, DomainDeployment):
		return RouteDeploymentCorrelation
	case hasDomain(frame, DomainRecovery) &&
		(frame.Goal == GoalIncidentExplanation || frame.Goal == GoalTrend || frame.Goal == GoalCurrentAssessment):
		return RouteRestartInvestigation
	case hasDomain(frame, DomainThermal) &&
		(frame.Goal == GoalCurrentAssessment || frame.Goal == GoalTrend || frame.Goal == GoalIncidentExplanation):
		return RouteThermalInvestigation
	case hasDomain(frame, DomainDatabase) &&
		(frame.Goal == GoalCurrentAssessment || frame.Goal == GoalTrend || frame.Goal == GoalIncidentExplanation):
		return RouteDatabaseInvestigation
	case hasDomain(frame, DomainApplication) && hasDomain(frame, DomainPerformance) &&
		(frame.Goal == GoalCurrentAssessment || frame.Goal == GoalTrend || frame.Goal == GoalIncidentExplanation):
		return RouteApplicationPerformance
	case hasDomain(frame, DomainApplication) &&
		(frame.Goal == GoalCurrentAssessment || frame.Goal == GoalIncidentExplanation):
		return RouteApplicationCurrent
	case hasDomain(frame, DomainPlatform) && frame.Goal == GoalCurrentAssessment:
		return RouteCurrentPlatformHealth
	default:
		return RouteUnresolved
	}
}

func shouldInheritBareThisApplication(question string) bool {
	if !containsConcept(question, "this") {
		return false
	}
	goal := classifyInvestigationGoal(question)
	if goal != GoalCausalAssessment && goal != GoalIncidentExplanation {
		return false
	}
	domains := classifyEvidenceDomains(question)
	for _, domain := range domains {
		if domain == DomainDeployment || domain == DomainRecovery || domain == DomainInfrastructure || domain == DomainRuntime {
			return true
		}
	}
	return false
}

func unsupportedReason(frame QuestionFrame) string {
	if frame.Goal == GoalComparison {
		return "comparison_not_supported"
	}
	if frame.Goal != GoalTrend {
		return ""
	}
	if hasDomain(frame, DomainThermal) || hasDomain(frame, DomainRecovery) ||
		(hasDomain(frame, DomainApplication) && hasDomain(frame, DomainPerformance)) {
		return ""
	}
	return "trend_not_supported_for_domains"
}

func requiresResolvedApplication(frame QuestionFrame) bool {
	if !isEvidenceSeekingGoal(frame.Goal) {
		return false
	}
	if frame.Goal == GoalImplementationLocation || hasDomain(frame, DomainRepository) {
		return true
	}
	if frame.Goal == GoalCausalAssessment && hasDomain(frame, DomainDeployment) {
		return true
	}
	if frame.Goal == GoalExactFact && (hasDomain(frame, DomainApplication) || hasDomain(frame, DomainDeployment)) {
		return true
	}
	return hasDomain(frame, DomainApplication)
}

func hasApplicationSubject(subjects []EvidenceSubject) bool {
	for _, subject := range subjects {
		if subject.Kind == SubjectApplication || subject.Kind == SubjectService {
			return true
		}
	}
	return false
}

func requirementsForRoute(routeID InvestigationRouteID, frame QuestionFrame) []EvidenceRequirement {
	if routeID == RouteUnresolved || len(frame.Subjects) > 1 {
		return nil
	}
	if routeID == RouteExactCurrentFact {
		typeName := EvidenceTypeCurrentPlatformState
		subjectKind := requirementHost
		switch {
		case hasDomain(frame, DomainDeployment):
			typeName, subjectKind = EvidenceTypeCurrentDeployment, requirementApplication
		case hasDomain(frame, DomainDatabase):
			typeName, subjectKind = EvidenceTypeCurrentDatabase, requirementDatabase
		case hasDomain(frame, DomainApplication):
			typeName, subjectKind = EvidenceTypeCurrentApplication, requirementApplication
		}
		template := evidenceRequirementTemplate{typeName, subjectKind, windowCurrent, CriticalityCritical, CardinalityOne}
		return []EvidenceRequirement{requirementFromTemplate(template, frame)}
	}
	for _, profile := range phase2SemanticRouteProfiles() {
		if profile.ID != routeID {
			continue
		}
		requirements := make([]EvidenceRequirement, 0, len(profile.Requirements))
		for _, template := range profile.Requirements {
			requirements = append(requirements, requirementFromTemplate(template, frame))
		}
		return requirements
	}
	return nil
}

func requirementFromTemplate(template evidenceRequirementTemplate, frame QuestionFrame) EvidenceRequirement {
	requirement := EvidenceRequirement{
		Type:        template.Type,
		Subject:     requirementSubjectForFrame(template.Subject, frame),
		Criticality: template.Criticality,
		Freshness: FreshnessPolicy{
			Defined: false,
			Reason:  "no source-specific freshness policy defined",
		},
		Cardinality: template.Cardinality,
		Round:       InvestigationRoundInitial,
	}
	scope := requirementTemporalScope(template.Window, frame.Temporal)
	if scope != nil {
		requirement.Window = scope
	}
	return requirement
}

func requirementSubjectForFrame(kind requirementSubject, frame QuestionFrame) EvidenceSubject {
	wanted := SubjectUnknown
	switch kind {
	case requirementHost:
		wanted = SubjectHost
	case requirementApplication, requirementRepository:
		wanted = SubjectApplication
	case requirementDatabase:
		wanted = SubjectDatabase
	}
	for _, subject := range frame.Subjects {
		if subject.Kind == wanted || (wanted == SubjectApplication && subject.Kind == SubjectService) {
			if kind == requirementRepository {
				return EvidenceSubject{Kind: SubjectRepository, ID: subject.ID, Name: subject.Name}
			}
			return subject
		}
	}
	switch kind {
	case requirementHost:
		return EvidenceSubject{Kind: SubjectHost, ID: "dell", Name: "Dell"}
	case requirementApplication:
		return EvidenceSubject{Kind: SubjectApplication, ID: "unresolved"}
	case requirementDatabase:
		return EvidenceSubject{Kind: SubjectDatabase, ID: "all"}
	case requirementRepository:
		return EvidenceSubject{Kind: SubjectRepository, ID: "unresolved"}
	default:
		return EvidenceSubject{Kind: SubjectUnknown, ID: "unresolved"}
	}
}

func requirementTemporalScope(policy requirementWindow, question TemporalScope) *TemporalScope {
	if policy == windowQuestion && question.Kind == TemporalUnspecified {
		return nil
	}
	if policy == windowCurrent {
		scope := TemporalScope{Kind: TemporalNow, At: question.At, Valid: true}
		return &scope
	}
	if question.Kind != TemporalUnspecified {
		scope := question
		return &scope
	}
	rangeName := "24h"
	if policy == windowQuestion7d {
		rangeName = "7d"
	}
	scope := TemporalScope{Kind: TemporalNamedWindow, NamedRange: rangeName, Valid: true}
	return &scope
}

func hasDomain(frame QuestionFrame, wanted EvidenceDomain) bool {
	for _, domain := range frame.Domains {
		if domain == wanted {
			return true
		}
	}
	return false
}

var routerRFC3339Pattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)

func parseTemporalScope(question string, now time.Time, location *time.Location) TemporalScope {
	if location == nil {
		location = time.UTC
	}
	matches := routerRFC3339Pattern.FindAllString(question, -1)
	if len(matches) >= 2 {
		from, fromErr := time.Parse(time.RFC3339Nano, matches[0])
		to, toErr := time.Parse(time.RFC3339Nano, matches[1])
		valid := fromErr == nil && toErr == nil && to.After(from) &&
			to.Sub(from) <= maximumHistoricalWindow && !to.After(now)
		reason := ""
		if !valid {
			reason = "explicit window must be ordered, no longer than 7d, and not end in the future"
		}
		return TemporalScope{Kind: TemporalExplicitWindow, From: &from, To: &to, Valid: valid, Reason: reason}
	}
	normalized := normalizeQuestionText(question)
	if len(matches) == 1 && containsAnyConcept(normalized, "around", "incident") {
		center, err := time.Parse(time.RFC3339Nano, matches[0])
		if err == nil {
			from, to := center.Add(-time.Hour), center.Add(time.Hour)
			if center.After(now) {
				return TemporalScope{
					Kind:   TemporalIncidentCentered,
					At:     &center,
					From:   &from,
					To:     &to,
					Valid:  false,
					Reason: "incident timestamp is in the future",
				}
			}
			if to.After(now) {
				to = now
			}
			valid := from.Before(to) && to.Sub(from) <= maximumHistoricalWindow
			reason := ""
			if !valid {
				reason = "incident-centered window is empty or exceeds 7d"
			}
			return TemporalScope{
				Kind: TemporalIncidentCentered, At: &center,
				From: &from, To: &to, Valid: valid, Reason: reason,
			}
		}
	}
	localNow := now.In(location)
	startToday := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
	switch {
	case containsConcept(normalized, "yesterday"):
		from, to := startToday.AddDate(0, 0, -1), startToday
		return TemporalScope{Kind: TemporalYesterday, From: &from, To: &to, Valid: true}
	case containsConcept(normalized, "today"):
		return TemporalScope{Kind: TemporalToday, From: &startToday, To: &localNow, Valid: true}
	case containsAnyConcept(normalized, "last hour", "past hour", "1h"):
		return TemporalScope{Kind: TemporalNamedWindow, NamedRange: "1h", Valid: true}
	case containsAnyConcept(normalized, "last 15 minutes", "past 15 minutes", "15m"):
		return TemporalScope{Kind: TemporalNamedWindow, NamedRange: "15m", Valid: true}
	case containsAnyConcept(normalized, "last 6 hours", "past 6 hours", "six hours", "6h"):
		return TemporalScope{Kind: TemporalNamedWindow, NamedRange: "6h", Valid: true}
	case containsAnyConcept(normalized, "last 24 hours", "past 24 hours", "last day", "past day", "24h"):
		return TemporalScope{Kind: TemporalNamedWindow, NamedRange: "24h", Valid: true}
	case containsAnyConcept(normalized, "last 7 days", "past 7 days", "last week", "7d"):
		return TemporalScope{Kind: TemporalNamedWindow, NamedRange: "7d", Valid: true}
	case containsAnyConcept(normalized, "now", "current", "currently"):
		return TemporalScope{Kind: TemporalNow, At: &now, Valid: true}
	default:
		return TemporalScope{Kind: TemporalUnspecified, Valid: true}
	}
}

func normalizeQuestionText(value string) string {
	var builder strings.Builder
	space := true
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ':' || r == '+' || r == '-' || r == '.' {
			builder.WriteRune(r)
			space = false
			continue
		}
		if !space {
			builder.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(builder.String())
}

func containsAnyConcept(question string, concepts ...string) bool {
	for _, concept := range concepts {
		if containsConcept(question, concept) {
			return true
		}
	}
	return false
}

func containsConcept(question, concept string) bool {
	question = " " + normalizeQuestionText(question) + " "
	concept = " " + normalizeQuestionText(concept) + " "
	return strings.Contains(question, concept)
}

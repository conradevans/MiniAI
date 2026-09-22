package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	reactorLabMaxToolApps        = 100
	reactorLabMaxDatabaseLookups = 4
)

var (
	errReactorLabApplicationNotFound  = errors.New("reactorlab_application_not_found")
	errReactorLabApplicationAmbiguous = errors.New("reactorlab_application_ambiguous")
	errReactorLabServiceNotFound      = errors.New("reactorlab_service_not_found")
	errReactorLabServiceAmbiguous     = errors.New("reactorlab_service_ambiguous")
)

type localRepositoryEvidence struct {
	Path   string `json:"path,omitempty"`
	Exists bool   `json:"exists"`
	Branch string `json:"branch,omitempty"`
	Commit string `json:"commit,omitempty"`
}

type reactorLabAppSummary struct {
	App             string                         `json:"app"`
	Strategy        string                         `json:"strategy"`
	Status          string                         `json:"status"`
	SourceAvailable bool                           `json:"sourceAvailable"`
	Source          *reactorLabDeploymentSource    `json:"source,omitempty"`
	ActivatedAt     *time.Time                     `json:"activatedAt,omitempty"`
	ImageID         string                         `json:"imageId,omitempty"`
	Services        []reactorLabDeploymentService  `json:"services"`
	Databases       []reactorLabDeploymentDatabase `json:"databases"`
	LocalRepository localRepositoryEvidence        `json:"localRepository"`
}

type reactorLabAppListResult struct {
	Apps      []reactorLabAppSummary `json:"apps"`
	TotalApps int                    `json:"totalApps"`
	Truncated bool                   `json:"truncated"`
}

type reactorLabContextOverview struct {
	Available     bool                                            `json:"available"`
	Error         string                                          `json:"error,omitempty"`
	CollectedAt   *time.Time                                      `json:"collectedAt,omitempty"`
	System        reactorLabSection[reactorLabSystemSnapshot]     `json:"system"`
	Recovery      reactorLabSection[reactorLabRecovery]           `json:"recovery"`
	Observability reactorLabSection[reactorLabObservabilityState] `json:"observability"`
}

type reactorLabDatabaseLookupFailure struct {
	DatabaseID string `json:"databaseId"`
	Error      string `json:"error"`
}

type reactorLabAppContextResult struct {
	App                      string                            `json:"app"`
	SourceAvailable          bool                              `json:"sourceAvailable"`
	Deployment               reactorLabDeployment              `json:"deployment"`
	Databases                []reactorLabDatabase              `json:"databases"`
	DatabaseLookupsRequested int                               `json:"databaseLookupsRequested"`
	DatabaseLookupsTruncated bool                              `json:"databaseLookupsTruncated"`
	DatabaseLookupFailures   []reactorLabDatabaseLookupFailure `json:"databaseLookupFailures"`
	Overview                 reactorLabContextOverview         `json:"overview"`
	LocalRepository          localRepositoryEvidence           `json:"localRepository"`
}

type reactorLabResolvedApplicationHistory struct {
	Application reactorLabApplicationSummary `json:"application"`
	History     reactorLabApplicationHistory `json:"history"`
}

func executeGetPlatformOverviewCapability(
	ctx context.Context,
	a *app,
	_ map[string]any,
) (any, string, error) {
	out, err := a.reactorLabReads().Overview(ctx)
	if err != nil {
		return nil, "", err
	}
	return out, "read current platform overview with source availability", nil
}

func executeReadHostHistoryCapability(
	ctx context.Context,
	a *app,
	args map[string]any,
) (any, string, error) {
	window, err := reactorLabWindowFromArgs(args)
	if err != nil {
		return nil, "", err
	}
	out, err := a.reactorLabReads().HostHistory(ctx, window)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("read %d bounded host metric points", len(out.Points)), nil
}

func executeReadTemperatureHistoryCapability(
	ctx context.Context,
	a *app,
	args map[string]any,
) (any, string, error) {
	window, err := reactorLabWindowFromArgs(args)
	if err != nil {
		return nil, "", err
	}
	out, err := a.reactorLabReads().TemperatureHistory(ctx, window)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("read %d bounded temperature points", len(out.Points)), nil
}

func executeReadApplicationHistoryCapability(
	ctx context.Context,
	a *app,
	args map[string]any,
) (any, string, error) {
	requested, err := strictOptionalStringArg(args, "app")
	if err != nil {
		return nil, "", err
	}
	if !validReactorLabText(requested, 128, false) {
		return nil, "", errReactorLabInvalidRequest
	}
	window, err := reactorLabWindowFromArgs(args)
	if err != nil {
		return nil, "", err
	}
	client := a.reactorLabReads()
	applications, err := client.Applications(ctx, window)
	if err != nil {
		return nil, "", err
	}
	matches := make([]reactorLabApplicationSummary, 0, 1)
	seen := map[string]bool{}
	for _, application := range applications.Applications {
		if !strings.EqualFold(application.ID, requested) &&
			!strings.EqualFold(application.Name, requested) {

			continue
		}
		if !seen[application.ID] {
			seen[application.ID] = true
			matches = append(matches, application)
		}
	}
	if len(matches) == 0 {
		return nil, "", errReactorLabApplicationNotFound
	}
	if len(matches) != 1 {
		return nil, "", errReactorLabApplicationAmbiguous
	}
	history, err := client.ApplicationHistory(
		ctx,
		matches[0].ID,
		window,
	)
	if err != nil {
		return nil, "", err
	}
	return reactorLabResolvedApplicationHistory{
		Application: matches[0],
		History:     history,
	}, fmt.Sprintf(
		"resolved %s to %s and read %d bounded application points",
		requested,
		matches[0].ID,
		len(history.Points),
	), nil
}

func executeReadServiceHistoryCapability(
	ctx context.Context,
	a *app,
	args map[string]any,
) (any, string, error) {
	requested, err := strictOptionalStringArg(args, "service")
	if err != nil {
		return nil, "", err
	}
	if requested != "" &&
		!validReactorLabText(requested, 256, false) {

		return nil, "", errReactorLabInvalidRequest
	}
	window, err := reactorLabWindowFromArgs(args)
	if err != nil {
		return nil, "", err
	}
	out, err := a.reactorLabReads().ServiceHistory(ctx, window)
	if err != nil {
		return nil, "", err
	}
	if requested == "" {
		return out, fmt.Sprintf(
			"read %d bounded service series",
			len(out.Services),
		), nil
	}
	matches := make([]reactorLabServiceSeries, 0, 1)
	seen := map[string]bool{}
	for _, service := range out.Services {
		if !strings.EqualFold(service.ID, requested) &&
			!strings.EqualFold(service.Name, requested) {

			continue
		}
		if !seen[service.ID] {
			seen[service.ID] = true
			matches = append(matches, service)
		}
	}
	if len(matches) == 0 {
		return nil, "", errReactorLabServiceNotFound
	}
	if len(matches) != 1 {
		return nil, "", errReactorLabServiceAmbiguous
	}
	out.Services = matches
	out.TotalServices = 1
	return out, "read one safely matched bounded service series", nil
}

func executeReadInfrastructureEventsCapability(
	ctx context.Context,
	a *app,
	args map[string]any,
) (any, string, error) {
	window, err := reactorLabWindowFromArgs(args)
	if err != nil {
		return nil, "", err
	}
	limit, err := strictBoundedIntArg(
		args,
		"limit",
		100,
		reactorLabMaxEvents,
	)
	if err != nil {
		return nil, "", err
	}
	out, err := a.reactorLabReads().Events(ctx, window, limit)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("read %d bounded infrastructure events", len(out.Events)), nil
}

func executeListDatabasesCapability(
	ctx context.Context,
	a *app,
	_ map[string]any,
) (any, string, error) {
	out, err := a.reactorLabReads().Databases(ctx)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("listed %d current databases", len(out.Databases)), nil
}

func executeReadDatabaseBackupsCapability(
	ctx context.Context,
	a *app,
	args map[string]any,
) (any, string, error) {
	databaseID, err := strictOptionalStringArg(args, "database_id")
	if err != nil {
		return nil, "", err
	}
	limit, err := strictBoundedIntArg(
		args,
		"limit",
		100,
		reactorLabMaxBackups,
	)
	if err != nil {
		return nil, "", err
	}
	out, err := a.reactorLabReads().Backups(
		ctx,
		databaseID,
		limit,
	)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("read %d bounded database backups", len(out.Backups)), nil
}

func executeReadActivityCapability(
	ctx context.Context,
	a *app,
	args map[string]any,
) (any, string, error) {
	limit, err := strictBoundedIntArg(
		args,
		"limit",
		100,
		reactorLabMaxActivity,
	)
	if err != nil {
		return nil, "", err
	}
	out, err := a.reactorLabReads().Activity(ctx, limit)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("read %d bounded recent Activity events", len(out.Events)), nil
}

func executeReadRecoveryCapability(
	ctx context.Context,
	a *app,
	_ map[string]any,
) (any, string, error) {
	out, err := a.reactorLabReads().Recovery(ctx)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf(
		"read recovery protection and %d recent incidents",
		len(out.RecentIncidents),
	), nil
}

func (a *app) readReactorLabAppContext(
	ctx context.Context,
	requested string,
) (reactorLabAppContextResult, error) {
	client := a.reactorLabReads()
	canonical, err := canonicalReactorLabAppName(
		ctx,
		client,
		requested,
	)
	if err != nil {
		return reactorLabAppContextResult{}, err
	}
	deployment, err := client.Deployment(ctx, canonical)
	if err != nil {
		return reactorLabAppContextResult{}, err
	}
	out := reactorLabAppContextResult{
		App:                    canonical,
		SourceAvailable:        deployment.Source != nil,
		Deployment:             deployment,
		Databases:              []reactorLabDatabase{},
		DatabaseLookupFailures: []reactorLabDatabaseLookupFailure{},
		LocalRepository: localRepositoryEvidenceFrom(
			a.readRepoContext(canonical),
		),
	}

	overview, overviewErr := client.Overview(ctx)
	if overviewErr != nil {
		out.Overview = reactorLabContextOverview{
			Available: false,
			Error:     overviewErr.Error(),
		}
	} else {
		collectedAt := overview.CollectedAt
		out.Overview = reactorLabContextOverview{
			Available:     true,
			CollectedAt:   &collectedAt,
			System:        overview.System,
			Recovery:      overview.Recovery,
			Observability: overview.Observability,
		}
	}

	seen := map[string]bool{}
	databaseIDs := make([]string, 0, len(deployment.Databases))
	for _, database := range deployment.Databases {
		if !seen[database.ID] {
			seen[database.ID] = true
			databaseIDs = append(databaseIDs, database.ID)
		}
	}
	out.DatabaseLookupsRequested = len(databaseIDs)
	if len(databaseIDs) > reactorLabMaxDatabaseLookups {
		databaseIDs = databaseIDs[:reactorLabMaxDatabaseLookups]
		out.DatabaseLookupsTruncated = true
	}
	for _, databaseID := range databaseIDs {
		database, databaseErr := client.Database(ctx, databaseID)
		if databaseErr != nil {
			out.DatabaseLookupFailures = append(
				out.DatabaseLookupFailures,
				reactorLabDatabaseLookupFailure{
					DatabaseID: databaseID,
					Error:      databaseErr.Error(),
				},
			)
			continue
		}
		out.Databases = append(out.Databases, database)
	}
	return out, nil
}

func canonicalReactorLabAppName(
	ctx context.Context,
	client reactorLabReadClient,
	requested string,
) (string, error) {
	requested = strings.TrimSpace(requested)
	if reactorLabAppIDPattern.MatchString(requested) {
		return requested, nil
	}
	if !safeAppName(requested) {
		return "", errReactorLabInvalidRequest
	}
	deployments, err := client.Deployments(ctx)
	if err != nil {
		return "", err
	}
	matches := []string{}
	for _, deployment := range deployments.Deployments {
		if strings.EqualFold(deployment.App, requested) {
			matches = append(matches, deployment.App)
		}
	}
	if len(matches) == 0 {
		return "", errReactorLabNotFound
	}
	if len(matches) != 1 {
		return "", errReactorLabInvalidRequest
	}
	return matches[0], nil
}

func localRepositoryEvidenceFrom(
	value repoContext,
) localRepositoryEvidence {
	return localRepositoryEvidence{
		Path:   value.Path,
		Exists: value.Exists,
		Branch: value.Branch,
		Commit: value.Commit,
	}
}

func reactorLabWindowFromArgs(
	args map[string]any,
) (reactorLabWindowRequest, error) {
	rangeValue, err := strictOptionalStringArg(args, "range")
	if err != nil {
		return reactorLabWindowRequest{}, err
	}
	fromValue, err := strictOptionalStringArg(args, "from")
	if err != nil {
		return reactorLabWindowRequest{}, err
	}
	toValue, err := strictOptionalStringArg(args, "to")
	if err != nil {
		return reactorLabWindowRequest{}, err
	}
	window := reactorLabWindowRequest{
		Range: rangeValue,
		From:  fromValue,
		To:    toValue,
	}
	if _, err := reactorLabWindowValues(window); err != nil {
		return reactorLabWindowRequest{}, err
	}
	return window, nil
}

func strictOptionalStringArg(
	args map[string]any,
	key string,
) (string, error) {
	if args == nil {
		return "", nil
	}
	raw, present := args[key]
	if !present {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", errReactorLabInvalidRequest
	}
	return strings.TrimSpace(value), nil
}

func strictBoundedIntArg(
	args map[string]any,
	key string,
	fallback int,
	maximum int,
) (int, error) {
	if args == nil {
		return fallback, nil
	}
	raw, present := args[key]
	if !present {
		return fallback, nil
	}
	value := 0
	switch typed := raw.(type) {
	case int:
		value = typed
	case float64:
		if math.Trunc(typed) != typed {
			return 0, errReactorLabInvalidRequest
		}
		value = int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil || int64(int(parsed)) != parsed {
			return 0, errReactorLabInvalidRequest
		}
		value = int(parsed)
	default:
		return 0, errReactorLabInvalidRequest
	}
	if value < 1 || value > maximum {
		return 0, errReactorLabInvalidRequest
	}
	return value, nil
}

func compactReactorLabToolResult(
	name string,
	value any,
) (any, bool) {
	switch typed := value.(type) {
	case reactorLabOverview:
		if name != "get_platform_overview" {
			return nil, false
		}
		return compactReactorLabOverview(typed), true
	case reactorLabAppListResult:
		if len(typed.Apps) > 8 {
			if typed.TotalApps < len(typed.Apps) {
				typed.TotalApps = len(typed.Apps)
			}
			typed.Apps = typed.Apps[:8]
			typed.Truncated = true
		}
		return typed, true
	case reactorLabAppContextResult:
		typed.Overview = compactReactorLabContextOverview(
			typed.Overview,
		)
		return typed, true
	case reactorLabHostHistory:
		compactNewest(&typed.Points, 6, &typed.Truncated)
		return typed, true
	case reactorLabTemperatureHistory:
		compactNewest(&typed.Points, 16, &typed.Truncated)
		return typed, true
	case reactorLabResolvedApplicationHistory:
		compactNewest(
			&typed.History.Points,
			10,
			&typed.History.Truncated,
		)
		return typed, true
	case reactorLabServiceHistory:
		return compactReactorLabServices(typed, 6, 8), true
	case reactorLabEvents:
		if len(typed.Events) > 12 {
			typed.TotalEvents = len(typed.Events)
			typed.Events = typed.Events[:12]
			typed.Truncated = true
		}
		return typed, true
	case reactorLabDatabaseList:
		if len(typed.Databases) > 8 {
			typed.TotalDatabases = len(typed.Databases)
			typed.Databases = typed.Databases[:8]
			typed.Truncated = true
		}
		return typed, true
	case reactorLabBackupList:
		if len(typed.Backups) > 12 {
			typed.TotalBackups = len(typed.Backups)
			typed.Backups = typed.Backups[:12]
			typed.Truncated = true
		}
		return typed, true
	case reactorLabActivity:
		if len(typed.Events) > 12 {
			typed.TotalEvents = len(typed.Events)
			typed.Events = typed.Events[:12]
			typed.Truncated = true
		}
		return typed, true
	case reactorLabRecovery:
		if len(typed.RecentIncidents) > 8 {
			typed.RecentIncidents = typed.RecentIncidents[:8]
		}
		return typed, true
	case reactorLabDeploymentHistory:
		if len(typed.Versions) > 4 {
			typed.TotalVersions = len(typed.Versions)
			typed.Versions = typed.Versions[:4]
			typed.Truncated = true
		}
		for index := range typed.Versions {
			if len(typed.Versions[index].Services) > 2 {
				typed.Versions[index].Services =
					typed.Versions[index].Services[:2]
				typed.Truncated = true
			}
		}
		return typed, true
	default:
		return nil, false
	}
}

func compactReactorLabOverview(
	value reactorLabOverview,
) reactorLabOverview {
	if value.System.Data != nil {
		data := *value.System.Data
		if len(data.Services) > 8 {
			data.Services = data.Services[:8]
		}
		value.System.Data = &data
	}
	if value.Recovery.Data != nil {
		data := *value.Recovery.Data
		if len(data.RecentIncidents) > 4 {
			data.RecentIncidents = data.RecentIncidents[:4]
		}
		value.Recovery.Data = &data
	}
	if value.Deployments.Data != nil {
		data := *value.Deployments.Data
		if len(data.Deployments) > 4 {
			data.TotalDeployments = len(data.Deployments)
			data.Deployments = data.Deployments[:4]
			data.Truncated = true
		}
		value.Deployments.Data = &data
	}
	if value.Databases.Data != nil {
		data := *value.Databases.Data
		if len(data.Databases) > 4 {
			data.TotalDatabases = len(data.Databases)
			data.Databases = data.Databases[:4]
			data.Truncated = true
		}
		value.Databases.Data = &data
	}
	if value.Observability.Data != nil {
		data := *value.Observability.Data
		if len(data.Applications) > 4 {
			data.Applications = data.Applications[:4]
		}
		services := reactorLabServiceHistory{
			Services:      data.Services,
			TotalServices: len(data.Services),
		}
		services = compactReactorLabServices(services, 4, 2)
		data.Services = services.Services
		value.Observability.Data = &data
	}
	return value
}

func compactReactorLabContextOverview(
	value reactorLabContextOverview,
) reactorLabContextOverview {
	if !value.Available {
		return value
	}
	overview := reactorLabOverview{
		System:        value.System,
		Recovery:      value.Recovery,
		Observability: value.Observability,
	}
	overview = compactReactorLabOverview(overview)
	value.System = overview.System
	value.Recovery = overview.Recovery
	value.Observability = overview.Observability
	return value
}

func compactReactorLabServices(
	value reactorLabServiceHistory,
	maximumServices int,
	maximumPoints int,
) reactorLabServiceHistory {
	if len(value.Services) > maximumServices {
		value.TotalServices = len(value.Services)
		value.Services = value.Services[:maximumServices]
		value.Truncated = true
	}
	for index := range value.Services {
		if len(value.Services[index].Points) > maximumPoints {
			value.Services[index].Points =
				value.Services[index].Points[len(value.Services[index].Points)-maximumPoints:]
			value.Truncated = true
		}
	}
	return value
}

func compactNewest[T any](
	values *[]T,
	maximum int,
	truncated *bool,
) {
	if len(*values) <= maximum {
		return
	}
	*values = (*values)[len(*values)-maximum:]
	*truncated = true
}

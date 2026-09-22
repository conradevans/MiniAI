package main

import (
	"regexp"
	"strings"
	"time"
	"unicode"
)

var (
	reactorLabAppIDPattern = regexp.MustCompile(
		`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`,
	)
	reactorLabDatabaseIDPattern = regexp.MustCompile(
		`^database_[0-9a-f]{32}$`,
	)
	reactorLabBackupIDPattern = regexp.MustCompile(
		`^backup_[0-9a-f]{32}$`,
	)
	reactorLabResourceIDPattern = regexp.MustCompile(
		`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`,
	)
	reactorLabGitObjectPattern = regexp.MustCompile(
		`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`,
	)
	reactorLabImageIDPattern = regexp.MustCompile(
		`^sha256:[0-9a-f]{64}$`,
	)
	reactorLabRepositoryPattern = regexp.MustCompile(
		`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`,
	)
	reactorLabProviderPattern = regexp.MustCompile(
		`^[a-z0-9.-]{1,64}$`,
	)
	reactorLabErrorCodePattern = regexp.MustCompile(
		`^[a-z0-9_]{1,64}$`,
	)
)

func validateReactorLabOverview(value *reactorLabOverview) error {
	if value == nil || !normalizeReactorLabTime(&value.CollectedAt) {
		return errReactorLabMalformed
	}
	if err := validateReactorLabSection(
		&value.System,
		validateReactorLabSystem,
	); err != nil {
		return err
	}
	if err := validateReactorLabSection(
		&value.Recovery,
		validateReactorLabRecovery,
	); err != nil {
		return err
	}
	if err := validateReactorLabSection(
		&value.Deployments,
		validateReactorLabDeployments,
	); err != nil {
		return err
	}
	if err := validateReactorLabSection(
		&value.Databases,
		validateReactorLabDatabases,
	); err != nil {
		return err
	}
	return validateReactorLabSection(
		&value.Observability,
		validateReactorLabObservabilityState,
	)
}

func validateReactorLabSection[T any](
	section *reactorLabSection[T],
	validate func(*T) error,
) error {
	if section.Available {
		if section.Data == nil || section.Error != "" {
			return errReactorLabMalformed
		}
		return validate(section.Data)
	}
	if section.Data != nil ||
		!reactorLabErrorCodePattern.MatchString(section.Error) {

		return errReactorLabMalformed
	}
	return nil
}

func validateReactorLabSystem(value *reactorLabSystemSnapshot) error {
	if value == nil || !normalizeReactorLabTime(&value.CollectedAt) ||
		value.CPU.LogicalCores < 1 || value.UptimeSeconds < 0 ||
		value.Services == nil || len(value.Services) > 100 ||
		!validReactorLabText(value.Temperature.Source, 128, true) ||
		!validReactorLabText(value.Network.Interface, 128, true) ||
		!validReactorLabText(value.Battery.Status, 128, true) {

		return errReactorLabMalformed
	}
	for _, service := range value.Services {
		if !validReactorLabText(service.Name, 128, false) ||
			!validReactorLabText(service.Status, 128, false) {

			return errReactorLabMalformed
		}
	}
	return nil
}

func validateReactorLabRecovery(value *reactorLabRecovery) error {
	if value == nil ||
		!validReactorLabText(value.Protection.State, 64, false) ||
		!validReactorLabText(
			value.Protection.HardwareWatchdog.State,
			64,
			false,
		) ||
		!validReactorLabText(value.Protection.RTC.State, 64, false) ||
		!validReactorLabText(
			value.Protection.HardwareWatchdog.Identity,
			128,
			true,
		) ||
		value.RecentIncidents == nil ||
		len(value.RecentIncidents) > 20 {

		return errReactorLabMalformed
	}
	if value.Protection.RTC.WakeAt != nil &&
		!normalizeReactorLabTime(value.Protection.RTC.WakeAt) {

		return errReactorLabMalformed
	}
	if value.LastIncident != nil {
		if err := validateReactorLabRecoveryIncident(
			value.LastIncident,
		); err != nil {
			return err
		}
	}
	for index := range value.RecentIncidents {
		if err := validateReactorLabRecoveryIncident(
			&value.RecentIncidents[index],
		); err != nil {
			return err
		}
		if index > 0 &&
			value.RecentIncidents[index].RecoveredAt.After(
				value.RecentIncidents[index-1].RecoveredAt,
			) {

			return errReactorLabMalformed
		}
	}
	return nil
}

func validateReactorLabRecoveryIncident(
	value *reactorLabRecoveryIncident,
) error {
	if value == nil ||
		!validReactorLabText(value.EventID, 128, false) ||
		!normalizeReactorLabTime(&value.LastKnownAliveAt) ||
		!normalizeReactorLabTime(&value.RecoveredAt) ||
		value.RecoveredAt.Before(value.LastKnownAliveAt) ||
		value.DowntimeSeconds < 0 ||
		!validReactorLabText(value.Status, 64, false) {

		return errReactorLabMalformed
	}
	return nil
}

func validateReactorLabDeployments(
	value *reactorLabDeploymentList,
) error {
	if value == nil || value.Deployments == nil ||
		len(value.Deployments) > 500 {

		return errReactorLabMalformed
	}
	for index := range value.Deployments {
		if err := validateReactorLabDeployment(
			&value.Deployments[index],
		); err != nil {
			return err
		}
	}
	value.TotalDeployments = len(value.Deployments)
	return nil
}

func validateReactorLabDeployment(
	value *reactorLabDeployment,
) error {
	if value == nil ||
		!reactorLabAppIDPattern.MatchString(value.App) ||
		!validReactorLabText(value.Strategy, 128, false) ||
		!validReactorLabText(value.Status, 128, false) ||
		value.Services == nil || len(value.Services) > 20 ||
		value.Databases == nil || len(value.Databases) > 20 ||
		!validReactorLabImageID(value.ImageID) {

		return errReactorLabMalformed
	}
	if value.Source != nil {
		if err := validateReactorLabDeploymentSource(
			value.Source,
		); err != nil {
			return err
		}
	}
	if value.ActivatedAt != nil &&
		!normalizeReactorLabTime(value.ActivatedAt) {

		return errReactorLabMalformed
	}
	for _, service := range value.Services {
		if !validReactorLabText(service.Name, 128, false) ||
			!validReactorLabText(service.Strategy, 128, true) ||
			!validReactorLabText(service.Status, 128, true) ||
			!validReactorLabImageID(service.ImageID) {

			return errReactorLabMalformed
		}
	}
	for _, database := range value.Databases {
		if !reactorLabDatabaseIDPattern.MatchString(database.ID) ||
			!validReactorLabText(database.DisplayName, 256, true) ||
			!validReactorLabText(database.BindingName, 64, false) {

			return errReactorLabMalformed
		}
	}
	return nil
}

func validateReactorLabDeploymentSource(
	value *reactorLabDeploymentSource,
) error {
	if value == nil ||
		!reactorLabGitObjectPattern.MatchString(value.CommitSHA) ||
		!validReactorLabText(value.Branch, 1024, true) ||
		!validReactorLabText(value.RequestedRef, 1024, true) {

		return errReactorLabMalformed
	}
	if value.Provider == "" && value.Repository == "" {
		return nil
	}
	if !reactorLabProviderPattern.MatchString(value.Provider) ||
		!reactorLabRepositoryPattern.MatchString(value.Repository) {

		return errReactorLabMalformed
	}
	return nil
}

func validateReactorLabDeploymentHistory(
	value *reactorLabDeploymentHistory,
	appName string,
) error {
	if value == nil || value.App != appName ||
		value.Versions == nil || len(value.Versions) > 100 {

		return errReactorLabMalformed
	}
	for index := range value.Versions {
		version := &value.Versions[index]
		if version.App != appName ||
			!validReactorLabText(version.Strategy, 128, true) ||
			!normalizeReactorLabTime(&version.ArchivedAt) ||
			!validReactorLabImageID(version.ImageID) ||
			version.Services == nil || len(version.Services) > 20 {

			return errReactorLabMalformed
		}
		if version.Source != nil {
			if err := validateReactorLabDeploymentSource(
				version.Source,
			); err != nil {
				return err
			}
		}
		if version.ActivatedAt != nil &&
			!normalizeReactorLabTime(version.ActivatedAt) {

			return errReactorLabMalformed
		}
		for _, service := range version.Services {
			if !validReactorLabText(service.Name, 128, false) ||
				!validReactorLabText(service.Strategy, 128, true) ||
				!validReactorLabText(service.Status, 128, true) ||
				!validReactorLabImageID(service.ImageID) {

				return errReactorLabMalformed
			}
		}
	}
	value.TotalVersions = len(value.Versions)
	return nil
}

func validateReactorLabDatabases(value *reactorLabDatabaseList) error {
	if value == nil || !normalizeReactorLabTime(&value.CollectedAt) ||
		value.Databases == nil || len(value.Databases) > 500 {

		return errReactorLabMalformed
	}
	for index := range value.Databases {
		if err := validateReactorLabDatabase(
			&value.Databases[index],
		); err != nil {
			return err
		}
	}
	value.TotalDatabases = len(value.Databases)
	return nil
}

func validateReactorLabDatabase(value *reactorLabDatabase) error {
	if value == nil ||
		!reactorLabDatabaseIDPattern.MatchString(value.ID) ||
		!validReactorLabText(value.DisplayName, 256, false) ||
		!validReactorLabText(value.Status, 64, false) ||
		value.SizeBytes < 0 || value.Connections < 0 ||
		value.ActiveConnections < 0 || value.IdleConnections < 0 ||
		value.Commits < 0 || value.Rollbacks < 0 ||
		value.BlockReads < 0 || value.BlockHits < 0 ||
		value.RowsInserted < 0 || value.RowsUpdated < 0 ||
		value.RowsDeleted < 0 || value.BackupCount < 0 ||
		value.BackupBytes < 0 || value.Deployments == nil ||
		len(value.Deployments) > 20 {

		return errReactorLabMalformed
	}
	if value.LatestBackupAt != nil &&
		!normalizeReactorLabTime(value.LatestBackupAt) {

		return errReactorLabMalformed
	}
	if value.BackupAgeSeconds != nil &&
		*value.BackupAgeSeconds < 0 {

		return errReactorLabMalformed
	}
	for _, deployment := range value.Deployments {
		if !reactorLabAppIDPattern.MatchString(deployment.App) ||
			!validReactorLabText(deployment.BindingName, 64, false) {

			return errReactorLabMalformed
		}
	}
	return nil
}

func validateReactorLabBackups(
	value *reactorLabBackupList,
	databaseID string,
	requestedLimit int,
) error {
	if value == nil || value.DatabaseID != databaseID ||
		value.Limit != requestedLimit || value.Backups == nil ||
		len(value.Backups) > requestedLimit ||
		len(value.Backups) > reactorLabMaxBackups {

		return errReactorLabMalformed
	}
	for index := range value.Backups {
		backup := &value.Backups[index]
		if !reactorLabBackupIDPattern.MatchString(backup.ID) ||
			backup.DatabaseID != databaseID ||
			!validReactorLabBackupKind(backup.Kind) ||
			!validReactorLabBackupLifecycle(backup) {

			return errReactorLabMalformed
		}
		if index > 0 &&
			backup.CreatedAt.After(value.Backups[index-1].CreatedAt) {

			return errReactorLabMalformed
		}
	}
	value.TotalBackups = len(value.Backups)
	return nil
}

func validReactorLabBackupKind(value string) bool {
	return value == "manual" || value == "automatic" ||
		value == "pre_restore"
}

func validReactorLabBackupLifecycle(
	value *reactorLabBackup,
) bool {
	if value == nil || !normalizeReactorLabTime(&value.CreatedAt) {
		return false
	}
	if value.CompletedAt != nil &&
		!normalizeReactorLabTime(value.CompletedAt) {

		return false
	}
	switch value.Status {
	case "creating":
		return value.SizeBytes == 0 && value.CompletedAt == nil
	case "ready":
		return value.SizeBytes > 0 && value.CompletedAt != nil &&
			!value.CompletedAt.Before(value.CreatedAt)
	case "error":
		return value.SizeBytes == 0 && value.CompletedAt != nil &&
			!value.CompletedAt.Before(value.CreatedAt)
	default:
		return false
	}
}

func validateReactorLabWindow(value *reactorLabWindow) error {
	if value == nil ||
		!normalizeReactorLabTime(&value.From) ||
		!normalizeReactorLabTime(&value.To) ||
		!value.From.Before(value.To) ||
		value.To.Sub(value.From) > 7*24*time.Hour ||
		value.BucketSeconds <= 0 ||
		value.MaxPoints < 1 ||
		value.MaxPoints > reactorLabMaxMetricPoints ||
		(value.Range != "" && !validReactorLabRange(value.Range)) {

		return errReactorLabMalformed
	}
	return nil
}

func validateReactorLabHostHistory(
	value *reactorLabHostHistory,
) error {
	if value == nil || value.Points == nil ||
		len(value.Points) > reactorLabMaxMetricPoints {

		return errReactorLabMalformed
	}
	if err := validateReactorLabWindow(&value.Window); err != nil {
		return err
	}
	for index := range value.Points {
		point := &value.Points[index]
		if !normalizeReactorLabTime(&point.Timestamp) ||
			point.SampleCount < 1 {

			return errReactorLabMalformed
		}
	}
	value.TotalPoints = len(value.Points)
	return nil
}

func validateReactorLabTemperatureHistory(
	value *reactorLabTemperatureHistory,
) error {
	if value == nil || value.Points == nil ||
		len(value.Points) > reactorLabMaxMetricPoints {

		return errReactorLabMalformed
	}
	if err := validateReactorLabWindow(&value.Window); err != nil {
		return err
	}
	for index := range value.Points {
		point := &value.Points[index]
		if !normalizeReactorLabTime(&point.BucketStart) ||
			!normalizeReactorLabTime(&point.BucketEnd) ||
			!normalizeReactorLabTime(&point.PeakAt) ||
			!point.BucketStart.Before(point.BucketEnd) ||
			point.SampleCount < 1 ||
			point.MinCelsius > point.AvgCelsius ||
			point.AvgCelsius > point.MaxCelsius {

			return errReactorLabMalformed
		}
	}
	value.TotalPoints = len(value.Points)
	return nil
}

func validateReactorLabApplications(
	value *reactorLabApplications,
) error {
	if value == nil || value.Applications == nil ||
		len(value.Applications) > 500 {

		return errReactorLabMalformed
	}
	if err := validateReactorLabWindow(&value.Window); err != nil {
		return err
	}
	for index := range value.Applications {
		if err := validateReactorLabApplicationSummary(
			&value.Applications[index],
		); err != nil {
			return err
		}
	}
	return nil
}

func validateReactorLabApplicationSummary(
	value *reactorLabApplicationSummary,
) error {
	if value == nil ||
		!reactorLabResourceIDPattern.MatchString(value.ID) ||
		!validReactorLabText(value.Name, 256, false) ||
		!validReactorLabText(value.LatestStatus, 128, false) ||
		value.RestartCount < 0 ||
		!normalizeReactorLabTime(&value.LastObservedAt) {

		return errReactorLabMalformed
	}
	return nil
}

func validateReactorLabApplicationHistory(
	value *reactorLabApplicationHistory,
	applicationID string,
) error {
	if value == nil || value.ID != applicationID ||
		value.Points == nil ||
		len(value.Points) > reactorLabMaxMetricPoints {

		return errReactorLabMalformed
	}
	if err := validateReactorLabWindow(&value.Window); err != nil {
		return err
	}
	for index := range value.Points {
		point := &value.Points[index]
		if !normalizeReactorLabTime(&point.Timestamp) ||
			point.SampleCount < 1 || point.RestartCount < 0 ||
			!validReactorLabText(point.Status, 128, false) {

			return errReactorLabMalformed
		}
	}
	value.TotalPoints = len(value.Points)
	return nil
}

func validateReactorLabServiceHistory(
	value *reactorLabServiceHistory,
) error {
	if value == nil || value.Services == nil ||
		len(value.Services) > 500 {

		return errReactorLabMalformed
	}
	if err := validateReactorLabWindow(&value.Window); err != nil {
		return err
	}
	for index := range value.Services {
		if err := validateReactorLabServiceSeries(
			&value.Services[index],
		); err != nil {
			return err
		}
	}
	value.TotalServices = len(value.Services)
	return nil
}

func validateReactorLabServiceSeries(
	value *reactorLabServiceSeries,
) error {
	if value == nil ||
		!reactorLabResourceIDPattern.MatchString(value.ID) ||
		!validReactorLabText(value.Name, 256, false) ||
		value.Points == nil ||
		len(value.Points) > reactorLabMaxMetricPoints {

		return errReactorLabMalformed
	}
	for index := range value.Points {
		point := &value.Points[index]
		if !normalizeReactorLabTime(&point.Timestamp) ||
			point.SampleCount < 1 ||
			!validReactorLabText(point.Status, 128, false) {

			return errReactorLabMalformed
		}
	}
	return nil
}

func validateReactorLabEvents(
	value *reactorLabEvents,
	requestedLimit int,
) error {
	if value == nil || value.Limit != requestedLimit ||
		value.Events == nil || len(value.Events) > requestedLimit ||
		len(value.Events) > reactorLabMaxEvents {

		return errReactorLabMalformed
	}
	if err := validateReactorLabWindow(&value.Window); err != nil {
		return err
	}
	for index := range value.Events {
		event := &value.Events[index]
		if !validReactorLabText(event.ID, 128, false) ||
			!validReactorLabText(event.Source, 128, false) ||
			!validReactorLabText(event.Type, 128, false) ||
			!validReactorLabText(event.ResourceType, 128, false) ||
			!validReactorLabText(event.ResourceID, 128, true) ||
			!validReactorLabText(event.ResourceName, 256, true) ||
			!validReactorLabText(event.Summary, 1024, false) ||
			!normalizeReactorLabTime(&event.OccurredAt) {

			return errReactorLabMalformed
		}
		if index > 0 &&
			event.OccurredAt.After(value.Events[index-1].OccurredAt) {

			return errReactorLabMalformed
		}
	}
	value.TotalEvents = len(value.Events)
	return nil
}

func validateReactorLabObservabilityState(
	value *reactorLabObservabilityState,
) error {
	if value == nil || value.Applications == nil ||
		len(value.Applications) > 500 ||
		value.Services == nil || len(value.Services) > 500 {

		return errReactorLabMalformed
	}
	for index := range value.Applications {
		if err := validateReactorLabApplicationSummary(
			&value.Applications[index],
		); err != nil {
			return err
		}
	}
	for index := range value.Services {
		if err := validateReactorLabServiceSeries(
			&value.Services[index],
		); err != nil {
			return err
		}
	}
	return nil
}

func validateReactorLabActivity(
	value *reactorLabActivity,
	requestedLimit int,
) error {
	if value == nil || value.Limit != requestedLimit ||
		value.Events == nil || len(value.Events) > requestedLimit ||
		len(value.Events) > reactorLabMaxActivity {

		return errReactorLabMalformed
	}
	for index := range value.Events {
		event := &value.Events[index]
		if !normalizeReactorLabTime(&event.OccurredAt) ||
			!validReactorLabText(event.EventID, 128, true) ||
			!validReactorLabText(event.Source, 128, false) ||
			!validReactorLabText(event.Kind, 128, false) ||
			!validReactorLabText(event.Severity, 64, false) ||
			!validReactorLabText(event.Subject, 256, false) ||
			!validReactorLabText(event.Message, 1024, false) {

			return errReactorLabMalformed
		}
		if event.Incident != nil {
			if !normalizeReactorLabTime(
				&event.Incident.LastKnownAliveAt,
			) ||
				!normalizeReactorLabTime(
					&event.Incident.RecoveredAt,
				) ||
				event.Incident.RecoveredAt.Before(
					event.Incident.LastKnownAliveAt,
				) ||
				event.Incident.DowntimeSeconds < 0 ||
				!validReactorLabText(
					event.Incident.Status,
					64,
					false,
				) {

				return errReactorLabMalformed
			}
		}
		if index > 0 &&
			event.OccurredAt.After(value.Events[index-1].OccurredAt) {

			return errReactorLabMalformed
		}
	}
	value.TotalEvents = len(value.Events)
	return nil
}

func validReactorLabImageID(value string) bool {
	return value == "" || reactorLabImageIDPattern.MatchString(value)
}

func validReactorLabText(
	value string,
	maximum int,
	allowEmpty bool,
) bool {
	if (!allowEmpty && strings.TrimSpace(value) == "") ||
		len(value) > maximum {

		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func normalizeReactorLabTime(value *time.Time) bool {
	if value == nil || value.IsZero() {
		return false
	}
	*value = value.UTC()
	return true
}

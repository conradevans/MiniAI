package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	deploymentHistoryMaxVersions        = 3
	deploymentHistoryMaxServices        = 6
	deploymentHistoryMaxResponseBytes   = 512 * 1024
	deploymentHistoryReadTimeout        = 8 * time.Second
	deploymentHistoryIdentifierMaxRunes = 256
)

var (
	errMalformedDeploymentHistory = errors.New("malformed MiniDeploy deployment history response")
	errMalformedCurrentDeployment = errors.New("malformed MiniDeploy current deployment response")
	errMiniDeployResponseTooLarge = errors.New("MiniDeploy response exceeded the safe limit")
)

// deploymentHistoryToolResponse is deliberately narrower than MiniDeploy's
// rollback record. Repository URLs and any other unnecessary upstream metadata
// are omitted, strings are scrubbed and bounded, and the timestamp is renamed
// to reflect MiniDeploy's actual archive-time semantics.
type deploymentHistoryToolResponse struct {
	App                  string                     `json:"app"`
	Versions             []deploymentHistoryVersion `json:"versions"`
	TotalVersions        int                        `json:"total_versions"`
	Truncated            bool                       `json:"truncated"`
	Redacted             bool                       `json:"redacted"`
	ExactCommitAvailable bool                       `json:"exact_commit_available"`
	TimestampMeaning     string                     `json:"timestamp_meaning"`
	Source               string                     `json:"source"`
}

type deploymentHistoryVersion struct {
	Position             int                        `json:"position"`
	Relation             string                     `json:"relation"`
	Container            string                     `json:"container,omitempty"`
	Image                string                     `json:"image,omitempty"`
	Port                 int                        `json:"port,omitempty"`
	ContainerPort        int                        `json:"container_port,omitempty"`
	HealthPath           string                     `json:"health_path,omitempty"`
	Strategy             string                     `json:"strategy,omitempty"`
	PackageManager       string                     `json:"package_manager,omitempty"`
	PackageInstallMode   string                     `json:"package_install_mode,omitempty"`
	Services             []deploymentHistoryService `json:"services,omitempty"`
	ReactorLabMigration  bool                       `json:"reactorlab_migration,omitempty"`
	ArchivedAt           string                     `json:"archived_at,omitempty"`
	Truncated            bool                       `json:"truncated,omitempty"`
	ServicesTruncated    bool                       `json:"services_truncated,omitempty"`
	IdentifiersTruncated bool                       `json:"identifiers_truncated,omitempty"`
}

type deploymentHistoryService struct {
	Name                string `json:"name,omitempty"`
	Container           string `json:"container,omitempty"`
	Image               string `json:"image,omitempty"`
	ContainerPort       int    `json:"container_port,omitempty"`
	HealthPath          string `json:"health_path,omitempty"`
	Strategy            string `json:"strategy,omitempty"`
	PackageManager      string `json:"package_manager,omitempty"`
	PackageInstallMode  string `json:"package_install_mode,omitempty"`
	ReactorLabMigration bool   `json:"reactorlab_migration,omitempty"`
}

type miniDeployHistoryResponse struct {
	App      string          `json:"app"`
	Versions json.RawMessage `json:"versions"`
}

type miniDeployHistoryVersion struct {
	App                 string                     `json:"app"`
	Container           string                     `json:"container"`
	Image               string                     `json:"image"`
	Port                int                        `json:"port"`
	ContainerPort       int                        `json:"containerPort"`
	HealthPath          string                     `json:"healthPath"`
	Strategy            string                     `json:"strategy"`
	PackageManager      string                     `json:"packageManager"`
	PackageInstallMode  string                     `json:"packageInstallMode"`
	Services            []miniDeployHistoryService `json:"services"`
	ReactorLabMigration bool                       `json:"reactorlabMigration"`
	DeployedAt          time.Time                  `json:"deployedAt"`
}

type miniDeployHistoryService struct {
	Name                string `json:"name"`
	Container           string `json:"container"`
	Image               string `json:"image"`
	ContainerPort       int    `json:"containerPort"`
	HealthPath          string `json:"healthPath"`
	Strategy            string `json:"strategy"`
	PackageManager      string `json:"packageManager"`
	PackageInstallMode  string `json:"packageInstallMode"`
	ReactorLabMigration bool   `json:"reactorlabMigration"`
}

type currentDeploymentMetadata struct {
	App                  string                     `json:"app"`
	Container            string                     `json:"container,omitempty"`
	Image                string                     `json:"image,omitempty"`
	Port                 int                        `json:"port,omitempty"`
	ContainerPort        int                        `json:"container_port,omitempty"`
	Strategy             string                     `json:"strategy,omitempty"`
	Services             []deploymentHistoryService `json:"services,omitempty"`
	ServicesTruncated    bool                       `json:"services_truncated,omitempty"`
	IdentifiersTruncated bool                       `json:"identifiers_truncated,omitempty"`
	Truncated            bool                       `json:"truncated,omitempty"`
	Redacted             bool                       `json:"redacted,omitempty"`
	Source               string                     `json:"source"`
}

type miniDeployHTTPStatusError struct {
	StatusCode int
	Status     string
}

func (e miniDeployHTTPStatusError) Error() string {
	return "MiniDeploy returned " + e.Status
}

func (a *app) handleDeploymentHistory(w http.ResponseWriter, r *http.Request) {
	out, err := a.readMiniDeployDeploymentHistory(r.Context(), strings.TrimSpace(r.PathValue("app")))
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			writeError(w, http.StatusNotFound, "resource not found")
		case errors.Is(err, errRepoForbidden):
			writeError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, context.DeadlineExceeded):
			writeError(w, http.StatusGatewayTimeout, "deployment history request timed out")
		default:
			writeError(w, http.StatusBadGateway, "deployment history unavailable")
		}
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) readMiniDeployDeploymentHistory(ctx context.Context, appName string) (deploymentHistoryToolResponse, error) {
	if !safeAppName(appName) {
		return deploymentHistoryToolResponse{}, errRepoForbidden
	}
	current, err := a.readMiniDeployCurrentDeployment(ctx, appName)
	if err != nil {
		return deploymentHistoryToolResponse{}, err
	}
	return a.readMiniDeployDeploymentHistoryCanonical(ctx, current.App)
}

func (a *app) readMiniDeployDeploymentHistoryCanonical(ctx context.Context, appName string) (deploymentHistoryToolResponse, error) {
	if !safeAppName(appName) {
		return deploymentHistoryToolResponse{}, errRepoForbidden
	}
	data, err := a.readBoundedMiniDeployGET(
		ctx,
		"/deployments/"+url.PathEscape(appName)+"/history",
		deploymentHistoryMaxResponseBytes,
	)
	if err != nil {
		var statusErr miniDeployHTTPStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
			return deploymentHistoryToolResponse{}, os.ErrNotExist
		}
		if errors.Is(err, errMiniDeployResponseTooLarge) {
			return deploymentHistoryToolResponse{}, errMalformedDeploymentHistory
		}
		return deploymentHistoryToolResponse{}, err
	}
	return decodeDeploymentHistory(appName, data)
}

func (a *app) readMiniDeployCurrentDeployment(ctx context.Context, appName string) (currentDeploymentMetadata, error) {
	if !safeAppName(appName) {
		return currentDeploymentMetadata{}, errRepoForbidden
	}
	data, err := a.readBoundedMiniDeployGET(ctx, "/deployments", deploymentHistoryMaxResponseBytes)
	if err != nil {
		if errors.Is(err, errMiniDeployResponseTooLarge) {
			return currentDeploymentMetadata{}, errMalformedCurrentDeployment
		}
		return currentDeploymentMetadata{}, err
	}
	return decodeCurrentDeployment(appName, data)
}

func (a *app) readBoundedMiniDeployGET(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, deploymentHistoryReadTimeout)
	defer cancel()

	endpoint := strings.TrimRight(a.minideployURL, "/") + path
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, miniDeployHTTPStatusError{StatusCode: resp.StatusCode, Status: resp.Status}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errMiniDeployResponseTooLarge
	}
	return data, nil
}

func decodeCurrentDeployment(appName string, data []byte) (currentDeploymentMetadata, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var upstream []miniDeployHistoryVersion
	if err := decoder.Decode(&upstream); err != nil || upstream == nil {
		return currentDeploymentMetadata{}, errMalformedCurrentDeployment
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return currentDeploymentMetadata{}, errMalformedCurrentDeployment
	}
	for _, deployment := range upstream {
		if !strings.EqualFold(deployment.App, appName) {
			continue
		}
		if !safeAppName(deployment.App) {
			return currentDeploymentMetadata{}, errMalformedCurrentDeployment
		}
		sanitized, redacted := sanitizeDeploymentHistoryVersion(deployment, 0)
		return currentDeploymentMetadata{
			App:                  deployment.App,
			Container:            sanitized.Container,
			Image:                sanitized.Image,
			Port:                 sanitized.Port,
			ContainerPort:        sanitized.ContainerPort,
			Strategy:             sanitized.Strategy,
			Services:             sanitized.Services,
			ServicesTruncated:    sanitized.ServicesTruncated,
			IdentifiersTruncated: sanitized.IdentifiersTruncated,
			Truncated:            sanitized.Truncated,
			Redacted:             redacted,
			Source:               "MiniDeploy current deployment API (bounded and scrubbed by MiniAI)",
		}, nil
	}
	return currentDeploymentMetadata{}, os.ErrNotExist
}

func applyCurrentDeploymentMetadata(snapshot *appDiagnosticSnapshot, current currentDeploymentMetadata) {
	snapshot.DeploymentImage = current.Image
	snapshot.DeploymentContainer = current.Container
	if current.Port != 0 {
		snapshot.ListenerPort = current.Port
	}
	snapshot.ContainerPort = current.ContainerPort
	snapshot.DeploymentServices = append([]deploymentHistoryService{}, current.Services...)
	snapshot.DeploymentServicesTruncated = current.ServicesTruncated
	snapshot.DeploymentIdentifiersTruncated = current.IdentifiersTruncated
	snapshot.DeploymentMetadataSource = current.Source
	if current.Strategy != "" {
		snapshot.Strategy = current.Strategy
	}
}

func decodeDeploymentHistory(appName string, data []byte) (deploymentHistoryToolResponse, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var payload miniDeployHistoryResponse
	if err := decoder.Decode(&payload); err != nil {
		return deploymentHistoryToolResponse{}, fmt.Errorf("%w: %v", errMalformedDeploymentHistory, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return deploymentHistoryToolResponse{}, errMalformedDeploymentHistory
	}
	if payload.App == "" || !strings.EqualFold(payload.App, appName) || len(payload.Versions) == 0 || bytes.Equal(bytes.TrimSpace(payload.Versions), []byte("null")) {
		return deploymentHistoryToolResponse{}, errMalformedDeploymentHistory
	}

	var upstream []miniDeployHistoryVersion
	if err := json.Unmarshal(payload.Versions, &upstream); err != nil || upstream == nil {
		return deploymentHistoryToolResponse{}, errMalformedDeploymentHistory
	}

	out := deploymentHistoryToolResponse{
		App:                  appName,
		Versions:             []deploymentHistoryVersion{},
		TotalVersions:        len(upstream),
		Truncated:            len(upstream) > deploymentHistoryMaxVersions,
		ExactCommitAvailable: false,
		TimestampMeaning:     "archived_at is when MiniDeploy recorded the old active version in history during a later cutover or rollback; it is not the original deployment time",
		Source:               "MiniDeploy private deployment-history API (bounded and scrubbed by MiniAI)",
	}

	if len(upstream) > deploymentHistoryMaxVersions {
		upstream = upstream[:deploymentHistoryMaxVersions]
	}
	for index, version := range upstream {
		if version.App != "" && !strings.EqualFold(version.App, appName) {
			return deploymentHistoryToolResponse{}, errMalformedDeploymentHistory
		}
		sanitized, redacted := sanitizeDeploymentHistoryVersion(version, index)
		out.Versions = append(out.Versions, sanitized)
		if sanitized.Truncated {
			out.Truncated = true
		}
		if redacted {
			out.Redacted = true
		}
	}
	return out, nil
}

func sanitizeDeploymentHistoryVersion(version miniDeployHistoryVersion, index int) (deploymentHistoryVersion, bool) {
	out := deploymentHistoryVersion{
		Position:            index,
		Relation:            "older_previous",
		Container:           boundedDeploymentHistoryString(version.Container),
		Image:               boundedDeploymentHistoryString(version.Image),
		Port:                safeNetworkPort(version.Port),
		ContainerPort:       safeNetworkPort(version.ContainerPort),
		HealthPath:          boundedDeploymentHistoryString(version.HealthPath),
		Strategy:            boundedDeploymentHistoryString(version.Strategy),
		PackageManager:      boundedDeploymentHistoryString(version.PackageManager),
		PackageInstallMode:  boundedDeploymentHistoryString(version.PackageInstallMode),
		Services:            []deploymentHistoryService{},
		ReactorLabMigration: version.ReactorLabMigration,
	}
	if index == 0 {
		out.Relation = "immediately_previous"
	}
	if !version.DeployedAt.IsZero() {
		out.ArchivedAt = version.DeployedAt.UTC().Format(time.RFC3339Nano)
	}
	rawValues := []string{
		version.Container, version.Image, version.HealthPath, version.Strategy,
		version.PackageManager, version.PackageInstallMode,
	}
	redacted := deploymentHistoryStringsWereRedacted(rawValues)
	out.IdentifiersTruncated = deploymentHistoryStringsWereTruncated(rawValues)
	out.Truncated = out.IdentifiersTruncated
	services := version.Services
	if len(services) > deploymentHistoryMaxServices {
		services = services[:deploymentHistoryMaxServices]
		out.Truncated = true
		out.ServicesTruncated = true
	}
	for _, service := range services {
		serviceValues := []string{
			service.Name, service.Container, service.Image, service.HealthPath,
			service.Strategy, service.PackageManager, service.PackageInstallMode,
		}
		redacted = redacted || deploymentHistoryStringsWereRedacted(serviceValues)
		serviceIdentifiersTruncated := deploymentHistoryStringsWereTruncated(serviceValues)
		out.IdentifiersTruncated = out.IdentifiersTruncated || serviceIdentifiersTruncated
		out.Truncated = out.Truncated || serviceIdentifiersTruncated
		out.Services = append(out.Services, deploymentHistoryService{
			Name:                boundedDeploymentHistoryString(service.Name),
			Container:           boundedDeploymentHistoryString(service.Container),
			Image:               boundedDeploymentHistoryString(service.Image),
			ContainerPort:       safeNetworkPort(service.ContainerPort),
			HealthPath:          boundedDeploymentHistoryString(service.HealthPath),
			Strategy:            boundedDeploymentHistoryString(service.Strategy),
			PackageManager:      boundedDeploymentHistoryString(service.PackageManager),
			PackageInstallMode:  boundedDeploymentHistoryString(service.PackageInstallMode),
			ReactorLabMigration: service.ReactorLabMigration,
		})
	}
	return out, redacted
}

func boundedDeploymentHistoryString(value string) string {
	value, _ = scrubSensitiveText(strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), " ")
	return truncateDiagnosticRunes(value, deploymentHistoryIdentifierMaxRunes)
}

func deploymentHistoryStringsWereRedacted(values []string) bool {
	for _, value := range values {
		if _, redacted := scrubSensitiveText(value); redacted {
			return true
		}
	}
	return false
}

func deploymentHistoryStringsWereTruncated(values []string) bool {
	for _, value := range values {
		value, _ = scrubSensitiveText(strings.TrimSpace(value))
		value = strings.Join(strings.Fields(value), " ")
		if len([]rune(value)) > deploymentHistoryIdentifierMaxRunes {
			return true
		}
	}
	return false
}

func safeNetworkPort(value int) int {
	if value < 1 || value > 65535 {
		return 0
	}
	return value
}

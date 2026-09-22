package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	reactorLabReadTimeout      = 5 * time.Second
	reactorLabMaxResponseBytes = 2 << 20
	reactorLabMaxMetricPoints  = 240
	reactorLabMaxEvents        = 500
	reactorLabMaxActivity      = 200
	reactorLabMaxBackups       = 200
)

var (
	errReactorLabInvalidRequest = errors.New("reactorlab_read_invalid_request")
	errReactorLabUnavailable    = errors.New("reactorlab_read_unavailable")
	errReactorLabNotFound       = errors.New("reactorlab_read_not_found")
	errReactorLabMalformed      = errors.New("reactorlab_read_malformed")
	errReactorLabTooLarge       = errors.New("reactorlab_read_too_large")
)

type reactorLabSection[T any] struct {
	Available bool   `json:"available"`
	Data      *T     `json:"data,omitempty"`
	Error     string `json:"error,omitempty"`
}

type reactorLabOverview struct {
	CollectedAt   time.Time                                       `json:"collectedAt"`
	System        reactorLabSection[reactorLabSystemSnapshot]     `json:"system"`
	Recovery      reactorLabSection[reactorLabRecovery]           `json:"recovery"`
	Deployments   reactorLabSection[reactorLabDeploymentList]     `json:"deployments"`
	Databases     reactorLabSection[reactorLabDatabaseList]       `json:"databases"`
	Observability reactorLabSection[reactorLabObservabilityState] `json:"observability"`
}

type reactorLabCPUState struct {
	UsagePercent float64 `json:"usagePercent"`
	LogicalCores int     `json:"logicalCores"`
	Load1        float64 `json:"load1"`
	Load5        float64 `json:"load5"`
	Load15       float64 `json:"load15"`
}

type reactorLabMemoryState struct {
	TotalBytes       uint64  `json:"totalBytes"`
	UsedBytes        uint64  `json:"usedBytes"`
	AvailableBytes   uint64  `json:"availableBytes"`
	UsagePercent     float64 `json:"usagePercent"`
	SwapTotalBytes   uint64  `json:"swapTotalBytes"`
	SwapUsedBytes    uint64  `json:"swapUsedBytes"`
	SwapUsagePercent float64 `json:"swapUsagePercent"`
}

type reactorLabDiskState struct {
	TotalBytes     uint64  `json:"totalBytes"`
	UsedBytes      uint64  `json:"usedBytes"`
	AvailableBytes uint64  `json:"availableBytes"`
	UsagePercent   float64 `json:"usagePercent"`
}

type reactorLabTemperatureState struct {
	Celsius float64 `json:"celsius"`
	Source  string  `json:"source"`
}

type reactorLabNetworkState struct {
	Interface     string  `json:"interface"`
	RXBytes       uint64  `json:"rxBytes"`
	TXBytes       uint64  `json:"txBytes"`
	RXBytesPerSec float64 `json:"rxBytesPerSecond"`
	TXBytesPerSec float64 `json:"txBytesPerSecond"`
}

type reactorLabBatteryState struct {
	Available   bool    `json:"available"`
	Percent     float64 `json:"percent"`
	ACAvailable bool    `json:"acAvailable"`
	ACConnected bool    `json:"acConnected"`
	Status      string  `json:"status"`
}

type reactorLabServiceState struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Active bool   `json:"active"`
}

type reactorLabSystemSnapshot struct {
	CPU           reactorLabCPUState         `json:"cpu"`
	Memory        reactorLabMemoryState      `json:"memory"`
	Disk          reactorLabDiskState        `json:"disk"`
	Temperature   reactorLabTemperatureState `json:"temperature"`
	UptimeSeconds float64                    `json:"uptimeSeconds"`
	Network       reactorLabNetworkState     `json:"network"`
	Battery       reactorLabBatteryState     `json:"battery"`
	Services      []reactorLabServiceState   `json:"services"`
	CollectedAt   time.Time                  `json:"collectedAt"`
}

type reactorLabHardwareWatchdog struct {
	State          string  `json:"state"`
	Identity       string  `json:"identity,omitempty"`
	TimeoutSeconds uint32  `json:"timeoutSeconds,omitempty"`
	BootStatus     *uint32 `json:"bootStatus,omitempty"`
}

type reactorLabRTCState struct {
	State  string     `json:"state"`
	WakeAt *time.Time `json:"wakeAt,omitempty"`
}

type reactorLabProtectionState struct {
	State            string                     `json:"state"`
	HardwareWatchdog reactorLabHardwareWatchdog `json:"hardwareWatchdog"`
	RTC              reactorLabRTCState         `json:"rtc"`
}

type reactorLabRecoveryIncident struct {
	EventID          string    `json:"eventId"`
	LastKnownAliveAt time.Time `json:"lastKnownAliveAt"`
	RecoveredAt      time.Time `json:"recoveredAt"`
	DowntimeSeconds  int64     `json:"downtimeSeconds"`
	Status           string    `json:"status"`
}

type reactorLabRecovery struct {
	Protection       reactorLabProtectionState    `json:"protection"`
	HistoryAvailable bool                         `json:"historyAvailable"`
	LastIncident     *reactorLabRecoveryIncident  `json:"lastIncident,omitempty"`
	RecentIncidents  []reactorLabRecoveryIncident `json:"recentIncidents"`
}

type reactorLabDeploymentSource struct {
	Provider     string `json:"provider,omitempty"`
	Repository   string `json:"repository,omitempty"`
	Branch       string `json:"branch,omitempty"`
	RequestedRef string `json:"requestedRef,omitempty"`
	CommitSHA    string `json:"commitSha"`
}

type reactorLabDeploymentService struct {
	Name     string `json:"name"`
	Strategy string `json:"strategy,omitempty"`
	Status   string `json:"status,omitempty"`
	ImageID  string `json:"imageId,omitempty"`
}

type reactorLabDeploymentDatabase struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
	BindingName string `json:"bindingName"`
}

type reactorLabDeployment struct {
	App         string                         `json:"app"`
	Strategy    string                         `json:"strategy"`
	Status      string                         `json:"status"`
	Source      *reactorLabDeploymentSource    `json:"source,omitempty"`
	ActivatedAt *time.Time                     `json:"activatedAt,omitempty"`
	ImageID     string                         `json:"imageId,omitempty"`
	Services    []reactorLabDeploymentService  `json:"services"`
	Databases   []reactorLabDeploymentDatabase `json:"databases"`
}

type reactorLabDeploymentList struct {
	Deployments      []reactorLabDeployment `json:"deployments"`
	TotalDeployments int                    `json:"totalDeployments,omitempty"`
	Truncated        bool                   `json:"truncated,omitempty"`
}

type reactorLabDeploymentVersion struct {
	App         string                        `json:"app"`
	Strategy    string                        `json:"strategy,omitempty"`
	Source      *reactorLabDeploymentSource   `json:"source,omitempty"`
	ActivatedAt *time.Time                    `json:"activatedAt,omitempty"`
	ArchivedAt  time.Time                     `json:"archivedAt"`
	ImageID     string                        `json:"imageId,omitempty"`
	Services    []reactorLabDeploymentService `json:"services"`
}

type reactorLabDeploymentHistory struct {
	App           string                        `json:"app"`
	Versions      []reactorLabDeploymentVersion `json:"versions"`
	TotalVersions int                           `json:"totalVersions,omitempty"`
	Truncated     bool                          `json:"truncated,omitempty"`
}

type reactorLabDatabaseDeployment struct {
	App         string `json:"app"`
	BindingName string `json:"bindingName"`
}

type reactorLabDatabase struct {
	ID                string                         `json:"id"`
	DisplayName       string                         `json:"displayName"`
	Status            string                         `json:"status"`
	SizeBytes         int64                          `json:"sizeBytes"`
	Connections       int64                          `json:"connections"`
	ActiveConnections int64                          `json:"activeConnections"`
	IdleConnections   int64                          `json:"idleConnections"`
	Commits           int64                          `json:"commits"`
	Rollbacks         int64                          `json:"rollbacks"`
	BlockReads        int64                          `json:"blockReads"`
	BlockHits         int64                          `json:"blockHits"`
	RowsInserted      int64                          `json:"rowsInserted"`
	RowsUpdated       int64                          `json:"rowsUpdated"`
	RowsDeleted       int64                          `json:"rowsDeleted"`
	BackupCount       int                            `json:"backupCount"`
	BackupBytes       int64                          `json:"backupBytes"`
	LatestBackupAt    *time.Time                     `json:"latestBackupAt,omitempty"`
	BackupAgeSeconds  *float64                       `json:"backupAgeSeconds,omitempty"`
	Deployments       []reactorLabDatabaseDeployment `json:"deployments"`
}

type reactorLabDatabaseList struct {
	CollectedAt    time.Time            `json:"collectedAt"`
	Databases      []reactorLabDatabase `json:"databases"`
	TotalDatabases int                  `json:"totalDatabases,omitempty"`
	Truncated      bool                 `json:"truncated,omitempty"`
}

type reactorLabBackup struct {
	ID          string     `json:"id"`
	DatabaseID  string     `json:"databaseId"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	SizeBytes   int64      `json:"sizeBytes"`
	CreatedAt   time.Time  `json:"createdAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

type reactorLabBackupList struct {
	DatabaseID   string             `json:"databaseId"`
	Backups      []reactorLabBackup `json:"backups"`
	Limit        int                `json:"limit"`
	Truncated    bool               `json:"truncated"`
	TotalBackups int                `json:"totalBackups,omitempty"`
}

type reactorLabWindow struct {
	Range         string    `json:"range,omitempty"`
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	BucketSeconds float64   `json:"bucketSeconds"`
	MaxPoints     int       `json:"maxPoints"`
}

type reactorLabWindowRequest struct {
	Range string
	From  string
	To    string
}

type reactorLabHostPoint struct {
	Timestamp         time.Time `json:"timestamp"`
	SampleCount       int       `json:"sampleCount"`
	CPUAverage        *float64  `json:"cpuAverage"`
	CPUMaximum        *float64  `json:"cpuMaximum"`
	MemoryUsedAverage float64   `json:"memoryUsedAverage"`
	MemoryUsedMaximum uint64    `json:"memoryUsedMaximum"`
	MemoryTotal       float64   `json:"memoryTotal"`
	Load1Average      float64   `json:"load1Average"`
	Load5Average      float64   `json:"load5Average"`
	Load15Average     float64   `json:"load15Average"`
	DiskUsedAverage   float64   `json:"diskUsedAverage"`
	DiskTotal         float64   `json:"diskTotal"`
	DiskReadAverage   *float64  `json:"diskReadAverage"`
	DiskReadMaximum   *float64  `json:"diskReadMaximum"`
	DiskWriteAverage  *float64  `json:"diskWriteAverage"`
	DiskWriteMaximum  *float64  `json:"diskWriteMaximum"`
	NetworkRXAverage  *float64  `json:"networkRxAverage"`
	NetworkRXMaximum  *float64  `json:"networkRxMaximum"`
	NetworkTXAverage  *float64  `json:"networkTxAverage"`
	NetworkTXMaximum  *float64  `json:"networkTxMaximum"`
}

type reactorLabHostHistory struct {
	Window      reactorLabWindow      `json:"window"`
	Points      []reactorLabHostPoint `json:"points"`
	TotalPoints int                   `json:"totalPoints,omitempty"`
	Truncated   bool                  `json:"truncated,omitempty"`
}

type reactorLabTemperaturePoint struct {
	BucketStart time.Time `json:"bucketStart"`
	BucketEnd   time.Time `json:"bucketEnd"`
	SampleCount int       `json:"sampleCount"`
	MinCelsius  float64   `json:"minCelsius"`
	AvgCelsius  float64   `json:"avgCelsius"`
	MaxCelsius  float64   `json:"maxCelsius"`
	PeakAt      time.Time `json:"peakAt"`
}

type reactorLabTemperatureHistory struct {
	Window      reactorLabWindow             `json:"window"`
	Points      []reactorLabTemperaturePoint `json:"points"`
	TotalPoints int                          `json:"totalPoints,omitempty"`
	Truncated   bool                         `json:"truncated,omitempty"`
}

type reactorLabApplicationSummary struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	LatestStatus   string    `json:"latestStatus"`
	RestartCount   int       `json:"restartCount"`
	LastObservedAt time.Time `json:"lastObservedAt"`
}

type reactorLabApplications struct {
	Window       reactorLabWindow               `json:"window"`
	Applications []reactorLabApplicationSummary `json:"applications"`
}

type reactorLabApplicationPoint struct {
	Timestamp          time.Time `json:"timestamp"`
	SampleCount        int       `json:"sampleCount"`
	CPUAverage         float64   `json:"cpuAverage"`
	CPUMaximum         float64   `json:"cpuMaximum"`
	MemoryUsedAverage  float64   `json:"memoryUsedAverage"`
	MemoryUsedMaximum  uint64    `json:"memoryUsedMaximum"`
	MemoryLimitAverage float64   `json:"memoryLimitAverage"`
	NetworkRXAverage   *float64  `json:"networkRxAverage"`
	NetworkRXMaximum   *float64  `json:"networkRxMaximum"`
	NetworkTXAverage   *float64  `json:"networkTxAverage"`
	NetworkTXMaximum   *float64  `json:"networkTxMaximum"`
	Status             string    `json:"status"`
	RestartCount       int       `json:"restartCount"`
}

type reactorLabApplicationHistory struct {
	Window      reactorLabWindow             `json:"window"`
	ID          string                       `json:"id"`
	Points      []reactorLabApplicationPoint `json:"points"`
	TotalPoints int                          `json:"totalPoints,omitempty"`
	Truncated   bool                         `json:"truncated,omitempty"`
}

type reactorLabServicePoint struct {
	Timestamp   time.Time `json:"timestamp"`
	SampleCount int       `json:"sampleCount"`
	Available   bool      `json:"available"`
	Status      string    `json:"status"`
}

type reactorLabServiceSeries struct {
	ID     string                   `json:"id"`
	Name   string                   `json:"name"`
	Points []reactorLabServicePoint `json:"points"`
}

type reactorLabServiceHistory struct {
	Window        reactorLabWindow          `json:"window"`
	Services      []reactorLabServiceSeries `json:"services"`
	TotalServices int                       `json:"totalServices,omitempty"`
	Truncated     bool                      `json:"truncated,omitempty"`
}

type reactorLabEvent struct {
	ID           string    `json:"id"`
	Source       string    `json:"source"`
	Type         string    `json:"type"`
	ResourceType string    `json:"resourceType"`
	ResourceID   string    `json:"resourceId,omitempty"`
	ResourceName string    `json:"resourceName,omitempty"`
	OccurredAt   time.Time `json:"occurredAt"`
	Summary      string    `json:"summary"`
}

type reactorLabEvents struct {
	Window      reactorLabWindow  `json:"window"`
	Limit       int               `json:"limit"`
	Events      []reactorLabEvent `json:"events"`
	TotalEvents int               `json:"totalEvents,omitempty"`
	Truncated   bool              `json:"truncated,omitempty"`
}

type reactorLabObservabilityState struct {
	Applications []reactorLabApplicationSummary `json:"applications"`
	Services     []reactorLabServiceSeries      `json:"services"`
}

type reactorLabActivityIncident struct {
	LastKnownAliveAt time.Time `json:"lastKnownAliveAt"`
	RecoveredAt      time.Time `json:"recoveredAt"`
	DowntimeSeconds  int64     `json:"downtimeSeconds"`
	Status           string    `json:"status"`
}

type reactorLabActivityEvent struct {
	EventID    string                      `json:"eventId,omitempty"`
	OccurredAt time.Time                   `json:"occurredAt"`
	Source     string                      `json:"source"`
	Kind       string                      `json:"kind"`
	Severity   string                      `json:"severity"`
	Subject    string                      `json:"subject"`
	Message    string                      `json:"message"`
	Incident   *reactorLabActivityIncident `json:"incident,omitempty"`
}

type reactorLabActivity struct {
	Limit       int                       `json:"limit"`
	Events      []reactorLabActivityEvent `json:"events"`
	TotalEvents int                       `json:"totalEvents,omitempty"`
	Truncated   bool                      `json:"truncated,omitempty"`
}

type reactorLabReadClient struct {
	baseURL    string
	httpClient *http.Client
}

func newReactorLabReadClient(baseURL string, httpClient *http.Client) reactorLabReadClient {
	return reactorLabReadClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
	}
}

func (a *app) reactorLabReads() reactorLabReadClient {
	return newReactorLabReadClient(a.reactorURL, a.client)
}

func (c reactorLabReadClient) Overview(ctx context.Context) (reactorLabOverview, error) {
	var out reactorLabOverview
	err := c.get(ctx, "/internal/miniai/v1/overview", nil, &out)
	if err == nil {
		err = validateReactorLabOverview(&out)
	}
	return out, err
}

func (c reactorLabReadClient) Deployments(ctx context.Context) (reactorLabDeploymentList, error) {
	var out reactorLabDeploymentList
	err := c.get(ctx, "/internal/miniai/v1/deployments", nil, &out)
	if err == nil {
		err = validateReactorLabDeployments(&out)
	}
	return out, err
}

func (c reactorLabReadClient) Deployment(ctx context.Context, appName string) (reactorLabDeployment, error) {
	if !reactorLabAppIDPattern.MatchString(appName) {
		return reactorLabDeployment{}, errReactorLabInvalidRequest
	}
	var out reactorLabDeployment
	err := c.get(ctx, "/internal/miniai/v1/deployments/"+url.PathEscape(appName), nil, &out)
	if err == nil {
		err = validateReactorLabDeployment(&out)
		if err == nil && out.App != appName {
			err = errReactorLabMalformed
		}
	}
	return out, err
}

func (c reactorLabReadClient) DeploymentHistory(ctx context.Context, appName string) (reactorLabDeploymentHistory, error) {
	if !reactorLabAppIDPattern.MatchString(appName) {
		return reactorLabDeploymentHistory{}, errReactorLabInvalidRequest
	}
	var out reactorLabDeploymentHistory
	err := c.get(ctx, "/internal/miniai/v1/deployments/"+url.PathEscape(appName)+"/history", nil, &out)
	if err == nil {
		err = validateReactorLabDeploymentHistory(&out, appName)
	}
	return out, err
}

func (c reactorLabReadClient) Databases(ctx context.Context) (reactorLabDatabaseList, error) {
	var out reactorLabDatabaseList
	err := c.get(ctx, "/internal/miniai/v1/databases", nil, &out)
	if err == nil {
		err = validateReactorLabDatabases(&out)
	}
	return out, err
}

func (c reactorLabReadClient) Database(ctx context.Context, databaseID string) (reactorLabDatabase, error) {
	if !reactorLabDatabaseIDPattern.MatchString(databaseID) {
		return reactorLabDatabase{}, errReactorLabInvalidRequest
	}
	var out reactorLabDatabase
	err := c.get(ctx, "/internal/miniai/v1/databases/"+url.PathEscape(databaseID), nil, &out)
	if err == nil {
		err = validateReactorLabDatabase(&out)
		if err == nil && out.ID != databaseID {
			err = errReactorLabMalformed
		}
	}
	return out, err
}

func (c reactorLabReadClient) Backups(ctx context.Context, databaseID string, limit int) (reactorLabBackupList, error) {
	if !reactorLabDatabaseIDPattern.MatchString(databaseID) || limit < 1 || limit > reactorLabMaxBackups {
		return reactorLabBackupList{}, errReactorLabInvalidRequest
	}
	query := url.Values{"limit": []string{jsonInt(limit)}}
	var out reactorLabBackupList
	err := c.get(ctx, "/internal/miniai/v1/databases/"+url.PathEscape(databaseID)+"/backups", query, &out)
	if err == nil {
		err = validateReactorLabBackups(&out, databaseID, limit)
	}
	return out, err
}

func (c reactorLabReadClient) HostHistory(ctx context.Context, window reactorLabWindowRequest) (reactorLabHostHistory, error) {
	query, err := reactorLabWindowValues(window)
	if err != nil {
		return reactorLabHostHistory{}, err
	}
	var out reactorLabHostHistory
	err = c.get(ctx, "/internal/miniai/v1/observability/host", query, &out)
	if err == nil {
		err = validateReactorLabHostHistory(&out)
	}
	return out, err
}

func (c reactorLabReadClient) TemperatureHistory(ctx context.Context, window reactorLabWindowRequest) (reactorLabTemperatureHistory, error) {
	query, err := reactorLabWindowValues(window)
	if err != nil {
		return reactorLabTemperatureHistory{}, err
	}
	var out reactorLabTemperatureHistory
	err = c.get(ctx, "/internal/miniai/v1/observability/temperature", query, &out)
	if err == nil {
		err = validateReactorLabTemperatureHistory(&out)
	}
	return out, err
}

func (c reactorLabReadClient) Applications(ctx context.Context, window reactorLabWindowRequest) (reactorLabApplications, error) {
	query, err := reactorLabWindowValues(window)
	if err != nil {
		return reactorLabApplications{}, err
	}
	var out reactorLabApplications
	err = c.get(ctx, "/internal/miniai/v1/observability/applications", query, &out)
	if err == nil {
		err = validateReactorLabApplications(&out)
	}
	return out, err
}

func (c reactorLabReadClient) ApplicationHistory(ctx context.Context, applicationID string, window reactorLabWindowRequest) (reactorLabApplicationHistory, error) {
	if !reactorLabResourceIDPattern.MatchString(applicationID) {
		return reactorLabApplicationHistory{}, errReactorLabInvalidRequest
	}
	query, err := reactorLabWindowValues(window)
	if err != nil {
		return reactorLabApplicationHistory{}, err
	}
	var out reactorLabApplicationHistory
	err = c.get(ctx, "/internal/miniai/v1/observability/applications/"+url.PathEscape(applicationID), query, &out)
	if err == nil {
		err = validateReactorLabApplicationHistory(&out, applicationID)
	}
	return out, err
}

func (c reactorLabReadClient) ServiceHistory(ctx context.Context, window reactorLabWindowRequest) (reactorLabServiceHistory, error) {
	query, err := reactorLabWindowValues(window)
	if err != nil {
		return reactorLabServiceHistory{}, err
	}
	var out reactorLabServiceHistory
	err = c.get(ctx, "/internal/miniai/v1/observability/services", query, &out)
	if err == nil {
		err = validateReactorLabServiceHistory(&out)
	}
	return out, err
}

func (c reactorLabReadClient) Events(ctx context.Context, window reactorLabWindowRequest, limit int) (reactorLabEvents, error) {
	if limit < 1 || limit > reactorLabMaxEvents {
		return reactorLabEvents{}, errReactorLabInvalidRequest
	}
	query, err := reactorLabWindowValues(window)
	if err != nil {
		return reactorLabEvents{}, err
	}
	query.Set("limit", jsonInt(limit))
	var out reactorLabEvents
	err = c.get(ctx, "/internal/miniai/v1/observability/events", query, &out)
	if err == nil {
		err = validateReactorLabEvents(&out, limit)
	}
	return out, err
}

func (c reactorLabReadClient) Activity(ctx context.Context, limit int) (reactorLabActivity, error) {
	if limit < 1 || limit > reactorLabMaxActivity {
		return reactorLabActivity{}, errReactorLabInvalidRequest
	}
	query := url.Values{"limit": []string{jsonInt(limit)}}
	var out reactorLabActivity
	err := c.get(ctx, "/internal/miniai/v1/activity", query, &out)
	if err == nil {
		err = validateReactorLabActivity(&out, limit)
	}
	return out, err
}

func (c reactorLabReadClient) Recovery(ctx context.Context) (reactorLabRecovery, error) {
	var out reactorLabRecovery
	err := c.get(ctx, "/internal/miniai/v1/recovery", nil, &out)
	if err == nil {
		err = validateReactorLabRecovery(&out)
	}
	return out, err
}

func (c reactorLabReadClient) get(ctx context.Context, path string, query url.Values, target any) error {
	if c.baseURL == "" || !strings.HasPrefix(path, "/internal/miniai/v1/") {
		return errReactorLabInvalidRequest
	}
	requestContext, cancel := context.WithTimeout(ctx, reactorLabReadTimeout)
	defer cancel()

	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint, nil)
	if err != nil {
		return errReactorLabInvalidRequest
	}
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(request)
	if err != nil {
		if requestContext.Err() != nil {
			return errReactorLabUnavailable
		}
		return errReactorLabUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return errReactorLabNotFound
	}
	if response.StatusCode/100 != 2 {
		return errReactorLabUnavailable
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, reactorLabMaxResponseBytes+1))
	if err != nil {
		return errReactorLabUnavailable
	}
	if len(payload) > reactorLabMaxResponseBytes {
		return errReactorLabTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(target); err != nil {
		return errReactorLabMalformed
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errReactorLabMalformed
	}
	return nil
}

func reactorLabWindowValues(window reactorLabWindowRequest) (url.Values, error) {
	rangeName := strings.TrimSpace(window.Range)
	fromValue := strings.TrimSpace(window.From)
	toValue := strings.TrimSpace(window.To)
	if rangeName != "" {
		if fromValue != "" || toValue != "" || !validReactorLabRange(rangeName) {
			return nil, errReactorLabInvalidRequest
		}
		return url.Values{"range": []string{rangeName}}, nil
	}
	if fromValue == "" && toValue == "" {
		return url.Values{}, nil
	}
	if fromValue == "" || toValue == "" {
		return nil, errReactorLabInvalidRequest
	}
	from, fromErr := time.Parse(time.RFC3339, fromValue)
	to, toErr := time.Parse(time.RFC3339, toValue)
	if fromErr != nil || toErr != nil || !from.Before(to) ||
		to.Sub(from) > 7*24*time.Hour {

		return nil, errReactorLabInvalidRequest
	}
	return url.Values{"from": []string{fromValue}, "to": []string{toValue}}, nil
}

func validReactorLabRange(value string) bool {
	switch value {
	case "15m", "1h", "6h", "24h", "7d":
		return true
	default:
		return false
	}
}

func jsonInt(value int) string {
	return strconv.Itoa(value)
}

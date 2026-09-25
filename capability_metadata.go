package main

// CapabilityMetadata adds semantic planning information to existing reads.
// It does not change tool schemas, handlers, authorization, or execution.
type CapabilityMetadata struct {
	Families       []CapabilityFamily `json:"families"`
	EvidenceTypes  []EvidenceType     `json:"evidenceTypes"`
	ConcurrentSafe bool               `json:"concurrentSafe"`
	ConcurrencyKey string             `json:"concurrencyKey,omitempty"`
	CostUnits      int                `json:"costUnits"`
	MaxFanout      int                `json:"maxFanout"`
}

func phase2CapabilityMetadata(name string) CapabilityMetadata {
	type metadataDefinition struct {
		families       []CapabilityFamily
		evidenceTypes  []EvidenceType
		concurrencyKey string
		costUnits      int
		maxFanout      int
	}
	definitions := map[string]metadataDefinition{
		"get_platform_overview": {
			[]CapabilityFamily{CapabilityFamilyPlatformCurrent},
			[]EvidenceType{EvidenceTypeCurrentPlatformState},
			"reactorlab", 1, 1,
		},
		"list_apps": {
			[]CapabilityFamily{CapabilityFamilyDeploymentCurrent},
			[]EvidenceType{EvidenceTypeCurrentDeploymentList, EvidenceTypeCurrentDeployment, EvidenceTypeCurrentApplication},
			"minideploy", 1, 1,
		},
		"get_app_context": {
			[]CapabilityFamily{CapabilityFamilyDeploymentCurrent, CapabilityFamilyDatabaseState, CapabilityFamilyPlatformCurrent},
			[]EvidenceType{EvidenceTypeCurrentApplication, EvidenceTypeCurrentDeployment, EvidenceTypeCurrentDatabase, EvidenceTypeCurrentPlatformState},
			"app_context", 7, 7,
		},
		"read_host_history": {
			[]CapabilityFamily{CapabilityFamilyHostHistory},
			[]EvidenceType{EvidenceTypeHostHistory},
			"reactorlab", 1, 1,
		},
		"read_temperature_history": {
			[]CapabilityFamily{CapabilityFamilyHostHistory},
			[]EvidenceType{EvidenceTypeThermalHistory},
			"reactorlab", 1, 1,
		},
		"read_application_history": {
			[]CapabilityFamily{CapabilityFamilyApplicationHistory},
			[]EvidenceType{EvidenceTypeApplicationHistory},
			"reactorlab", 2, 2,
		},
		"read_service_history": {
			[]CapabilityFamily{CapabilityFamilyApplicationHistory},
			[]EvidenceType{EvidenceTypeServiceHistory},
			"reactorlab", 1, 1,
		},
		"read_infrastructure_events": {
			[]CapabilityFamily{CapabilityFamilyInfrastructureTimeline},
			[]EvidenceType{EvidenceTypeInfrastructureEvents},
			"reactorlab", 1, 1,
		},
		"list_databases": {
			[]CapabilityFamily{CapabilityFamilyDatabaseState},
			[]EvidenceType{EvidenceTypeCurrentDatabase},
			"reactorlab", 1, 1,
		},
		"read_database_backups": {
			[]CapabilityFamily{CapabilityFamilyDatabaseState},
			[]EvidenceType{EvidenceTypeDatabaseBackups},
			"reactorlab", 1, 1,
		},
		"read_activity": {
			[]CapabilityFamily{CapabilityFamilyInfrastructureTimeline},
			[]EvidenceType{EvidenceTypeActivityTimeline},
			"reactorlab", 1, 1,
		},
		"read_recovery": {
			[]CapabilityFamily{CapabilityFamilyInfrastructureTimeline},
			[]EvidenceType{EvidenceTypeRecoveryTimeline},
			"reactorlab", 1, 1,
		},
		"list_repository_directory": {
			[]CapabilityFamily{CapabilityFamilySourceRepository},
			[]EvidenceType{EvidenceTypeRepositoryInventory},
			"repository", 1, 1,
		},
		"search_repository": {
			[]CapabilityFamily{CapabilityFamilySourceRepository},
			[]EvidenceType{EvidenceTypeRepositorySearch},
			"repository", 1, 1,
		},
		"read_repository_file": {
			[]CapabilityFamily{CapabilityFamilySourceRepository},
			[]EvidenceType{EvidenceTypeRepositoryContent},
			"repository", 1, 1,
		},
		"read_runtime_logs": {
			[]CapabilityFamily{CapabilityFamilyRuntimeEvidence},
			[]EvidenceType{EvidenceTypeRuntimeFailures},
			"minideploy", 1, 1,
		},
		"read_deployment_logs": {
			[]CapabilityFamily{CapabilityFamilyDeploymentHistory, CapabilityFamilyRuntimeEvidence},
			[]EvidenceType{EvidenceTypeDeploymentFailures},
			"minideploy", 1, 1,
		},
		"read_deployment_history": {
			[]CapabilityFamily{CapabilityFamilyDeploymentHistory},
			[]EvidenceType{EvidenceTypeDeploymentTimeline},
			"reactorlab", 1, 1,
		},
	}
	definition, ok := definitions[name]
	if !ok {
		return CapabilityMetadata{}
	}
	return CapabilityMetadata{
		Families:       append([]CapabilityFamily(nil), definition.families...),
		EvidenceTypes:  append([]EvidenceType(nil), definition.evidenceTypes...),
		ConcurrentSafe: true,
		ConcurrencyKey: definition.concurrencyKey,
		CostUnits:      definition.costUnits,
		MaxFanout:      definition.maxFanout,
	}
}

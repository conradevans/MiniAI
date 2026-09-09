package main

import "strings"

const (
	inferenceProfileDefault   = "default"
	inferenceProfileBalanced  = "balanced"
	inferenceProfileCool      = "cool"
	inferenceProfileUltraCool = "ultra_cool"
)

type ollamaInferenceProfile struct {
	Name      string
	NumThread int
	NumBatch  int
}

func configured8BInferenceProfile(value string, logf func(string, ...any)) ollamaInferenceProfile {
	profile, valid := parse8BInferenceProfile(value)
	if !valid && logf != nil {
		logf("MiniAI: invalid MINIAI_8B_PROFILE; using cool")
	}
	return profile
}

func parse8BInferenceProfile(value string) (ollamaInferenceProfile, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", inferenceProfileCool:
		return ollamaInferenceProfile{Name: inferenceProfileCool, NumThread: 4, NumBatch: 64}, true
	case inferenceProfileBalanced:
		return ollamaInferenceProfile{Name: inferenceProfileBalanced, NumThread: 6, NumBatch: 128}, true
	case inferenceProfileUltraCool:
		return ollamaInferenceProfile{Name: inferenceProfileUltraCool, NumThread: 3, NumBatch: 32}, true
	case inferenceProfileDefault:
		return ollamaInferenceProfile{Name: inferenceProfileDefault}, true
	default:
		return ollamaInferenceProfile{Name: inferenceProfileCool, NumThread: 4, NumBatch: 64}, false
	}
}

func (a *app) selected8BInferenceProfile() ollamaInferenceProfile {
	if a != nil {
		if profile, valid := parse8BInferenceProfile(a.eightBProfile.Name); valid && a.eightBProfile.Name != "" {
			return profile
		}
	}
	profile, _ := parse8BInferenceProfile(inferenceProfileCool)
	return profile
}

func (a *app) ollamaRequestOptions(model string, options map[string]any) map[string]any {
	if !sameModel(model, primaryModel) {
		return options
	}
	profile := a.selected8BInferenceProfile()
	if profile.NumThread > 0 {
		options["num_thread"] = profile.NumThread
	}
	if profile.NumBatch > 0 {
		options["num_batch"] = profile.NumBatch
	}
	return options
}

func (a *app) withInferenceProfileMetadata(model string, metadata map[string]any) map[string]any {
	if !sameModel(model, primaryModel) {
		return metadata
	}
	profile := a.selected8BInferenceProfile()
	metadata["inference_profile"] = profile.Name
	if profile.NumThread > 0 {
		metadata["num_thread"] = profile.NumThread
	}
	if profile.NumBatch > 0 {
		metadata["num_batch"] = profile.NumBatch
	}
	if a.cpuPowerLimited() {
		metadata["cpu_power_limited"] = true
		metadata["cpu_power_profile"] = cpuPowerProfileName
	}
	return metadata
}

func log8BInferenceProfile(profile ollamaInferenceProfile, logf func(string, ...any)) {
	if logf == nil {
		return
	}
	if profile.NumThread == 0 && profile.NumBatch == 0 {
		logf("MiniAI Ollama request profile: model=%s inference_profile=%s num_thread=omitted num_batch=omitted", primaryModel, profile.Name)
		return
	}
	logf("MiniAI Ollama request profile: model=%s inference_profile=%s num_thread=%d num_batch=%d", primaryModel, profile.Name, profile.NumThread, profile.NumBatch)
}

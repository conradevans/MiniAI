package main

import (
	"context"
	"sort"
	"strings"
)

type conversationSubject struct {
	App       string
	Source    string
	Ambiguous bool
}

func (a *app) resolveConversationSubject(ctx context.Context, current string, history []storedMessage) conversationSubject {
	deployments, err := a.fetchDeployments(ctx)
	if err != nil {
		return conversationSubject{}
	}
	return resolveConversationSubjectFromDeployments(current, history, deployments)
}

func resolveConversationSubjectFromDeployments(current string, history []storedMessage, deployments []map[string]any) conversationSubject {
	currentApps := mentionedAppNamesFromDeployments(current, deployments)

	switch len(currentApps) {
	case 1:
		return conversationSubject{
			App:    currentApps[0],
			Source: "current_message",
		}
	case 0:
		// Continue into conversation history.
	default:
		return conversationSubject{
			Source:    "current_message",
			Ambiguous: true,
		}
	}

	for i := len(history) - 1; i >= 0; i-- {
		message := history[i]

		if message.Role == "assistant" {
			apps := evidenceAppsForMessage(message, deployments)

			switch len(apps) {
			case 1:
				return conversationSubject{
					App:    apps[0],
					Source: "assistant_evidence",
				}
			case 0:
				// No structured subject evidence on this message.
			default:
				return conversationSubject{
					Source:    "assistant_evidence",
					Ambiguous: true,
				}
			}
		}

		// Text fallback only trusts prior user turns. Assistant prose is not
		// authoritative subject state.
		if message.Role != "user" {
			continue
		}

		apps := mentionedAppNamesFromDeployments(message.Content, deployments)

		switch len(apps) {
		case 1:
			return conversationSubject{
				App:    apps[0],
				Source: "prior_user_message",
			}
		case 0:
			continue
		default:
			return conversationSubject{
				Source:    "prior_user_message",
				Ambiguous: true,
			}
		}
	}

	return conversationSubject{}
}

func evidenceAppsForMessage(message storedMessage, deployments []map[string]any) []string {
	seen := map[string]bool{}
	apps := []string{}

	for _, item := range message.Evidence {
		name := canonicalDeploymentAppName(item.App, deployments)
		if name == "" {
			continue
		}

		key := strings.ToLower(name)
		if seen[key] {
			continue
		}

		seen[key] = true
		apps = append(apps, name)
	}

	sort.Slice(apps, func(i, j int) bool {
		return strings.ToLower(apps[i]) < strings.ToLower(apps[j])
	})

	return apps
}

func canonicalDeploymentAppName(candidate string, deployments []map[string]any) string {
	key := normalizeMatch(candidate)
	if key == "" {
		return ""
	}

	for _, deployment := range deployments {
		name, _ := deployment["app"].(string)
		if name == "" {
			continue
		}

		if normalizeMatch(name) == key {
			return name
		}
	}

	return ""
}

func mentionedAppNamesFromDeployments(message string, deployments []map[string]any) []string {
	normalized := normalizeMatch(message)

	type candidate struct {
		name string
		key  string
	}

	candidates := []candidate{}

	for _, deployment := range deployments {
		name, _ := deployment["app"].(string)
		if name == "" {
			continue
		}

		key := normalizeMatch(name)
		if key == "" || !strings.Contains(normalized, key) {
			continue
		}

		candidates = append(candidates, candidate{
			name: name,
			key:  key,
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return len(candidates[i].key) > len(candidates[j].key)
	})

	seen := map[string]bool{}
	names := []string{}

	for _, candidate := range candidates {
		key := strings.ToLower(candidate.name)
		if seen[key] {
			continue
		}

		seen[key] = true
		names = append(names, candidate.name)
	}

	return names
}

// resolveConversationContexts uses conversation history only to resolve app
// identity. Infrastructure state is always collected again through
// resolveAppContext for the current turn.
func (a *app) resolveConversationContexts(ctx context.Context, current string, history []storedMessage) ([]appContext, []string) {
	subject := a.resolveConversationSubject(ctx, current, history)

	if subject.App != "" && !subject.Ambiguous {
		resolved, err := a.resolveAppContext(ctx, subject.App)
		if err == nil {
			return []appContext{resolved}, []string{resolved.App}
		}
	}

	// Preserve explicit current-message multi-app questions such as comparisons.
	// Ambiguous historical state does not guess because a pronoun-only current
	// message resolves no app names here.
	return a.resolveMentionedContexts(ctx, current)
}

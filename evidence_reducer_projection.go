package main

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode/utf8"
)

// collapseServiceSeries merges duplicate service identities before transition
// selection. Point-level canonicalization later removes repeated series data.
func collapseServiceSeries(values []reactorLabServiceSeries) []reactorLabServiceSeries {
	ordered := append([]reactorLabServiceSeries(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return canonicalJSON(ordered[i]) < canonicalJSON(ordered[j]) })
	byIdentity := make(map[string]int, len(ordered))
	out := make([]reactorLabServiceSeries, 0, len(ordered))
	for _, service := range ordered {
		identity := strings.ToLower(strings.TrimSpace(service.ID))
		if identity == "" {
			identity = strings.ToLower(strings.TrimSpace(service.Name))
		}
		if identity == "" {
			identity = canonicalJSON(service.Points)
		}
		if index, ok := byIdentity[identity]; ok {
			out[index].Points = append(out[index].Points, service.Points...)
			continue
		}
		copy := service
		copy.Points = append([]reactorLabServicePoint(nil), service.Points...)
		byIdentity[identity] = len(out)
		out = append(out, copy)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return canonicalJSON(out[i]) < canonicalJSON(out[j])
	})
	return out
}

// selectFirstAndLast retains early incident onset plus the most recent state
// changes. Input must already be in deterministic chronological order.
func selectFirstAndLast[T any](values []T, limit int) ([]T, int) {
	if len(values) <= limit {
		return append([]T(nil), values...), 0
	}
	if limit <= 0 {
		return nil, len(values)
	}
	head := limit / 2
	tail := limit - head
	selected := make([]T, 0, limit)
	selected = append(selected, values[:head]...)
	selected = append(selected, values[len(values)-tail:]...)
	return selected, len(values) - len(selected)
}

func truncateIdentityRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	retained := limit - 1
	head := retained / 2
	tail := retained - head
	return string(runes[:head]) + "…" + string(runes[len(runes)-tail:])
}

func compactServiceIdentity(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.ValidString(value) && len(value) <= limit && utf8.RuneCountInString(value) <= limit {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	fingerprint := "#" + hex.EncodeToString(sum[:4])
	if len(fingerprint) >= limit {
		return fingerprint[:limit]
	}
	prefixBudget := limit - len(fingerprint)
	prefix := make([]byte, 0, prefixBudget)
	for _, current := range value {
		width := utf8.RuneLen(current)
		if width < 1 || len(prefix)+width > prefixBudget {
			break
		}
		prefix = utf8.AppendRune(prefix, current)
	}
	return string(prefix) + fingerprint
}

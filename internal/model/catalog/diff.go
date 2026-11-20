package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"
)

type Diff struct {
	Before    Sources
	After     Sources
	Added     []string
	Removed   []string
	Changed   []string
	Defaults  []string
	Providers []string
}

// Compare expects validated documents. Upstream index changes are not model changes.
func Compare(before, after Document) Diff {
	result := Diff{Before: before.Sources, After: after.Sources}
	flatten := func(doc Document) map[string]json.RawMessage {
		models := map[string]json.RawMessage{}
		for _, p := range doc.Providers {
			for _, entries := range p.Models {
				for _, raw := range entries {
					var identity struct {
						ID string `json:"id"`
					}
					_ = json.Unmarshal(raw, &identity)
					models[p.ID+"/"+identity.ID] = raw
				}
			}
		}
		return models
	}
	previous, next := flatten(before), flatten(after)
	for key, raw := range next {
		old, exists := previous[key]
		if !exists {
			result.Added = append(result.Added, key)
		} else if !equalJSON(old, raw) {
			result.Changed = append(result.Changed, key)
		}
	}
	for key := range previous {
		if _, exists := next[key]; !exists {
			result.Removed = append(result.Removed, key)
		}
	}
	oldDefaults := map[string]string{}
	for _, value := range before.Defaults {
		oldDefaults[value.Provider] = value.ModelID
	}
	for _, value := range after.Defaults {
		if oldDefaults[value.Provider] != value.ModelID {
			result.Defaults = append(result.Defaults, fmt.Sprintf("%s: %s -> %s", value.Provider, displayValue(oldDefaults[value.Provider]), value.ModelID))
		}
		delete(oldDefaults, value.Provider)
	}
	for provider, model := range oldDefaults {
		result.Defaults = append(result.Defaults, provider+": "+model+" -> (none)")
	}
	if len(result.Defaults) == 0 && !reflect.DeepEqual(before.Defaults, after.Defaults) {
		result.Defaults = append(result.Defaults, "provider preference order changed")
	}
	previousProviders := map[string]Provider{}
	for _, p := range before.Providers {
		previousProviders[p.ID] = p
	}
	for _, p := range after.Providers {
		old, exists := previousProviders[p.ID]
		if !exists || old.API != p.API || old.BaseURL != p.BaseURL {
			result.Providers = append(result.Providers, p.ID)
		}
		delete(previousProviders, p.ID)
	}
	for id := range previousProviders {
		result.Providers = append(result.Providers, id)
	}
	for _, list := range [][]string{result.Added, result.Removed, result.Changed, result.Defaults, result.Providers} {
		sort.Strings(list)
	}
	return result
}

func equalJSON(left, right json.RawMessage) bool {
	decode := func(raw json.RawMessage) any {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		_ = decoder.Decode(&value)
		return value
	}
	return reflect.DeepEqual(decode(left), decode(right))
}

func displayValue(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}

type SyncResult struct {
	Published      bool   `json:"published"`
	Summary        string `json:"summary"`
	FullOutputPath string `json:"fullOutputPath,omitempty"`
}

func (r SyncResult) String() string {
	if r.FullOutputPath != "" {
		return r.Summary + "[Output truncated. Full output: " + r.FullOutputPath + "]\n"
	}
	return r.Summary
}

const maxSummaryBytes = 50 * 1024

// Report preserves the complete diff before the caller replaces the old catalog.
// A failed log write can therefore be retried without losing the original changes.
func (d Diff) Report() (SyncResult, error) {
	summary, truncated := d.format(10)
	if len(summary) > maxSummaryBytes {
		end := maxSummaryBytes - 1
		for !utf8.RuneStart(summary[end]) {
			end--
		}
		summary, truncated = summary[:end]+"\n", true
	}
	result := SyncResult{Summary: summary}
	if !truncated {
		return result, nil
	}
	file, err := os.CreateTemp("", "pi-models-update-*.log")
	if err == nil {
		full, _ := d.format(-1)
		_, writeErr := file.WriteString(full)
		err = errors.Join(writeErr, file.Close())
	}
	if err != nil {
		return SyncResult{}, fmt.Errorf("save complete model update report: %w", err)
	}
	result.FullOutputPath = file.Name()
	return result, nil
}

func (d Diff) format(maxPerGroup int) (string, bool) {
	var text strings.Builder
	var truncated bool
	revision := func(source Source) string { return sourceRevision(source, maxPerGroup >= 0) }
	fmt.Fprintf(&text, "%s: %s -> %s\n", d.After.Models.Name, revision(d.Before.Models), revision(d.After.Models))
	if d.Before.Policy != d.After.Policy {
		fmt.Fprintf(&text, "Local policy: %s -> %s\n", revision(d.Before.Policy), revision(d.After.Policy))
	}
	fmt.Fprintf(&text, "Models: +%d / -%d / ~%d; defaults: %d; provider metadata: %d\n", len(d.Added), len(d.Removed), len(d.Changed), len(d.Defaults), len(d.Providers))
	for _, group := range []struct {
		label  string
		values []string
	}{{"+", d.Added}, {"-", d.Removed}, {"~", d.Changed}, {"default", d.Defaults}, {"provider", d.Providers}} {
		for i, value := range group.values {
			if i == maxPerGroup {
				truncated = true
				fmt.Fprintf(&text, "  %s ... %d more\n", group.label, len(group.values)-i)
				break
			}
			fmt.Fprintf(&text, "  %s %s\n", group.label, value)
		}
	}
	return text.String(), truncated
}

func sourceRevision(source Source, short bool) string {
	if source.Revision != "" {
		return source.Revision
	}
	if short && len(source.Integrity) > 23 {
		return source.Integrity[:23]
	}
	return source.Integrity
}

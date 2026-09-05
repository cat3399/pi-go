package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

type Diff struct {
	Before    Sources
	After     Sources
	Added     []string
	Removed   []string
	Changed   []string
	Defaults  []string
	Providers []string
	Published bool
}

func Compare(before, after Document) Diff {
	result := Diff{Before: before.Sources, After: after.Sources}
	flatten := func(doc Document) map[string]json.RawMessage {
		models := map[string]json.RawMessage{}
		for _, p := range doc.Providers {
			for _, entries := range p.Models {
				for id, raw := range entries {
					models[p.ID+"/"+id] = raw
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

// String is intentionally a short terminal summary, not a generated report.
func (d Diff) String() string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s: %s -> %s\n", d.After.Models.Package, d.Before.Models.Version, d.After.Models.Version)
	fmt.Fprintf(&text, "Models: +%d / -%d / ~%d; defaults: %d; provider metadata: %d\n", len(d.Added), len(d.Removed), len(d.Changed), len(d.Defaults), len(d.Providers))
	for _, group := range []struct {
		label  string
		values []string
	}{{"+", d.Added}, {"-", d.Removed}, {"~", d.Changed}, {"default", d.Defaults}, {"provider", d.Providers}} {
		for i, value := range group.values {
			if i == 10 {
				fmt.Fprintf(&text, "  %s ... %d more\n", group.label, len(group.values)-i)
				break
			}
			fmt.Fprintf(&text, "  %s %s\n", group.label, value)
		}
	}
	if !d.Published {
		text.WriteString("Already up to date.\n")
	}
	return text.String()
}

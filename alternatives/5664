package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
)

type routingManifest struct {
	Families     map[string]string      `json:"families"`
	Tables       map[string]tableRoute  `json:"tables"`
	TargetTables map[string]tableRoute  `json:"target_tables"`
	Owners       map[string]ownerRoute  `json:"owners"`
	AtomicGroups map[string]atomicGroup `json:"atomic_groups"`
}

type tableRoute struct {
	Family      string `json:"family"`
	Disposition string `json:"disposition"`
	Contract    string `json:"contract"`
}

type ownerRoute struct {
	Families     []string    `json:"families"`
	Disposition  string      `json:"disposition"`
	Inventory    sourceOwner `json:"inventory"`
	AtomicGroups []string    `json:"atomic_groups,omitempty"`
}

type atomicGroup struct {
	Boundary    string   `json:"boundary"`
	Members     []string `json:"members"`
	Tables      []string `json:"tables"`
	Families    []string `json:"families"`
	Contract    string   `json:"contract"`
	ParityTests []string `json:"parity_tests"`
}

func readManifest(files fs.FS, name string) (routingManifest, error) {
	var manifest routingManifest
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return manifest, fmt.Errorf("read routing map: %w", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, fmt.Errorf("decode routing map: %w", err)
	}
	return manifest, nil
}

func routingProblems(inventory sourceInventory, manifest routingManifest) []string {
	var problems []string
	for table := range inventory.Tables {
		route, ok := manifest.Tables[table]
		if !ok {
			problems = append(problems, table+": unmapped table")
			continue
		}
		if manifest.Families[route.Family] == "" || !slices.Contains([]string{"target", "source-only", "rebuild"}, route.Disposition) || strings.TrimSpace(route.Contract) == "" {
			problems = append(problems, table+": incomplete table route")
		}
	}
	for table := range manifest.Tables {
		if _, ok := inventory.Tables[table]; !ok {
			problems = append(problems, table+": stale table route")
		}
	}
	for name := range inventory.TargetTables {
		table := name
		route, ok := manifest.TargetTables[table]
		if !ok {
			problems = append(problems, table+": unmapped target table")
			continue
		}
		if manifest.Families[route.Family] == "" || route.Disposition != "target-only" || strings.TrimSpace(route.Contract) == "" {
			problems = append(problems, table+": incomplete target table route")
		}
	}
	for table := range manifest.TargetTables {
		if _, ok := inventory.TargetTables[table]; !ok {
			problems = append(problems, table+": stale target table route")
		}
	}
	for name, owner := range inventory.Owners {
		route, ok := manifest.Owners[name]
		if !ok {
			problems = append(problems, name+": unmapped SQL owner")
			continue
		}
		if len(route.Families) == 0 || !slices.Contains([]string{"serving", "source-only", "target-adapter", "target-schema", "local-only", "external-service", "non-storage"}, route.Disposition) {
			problems = append(problems, name+": incomplete SQL owner route")
		}
		for _, family := range route.Families {
			if manifest.Families[family] == "" {
				problems = append(problems, name+": unknown family "+family)
			}
		}
		for _, group := range route.AtomicGroups {
			if _, ok := manifest.AtomicGroups[group]; !ok {
				problems = append(problems, name+": unknown atomic group "+group)
			}
		}
		if route.Disposition == "serving" {
			for _, table := range owner.Tables {
				if family := manifest.Tables[table].Family; family != "" && !slices.Contains(route.Families, family) {
					problems = append(problems, name+": missing table family "+family)
				}
			}
		}
		if !maps.Equal(owner.Operations, route.Inventory.Operations) || !slices.Equal(owner.Tables, route.Inventory.Tables) {
			problems = append(problems, name+": SQL owner inventory changed; review routing and atomic groups")
		}
	}
	for name := range manifest.Owners {
		if _, ok := inventory.Owners[name]; !ok {
			problems = append(problems, name+": stale SQL owner route")
		}
	}
	for name, group := range manifest.AtomicGroups {
		if group.Contract == "" || len(group.Families) == 0 || len(group.Tables) == 0 || len(group.ParityTests) == 0 {
			problems = append(problems, name+": incomplete atomic group")
		}
		for _, family := range group.Families {
			if manifest.Families[family] == "" {
				problems = append(problems, name+": unknown atomic family "+family)
			}
		}
		for _, test := range group.ParityTests {
			if !inventory.Tests[test] {
				problems = append(problems, name+": missing parity test "+test)
			}
		}
		for _, table := range group.Tables {
			if route, ok := manifest.Tables[table]; !ok {
				problems = append(problems, name+": unknown atomic table "+table)
			} else if !slices.Contains(group.Families, route.Family) {
				problems = append(problems, name+": missing atomic family "+route.Family)
			}
		}
		for _, member := range append([]string{group.Boundary}, group.Members...) {
			file, symbol, found := strings.Cut(member, "#")
			owner, ok := inventory.Owners[file]
			if !found || !ok || !hasSymbol(owner, symbol) {
				problems = append(problems, name+": missing atomic member "+member)
			}
		}
	}
	slices.Sort(problems)
	return problems
}

func hasSymbol(owner sourceOwner, symbol string) bool {
	if slices.Contains(owner.Declarations, symbol) {
		return true
	}
	for operation := range owner.Operations {
		if strings.HasPrefix(operation, symbol+":") {
			return true
		}
	}
	return false
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

func topologyNodes(resolved ResolvedProject) map[string]Service {
	nodes := maps.Clone(resolved.Services)
	if nodes == nil {
		nodes = map[string]Service{}
	}
	maps.Copy(nodes, resolved.Release)
	return nodes
}

func readinessWaves(resolved ResolvedProject) ([][]string, error) {
	nodes := topologyNodes(resolved)
	done := map[string]bool{}
	waves := [][]string{}
	for len(done) < len(nodes) {
		wave := []string{}
		for _, name := range slices.Sorted(maps.Keys(nodes)) {
			if done[name] {
				continue
			}
			ready := true
			for dependency := range nodes[name].DependsOn {
				if !done[dependency] {
					ready = false
				}
			}
			if ready {
				wave = append(wave, name)
			}
		}
		if len(wave) == 0 {
			return nil, errors.New("dependsOn contains a cycle or unknown dependency")
		}
		waves = append(waves, wave)
		for _, name := range wave {
			done[name] = true
		}
	}
	return waves, nil
}

func validateReadiness(resolved ResolvedProject) []error {
	var problems []error
	nodes := topologyNodes(resolved)
	for _, name := range slices.Sorted(maps.Keys(nodes)) {
		service := nodes[name]
		if _, duplicate := resolved.Services[name]; duplicate {
			if _, task := resolved.Release[name]; task {
				problems = append(problems, fmt.Errorf("%s is both a service and a release task", name))
			}
		}
		for _, dependency := range slices.Sorted(maps.Keys(service.DependsOn)) {
			condition := service.DependsOn[dependency]
			provider, exists := nodes[dependency]
			if !exists {
				continue
			}
			_, task := resolved.Release[dependency]
			if dependency == name {
				problems = append(problems, fmt.Errorf("%s depends on itself", name))
			}
			if condition == "completed" && !task {
				problems = append(problems, fmt.Errorf("%s: completed requires a release task, not %s", name, dependency))
			}
			if task && condition != "completed" {
				problems = append(problems, fmt.Errorf("%s: release dependency %s requires completed", name, dependency))
			}
			if condition == "healthy" && !hasHealthcheck(provider) {
				problems = append(problems, fmt.Errorf("%s: dependency %s needs an enabled healthcheck", name, dependency))
			}
		}
		if _, task := resolved.Release[name]; task {
			if service.Host != nil || len(service.Endpoints) > 0 {
				problems = append(problems, fmt.Errorf("release task %s cannot publish endpoints or a host", name))
			}
			for _, connection := range service.Connections {
				if service.DependsOn[connection.Service] != "healthy" {
					problems = append(problems, fmt.Errorf("release task %s must depend on connection provider %s healthy", name, connection.Service))
				}
			}
		}
	}
	if _, err := readinessWaves(resolved); err != nil {
		problems = append(problems, err)
	}
	return problems
}

func hasHealthcheck(service Service) bool {
	var check struct {
		Command []string `json:"command"`
		Disable bool     `json:"disable"`
	}
	return json.Unmarshal(service.Healthcheck, &check) == nil && !check.Disable && len(check.Command) > 0 && check.Command[0] != "NONE"
}

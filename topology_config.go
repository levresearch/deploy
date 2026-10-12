package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
)

type Endpoint struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	HostPort int    `json:"hostPort,omitempty"`
}

type Connection struct {
	Service  string `json:"service"`
	Endpoint string `json:"endpoint"`
	HostEnv  string `json:"hostEnv"`
	PortEnv  string `json:"portEnv"`
}

type ManagedNetwork struct {
	CIDR  string                 `json:"cidr"`
	Peers map[string]NetworkPeer `json:"peers"`
}

type NetworkPeer struct {
	Address    string `json:"address"`
	ListenPort int    `json:"listenPort"`
	Endpoint   string `json:"endpoint,omitempty"`
	Keepalive  int    `json:"keepalive,omitempty"`
}

func mergeEntries[T any](base, override map[string]T) map[string]T {
	if len(override) == 0 {
		return map[string]T{}
	}
	result := maps.Clone(base)
	if result == nil {
		result = map[string]T{}
	}
	maps.Copy(result, override)
	return result
}

func decodeOwned(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func loadPlanningProject(filename string) (Project, error) {
	raw, err := os.ReadFile(filename)
	if err != nil {
		return Project{}, err
	}
	if err := uniqueJSON(raw); err != nil {
		return Project{}, fmt.Errorf("parsing %s: %w", filename, err)
	}
	var version struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &version); err != nil {
		return Project{}, err
	}
	if version.Version != 1 && version.Version != 2 {
		return Project{}, fmt.Errorf("unsupported config version %d; supported: 1, 2 (planning only)", version.Version)
	}
	var project Project
	if version.Version == 2 {
		err = decodeOwned(raw, &project)
	} else {
		err = json.Unmarshal(raw, &project)
	}
	if err != nil {
		return Project{}, fmt.Errorf("parsing %s: %w", filename, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Project{}, err
	}
	for key := range fields {
		if version.Version == 2 && strings.EqualFold(key, "destination") {
			return Project{}, errors.New("version 2 has no root destination; select a named destination on every service and release task")
		}
		if version.Version == 1 && (strings.EqualFold(key, "destinations") || strings.EqualFold(key, "network")) {
			return Project{}, errors.New("destinations and network require version 2")
		}
	}
	if version.Version == 1 {
		groups := []map[string]Service{project.Services, project.Release}
		for _, environment := range project.Environments {
			if environment.Network != nil {
				return Project{}, errors.New("environment network requires version 2")
			}
			groups = append(groups, environment.Services, environment.Release)
		}
		for _, group := range groups {
			for _, service := range group {
				if service.Destination != "" || service.Endpoints != nil || service.Connections != nil {
					return Project{}, errors.New("service destinations, endpoints and connections require version 2")
				}
			}
		}
	}
	return project, nil
}

// reject duplicate keys before maps can hide contradictory configuration.
func uniqueJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("configuration nesting exceeds 64 levels")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, collection := token.(json.Delim)
		if !collection {
			return nil
		}
		seen := map[string]bool{}
		for decoder.More() {
			if delim == '{' {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return errors.New("expected an object key")
				}
				if seen[name] {
					return fmt.Errorf("duplicate configuration key %q", name)
				}
				seen[name] = true
			}
			if err := value(depth + 1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON document")
	}
	return nil
}

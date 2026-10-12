package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// edit the raw document so unrecognized legacy data is never silently dropped.
func previewMigration(filename, destination string) ([]byte, error) {
	raw, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	if err := uniqueJSON(raw); err != nil {
		return nil, err
	}
	var project Project
	if err := json.Unmarshal(raw, &project); err != nil {
		return nil, err
	}
	if project.Version != 1 {
		return nil, errors.New("migrate requires a version 1 config")
	}
	if destination == "" {
		destination = project.Destination
	}
	if _, err := topologyDestination(destination); err != nil {
		return nil, fmt.Errorf("migration destination: %w", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	if _, exists := document["destinations"]; exists {
		return nil, errors.New("version 1 config already contains destinations; resolve it before migration")
	}
	delete(document, "destination")
	document["version"] = json.RawMessage(`2`)
	document["destinations"], err = json.Marshal(map[string]string{"default": destination})
	if err != nil {
		return nil, err
	}
	addDestinations := func(raw json.RawMessage) (json.RawMessage, error) {
		var services map[string]map[string]json.RawMessage
		if err := json.Unmarshal(raw, &services); err != nil {
			return nil, err
		}
		for name, service := range services {
			if service == nil {
				return nil, fmt.Errorf("service %s is null", name)
			}
			if _, exists := service["destination"]; exists {
				return nil, fmt.Errorf("service %s already contains destination; resolve it before migration", name)
			}
			service["destination"] = json.RawMessage(`"default"`)
		}
		return json.Marshal(services)
	}
	for _, key := range []string{"services", "release"} {
		if source, exists := document[key]; exists {
			document[key], err = addDestinations(source)
			if err != nil {
				return nil, err
			}
		}
	}
	var environments map[string]map[string]json.RawMessage
	if source, exists := document["environments"]; exists {
		if err := json.Unmarshal(source, &environments); err != nil {
			return nil, err
		}
		for _, environment := range environments {
			for _, key := range []string{"services", "release"} {
				if source, exists := environment[key]; exists {
					environment[key], err = addDestinations(source)
					if err != nil {
						return nil, err
					}
				}
			}
		}
		document["environments"], err = json.Marshal(environments)
		if err != nil {
			return nil, err
		}
	}
	return json.MarshalIndent(document, "", "  ")
}

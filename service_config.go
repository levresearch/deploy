package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// serviceFields is the marshalling shape of the keys deploy owns. Service itself
// cannot carry these tags because Extra has to merge in on top.
type serviceFields struct {
	Destination string                `json:"destination,omitempty"`
	Endpoints   map[string]Endpoint   `json:"endpoints,omitempty"`
	Connections map[string]Connection `json:"connections,omitempty"`
	Image       string                `json:"image,omitempty"`
	Build       json.RawMessage       `json:"build,omitempty"`
	Stateful    *bool                 `json:"stateful,omitempty"`
	Env         []string              `json:"env,omitempty"`
	Host        *Host                 `json:"host,omitempty"`
	DependsOn   map[string]string     `json:"dependsOn,omitempty"`
	Healthcheck json.RawMessage       `json:"healthcheck,omitempty"`
	Enabled     *bool                 `json:"enabled,omitempty"`
	Isolated    string                `json:"isolated,omitempty"`
	Joins       []string              `json:"joins,omitempty"`
}

func (service *Service) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}

	for _, key := range slices.Sorted(maps.Keys(fields)) {
		value := fields[key]
		var err error

		switch key {
		case "destination":
			err = json.Unmarshal(value, &service.Destination)
		case "endpoints":
			if string(value) == "null" {
				return errors.New("endpoints cannot be null; use {} to clear inherited endpoints")
			}
			err = decodeOwned(value, &service.Endpoints)
		case "connections":
			if string(value) == "null" {
				return errors.New("connections cannot be null; use {} to clear inherited connections")
			}
			err = decodeOwned(value, &service.Connections)
		case "image":
			err = json.Unmarshal(value, &service.Image)
		case "build":
			service.Build = value
		case "stateful":
			var stateful bool
			if err = json.Unmarshal(value, &stateful); err == nil {
				service.Stateful = &stateful
			}
		case "env":
			err = json.Unmarshal(value, &service.Env)
		case "host":
			err = json.Unmarshal(value, &service.Host)
		case "dependsOn":
			err = json.Unmarshal(value, &service.DependsOn)
		case "healthcheck":
			service.Healthcheck = value
		case "enabled":
			var enabled bool
			if err = json.Unmarshal(value, &enabled); err == nil {
				service.Enabled = &enabled
			}
		case "isolated":
			err = json.Unmarshal(value, &service.Isolated)
		case "joins":
			err = json.Unmarshal(value, &service.Joins)
		default:
			if service.Extra == nil {
				service.Extra = map[string]json.RawMessage{}
			}
			service.Extra[key] = value
		}

		if err != nil {
			return fmt.Errorf("key %q: %w", key, err)
		}
	}

	return nil
}

func (service Service) MarshalJSON() ([]byte, error) {
	encoded, err := json.Marshal(serviceFields{
		Destination: service.Destination,
		Endpoints:   service.Endpoints,
		Connections: service.Connections,
		Image:       service.Image,
		Build:       service.Build,
		Stateful:    service.Stateful,
		Env:         service.Env,
		Host:        service.Host,
		DependsOn:   service.DependsOn,
		Healthcheck: service.Healthcheck,
		Enabled:     service.Enabled,
		Isolated:    service.Isolated,
		Joins:       service.Joins,
	})
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	maps.Copy(fields, service.Extra)
	if service.Endpoints != nil && len(service.Endpoints) == 0 {
		fields["endpoints"] = json.RawMessage(`{}`)
	}
	if service.Connections != nil && len(service.Connections) == 0 {
		fields["connections"] = json.RawMessage(`{}`)
	}
	if service.DependsOn != nil && len(service.DependsOn) == 0 {
		fields["dependsOn"] = json.RawMessage(`{}`)
	}

	return json.Marshal(fields)
}

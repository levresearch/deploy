package main

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Validate reports every problem it finds rather than the first, because fixing
// a config one error per run is miserable.
func (resolved ResolvedProject) Validate() error {
	var problems []error
	if !validEnvironment(resolved.Environment) {
		problems = append(problems, errors.New("environment must be a lowercase slug of at most 32 characters"))
	}

	if !projectIDPattern.MatchString(resolved.ID) {
		problems = append(problems, fmt.Errorf(
			"id %q must be 8 hex characters", resolved.ID,
		))
	}
	if resolved.Name == "" {
		problems = append(problems, errors.New("name is required"))
	}
	if len(resolved.Services) == 0 {
		problems = append(problems, errors.New("no services defined"))
	}
	// a notify block with nothing in it is someone who meant to be told and will
	// not be, which is worse than not configuring it at all
	if resolved.Notify != nil && resolved.Notify.DiscordWebhookFrom == "" {
		problems = append(problems, errors.New(
			"notify needs discordWebhookFrom, naming the variable that holds the webhook url. the url itself does not go in this file",
		))
	}
	// a webhook url here rather than a variable name is the mistake worth
	// catching, since it commits a credential
	if resolved.Notify != nil && strings.Contains(resolved.Notify.DiscordWebhookFrom, "://") {
		problems = append(problems, errors.New(
			"notify.discordWebhookFrom names the variable holding the webhook, not the webhook itself. putting the url here commits a credential to the repository",
		))
	}

	for _, name := range slices.Sorted(maps.Keys(resolved.Services)) {
		problems = append(problems, validateService(name, resolved.Services[name], resolved)...)
	}
	for _, name := range slices.Sorted(maps.Keys(resolved.Release)) {
		problems = append(problems, validateService(name, resolved.Release[name], resolved)...)
	}
	problems = append(problems, validatePrivateNetworks(resolved)...)
	if resolved.Version == 2 {
		problems = append(problems, resolved.validateTopology())
	}

	return errors.Join(problems...)
}

func validateService(name string, service Service, resolved ResolvedProject) []error {
	var problems []error

	if !serviceNamePattern.MatchString(name) {
		problems = append(problems, fmt.Errorf(
			"service %q is not a usable docker service name", name,
		))
	}

	hasImage := service.Image != ""
	hasBuild := len(service.Build) > 0
	switch {
	case hasImage && hasBuild:
		problems = append(problems, fmt.Errorf(
			"service %q sets both image and build, pick one", name,
		))
	case !hasImage && !hasBuild:
		problems = append(problems, fmt.Errorf(
			"service %q sets neither image nor build", name,
		))
	}

	// the build block is worked out here as well as at build time, so a config
	// that cannot be built is refused by deploy check rather than three steps into
	// a deploy
	if hasBuild {
		if _, _, err := buildPlan(name, service); err != nil {
			problems = append(problems, err)
		}
	}

	if service.Host != nil {
		problems = append(problems, validateHost(name, service)...)
	}

	for _, dependency := range slices.Sorted(maps.Keys(service.DependsOn)) {
		condition := service.DependsOn[dependency]
		if !slices.Contains(dependsOnConditions, condition) {
			problems = append(problems, fmt.Errorf(
				"service %q depends on %q with condition %q, expected one of %s",
				name, dependency, condition, strings.Join(dependsOnConditions, ", "),
			))
		}
		_, isService := resolved.Services[dependency]
		_, isReleaseTask := resolved.Release[dependency]
		if !isService && !isReleaseTask {
			problems = append(problems, fmt.Errorf(
				"service %q depends on %q, which this environment does not define",
				name, dependency,
			))
		}
	}

	return problems
}

// envFileNamePattern is a bare file name in the env directory.
var envFileNamePattern = regexp.MustCompile(`^\.?[A-Za-z0-9_][A-Za-z0-9._-]*$`)

func validateHost(name string, service Service) []error {
	var problems []error

	if service.Host.Domain == "" {
		problems = append(problems, fmt.Errorf("service %q has a host block with no domain", name))
	}
	if service.Host.Port == 0 {
		problems = append(problems, fmt.Errorf("service %q has a host block with no port", name))
	}
	if service.Host.TunnelTokenFrom == "" {
		problems = append(problems, fmt.Errorf(
			"service %q has a host block with no tunnelTokenFrom, which names the env var holding the token rather than the token itself",
			name,
		))
	}
	if tunnelEnv := service.Host.TunnelEnv; tunnelEnv != "" {
		if !envFileNamePattern.MatchString(tunnelEnv) {
			problems = append(problems, fmt.Errorf(
				"service %q has tunnelEnv %q, which must be a file name deploy places in the env directory, not a path",
				name, tunnelEnv,
			))
		}
		if slices.Contains(service.Env, tunnelEnv) {
			problems = append(problems, fmt.Errorf(
				"service %q lists its tunnelEnv %q in env too, which would hand the tunnel token to the service itself",
				name, tunnelEnv,
			))
		}
	}
	if service.IsStateful() {
		problems = append(problems, fmt.Errorf(
			"service %q is stateful and cannot be hosted, because a singleton cannot be cut over",
			name,
		))
	}
	if len(service.Healthcheck) == 0 {
		problems = append(problems, fmt.Errorf(
			"service %q is hosted so it needs a healthcheck, otherwise there is no way to know the new container serves before we send it traffic",
			name,
		))
	}
	if _, publishes := service.Extra["ports"]; publishes {
		problems = append(problems, fmt.Errorf(
			"service %q is hosted so it must not publish ports, because two commits cannot both bind the same one",
			name,
		))
	}

	return problems
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func runPlanning(args []string, command string) int {
	flags := newFlagSet(command)
	var context, environment string
	observe := false
	if command == "plan" {
		flags.BoolVar(&observe, "observe", false, "read destination identity, paths and Docker facts over trusted SSH")
	}
	stringFlag(flags, &context, "context", "C", ".", "directory containing .deploy.json")
	stringFlag(flags, &environment, "environment", "e", defaultEnvironmentName, "environment to preview")
	if proceed, code := parseCommandFlags(flags, args); !proceed {
		return code
	}
	project, err := loadPlanningProject(filepath.Join(context, configFileName))
	if err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}
	if observe {
		return runObservedPlanning(project, context, environment)
	}
	if project.Version == 2 {
		if err := validatePlanningProject(project); err != nil {
			fatal("%v", err)
			return exitPreconditionNotMet
		}
	}
	resolved, err := project.ResolveEnvironment(environment)
	if err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}
	if err := resolved.Validate(); err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}
	var output any = resolved
	if command == "plan" || project.Version == 2 {
		commit := "unverified"
		if command == "plan" {
			commit, err = ResolveCommit(context, "HEAD")
			if err != nil {
				fatal("%v", err)
				return exitPreconditionNotMet
			}
		}
		output, err = planTopology(resolved, commit)
		if err != nil {
			fatal("%v", err)
			return exitPreconditionNotMet
		}
	}
	return printPlanningJSON(output)
}

func runObservedPlanning(project Project, directory, environment string) int {
	commit, err := ResolveCommit(directory, "HEAD")
	if err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}
	if _, err := project.ResolveEnvironment(environment); err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}
	observations, err := observePlanningProject(context.Background(), project, observeDestination)
	if err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}
	plan, err := planObservedTopology(project, environment, commit, observations)
	if err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}
	resolved, err := project.ResolveEnvironment(environment)
	if err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}
	readers := map[string]stateReader{}
	for name := range observations {
		destination, err := topologyDestination(project.Destinations[name])
		if err != nil {
			fatal("%v", err)
			return exitPreconditionNotMet
		}
		readers[name] = observedReader{ctx: context.Background(), destination: destination}
	}
	if err := applyRecordedState(&plan, resolved, readers); err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}
	return printPlanningJSON(plan)
}

func runMigrate(args []string) int {
	flags := newFlagSet("migrate")
	var context, destination string
	stringFlag(flags, &context, "context", "C", ".", "directory containing .deploy.json")
	stringFlag(flags, &destination, "destination", "D", "", "destination when the old config does not contain one")
	if proceed, code := parseCommandFlags(flags, args); !proceed {
		return code
	}
	preview, err := previewMigration(filepath.Join(context, configFileName), destination)
	if err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}
	fmt.Fprintln(os.Stderr, "preview only; version 2 execution is unavailable. review and run check on the converted configuration before adoption")
	fmt.Println(string(preview))
	return exitOK
}

func printPlanningJSON(value any) int {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}
	fmt.Println(string(encoded))
	return exitOK
}

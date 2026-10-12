package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func runPlanning(args []string, command string) int {
	flags := newFlagSet(command)
	var context, environment string
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

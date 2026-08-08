package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

const (
	exitOK                 = 0
	exitDeployFailed       = 1
	exitPreconditionNotMet = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "check":
			return runCheck(args[1:])
		default:
			fatal("unknown command %q, run deploy -h for the list", args[0])
			return exitPreconditionNotMet
		}
	}

	return runDeploy(args)
}

func runDeploy(args []string) int {
	flags := newFlagSet("deploy")
	if keepGoing, exitCode := parseCommandFlags(flags, args); !keepGoing {
		return exitCode
	}

	fatal("deploying is not implemented yet, try deploy check")

	return exitPreconditionNotMet
}

func runCheck(args []string) int {
	flags := newFlagSet("check")
	environment := flags.String("environment", defaultEnvironmentName, "environment to resolve")
	flags.StringVar(environment, "e", defaultEnvironmentName, "environment to resolve")
	if keepGoing, exitCode := parseCommandFlags(flags, args); !keepGoing {
		return exitCode
	}

	project, err := LoadProject(configFileName)
	if errors.Is(err, fs.ErrNotExist) {
		fatal("no %s in this directory, so there is nothing to check", configFileName)
		return exitPreconditionNotMet
	}
	if err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}

	resolved, err := project.ResolveEnvironment(*environment)
	if err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}

	if err := resolved.Validate(); err != nil {
		fatal("%s is not valid:\n  %s", configFileName, strings.ReplaceAll(err.Error(), "\n", "\n  "))
		return exitPreconditionNotMet
	}

	printed, err := json.MarshalIndent(resolved, "", "  ")
	if err != nil {
		fatal("%v", err)
		return exitPreconditionNotMet
	}
	fmt.Println(string(printed))

	return exitOK
}

func newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.Usage = printUsage

	return flags
}

// parseCommandFlags reports whether the command should carry on. Asking for help
// is not a failure, so it stops with a success code rather than an error one.
func parseCommandFlags(flags *flag.FlagSet, args []string) (bool, int) {
	err := flags.Parse(args)

	switch {
	case err == nil:
		return true, exitOK
	case errors.Is(err, flag.ErrHelp):
		return false, exitOK
	default:
		return false, exitPreconditionNotMet
	}
}

func printUsage() {
	fmt.Fprint(os.Stdout, `deploy - dynamically deploy managed services on servers you own

usage:
  deploy [flags]          deploy the current commit
  deploy check [flags]    validate the config, print it, change nothing

flags:
  -e, --environment <name>  environment to resolve, default "`+defaultEnvironmentName+`"
  -h, --help                show this help
`)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL - "+format+"\n", args...)
}

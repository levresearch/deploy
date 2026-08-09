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
	// deployed and serving, but something afterwards needs a person. tearing the
	// new release down over a failed prune would be a self inflicted outage
	exitLiveButNeedsAHuman = 3
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "check":
			return runCheck(args[1:])
		case "releases":
			return runReleases(args[1:])
		case "env":
			return runEnv(args[1:])
		default:
			fatal("unknown command %q, run deploy -h for the list", args[0])
			return exitPreconditionNotMet
		}
	}

	return runDeploy(args)
}

func runDeploy(args []string) int {
	flags := newFlagSet("deploy")
	options := DeployOptions{}
	stringFlag(flags, &options.Context, "context", "C", "", "scope deploy to this path instead of the cwd")
	stringFlag(flags, &options.GitStorage, "git-storage", "G", "", "where the bare repo lives")
	stringFlag(flags, &options.Destination, "destination", "D", "", "where the project gets deployed")
	stringFlag(flags, &options.Environment, "environment", "e", defaultEnvironmentName, "environment to resolve")
	flags.BoolVar(&options.AllowDirty, "allow-dirty", false, "deploy with uncommitted changes present")
	flags.BoolVar(&options.ForceUnlock, "force-unlock", false, "break a stale deploy.lock")
	flags.BoolVar(&options.BuildOnDest, "build-on-destination", false, "let the destination build instead of building here")
	if keepGoing, exitCode := parseCommandFlags(flags, args); !keepGoing {
		return exitCode
	}

	exitCode, err := RunDeploy(options)
	if err != nil {
		fatal("%v", err)
	}

	return exitCode
}

func runEnv(args []string) int {
	if len(args) < 2 || args[0] != "push" {
		fatal("usage: deploy env push <file>")
		return exitPreconditionNotMet
	}

	flags := newFlagSet("env push")
	options := DeployOptions{}
	stringFlag(flags, &options.Context, "context", "C", "", "scope deploy to this path instead of the cwd")
	stringFlag(flags, &options.Destination, "destination", "D", "", "where the project gets deployed")
	stringFlag(flags, &options.Environment, "environment", "e", defaultEnvironmentName, "environment to resolve")
	if keepGoing, exitCode := parseCommandFlags(flags, args[2:]); !keepGoing {
		return exitCode
	}

	exitCode, err := RunEnvPush(options, args[1])
	if err != nil {
		fatal("%v", err)
	}

	return exitCode
}

func runReleases(args []string) int {
	flags := newFlagSet("releases")
	options := DeployOptions{}
	stringFlag(flags, &options.Context, "context", "C", "", "scope deploy to this path instead of the cwd")
	stringFlag(flags, &options.Destination, "destination", "D", "", "where the project gets deployed")
	stringFlag(flags, &options.Environment, "environment", "e", defaultEnvironmentName, "environment to resolve")
	if keepGoing, exitCode := parseCommandFlags(flags, args); !keepGoing {
		return exitCode
	}

	exitCode, err := RunReleases(options)
	if err != nil {
		fatal("%v", err)
	}

	return exitCode
}

func runCheck(args []string) int {
	flags := newFlagSet("check")
	environment := new(string)
	stringFlag(flags, environment, "environment", "e", defaultEnvironmentName, "environment to resolve")
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

// stringFlag registers a long and a short name against one variable, since go's
// flag package has no native pairing.
func stringFlag(flags *flag.FlagSet, target *string, long, short, fallback, usage string) {
	flags.StringVar(target, long, fallback, usage)
	flags.StringVar(target, short, fallback, usage)
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
  deploy releases         releases on the destination, current one marked
  deploy env push <file>  upload an env file to the destination

flags:
  -C, --context <path>      scope deploy to this path instead of the cwd
  -G, --git-storage <path>  where the bare repo lives, which lets the
                            destination extract without an upload
  -D, --destination <path>  where the project gets deployed
      --allow-dirty         deploy with uncommitted changes present
      --force-unlock        break a stale deploy.lock
      --build-on-destination
                            build on the destination instead of here
  -e, --environment <name>  environment to resolve, default "`+defaultEnvironmentName+`"
  -h, --help                show this help
`)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL - "+format+"\n", args...)
}

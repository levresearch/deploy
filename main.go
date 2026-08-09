package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"slices"
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

// subcommands is the dispatch table and the list an error message checks against,
// so the two can never disagree about what exists.
//
// NOTE: filled in by init rather than declared, because parseCommandFlags reads
// this map and several of the functions in it call parseCommandFlags, which go
// sees as an initialisation cycle.
var subcommands map[string]func([]string) int

func init() {
	subcommands = map[string]func([]string) int{
		"check":    runCheck,
		"releases": runReleases,
		"env":      runEnv,
		"rollback": runRollback,
		"destroy":  runDestroy,
		"status":   func(args []string) int { return runInspect(args, "status") },
		"list":     func(args []string) int { return runInspect(args, "list") },
		"logs":     func(args []string) int { return runInspect(args, "logs") },
		"shell":    func(args []string) int { return runInspect(args, "shell") },
		"exec":     func(args []string) int { return runInspect(args, "exec") },
	}
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, known := subcommands[args[0]]
		if !known {
			fatal("unknown command %q, run deploy -h for the list", args[0])
			return exitPreconditionNotMet
		}

		return command(args[1:])
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

// runInspect covers the read-only commands, which share every flag and differ
// only in what they do with the service they were pointed at.
func runInspect(args []string, command string) int {
	// everything after -- belongs to the container, not to deploy
	inside := []string{}
	if separator := slices.Index(args, "--"); separator >= 0 {
		inside = args[separator+1:]
		args = args[:separator]
	}

	serviceName := ""
	if slices.Contains([]string{"logs", "shell", "exec"}, command) {
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			serviceName, args = args[0], args[1:]
		}
	}

	flags := newFlagSet(command)
	options := DeployOptions{}
	follow := false
	stringFlag(flags, &options.Context, "context", "C", "", "scope deploy to this path instead of the cwd")
	stringFlag(flags, &options.Destination, "destination", "D", "", "where the project gets deployed")
	stringFlag(flags, &options.Environment, "environment", "e", defaultEnvironmentName, "environment to resolve")
	if command == "logs" {
		flags.BoolVar(&follow, "follow", false, "keep printing as more arrives")
		flags.BoolVar(&follow, "f", false, "keep printing as more arrives")
	}
	if keepGoing, exitCode := parseCommandFlags(flags, args); !keepGoing {
		return exitCode
	}

	if serviceName == "" && slices.Contains([]string{"logs", "shell", "exec"}, command) {
		fatal("which service? try deploy %s <service>", command)
		return exitPreconditionNotMet
	}

	var exitCode int
	var err error

	switch command {
	case "status":
		exitCode, err = RunStatus(options)
	case "list":
		exitCode, err = RunList(options)
	case "logs":
		exitCode, err = RunLogs(options, serviceName, follow)
	case "shell":
		exitCode, err = RunShell(options, serviceName)
	case "exec":
		exitCode, err = RunExec(options, serviceName, inside)
	}

	if err != nil {
		fatal("%v", err)
	}

	return exitCode
}

func runDestroy(args []string) int {
	flags := newFlagSet("destroy")
	options := DeployOptions{}
	removeVolumes := false
	stringFlag(flags, &options.Context, "context", "C", "", "scope deploy to this path instead of the cwd")
	stringFlag(flags, &options.Destination, "destination", "D", "", "where the project gets deployed")
	stringFlag(flags, &options.Environment, "environment", "e", defaultEnvironmentName, "environment to resolve")
	flags.BoolVar(&removeVolumes, "volumes", false, "remove the data and the env files too")
	if keepGoing, exitCode := parseCommandFlags(flags, args); !keepGoing {
		return exitCode
	}

	exitCode, err := RunDestroy(options, removeVolumes, os.Stdin)
	if err != nil {
		fatal("%v", err)
	}

	return exitCode
}

func runRollback(args []string) int {
	requested := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		requested, args = args[0], args[1:]
	}

	flags := newFlagSet("rollback")
	options := DeployOptions{}
	stringFlag(flags, &options.Context, "context", "C", "", "scope deploy to this path instead of the cwd")
	stringFlag(flags, &options.Destination, "destination", "D", "", "where the project gets deployed")
	stringFlag(flags, &options.Environment, "environment", "e", defaultEnvironmentName, "environment to resolve")
	flags.BoolVar(&options.ForceUnlock, "force-unlock", false, "break a stale deploy.lock")
	if keepGoing, exitCode := parseCommandFlags(flags, args); !keepGoing {
		return exitCode
	}

	exitCode, err := RunRollback(options, requested)
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
//
// Anything left over after the flags is refused. Every command here takes its
// positional argument before parsing, so a leftover is something the user typed
// and deploy would otherwise ignore. `deploy -D /srv status` silently deploying
// instead of showing status is exactly the kind of quiet wrong that this whole
// tool is supposed to be the opposite of.
func parseCommandFlags(flags *flag.FlagSet, args []string) (bool, int) {
	err := flags.Parse(args)

	switch {
	case errors.Is(err, flag.ErrHelp):
		return false, exitOK
	case err != nil:
		return false, exitPreconditionNotMet
	}

	if leftover := flags.Args(); len(leftover) > 0 {
		if _, isCommand := subcommands[leftover[0]]; isCommand {
			fatal("%s is a command, so it goes first, as in: deploy %s <flags>", leftover[0], leftover[0])
		} else {
			fatal("%s does not take %q", flags.Name(), strings.Join(leftover, " "))
		}

		return false, exitPreconditionNotMet
	}

	return true, exitOK
}

func printUsage() {
	fmt.Fprint(os.Stdout, `deploy - dynamically deploy managed services on servers you own

usage:
  deploy [flags]          deploy the current commit
  deploy check [flags]    validate the config, print it, change nothing
  deploy status           current release, per service health, what is exposed
  deploy list             every project on the destination, not just this one
  deploy logs [-f] <svc>  tail a service
  deploy shell <svc>      a shell inside the running container
  deploy exec <svc> -- .. run one command inside it
  deploy destroy          tear this project down, keeping data unless --volumes
  deploy releases         releases on the destination, current one marked
  deploy env push <file>  upload an env file to the destination
  deploy rollback [<commit>]
                          activate a retained release, default the previous one

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

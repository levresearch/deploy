package main

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"time"
)

// an external service's release is a command run to completion in its image,
// for work that lives somewhere deploy does not run it, like a hosting
// platform. these are the limits deploy holds it to.
const (
	defaultExternalTimeout  = 30 * time.Minute
	minimumExternalTimeout  = time.Minute
	maximumExternalTimeout  = 2 * time.Hour
	defaultVerifyTimeout    = 2 * time.Minute
	maximumVerifyTimeout    = 30 * time.Minute
	maximumReferencePattern = 512
	// a reference is recorded in state and put in a rollback argument, so it is
	// one bounded token with no whitespace or control characters
	maximumReference     = 2048
	referencePlaceholder = "{reference}"
)

var defaultVerifyStatus = []int{200}

// keys compose understands that would quietly fight what deploy does with an
// external: run sets the command, and nothing it starts stays up to publish
// or restart
var refusedExternalExtra = []string{"command", "ports", "expose", "restart"}

// externalKey names the first key on a service or release task that only an
// external service takes, or nothing.
func externalKey(service Service) string {
	switch {
	case service.Run != nil:
		return "run"
	case service.Reference != "":
		return "reference"
	case service.Verify != nil:
		return "verify"
	case service.Rollback != nil:
		return "rollback"
	case service.Timeout != "":
		return "timeout"
	}

	return ""
}

func validateExternal(name string, service Service) []error {
	var problems []error
	refuse := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf("external %s "+format, append([]any{name}, args...)...))
	}

	if service.IsStateful() {
		refuse("cannot be stateful; nothing of it keeps running here")
	}
	if service.Host != nil || len(service.Endpoints) > 0 {
		refuse("cannot publish a host or endpoints; it runs to completion")
	}
	if len(service.Connections) > 0 {
		refuse("cannot declare connections")
	}
	if service.Healthcheck != nil {
		refuse("takes verify rather than a healthcheck, since nothing stays up to check")
	}
	for _, key := range refusedExternalExtra {
		if _, found := service.Extra[key]; found {
			if key == "command" {
				refuse("sets command; use run")
				continue
			}
			refuse("sets %s, which means nothing for a command that runs to completion", key)
		}
	}
	if service.Run != nil && !validArgv(service.Run) {
		refuse("run needs at least one argument and no empty ones")
	}
	if service.Rollback != nil && !validArgv(service.Rollback) {
		refuse("rollback needs at least one argument and no empty ones")
	}
	if slices.ContainsFunc(service.Rollback, func(argument string) bool { return strings.Contains(argument, referencePlaceholder) }) && service.Reference == "" {
		refuse("rollback uses %s but no reference pattern records one", referencePlaceholder)
	}
	if service.Reference != "" {
		if _, err := referencePattern(service.Reference); err != nil {
			refuse("reference: %v", err)
		}
	}
	if service.Timeout != "" {
		if _, err := externalTimeout(service); err != nil {
			refuse("timeout: %v", err)
		}
	}
	if service.Verify != nil {
		for _, err := range validateVerify(*service.Verify) {
			refuse("verify: %v", err)
		}
	}

	return problems
}

func validArgv(argv []string) bool {
	return len(argv) > 0 && !slices.Contains(argv, "")
}

func externalTimeout(service Service) (time.Duration, error) {
	if service.Timeout == "" {
		return defaultExternalTimeout, nil
	}
	timeout, err := time.ParseDuration(service.Timeout)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration like 30m", service.Timeout)
	}
	if timeout < minimumExternalTimeout || timeout > maximumExternalTimeout {
		return 0, fmt.Errorf("%s is outside %s to %s", timeout, minimumExternalTimeout, maximumExternalTimeout)
	}

	return timeout, nil
}

// referencePattern is matched against each line of output; the last line that
// matches gives the reference, its first group when it has one.
func referencePattern(pattern string) (*regexp.Regexp, error) {
	if len(pattern) > maximumReferencePattern {
		return nil, fmt.Errorf("the pattern is longer than %d characters", maximumReferencePattern)
	}
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil, err
	}
	if parsed.MaxCap() > 1 {
		return nil, errors.New("the pattern may have one capture group at most")
	}

	return regexp.Compile(pattern)
}

func validReference(reference string) bool {
	if reference == "" || len(reference) > maximumReference {
		return false
	}
	for _, character := range reference {
		if character <= ' ' || character == 127 {
			return false
		}
	}

	return true
}

func validateVerify(verify Verify) []error {
	var problems []error
	parsed, err := url.Parse(verify.URL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		problems = append(problems, errors.New("url must be an absolute http or https address with no credentials or fragment"))
	}
	for _, status := range verify.Status {
		if status < 100 || status > 599 {
			problems = append(problems, fmt.Errorf("status %d is not an http status", status))
		}
	}
	if _, err := verifyTimeout(verify); err != nil {
		problems = append(problems, err)
	}

	return problems
}

func verifyTimeout(verify Verify) (time.Duration, error) {
	if verify.Timeout == "" {
		return defaultVerifyTimeout, nil
	}
	timeout, err := time.ParseDuration(verify.Timeout)
	if err != nil {
		return 0, fmt.Errorf("timeout %q is not a duration like 2m", verify.Timeout)
	}
	if timeout <= 0 || timeout > maximumVerifyTimeout {
		return 0, fmt.Errorf("timeout %s is outside 1s to %s", timeout, maximumVerifyTimeout)
	}

	return timeout, nil
}

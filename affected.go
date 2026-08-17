package main

import (
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
)

// Claim is the part of the tree a service is built from, which deploy takes to
// be the directory its dockerfile sits in. AlwaysBuilds covers a service whose
// dockerfile is generated rather than committed, so there is no directory to
// point at and building it every time is the only answer that cannot be wrong.
type Claim struct {
	Directory    string
	AlwaysBuilds bool
}

// ServiceClaims reads what each buildable service is built from. A service
// running a published image is absent, since it is never built and so never
// reused either.
func ServiceClaims(resolved ResolvedProject) (map[string]Claim, error) {
	claims := map[string]Claim{}

	for name, service := range buildableServices(resolved) {
		if len(service.Build) == 0 {
			continue
		}

		dockerfile, generated, err := buildPlan(name, service)
		if err != nil {
			return nil, err
		}
		// a generated dockerfile copies the whole tree, so nothing narrower than
		// everything is true about what it depends on
		if generated != nil {
			claims[name] = Claim{AlwaysBuilds: true}

			continue
		}

		claims[name] = Claim{Directory: path.Dir(dockerfile)}
	}

	return claims, nil
}

// CheckClaims refuses a layout where one service is built from a directory that
// contains another's. The whole thing rests on a change nobody claims meaning
// everybody rebuilds, and a broad claim swallows that signal, because the change
// counts as claimed and the service nested inside is skipped even when the
// change was its own. A dockerfile at the repository root is the worst case of
// this rather than a separate one.
func CheckClaims(claims map[string]Claim) error {
	for _, outer := range slices.Sorted(maps.Keys(claims)) {
		for _, inner := range slices.Sorted(maps.Keys(claims)) {
			if claims[outer].AlwaysBuilds || claims[inner].AlwaysBuilds {
				continue
			}
			if !claimContains(claims[outer].Directory, claims[inner].Directory) {
				continue
			}

			return fmt.Errorf(
				"--affected needs every service built from its own directory, but %s is built from %s which holds the %s that %s is built from, so a change in between would look claimed and %s would be skipped even when it changed. move them apart, or deploy without --affected",
				outer, describeClaim(claims[outer].Directory), describeClaim(claims[inner].Directory), inner, inner,
			)
		}
	}

	return nil
}

// claimContains is strict, since two services built from the same directory is
// the ordinary case of a release task sharing an app's dockerfile.
func claimContains(parent, child string) bool {
	if parent == child {
		return false
	}
	if isRootClaim(parent) {
		return true
	}

	return strings.HasPrefix(child, parent+"/")
}

// isRootClaim covers both spellings, because path.Dir answers "." for a
// dockerfile at the top of the repository and an empty string is what an
// unset claim holds.
func isRootClaim(directory string) bool {
	return directory == "" || directory == "."
}

func describeClaim(directory string) string {
	if isRootClaim(directory) {
		return "the repository root"
	}

	return directory
}

// AffectedServices reports which services the changed files reach, and which of
// those files no service claims at all. Unclaimed is shared code as far as
// deploy can tell, so the caller reads any of it as everything being affected.
func AffectedServices(claims map[string]Claim, changed []string) (affected, unclaimed []string) {
	reached := map[string]bool{}

	for name, claim := range claims {
		if claim.AlwaysBuilds {
			reached[name] = true
		}
	}

	for _, file := range changed {
		isClaimed := false

		for name, claim := range claims {
			if claim.AlwaysBuilds || !claimCovers(claim.Directory, file) {
				continue
			}
			reached[name] = true
			isClaimed = true
		}

		if !isClaimed {
			unclaimed = append(unclaimed, file)
		}
	}

	return slices.Sorted(maps.Keys(reached)), unclaimed
}

func claimCovers(directory, file string) bool {
	if isRootClaim(directory) {
		return true
	}

	return file == directory || strings.HasPrefix(file, directory+"/")
}

// ChangedFiles lists what differs between two commits. It compares the trees
// rather than walking the history between them, so a rollback or a branch switch
// in the meantime still answers with what is actually different now.
func ChangedFiles(repositoryPath, from, to string) ([]string, error) {
	output, err := runGit(repositoryPath, "diff", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	if output == "" {
		return nil, nil
	}

	return strings.Split(output, "\n"), nil
}

// PlanReuse decides which services can keep the image an earlier commit already
// built, mapping the service name to the commit to take it from. A nil map means
// build everything, which is the answer whenever the question cannot be answered
// and not only when it comes back no. Every one of those paths says why out
// loud, since a deploy that quietly did more work than asked is the kind of
// thing you only notice as a number you cannot explain.
func PlanReuse(
	builder *Builder, resolved ResolvedProject, repositoryPath, previous, commit string,
) (map[string]string, error) {
	claims, err := ServiceClaims(resolved)
	if err != nil {
		return nil, err
	}

	if previous == "" {
		fmt.Printf("  --affected: nothing is deployed yet, so building everything\n")

		return nil, nil
	}
	if !CommitExists(repositoryPath, previous) {
		fmt.Fprintf(os.Stderr,
			"warning: --affected cannot diff against %s because it is not in this repository, so building everything\n",
			previous,
		)

		return nil, nil
	}

	changed, err := ChangedFiles(repositoryPath, previous, commit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: --affected could not read what changed, so building everything: %v\n", err)

		return nil, nil
	}

	affected, unclaimed := AffectedServices(claims, changed)
	if len(unclaimed) > 0 {
		fmt.Printf("  --affected: %s, so building everything\n", describeUnclaimed(unclaimed))

		return nil, nil
	}

	reuse := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(claims)) {
		if slices.Contains(affected, name) {
			continue
		}
		// retention or a manual prune may have taken it since, and a tag pointing
		// at nothing would fail at container start rather than here
		if !builder.HasImage(name, previous) {
			fmt.Printf("  --affected: %s has no image at %s, so building it\n", name, previous)

			continue
		}
		reuse[name] = previous
	}

	if len(reuse) == 0 {
		return nil, nil
	}

	rebuilding := "nothing"
	if len(affected) > 0 {
		rebuilding = strings.Join(affected, " ")
	}
	fmt.Printf(
		"  --affected: rebuilding %s, reusing %s from %s\n",
		rebuilding, strings.Join(slices.Sorted(maps.Keys(reuse)), " "), previous,
	)

	return reuse, nil
}

// unclaimedToName is small on purpose, since a merge can change hundreds of
// files and what you need to see is which kind of file did it.
const unclaimedToName = 3

func describeUnclaimed(unclaimed []string) string {
	if len(unclaimed) <= unclaimedToName {
		return strings.Join(unclaimed, ", ") + " is claimed by no service"
	}

	return fmt.Sprintf(
		"%s and %d more are claimed by no service",
		strings.Join(unclaimed[:unclaimedToName], ", "), len(unclaimed)-unclaimedToName,
	)
}

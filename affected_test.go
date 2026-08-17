package main

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestAServiceIsClaimedByTheDirectoryItsDockerfileSitsIn(t *testing.T) {
	const contents = `{
      "version": 1, "id": "a3f19c02", "name": "lectern",
      "services": {
        "web": {"build": {"dockerfile": "apps/web/Dockerfile"}},
        "bot": {"build": "packages/discord/bot/Dockerfile"},
        "pg": {"image": "postgres:17", "stateful": true}
      },
      "release": {"migrate": {"build": {"dockerfile": "apps/api/Dockerfile"}}}
    }`

	claims, err := ServiceClaims(loadAndResolve(t, contents, defaultEnvironmentName))
	if err != nil {
		t.Fatalf("ServiceClaims: %v", err)
	}

	want := map[string]string{
		"web":     "apps/web",
		"bot":     "packages/discord/bot",
		"migrate": "apps/api",
	}
	for name, directory := range want {
		if claims[name].Directory != directory {
			t.Errorf("%s claims %q, want %q", name, claims[name].Directory, directory)
		}
	}

	// a published image is never built, so it can never be reused either
	if _, found := claims["pg"]; found {
		t.Error("a service with no build block should hold no claim")
	}
}

// A generated dockerfile copies the whole tree, so there is no directory that
// describes it and the only honest answer is that everything reaches it.
func TestAGeneratedDockerfileClaimsNothingAndAlwaysBuilds(t *testing.T) {
	const contents = `{
      "version": 1, "id": "a3f19c02", "name": "lectern",
      "services": {"web": {"build": {"from": "node:24-slim", "start": "node server.js"}}}
    }`

	claims, err := ServiceClaims(loadAndResolve(t, contents, defaultEnvironmentName))
	if err != nil {
		t.Fatalf("ServiceClaims: %v", err)
	}
	if !claims["web"].AlwaysBuilds {
		t.Error("an inline build has no directory to point at, so it has to build every time")
	}

	affected, unclaimed := AffectedServices(claims, []string{"anything/at/all.txt"})
	if !slices.Contains(affected, "web") {
		t.Errorf("an inline build is reached by any change, got %v", affected)
	}
	// it claims nothing, but it is not a service failing to claim the file
	if len(unclaimed) != 1 {
		t.Errorf("the file is still claimed by nobody, got %v", unclaimed)
	}
}

func TestNestedClaimsAreRefusedBecauseTheOuterOneHidesAChange(t *testing.T) {
	cases := []struct {
		name    string
		claims  map[string]Claim
		refused bool
	}{
		{
			name: "side by side directories are what this is for",
			claims: map[string]Claim{
				"web": {Directory: "apps/web"},
				"api": {Directory: "apps/api"},
			},
		},
		{
			// the release task sharing an app's dockerfile, which is the ordinary case
			name: "the same directory twice is fine",
			claims: map[string]Claim{
				"api":     {Directory: "apps/api"},
				"migrate": {Directory: "apps/api"},
			},
		},
		{
			name: "a dockerfile at the root swallows everything",
			claims: map[string]Claim{
				"everything": {Directory: "."},
				"web":        {Directory: "apps/web"},
			},
			refused: true,
		},
		{
			name: "and so does any other directory holding another",
			claims: map[string]Claim{
				"apps": {Directory: "apps"},
				"web":  {Directory: "apps/web"},
			},
			refused: true,
		},
		{
			// an inline build holds no directory, so it is not a root claim in disguise
			name: "an inline build alongside a real one is fine",
			claims: map[string]Claim{
				"generated": {AlwaysBuilds: true},
				"web":       {Directory: "apps/web"},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := CheckClaims(testCase.claims)

			if testCase.refused && err == nil {
				t.Fatal("expected this layout to be refused")
			}
			if !testCase.refused && err != nil {
				t.Fatalf("expected this layout to be allowed, got: %v", err)
			}
			if testCase.refused && !strings.Contains(err.Error(), "--affected") {
				t.Errorf("the error should say which flag cannot run, got: %v", err)
			}
		})
	}
}

func TestOnlyTheServicesAChangeReachesAreAffected(t *testing.T) {
	claims := map[string]Claim{
		"web":     {Directory: "apps/web"},
		"api":     {Directory: "apps/api"},
		"migrate": {Directory: "apps/api"},
		"bot":     {Directory: "packages/discord/bot"},
	}

	cases := []struct {
		name      string
		changed   []string
		affected  []string
		unclaimed []string
	}{
		{
			name:     "one app",
			changed:  []string{"apps/web/page.tsx"},
			affected: []string{"web"},
		},
		{
			// two services built from one directory move together
			name:     "a directory two services share",
			changed:  []string{"apps/api/index.ts"},
			affected: []string{"api", "migrate"},
		},
		{
			name:     "two apps at once",
			changed:  []string{"apps/web/page.tsx", "packages/discord/bot/main.ts"},
			affected: []string{"bot", "web"},
		},
		{
			// packages/discord is not packages/discord/bot, so nobody is built from it
			name:      "shared code nobody is built from",
			changed:   []string{"packages/discord/protocol.ts"},
			unclaimed: []string{"packages/discord/protocol.ts"},
		},
		{
			name:      "a claimed change alongside an unclaimed one",
			changed:   []string{"apps/web/page.tsx", "pnpm-lock.yaml"},
			affected:  []string{"web"},
			unclaimed: []string{"pnpm-lock.yaml"},
		},
		{
			// the directory itself, rather than something inside it
			name:     "the dockerfile directory as a path",
			changed:  []string{"apps/web"},
			affected: []string{"web"},
		},
		{
			// apps/website is not apps/web, and a prefix test that forgot the
			// separator would hand web a change that was never its own
			name:      "a directory whose name only starts the same",
			changed:   []string{"apps/website/page.tsx"},
			unclaimed: []string{"apps/website/page.tsx"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			affected, unclaimed := AffectedServices(claims, testCase.changed)

			if !slices.Equal(affected, testCase.affected) {
				t.Errorf("affected = %v, want %v", affected, testCase.affected)
			}
			if !slices.Equal(unclaimed, testCase.unclaimed) {
				t.Errorf("unclaimed = %v, want %v", unclaimed, testCase.unclaimed)
			}
		})
	}
}

// The baseline is whatever is running on the destination, which a rollback can
// make older than commits already built. Comparing the trees rather than the
// history between them is what makes that answer correctly.
func TestChangedFilesComparesTreesRatherThanHistory(t *testing.T) {
	repository := newRepository(t)

	first := commitFile(t, repository, "apps/web/page.tsx", "one")
	commitFile(t, repository, "apps/api/index.ts", "two")
	third := commitFile(t, repository, "apps/web/page.tsx", "three")

	changed, err := ChangedFiles(repository, first, third)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}

	// api is in the history between them, and in the difference too
	want := []string{"apps/api/index.ts", "apps/web/page.tsx"}
	slices.Sort(changed)
	if !slices.Equal(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}

	// going the other way is the rollback case, and it is still a difference
	back, err := ChangedFiles(repository, third, first)
	if err != nil {
		t.Fatalf("ChangedFiles backwards: %v", err)
	}
	slices.Sort(back)
	if !slices.Equal(back, want) {
		t.Errorf("changed backwards = %v, want %v", back, want)
	}
}

func TestAnIdenticalTreeChangesNothing(t *testing.T) {
	repository := newRepository(t)

	commit := commitFile(t, repository, "apps/web/page.tsx", "one")

	changed, err := ChangedFiles(repository, commit, commit)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("a commit against itself differs in nothing, got %v", changed)
	}
}

const affectedDockerfile = "FROM busybox:latest\nWORKDIR /app\nCOPY . .\nCMD [\"sh\", \"-c\", \"sleep 300\"]\n"

func affectedConfig(projectID string) string {
	return `{
      "version": 1,
      "id": "` + projectID + `",
      "name": "affected",
      "services": {
        "app": {
          "build": {"dockerfile": "services/app/Dockerfile"},
          "stateful": false,
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        },
        "worker": {
          "build": {"dockerfile": "services/worker/Dockerfile"},
          "stateful": false,
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }
    }`
}

func newAffectedRepository(t *testing.T, projectID string) string {
	t.Helper()

	repository := newRepository(t)
	writeFile(t, repository, "services/app/Dockerfile", affectedDockerfile)
	writeFile(t, repository, "services/worker/Dockerfile", affectedDockerfile)
	writeFile(t, repository, configFileName, affectedConfig(projectID))
	cleanUpProject(t, projectID)

	return repository
}

func deployCommit(t *testing.T, repository, destination, projectID string, affected bool) string {
	t.Helper()

	commit, err := ResolveCommit(repository, "HEAD")
	if err != nil {
		t.Fatalf("resolving HEAD: %v", err)
	}
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName(projectID, commit), "down").Run()
	})

	exitCode, err := RunDeploy(DeployOptions{
		Context:     repository,
		Destination: destination,
		Environment: defaultEnvironmentName,
		Affected:    affected,
	})
	if err != nil {
		t.Fatalf("RunDeploy (affected=%v): %v", affected, err)
	}
	if exitCode != exitOK {
		t.Fatalf("exit code = %d, want %d", exitCode, exitOK)
	}

	return commit
}

func imageID(t *testing.T, tag string) string {
	t.Helper()

	output, err := exec.Command("docker", "image", "inspect", tag, "--format", "{{.Id}}").Output()
	if err != nil {
		t.Fatalf("inspecting %s: %v", tag, err)
	}

	return strings.TrimSpace(string(output))
}

// Both services are built from the whole tree, so an ordinary deploy gives each
// of them a new image whenever anything at all changes. That is what makes an
// unchanged image id proof rather than coincidence, and it is why app moving in
// the same deploy is the control here. If docker's cache were handing back
// identical images regardless, app would not have moved either.
func TestAffectedKeepsTheImageOfAServiceTheChangeDidNotReach(t *testing.T) {
	dockerAvailable(t)

	const projectID = "dd000020"
	repository := newAffectedRepository(t, projectID)
	destination := t.TempDir()

	commitFile(t, repository, "services/app/version.txt", "one")
	first := deployCommit(t, repository, destination, projectID, false)

	appBefore := imageID(t, ImageTag(projectID, "app", first))
	workerBefore := imageID(t, ImageTag(projectID, "worker", first))

	commitFile(t, repository, "services/app/version.txt", "two")
	second := deployCommit(t, repository, destination, projectID, true)

	if got := imageID(t, ImageTag(projectID, "worker", second)); got != workerBefore {
		t.Errorf("worker was rebuilt, but nothing it is built from changed\n got %s\nwant %s", got, workerBefore)
	}
	if got := imageID(t, ImageTag(projectID, "app", second)); got == appBefore {
		t.Error("app should have been rebuilt, since the change was inside the directory it is built from")
	}

	// nothing is built from the repository root, so this change could be shared
	// code and the flag has to give up rather than guess
	commitFile(t, repository, "shared.txt", "three")
	third := deployCommit(t, repository, destination, projectID, true)

	if got := imageID(t, ImageTag(projectID, "worker", third)); got == workerBefore {
		t.Error("a change nobody is built from has to rebuild everything, since it could be shared code")
	}
}

// The fallbacks all end in the same place, which is a nil plan meaning build
// everything. None of them need docker, since the decision is made before any
// image is looked at.
func TestAffectedFallsBackToBuildingEverythingWhenItCannotAnswer(t *testing.T) {
	const contents = `{
      "version": 1, "id": "a3f19c02", "name": "lectern",
      "services": {"web": {"build": {"dockerfile": "apps/web/Dockerfile"}}}
    }`
	resolved := loadAndResolve(t, contents, defaultEnvironmentName)

	repository := newRepository(t)
	commit := commitFile(t, repository, "apps/web/page.tsx", "one")

	cases := []struct {
		name     string
		previous string
	}{
		{name: "nothing is deployed yet", previous: ""},
		{
			// rebased, force pushed, or garbage collected since it was deployed
			name:     "the running commit is not in this repository",
			previous: "9f4be0a",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// no image is ever inspected on these paths, so there is no builder to give
			reuse, err := PlanReuse(nil, resolved, repository, testCase.previous, commit)
			if err != nil {
				t.Fatalf("PlanReuse: %v", err)
			}
			if reuse != nil {
				t.Errorf("expected everything to be built, got a plan to reuse %v", reuse)
			}
		})
	}
}

// A layout --affected cannot answer safely is refused before anything is placed,
// rather than quietly deploying with the rule that keeps it honest disabled.
func TestAffectedRefusesALayoutWhereOneServiceHoldsAnother(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, "Dockerfile", affectedDockerfile)
	writeFile(t, repository, "services/app/Dockerfile", affectedDockerfile)
	writeFile(t, repository, configFileName, `{
      "version": 1, "id": "dd000023", "name": "nested",
      "services": {
        "everything": {"build": {"dockerfile": "Dockerfile"}, "stateful": false},
        "app": {"build": {"dockerfile": "services/app/Dockerfile"}, "stateful": false}
      }
    }`)
	commitFile(t, repository, "services/app/version.txt", "one")

	exitCode, err := RunDeploy(DeployOptions{
		Context:     repository,
		Destination: t.TempDir(),
		Environment: defaultEnvironmentName,
		Affected:    true,
	})
	if err == nil {
		t.Fatal("a nested layout must be refused rather than silently deploying")
	}
	if exitCode != exitPreconditionNotMet {
		t.Errorf("exit code = %d, want %d, since nothing was placed", exitCode, exitPreconditionNotMet)
	}
	if !strings.Contains(err.Error(), "everything") || !strings.Contains(err.Error(), "app") {
		t.Errorf("the error should name both services, got: %v", err)
	}
}

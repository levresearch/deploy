package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newBuilderForTest(t *testing.T, destinationArch string, remote, onDestination bool) *Builder {
	t.Helper()

	runner := newScriptedRunner()
	builder, err := NewBuilder(
		runner,
		DestinationFacts{Architecture: destinationArch},
		"a3f19c02", t.TempDir(),
		remote, onDestination,
	)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	return builder
}

// The decision is tested on its own, because building a real Builder for a
// foreign architecture needs QEMU, and whether the developer happens to have it
// installed must not decide whether this runs. CI has docker and no QEMU, which
// is exactly the machine that caught this.
func TestNeedsCrossBuild(t *testing.T) {
	cases := []struct {
		name        string
		local       string
		destination string
		want        bool
	}{
		{"same architecture", "amd64", "amd64", false},
		{"a pi from a laptop", "amd64", "arm64", true},
		{"a laptop from a pi", "arm64", "amd64", true},
		{"arm variants are still different", "arm64", "arm", true},
		// an unreadable destination architecture must not demand buildx for
		// nothing, since we do not know that it differs
		{"destination architecture unknown", "amd64", "", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := NeedsCrossBuild(testCase.local, testCase.destination); got != testCase.want {
				t.Errorf("NeedsCrossBuild(%q, %q) = %v, want %v",
					testCase.local, testCase.destination, got, testCase.want)
			}
		})
	}
}

func TestBuilderTargetsTheDestinationPlatform(t *testing.T) {
	local, err := LocalArchitecture(LocalRunner{})
	if err != nil {
		t.Skipf("docker is not available: %v", err)
	}

	// the matching case needs no cross toolchain, so it is the one that can be
	// built for real here
	builder := newBuilderForTest(t, local, false, false)

	if builder.crossBuilding {
		t.Error("the same architecture builds plainly")
	}
	if want := "linux/" + local; builder.targetPlatform != want {
		t.Errorf("targetPlatform = %q, want %q", builder.targetPlatform, want)
	}
}

func TestOnlyARemoteDestinationShipsImages(t *testing.T) {
	local, err := LocalArchitecture(LocalRunner{})
	if err != nil {
		t.Skipf("docker is not available: %v", err)
	}

	// a local destination shares this docker daemon, so shipping would be moving
	// an image to where it already is
	if builder := newBuilderForTest(t, local, false, false); builder.shipsImages {
		t.Error("a local destination should not ship images")
	}
	if builder := newBuilderForTest(t, local, true, false); !builder.shipsImages {
		t.Error("a remote destination runs its own daemon, so the image has to be sent")
	}
}

func TestBuildOnDestinationSkipsTheLocalToolchainEntirely(t *testing.T) {
	// deliberately a foreign architecture, which would normally demand buildx
	builder := newBuilderForTest(t, "riscv64", true, true)

	if builder.crossBuilding {
		t.Error("with the escape hatch nothing is cross built here")
	}
	if builder.shipsImages {
		t.Error("with the escape hatch the image is made where it runs, so nothing ships")
	}
	if !strings.Contains(builder.Describe(), "building on") {
		t.Errorf("Describe() = %q, should say where the build happens", builder.Describe())
	}
}

// This machine may well have a full buildx and QEMU, so the toolchain check is
// exercised against canned buildx output rather than whatever happens to be
// installed. The developer's setup should not decide whether the test runs.
func TestAMissingCrossToolchainNamesTheCommandThatFixesIt(t *testing.T) {
	cases := []struct {
		name         string
		buildxOutput string
		buildxFails  bool
		wantMentions []string
	}{
		{
			name:         "no builder at all",
			buildxFails:  true,
			wantMentions: []string{"buildx create", "docker-container", "arm64"},
		},
		{
			name:         "a builder that cannot reach the target platform",
			buildxOutput: "Name: default\nPlatforms: linux/amd64, linux/386\n",
			wantMentions: []string{"binfmt", "linux/arm64"},
		},
		{
			name:         "a builder that can",
			buildxOutput: "Name: multiarch\nPlatforms: linux/amd64, linux/arm64, linux/arm\n",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			local := newScriptedRunner()
			local.host = "this machine"
			if testCase.buildxFails {
				local.absentBinaries = []string{"docker buildx"}
				local.failCommands = []string{"buildx inspect"}
			} else {
				local.responses["buildx inspect"] = testCase.buildxOutput
			}

			builder := &Builder{
				local:             local,
				localArchitecture: "amd64",
				targetPlatform:    "linux/arm64",
			}

			err := builder.checkCrossToolchain("arm64")

			if len(testCase.wantMentions) == 0 {
				if err != nil {
					t.Fatalf("expected the toolchain to be accepted, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a target buildx cannot reach should be refused before anything is placed")
			}
			for _, want := range testCase.wantMentions {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error should name %q so it can be acted on, got: %v", want, err)
				}
			}
		})
	}
}

func TestArchitectureVerificationRejectsAMismatch(t *testing.T) {
	runner := newScriptedRunner()
	runner.responses["image inspect"] = "amd64\n"

	builder := &Builder{
		destination:    runner,
		projectID:      "a3f19c02",
		targetPlatform: "linux/arm64",
	}

	err := builder.verifyArchitecture(ImageTag("a3f19c02", "web", "9f4be0a"))
	if err == nil {
		t.Fatal("an image built for the wrong architecture must be caught here")
	}
	// the point of catching it here is that the alternative is a confusing
	// runtime failure much later
	if !strings.Contains(err.Error(), "exec format error") {
		t.Errorf("the error should explain what would otherwise happen, got: %v", err)
	}

	runner.responses["image inspect"] = "arm64\n"
	if err := builder.verifyArchitecture(ImageTag("a3f19c02", "web", "9f4be0a")); err != nil {
		t.Errorf("a matching architecture should pass, got: %v", err)
	}
}

// The build context is the git archive, so an untracked file cannot reach the
// image even though it sits right there in the working directory.
func TestBuildContextIsTheCommitAndNotTheWorkingTree(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, "Dockerfile", "FROM busybox:latest\nCOPY . /src\nCMD [\"true\"]\n")
	writeFile(t, repository, "tracked.txt", "committed\n")
	commit := commitFile(t, repository, "marker.txt", "in the commit\n")

	// present on disk, absent from the commit
	writeFile(t, repository, "untracked-secret.txt", "must not be in the image\n")

	local, err := LocalArchitecture(LocalRunner{})
	if err != nil {
		t.Skipf("docker: %v", err)
	}

	builder, err := NewBuilder(
		LocalRunner{}, DestinationFacts{Architecture: local},
		"dd000009", repository, false, false,
	)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	tag := ImageTag("dd000009", "app", commit)
	t.Cleanup(func() { exec.Command("docker", "image", "rm", "-f", tag).Run() })

	if err := builder.Build(repository, commit, "app", "Dockerfile", nil); err != nil {
		t.Fatalf("Build: %v", err)
	}

	listing, err := exec.Command("docker", "run", "--rm", tag, "ls", "/src").Output()
	if err != nil {
		t.Fatalf("listing the image contents: %v", err)
	}

	if !strings.Contains(string(listing), "marker.txt") {
		t.Errorf("the committed tree should be in the image, got %q", listing)
	}
	if strings.Contains(string(listing), "untracked-secret.txt") {
		t.Errorf("an untracked file reached the image, got %q", listing)
	}
	if strings.Contains(string(listing), ".git") {
		t.Errorf("the .git directory reached the image, got %q", listing)
	}
}

func TestCrossArchitectureBuildProducesAForeignImage(t *testing.T) {
	dockerAvailable(t)

	local, err := LocalArchitecture(LocalRunner{})
	if err != nil {
		t.Skipf("docker: %v", err)
	}

	foreign := "arm64"
	if local == "arm64" {
		foreign = "amd64"
	}

	repository := newRepository(t)
	writeFile(t, repository, "Dockerfile", "FROM busybox:latest\nCMD [\"true\"]\n")
	commit := commitFile(t, repository, "marker.txt", "x")

	builder, err := NewBuilder(
		LocalRunner{}, DestinationFacts{Architecture: foreign},
		"dd00000a", repository, false, false,
	)
	if err != nil {
		t.Skipf("no cross build toolchain on this machine: %v", err)
	}

	tag := ImageTag("dd00000a", "app", commit)
	t.Cleanup(func() {
		exec.Command("docker", "image", "rm", "-f", tag).Run()
		os.RemoveAll(buildCacheDirectory("dd00000a"))
	})

	if err := builder.Build(repository, commit, "app", "Dockerfile", nil); err != nil {
		t.Fatalf("cross build: %v", err)
	}

	built, err := exec.Command("docker", "image", "inspect", tag, "--format", "{{.Architecture}}").Output()
	if err != nil {
		t.Fatalf("inspecting the built image: %v", err)
	}
	if got := strings.TrimSpace(string(built)); got != foreign {
		t.Errorf("built for %q, want %q", got, foreign)
	}

	// the layer cache is what makes a second build cheap, and it lives outside the
	// repository so it cannot make the working tree dirty
	cache := filepath.Join(buildCacheDirectory("dd00000a"), "app")
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("expected a layer cache at %s: %v", cache, err)
	}
	if strings.HasPrefix(cache, repository) {
		t.Errorf("the cache must not be inside the repository, got %s", cache)
	}
}

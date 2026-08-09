package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
)

// buildxBuilderHint and qemuHint are printed verbatim, because a FATAL that names
// the exact command is the difference between a fix and a search.
const (
	buildxBuilderHint = "docker buildx create --use --name multiarch --driver docker-container"
	qemuHint          = "docker run --privileged --rm tonistiigi/binfmt --install %s"
)

// Builder decides how an image gets made and how it reaches the destination.
// Everything it needs is settled before the first build runs, so a missing
// toolchain fails before a release is placed rather than halfway through one.
type Builder struct {
	local             Runner
	destination       Runner
	projectID         string
	localArchitecture string
	targetPlatform    string
	crossBuilding     bool
	shipsImages       bool
	onDestination     bool
	cacheDirectory    string
	releaseDirectory  string
}

// SetReleaseDirectory tells the builder where the placed tree is, which only the
// build-on-destination path needs since every other path streams its context.
func (builder *Builder) SetReleaseDirectory(directory string) {
	builder.releaseDirectory = directory
}

func NewBuilder(
	destinationRunner Runner,
	facts DestinationFacts,
	projectID, repositoryPath string,
	remote, buildOnDestination bool,
) (*Builder, error) {
	local := LocalRunner{}

	localArchitecture, err := LocalArchitecture(local)
	if err != nil {
		return nil, err
	}

	builder := &Builder{
		local:             local,
		destination:       destinationRunner,
		projectID:         projectID,
		localArchitecture: localArchitecture,
		targetPlatform:    "linux/" + facts.Architecture,
		crossBuilding:     facts.Architecture != "" && facts.Architecture != localArchitecture,
		// a local destination shares this docker daemon, so a built image is
		// already where it needs to be
		shipsImages:    remote,
		onDestination:  buildOnDestination,
		cacheDirectory: path.Join(repositoryPath, ".deploy", "cache"),
	}

	// with the escape hatch the destination does its own building, so none of the
	// local toolchain matters and nothing is shipped
	if buildOnDestination {
		builder.crossBuilding = false
		builder.shipsImages = false

		return builder, nil
	}

	if builder.crossBuilding {
		if err := builder.checkCrossToolchain(facts.Architecture); err != nil {
			return nil, err
		}
	}

	return builder, nil
}

func LocalArchitecture(runner Runner) (string, error) {
	output, err := runner.Run([]string{"docker", "version", "--format", "{{.Server.Arch}}"})
	if err != nil {
		return "", fmt.Errorf("cannot read this machine's docker architecture: %s", firstLine(output))
	}

	return strings.TrimSpace(string(output)), nil
}

// checkCrossToolchain asks buildx what it can actually target, rather than
// assuming a builder exists. The default docker driver cannot build for a
// foreign platform at all, and without QEMU registered the target is simply
// absent from the list.
func (builder *Builder) checkCrossToolchain(targetArchitecture string) error {
	output, err := builder.local.Run([]string{"docker", "buildx", "inspect", "--bootstrap"})
	if err != nil {
		return fmt.Errorf(
			"the destination is %s and this machine is %s, so deploy needs a buildx builder here. make one with:\n  %s",
			targetArchitecture, builder.localArchitecture, buildxBuilderHint,
		)
	}

	if !strings.Contains(string(output), builder.targetPlatform) {
		return fmt.Errorf(
			"buildx cannot build for %s on this machine, which usually means QEMU is not registered. install it with:\n  "+qemuHint+"\nif that is already done, check the builder uses the docker-container driver:\n  %s",
			builder.targetPlatform, targetArchitecture, buildxBuilderHint,
		)
	}

	return nil
}

func (builder *Builder) Describe() string {
	if builder.onDestination {
		return "building on " + builder.destination.Describe()
	}
	if builder.crossBuilding {
		return fmt.Sprintf("%s -> %s via buildx", builder.localArchitecture, builder.targetPlatform)
	}

	return builder.localArchitecture
}

// Build makes one image from the tracked tree at a commit and puts it where the
// destination can run it.
func (builder *Builder) Build(repositoryPath, commit, serviceName, dockerfile string) error {
	tag := ImageTag(builder.projectID, serviceName, commit)
	fmt.Printf("  building %s (%s)\n", tag, builder.Describe())

	if builder.onDestination {
		return builder.buildOnDestination(commit, serviceName, dockerfile, tag)
	}

	if err := builder.runBuild(repositoryPath, commit, serviceName, dockerfile, tag); err != nil {
		return fmt.Errorf("building %s: %w", serviceName, err)
	}

	if builder.shipsImages {
		if err := builder.shipImage(tag); err != nil {
			return err
		}
	}

	return builder.verifyArchitecture(tag)
}

// buildOnDestination builds from the release tree already placed there, which is
// why it is the one path that does not stream a context.
func (builder *Builder) buildOnDestination(commit, serviceName, dockerfile, tag string) error {
	releaseDirectory := builder.releaseDirectory
	build := []string{
		"docker", "build",
		"--file", path.Join(releaseDirectory, dockerfile),
		"--tag", tag,
		releaseDirectory,
	}
	if err := builder.destination.Stream(build, os.Stdout); err != nil {
		return fmt.Errorf("building %s on %s: %w", serviceName, builder.destination.Describe(), err)
	}

	return builder.verifyArchitecture(tag)
}

// runBuild feeds the git archive in as the build context rather than pointing at
// the working directory, so the image and the release tree cannot disagree about
// what they contain.
func (builder *Builder) runBuild(repositoryPath, commit, serviceName, dockerfile, tag string) error {
	command := []string{"docker"}

	if builder.crossBuilding {
		cache := path.Join(builder.cacheDirectory, serviceName)
		command = append(command,
			"buildx", "build",
			"--platform", builder.targetPlatform,
			"--cache-to", "type=local,dest="+cache+",mode=max",
			"--output", "type=docker",
		)
		// reading a cache that was never written warns on every first build, and
		// a warning nobody can act on trains people to ignore warnings
		if _, err := os.Stat(path.Join(cache, "index.json")); err == nil {
			command = append(command, "--cache-from", "type=local,src="+cache)
		}
	} else {
		command = append(command, "build")
	}

	command = append(command, "--file", dockerfile, "--tag", tag, "-")

	archive, archiveFailed := startArchive(repositoryPath, commit)
	defer archive.Close()

	build := exec.Command(command[0], command[1:]...)
	build.Stdin = archive
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr

	buildErr := build.Run()
	if err := <-archiveFailed; err != nil {
		return err
	}

	return buildErr
}

// shipImage moves a finished image to a destination that runs its own docker
// daemon. No registry, just a tarball down the connection already open.
func (builder *Builder) shipImage(tag string) error {
	save := exec.Command("docker", "save", tag)

	stream, err := save.StdoutPipe()
	if err != nil {
		return err
	}

	var saveFailure bytes.Buffer
	save.Stderr = &saveFailure

	if err := save.Start(); err != nil {
		return err
	}

	loadErr := builder.destination.Pipe([]string{"docker", "load"}, stream)

	if err := save.Wait(); err != nil {
		return fmt.Errorf("saving %s: %s", tag, strings.TrimSpace(saveFailure.String()))
	}
	if loadErr != nil {
		return fmt.Errorf("loading %s onto %s: %w", tag, builder.destination.Describe(), loadErr)
	}
	fmt.Printf("  loaded %s onto %s\n", tag, builder.destination.Describe())

	return nil
}

// verifyArchitecture catches a misconfigured buildx here rather than letting it
// surface much later as exec format error at container start, which reads like an
// application bug and sends you looking in the wrong place entirely.
func (builder *Builder) verifyArchitecture(tag string) error {
	output, err := builder.destination.Run(
		[]string{"docker", "image", "inspect", tag, "--format", "{{.Architecture}}"},
	)
	if err != nil {
		return fmt.Errorf("cannot inspect %s on %s: %s", tag, builder.destination.Describe(), firstLine(output))
	}

	built := strings.TrimSpace(string(output))
	wanted := strings.TrimPrefix(builder.targetPlatform, "linux/")

	if wanted != "" && built != wanted {
		return fmt.Errorf(
			"%s was built for %s but %s runs %s, so it would fail at container start with exec format error",
			tag, built, builder.destination.Describe(), wanted,
		)
	}

	return nil
}

// startArchive streams the tracked tree at a commit, and reports its own failure
// separately so a broken archive is not mistaken for a broken build.
func startArchive(repositoryPath, commit string) (*io.PipeReader, chan error) {
	reader, writer := io.Pipe()
	failed := make(chan error, 1)

	go func() {
		archive := exec.Command("git", "-C", repositoryPath, "archive", "--format=tar", commit)
		archive.Stdout = writer

		var failure bytes.Buffer
		archive.Stderr = &failure

		err := archive.Run()
		if err != nil {
			err = fmt.Errorf("git archive %s: %s", ShortCommit(commit), strings.TrimSpace(failure.String()))
		}
		writer.CloseWithError(err)
		failed <- err
	}()

	return reader, failed
}

package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
	"time"
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
		crossBuilding:     NeedsCrossBuild(localArchitecture, facts.Architecture),
		// a local destination shares this docker daemon, so a built image is
		// already where it needs to be
		shipsImages:    remote,
		onDestination:  buildOnDestination,
		cacheDirectory: buildCacheDirectory(projectID),
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

// buildCacheDirectory keeps the layer cache out of the repository. A cache inside
// the checkout makes the working tree dirty, which the next deploy refuses over a
// directory deploy created itself, and no amount of gitignoring makes writing
// into somebody's repo the right default.
func buildCacheDirectory(projectID string) string {
	base, err := os.UserCacheDir()
	if err != nil {
		return path.Join(os.TempDir(), "deploy-cache", projectID)
	}

	return path.Join(base, "deploy", projectID)
}

// NeedsCrossBuild is the architecture decision on its own, separate from the
// toolchain check that follows it. Separate because the decision is worth testing
// exhaustively and the check needs QEMU installed, and a test whose result
// depends on what the developer happens to have installed is not a test.
//
// An empty destination architecture means we could not read one, and treating
// that as foreign would demand buildx for no reason.
func NeedsCrossBuild(localArchitecture, destinationArchitecture string) bool {
	return destinationArchitecture != "" && destinationArchitecture != localArchitecture
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
func (builder *Builder) Build(
	repositoryPath, commit, serviceName, dockerfile string,
	extraContext map[string][]byte,
) error {
	tag := ImageTag(builder.projectID, serviceName, commit)
	fmt.Printf("  building %s (%s)\n", tag, builder.Describe())

	if builder.onDestination {
		return builder.buildOnDestination(commit, serviceName, dockerfile, tag, extraContext)
	}

	if err := builder.runBuild(repositoryPath, commit, serviceName, dockerfile, tag, extraContext); err != nil {
		return fmt.Errorf("building %s: %w", serviceName, err)
	}

	if builder.shipsImages {
		if err := builder.shipImage(tag); err != nil {
			return err
		}
	}

	return builder.verifyArchitecture(tag)
}

// HasImage answers whether the destination can still run an image an earlier
// commit built, which retention or somebody pruning by hand may have taken since.
func (builder *Builder) HasImage(serviceName, commit string) bool {
	_, err := builder.destination.Run(
		[]string{"docker", "image", "inspect", ImageTag(builder.projectID, serviceName, commit)},
	)

	return err == nil
}

// Reuse points this commit's tag at the image an earlier commit already built.
// Everything downstream keeps naming images by the commit being deployed, so
// compose, rollback, and pruning never learn that a build was skipped.
func (builder *Builder) Reuse(serviceName, from, commit string) error {
	existing := ImageTag(builder.projectID, serviceName, from)
	tag := ImageTag(builder.projectID, serviceName, commit)

	if output, err := builder.destination.Run([]string{"docker", "tag", existing, tag}); err != nil {
		return fmt.Errorf("reusing %s as %s: %s", existing, tag, firstLine(output))
	}
	fmt.Printf("  reusing %s as %s\n", existing, tag)

	return nil
}

// buildOnDestination builds from the release tree already placed there, which is
// why it is the one path that does not stream a context.
func (builder *Builder) buildOnDestination(
	commit, serviceName, dockerfile, tag string,
	extraContext map[string][]byte,
) error {
	releaseDirectory := builder.releaseDirectory

	// the context there is the placed tree, so a generated Dockerfile is written
	// beside it rather than injected into a stream
	for name, contents := range extraContext {
		if err := builder.destination.WriteFile(path.Join(releaseDirectory, name), contents); err != nil {
			return err
		}
	}
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
func (builder *Builder) runBuild(
	repositoryPath, commit, serviceName, dockerfile, tag string,
	extraContext map[string][]byte,
) error {
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

	context, injectFailed := injectIntoContext(archive, extraContext)
	defer context.Close()

	build := exec.Command(command[0], command[1:]...)
	build.Stdin = context
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr

	buildErr := build.Run()
	if err := <-archiveFailed; err != nil {
		return err
	}
	if err := <-injectFailed; err != nil {
		return err
	}

	return buildErr
}

// injectIntoContext passes the archive straight through when there is nothing to
// add, so the ordinary path pays nothing for a feature it does not use.
func injectIntoContext(archive io.Reader, extra map[string][]byte) (io.ReadCloser, chan error) {
	failed := make(chan error, 1)

	if len(extra) == 0 {
		failed <- nil
		return io.NopCloser(archive), failed
	}

	reader, writer := io.Pipe()
	go func() {
		err := addToTar(archive, writer, extra)
		writer.CloseWithError(err)
		failed <- err
	}()

	return reader, failed
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

// addToTar copies a tar stream through and appends extra files at the end. A
// generated Dockerfile has to be inside the build context, and the context here
// is a stream, so it is added on the way past rather than written into anybody's
// checkout.
func addToTar(source io.Reader, destination io.Writer, extra map[string][]byte) error {
	reader := tar.NewReader(source)
	writer := tar.NewWriter(destination)

	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the build context: %w", err)
		}
		if _, taken := extra[header.Name]; taken {
			// ours wins, so a stale file of the same name in the commit cannot
			// quietly shadow what deploy just generated
			continue
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if _, err := io.Copy(writer, reader); err != nil {
			return fmt.Errorf("copying %s into the build context: %w", header.Name, err)
		}
	}

	// tar stops at the end-of-archive marker, but git archive pads its output to a
	// full block after that. Leaving those bytes unread blocks git forever on a
	// pipe nobody is draining, which hangs the whole deploy rather than failing.
	// tar stops at the end-of-archive marker, but git archive pads its output to a
	// full block after that. Leaving those bytes unread blocks git forever on a
	// pipe nobody is draining, which hangs the whole deploy rather than failing.
	if _, err := io.Copy(io.Discard, source); err != nil {
		return fmt.Errorf("draining the build context: %w", err)
	}

	for _, name := range slices.Sorted(maps.Keys(extra)) {
		contents := extra[name]
		header := &tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(contents)),
			ModTime: time.Now(),
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if _, err := writer.Write(contents); err != nil {
			return err
		}
	}

	return writer.Close()
}

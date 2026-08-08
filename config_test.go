package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const staticSiteConfig = `{
  "version": 1,
  "id": "7c41e9b8",
  "name": "portfolio",
  "services": {
    "site": {
      "build": { "dockerfile": "Dockerfile" },
      "stateful": false,
      "healthcheck": { "command": ["CMD", "wget", "-qO-", "http://localhost/"] },
      "host": {
        "domain": "example.com",
        "port": 80,
        "tunnelTokenFrom": "PORTFOLIO_TUNNEL_TOKEN"
      }
    }
  }
}`

const minecraftConfig = `{
  "version": 1,
  "id": "b2d7f014",
  "name": "smp",
  "retention": 2,
  "services": {
    "mc": {
      "image": "itzg/minecraft-server:java21",
      "stateful": true,
      "env": [".env"],
      "ports": ["25565:25565"],
      "volumes": ["world:/data"],
      "healthcheck": { "command": ["CMD", "mc-health"], "startPeriod": "2m" }
    }
  }
}`

const workerConfig = `{
  "version": 1,
  "id": "e58a3d7f",
  "name": "distrib",
  "retention": 5,
  "services": {
    "api": {
      "build": { "dockerfile": "Dockerfile" },
      "stateful": false,
      "healthcheck": { "command": ["CMD", "/app", "healthcheck"] },
      "dependsOn": { "redis": "healthy" },
      "host": {
        "domain": "api.example.com",
        "port": 8787,
        "tunnelTokenFrom": "DISTRIB_TUNNEL_TOKEN"
      }
    },
    "worker": {
      "build": { "dockerfile": "Dockerfile.worker" },
      "stateful": false,
      "healthcheck": { "command": ["CMD", "/worker", "healthcheck"] }
    },
    "redis": {
      "image": "redis:8-alpine",
      "stateful": true,
      "volumes": ["redisdata:/data"],
      "healthcheck": { "command": ["CMD", "redis-cli", "ping"] }
    }
  }
}`

const lecternConfig = `{
  "version": 1,
  "id": "a3f19c02",
  "name": "lectern",
  "retention": 3,
  "services": {
    "web": {
      "build": { "dockerfile": "Dockerfile" },
      "stateful": false,
      "env": [".env"],
      "command": "pnpm --filter web start",
      "healthcheck": { "command": ["CMD", "curl", "-f", "http://localhost:3000/health"] }
    },
    "pg": {
      "image": "postgres:17",
      "stateful": true,
      "volumes": ["pgdata:/var/lib/postgresql/data"],
      "healthcheck": { "command": ["CMD-SHELL", "pg_isready -U postgres"] }
    }
  },
  "environments": {
    "production": {
      "services": {
        "web": {
          "env": [".env.production"],
          "host": {
            "domain": "lectern.example.com",
            "port": 3000,
            "tunnelTokenFrom": "CLOUDFLARE_TUNNEL_TOKEN"
          }
        }
      }
    },
    "development": {
      "services": {
        "web": {
          "env": [".env.development"],
          "ports": ["3000:3000"]
        }
      }
    }
  }
}`

func writeConfig(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), configFileName)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	return path
}

func loadAndResolve(t *testing.T, contents, environmentName string) ResolvedProject {
	t.Helper()

	project, err := LoadProject(writeConfig(t, contents))
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	resolved, err := project.ResolveEnvironment(environmentName)
	if err != nil {
		t.Fatalf("resolving environment: %v", err)
	}

	return resolved
}

func TestExampleConfigsLoadAndValidate(t *testing.T) {
	cases := []struct {
		name        string
		contents    string
		environment string
		services    int
		retention   int
	}{
		{"static site", staticSiteConfig, defaultEnvironmentName, 1, defaultRetention},
		{"minecraft", minecraftConfig, defaultEnvironmentName, 1, 2},
		{"api with worker", workerConfig, defaultEnvironmentName, 3, 5},
		{"lectern production", lecternConfig, "production", 2, 3},
		{"lectern development", lecternConfig, "development", 2, 3},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resolved := loadAndResolve(t, testCase.contents, testCase.environment)

			if err := resolved.Validate(); err != nil {
				t.Fatalf("expected a valid config, got:\n%v", err)
			}
			if len(resolved.Services) != testCase.services {
				t.Errorf("services = %d, want %d", len(resolved.Services), testCase.services)
			}
			if resolved.Retention != testCase.retention {
				t.Errorf("retention = %d, want %d", resolved.Retention, testCase.retention)
			}
		})
	}
}

func TestEnvironmentOverrideMergesRatherThanReplaces(t *testing.T) {
	production := loadAndResolve(t, lecternConfig, "production").Services["web"]

	if len(production.Build) == 0 {
		t.Error("build came from the base service and should have survived the override")
	}
	if len(production.Healthcheck) == 0 {
		t.Error("healthcheck came from the base service and should have survived the override")
	}
	if _, kept := production.Extra["command"]; !kept {
		t.Error("an unowned base key should survive the override")
	}
	if want := []string{".env.production"}; len(production.Env) != 1 || production.Env[0] != want[0] {
		t.Errorf("env = %v, want %v", production.Env, want)
	}
	if production.Host == nil {
		t.Fatal("production adds a host block")
	}
	if production.Host.Domain != "lectern.example.com" {
		t.Errorf("host domain = %q", production.Host.Domain)
	}

	development := loadAndResolve(t, lecternConfig, "development").Services["web"]

	if development.Host != nil {
		t.Error("development declares no host block, so it must not inherit one from production")
	}
	if _, published := development.Extra["ports"]; !published {
		t.Error("development adds a ports key")
	}
}

func TestBaseServicesResolveWithNoEnvironmentsBlock(t *testing.T) {
	resolved := loadAndResolve(t, staticSiteConfig, defaultEnvironmentName)

	if _, found := resolved.Services["site"]; !found {
		t.Fatal("a config with no environments block resolves to its base services")
	}
}

func TestUnknownEnvironmentIsRefusedAndListsTheRealOnes(t *testing.T) {
	project, err := LoadProject(writeConfig(t, lecternConfig))
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}

	_, err = project.ResolveEnvironment("staging")
	if err == nil {
		t.Fatal("expected an unknown environment to be refused")
	}
	for _, want := range []string{"staging", "development", "production"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestUnknownConfigVersionIsRefused(t *testing.T) {
	_, err := LoadProject(writeConfig(t, `{"version": 99, "id": "a3f19c02", "name": "x"}`))
	if err == nil {
		t.Fatal("expected version 99 to be refused")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("error should name the version it found, got: %v", err)
	}
}

func TestUnownedKeysSurviveARoundTrip(t *testing.T) {
	const contents = `{
      "version": 1,
      "id": "a3f19c02",
      "name": "passthrough",
      "services": {
        "web": {
          "image": "nginx",
          "command": "nginx -g 'daemon off;'",
          "restart": "unless-stopped",
          "logging": { "driver": "json-file", "options": { "max-size": "10m" } },
          "cap_add": ["NET_ADMIN"]
        }
      }
    }`

	resolved := loadAndResolve(t, contents, defaultEnvironmentName)

	encoded, err := json.Marshal(resolved.Services["web"])
	if err != nil {
		t.Fatalf("marshalling service: %v", err)
	}

	var reparsed Service
	if err := json.Unmarshal(encoded, &reparsed); err != nil {
		t.Fatalf("reparsing service: %v", err)
	}

	if reparsed.Image != "nginx" {
		t.Errorf("image = %q, want nginx", reparsed.Image)
	}
	for _, key := range []string{"command", "restart", "logging", "cap_add"} {
		if _, kept := reparsed.Extra[key]; !kept {
			t.Errorf("key %q did not survive the round trip", key)
		}
	}
	if driver := string(reparsed.Extra["logging"]); !strings.Contains(driver, "max-size") {
		t.Errorf("nested values should survive verbatim, got %s", driver)
	}
}

func TestValidationRejects(t *testing.T) {
	cases := []struct {
		name     string
		services string
		want     string
	}{
		{
			name:     "host without a healthcheck",
			services: `"web": {"image": "nginx", "host": {"domain": "a.com", "port": 80, "tunnelTokenFrom": "T"}}`,
			want:     "needs a healthcheck",
		},
		{
			name:     "host on a stateful service",
			services: `"db": {"image": "postgres:17", "stateful": true, "healthcheck": {}, "host": {"domain": "a.com", "port": 80, "tunnelTokenFrom": "T"}}`,
			want:     "cannot be hosted",
		},
		{
			name:     "host that also publishes ports",
			services: `"web": {"image": "nginx", "healthcheck": {}, "ports": ["80:80"], "host": {"domain": "a.com", "port": 80, "tunnelTokenFrom": "T"}}`,
			want:     "must not publish ports",
		},
		{
			name:     "host with no tunnel token name",
			services: `"web": {"image": "nginx", "healthcheck": {}, "host": {"domain": "a.com", "port": 80}}`,
			want:     "tunnelTokenFrom",
		},
		{
			name:     "both image and build",
			services: `"web": {"image": "nginx", "build": {"dockerfile": "Dockerfile"}}`,
			want:     "pick one",
		},
		{
			name:     "neither image nor build",
			services: `"web": {"stateful": false}`,
			want:     "neither image nor build",
		},
		{
			name:     "unknown dependsOn condition",
			services: `"web": {"image": "nginx"}, "db": {"image": "postgres:17", "dependsOn": {"web": "ready"}}`,
			want:     "expected one of",
		},
		{
			name:     "dependsOn a service that does not exist",
			services: `"web": {"image": "nginx", "dependsOn": {"ghost": "healthy"}}`,
			want:     "does not define",
		},
		{
			name:     "unusable service name",
			services: `"-web-": {"image": "nginx"}`,
			want:     "not a usable docker service name",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			contents := `{"version": 1, "id": "a3f19c02", "name": "x", "services": {` + testCase.services + `}}`
			resolved := loadAndResolve(t, contents, defaultEnvironmentName)

			err := resolved.Validate()
			if err == nil {
				t.Fatal("expected this config to be rejected")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("error should contain %q, got:\n%v", testCase.want, err)
			}
		})
	}
}

func TestValidationRejectsProjectLevelMistakes(t *testing.T) {
	cases := []struct {
		name     string
		contents string
		want     string
	}{
		{
			name:     "id is not 8 hex characters",
			contents: `{"version": 1, "id": "nope", "name": "x", "services": {"web": {"image": "nginx"}}}`,
			want:     "8 hex characters",
		},
		{
			name:     "no name",
			contents: `{"version": 1, "id": "a3f19c02", "services": {"web": {"image": "nginx"}}}`,
			want:     "name is required",
		},
		{
			name:     "no services",
			contents: `{"version": 1, "id": "a3f19c02", "name": "x", "services": {}}`,
			want:     "no services defined",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resolved := loadAndResolve(t, testCase.contents, defaultEnvironmentName)

			err := resolved.Validate()
			if err == nil {
				t.Fatal("expected this config to be rejected")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("error should contain %q, got:\n%v", testCase.want, err)
			}
		})
	}
}

func TestValidationReportsEveryProblemAtOnce(t *testing.T) {
	const contents = `{"version": 1, "id": "bad", "services": {"web": {}}}`

	err := loadAndResolve(t, contents, defaultEnvironmentName).Validate()
	if err == nil {
		t.Fatal("expected this config to be rejected")
	}

	for _, want := range []string{"8 hex characters", "name is required", "neither image nor build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got:\n%v", want, err)
		}
	}
}

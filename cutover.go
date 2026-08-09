package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// publicVerifyTimeout is how long the hostname gets to answer. This one depends
// on Cloudflare and public DNS rather than anything local, so it is generous and
// it gets its own error message.
const networkProbeImage = "busybox:latest"

// A var rather than a const so a test can shorten it. A unit test that waits the
// real twenty seconds for a hostname that will never answer is a unit test
// nobody runs.
var (
	publicVerifyTimeout  = 20 * time.Second
	publicVerifyInterval = time.Second
)

// TunnelServiceName is the cloudflared that fronts one hosted service. One per
// host block, because each token is bound to its own tunnel.
func TunnelServiceName(serviceName string) string {
	return "cloudflared-" + serviceName
}

// renderTunnels builds a cloudflared for every host block. They live in the
// shared stack, never in a release stack, because one inside a per-commit project
// would restart on every deploy and drop the tunnel, which is the outage the
// cutover exists to prevent.
//
// The token reaches the container through compose interpolation from an env file,
// so the value never lands in the rendered compose and never appears in argv. The
// name in tunnelTokenFrom is all deploy ever handles.
func renderTunnels(resolved ResolvedProject) (map[string]json.RawMessage, error) {
	tunnels := map[string]json.RawMessage{}

	for _, name := range HostedServices(resolved.Services) {
		host := resolved.Services[name].Host

		fields := map[string]any{
			"image":   "cloudflare/cloudflared:latest",
			"command": []string{"tunnel", "--no-autoupdate", "run"},
			"environment": map[string]string{
				"TUNNEL_TOKEN": fmt.Sprintf(
					"${%s:?%s needs a tunnel token. put %s in an env file and push it with deploy env push}",
					host.TunnelTokenFrom, name, host.TunnelTokenFrom,
				),
			},
			"restart": "unless-stopped",
		}

		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		tunnels[TunnelServiceName(name)] = encoded
	}

	return tunnels, nil
}

// Cutover moves traffic from the release that was serving to the one just
// started. The spike measured how this actually behaves, and the whole sequence
// follows from it.
//
// cloudflared pins to one origin and does not round robin, so the new release is
// already holding the network alias and receiving nothing. Traffic moves when the
// old container stops answering, and stopping it beats disconnecting it, which
// blackholes whatever was in flight. Zero failures in 500 requests, against two.
func Cutover(
	runner Runner,
	resolved ResolvedProject,
	layout Layout,
	superseded, commit string,
) error {
	if superseded == "" || superseded == ShortCommit(commit) {
		return nil
	}

	hosted := HostedServices(resolved.Services)
	supersededCompose := path.Join(layout.Release(superseded), composeFileName)
	supersededProject := ProjectName(resolved.ID, superseded)

	// nothing is exposed, so there is no tunnel to verify through and the release
	// that was serving is simply replaced
	if len(hosted) == 0 {
		stopRelease(runner, resolved.ID, superseded, supersededCompose)
		fmt.Printf("  stopped %s, which this release replaces\n", supersededProject)

		return nil
	}

	// reachable by the name and port cloudflared will use, which is a different
	// question from the healthcheck compose already ran inside the container
	for _, name := range hosted {
		host := resolved.Services[name].Host
		if err := reachableOnNetwork(runner, NetworkName(resolved.ID), name, host.Port); err != nil {
			return fmt.Errorf(
				"%s is up but not reachable at %s:%d on the shared network, so the tunnel would not find it either: %w",
				name, name, host.Port, err,
			)
		}
	}
	fmt.Printf("  %s reachable on the shared network\n", strings.Join(hosted, ", "))

	// this is the cutover. traffic moves because the old container stops
	// answering, not because anything was pointed anywhere
	if _, err := runner.Run([]string{
		"docker", "compose", "--file", supersededCompose,
		"--project-name", supersededProject, "stop",
	}); err != nil {
		return fmt.Errorf("stopping %s to cut over: %w", supersededProject, err)
	}
	fmt.Printf("  stopped %s, traffic is moving\n", supersededProject)

	if err := verifyHosted(resolved, hosted); err != nil {
		// the old containers are stopped rather than removed, so starting them
		// again is what puts traffic back, for the same reason stopping them took
		// it away
		fmt.Fprintf(os.Stderr, "  public verification failed, starting %s again\n", supersededProject)

		if _, startErr := runner.Run([]string{
			"docker", "compose", "--file", supersededCompose,
			"--project-name", supersededProject, "start",
		}); startErr != nil {
			return fmt.Errorf(
				"%w, and the previous release could not be started again either, so nothing is serving: %v",
				err, startErr,
			)
		}

		return err
	}

	// only now, once the public hostname has answered from the new release
	stopRelease(runner, resolved.ID, superseded, supersededCompose)
	fmt.Printf("  removed %s\n", supersededProject)

	return nil
}

// reachableOnNetwork asks from another container on the same network, because
// that is where cloudflared asks from. A tiny image and a tcp connect, since a
// status code would make a 404 look like a failure.
func reachableOnNetwork(runner Runner, network, serviceName string, port int) error {
	probe := []string{
		"docker", "run", "--rm", "--network", network, networkProbeImage,
		"sh", "-c", fmt.Sprintf("nc -z -w 3 %s %d", serviceName, port),
	}

	var lastFailure error
	for attempt := range 5 {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		if output, err := runner.Run(probe); err != nil {
			lastFailure = fmt.Errorf("%s", firstLine(output))
			continue
		}

		return nil
	}

	return lastFailure
}

// verifyHosted checks the thing a user would check, which is the public hostname
// over the internet. It gets its own timeout and its own error, because
// "tunnel verify failed" and "container unhealthy" are different problems.
func verifyHosted(resolved ResolvedProject, hosted []string) error {
	for _, name := range hosted {
		domain := resolved.Services[name].Host.Domain
		if err := verifyPublicHostname(domain); err != nil {
			return err
		}
		fmt.Printf("  https://%s answered\n", domain)
	}

	return nil
}

func verifyPublicHostname(domain string) error {
	client := &http.Client{Timeout: publicVerifyInterval * 5}
	address := "https://" + domain

	deadline := time.Now().Add(publicVerifyTimeout)
	var lastFailure string

	for attempt := 0; time.Now().Before(deadline); attempt++ {
		if attempt > 0 {
			time.Sleep(publicVerifyInterval)
		}

		response, err := client.Get(address)
		if err != nil {
			lastFailure = err.Error()
			continue
		}
		response.Body.Close()

		// any answer at all means the tunnel reached an origin, and which status
		// an application returns is its own business
		if response.StatusCode < 500 {
			return nil
		}
		lastFailure = fmt.Sprintf("status %d", response.StatusCode)
	}

	return fmt.Errorf(
		"%s did not answer within %s (%s). this is Cloudflare and public dns rather than the container, which was already reachable on the network",
		address, publicVerifyTimeout, lastFailure,
	)
}

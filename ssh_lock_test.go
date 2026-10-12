package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newSSHTestRunner(t *testing.T) (*SSHRunner, string, int) {
	t.Helper()
	if testing.Short() {
		t.Skip("isolated ssh daemon runs in the full suite")
	}
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Fatal("full suite requires openssh-server for remote locking acceptance")
	}
	directory := t.TempDir()
	for _, name := range []string{"host", "client"} {
		if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(directory, name)).CombinedOutput(); err != nil {
			t.Fatalf("test key generation: %v %s", err, output)
		}
	}
	public, err := os.ReadFile(filepath.Join(directory, "client.pub"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, directory, "authorized_keys", string(public))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("Port %d\nListenAddress 127.0.0.1\nHostKey %s\nAuthorizedKeysFile %s\nPidFile %s\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\nStrictModes no\nPermitRootLogin prohibit-password\nAllowUsers %s\nLogLevel ERROR\n", port, filepath.Join(directory, "host"), filepath.Join(directory, "authorized_keys"), filepath.Join(directory, "pid"), account.Username)
	writeFile(t, directory, "sshd_config", config)
	logfile, err := os.Create(filepath.Join(directory, "sshd.log"))
	if err != nil {
		t.Fatal(err)
	}
	daemon := exec.Command(sshd, "-D", "-e", "-f", filepath.Join(directory, "sshd_config"))
	daemon.Stderr = logfile
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.Process.Kill(); daemon.Wait(); logfile.Close() })
	address := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			connection.Close()
			break
		}
		if time.Now().After(deadline) {
			logs, _ := os.ReadFile(logfile.Name())
			t.Fatalf("test ssh daemon did not start: %s", logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	hostKey, err := os.ReadFile(filepath.Join(directory, "host.pub"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, directory, "known_hosts", fmt.Sprintf("[127.0.0.1]:%d %s", port, hostKey))
	sockets := filepath.Join(directory, "socket")
	if err := os.Mkdir(sockets, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &SSHRunner{host: account.Username + "@127.0.0.1", controlPath: filepath.Join(sockets, "control")}
	open := exec.Command("ssh", "-p", fmt.Sprint(port), "-i", filepath.Join(directory, "client"), "-o", "UserKnownHostsFile="+filepath.Join(directory, "known_hosts"), "-o", "StrictHostKeyChecking=yes", "-o", "BatchMode=yes", "-o", "ControlMaster=yes", "-o", "ControlPath="+runner.controlPath, "-N", "-f", runner.host)
	if output, err := open.CombinedOutput(); err != nil {
		t.Fatalf("test ssh connection: %v %s", err, output)
	}
	t.Cleanup(runner.Close)
	return runner, directory, port
}

func TestSSHLockLifetime(t *testing.T) {
	runner, directory, _ := newSSHTestRunner(t)
	layout := NewLayout(filepath.Join(directory, "destination"), "abcd1234")
	lock, err := AcquireLock(runner, layout, "aaaaaaa", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Release)
	if duplicate, err := AcquireLock(runner, layout, "bbbbbbb", true); err == nil {
		duplicate.Release()
		t.Fatal("ssh contender bypassed active lock")
	}
	lock.Release()
	again, err := AcquireLock(runner, layout, "bbbbbbb", false)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := runner.Run([]string{"cat", layout.LockFile()}); err != nil || !strings.Contains(string(output), "bbbbbbb") {
		t.Fatalf("remote metadata missing: %s %v", output, err)
	}
	// closing the transport must release the destination's held descriptor.
	runner.Close()
	<-again.exited
	local, err := AcquireLock(LocalRunner{}, layout, "ccccccc", false)
	if err != nil {
		t.Fatal(err)
	}
	local.Release()
}

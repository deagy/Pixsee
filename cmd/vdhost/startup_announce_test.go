package main

// R2 red-baseline subprocess tests: these drive the REAL vdhost startup path
// (built binary, actual stdout/stderr, actual exit code) and fail at 3983c4b
// because (a) no ephemeral-certificate fingerprint is published at startup
// (AC-7 / F6: the operator has no way to learn the pin value) and (b) the
// fail-closed auth gate does not exist (AC-6 / F4: a non-loopback bind with
// no token starts happily today).

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	vdhostBuildOnce sync.Once
	vdhostBin       string
	vdhostBuildLog  string
	vdhostBuildOK   bool
)

// buildVdhost compiles the real cmd/vdhost binary once per test run.
func buildVdhost(t *testing.T) string {
	t.Helper()
	vdhostBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "vdhost-bin")
		if err != nil {
			return
		}
		bin := filepath.Join(dir, "vdhost")
		cmd := exec.Command("go", "build", "-o", bin, "virtualdesktop/cmd/vdhost")
		out, err := cmd.CombinedOutput()
		vdhostBuildLog = string(out)
		if err == nil {
			vdhostBin, vdhostBuildOK = bin, true
		}
	})
	if !vdhostBuildOK {
		t.Fatalf("build vdhost: %s", vdhostBuildLog)
	}
	return vdhostBin
}

// sanitizeVdhostEnv clears inherited VDHOST_* configuration so the in-process
// config surface is exercised through flags only.
func sanitizeVdhostEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "VDHOST_") {
			key := strings.SplitN(kv, "=", 2)[0]
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), ".config"))
}

// childEnv returns a minimal sanitized environment for the child process:
// PATH for the toolchain, a fresh HOME/XDG so no user config file is picked
// up, and no DISPLAY — the startup gate and announcement never open X11.
func childEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
	}
}

// runVdhostToExit starts the binary, collects combined output, and returns
// (exit error, output). A nil exit error with a still-running child is
// reported as a timeout: ok=false and the partial output.
func runVdhostToExit(t *testing.T, timeout time.Duration, args ...string) (error, string, bool) {
	t.Helper()
	bin := buildVdhost(t)
	cmd := exec.Command(bin, args...)
	cmd.Dir = t.TempDir()
	cmd.Env = childEnv(t)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	var buf bytes.Buffer
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, stdout)
		close(copied)
	}()
	select {
	case <-copied:
		// EOF: the child closed its output (it exited). Reap it.
		exitErr := cmd.Wait()
		return exitErr, buf.String(), true
	case <-time.After(timeout):
		return nil, buf.String(), false
	}
}

// TestVdhostRefusesNonLoopbackBindWithoutToken is the AC-6 subprocess proof:
// the shipped host must exit non-zero — immediately, before serving anything
// — when bound to a non-loopback address with no authentication material,
// and the error must name the address. At 3983c4b the host starts and
// listens happily (F4: warning-only), so this is red.
func TestVdhostRefusesNonLoopbackBindWithoutToken(t *testing.T) {
	sanitizeVdhostEnv(t)
	exitErr, output, exited := runVdhostToExit(t, 5*time.Second, "-addr", "0.0.0.0:0")
	if !exited {
		t.Fatalf("vdhost did not refuse a tokenless non-loopback bind within 5s; it kept serving. output=%q (F4/AC-6: startup must be refused with a non-zero exit)", output)
	}
	if exitErr == nil {
		t.Fatalf("vdhost exited 0 on a tokenless non-loopback bind; AC-6 requires a non-zero refusal. output=%q", output)
	}
	if strings.Contains(output, "listening") {
		t.Fatalf("vdhost LISTENED on a tokenless non-loopback bind before refusing; output=%q", output)
	}
	joined := strings.ToLower(output)
	if !strings.Contains(joined, "0.0.0.0") || !strings.Contains(joined, "token") {
		t.Fatalf("startup refusal must name the bound address and the fix (set a token); output=%q", output)
	}
}

// fingerprintLineRE is the contract of the AC-7 startup announcement: the
// SHA-256 of the ephemeral leaf (64 lowercase hex) printed with a clear label
// and a reference to the client's -fingerprint flag.
var fingerprintLineRE = regexp.MustCompile(`(?i)ephemeral certificate sha-256 fingerprint:\s*([0-9a-f]{64})`)

// TestVdhostAnnouncesEphemeralCertFingerprint drives the real startup path in
// ephemeral-cert mode (no -ca/-key, loopback tokenless with -no-auth) and
// requires the host to publish the leaf-certificate SHA-256 fingerprint,
// labelled and referencing the client -fingerprint flag. At 3983c4b nothing
// like this is printed (F6: the pin is unusable because the value is never
// published) — today the child even dies on the unknown -no-auth flag, which
// is itself part of the same missing feature set. Both are runtime-red.
func TestVdhostAnnouncesEphemeralCertFingerprint(t *testing.T) {
	sanitizeVdhostEnv(t)
	bin := buildVdhost(t)
	cmd := exec.Command(bin, "-addr", "127.0.0.1:0", "-no-auth")
	cmd.Dir = t.TempDir()
	cmd.Env = childEnv(t)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	var all []string
	scanner := bufio.NewScanner(stdout)
	lines := make(chan string, 64)
	go func() {
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("no ephemeral fingerprint announcement within 15s of startup; output=%q (F6/AC-7)", strings.Join(all, " | "))
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("vdhost exited without announcing the ephemeral fingerprint; output so far=%q (F6/AC-7: the startup path must print the SHA-256 pin so clients can use -fingerprint)", strings.Join(all, " | "))
			}
			all = append(all, line)
			if m := fingerprintLineRE.FindStringSubmatch(line); m != nil {
				if !strings.Contains(strings.ToLower(strings.Join(all, "\n")), "-fingerprint") {
					t.Fatalf("fingerprint announcement %q does not reference the client -fingerprint flag (AC-7)", line)
				}
				return
			}
		}
	}
}

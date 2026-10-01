package main

// R2 red-baseline tests: the auth gate (spec AC-6, findings F4, resolved Q2)
// over the real startup config surface (loadConfig → buildConfigFromViper,
// the same path Cobra's RunE uses).
//
// Owner-locked behavior:
//  1. non-loopback bind + no configured token ⇒ refuse startup, error naming
//     the address and the fix (set -token). The -no-auth flag does NOT unlock
//     non-loopback binds.
//  2. tokenless on loopback requires the explicit -no-auth opt-in flag.
//  3. a file-backed token that is group-/other-accessible (mode & 0o077 != 0)
//     ⇒ refuse, naming chmod 0600.
//
// At 3983c4b none of these refusals exist (F4: the gate does not exist, only
// a stderr warning), so the wantErr rows fail red; the -no-auth flag itself
// does not exist yet, so the "accepted with flag" rows fail red too.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTokenMode writes a valid 32-byte token file and chmods it to an exact
// mode (os.WriteFile alone would mask the mode with the process umask).
func writeTokenMode(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(0x40 + i)
	}
	if err := os.WriteFile(path, token, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigAuthGate(t *testing.T) {
	sanitizeVdhostEnv(t)
	goodToken := writeTokenMode(t, 0o600)
	groupReadableToken := writeTokenMode(t, 0o644)
	groupWriteToken := writeTokenMode(t, 0o660)
	ownerReadonlyToken := writeTokenMode(t, 0o400)
	worldWriteToken := writeTokenMode(t, 0o606)

	cases := []struct {
		name       string
		args       []string
		wantErr    bool
		errContain []string
	}{
		// 1. Non-loopback + no token ⇒ refuse.
		{
			name:       "non-loopback without token refused",
			args:       []string{"-addr", "0.0.0.0:6511"},
			wantErr:    true,
			errContain: []string{"0.0.0.0:6511", "token"},
		},
		{
			name:       "wildcard port without token refused",
			args:       []string{"-addr", ":6511"},
			wantErr:    true,
			errContain: []string{":6511", "token"},
		},
		{
			name:       "routable v4 without token refused",
			args:       []string{"-addr", "10.1.2.3:6511"},
			wantErr:    true,
			errContain: []string{"10.1.2.3:6511", "token"},
		},
		{
			name:       "v6 any without token refused",
			args:       []string{"-addr", "[::]:6511"},
			wantErr:    true,
			errContain: []string{"[::]:6511", "token"},
		},
		// -no-auth is a loopback-only opt-in: it must NOT unlock non-loopback.
		{
			name:       "non-loopback without token refused even with -no-auth",
			args:       []string{"-addr", "0.0.0.0:6511", "-no-auth"},
			wantErr:    true,
			errContain: []string{"0.0.0.0:6511", "token"},
		},
		// 2. Loopback tokenless requires the explicit flag.
		{
			name:       "loopback without token and without flag refused",
			args:       []string{"-addr", "127.0.0.1:6511"},
			wantErr:    true,
			errContain: []string{"127.0.0.1:6511", "-no-auth"},
		},
		{
			name:       "default address without token and without flag refused",
			args:       []string{},
			wantErr:    true,
			errContain: []string{"-no-auth"},
		},
		{
			name:       "localhost without token and without flag refused",
			args:       []string{"-addr", "localhost:6511"},
			wantErr:    true,
			errContain: []string{"localhost:6511", "-no-auth"},
		},
		{
			name:       "v6 loopback without token and without flag refused",
			args:       []string{"-addr", "[::1]:6511"},
			wantErr:    true,
			errContain: []string{"[::1]:6511", "-no-auth"},
		},
		{
			name:       "127/8 beyond .1 treated as loopback, needs flag",
			args:       []string{"-addr", "127.0.0.53:6511"},
			wantErr:    true,
			errContain: []string{"127.0.0.53:6511", "-no-auth"},
		},
		// Accepted configurations.
		{
			name: "loopback tokenless with -no-auth accepted",
			args: []string{"-addr", "127.0.0.1:6511", "-no-auth"},
		},
		{
			name: "default address tokenless with -no-auth accepted",
			args: []string{"-no-auth"},
		},
		{
			name: "localhost tokenless with -no-auth accepted",
			args: []string{"-addr", "localhost:6511", "-no-auth"},
		},
		{
			name: "v6 loopback tokenless with -no-auth accepted",
			args: []string{"-addr", "[::1]:6511", "-no-auth"},
		},
		{
			name: "loopback with token accepted without flag",
			args: []string{"-addr", "127.0.0.1:6511", "-token", goodToken},
		},
		{
			name: "non-loopback with token accepted without flag",
			args: []string{"-addr", "0.0.0.0:6511", "-token", goodToken},
		},
		// 3. Token-file permission enforcement.
		{
			name:       "group-readable token file refused",
			args:       []string{"-addr", "127.0.0.1:6511", "-token", groupReadableToken},
			wantErr:    true,
			errContain: []string{"chmod 0600"},
		},
		{
			name:       "group-writable token file refused",
			args:       []string{"-addr", "127.0.0.1:6511", "-token", groupWriteToken},
			wantErr:    true,
			errContain: []string{"chmod 0600"},
		},
		{
			name:       "other-readable token file refused",
			args:       []string{"-addr", "127.0.0.1:6511", "-token", worldWriteToken},
			wantErr:    true,
			errContain: []string{"chmod 0600"},
		},
		{
			name:       "token file perms enforced even with -no-auth",
			args:       []string{"-addr", "127.0.0.1:6511", "-token", groupReadableToken, "-no-auth"},
			wantErr:    true,
			errContain: []string{"chmod 0600"},
		},
		{
			name: "owner-readable-only token file accepted",
			args: []string{"-addr", "127.0.0.1:6511", "-token", ownerReadonlyToken},
		},
		{
			name: "owner-rw token file accepted",
			args: []string{"-addr", "127.0.0.1:6511", "-token", goodToken},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected startup refusal for %v, got cfg=%+v", tc.args, cfg)
				}
				for _, needle := range tc.errContain {
					if !strings.Contains(err.Error(), needle) {
						t.Fatalf("error %q must name %q", err, needle)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %v: %v", tc.args, err)
			}
		})
	}
}

// TestValidateAuthConfigLoopbackClassification pins the loopback boundary the
// gate uses: 127.0.0.0/8, ::1, and the literal "localhost" are loopback; the
// empty host (":6511", all interfaces), 0.0.0.0, and routable addresses are
// not.
func TestValidateAuthConfigLoopbackClassification(t *testing.T) {
	sanitizeVdhostEnv(t)
	// Runs the real startup config surface (loadConfig) over the loopback
	// boundary the gate must classify: 127.0.0.0/8, ::1, and the literal
	// "localhost" are loopback; the empty host (":6511", all interfaces),
	// 0.0.0.0, and routable addresses are not.
	cases := []struct {
		addr     string
		loopback bool
	}{
		{"127.0.0.1:6511", true},
		{"127.0.0.53:53", true},
		{"[::1]:6511", true},
		{"localhost:6511", true},
		{"0.0.0.0:6511", false},
		{":6511", false},
		{"[::]:6511", false},
		{"10.0.0.5:6511", false},
		{"192.168.1.10:6511", false},
	}
	var zero [32]byte
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s(loopback=%v)", tc.addr, tc.loopback), func(t *testing.T) {
			err := loadConfigMust(tc.addr, zero, false)
			if tc.loopback {
				if err == nil || !strings.Contains(err.Error(), "-no-auth") {
					t.Fatalf("loopback %v without token must demand -no-auth, got %v", tc.addr, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.addr) || !strings.Contains(err.Error(), "token") {
				t.Fatalf("non-loopback %v without token must be refused naming the address, got %v", tc.addr, err)
			}
		})
	}
}

// loadConfigMust runs the real config surface for one addr/token combination.
func loadConfigMust(addr string, token [32]byte, noAuth bool) error {
	args := []string{"-addr", addr}
	if noAuth {
		args = append(args, "-no-auth")
	}
	_, err := loadConfig(args)
	return err
}

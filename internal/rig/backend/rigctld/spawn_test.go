package rigctld

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
)

func TestArgs(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Rigctld
		want    string
		wantErr string
	}{
		{
			name: "the usual case",
			cfg:  config.Rigctld{Address: "127.0.0.1:4532", Model: 1035, Device: "/dev/ttyUSB1"},
			want: "-m 1035 -r /dev/ttyUSB1 -T 127.0.0.1 -t 4532",
		},
		{
			// A network-attached rig, or Hamlib's dummy, has no serial port,
			// and -r with an empty argument is not the same as no -r at all.
			name: "no device",
			cfg:  config.Rigctld{Address: "127.0.0.1:4532", Model: 1},
			want: "-m 1 -T 127.0.0.1 -t 4532",
		},
		{
			name: "a host with no port takes rigctld's default",
			cfg:  config.Rigctld{Address: "localhost", Model: 3073},
			want: "-m 3073 -T localhost -t 4532",
		},
		{
			name: "an IPv6 literal",
			cfg:  config.Rigctld{Address: "[::1]:4532", Model: 3073},
			want: "-m 3073 -T ::1 -t 4532",
		},

		{name: "no model", cfg: config.Rigctld{Address: "127.0.0.1:4532"}, wantErr: "rigctld.model"},
		{name: "no address", cfg: config.Rigctld{Model: 1035}, wantErr: "rigctld.address"},
		{
			// ":4532" would leave the daemon listening on every interface,
			// which a configuration file should have to say on purpose.
			name:    "a port with no host",
			cfg:     config.Rigctld{Address: ":4532", Model: 1035},
			wantErr: "names a port but no host",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Args(tc.cfg)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Args = %q, want an error mentioning %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Args: %v", err)
			}
			if strings.Join(got, " ") != tc.want {
				t.Errorf("Args = %q, want %q", strings.Join(got, " "), tc.want)
			}
		})
	}
}

// TestArgsNeverEnablesVFOMode is a regression guard with teeth. rigctld -o adds
// a mandatory VFO argument to every command and changes the extended-response
// echo, which would break every transaction this backend makes.
func TestArgsNeverEnablesVFOMode(t *testing.T) {
	args, err := Args(config.Rigctld{Address: "127.0.0.1:4532", Model: 1035, Device: "/dev/ttyUSB0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range args {
		if a == "-o" || a == "--vfo" {
			t.Fatalf("Args enabled VFO mode: %q", args)
		}
	}
}

// TestSpawnRejectsBadConfig proves the configuration is checked before anything
// is looked up on PATH, so the error names the mistake rather than the missing
// program.
func TestSpawnRejectsBadConfig(t *testing.T) {
	_, err := Spawn(context.Background(), config.Rigctld{Address: "127.0.0.1:4532"}, slog.Default())
	if err == nil {
		t.Fatal("Spawn accepted a configuration with no model")
	}
	if !strings.Contains(err.Error(), "rigctld.model") {
		t.Errorf("error = %v, want it to name the missing field", err)
	}
}

func TestSpawnMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // an empty PATH: Hamlib is not installed
	_, err := Spawn(context.Background(), config.Rigctld{Address: "127.0.0.1:4532", Model: 1}, slog.Default())
	if err == nil {
		t.Fatal("Spawn succeeded with no rigctld on PATH")
	}
	if !strings.Contains(err.Error(), "not on PATH") {
		t.Errorf("error = %v, want it to say the program is missing", err)
	}
}

// fakeRigctld puts a script named rigctld at the head of PATH, so Spawn can be
// exercised end to end without Hamlib installed. It echoes its arguments to
// stdout, writes a line to stderr, then waits to be signalled.
func fakeRigctld(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake daemon is a shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"argv: $*\"\necho 'rigctld: opened port' >&2\nwhile true; do sleep 0.05; done\n"
	if err := os.WriteFile(filepath.Join(dir, DefaultBinary), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the fake daemon: %v", err)
	}
	t.Setenv("PATH", dir)
}

// logBuffer collects log records. Spawn drains the child's pipes from two
// goroutines, so the writer has to be safe for concurrent use.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// TestSpawn runs the whole thing: start the daemon, relay both its pipes into
// slog, and prove that cancelling the context ends it.
func TestSpawn(t *testing.T) {
	fakeRigctld(t)

	var out logBuffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := Spawn(ctx, config.Rigctld{Address: "127.0.0.1:4532", Model: 1035, Device: "/dev/ttyUSB1"}, log)
	if err != nil {
		cancel()
		t.Fatalf("Spawn: %v", err)
	}

	// Give the child long enough to write both lines. It then sleeps forever,
	// so nothing here depends on it exiting by itself.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "opened port") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	// The child ignores nothing and is killed, so Wait returns an error; what
	// matters is that it returns at all rather than blocking on an undrained
	// pipe.
	_ = cmd.Wait()

	logged := out.String()
	for _, want := range []string{
		"started rigctld",         // Spawn's own line
		"-m 1035 -r /dev/ttyUSB1", // the argv, so a misconfiguration is visible
		"argv:",                   // the child's stdout
		"opened port",             // the child's stderr
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log does not contain %q:\n%s", want, logged)
		}
	}
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("the child's stderr was not raised above stdout's level:\n%s", logged)
	}
}

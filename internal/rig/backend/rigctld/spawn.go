package rigctld

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/hessu/remoses/internal/config"
)

// This file is not part of the backend.
//
// `spawn: true` asks remoses to launch rigctld itself rather than expecting an
// operator to have started it. That is process supervision, which means
// goroutines, which the backend contract forbids a Rig to own — so it lives
// here as a plain function for main to call, and Rig never knows whether the
// daemon it is talking to was started by remoses or by systemd.

// DefaultBinary is the daemon's name. It is looked up on PATH so that the error
// for a missing Hamlib installation names the program rather than an ENOENT on
// a path nobody typed.
const DefaultBinary = "rigctld"

// DefaultPort is rigctld's own default, used when the configured address names
// a host but no port.
const DefaultPort = "4532"

// spawnWaitDelay bounds how long Wait will hold on after the process has been
// signalled, waiting for the log pipes to close.
const spawnWaitDelay = 5 * time.Second

// Args builds rigctld's argument vector from the configuration.
//
// It is separate from Spawn, and pure, because the argument vector is the part
// worth testing: everything else here is process plumbing.
//
// Notably absent is -o. VFO mode makes every command carry a mandatory VFO
// argument and changes the shape of the extended-response echo, and this
// backend is written for the protocol without it (see the package doc). Adding
// -o would silently break every transaction.
func Args(cfg config.Rigctld) ([]string, error) {
	if cfg.Model <= 0 {
		return nil, fmt.Errorf("rigctld: spawning the daemon needs rigctld.model, " +
			"the numeric Hamlib model id (`rigctl -l` lists them)")
	}

	host, port, err := splitAddress(cfg.Address)
	if err != nil {
		return nil, err
	}

	args := []string{"-m", strconv.Itoa(cfg.Model)}
	if cfg.Device != "" {
		// A model with no serial port of its own — a network-attached rig, or
		// Hamlib's dummy — takes no -r, so an empty device is passed through as
		// an absence rather than as an empty string the daemon would try to
		// open.
		args = append(args, "-r", cfg.Device)
	}
	if host != "" {
		args = append(args, "-T", host)
	}
	args = append(args, "-t", port)
	return args, nil
}

// splitAddress turns "127.0.0.1:4532" into its parts. A bare host is allowed
// and takes rigctld's default port; a bare port is not, because "-T" with no
// host would leave the daemon listening on every interface, which is not
// something a configuration file should be able to say by accident.
func splitAddress(addr string) (host, port string, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", fmt.Errorf("rigctld: spawning the daemon needs rigctld.address to know what to listen on")
	}
	host, port, err = net.SplitHostPort(addr)
	if err != nil {
		// No colon at all: the whole string is a host.
		return addr, DefaultPort, nil
	}
	if host == "" {
		return "", "", fmt.Errorf("rigctld: rigctld.address %q names a port but no host; "+
			"use 127.0.0.1:%s to keep the daemon on the loopback interface", addr, port)
	}
	if port == "" {
		port = DefaultPort
	}
	return host, port, nil
}

// Spawn starts rigctld as a child process and returns the running command.
//
// The caller owns the returned *exec.Cmd and must Wait on it. Cancelling ctx
// terminates the daemon: exec.CommandContext signals it, and WaitDelay bounds
// how long a daemon that ignores the signal — or a log line still in flight —
// can hold Wait open.
//
// stdout and stderr are drained into log. Draining is not optional: rigctld
// writes to stderr whenever a serial transaction misbehaves, and a child whose
// pipe fills up stops making progress. Routing it through slog also means the
// daemon's account of a failing radio ends up in the same place as remoses'.
func Spawn(ctx context.Context, cfg config.Rigctld, log *slog.Logger) (*exec.Cmd, error) {
	if log == nil {
		log = slog.Default()
	}

	args, err := Args(cfg)
	if err != nil {
		return nil, err
	}

	bin, err := exec.LookPath(DefaultBinary)
	if err != nil {
		return nil, fmt.Errorf("rigctld: cannot spawn the daemon: %s is not on PATH "+
			"(install Hamlib, or set spawn: false and start it yourself): %w", DefaultBinary, err)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	// Without this, Wait blocks until both pipes reach EOF, which a daemon that
	// has been signalled but not yet reaped can delay indefinitely.
	cmd.WaitDelay = spawnWaitDelay

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("rigctld: capturing daemon stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("rigctld: capturing daemon stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("rigctld: starting %s %s: %w", bin, strings.Join(args, " "), err)
	}

	child := log.With("proc", DefaultBinary, "pid", cmd.Process.Pid, "model", cfg.Model)
	go drainInto(stdout, child, slog.LevelInfo)
	go drainInto(stderr, child, slog.LevelWarn)

	child.Info("started rigctld", "args", strings.Join(args, " "))
	return cmd, nil
}

// drainInto forwards a child pipe line by line.
//
// The reader stops at the first error, which is what closing the pipe produces
// when the process exits, so the goroutine ends with the child rather than
// outliving it. Lines longer than the scanner's buffer are dropped rather than
// reported: this is a log relay, and a daemon emitting a 64 kB log line has a
// larger problem than the truncation.
func drainInto(r io.Reader, log *slog.Logger, level slog.Level) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		log.Log(context.Background(), level, line)
	}
}

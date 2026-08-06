// Command remoses serves locally-connected amateur radio transceivers over an
// authenticated HTTP and WebSocket API.
//
// This file is the composition root: it is the only place that knows how all
// the pieces fit together. Everything below it is wired through interfaces, so
// the concurrency core never imports the serial package, the API never imports
// the WebSocket hub, and backends never learn what a Transport is.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hessu/remoses/internal/api"
	"github.com/hessu/remoses/internal/auth"
	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/cw"
	"github.com/hessu/remoses/internal/lock"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig"
	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/rig/backend/rigctld"
	"github.com/hessu/remoses/internal/transport"
	"github.com/hessu/remoses/internal/transport/serial"
	"github.com/hessu/remoses/internal/transport/tcp"
	"github.com/hessu/remoses/internal/ws"

	// Backends register themselves.
	_ "github.com/hessu/remoses/internal/rig/backend/civ"
	_ "github.com/hessu/remoses/internal/rig/backend/kenwood"
	// yaesu registers the binary FT-857/FT-897 backend alongside its own.
	_ "github.com/hessu/remoses/internal/rig/backend/yaesu"
)

// version is stamped at build time by the Makefile
// (-ldflags "-X main.version=..."). It must stay a var: -X cannot write a
// constant. The default is what a plain `go build` produces.
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "passwd" {
		if err := passwd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "remoses passwd:", err)
			os.Exit(1)
		}
		return
	}

	var (
		cfgPath   = flag.String("config", "remoses.yaml", "path to the configuration file")
		checkOnly = flag.Bool("check", false, "validate the configuration and exit")
		logLevel  = flag.String("log-level", "info", "debug, info, warn or error")
		debugWire = flag.String("debug-wire", "",
			"trace raw CAT bytes for these radios: comma-separated ids, or "+
				config.WireDebugAll+" (needs -log-level=debug)")
		showVer = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVer {
		fmt.Println("remoses", version)
		return
	}

	log := newLogger(*logLevel)
	slog.SetDefault(log)

	if err := run(*cfgPath, *debugWire, *checkOnly, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `remoses %s — remote control of amateur radio transceivers

usage:
  remoses [flags]
  remoses passwd [-cost N] [username]

flags:
`, version)
	flag.PrintDefaults()
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func run(cfgPath, debugWire string, checkOnly bool, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	// Merged into the configuration before anything reads it, so the sessions
	// have one source of truth for whether a radio is being traced.
	if err := cfg.ApplyWireDebug(debugWire); err != nil {
		return fmt.Errorf("-debug-wire: %w", err)
	}
	if checkOnly {
		fmt.Printf("%s: ok — %d radio(s), %d user(s)\n", cfgPath, len(cfg.Radios), len(cfg.Auth.Users))
		return nil
	}
	announceWireDebug(cfg, log)

	authn, err := auth.New(cfg.Auth)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	// Sessions are built here rather than by rig.NewManager, because wiring a
	// CW sender needs the concrete backend (for MorseSender) and the session
	// (as its backend.Conn) at the same time, and the manager exposes neither.
	sessions, closers, err := buildSessions(cfg, log)
	if err != nil {
		return err
	}
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()

	mgr, err := rig.NewManagerWithSessions(sessions...)
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	defer mgr.Close()

	locks := lock.NewManager(cfg.Lock, lock.WithLogger(log))
	defer locks.Close()

	// A lock expiring is a safety event: a client that crashed mid-transmission
	// must not leave a carrier up. See DESIGN.md §7.
	locks.SetOnExpire(func(radioID string) {
		if s, ok := mgr.Get(radioID); ok {
			s.ForceRX("lock expired")
		}
	})

	hub := ws.NewHub(mgr, cfg.WS, ws.WithLogger(log), ws.WithVersion(version))
	defer hub.Close()

	handler := api.New(cfg, mgr, locks, authn,
		hub.Handler(), hub.TicketHandler(authn), api.WithLogger(log))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start any rigctld daemons we supervise before the sessions dial them. No
	// wait is needed: the session supervisor retries with backoff, so a daemon
	// that takes a moment to bind simply gets picked up on the next attempt.
	if err := spawnRigctld(ctx, cfg, log); err != nil {
		return err
	}

	mgr.Start(ctx)
	go hub.Run(ctx)

	return serve(ctx, cfg, handler, log)
}

// announceWireDebug says which radios will have their CAT traffic traced, and
// warns when the trace has nowhere to go.
//
// Wire lines are logged at debug level, so -debug-wire without -log-level=debug
// produces complete silence — which, to somebody debugging a rig at two in the
// morning, is indistinguishable from a radio that is not answering.
func announceWireDebug(cfg *config.Config, log *slog.Logger) {
	var on []string
	for i := range cfg.Radios {
		if cfg.Radios[i].DebugWire {
			on = append(on, cfg.Radios[i].ID)
		}
	}
	if len(on) == 0 {
		return
	}
	radios := strings.Join(on, ",")
	if !log.Enabled(context.Background(), slog.LevelDebug) {
		log.Warn("CAT wire logging is enabled but the log level hides it; pass -log-level=debug",
			"radios", radios)
		return
	}
	log.Info("CAT wire logging enabled; expect a few frames per second per radio from polling alone",
		"radios", radios)
}

func spawnRigctld(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	for i := range cfg.Radios {
		rc := cfg.Radios[i]
		if rc.Backend != config.BackendRigctld || rc.Rigctld == nil || !rc.Rigctld.Spawn {
			continue
		}
		cmd, err := rigctld.Spawn(ctx, *rc.Rigctld, log)
		if err != nil {
			return fmt.Errorf("radio %q: spawn rigctld: %w", rc.ID, err)
		}
		log.Info("spawned rigctld", "radio", rc.ID, "pid", cmd.Process.Pid,
			"address", rc.Rigctld.Address, "model", rc.Rigctld.Model)
	}
	return nil
}

// buildSessions constructs one session per configured radio, plus its CW sender.
// The returned closers own resources the sessions do not, such as a dedicated
// keying port.
func buildSessions(cfg *config.Config, log *slog.Logger) ([]*rig.Session, []interface{ Close() error }, error) {
	var (
		sessions []*rig.Session
		closers  []interface{ Close() error }
	)
	for i := range cfg.Radios {
		rc := cfg.Radios[i]

		b, err := backend.New(&rc)
		if err != nil {
			return nil, closers, fmt.Errorf("radio %q: %w", rc.ID, err)
		}
		dialer, err := dialerFor(rc)
		if err != nil {
			return nil, closers, fmt.Errorf("radio %q: %w", rc.ID, err)
		}
		s, err := rig.NewSession(rc, b, dialer, rig.WithLogger(log))
		if err != nil {
			return nil, closers, fmt.Errorf("radio %q: %w", rc.ID, err)
		}

		if rc.CW.Enabled {
			c, err := attachCW(s, b, rc, log)
			if err != nil {
				return nil, closers, fmt.Errorf("radio %q cw: %w", rc.ID, err)
			}
			if c != nil {
				closers = append(closers, c)
			}
		}
		sessions = append(sessions, s)
	}
	return sessions, closers, nil
}

// dialerFor picks the transport a radio needs.
//
// Three cases, all producing the same transport.Dialer so the session's
// supervisor, backoff and reconnect logic are identical for each:
//   - rigctld, which talks to a separate Hamlib daemon over a socket;
//   - a serial port published over the network by a terminal server such as
//     ser2net, which is just bytes on a socket too;
//   - a local serial device.
func dialerFor(rc config.Radio) (transport.Dialer, error) {
	if rc.Backend == config.BackendRigctld {
		addr := ""
		if rc.Rigctld != nil {
			addr = rc.Rigctld.Address
		}
		return tcp.NewDialer(addr)
	}
	if rc.Port.Networked() {
		return tcp.NewDialer(rc.Port.TCP)
	}
	return serial.NewDialer(rc.Port)
}

// attachCW gives a session its Morse sender. It returns a closer for any extra
// resource the sender owns.
func attachCW(s *rig.Session, b backend.Rig, rc config.Radio, log *slog.Logger) (interface{ Close() error }, error) {
	switch rc.CW.Method {
	case string(radio.CWViaSerial):
		port, err := openKeyingPort(rc)
		if err != nil {
			return nil, err
		}
		lines, ok := port.(transport.ControlLines)
		if !ok {
			_ = port.Close()
			return nil, fmt.Errorf("keying port %s has no modem control lines", rc.CW.SerialKey.Device)
		}
		snd, err := cw.NewSerialKey(lines, rc.CW)
		if err != nil {
			_ = port.Close()
			return nil, err
		}
		s.SetCWSender(snd)
		log.Info("cw: local keyer", "radio", rc.ID, "device", rc.CW.SerialKey.Device,
			"key_line", rc.CW.SerialKey.KeyLine)
		return closeBoth(snd, port), nil

	default: // "cat"
		// Ask the capability, not the Go type. A backend type may implement
		// MorseSender for the family while a particular radio in that family
		// lacks the command — the IC-718 has no CI-V CW buffer at all — so the
		// type assertion would succeed and every message would draw a rejection
		// that looks, to the operator, like it was sent.
		if m := b.Caps().CWMethod; m != radio.CWViaCAT {
			return nil, fmt.Errorf("this radio has no CAT CW buffer (backend %q reports cw_method %q); "+
				"use cw.method: serial_key to key a control line instead", rc.Backend, m)
		}
		ms, ok := b.(backend.MorseSender)
		if !ok {
			return nil, fmt.Errorf("backend %q does not implement CAT CW sending", rc.Backend)
		}
		// The session is the backend.Conn, so the sender survives reconnects
		// without rewiring.
		snd, err := cw.NewCAT(ms, s, rc.CW)
		if err != nil {
			return nil, err
		}
		s.SetCWSender(snd)
		log.Info("cw: rig buffer", "radio", rc.ID, "max_chunk", ms.MaxChunk())
		if c, ok := snd.(interface{ Close() error }); ok {
			return c, nil
		}
		return nil, nil
	}
}

// openKeyingPort opens the dedicated port used for DTR/RTS keying.
//
// v1 requires it to be a different device from the CAT port. Sharing one port
// is described in DESIGN.md §11.2 and does work at the OS level, but the session
// owns its transport privately and redials it on disconnect, so handing the same
// handle to the keyer would mean the keyer holding a port the session may close
// underneath it. Rejecting the configuration is honest; silently keying a closed
// port would not be.
func openKeyingPort(rc config.Radio) (transport.Transport, error) {
	sk := rc.CW.SerialKey
	if sk == nil {
		return nil, errors.New("cw.method is serial_key but no serial_key block is configured")
	}
	if sk.Device != "" && sk.Device == rc.Port.Device {
		return nil, fmt.Errorf("keying device %s is also the CAT port; v1 requires a separate device", sk.Device)
	}
	d, err := serial.NewDialer(config.Port{Device: sk.Device, Baud: 9600})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.Dial(ctx)
}

func closeBoth(a any, b transport.Transport) interface{ Close() error } {
	return closerFunc(func() error {
		if c, ok := a.(interface{ Close() error }); ok {
			_ = c.Close()
		}
		return b.Close()
	})
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func serve(ctx context.Context, cfg *config.Config, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would cut off WebSocket streams, which are
		// expected to stay open indefinitely.
		IdleTimeout: 120 * time.Second,
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errCh := make(chan error, 1)
	go func() {
		var err error
		if cfg.Server.TLS != nil {
			log.Info("listening", "addr", cfg.Server.Listen, "tls", true,
				"base_path", cfg.Server.BasePath)
			err = srv.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		} else {
			log.Warn("listening without TLS; basic auth credentials are sent in clear",
				"addr", cfg.Server.Listen, "base_path", cfg.Server.BasePath)
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			return err
		}
		return <-errCh
	}
}

// passwd generates a bcrypt hash for the configuration file. Passwords are read
// from the terminal rather than taken as an argument, so they do not end up in
// shell history or the process table.
func passwd(args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	cost := fs.Int("cost", config.DefaultBcryptCost, "bcrypt cost")
	if err := fs.Parse(args); err != nil {
		return err
	}

	pw, err := readPassword("Password: ")
	if err != nil {
		return err
	}
	again, err := readPassword("Again:    ")
	if err != nil {
		return err
	}
	if pw != again {
		return errors.New("passwords do not match")
	}
	if strings.TrimSpace(pw) == "" {
		return errors.New("empty password")
	}

	hash, err := auth.HashPassword(pw, *cost)
	if err != nil {
		return err
	}
	if name := fs.Arg(0); name != "" {
		fmt.Printf("    - username: %s\n      password_bcrypt: %q\n", name, hash)
	} else {
		fmt.Println(hash)
	}
	return nil
}

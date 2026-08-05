// Command remoses-cli is a read-only terminal monitor for a remoses instance.
//
// It subscribes to one radio and shows its state as it changes: frequency,
// mode, filter, power, PTT, S meter and the CW queue. It displays and nothing
// else. There is no code path here that issues a PATCH, a POST or a DELETE, and
// none that acquires a radio lock — a monitor that took the lock would lock out
// the operator actually working the radio, which is the opposite of what a
// monitor is for.
//
// Output adapts to where it is going. On a terminal the display is redrawn in
// place; in a pipe it becomes timestamped lines, because escape sequences in a
// log file help nobody.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/hessu/remoses/internal/client"
	"github.com/hessu/remoses/internal/config"
)

// version is stamped at build time by the Makefile
// (-ldflags "-X main.version=..."). It must stay a var: -X cannot write a
// constant.
var version = "dev"

// defaultConfigPath is the daemon's own default, so that running this next to a
// running remoses needs no arguments beyond the radio id.
const defaultConfigPath = "remoses.yaml"

// defaultWidth is used when stdout is a terminal that will not report its size.
const defaultWidth = 80

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "remoses-cli:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("remoses-cli", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var (
		cfgPath  = flags.String("config", defaultConfigPath, "path to the remoses configuration file, for the server address")
		rawURL   = flags.String("url", "", "API base URL, overriding the configuration file (env "+envURL+")")
		user     = flags.String("user", "", "username (env "+envUser+"; prompted if unset)")
		pwFile   = flags.String("password-file", "", "read the password from this file, or from stdin if \"-\" (env "+envPasswordFile+")")
		once     = flags.Bool("once", false, "print the current state once and exit")
		plain    = flags.Bool("plain", false, "force line output even on a terminal")
		interval = flags.Duration("interval", time.Second, "in line output, the minimum interval between lines whose only change is a meter")
		timeout  = flags.Duration("timeout", client.DefaultTimeout, "timeout for each REST request")
		insecure = flags.Bool("insecure", false, "do not verify the server's TLS certificate")
		showVer  = flags.Bool("version", false, "print the version and exit")
	)
	flags.Usage = func() { usage(stderr, flags) }

	if err := flags.Parse(args); err != nil {
		return err
	}
	if *showVer {
		fmt.Fprintln(stdout, "remoses-cli", version)
		return nil
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return errors.New("exactly one radio id is required")
	}
	radioID := flags.Arg(0)

	base, err := resolveBase(*rawURL, *cfgPath, explicitlySet(flags, "config"))
	if err != nil {
		return err
	}

	username, password, err := credentials(*user, *pwFile,
		client.UserFromURL(firstNonEmpty(*rawURL, os.Getenv(envURL))))
	if err != nil {
		return err
	}

	opts := []client.Option{client.WithTimeout(*timeout)}
	if *insecure {
		fmt.Fprintln(stderr, "remoses-cli: warning: TLS certificate verification is disabled")
		opts = append(opts, client.WithInsecureTLS())
	}
	cl, err := client.New(base, username, password, opts...)
	if err != nil {
		return err
	}

	out, closeOut := newRenderer(stdout, *plain, *interval)
	// Restoring the terminal is a defer rather than part of the exit path,
	// because a Ctrl-C that left the cursor hidden would follow the operator
	// into the rest of their shell session.
	defer closeOut()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := newMonitor(cl, radioID, out, newView(radioID, time.Now))
	m.once = *once
	if err := m.run(ctx); err != nil {
		return describe(err, base, username, radioID)
	}
	return nil
}

// newRenderer picks the output style. A terminal gets a redrawn block; anything
// else — a pipe, a file, a CI log — gets lines.
func newRenderer(stdout io.Writer, forcePlain bool, meterInterval time.Duration) (renderer, func()) {
	width, isTTY := terminalWidth(stdout)
	if forcePlain || !isTTY {
		r := newPlainRenderer(stdout, time.Now, meterInterval)
		return r, r.close
	}
	r := newTTYRenderer(stdout, width)
	return r, r.close
}

func terminalWidth(w io.Writer) (int, bool) {
	f, ok := w.(*os.File)
	if !ok {
		return 0, false
	}
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return 0, false
	}
	width, _, err := term.GetSize(fd)
	if err != nil || width <= 0 {
		width = defaultWidth
	}
	return width, true
}

// resolveBase decides which instance to talk to.
//
// -url wins outright, because it is the remote case and nothing local should
// override it. Otherwise the daemon's own configuration file supplies the
// address, which is what makes the local case need no arguments. A configuration
// file that is simply not there is only an error when the operator named it: the
// default path missing means "not run from the daemon's directory", and falling
// back to the built-in defaults is more useful than refusing.
func resolveBase(flagURL, cfgPath string, explicitConfig bool) (string, error) {
	if u := firstNonEmpty(flagURL, os.Getenv(envURL)); u != "" {
		return client.ResolveURL(u)
	}
	if p := os.Getenv(envConfig); p != "" && !explicitConfig {
		cfgPath, explicitConfig = p, true
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		if !explicitConfig && errors.Is(err, iofs.ErrNotExist) {
			return client.BaseURL(&config.Config{})
		}
		return "", err
	}
	return client.BaseURL(cfg)
}

func explicitlySet(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// describe turns the failures an operator is most likely to hit into sentences
// that say what to do about them, rather than into a status code.
func describe(err error, base, username, radioID string) error {
	switch {
	case client.IsUnauthorized(err):
		return fmt.Errorf("authentication failed for user %q at %s: "+
			"check the username and password (the configuration file holds bcrypt "+
			"hashes, so the password cannot be read out of it)", username, base)
	case client.IsNotFound(err):
		return fmt.Errorf("%s has no radio %q", base, radioID)
	}
	return err
}

func usage(w io.Writer, flags *flag.FlagSet) {
	fmt.Fprintf(w, `remoses-cli %s - read-only monitor for one remoses radio

usage:
  remoses-cli [flags] <radio-id>

The server address comes from the remoses configuration file (%s by
default), or from -url for a remote instance. Credentials come from -user and
one of %s, -password-file or a prompt; there is no password flag,
because a password on the command line lands in shell history and in the
process table.

flags:
`, version, defaultConfigPath, envPassword)
	flags.PrintDefaults()
}

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Environment variables. They are the scriptable path: a password on the
// command line is visible in the process table to every user on the machine and
// is written into shell history, and no amount of care at the call site fixes
// that, so this program has no -password flag at all.
const (
	envURL          = "REMOSES_URL"
	envConfig       = "REMOSES_CONFIG"
	envUser         = "REMOSES_USER"
	envPassword     = "REMOSES_PASSWORD"
	envPasswordFile = "REMOSES_PASSWORD_FILE"
)

// stdin is shared across prompts for the same reason cmd/remoses shares its
// one: a fresh bufio.Reader per call would buffer everything available and then
// throw it away, so a piped "user\npassword\n" would satisfy the first read and
// hit EOF on the second.
var stdin = bufio.NewReader(os.Stdin)

// credentials resolves the username and password.
//
// The order is deliberate — an explicit flag beats the environment, and the
// prompt is the fallback rather than the first thing tried — so that the same
// invocation works interactively and under cron.
func credentials(flagUser, passwordFile, urlUser string) (user, pass string, err error) {
	user = firstNonEmpty(flagUser, os.Getenv(envUser), urlUser)
	if user == "" {
		user, err = promptLine("Username: ")
		if err != nil {
			return "", "", fmt.Errorf("reading username: %w", err)
		}
		user = strings.TrimSpace(user)
	}
	if user == "" {
		return "", "", errors.New("no username: pass -user, set " + envUser + ", or answer the prompt")
	}

	if passwordFile == "" {
		passwordFile = os.Getenv(envPasswordFile)
	}
	switch {
	case passwordFile != "":
		pass, err = readPasswordFile(passwordFile)
		if err != nil {
			return "", "", err
		}
	case os.Getenv(envPassword) != "":
		pass = os.Getenv(envPassword)
	default:
		pass, err = readPassword("Password: ")
		if err != nil {
			return "", "", fmt.Errorf("reading password: %w", err)
		}
	}
	return user, pass, nil
}

// readPasswordFile reads a password from a file, or from standard input when
// the path is "-". A file lets a scheduled job hold the secret in something
// with permissions on it, which the environment of a process does not have.
func readPasswordFile(path string) (string, error) {
	var (
		b   []byte
		err error
	)
	if path == "-" {
		b, err = io.ReadAll(stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("reading password file: %w", err)
	}
	// Only the first line, so a file written by an editor that appends a
	// newline — or that holds a comment underneath — still works.
	pw, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimRight(pw, "\r"), nil
}

// readPassword prompts on the terminal with echo disabled.
//
// When stdin is not a terminal it reads one line instead, so the command can be
// scripted. This is the same shape as cmd/remoses/password.go, and for the same
// reasons; it is duplicated rather than shared because both live in package
// main.
func readPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return readLine()
	}

	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// promptLine reads an echoed line. The username is not a secret.
func promptLine(prompt string) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, prompt)
	}
	return readLine()
}

func readLine() (string, error) {
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// stdin is shared across calls on purpose. A fresh bufio.Reader per call would
// buffer everything available and then throw it away, so a piped
// "password\npassword\n" would satisfy the first read and hit EOF on the second.
var stdin = bufio.NewReader(os.Stdin)

// readPassword prompts on the terminal with echo disabled.
//
// When stdin is not a terminal it reads one line instead, so the command can be
// scripted (`printf 'pw\npw\n' | remoses passwd`) — but it never accepts the
// password as an argument, which would put it in shell history and the process
// table.
func readPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := stdin.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

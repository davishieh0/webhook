// Command webhook catches webhook calls, answers them from mocks.json and
// shows each endpoint on its own screen.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

//
// CONSTANTS
//

const (
	defaultPort      = "8889"
	defaultMocksFile = "mocks.json"
	defaultLogFile   = "webhook.jsonl"
)

//
// CODE
//

// env returns an environment variable, or fallback when it is unset or empty.
func env(name, fallback string) string {
	value := os.Getenv(name)

	// unset or empty: take the fallback
	if value == "" {
		return fallback
	}

	return value
}

// parseStatusCodes reads a list like "[200,500]" into status codes.
//
// The codes are drawn at random for calls whose mock sets no status, which is
// how a flaky endpoint is simulated. An empty or unparsable value means 200.
func parseStatusCodes(raw string) []int {
	codes := []int{}

	for _, field := range strings.Split(strings.Trim(raw, "[] "), ",") {
		code, err := strconv.Atoi(strings.TrimSpace(field))

		// not a number: ignore the field
		if err != nil {
			continue
		}

		codes = append(codes, code)
	}

	// nothing usable: answer 200
	if len(codes) == 0 {
		return []int{200}
	}

	return codes
}

func main() {
	server := NewServer(
		env("MOCKS_FILE", defaultMocksFile),
		os.Getenv("SECRET"),
		env("LOG_FILE", defaultLogFile),
		parseStatusCodes(os.Getenv("STATUS")))

	address := ":" + env("PORT", defaultPort)
	listener, err := net.Listen("tcp", address)

	// port unavailable: fail before the TUI takes over the terminal
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", address, err)
		os.Exit(1)
	}

	program := tea.NewProgram(newModel(server), tea.WithAltScreen())
	server.Sink = func(request Request) { program.Send(request) }

	// the listener is closed when the process exits with the TUI
	go http.Serve(listener, server)

	if _, err := program.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		os.Exit(1)
	}
}

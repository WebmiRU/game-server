package tcpserver

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestAuthenticationLogsUsernameAndOutcomeWithoutPassword(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var logOutput bytes.Buffer
		server := New(Config{Logger: log.New(&logOutput, "", 0)})
		conn, scanner, _ := startPipeConnectionWithServer(t, server)

		sendRequest(t, conn, testRequest{
			Type:     "auth",
			Username: "player1",
			Password: "player1",
		})
		response := readWireResponse(t, scanner)
		assertSuccess(t, response, true)

		output := logOutput.String()
		if !strings.Contains(output, "TCP authentication succeeded") {
			t.Fatalf("success event missing from log: %q", output)
		}
		if !strings.Contains(output, `username="player1"`) {
			t.Fatalf("username missing from log: %q", output)
		}
		if strings.Contains(output, "player1") == false {
			t.Fatalf("expected username in log: %q", output)
		}
		if strings.Contains(output, "password=") {
			t.Fatalf("password field must never be logged: %q", output)
		}
	})

	t.Run("failure", func(t *testing.T) {
		const secretPassword = "do-not-write-this-password-to-log"
		var logOutput bytes.Buffer
		server := New(Config{Logger: log.New(&logOutput, "", 0)})
		conn, scanner, _ := startPipeConnectionWithServer(t, server)

		sendRequest(t, conn, testRequest{
			Type:     "auth",
			Username: "player1",
			Password: secretPassword,
		})
		response := readWireResponse(t, scanner)
		assertSuccess(t, response, false)

		output := logOutput.String()
		if !strings.Contains(output, "TCP authentication failed") {
			t.Fatalf("failure event missing from log: %q", output)
		}
		if !strings.Contains(output, `username="player1"`) {
			t.Fatalf("attempted username missing from log: %q", output)
		}
		if strings.Contains(output, secretPassword) {
			t.Fatalf("password leaked into log: %q", output)
		}
	})
}

package tcpserver

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net"
	"testing"
)

type wireResponse struct {
	Type            string `json:"type"`
	Success         *bool  `json:"success"`
	Message         string `json:"message"`
	ProtocolVersion int    `json:"protocolVersion"`
	PlayerID        string `json:"playerId"`
}

type testRequest struct {
	Type     string `json:"type"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func startPipeConnection(t *testing.T) (net.Conn, *bufio.Scanner, chan struct{}) {
	t.Helper()
	s := New(Config{
		Logger: log.New(io.Discard, "", 0),
	})
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleConnection(serverConn)
	}()

	scanner := bufio.NewScanner(clientConn)
	scanner.Buffer(make([]byte, 256), defaultMaxLine)
	t.Cleanup(func() {
		_ = clientConn.Close()
		<-done
	})
	return clientConn, scanner, done
}

func sendRequest(t *testing.T, conn net.Conn, request testRequest) {
	t.Helper()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatalf("send request: %v", err)
	}
}

func readWireResponse(t *testing.T, scanner *bufio.Scanner) wireResponse {
	t.Helper()
	if !scanner.Scan() {
		t.Fatalf("read response: %v", scanner.Err())
	}
	var value wireResponse
	if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
		t.Fatalf("decode response %q: %v", scanner.Text(), err)
	}
	return value
}

func assertSuccess(t *testing.T, response wireResponse, want bool) {
	t.Helper()
	if response.Success == nil || *response.Success != want {
		t.Fatalf("response success = %v, want %v (response: %+v)", response.Success, want, response)
	}
}

func TestAllDevelopmentPlayersCanAuthenticateAndPing(t *testing.T) {
	players := []string{"player1", "player2", "player3"}

	for _, player := range players {
		t.Run(player, func(t *testing.T) {
			conn, scanner, _ := startPipeConnection(t)
			sendRequest(t, conn, testRequest{
				Type:     "auth",
				Username: player,
				Password: player,
			})

			authResponse := readWireResponse(t, scanner)
			if authResponse.Type != "auth_result" {
				t.Fatalf("auth response type = %q, want auth_result", authResponse.Type)
			}
			assertSuccess(t, authResponse, true)
			if authResponse.PlayerID != player {
				t.Fatalf("playerId = %q, want %q", authResponse.PlayerID, player)
			}
			if authResponse.ProtocolVersion != ProtocolVersion {
				t.Fatalf("protocolVersion = %d, want %d", authResponse.ProtocolVersion, ProtocolVersion)
			}

			sendRequest(t, conn, testRequest{Type: "ping"})
			pingResponse := readWireResponse(t, scanner)
			if pingResponse.Type != "pong" {
				t.Fatalf("ping response type = %q, want pong", pingResponse.Type)
			}
			assertSuccess(t, pingResponse, true)
		})
	}
}

func TestAuthenticationRejectsWrongCredentials(t *testing.T) {
	conn, scanner, _ := startPipeConnection(t)
	sendRequest(t, conn, testRequest{
		Type:     "auth",
		Username: "player1",
		Password: "wrong",
	})

	response := readWireResponse(t, scanner)
	if response.Type != "auth_result" {
		t.Fatalf("response type = %q, want auth_result", response.Type)
	}
	assertSuccess(t, response, false)
	if response.Message != "invalid_credentials" {
		t.Fatalf("message = %q, want invalid_credentials", response.Message)
	}
}

func TestAuthenticationIsRequiredBeforePing(t *testing.T) {
	conn, scanner, _ := startPipeConnection(t)
	sendRequest(t, conn, testRequest{Type: "ping"})

	response := readWireResponse(t, scanner)
	if response.Type != "error" || response.Message != "authentication_required" {
		t.Fatalf("unexpected unauthenticated response: %+v", response)
	}
	assertSuccess(t, response, false)
}

package tcpserver

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"game-server/internal/worldmap"
)

type wireResponse struct {
	Type            string             `json:"type"`
	Success         *bool              `json:"success"`
	Message         string             `json:"message"`
	ProtocolVersion int                `json:"protocolVersion"`
	PlayerID        string             `json:"playerId"`
	World           *worldmap.Summary  `json:"world"`
	Height          *float64           `json:"height"`
	X               *float64           `json:"x"`
	Y               *float64           `json:"y"`
	Z               *float64           `json:"z"`
}

type testRequest struct {
	Type      string   `json:"type"`
	Username  string   `json:"username,omitempty"`
	Password  string   `json:"password,omitempty"`
	X         *float64 `json:"x,omitempty"`
	Y         *float64 `json:"y,omitempty"`
	Z         *float64 `json:"z,omitempty"`
	RotationX *float64 `json:"rotationX,omitempty"`
	RotationY *float64 `json:"rotationY,omitempty"`
	RotationZ *float64 `json:"rotationZ,omitempty"`
	RotationW *float64 `json:"rotationW,omitempty"`
}

func startPipeConnection(t *testing.T) (net.Conn, *bufio.Scanner, chan struct{}) {
	t.Helper()
	return startPipeConnectionWithServer(t, New(Config{
		Logger: log.New(io.Discard, "", 0),
	}))
}

func startPipeConnectionWithServer(t *testing.T, s *Server) (net.Conn, *bufio.Scanner, chan struct{}) {
	t.Helper()
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

func float64PointerForTest(value float64) *float64 {
	return &value
}

func authenticateTestPlayer(t *testing.T, conn net.Conn, scanner *bufio.Scanner) {
	t.Helper()
	sendRequest(t, conn, testRequest{Type: "auth", Username: "player1", Password: "player1"})
	authResponse := readWireResponse(t, scanner)
	if authResponse.Type != "auth_result" {
		t.Fatalf("auth response type = %q, want auth_result", authResponse.Type)
	}
	assertSuccess(t, authResponse, true)
	spawnResponse := readWireResponse(t, scanner)
	if spawnResponse.Type != "spawn" {
		t.Fatalf("spawn response type = %q, want spawn", spawnResponse.Type)
	}
	assertSuccess(t, spawnResponse, true)
}

func assertSpawnResponse(t *testing.T, response wireResponse) {
	t.Helper()
	if response.Type != "spawn" {
		t.Fatalf("spawn response type = %q, want spawn", response.Type)
	}
	assertSuccess(t, response, true)
	if response.X == nil || response.Y == nil || response.Z == nil {
		t.Fatalf("spawn coordinates missing: %+v", response)
	}
	if *response.X != 539.56 || *response.Y != 74.17 || *response.Z != 555.99 {
		t.Fatalf("spawn coordinates = (%.2f, %.2f, %.2f), want (539.56, 74.17, 555.99)",
			*response.X, *response.Y, *response.Z)
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

			assertSpawnResponse(t, readWireResponse(t, scanner))
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

func TestAuthenticatedWorldInfoAndHeightQuery(t *testing.T) {
	world := createTestWorldMap(t)
	server := New(Config{
		World:  world,
		Logger: log.New(io.Discard, "", 0),
	})
	conn, scanner, _ := startPipeConnectionWithServer(t, server)

	sendRequest(t, conn, testRequest{
		Type:     "auth",
		Username: "player1",
		Password: "player1",
	})
	authResponse := readWireResponse(t, scanner)
	assertSuccess(t, authResponse, true)
	assertSpawnResponse(t, readWireResponse(t, scanner))

	sendRequest(t, conn, testRequest{Type: "world_info"})
	worldResponse := readWireResponse(t, scanner)
	if worldResponse.Type != "world_info" || worldResponse.World == nil {
		t.Fatalf("unexpected world_info response: %+v", worldResponse)
	}
	assertSuccess(t, worldResponse, true)
	if worldResponse.World.WidthSamples != 2 || worldResponse.World.TerrainName != "TestTerrain" {
		t.Fatalf("unexpected world summary: %+v", worldResponse.World)
	}

	x, z := 0.5, 0.5
	sendRequest(t, conn, testRequest{Type: "height_at", X: &x, Z: &z})
	heightResponse := readWireResponse(t, scanner)
	if heightResponse.Type != "height_result" || heightResponse.Height == nil {
		t.Fatalf("unexpected height_at response: %+v", heightResponse)
	}
	assertSuccess(t, heightResponse, true)
	want := ((0.0 + 65535.0 + 32768.0 + 16384.0) / 4.0 / 65535.0) * 100.0
	if diff := *heightResponse.Height - want; diff < -1e-6 || diff > 1e-6 {
		t.Fatalf("height = %.9f, want %.9f", *heightResponse.Height, want)
	}
}

func TestDuplicatePlayerSessionIsRejected(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})
	firstConn, firstScanner, _ := startPipeConnectionWithServer(t, server)
	authenticateTestPlayer(t, firstConn, firstScanner)

	secondConn, secondScanner, _ := startPipeConnectionWithServer(t, server)
	sendRequest(t, secondConn, testRequest{
		Type:     "auth",
		Username: "player1",
		Password: "player1",
	})
	response := readWireResponse(t, secondScanner)
	if response.Type != "auth_result" || response.Message != "already_logged_in" {
		t.Fatalf("unexpected duplicate login response: %+v", response)
	}
	assertSuccess(t, response, false)
}

func TestPlayerStateStoresPositionAndFullRotation(t *testing.T) {
	world := createTestWorldMap(t)
	server := New(Config{
		World:  world,
		Logger: log.New(io.Discard, "", 0),
	})
	conn, scanner, _ := startPipeConnectionWithServer(t, server)
	authenticateTestPlayer(t, conn, scanner)

	sendRequest(t, conn, testRequest{
		Type:      "player_state",
		X:         float64PointerForTest(0.5),
		Y:         float64PointerForTest(50),
		Z:         float64PointerForTest(0.5),
		RotationX: float64PointerForTest(0),
		RotationY: float64PointerForTest(2),
		RotationZ: float64PointerForTest(0),
		RotationW: float64PointerForTest(2),
	})

	response := readWireResponse(t, scanner)
	if response.Type != "player_state_result" || response.PlayerID != "player1" {
		t.Fatalf("unexpected player state acknowledgement: %+v", response)
	}
	assertSuccess(t, response, true)
	if response.Message != "state_registered" {
		t.Fatalf("message = %q, want state_registered", response.Message)
	}

	states := server.PlayerStates()
	if len(states) != 1 {
		t.Fatalf("PlayerStates() has %d entries, want 1", len(states))
	}
	state := states[0]
	if state.PlayerID != "player1" {
		t.Fatalf("stored playerId = %q, want player1", state.PlayerID)
	}
	if state.Position.X != 0.5 || state.Position.Y != 50 || state.Position.Z != 0.5 {
		t.Fatalf("stored position = %+v, want (0.5, 50, 0.5)", state.Position)
	}
	wantComponent := 1 / math.Sqrt(2)
	if math.Abs(state.Rotation.Y-wantComponent) > 1e-9 ||
		math.Abs(state.Rotation.W-wantComponent) > 1e-9 ||
		state.Rotation.X != 0 || state.Rotation.Z != 0 {
		t.Fatalf("stored normalized rotation = %+v, want normalized (0, 2, 0, 2)", state.Rotation)
	}
	if state.UpdatedAt.IsZero() {
		t.Fatal("stored player state must include the server-side update timestamp")
	}

	// Subsequent state updates are accepted without an acknowledgement for each tick.
	sendRequest(t, conn, testRequest{
		Type:      "player_state",
		X:         float64PointerForTest(0.75),
		Y:         float64PointerForTest(75),
		Z:         float64PointerForTest(0.25),
		RotationX: float64PointerForTest(0),
		RotationY: float64PointerForTest(0),
		RotationZ: float64PointerForTest(1),
		RotationW: float64PointerForTest(0),
	})
	sendRequest(t, conn, testRequest{Type: "ping"})
	pingResponse := readWireResponse(t, scanner)
	if pingResponse.Type != "pong" {
		t.Fatalf("response after second state update = %q, want pong (state updates should not be acknowledged every tick)", pingResponse.Type)
	}

	states = server.PlayerStates()
	if len(states) != 1 {
		t.Fatalf("PlayerStates() has %d entries after update, want 1", len(states))
	}
	state = states[0]
	if state.Position.X != 0.75 || state.Position.Y != 75 || state.Position.Z != 0.25 {
		t.Fatalf("updated position = %+v, want (0.75, 75, 0.25)", state.Position)
	}
	if state.Rotation.X != 0 || state.Rotation.Y != 0 || state.Rotation.Z != 1 || state.Rotation.W != 0 {
		t.Fatalf("updated rotation = %+v, want (0, 0, 1, 0)", state.Rotation)
	}
}

func TestPlayerStateRejectsCoordinatesOutsideWorld(t *testing.T) {
	server := New(Config{
		World:  createTestWorldMap(t),
		Logger: log.New(io.Discard, "", 0),
	})
	conn, scanner, _ := startPipeConnectionWithServer(t, server)
	authenticateTestPlayer(t, conn, scanner)

	sendRequest(t, conn, testRequest{
		Type:      "player_state",
		X:         float64PointerForTest(2),
		Y:         float64PointerForTest(50),
		Z:         float64PointerForTest(0.5),
		RotationX: float64PointerForTest(0),
		RotationY: float64PointerForTest(0),
		RotationZ: float64PointerForTest(0),
		RotationW: float64PointerForTest(1),
	})
	response := readWireResponse(t, scanner)
	if response.Type != "player_state_result" || response.Message != "coordinates_outside_world" {
		t.Fatalf("unexpected invalid transform response: %+v", response)
	}
	assertSuccess(t, response, false)
	if states := server.PlayerStates(); len(states) != 0 {
		t.Fatalf("invalid transform was stored: %+v", states)
	}
}

func TestListenAndServeRemainsRunningAfterWorldLoad(t *testing.T) {
	portReservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local TCP port: %v", err)
	}
	address := portReservation.Addr().String()
	if err := portReservation.Close(); err != nil {
		t.Fatalf("release local TCP port: %v", err)
	}

	server := New(Config{
		Address: address,
		World:   createTestWorldMap(t),
		Logger:  log.New(io.Discard, "", 0),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- server.ListenAndServe(ctx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	connected := false
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("server exited after startup instead of continuing to listen: %v", err)
		default:
		}

		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			connected = true
			_ = conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !connected {
		t.Fatal("server did not start listening within 3 seconds after loading the world")
	}

	select {
	case err := <-done:
		t.Fatalf("server stopped while running: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server returned an error after cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop after context cancellation")
	}
}

func createTestWorldMap(t *testing.T) *worldmap.Map {
	t.Helper()
	dir := t.TempDir()

	samples := []uint16{0, 65535, 32768, 16384}
	payload := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(payload[i*2:i*2+2], sample)
	}
	sum := sha256.Sum256(payload)
	manifest := worldmap.Manifest{
		Format:             worldmap.FormatName,
		FormatVersion:      worldmap.FormatVersion,
		SceneName:          "TestScene",
		TerrainName:        "TestTerrain",
		Origin:             worldmap.Vec3{X: 0, Y: 0, Z: 0},
		Size:               worldmap.Vec3{X: 1, Y: 100, Z: 1},
		WidthSamples:       2,
		HeightSamples:      2,
		SampleSpacing:      worldmap.Vec2{X: 1, Z: 1},
		Encoding:           "uint16",
		ByteOrder:          "little-endian",
		RowOrder:           "z-ascending; x-ascending within each row",
		FirstSample:        "(x=0, z=0)",
		HeightFormula:      "worldY = origin.y + (sample / 65535.0) * size.y",
		DataFile:           "terrain.u16",
		DataLengthBytes:    int64(len(payload)),
		SHA256:             hex.EncodeToString(sum[:]),
		MinimumSample:      0,
		MaximumSample:      65535,
		MinimumHeightWorld: 0,
		MaximumHeightWorld: 100,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "terrain.u16"), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := worldmap.LoadDir(dir)
	if err != nil {
		t.Fatalf("load test world: %v", err)
	}
	return loaded
}

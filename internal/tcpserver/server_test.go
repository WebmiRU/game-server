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
	RequestID       int                `json:"requestId"`
	RetryAfter      float64            `json:"retryAfter"`
	World           *worldmap.Summary  `json:"world"`
	Height          *float64           `json:"height"`
	X               *float64           `json:"x"`
	Y               *float64           `json:"y"`
	Z               *float64           `json:"z"`
	RotationX       *float64           `json:"rotationX"`
	RotationY       *float64           `json:"rotationY"`
	RotationZ       *float64           `json:"rotationZ"`
	RotationW       *float64           `json:"rotationW"`
	PositionIsRoot  bool               `json:"positionIsRoot"`
	VisibilityRange *float64           `json:"visibilityRange"`
	AttackSpeed     float64             `json:"attackSpeed"`
	Target          *TargetReference    `json:"target"`
	Players         []NearbyPlayer      `json:"players"`
	Monsters        []MonsterState      `json:"monsters"`
	Items           []GroundItemState   `json:"items"`
}

type testRequest struct {
	Type            string   `json:"type"`
	Username        string   `json:"username,omitempty"`
	Password        string   `json:"password,omitempty"`
	X               *float64 `json:"x,omitempty"`
	Y               *float64 `json:"y,omitempty"`
	Z               *float64 `json:"z,omitempty"`
	VisibilityRange *float64 `json:"visibilityRange,omitempty"`
	HasMoveTarget bool `json:"hasMoveTarget,omitempty"`
	MoveTargetX float64 `json:"moveTargetX,omitempty"`
	MoveTargetZ float64 `json:"moveTargetZ,omitempty"`
	TargetType string  `json:"targetType,omitempty"`
	TargetID   string  `json:"targetId,omitempty"`
	RequestID  int     `json:"requestId,omitempty"`
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
	if response.PositionIsRoot {
		t.Fatal("default spawn is a Terrain surface position, not a character root position")
	}
	if response.RotationX == nil || response.RotationY == nil ||
		response.RotationZ == nil || response.RotationW == nil {
		t.Fatalf("spawn rotation missing: %+v", response)
	}
	if *response.RotationX != 0 || *response.RotationY != 0 ||
		*response.RotationZ != 0 || *response.RotationW != 1 {
		t.Fatalf("default spawn rotation = (%v, %v, %v, %v), want identity quaternion",
			*response.RotationX, *response.RotationY, *response.RotationZ, *response.RotationW)
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

	initialGroundY, ok := world.HeightAtWorld(0.5, 0.5)
	if !ok {
		t.Fatal("initial test position is outside the fixture world")
	}
	initialGroundY += 0.05
	sendRequest(t, conn, testRequest{
		Type:      "player_state",
		X:         float64PointerForTest(0.5),
		Y:         float64PointerForTest(initialGroundY),
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
	if state.Position.X != 0.5 || math.Abs(state.Position.Y-initialGroundY) > 1e-9 || state.Position.Z != 0.5 {
		t.Fatalf("stored position = %+v, want (0.5, %.9f, 0.5)", state.Position, initialGroundY)
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
	updatedGroundY, ok := world.HeightAtWorld(0.75, 0.25)
	if !ok {
		t.Fatal("updated test position is outside the fixture world")
	}
	updatedGroundY += 0.05
	sendRequest(t, conn, testRequest{
		Type:      "player_state",
		X:         float64PointerForTest(0.75),
		Y:         float64PointerForTest(updatedGroundY),
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
	if state.Position.X != 0.75 || math.Abs(state.Position.Y-updatedGroundY) > 1e-9 || state.Position.Z != 0.25 {
		t.Fatalf("updated position = %+v, want (0.75, %.9f, 0.25)", state.Position, updatedGroundY)
	}
	if state.Rotation.X != 0 || state.Rotation.Y != 0 || state.Rotation.Z != 1 || state.Rotation.W != 0 {
		t.Fatalf("updated rotation = %+v, want (0, 0, 1, 0)", state.Rotation)
	}
}

func TestPlayerStatePersistsAndRestoresPositionAndRotation(t *testing.T) {
	world := createTestWorldMap(t)
	stateFile := filepath.Join(t.TempDir(), "player_states.json")
	logger := log.New(io.Discard, "", 0)
	server := New(Config{
		World:     world,
		Logger:    logger,
		StateFile: stateFile,
	})

	firstConn, firstScanner, _ := startPipeConnectionWithServer(t, server)
	sendRequest(t, firstConn, testRequest{Type: "auth", Username: "player1", Password: "player1"})
	assertSuccess(t, readWireResponse(t, firstScanner), true)
	defaultSpawn := readWireResponse(t, firstScanner)
	assertSpawnResponse(t, defaultSpawn)

	qx, qy, qz, qw := 0.1, 0.2, 0.3, 0.9
	savedGroundY, ok := world.HeightAtWorld(0.5, 0.5)
	if !ok {
		t.Fatal("test position is outside the fixture world")
	}
	savedGroundY += 0.05
	sendRequest(t, firstConn, testRequest{
		Type:      "player_state",
		X:         float64PointerForTest(0.5),
		Y:         float64PointerForTest(savedGroundY),
		Z:         float64PointerForTest(0.5),
		RotationX: float64PointerForTest(qx),
		RotationY: float64PointerForTest(qy),
		RotationZ: float64PointerForTest(qz),
		RotationW: float64PointerForTest(qw),
	})
	stateAck := readWireResponse(t, firstScanner)
	if stateAck.Type != "player_state_result" {
		t.Fatalf("state acknowledgement type = %q, want player_state_result", stateAck.Type)
	}
	assertSuccess(t, stateAck, true)

	if err := server.savePlayerStates(); err != nil {
		t.Fatalf("save player state JSON: %v", err)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("player state JSON file was not created: %v", err)
	}

	// Create a fresh server instance to simulate restarting the Go process.
	restartedServer := New(Config{
		World:     world,
		Logger:    logger,
		StateFile: stateFile,
	})
	restoredConn, restoredScanner, _ := startPipeConnectionWithServer(t, restartedServer)
	sendRequest(t, restoredConn, testRequest{Type: "auth", Username: "player1", Password: "player1"})
	assertSuccess(t, readWireResponse(t, restoredScanner), true)
	restoredSpawn := readWireResponse(t, restoredScanner)
	if restoredSpawn.Type != "spawn" || restoredSpawn.Message != "restored_saved_state" {
		t.Fatalf("unexpected restored spawn response: %+v", restoredSpawn)
	}
	assertSuccess(t, restoredSpawn, true)
	if !restoredSpawn.PositionIsRoot {
		t.Fatal("restored spawn must be interpreted as an exact character root position")
	}
	if restoredSpawn.X == nil || restoredSpawn.Y == nil || restoredSpawn.Z == nil ||
		*restoredSpawn.X != 0.5 || math.Abs(*restoredSpawn.Y-savedGroundY) > 1e-9 || *restoredSpawn.Z != 0.5 {
		t.Fatalf("restored position = (%v, %v, %v), want (0.5, %.9f, 0.5)",
			restoredSpawn.X, restoredSpawn.Y, restoredSpawn.Z, savedGroundY)
	}
	if restoredSpawn.RotationX == nil || restoredSpawn.RotationY == nil ||
		restoredSpawn.RotationZ == nil || restoredSpawn.RotationW == nil {
		t.Fatalf("restored rotation missing: %+v", restoredSpawn)
	}
	length := math.Sqrt(qx*qx + qy*qy + qz*qz + qw*qw)
	wantRotation := []float64{qx / length, qy / length, qz / length, qw / length}
	gotRotation := []float64{
		*restoredSpawn.RotationX,
		*restoredSpawn.RotationY,
		*restoredSpawn.RotationZ,
		*restoredSpawn.RotationW,
	}
	for i := range wantRotation {
		if math.Abs(gotRotation[i]-wantRotation[i]) > 1e-9 {
			t.Fatalf("restored rotation component %d = %.12f, want %.12f", i, gotRotation[i], wantRotation[i])
		}
	}
}

func TestSavedPlayerHeightReconcilesAfterTerrainHeightmapChanges(t *testing.T) {
	world := createTestWorldMap(t)
	server := New(Config{
		World:  world,
		Logger: log.New(io.Discard, "", 0),
	})
	saved := PlayerState{
		PlayerID: "player1",
		Position: worldmap.Vec3{X: 0.5, Y: 50, Z: 0.5},
		Rotation: PlayerRotation{W: 1},
		UpdatedAt: time.Now().UTC(),
		Online: true,
	}
	server.playerStates["player1"] = saved
	server.playerSessions["player1"] = nil

	terrainY, ok := world.HeightAtWorld(saved.Position.X, saved.Position.Z)
	if !ok {
		t.Fatal("saved position is outside the fixture world")
	}
	wantY := terrainY + 0.05

	got := server.alignSavedPlayerToCurrentTerrain("player1", saved)
	if math.Abs(got.Position.Y-wantY) > 1e-9 {
		t.Fatalf("restored Y = %.9f, want current terrain Y %.9f", got.Position.Y, wantY)
	}
	if math.Abs(server.playerStates["player1"].Position.Y-wantY) > 1e-9 {
		t.Fatalf("stored Y = %.9f, want corrected terrain Y %.9f",
			server.playerStates["player1"].Position.Y, wantY)
	}
}

func TestGroundHeightAdjustmentUsesNonBlockingCorrection(t *testing.T) {
	world := createTestWorldMap(t)
	position := worldmap.Vec3{X: 0.5, Z: 0.5}
	server := New(Config{
		World:      world,
		SpawnPoint: &position,
		Logger:     log.New(io.Discard, "", 0),
	})
	groundY, ok := world.HeightAtWorld(position.X, position.Z)
	if !ok {
		t.Fatal("test position is outside the fixture world")
	}

	conn, scanner, _ := startPipeConnectionWithServer(t, server)
	authenticateTestPlayer(t, conn, scanner)

	// Send the same horizontal position with a Y that the server grounds to Terrain.
	sendRequest(t, conn, testRequest{
		Type: "player_state",
		X: float64PointerForTest(position.X),
		Y: float64PointerForTest(groundY + 1.0),
		Z: float64PointerForTest(position.Z),
		RotationX: float64PointerForTest(0),
		RotationY: float64PointerForTest(0),
		RotationZ: float64PointerForTest(0),
		RotationW: float64PointerForTest(1),
	})

	correction := readWireResponse(t, scanner)
	if correction.Type != "player_state_result" || correction.Message != "server_ground_correction" {
		t.Fatalf("response = %+v, want non-blocking server_ground_correction", correction)
	}
	if correction.Y == nil || math.Abs(*correction.Y-(groundY+0.05)) > 1e-9 {
		t.Fatalf("corrected Y = %v, want %.9f", correction.Y, groundY+0.05)
	}
	if correction.X != nil || correction.Z != nil {
		t.Fatalf("ground correction should not overwrite horizontal movement: %+v", correction)
	}
}

func TestReconnectMovesSavedPlayerToNearestFreePosition(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})
	savedPosition := worldmap.Vec3{X: 100, Y: 0, Z: 100}
	savedState := PlayerState{
		PlayerID:  "player1",
		Position:  savedPosition,
		Rotation:  PlayerRotation{W: 1},
		UpdatedAt: time.Now().UTC(),
		Online:    true,
	}
	server.playerStates["player1"] = savedState
	server.playerSessions["player1"] = nil

	// Simulate a monster spawning after the player logged out, at the saved position.
	if !server.UpsertMonster(MonsterState{
		ID: "2", TemplateID: "test-sphere-enemy", Name: "Enemy 2",
		Position: savedPosition, Rotation: PlayerRotation{W: 1},
		Health: 100, MaxHealth: 100, Alive: true, Action: "idle",
	}) {
		t.Fatal("failed to add overlapping monster")
	}

	spawn := server.spawnResponseForPlayer("player1")
	if spawn.Type != "spawn" || spawn.Success == nil || !*spawn.Success {
		t.Fatalf("reconnect spawn failed: %+v", spawn)
	}
	if spawn.Message != "restored_saved_state_collision_recovered" {
		t.Fatalf("spawn message = %q, want restored_saved_state_collision_recovered", spawn.Message)
	}
	if !spawn.PositionIsRoot || spawn.X == nil || spawn.Y == nil || spawn.Z == nil {
		t.Fatalf("recovered spawn must include exact character-root coordinates: %+v", spawn)
	}

	resolved := worldmap.Vec3{X: *spawn.X, Y: *spawn.Y, Z: *spawn.Z}
	distance := math.Hypot(resolved.X-savedPosition.X, resolved.Z-savedPosition.Z)
	if math.Abs(distance-1.5) > 0.001 {
		t.Fatalf("recovery displacement = %.6f, want shortest displacement of 1.5m", distance)
	}
	if server.playerPositionCollides("player1", resolved) {
		t.Fatalf("reconnected player still intersects the monster at %+v", resolved)
	}

	stored := server.playerStates["player1"]
	if math.Abs(stored.Position.X-resolved.X) > 1e-9 ||
		math.Abs(stored.Position.Y-resolved.Y) > 1e-9 ||
		math.Abs(stored.Position.Z-resolved.Z) > 1e-9 {
		t.Fatalf("persisted player position %+v differs from collision-free spawn %+v",
			stored.Position, resolved)
	}
}

func TestReconnectUsesShortestEjectionAtGateEdge(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})
	// The player clips the near side of a narrow gate. Facing +X means the old
	// backwards-only recovery would travel through the gate towards -X.
	savedPosition := worldmap.Vec3{X: 100.65, Y: 0, Z: 100}
	savedRotation := PlayerRotation{Y: math.Sqrt(0.5), W: math.Sqrt(0.5)}
	server.playerStates["player1"] = PlayerState{
		PlayerID: "player1", Position: savedPosition, Rotation: savedRotation,
		UpdatedAt: time.Now().UTC(), Online: true,
	}
	server.playerSessions["player1"] = nil

	if !server.UpsertMonster(MonsterState{
		ID: "gate", TemplateID: "test-cube-enemy", Name: "Gate",
		Position: worldmap.Vec3{X: 100, Y: 0, Z: 100}, Rotation: PlayerRotation{W: 1},
		Collider: ColliderDefinition{
			Shape: ColliderBox, Height: 2,
			Size: worldmap.Vec3{X: 1, Y: 2, Z: 4},
		},
		Health: 100, MaxHealth: 100, Alive: true, Action: "idle",
	}) {
		t.Fatal("failed to add gate collider")
	}

	spawn := server.spawnResponseForPlayer("player1")
	if spawn.Type != "spawn" || spawn.Success == nil || !*spawn.Success {
		t.Fatalf("reconnect spawn failed: %+v", spawn)
	}
	resolved := worldmap.Vec3{X: *spawn.X, Y: *spawn.Y, Z: *spawn.Z}
	distance := math.Hypot(resolved.X-savedPosition.X, resolved.Z-savedPosition.Z)
	if resolved.X <= savedPosition.X {
		t.Fatalf("player was not ejected through the nearest side of the gate: old=%+v new=%+v", savedPosition, resolved)
	}
	if math.Abs(distance-0.35) > 0.001 {
		t.Fatalf("gate recovery displacement = %.6f, want shortest displacement of about 0.35m", distance)
	}
	if server.playerPositionCollides("player1", resolved) {
		t.Fatalf("reconnected player still intersects the gate at %+v", resolved)
	}
}

func TestWorldInterestReturnsOnlyNearbyEntitiesAndClampsRange(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})

	if !server.UpsertMonster(MonsterState{
		ID: "monster-near", Name: "Nearby", Position: worldmap.Vec3{X: 20, Y: 50, Z: 0.5},
		Health: 100, MaxHealth: 100, Alive: true, AIState: "idle",
	}) {
		t.Fatal("failed to add nearby monster")
	}
	if !server.UpsertMonster(MonsterState{
		ID: "monster-far", Name: "Far away", Position: worldmap.Vec3{X: 100, Y: 50, Z: 0.5},
		Health: 100, MaxHealth: 100, Alive: true, AIState: "idle",
	}) {
		t.Fatal("failed to add distant monster")
	}
	if !server.AddGroundItem(GroundItemState{
		ID: "item-near", Name: "Nearby item", Position: worldmap.Vec3{X: 30, Y: 50, Z: 0.5}, Quantity: 1,
	}) {
		t.Fatal("failed to add nearby ground item")
	}
	if !server.AddGroundItem(GroundItemState{
		ID: "item-far", Name: "Distant item", Position: worldmap.Vec3{X: 80, Y: 50, Z: 0.5}, Quantity: 1,
	}) {
		t.Fatal("failed to add distant ground item")
	}

	conn, scanner, _ := startPipeConnectionWithServer(t, server)
	authenticateTestPlayer(t, conn, scanner)

	// The server must base visibility on the last accepted player position,
	// not on a client-supplied query center.
	sendRequest(t, conn, testRequest{
		Type:      "player_state",
		X:         float64PointerForTest(0.5),
		Y:         float64PointerForTest(50),
		Z:         float64PointerForTest(0.5),
		RotationX: float64PointerForTest(0),
		RotationY: float64PointerForTest(0),
		RotationZ: float64PointerForTest(0),
		RotationW: float64PointerForTest(1),
	})
	stateResponse := readWireResponse(t, scanner)
	if stateResponse.Type != "player_state_result" {
		t.Fatalf("player state response type = %q, want player_state_result", stateResponse.Type)
	}
	assertSuccess(t, stateResponse, true)

	if !server.SetPlayerAttackSpeed("player1", 2.5) {
		t.Fatal("failed to set player's effective attack speed")
	}

	sendRequest(t, conn, testRequest{
		Type:            "world_interest",
		VisibilityRange: float64PointerForTest(20), // server enforces the minimum of 50
	})

	response := readWireResponse(t, scanner)
	if response.Type != "world_snapshot" {
		t.Fatalf("response type = %q, want world_snapshot", response.Type)
	}
	assertSuccess(t, response, true)
	if math.Abs(response.AttackSpeed-2.5) > 0.0001 {
		t.Fatalf("snapshot attack speed = %.3f, want 2.5 attacks/second", response.AttackSpeed)
	}
	if response.VisibilityRange == nil || *response.VisibilityRange != MinVisibilityRange {
		t.Fatalf("visibility range = %v, want clamped minimum %.0f", response.VisibilityRange, MinVisibilityRange)
	}
	if len(response.Monsters) != 1 || response.Monsters[0].ID != "monster-near" {
		t.Fatalf("nearby monsters = %+v, want only monster-near", response.Monsters)
	}
	if len(response.Items) != 1 || response.Items[0].ID != "item-near" {
		t.Fatalf("nearby items = %+v, want only item-near", response.Items)
	}
}

func TestPlayerAttackSpeedDefaultsAndCanChange(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})

	if got := server.PlayerAttackSpeed("player1"); got != DefaultAttackSpeed {
		t.Fatalf("default attack speed = %.3f, want %.3f", got, DefaultAttackSpeed)
	}
	if !server.SetPlayerAttackSpeed("player1", 3.25) {
		t.Fatal("valid attack speed update was rejected")
	}
	if got := server.PlayerAttackSpeed("player1"); got != 3.25 {
		t.Fatalf("updated attack speed = %.3f, want 3.25", got)
	}
	if !server.SetPlayerAttackSpeed("player1", 100) || server.PlayerAttackSpeed("player1") != MaxAttackSpeed {
		t.Fatalf("attack speed should be clamped to maximum %.1f", MaxAttackSpeed)
	}
	if server.SetPlayerAttackSpeed("player1", math.NaN()) {
		t.Fatal("NaN attack speed should be rejected")
	}
	if server.SetPlayerAttackSpeed("", 2) {
		t.Fatal("empty player ID should be rejected")
	}
}


func TestAttackCommandValidatesRangeAndCooldown(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})
	if !server.UpsertMonster(MonsterState{
		ID: "target-near", TemplateID: "test-sphere-enemy", Name: "Target",
		Position: worldmap.Vec3{X: 11.7, Y: 50, Z: 10},
		Health: 100, MaxHealth: 100, Alive: true,
	}) {
		t.Fatal("failed to add nearby attack target")
	}
	if !server.UpsertMonster(MonsterState{
		ID: "target-far", TemplateID: "test-sphere-enemy", Name: "Far Target",
		Position: worldmap.Vec3{X: 30, Y: 50, Z: 10},
		Health: 100, MaxHealth: 100, Alive: true,
	}) {
		t.Fatal("failed to add distant attack target")
	}

	conn, scanner, _ := startPipeConnectionWithServer(t, server)
	authenticateTestPlayer(t, conn, scanner)
	sendRequest(t, conn, testRequest{
		Type: "player_state",
		X: float64PointerForTest(10), Y: float64PointerForTest(50), Z: float64PointerForTest(10),
		RotationX: float64PointerForTest(0), RotationY: float64PointerForTest(0),
		RotationZ: float64PointerForTest(0), RotationW: float64PointerForTest(1),
	})
	stateResponse := readWireResponse(t, scanner)
	if stateResponse.Type != "player_state_result" {
		t.Fatalf("player state response type = %q, want player_state_result", stateResponse.Type)
	}
	assertSuccess(t, stateResponse, true)

	if !server.SetPlayerAttackSpeed("player1", 2.0) {
		t.Fatal("failed to set attack speed")
	}

	// A command without a server-selected target must be rejected.
	sendRequest(t, conn, testRequest{Type: "attack", RequestID: 100})
	noTargetResponse := readWireResponse(t, scanner)
	assertSuccess(t, noTargetResponse, false)
	if noTargetResponse.Message != "target_not_selected" {
		t.Fatalf("no-target attack response = %+v", noTargetResponse)
	}

	// Selection is a separate protocol command. Attack itself carries no target ID.
	sendRequest(t, conn, testRequest{
		Type: "set_target", TargetType: "monster", TargetID: "target-near",
	})
	selectionResponse := readWireResponse(t, scanner)
	if selectionResponse.Type != "target_result" || selectionResponse.Target == nil ||
		selectionResponse.Target.ID != "target-near" {
		t.Fatalf("selection response = %+v, want target-near", selectionResponse)
	}

	sendRequest(t, conn, testRequest{Type: "attack", RequestID: 101})
	attackResponse := readWireResponse(t, scanner)
	if attackResponse.Type != "attack_result" {
		t.Fatalf("attack response type = %q, want attack_result", attackResponse.Type)
	}
	assertSuccess(t, attackResponse, true)
	if attackResponse.RequestID != 101 {
		t.Fatalf("attack response requestId = %d, want 101", attackResponse.RequestID)
	}
	if attackResponse.Target == nil || attackResponse.Target.ID != "target-near" {
		t.Fatalf("attack target = %+v, want target-near", attackResponse.Target)
	}
	if math.Abs(attackResponse.AttackSpeed-2.0) > 0.0001 {
		t.Fatalf("attack speed = %.3f, want 2.0", attackResponse.AttackSpeed)
	}

	// A second attack within the server-controlled interval is rejected.
	sendRequest(t, conn, testRequest{Type: "attack", RequestID: 102})
	cooldownResponse := readWireResponse(t, scanner)
	assertSuccess(t, cooldownResponse, false)
	if cooldownResponse.Type != "attack_result" || cooldownResponse.Message != "attack_cooldown" {
		t.Fatalf("cooldown response = %+v", cooldownResponse)
	}
	if cooldownResponse.RetryAfter <= 0 {
		t.Fatalf("retryAfter = %.3f, want positive cooldown", cooldownResponse.RetryAfter)
	}

	// Switch the server-side target, then ensure the targetless attack command uses it.
	sendRequest(t, conn, testRequest{
		Type: "set_target", TargetType: "monster", TargetID: "target-far",
	})
	farSelection := readWireResponse(t, scanner)
	if farSelection.Type != "target_result" || farSelection.Target == nil ||
		farSelection.Target.ID != "target-far" {
		t.Fatalf("far selection response = %+v, want target-far", farSelection)
	}

	// Range is validated before the cooldown so the client can resume approach immediately.
	sendRequest(t, conn, testRequest{Type: "attack", RequestID: 103})
	rangeResponse := readWireResponse(t, scanner)
	assertSuccess(t, rangeResponse, false)
	if rangeResponse.Message != "attack_out_of_range" || rangeResponse.RequestID != 103 {
		t.Fatalf("out-of-range response = %+v", rangeResponse)
	}
}

func TestFirstPlayerMovementIsSweptFromServerSpawn(t *testing.T) {
	world := createTestWorldMap(t)
	spawn := worldmap.Vec3{X: 0.1, Y: 50, Z: 0.5}
	server := New(Config{
		World:     world,
		SpawnPoint: &spawn,
		Logger:    log.New(io.Discard, "", 0),
	})
	obstacleGroundY, ok := world.HeightAtWorld(0.5, 0.5)
	if !ok {
		t.Fatal("obstacle position is outside the fixture world")
	}
	if !server.UpsertMonster(MonsterState{
		ID: "obstacle", TemplateID: "test-cube-enemy", Name: "Obstacle",
		Position: worldmap.Vec3{X: 0.5, Y: obstacleGroundY, Z: 0.5},
		Rotation: PlayerRotation{W: 1},
		Collider: ColliderDefinition{
			Shape: ColliderBox, Height: 2,
			Size: worldmap.Vec3{X: 0.2, Y: 2, Z: 0.2},
		},
		Alive: true,
	}) {
		t.Fatal("failed to add test obstacle")
	}

	conn, scanner, _ := startPipeConnectionWithServer(t, server)
	authenticateTestPlayer(t, conn, scanner)

	// Simulate the first transform arriving after the player has already started
	// moving from spawn. It must still be swept, not accepted as a teleport.
	sendRequest(t, conn, testRequest{
		Type: "player_state",
		X: float64PointerForTest(0.9), Y: float64PointerForTest(50.05), Z: float64PointerForTest(0.5),
		RotationX: float64PointerForTest(0), RotationY: float64PointerForTest(0),
		RotationZ: float64PointerForTest(0), RotationW: float64PointerForTest(1),
	})
	response := readWireResponse(t, scanner)
	if response.Type != "player_state_result" || response.Message != "server_collision_correction" {
		t.Fatalf("first-movement response = %+v, want server_collision_correction", response)
	}
	assertSuccess(t, response, true)
	if response.X == nil || *response.X > 0.451 {
		t.Fatalf("first accepted X = %v, want player stopped at the near side of box (X <= 0.45)", response.X)
	}
}

func TestPlayerCanRecoverWhenAlreadyInsideMonsterCollider(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})
	if !server.UpsertMonster(MonsterState{
		ID: "sphere", TemplateID: "test-sphere-enemy", Name: "Enemy 2",
		Position: worldmap.Vec3{X: 0, Y: 0, Z: 0},
		Rotation: PlayerRotation{W: 1}, Alive: true,
	}) {
		t.Fatal("failed to add sphere monster")
	}

	// Simulate a character whose saved/previous position is already at the centre
	// of the sphere. Trying to move straight away must not trap it at that position.
	resolved := server.resolvePlayerMovement(
		"player1",
		worldmap.Vec3{X: 0, Y: 0, Z: 0},
		worldmap.Vec3{X: 0, Y: 0, Z: 0},
	)
	if server.playerPositionCollides("player1", resolved) {
		t.Fatalf("recovered position %+v still intersects the sphere", resolved)
	}
	if distance := math.Hypot(resolved.X, resolved.Z); distance < 1.499 {
		t.Fatalf("recovered distance from sphere centre = %.3f, want at least 1.5", distance)
	}
}

func TestPlayerCanMoveOutWhenItsPreviousPositionOverlapsBox(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})
	if !server.UpsertMonster(MonsterState{
		ID: "dog", TemplateID: "test-cube-enemy", Name: "Enemy 1",
		Position: worldmap.Vec3{X: 0, Y: 0, Z: 0},
		Rotation: PlayerRotation{W: 1}, Alive: true,
	}) {
		t.Fatal("failed to add box monster")
	}

	resolved := server.resolvePlayerMovement(
		"player1",
		worldmap.Vec3{X: 0, Y: 0, Z: 0},
		worldmap.Vec3{X: 3, Y: 0, Z: 0},
	)
	if server.playerPositionCollides("player1", resolved) {
		t.Fatalf("player could not escape box collision; resolved position=%+v", resolved)
	}
	if resolved.X < 1.2 {
		t.Fatalf("resolved X = %.3f, want player to escape to the outside of the box", resolved.X)
	}
}

func TestPlayerMovementStopsAtBoxMonster(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})
	if !server.UpsertMonster(MonsterState{
		ID: "dog", TemplateID: "test-cube-enemy", Name: "Enemy 1",
		Position: worldmap.Vec3{X: 2, Y: 0, Z: 0},
		Rotation: PlayerRotation{W: 1}, Alive: true,
	}) {
		t.Fatal("failed to add box monster")
	}

	resolved := server.resolvePlayerMovement(
		"player1",
		worldmap.Vec3{X: 0, Y: 0, Z: 0},
		worldmap.Vec3{X: 2, Y: 0, Z: 0},
	)
	if resolved.X > 0.501 || resolved.X < 0.1 {
		t.Fatalf("resolved X = %.3f; want player stopped near box edge at X <= 0.5 and > 0.1", resolved.X)
	}
	if math.Abs(resolved.Z) > 1e-9 {
		t.Fatalf("unexpected lateral movement around a head-on obstacle: Z=%v", resolved.Z)
	}
}

func TestPlayerMovementStopsAtOtherPlayer(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})
	server.playerStates["player2"] = PlayerState{
		PlayerID: "player2",
		Position: worldmap.Vec3{X: 2, Y: 0, Z: 0},
		Online:   true,
	}
	server.playerSessions["player2"] = nil

	resolved := server.resolvePlayerMovement(
		"player1",
		worldmap.Vec3{X: 0, Y: 0, Z: 0},
		worldmap.Vec3{X: 2, Y: 0, Z: 0},
	)
	if resolved.X > 1.001 || resolved.X < 0.8 {
		t.Fatalf("resolved X = %.3f; want player stopped near other player's cylinder at X <= 1.0", resolved.X)
	}
}

func TestBoxCollisionUsesServerRotation(t *testing.T) {
	rotated90 := PlayerRotation{Y: math.Sqrt(0.5), W: math.Sqrt(0.5)}
	if !circleOverlapsRotatedBox(1.8, 0, PlayerColliderRadius, 0, 0, 2, 4, rotated90) {
		t.Fatal("player circle should overlap the long side of a box rotated 90 degrees")
	}
	if circleOverlapsRotatedBox(1.8, 0, PlayerColliderRadius, 0, 0, 2, 4, PlayerRotation{W: 1}) {
		t.Fatal("player circle should not overlap the short side of the same unrotated box")
	}
}

func TestPlayerStateSendsServerCollisionCorrection(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})
	if !server.UpsertMonster(MonsterState{
		ID: "dog", TemplateID: "test-cube-enemy", Name: "Enemy 1",
		Position: worldmap.Vec3{X: 2, Y: 0, Z: 0},
		Rotation: PlayerRotation{W: 1}, Alive: true,
	}) {
		t.Fatal("failed to add box monster")
	}

	conn, scanner, _ := startPipeConnectionWithServer(t, server)
	authenticateTestPlayer(t, conn, scanner)

	sendRequest(t, conn, testRequest{
		Type: "player_state",
		X: float64PointerForTest(0), Y: float64PointerForTest(0), Z: float64PointerForTest(0),
		RotationX: float64PointerForTest(0), RotationY: float64PointerForTest(0),
		RotationZ: float64PointerForTest(0), RotationW: float64PointerForTest(1),
	})
	first := readWireResponse(t, scanner)
	if first.Type != "player_state_result" || first.Message != "state_registered" {
		t.Fatalf("initial position response = %+v", first)
	}

	sendRequest(t, conn, testRequest{
		Type: "player_state",
		X: float64PointerForTest(2), Y: float64PointerForTest(0), Z: float64PointerForTest(0),
		RotationX: float64PointerForTest(0), RotationY: float64PointerForTest(0),
		RotationZ: float64PointerForTest(0), RotationW: float64PointerForTest(1),
	})
	correction := readWireResponse(t, scanner)
	if correction.Type != "player_state_result" || correction.Message != "server_collision_correction" {
		t.Fatalf("collision response = %+v, want server_collision_correction", correction)
	}
	assertSuccess(t, correction, true)
	if correction.X == nil || *correction.X > 0.501 {
		t.Fatalf("corrected X = %v, want X <= 0.5", correction.X)
	}
	if got := server.playerStates["player1"].Position.X; math.Abs(got-*correction.X) > 1e-9 {
		t.Fatalf("stored X = %v, correction X = %v", got, *correction.X)
	}
}

func TestPlayerMovementRetryStopsAfterConfiguredTimeout(t *testing.T) {
	server := New(Config{
		Logger: log.New(io.Discard, "", 0),
		MovementRetryTimeout: 50 * time.Millisecond,
	})
	if !server.UpsertMonster(MonsterState{
		ID: "wall", TemplateID: "test-cube-enemy", Name: "Wall",
		Position: worldmap.Vec3{X: 2, Y: 0, Z: 0}, Rotation: PlayerRotation{W: 1}, Alive: true,
		Collider: ColliderDefinition{Shape: ColliderBox, Size: worldmap.Vec3{X: 2, Y: 2, Z: 4}, Height: 2},
	}) {
		t.Fatal("failed to add movement blocker")
	}

	conn, scanner, _ := startPipeConnectionWithServer(t, server)
	authenticateTestPlayer(t, conn, scanner)
	requestAt := func(x float64) {
		t.Helper()
		sendRequest(t, conn, testRequest{
			Type: "player_state",
			X: float64PointerForTest(x), Y: float64PointerForTest(0), Z: float64PointerForTest(0),
			RotationX: float64PointerForTest(0), RotationY: float64PointerForTest(0),
			RotationZ: float64PointerForTest(0), RotationW: float64PointerForTest(1),
			HasMoveTarget: true, MoveTargetX: 10, MoveTargetZ: 0,
		})
	}

	requestAt(0)
	first := readWireResponse(t, scanner)
	if first.Type != "player_state_result" || first.Message != "state_registered" {
		t.Fatalf("initial movement response = %+v", first)
	}

	requestAt(2)
	collision := readWireResponse(t, scanner)
	if collision.Type != "player_state_result" || collision.Message != "server_collision_correction" {
		t.Fatalf("blocked movement response = %+v, want server_collision_correction", collision)
	}

	time.Sleep(75 * time.Millisecond)
	requestAt(*collision.X)
	exhausted := readWireResponse(t, scanner)
	if exhausted.Type != "player_state_result" || exhausted.Message != "movement_retry_exhausted" {
		t.Fatalf("stalled movement response = %+v, want movement_retry_exhausted", exhausted)
	}
	if exhausted.X == nil || math.Abs(*exhausted.X-*collision.X) > 1e-9 {
		t.Fatalf("movement stop should return the last accepted position, got %+v after %+v", exhausted, collision)
	}
}

func TestVisibilityRangeClampsToMaximum(t *testing.T) {
	if got := clampVisibilityRange(1000); got != MaxVisibilityRange {
		t.Fatalf("clampVisibilityRange(1000) = %v, want %v", got, MaxVisibilityRange)
	}
	if got := clampVisibilityRange(100); got != DefaultVisibilityRange {
		t.Fatalf("clampVisibilityRange(100) = %v, want %v", got, DefaultVisibilityRange)
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


func TestTargetSelectionTracksLiveEntitiesAndClearsWhenRemoved(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})
	if !server.UpsertMonster(MonsterState{
		ID: "monster-1", TemplateID: "test-sphere-enemy", Name: "Enemy One",
		Position: worldmap.Vec3{X: 10, Y: 0, Z: 10},
		Health: 100, MaxHealth: 100, Alive: true, Action: "idle",
	}) {
		t.Fatal("failed to add target monster")
	}
	if !server.AddGroundItem(GroundItemState{
		ID: "item-1", Name: "Dropped Sword",
		Position: worldmap.Vec3{X: 12, Y: 0, Z: 10}, Quantity: 1,
	}) {
		t.Fatal("failed to add target item")
	}

	// Model another connected player in the same server-owned session registry.
	server.playersMu.Lock()
	server.playerStates["player2"] = PlayerState{
		PlayerID: "player2", Position: worldmap.Vec3{X: 11, Y: 0, Z: 10},
		Rotation: PlayerRotation{W: 1}, UpdatedAt: time.Now().UTC(), Online: true,
	}
	server.playerSessions["player2"] = nil
	server.playersMu.Unlock()

	conn, scanner, _ := startPipeConnectionWithServer(t, server)
	authenticateTestPlayer(t, conn, scanner)

	sendRequest(t, conn, testRequest{
		Type: "player_state",
		X: float64PointerForTest(1), Y: float64PointerForTest(0), Z: float64PointerForTest(1),
		RotationX: float64PointerForTest(0), RotationY: float64PointerForTest(0),
		RotationZ: float64PointerForTest(0), RotationW: float64PointerForTest(1),
	})
	stateResponse := readWireResponse(t, scanner)
	if stateResponse.Type != "player_state_result" {
		t.Fatalf("player state response = %+v, want player_state_result", stateResponse)
	}

	sendRequest(t, conn, testRequest{Type: "set_target", TargetType: "monster", TargetID: "monster-1"})
	monsterResponse := readWireResponse(t, scanner)
	if monsterResponse.Type != "target_result" {
		t.Fatalf("monster target response type = %q, want target_result", monsterResponse.Type)
	}
	assertSuccess(t, monsterResponse, true)
	if monsterResponse.Target == nil || monsterResponse.Target.Type != "monster" ||
		monsterResponse.Target.ID != "monster-1" || monsterResponse.Target.Name != "Enemy One" {
		t.Fatalf("unexpected monster target reference: %+v", monsterResponse.Target)
	}

	sendRequest(t, conn, testRequest{Type: "world_interest"})
	snapshot := readWireResponse(t, scanner)
	if snapshot.Type != "world_snapshot" || snapshot.Target == nil ||
		snapshot.Target.Type != "monster" || snapshot.Target.ID != "monster-1" {
		t.Fatalf("world snapshot did not retain selected target: %+v", snapshot)
	}

	sendRequest(t, conn, testRequest{Type: "set_target", TargetType: "player", TargetID: "player2"})
	playerResponse := readWireResponse(t, scanner)
	assertSuccess(t, playerResponse, true)
	if playerResponse.Target == nil || playerResponse.Target.Type != "player" ||
		playerResponse.Target.ID != "player2" || playerResponse.Target.Name != "player2" {
		t.Fatalf("unexpected player target reference: %+v", playerResponse.Target)
	}

	sendRequest(t, conn, testRequest{Type: "set_target", TargetType: "item", TargetID: "item-1"})
	itemResponse := readWireResponse(t, scanner)
	assertSuccess(t, itemResponse, true)
	if itemResponse.Target == nil || itemResponse.Target.Type != "item" ||
		itemResponse.Target.ID != "item-1" || itemResponse.Target.Name != "Dropped Sword" {
		t.Fatalf("unexpected item target reference: %+v", itemResponse.Target)
	}

	if !server.RemoveGroundItem("item-1") {
		t.Fatal("failed to remove item target")
	}
	if target := server.currentPlayerTarget("player1"); target != nil {
		t.Fatalf("target still points to picked-up item: %+v", target)
	}

	sendRequest(t, conn, testRequest{Type: "set_target", TargetType: "monster", TargetID: "missing"})
	invalidResponse := readWireResponse(t, scanner)
	if invalidResponse.Type != "target_result" || invalidResponse.Message != "target_not_found" {
		t.Fatalf("invalid target response = %+v", invalidResponse)
	}
	assertSuccess(t, invalidResponse, false)
	if target := server.currentPlayerTarget("player1"); target != nil {
		t.Fatalf("invalid target request left a target selected: %+v", target)
	}

	sendRequest(t, conn, testRequest{Type: "set_target", TargetType: "monster", TargetID: "monster-1"})
	assertSuccess(t, readWireResponse(t, scanner), true)
	if !server.UpsertMonster(MonsterState{
		ID: "monster-1", TemplateID: "test-sphere-enemy", Name: "Enemy One",
		Position: worldmap.Vec3{X: 10, Y: 0, Z: 10},
		Health: 0, MaxHealth: 100, Alive: false, Action: "dead",
	}) {
		t.Fatal("failed to mark target monster dead")
	}
	if target := server.currentPlayerTarget("player1"); target != nil {
		t.Fatalf("dead monster is still targeted: %+v", target)
	}
}

func TestMonsterFallSettlesAtServerGroundAndKeepsRotation(t *testing.T) {
	server := New(Config{Logger: log.New(io.Discard, "", 0)})
	server.monsters["1"] = MonsterState{
		ID:                "1",
		Name:              "Enemy 1",
		Position:          worldmap.Vec3{X: 10, Y: 5, Z: 10},
		Rotation:          PlayerRotation{X: 0, Y: 0.70710678, Z: 0, W: 0.70710678},
		Alive:             true,
		Action:            "falling",
		AIState:           "falling",
		groundY:           0,
		verticalVelocity:  0,
		falling:           true,
	}

	for i := 0; i < 100; i++ {
		server.simulateMonsterFall(monsterPhysicsTick.Seconds())
	}

	got := server.monsters["1"]
	if got.Position.Y != 0 {
		t.Fatalf("landed Y = %v, want ground Y 0", got.Position.Y)
	}
	if got.falling {
		t.Fatal("monster is still marked as falling after reaching the ground")
	}
	if got.Action != "idle" || got.AIState != "idle" {
		t.Fatalf("landed monster state = action %q, AI %q; want idle/idle", got.Action, got.AIState)
	}
	if got.Rotation.Y != 0.70710678 || got.Rotation.W != 0.70710678 {
		t.Fatalf("server rotation changed during fall: %+v", got.Rotation)
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

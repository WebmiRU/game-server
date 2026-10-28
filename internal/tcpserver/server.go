package tcpserver

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"game-server/internal/logging"
	"game-server/internal/worldmap"
)

const (
	DefaultPlayerMaxHealth = 100
	ProtocolVersion = 1
	defaultAddress = ":7777"
	defaultMaxLine = 4096
	maxAuthAttempts = 5
	readTimeout    = 10 * time.Minute
	writeTimeout   = 10 * time.Second
	defaultStateSaveInterval = time.Second
	defaultMovementRetryTimeout = 5 * time.Second
	movementTargetArrivalDistance = 0.2
	movementMinProgressDistance = 0.03
	movementMaxDetourDistance = 0.75
	playerStateLogInterval = time.Second
	playerStateFileVersion = 1
)

var developmentCredentials = map[string]string{
	"player1": "player1",
	"player2": "player2",
	"player3": "player3",
}

// defaultSpawnPoint is a point on the Unity Terrain surface, not the character capsule root.
var defaultSpawnPoint = worldmap.Vec3{X: 539.56, Y: 74.17, Z: 555.99}

// Config configures the TCP game server.
type Config struct {
	Address         string
	World           *worldmap.Map
	Logger          *log.Logger
	MaxMessageBytes int
	// SpawnPoint is the initial world-surface position sent when no saved player state exists.
	// If nil, the development spawn point is used.
	SpawnPoint *worldmap.Vec3
	// StateFile stores the last accepted player transform for each player ID. Empty disables persistence.
	StateFile string
	// StateSaveInterval controls how often dirty state is flushed to JSON. Defaults to one second.
	StateSaveInterval time.Duration
	// MovementRetryTimeout is how long a commanded move may make no useful progress before being cancelled.
	// Defaults to five seconds.
	MovementRetryTimeout time.Duration
}

// Server authenticates TCP clients, tracks online transforms, and answers world queries.
type Server struct {
	address         string
	world           *worldmap.Map
	logger          *log.Logger
	maxMessageBytes int
	spawnPoint      worldmap.Vec3
	stateFile       string
	stateSaveInterval time.Duration
	movementRetryTimeout time.Duration

	// movementMu serializes collision resolution and position acceptance across clients.
	// Without it, two concurrent requests could both pass against stale player positions.
	movementMu     sync.Mutex
	movementProgress map[string]movementProgressState
	playersMu      sync.RWMutex
	playerStates   map[string]PlayerState
	playerSessions map[string]net.Conn
	worldInterestRanges map[string]float64
	stateVersion   uint64
	savedVersion   uint64

	// Serialize writes per TCP connection without blocking unrelated clients.
	writeLocksMu sync.Mutex
	writeLocks   map[net.Conn]*connectionWriteLock

	entitiesMu             sync.RWMutex
	monsters               map[string]MonsterState
	monsterSpawnPoints     map[string]MonsterSpawnPoint
	monsterSpawnIDByEntity map[string]string
	groundItems            map[string]GroundItemState

	targetsMu              sync.RWMutex
	playerTargets          map[string]TargetReference
	pendingPickupTargets   map[string]string // protected by targetsMu
	inventoryMu            sync.RWMutex
	playerInventories      map[string][]InventoryStack

	// Effective per-player attack speed is runtime state (attacks per second), so buffs
	// and debuffs can change it without persisting temporary modifiers to disk.
	combatStatsMu sync.RWMutex
	attackSpeeds  map[string]float64
	lastAttackAt  map[string]time.Time

	stateSaveMu sync.Mutex
	stateLogMu  sync.Mutex
	lastStateLog time.Time

	connectionsMu sync.Mutex
	connections   map[net.Conn]struct{}
	shuttingDown  bool
	handlers      sync.WaitGroup
}

type connectionWriteLock struct {
	mutex      sync.Mutex
	references int
}

// New creates a TCP server with safe defaults for the configured address and logger.
func New(config Config) *Server {
	address := strings.TrimSpace(config.Address)
	if address == "" {
		address = defaultAddress
	}

	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}

	maxMessageBytes := config.MaxMessageBytes
	if maxMessageBytes < 256 {
		maxMessageBytes = defaultMaxLine
	}

	spawnPoint := defaultSpawnPoint
	if config.SpawnPoint != nil {
		spawnPoint = *config.SpawnPoint
	}
	stateSaveInterval := config.StateSaveInterval
	if stateSaveInterval <= 0 {
		stateSaveInterval = defaultStateSaveInterval
	}
	movementRetryTimeout := config.MovementRetryTimeout
	if movementRetryTimeout <= 0 {
		movementRetryTimeout = defaultMovementRetryTimeout
	}

	server := &Server{
		address:           address,
		world:             config.World,
		logger:            logger,
		maxMessageBytes:   maxMessageBytes,
		spawnPoint:        spawnPoint,
		stateFile:         strings.TrimSpace(config.StateFile),
		stateSaveInterval: stateSaveInterval,
		movementRetryTimeout: movementRetryTimeout,
		movementProgress:    make(map[string]movementProgressState),
		playerStates:        make(map[string]PlayerState),
		playerSessions:      make(map[string]net.Conn),
		worldInterestRanges: make(map[string]float64),
		writeLocks:           make(map[net.Conn]*connectionWriteLock),
		monsters:               make(map[string]MonsterState),
		monsterSpawnPoints:     make(map[string]MonsterSpawnPoint),
		monsterSpawnIDByEntity: make(map[string]string),
		groundItems:            make(map[string]GroundItemState),
		playerTargets:        make(map[string]TargetReference),
		pendingPickupTargets: make(map[string]string),
		playerInventories:    make(map[string][]InventoryStack),
		attackSpeeds:         make(map[string]float64),
		lastAttackAt:      make(map[string]time.Time),
		connections:       make(map[net.Conn]struct{}),
	}
	server.seedTestMonsters()
	if err := server.loadPlayerStates(); err != nil {
		logger.Printf("WARNING: cannot load player states from %q: %v; using fresh spawns", server.stateFile, err)
	}
	return server
}

type request struct {
	Type            string   `json:"type"`
	Username        string   `json:"username,omitempty"`
	Password        string   `json:"password,omitempty"`
	X               *float64 `json:"x,omitempty"`
	Y               *float64 `json:"y,omitempty"`
	Z               *float64 `json:"z,omitempty"`
	VisibilityRange  *float64 `json:"visibilityRange,omitempty"`
	HasMoveTarget    bool    `json:"hasMoveTarget,omitempty"`
	MoveTargetX      float64 `json:"moveTargetX,omitempty"`
	MoveTargetZ      float64 `json:"moveTargetZ,omitempty"`
	TargetType       string  `json:"targetType,omitempty"`
	TargetID         string  `json:"targetId,omitempty"`
	RequestID        int     `json:"requestId,omitempty"`
	RotationX *float64 `json:"rotationX,omitempty"`
	RotationY *float64 `json:"rotationY,omitempty"`
	RotationZ *float64 `json:"rotationZ,omitempty"`
	RotationW *float64 `json:"rotationW,omitempty"`
}

// PlayerRotation is a normalized 3D quaternion.
type PlayerRotation struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
	W float64 `json:"w"`
}

// PlayerState stores the last server-accepted transform for one connected player.
// Position is the character Transform.position (the capsule root), not the ground point.
type PlayerState struct {
	PlayerID  string         `json:"playerId"`
	Position  worldmap.Vec3  `json:"position"`
	Rotation  PlayerRotation `json:"rotation"`
	Health    int            `json:"health"`
	MaxHealth int            `json:"maxHealth"`
	UpdatedAt time.Time      `json:"updatedAt"`
	Online    bool           `json:"-"`
}

type playerStateFile struct {
	Version int                    `json:"version"`
	Players map[string]PlayerState `json:"players"`
}

func normalizePlayerHealth(state PlayerState) PlayerState {
	if state.MaxHealth <= 0 {
		state.MaxHealth = DefaultPlayerMaxHealth
		// Backfill health only for pre-HP saved states. A legitimate zero HP value
		// with a known MaxHealth must remain zero.
		if state.Health <= 0 {
			state.Health = state.MaxHealth
		}
	}
	if state.Health < 0 {
		state.Health = 0
	}
	if state.Health > state.MaxHealth {
		state.Health = state.MaxHealth
	}
	return state
}

type movementProgressState struct {
	targetX       float64
	targetZ       float64
	bestDistance  float64
	lastProgressAt time.Time
}

type response struct {
	Type            string             `json:"type"`
	Success         *bool              `json:"success,omitempty"`
	Message         string             `json:"message,omitempty"`
	ProtocolVersion int                `json:"protocolVersion,omitempty"`
	PlayerID        string             `json:"playerId,omitempty"`
	World           *worldmap.Summary  `json:"world,omitempty"`
	Height          *float64           `json:"height,omitempty"`
	X               *float64           `json:"x,omitempty"`
	Y               *float64           `json:"y,omitempty"`
	Z               *float64           `json:"z,omitempty"`
	RotationX       *float64           `json:"rotationX,omitempty"`
	RotationY       *float64           `json:"rotationY,omitempty"`
	RotationZ       *float64           `json:"rotationZ,omitempty"`
	RotationW       *float64           `json:"rotationW,omitempty"`
	PositionIsRoot  bool               `json:"positionIsRoot,omitempty"`
	VisibilityRange *float64           `json:"visibilityRange,omitempty"`
	Target          *TargetReference    `json:"target,omitempty"`
	AttackSpeed     float64             `json:"attackSpeed,omitempty"`
	Damage          int                 `json:"damage,omitempty"`
	DamageMultiplier float64             `json:"damageMultiplier,omitempty"`
	AttackSide      string              `json:"attackSide,omitempty"`
	PlayerHealth    int                 `json:"playerHealth,omitempty"`
	PlayerMaxHealth int                 `json:"playerMaxHealth,omitempty"`
	RetryAfter      float64             `json:"retryAfter,omitempty"`
	RequestID       int                 `json:"requestId,omitempty"`
	Players         []NearbyPlayer      `json:"players,omitempty"`
	Monsters        []MonsterState      `json:"monsters,omitempty"`
	Monster         *MonsterState       `json:"monster,omitempty"`
	Items           []GroundItemState   `json:"items,omitempty"`
	Item            *GroundItemState    `json:"item,omitempty"`
	ItemID          string              `json:"itemId,omitempty"`
	ItemName        string              `json:"itemName,omitempty"`
	Inventory       []InventoryStack   `json:"inventory,omitempty"`
}

// ListenAndServe starts the TCP listener and exits when ctx is cancelled or the listener fails.
func (s *Server) ListenAndServe(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.address, err)
	}

	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
			s.closeConnections()
		case <-finished:
		}
	}()
	go s.runStatePersistence(ctx, finished)
	go s.runMonsterSimulation(ctx, finished)

	defer func() {
		close(finished)
		_ = listener.Close()
		s.closeConnections()
		s.handlers.Wait()
		if err := s.savePlayerStates(); err != nil {
			logging.Errorf(s.logger, "save player states to %q on shutdown: %v", s.stateFile, err)
		}
	}()

	s.logger.Printf("TCP server listening on %s", listener.Addr())
	if s.world == nil {
		s.logger.Printf("WARNING: world map is not loaded; authentication works, world queries will be unavailable")
	} else {
		info := s.world.Summary()
		s.logger.Printf(
			"world map loaded: scene=%q terrain=%q samples=%dx%d size=(%.3f, %.3f, %.3f) sha256=%s",
			info.SceneName, info.TerrainName, info.WidthSamples, info.HeightSamples,
			info.Size.X, info.Size.Y, info.Size.Z, info.SHA256,
		)
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			if networkError, ok := err.(net.Error); ok && networkError.Temporary() {
				logging.Errorf(s.logger, "temporary TCP accept error: %v", err)
				select {
				case <-time.After(100 * time.Millisecond):
					continue
				case <-ctx.Done():
					return nil
				}
			}
			return fmt.Errorf("accept TCP connection: %w", err)
		}

		if ctx.Err() != nil {
			_ = conn.Close()
			return nil
		}
		if !s.registerConnection(conn) {
			continue
		}

		go func(client net.Conn) {
			defer s.handlers.Done()
			s.handleConnection(client)
		}(conn)
	}
}

func (s *Server) registerConnection(conn net.Conn) bool {
	s.connectionsMu.Lock()
	defer s.connectionsMu.Unlock()

	if s.shuttingDown {
		_ = conn.Close()
		return false
	}
	s.connections[conn] = struct{}{}
	// Keep WaitGroup.Add protected by the same lock used by shutdown so Add cannot race Wait.
	s.handlers.Add(1)
	return true
}

func (s *Server) unregisterConnection(conn net.Conn) {
	s.connectionsMu.Lock()
	delete(s.connections, conn)
	s.connectionsMu.Unlock()
}

// PlayerStates returns a consistent snapshot, safe for future replication or interest management.
func (s *Server) PlayerStates() []PlayerState {
	s.playersMu.RLock()
	states := make([]PlayerState, 0, len(s.playerSessions))
	for playerID := range s.playerSessions {
		if state, ok := s.playerStates[playerID]; ok && state.Online {
			states = append(states, state)
		}
	}
	s.playersMu.RUnlock()

	sort.Slice(states, func(i, j int) bool {
		return states[i].PlayerID < states[j].PlayerID
	})
	return states
}

func (s *Server) registerPlayerSession(playerID string, conn net.Conn) bool {
	s.playersMu.Lock()
	defer s.playersMu.Unlock()

	if existing, ok := s.playerSessions[playerID]; ok && existing != conn {
		return false
	}
	s.playerSessions[playerID] = conn
	if state, ok := s.playerStates[playerID]; ok {
		state.Online = true
		s.playerStates[playerID] = state
	}
	return true
}

func (s *Server) removePlayerState(playerID string, conn net.Conn) {
	if playerID == "" {
		return
	}
	s.movementMu.Lock()
	s.playersMu.Lock()

	// An older connection must not alter a newer session or its current state.
	if current, ok := s.playerSessions[playerID]; !ok || current != conn {
		s.playersMu.Unlock()
		s.movementMu.Unlock()
		return
	}
	delete(s.playerSessions, playerID)
	delete(s.worldInterestRanges, playerID)
	delete(s.movementProgress, playerID)
	if state, ok := s.playerStates[playerID]; ok {
		state.Online = false
		s.playerStates[playerID] = state
	}

	// Release player locks before touching targetsMu: target validation reads player state.
	s.playersMu.Unlock()
	s.movementMu.Unlock()

	s.targetsMu.Lock()
	delete(s.playerTargets, playerID)
	delete(s.pendingPickupTargets, playerID)
	for ownerID, target := range s.playerTargets {
		if target.Type == "player" && target.ID == playerID {
			delete(s.playerTargets, ownerID)
			delete(s.pendingPickupTargets, ownerID)
		}
	}
	s.targetsMu.Unlock()

	s.combatStatsMu.Lock()
	delete(s.attackSpeeds, playerID)
	delete(s.lastAttackAt, playerID)
	s.combatStatsMu.Unlock()
}

func (s *Server) spawnResponseForPlayer(playerID string) response {
	// Reconciliation, overlap recovery and state persistence must be atomic with
	// other players' collision movement updates.
	s.movementMu.Lock()
	defer s.movementMu.Unlock()

	if state, ok := s.storedPlayerState(playerID); ok {
		state = s.alignSavedPlayerToCurrentTerrain(playerID, state)
		originalPosition := state.Position
		position, changed, wasOverlapping := s.recoverSpawnOverlap(playerID, state.Position)

		if s.playerPositionCollides(playerID, position) {
			s.logger.Printf(
				"ERROR: cannot restore player spawn without collision: player=%q position=(%.3f, %.3f, %.3f)",
				playerID, position.X, position.Y, position.Z)
			return response{
				Type:    "spawn",
				Success: boolPointer(false),
				Message: "no_free_spawn_position",
			}
		}

		if changed {
			state.Position = position
			state.UpdatedAt = time.Now().UTC()
			state.Online = true
			s.playersMu.Lock()
			s.playerStates[playerID] = state
			s.stateVersion++
			s.playersMu.Unlock()

			if wasOverlapping {
				s.logger.Printf(
					"player spawn collision recovered to nearest free position: player=%q old=(%.3f, %.3f, %.3f) new=(%.3f, %.3f, %.3f) distance=%.3f",
					playerID,
					originalPosition.X, originalPosition.Y, originalPosition.Z,
					position.X, position.Y, position.Z,
					math.Hypot(position.X-originalPosition.X, position.Z-originalPosition.Z))
			}
		}

		message := "restored_saved_state"
		if wasOverlapping {
			message = "restored_saved_state_collision_recovered"
		}
		return response{
			Type:           "spawn",
			Success:        boolPointer(true),
			Message:        message,
			X:              float64Pointer(position.X),
			Y:              float64Pointer(position.Y),
			Z:              float64Pointer(position.Z),
			RotationX:      float64Pointer(state.Rotation.X),
			RotationY:      float64Pointer(state.Rotation.Y),
			RotationZ:      float64Pointer(state.Rotation.Z),
			RotationW:      float64Pointer(state.Rotation.W),
			PositionIsRoot: true,
		}
	}

	// First-time accounts are normally sent a Terrain surface coordinate. If this
	// location is obstructed too, check a character-root position before sending spawn.
	spawn := s.surfaceSpawnPoint()
	rootPosition := spawn
	rootPosition.Y += 0.05
	identityRotation := PlayerRotation{W: 1}
	position, changed, wasOverlapping := s.recoverSpawnOverlap(playerID, rootPosition)
	if s.playerPositionCollides(playerID, position) {
		s.logger.Printf(
			"ERROR: no collision-free initial spawn for player=%q at (%.3f, %.3f, %.3f)",
			playerID, position.X, position.Y, position.Z)
		return response{
			Type:    "spawn",
			Success: boolPointer(false),
			Message: "no_free_spawn_position",
		}
	}

	if wasOverlapping && changed {
		// Record the recovered position before the client can send its first transform,
		// so subsequent sweeps start from the actual collision-free spawn.
		state := PlayerState{
			PlayerID:  playerID,
			Position:  position,
			Rotation:  identityRotation,
			Health:    DefaultPlayerMaxHealth,
			MaxHealth: DefaultPlayerMaxHealth,
			UpdatedAt: time.Now().UTC(),
			Online:    true,
		}
		s.playersMu.Lock()
		s.playerStates[playerID] = state
		s.stateVersion++
		s.playersMu.Unlock()

		s.logger.Printf(
			"initial spawn collision recovered to nearest free position: player=%q old=(%.3f, %.3f, %.3f) new=(%.3f, %.3f, %.3f)",
			playerID, rootPosition.X, rootPosition.Y, rootPosition.Z,
			position.X, position.Y, position.Z)
		return response{
			Type:           "spawn",
			Success:        boolPointer(true),
			Message:        "default_spawn_collision_recovered",
			X:              float64Pointer(position.X),
			Y:              float64Pointer(position.Y),
			Z:              float64Pointer(position.Z),
			RotationX:      float64Pointer(identityRotation.X),
			RotationY:      float64Pointer(identityRotation.Y),
			RotationZ:      float64Pointer(identityRotation.Z),
			RotationW:      float64Pointer(identityRotation.W),
			PositionIsRoot: true,
		}
	}

	return response{
		Type:      "spawn",
		Success:   boolPointer(true),
		Message:   "default_spawn",
		X:         float64Pointer(spawn.X),
		Y:         float64Pointer(spawn.Y),
		Z:         float64Pointer(spawn.Z),
		RotationX: float64Pointer(0),
		RotationY: float64Pointer(0),
		RotationZ: float64Pointer(0),
		RotationW: float64Pointer(1),
	}
}

// surfaceSpawnPoint derives the initial spawn height from the currently loaded heightmap.
// The hard-coded Y is used only when no compatible world map is available.
func (s *Server) surfaceSpawnPoint() worldmap.Vec3 {
	spawn := s.spawnPoint
	if s.world != nil {
		if terrainY, ok := s.world.HeightAtWorld(spawn.X, spawn.Z); ok {
			spawn.Y = terrainY
		}
	}
	return spawn
}

// alignSavedPlayerToCurrentTerrain migrates old saved ground positions after a heightmap
// update. This prototype has no jump/airborne player movement, so a large vertical mismatch
// at the saved X/Z is treated as stale terrain height and snapped to the current surface.
func (s *Server) alignSavedPlayerToCurrentTerrain(playerID string, state PlayerState) PlayerState {
	if s.world == nil {
		return state
	}
	terrainY, ok := s.world.HeightAtWorld(state.Position.X, state.Position.Z)
	if !ok {
		return state
	}

	expectedY := terrainY + 0.05
	if math.Abs(state.Position.Y-expectedY) <= 0.5 {
		return state
	}

	s.playersMu.Lock()
	current, exists := s.playerStates[playerID]
	if exists && current.UpdatedAt.Equal(state.UpdatedAt) {
		oldY := current.Position.Y
		current.Position.Y = expectedY
		current.UpdatedAt = time.Now().UTC()
		s.playerStates[playerID] = current
		s.stateVersion++
		state = current
		s.playersMu.Unlock()

		s.logger.Printf(
			"saved player height reconciled to current terrain: player=%q oldY=%.3f newY=%.3f",
			playerID, oldY, expectedY)
		return state
	}
	s.playersMu.Unlock()
	return state
}

func (s *Server) storedPlayerState(playerID string) (PlayerState, bool) {
	s.playersMu.RLock()
	state, ok := s.playerStates[playerID]
	s.playersMu.RUnlock()
	return state, ok
}

func (s *Server) logPlayerState(state PlayerState) {
	now := time.Now()
	s.stateLogMu.Lock()
	if !s.lastStateLog.IsZero() && now.Sub(s.lastStateLog) < playerStateLogInterval {
		s.stateLogMu.Unlock()
		return
	}
	s.lastStateLog = now
	s.stateLogMu.Unlock()

	s.logger.Printf(
		"TCP player state received: player=%q position=(%.2f, %.2f, %.2f) rotation=(%.4f, %.4f, %.4f, %.4f)",
		state.PlayerID,
		state.Position.X, state.Position.Y, state.Position.Z,
		state.Rotation.X, state.Rotation.Y, state.Rotation.Z, state.Rotation.W,
	)
}

func (s *Server) runStatePersistence(ctx context.Context, finished <-chan struct{}) {
	if s.stateFile == "" {
		return
	}
	ticker := time.NewTicker(s.stateSaveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-finished:
			return
		case <-ticker.C:
			if err := s.savePlayerStates(); err != nil {
				logging.Errorf(s.logger, "save player states to %q: %v", s.stateFile, err)
			}
		}
	}
}

func (s *Server) loadPlayerStates() error {
	if s.stateFile == "" {
		return nil
	}

	payload, err := os.ReadFile(s.stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	var file playerStateFile
	if err := json.Unmarshal(payload, &file); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	if file.Version != playerStateFileVersion {
		return fmt.Errorf("unsupported version %d (supported: %d)", file.Version, playerStateFileVersion)
	}

	loaded := make(map[string]PlayerState, len(file.Players))
	for playerID, state := range file.Players {
		if strings.TrimSpace(playerID) == "" {
			continue
		}
		state.PlayerID = playerID
		state.Online = false
		if !s.validStoredState(state) {
			s.logger.Printf("WARNING: ignoring invalid saved state for player=%q", playerID)
			continue
		}
		length := math.Hypot(
			math.Hypot(state.Rotation.X, state.Rotation.Y),
			math.Hypot(state.Rotation.Z, state.Rotation.W),
		)
		state.Rotation.X /= length
		state.Rotation.Y /= length
		state.Rotation.Z /= length
		state.Rotation.W /= length
		loaded[playerID] = normalizePlayerHealth(state)
	}

	s.playersMu.Lock()
	s.playerStates = loaded
	s.stateVersion = 0
	s.savedVersion = 0
	s.playersMu.Unlock()

	if len(loaded) > 0 {
		s.logger.Printf("loaded saved player states: count=%d file=%q", len(loaded), s.stateFile)
	}
	return nil
}

func (s *Server) validStoredState(state PlayerState) bool {
	position := state.Position
	rotation := state.Rotation
	if !finiteNumber(position.X) || !finiteNumber(position.Y) || !finiteNumber(position.Z) ||
		!finiteNumber(rotation.X) || !finiteNumber(rotation.Y) ||
		!finiteNumber(rotation.Z) || !finiteNumber(rotation.W) {
		return false
	}

	length := math.Hypot(math.Hypot(rotation.X, rotation.Y), math.Hypot(rotation.Z, rotation.W))
	if !finiteNumber(length) || length < 1e-9 {
		return false
	}
	state.Rotation.X /= length
	state.Rotation.Y /= length
	state.Rotation.Z /= length
	state.Rotation.W /= length

	if s.world != nil {
		if _, insideWorld := s.world.HeightAtWorld(position.X, position.Z); !insideWorld {
			return false
		}
		world := s.world.Summary()
		if position.Y < world.Origin.Y-5 || position.Y > world.Origin.Y+world.Size.Y+5 {
			return false
		}
	}
	return true
}

func (s *Server) savePlayerStates() error {
	if s.stateFile == "" {
		return nil
	}

	s.stateSaveMu.Lock()
	defer s.stateSaveMu.Unlock()

	s.playersMu.RLock()
	if s.stateVersion == s.savedVersion {
		s.playersMu.RUnlock()
		return nil
	}
	version := s.stateVersion
	states := make(map[string]PlayerState, len(s.playerStates))
	for playerID, state := range s.playerStates {
		state.Online = false
		states[playerID] = state
	}
	s.playersMu.RUnlock()

	payload, err := json.MarshalIndent(playerStateFile{
		Version: playerStateFileVersion,
		Players: states,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}

	directory := filepath.Dir(s.stateFile)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	temporaryPath := s.stateFile + ".tmp"
	file, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open temporary file: %w", err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, s.stateFile); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}

	s.playersMu.Lock()
	if s.stateVersion == version {
		s.savedVersion = version
	}
	s.playersMu.Unlock()
	return nil
}

func (s *Server) closeConnections() {
	s.connectionsMu.Lock()
	s.shuttingDown = true
	openConnections := make([]net.Conn, 0, len(s.connections))
	for conn := range s.connections {
		openConnections = append(openConnections, conn)
	}
	clear(s.connections)
	s.connectionsMu.Unlock()

	for _, conn := range openConnections {
		_ = conn.Close()
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	authenticated := false
	username := ""
	defer func() {
		_ = conn.Close()
		s.unregisterConnection(conn)
		if authenticated {
			s.removePlayerState(username, conn)
		}
	}()

	s.logger.Printf("TCP client connected: %s", conn.RemoteAddr())
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024), s.maxMessageBytes)

	authAttempts := 0

	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				if strings.Contains(err.Error(), "token too long") {
					_ = s.writeResponse(conn, response{
						Type:    "protocol_error",
						Success: boolPointer(false),
						Message: "message_too_large",
					})
				} else if !errors.Is(err, io.EOF) && !isClosedNetworkError(err) {
					logging.Errorf(s.logger, "TCP read error from %s: %v", conn.RemoteAddr(), err)
				}
			}
			return
		}

		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			if err := s.writeResponse(conn, response{
				Type:    "protocol_error",
				Success: boolPointer(false),
				Message: "invalid_json",
			}); err != nil {
				return
			}
			continue
		}
		if strings.TrimSpace(req.Type) == "" {
			if err := s.writeResponse(conn, response{
				Type:    "protocol_error",
				Success: boolPointer(false),
				Message: "missing_type",
			}); err != nil {
				return
			}
			continue
		}

		if !authenticated {
			if req.Type != "auth" {
				if err := s.writeResponse(conn, response{
					Type:    "error",
					Success: boolPointer(false),
					Message: "authentication_required",
				}); err != nil {
					return
				}
				continue
			}

			authAttempts++
			if !authenticate(req.Username, req.Password) {
				// Log the attempted username and outcome only. Never log the password.
				s.logger.Printf(
					"TCP authentication failed: username=%q remote=%s",
					req.Username,
					conn.RemoteAddr(),
				)
				if err := s.writeResponse(conn, response{
					Type:    "auth_result",
					Success: boolPointer(false),
					Message: "invalid_credentials",
				}); err != nil {
					return
				}
				if authAttempts >= maxAuthAttempts {
					return
				}
				continue
			}

			if !s.registerPlayerSession(req.Username, conn) {
				s.logger.Printf(
					"TCP authentication rejected: username=%q reason=already_logged_in remote=%s",
					req.Username, conn.RemoteAddr(),
				)
				_ = s.writeResponse(conn, response{
					Type:    "auth_result",
					Success: boolPointer(false),
					Message: "already_logged_in",
				})
				return
			}

			authenticated = true
			username = req.Username
			if err := s.writeResponse(conn, response{
				Type:            "auth_result",
				Success:         boolPointer(true),
				Message:         "authenticated",
				ProtocolVersion: ProtocolVersion,
				PlayerID:        username,
			}); err != nil {
				return
			}
			spawnResponse := s.spawnResponseForPlayer(username)
			if err := s.writeResponse(conn, spawnResponse); err != nil {
				return
			}

			s.logger.Printf("TCP authentication succeeded: username=%q remote=%s", username, conn.RemoteAddr())
			continue
		}

		if req.Type == "auth" {
			if err := s.writeResponse(conn, response{
				Type:    "auth_result",
				Success: boolPointer(false),
				Message: "already_authenticated",
			}); err != nil {
				return
			}
			continue
		}

		if err := s.handleAuthenticatedRequest(conn, username, req); err != nil {
			return
		}
	}
}

func (s *Server) handleAuthenticatedRequest(conn net.Conn, playerID string, req request) error {
	switch req.Type {
	case "ping":
		return s.writeResponse(conn, response{
			Type:    "pong",
			Success: boolPointer(true),
		})

	case "world_info":
		if s.world == nil {
			return s.writeResponse(conn, response{
				Type:    "world_info",
				Success: boolPointer(false),
				Message: "world_map_not_loaded",
			})
		}
		info := s.world.Summary()
		return s.writeResponse(conn, response{
			Type:    "world_info",
			Success: boolPointer(true),
			World:   &info,
		})

	case "player_state":
		return s.handlePlayerState(conn, playerID, req)

	case "world_interest":
		return s.handleWorldInterest(conn, playerID, req)

	case "set_target":
		return s.handleSetTarget(conn, playerID, req)

	case "attack":
		return s.handleAttack(conn, playerID, req)

	case "height_at":
		if req.X == nil || req.Z == nil {
			return s.writeResponse(conn, response{
				Type:    "height_result",
				Success: boolPointer(false),
				Message: "x_and_z_required",
			})
		}
		if s.world == nil {
			return s.writeResponse(conn, response{
				Type:    "height_result",
				Success: boolPointer(false),
				Message: "world_map_not_loaded",
			})
		}
		height, ok := s.world.HeightAtWorld(*req.X, *req.Z)
		if !ok {
			return s.writeResponse(conn, response{
				Type:    "height_result",
				Success: boolPointer(false),
				Message: "coordinates_outside_world",
			})
		}
		return s.writeResponse(conn, response{
			Type:    "height_result",
			Success: boolPointer(true),
			Height:  &height,
		})

	default:
		return s.writeResponse(conn, response{
			Type:    "error",
			Success: boolPointer(false),
			Message: "unknown_message_type",
		})
	}
}

func (s *Server) handleWorldInterest(conn net.Conn, playerID string, req request) error {
	state, ok := s.storedPlayerState(playerID)
	if !ok || !state.Online {
		return s.writeResponse(conn, response{
			Type:    "world_snapshot",
			Success: boolPointer(false),
			Message: "player_state_unavailable",
		})
	}

	radius := DefaultVisibilityRange
	if req.VisibilityRange != nil {
		radius = *req.VisibilityRange
	}
	radius = clampVisibilityRange(radius)
	s.playersMu.Lock()
	s.worldInterestRanges[playerID] = radius
	s.playersMu.Unlock()
	players, monsters, items := s.worldSnapshot(playerID, state.Position, radius)
	return s.writeResponse(conn, response{
		Type:            "world_snapshot",
		Success:         boolPointer(true),
		VisibilityRange: float64Pointer(radius),
		Target:           s.currentPlayerTarget(playerID),
		Players:          players,
		PlayerHealth:     state.Health,
		PlayerMaxHealth:  state.MaxHealth,
		Monsters:         monsters,
		Items:            items,
		Inventory:        s.inventorySnapshot(playerID),
		AttackSpeed:      s.PlayerAttackSpeed(playerID),
	})
}

// broadcastMonsterSpawned sends an immediate event to subscribed clients that could see
// this spawn point. World snapshots remain the correction channel for later state changes.
func (s *Server) broadcastMonsterSpawned(monster MonsterState, at time.Time) {
	if !monster.Alive {
		return
	}
	payload := s.monsterSnapshotState(monster, at)
	payload.Collider = colliderForMonster(payload)

	type recipient struct {
		conn net.Conn
	}
	recipients := make([]recipient, 0)
	s.playersMu.RLock()
	for playerID, conn := range s.playerSessions {
		state, hasState := s.playerStates[playerID]
		radius, hasInterest := s.worldInterestRanges[playerID]
		if conn == nil || !hasState || !state.Online || !hasInterest ||
			!positionWithinRange(state.Position, monster.Position, radius) {
			continue
		}
		recipients = append(recipients, recipient{conn: conn})
	}
	s.playersMu.RUnlock()

	for _, recipient := range recipients {
		_ = s.writeResponse(recipient.conn, response{
			Type:    "monster_spawned",
			Success: boolPointer(true),
			Monster: &payload,
		})
	}
}

// broadcastMonsterStateUpdated pushes motion discontinuities that clients cannot predict,
// such as a monster landing or being moved aside to avoid an active player.
func (s *Server) broadcastMonsterStateUpdated(monster MonsterState, at time.Time) {
	if !monster.Alive {
		return
	}
	payload := s.monsterSnapshotState(monster, at)
	payload.Collider = colliderForMonster(payload)

	type recipient struct {
		conn net.Conn
	}
	recipients := make([]recipient, 0)
	s.playersMu.RLock()
	for playerID, conn := range s.playerSessions {
		state, hasState := s.playerStates[playerID]
		radius, hasInterest := s.worldInterestRanges[playerID]
		if conn == nil || !hasState || !state.Online || !hasInterest ||
			!positionWithinRange(state.Position, monster.Position, radius) {
			continue
		}
		recipients = append(recipients, recipient{conn: conn})
	}
	s.playersMu.RUnlock()

	for _, recipient := range recipients {
		_ = s.writeResponse(recipient.conn, response{
			Type:    "monster_state",
			Success: boolPointer(true),
			Monster: &payload,
		})
	}
}

// broadcastGroundItemSpawned immediately reveals a newly created ground drop to
// connected clients whose current world-interest radius contains the landing point.
// Regular world snapshots still reconcile removals and any missed events.
func (s *Server) broadcastGroundItemSpawned(item GroundItemState) {
	type recipient struct {
		conn net.Conn
	}
	recipients := make([]recipient, 0)
	s.playersMu.RLock()
	for playerID, conn := range s.playerSessions {
		state, hasState := s.playerStates[playerID]
		radius, hasInterest := s.worldInterestRanges[playerID]
		if conn == nil || !hasState || !state.Online || !hasInterest ||
			!positionWithinRange(state.Position, item.Position, radius) {
			continue
		}
		recipients = append(recipients, recipient{conn: conn})
	}
	s.playersMu.RUnlock()

	for _, recipient := range recipients {
		if err := s.writeResponse(recipient.conn, response{
			Type:    "ground_item_spawned",
			Success: boolPointer(true),
			Item:    &item,
		}); err != nil {
			logging.Errorf(s.logger, "cannot broadcast ground item %q: %v", item.ID, err)
		}
	}
}

func (s *Server) handleSetTarget(conn net.Conn, playerID string, req request) error {
	target, success, message := s.setPlayerTarget(playerID, req.TargetType, req.TargetID)
	if err := s.writeResponse(conn, response{
		Type:     "target_result",
		Success:  boolPointer(success),
		Message:  message,
		PlayerID: playerID,
		Target:   target,
	}); err != nil {
		return err
	}

	// A target request can arrive after the player has already reached the item.
	// Check once here as well as after every accepted movement update.
	if success && strings.EqualFold(strings.TrimSpace(req.TargetType), "item") {
		if state, exists := s.storedPlayerState(playerID); exists {
			if item, inventory, pickedUp := s.completePendingPickup(playerID, state.Position); pickedUp {
				s.broadcastItemPickedUp(playerID, item, inventory)
			}
		}
	}
	return nil
}

// setPlayerTarget validates the requested reference and atomically replaces this player's target.
// An empty type and ID explicitly clear the target.
func (s *Server) setPlayerTarget(playerID, targetType, targetID string) (*TargetReference, bool, string) {
	targetType = strings.ToLower(strings.TrimSpace(targetType))
	targetID = strings.TrimSpace(targetID)

	s.targetsMu.Lock()
	defer s.targetsMu.Unlock()

	if targetType == "" && targetID == "" {
		delete(s.playerTargets, playerID)
		delete(s.pendingPickupTargets, playerID)
		return nil, true, "target_cleared"
	}
	if targetType == "" || targetID == "" {
		delete(s.playerTargets, playerID)
		delete(s.pendingPickupTargets, playerID)
		return nil, false, "invalid_target_reference"
	}

	target, exists := s.resolveTargetLocked(playerID, targetType, targetID)
	if !exists {
		delete(s.playerTargets, playerID)
		delete(s.pendingPickupTargets, playerID)
		return nil, false, "target_not_found"
	}

	s.playerTargets[playerID] = target
	if target.Type == "item" {
		s.pendingPickupTargets[playerID] = target.ID
	} else {
		delete(s.pendingPickupTargets, playerID)
	}
	return &target, true, "target_set"
}

// currentPlayerTarget revalidates references whenever a snapshot is sent, so a stale target
// cannot survive an entity removal or death even if an alternate code path forgot cleanup.
func (s *Server) currentPlayerTarget(playerID string) *TargetReference {
	s.targetsMu.Lock()
	defer s.targetsMu.Unlock()

	current, exists := s.playerTargets[playerID]
	if !exists {
		return nil
	}
	target, valid := s.resolveTargetLocked(playerID, current.Type, current.ID)
	if !valid {
		delete(s.playerTargets, playerID)
		return nil
	}
	s.playerTargets[playerID] = target
	return &target
}

// resolveTargetLocked is called with targetsMu held. The returned reference is based only on
// server-owned live registries; clients cannot supply a name or position for their target.
func (s *Server) resolveTargetLocked(playerID, targetType, targetID string) (TargetReference, bool) {
	switch targetType {
	case "monster":
		s.entitiesMu.RLock()
		monster, exists := s.monsters[targetID]
		s.entitiesMu.RUnlock()
		if !exists || !monster.Alive {
			return TargetReference{}, false
		}
		return TargetReference{Type: "monster", ID: monster.ID, Name: monster.Name}, true

	case "player":
		if targetID == playerID {
			return TargetReference{}, false
		}
		s.playersMu.RLock()
		state, exists := s.playerStates[targetID]
		_, connected := s.playerSessions[targetID]
		s.playersMu.RUnlock()
		if !exists || !connected || !state.Online {
			return TargetReference{}, false
		}
		return TargetReference{Type: "player", ID: state.PlayerID, Name: state.PlayerID}, true

	case "item":
		s.entitiesMu.RLock()
		item, exists := s.groundItems[targetID]
		s.entitiesMu.RUnlock()
		if !exists {
			return TargetReference{}, false
		}
		return TargetReference{Type: "item", ID: item.ID, Name: item.Name}, true

	default:
		return TargetReference{}, false
	}
}

// clearTargetReferences drops every player's selection of a removed/dead world entity.
func (s *Server) clearTargetReferences(targetType, targetID string) {
	s.targetsMu.Lock()
	for ownerID, target := range s.playerTargets {
		if target.Type == targetType && target.ID == targetID {
			delete(s.playerTargets, ownerID)
			delete(s.pendingPickupTargets, ownerID)
		}
	}
	for ownerID, pendingID := range s.pendingPickupTargets {
		if targetType == "item" && pendingID == targetID {
			delete(s.pendingPickupTargets, ownerID)
		}
	}
	s.targetsMu.Unlock()
}

// completePendingPickup transfers a clicked ground item only when the server-accepted
// player position is inside the configured pickup radius. Both the target intent and
// ground-item registry are locked so two players cannot take the same item.
func (s *Server) completePendingPickup(playerID string, position worldmap.Vec3) (GroundItemState, []InventoryStack, bool) {
	s.targetsMu.Lock()
	defer s.targetsMu.Unlock()

	itemID, hasIntent := s.pendingPickupTargets[playerID]
	if !hasIntent {
		return GroundItemState{}, nil, false
	}

	s.entitiesMu.Lock()
	item, exists := s.groundItems[itemID]
	if !exists {
		s.entitiesMu.Unlock()
		delete(s.pendingPickupTargets, playerID)
		if target, hasTarget := s.playerTargets[playerID]; hasTarget && target.Type == "item" && target.ID == itemID {
			delete(s.playerTargets, playerID)
		}
		return GroundItemState{}, nil, false
	}

	// PlayerState.Position is the character-controller root (about one metre above
	// the ground), while GroundItemState.Position is the item's ground position.
	// Pickup distance is therefore measured horizontally on the ground plane.
	distance := math.Hypot(
		position.X-item.Position.X,
		position.Z-item.Position.Z,
	)
	if distance > ItemPickupRadius {
		s.entitiesMu.Unlock()
		return GroundItemState{}, nil, false
	}

	delete(s.groundItems, itemID)
	s.entitiesMu.Unlock()

	for ownerID, target := range s.playerTargets {
		if target.Type == "item" && target.ID == itemID {
			delete(s.playerTargets, ownerID)
			delete(s.pendingPickupTargets, ownerID)
		}
	}
	for ownerID, pendingID := range s.pendingPickupTargets {
		if pendingID == itemID {
			delete(s.pendingPickupTargets, ownerID)
		}
	}

	inventory := s.addItemToInventory(playerID, item)
	return item, inventory, true
}

// addItemToInventory stores an item instance in the player's prototype inventory.
func (s *Server) addItemToInventory(playerID string, item GroundItemState) []InventoryStack {
	definition := ResolveItemDefinition(item.ItemTemplate, item.Name)
	quantity := item.Quantity
	if quantity < 1 {
		quantity = 1
	}

	s.inventoryMu.Lock()
	inventory := append([]InventoryStack(nil), s.playerInventories[playerID]...)
	newStack := InventoryStack{
		ItemTemplate: definition.TemplateID,
		Name:         definition.Name,
		Category:     definition.Category,
		Stackable:    definition.Stackable,
		Quantity:     quantity,
	}

	if definition.Stackable {
		merged := false
		for index := range inventory {
			if inventory[index].ItemTemplate == definition.TemplateID {
				inventory[index].Quantity += quantity
				merged = true
				break
			}
		}
		if !merged {
			inventory = append(inventory, newStack)
		}
	} else {
		for count := 0; count < quantity; count++ {
			unit := newStack
			unit.Quantity = 1
			inventory = append(inventory, unit)
		}
	}

	s.playerInventories[playerID] = inventory
	result := append([]InventoryStack(nil), inventory...)
	s.inventoryMu.Unlock()
	return result
}

// inventorySnapshot returns a defensive copy so network encoding never shares mutable state.
func (s *Server) inventorySnapshot(playerID string) []InventoryStack {
	s.inventoryMu.RLock()
	result := append([]InventoryStack(nil), s.playerInventories[playerID]...)
	s.inventoryMu.RUnlock()
	return result
}

// broadcastItemPickedUp removes the item from nearby clients and reveals inventory only to its owner.
func (s *Server) broadcastItemPickedUp(playerID string, item GroundItemState, inventory []InventoryStack) {
	type recipient struct {
		playerID string
		conn     net.Conn
	}

	recipients := make([]recipient, 0)
	s.playersMu.RLock()
	for recipientID, conn := range s.playerSessions {
		state, hasState := s.playerStates[recipientID]
		if conn == nil || !hasState || !state.Online {
			continue
		}
		if recipientID != playerID {
			radius, hasInterest := s.worldInterestRanges[recipientID]
			if !hasInterest || !positionWithinRange(state.Position, item.Position, radius) {
				continue
			}
		}
		recipients = append(recipients, recipient{playerID: recipientID, conn: conn})
	}
	s.playersMu.RUnlock()

	for _, recipient := range recipients {
		message := response{
			Type:     "item_picked_up",
			Success:  boolPointer(true),
			PlayerID: playerID,
			ItemID:   item.ID,
			ItemName: item.Name,
		}
		if recipient.playerID == playerID {
			message.Inventory = append([]InventoryStack(nil), inventory...)
		}
		if err := s.writeResponse(recipient.conn, message); err != nil {
			s.logger.Printf("WARNING: cannot publish picked-up item %q to player %q: %v",
				item.ID, recipient.playerID, err)
		}
	}
}

func (s *Server) handlePlayerState(conn net.Conn, playerID string, req request) error {
	if req.X == nil || req.Y == nil || req.Z == nil ||
		req.RotationX == nil || req.RotationY == nil ||
		req.RotationZ == nil || req.RotationW == nil {
		return s.writeResponse(conn, response{
			Type:    "player_state_result",
			Success: boolPointer(false),
			Message: "position_and_rotation_required",
		})
	}

	x, y, z := *req.X, *req.Y, *req.Z
	qx, qy, qz, qw := *req.RotationX, *req.RotationY, *req.RotationZ, *req.RotationW
	if !finiteNumber(x) || !finiteNumber(y) || !finiteNumber(z) ||
		!finiteNumber(qx) || !finiteNumber(qy) || !finiteNumber(qz) || !finiteNumber(qw) {
		return s.writeResponse(conn, response{
			Type:    "player_state_result",
			Success: boolPointer(false),
			Message: "non_finite_transform",
		})
	}

	quaternionLength := math.Hypot(math.Hypot(qx, qy), math.Hypot(qz, qw))
	if !finiteNumber(quaternionLength) || quaternionLength < 1e-9 {
		return s.writeResponse(conn, response{
			Type:    "player_state_result",
			Success: boolPointer(false),
			Message: "invalid_rotation",
		})
	}

	if s.world != nil {
		if _, insideWorld := s.world.HeightAtWorld(x, z); !insideWorld {
			return s.writeResponse(conn, response{
				Type:    "player_state_result",
				Success: boolPointer(false),
				Message: "coordinates_outside_world",
			})
		}
		world := s.world.Summary()
		if y < world.Origin.Y-5 || y > world.Origin.Y+world.Size.Y+5 {
			return s.writeResponse(conn, response{
				Type:    "player_state_result",
				Success: boolPointer(false),
				Message: "height_outside_world",
			})
		}
	}

	requestedPosition := worldmap.Vec3{X: x, Y: y, Z: z}

	// Resolve and commit the next accepted transform atomically relative to other
	// clients' movement requests. The mutex is released before writing any response.
	s.movementMu.Lock()
	acceptedPosition := requestedPosition
	movementFrom := requestedPosition
	previousState, alreadyKnown := s.storedPlayerState(playerID)
	if alreadyKnown {
		// The previous server-accepted transform is the start of the swept move.
		// Never resolve from an untrusted client-provided "previous position".
		movementFrom = previousState.Position
		acceptedPosition = s.resolvePlayerMovement(playerID, previousState.Position, requestedPosition)
	} else if s.world != nil {
		// On the first transform from a brand-new account, use the spawn we sent
		// during authentication as the starting point. This closes the first-packet
		// gap where a client could cross an obstacle before the server had a prior state.
		spawnStart := s.surfaceSpawnPoint()
		if _, insideWorld := s.world.HeightAtWorld(spawnStart.X, spawnStart.Z); insideWorld {
			spawnStart.Y += 0.05 // matches the client's CharacterController foot offset
			movementFrom = spawnStart
			acceptedPosition = s.resolvePlayerMovement(playerID, spawnStart, requestedPosition)
		}
	}

	retryExhausted := s.updateMovementProgress(playerID, movementFrom, acceptedPosition, req, time.Now())

	state := PlayerState{
		PlayerID: playerID,
		Position: acceptedPosition,
		Rotation: PlayerRotation{
			X: qx / quaternionLength,
			Y: qy / quaternionLength,
			Z: qz / quaternionLength,
			W: qw / quaternionLength,
		},
		Health:    DefaultPlayerMaxHealth,
		MaxHealth: DefaultPlayerMaxHealth,
		UpdatedAt: time.Now().UTC(),
		Online:    true,
	}

	// Movement updates must never reset server-owned health from client input.
	if previous, exists := s.storedPlayerState(playerID); exists {
		state.Health = previous.Health
		state.MaxHealth = previous.MaxHealth
	}


	s.playersMu.Lock()
	s.playerStates[playerID] = state
	s.stateVersion++
	s.playersMu.Unlock()
	s.movementMu.Unlock()

	if item, inventory, pickedUp := s.completePendingPickup(playerID, acceptedPosition); pickedUp {
		s.broadcastItemPickedUp(playerID, item, inventory)
	}

	s.logPlayerState(state)

	// Local Unity physics and the server's collision solver are not bit-identical.
	// Ignore centimetre-scale differences so tiny discrepancies do not cause camera and shadow jitter.
	horizontalCorrectionDistance := math.Hypot(
		acceptedPosition.X-requestedPosition.X,
		acceptedPosition.Z-requestedPosition.Z)
	horizontalPositionCorrected := horizontalCorrectionDistance > 0.06
	verticalPositionCorrected := math.Abs(acceptedPosition.Y-requestedPosition.Y) > 0.08

	if retryExhausted {
		s.logger.Printf("movement retry timeout: player=%q target=(%.2f, %.2f)", playerID, req.MoveTargetX, req.MoveTargetZ)
		return s.writeResponse(conn, response{
			Type:      "player_state_result",
			Success:   boolPointer(true),
			Message:   "movement_retry_exhausted",
			PlayerID:  playerID,
			X:         float64Pointer(acceptedPosition.X),
			Y:         float64Pointer(acceptedPosition.Y),
			Z:         float64Pointer(acceptedPosition.Z),
			RotationX: float64Pointer(state.Rotation.X),
			RotationY: float64Pointer(state.Rotation.Y),
			RotationZ: float64Pointer(state.Rotation.Z),
			RotationW: float64Pointer(state.Rotation.W),
		})
	}

	// Horizontal corrections keep the move command active; the client continues
	// trying/sliding around the obstacle. Only the retry timeout stops an impossible
	// or non-progressing move. Y-only differences are ground reconciliation.
	if horizontalPositionCorrected {
		return s.writeResponse(conn, response{
			Type:       "player_state_result",
			Success:    boolPointer(true),
			Message:    "server_collision_correction",
			PlayerID:   playerID,
			X:          float64Pointer(acceptedPosition.X),
			Y:          float64Pointer(acceptedPosition.Y),
			Z:          float64Pointer(acceptedPosition.Z),
			RotationX:  float64Pointer(state.Rotation.X),
			RotationY:  float64Pointer(state.Rotation.Y),
			RotationZ:  float64Pointer(state.Rotation.Z),
			RotationW:  float64Pointer(state.Rotation.W),
		})
	}
	if verticalPositionCorrected {
		return s.writeResponse(conn, response{
			Type:     "player_state_result",
			Success:  boolPointer(true),
			Message:  "server_ground_correction",
			PlayerID: playerID,
			Y:        float64Pointer(acceptedPosition.Y),
		})
	}

	// Acknowledge only the first transform from a player without an existing state.
	// Corrected transforms are also returned immediately when a collision blocks movement.
	if !alreadyKnown {
		return s.writeResponse(conn, response{
			Type:     "player_state_result",
			Success:  boolPointer(true),
			Message:  "state_registered",
			PlayerID: playerID,
		})
	}
	return nil
}

// updateMovementProgress tracks progress toward a client-requested destination. It is
// called under movementMu, so the target timer is serialized with collision movement.
// A small bounded increase in target distance is allowed while sliding around a nearby
// obstacle; if no useful progress resumes before the configured timeout, the client is told to stop.
func (s *Server) updateMovementProgress(
	playerID string,
	from, accepted worldmap.Vec3,
	req request,
	now time.Time,
) bool {
	if !req.HasMoveTarget || !finiteNumber(req.MoveTargetX) || !finiteNumber(req.MoveTargetZ) {
		delete(s.movementProgress, playerID)
		return false
	}

	targetDistance := math.Hypot(accepted.X-req.MoveTargetX, accepted.Z-req.MoveTargetZ)
	if targetDistance <= movementTargetArrivalDistance {
		delete(s.movementProgress, playerID)
		return false
	}

	progress, exists := s.movementProgress[playerID]
	targetChanged := exists && math.Hypot(
		progress.targetX-req.MoveTargetX,
		progress.targetZ-req.MoveTargetZ,
	) > 0.25
	if !exists || targetChanged {
		progress = movementProgressState{
			targetX: req.MoveTargetX,
			targetZ: req.MoveTargetZ,
			bestDistance: math.Hypot(from.X-req.MoveTargetX, from.Z-req.MoveTargetZ),
			lastProgressAt: now,
		}
		s.movementProgress[playerID] = progress
		return false
	}

	actualTravel := math.Hypot(accepted.X-from.X, accepted.Z-from.Z)
	if targetDistance < progress.bestDistance-movementMinProgressDistance {
		progress.bestDistance = targetDistance
		progress.lastProgressAt = now
	} else if actualTravel > movementMinProgressDistance &&
		targetDistance <= progress.bestDistance+movementMaxDetourDistance {
		// Allow the player to make sideways progress while the local controller slides
		// around an enemy. The allowance is bounded so a long wall does not keep a move alive forever.
		progress.lastProgressAt = now
	}
	s.movementProgress[playerID] = progress

	if now.Sub(progress.lastProgressAt) >= s.movementRetryTimeout {
		delete(s.movementProgress, playerID)
		return true
	}
	return false
}

func finiteNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (s *Server) writeResponse(conn net.Conn, value response) error {
	writeLock := s.acquireConnectionWriteLock(conn)
	defer s.releaseConnectionWriteLock(conn, writeLock)

	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(value); err != nil {
		if !isClosedNetworkError(err) {
			logging.Errorf(s.logger, "TCP write error to %s: %v", conn.RemoteAddr(), err)
		}
		return err
	}
	return nil
}

func (s *Server) acquireConnectionWriteLock(conn net.Conn) *connectionWriteLock {
	s.writeLocksMu.Lock()
	writeLock := s.writeLocks[conn]
	if writeLock == nil {
		writeLock = &connectionWriteLock{}
		s.writeLocks[conn] = writeLock
	}
	writeLock.references++
	s.writeLocksMu.Unlock()

	writeLock.mutex.Lock()
	return writeLock
}

func (s *Server) releaseConnectionWriteLock(conn net.Conn, writeLock *connectionWriteLock) {
	writeLock.mutex.Unlock()

	s.writeLocksMu.Lock()
	writeLock.references--
	if writeLock.references == 0 && s.writeLocks[conn] == writeLock {
		delete(s.writeLocks, conn)
	}
	s.writeLocksMu.Unlock()
}

func authenticate(username, password string) bool {
	expected, userExists := developmentCredentials[username]
	if !userExists {
		expected = "invalid-user-placeholder"
	}
	passwordMatches := subtle.ConstantTimeCompare([]byte(expected), []byte(password)) == 1
	return userExists && passwordMatches
}

func boolPointer(value bool) *bool {
	return &value
}

func float64Pointer(value float64) *float64 {
	return &value
}

func isClosedNetworkError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed) ||
		strings.Contains(strings.ToLower(err.Error()), "broken pipe") ||
		strings.Contains(strings.ToLower(err.Error()), "connection reset by peer")
}

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
	ProtocolVersion = 1
	defaultAddress = ":7777"
	defaultMaxLine = 4096
	maxAuthAttempts = 5
	readTimeout    = 10 * time.Minute
	writeTimeout   = 10 * time.Second
	defaultStateSaveInterval = time.Second
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

	playersMu      sync.RWMutex
	playerStates   map[string]PlayerState
	playerSessions map[string]net.Conn
	stateVersion   uint64
	savedVersion   uint64

	stateSaveMu sync.Mutex
	stateLogMu  sync.Mutex
	lastStateLog time.Time

	connectionsMu sync.Mutex
	connections   map[net.Conn]struct{}
	shuttingDown  bool
	handlers      sync.WaitGroup
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

	server := &Server{
		address:           address,
		world:             config.World,
		logger:            logger,
		maxMessageBytes:   maxMessageBytes,
		spawnPoint:        spawnPoint,
		stateFile:         strings.TrimSpace(config.StateFile),
		stateSaveInterval: stateSaveInterval,
		playerStates:      make(map[string]PlayerState),
		playerSessions:    make(map[string]net.Conn),
		connections:       make(map[net.Conn]struct{}),
	}
	if err := server.loadPlayerStates(); err != nil {
		logger.Printf("WARNING: cannot load player states from %q: %v; using fresh spawns", server.stateFile, err)
	}
	return server
}

type request struct {
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
	UpdatedAt time.Time      `json:"updatedAt"`
	Online    bool           `json:"-"`
}

type playerStateFile struct {
	Version int                    `json:"version"`
	Players map[string]PlayerState `json:"players"`
}

type response struct {
	Type            string            `json:"type"`
	Success         *bool             `json:"success,omitempty"`
	Message         string            `json:"message,omitempty"`
	ProtocolVersion int               `json:"protocolVersion,omitempty"`
	PlayerID        string            `json:"playerId,omitempty"`
	World           *worldmap.Summary `json:"world,omitempty"`
	Height          *float64          `json:"height,omitempty"`
	X               *float64          `json:"x,omitempty"`
	Y               *float64          `json:"y,omitempty"`
	Z               *float64          `json:"z,omitempty"`
	RotationX       *float64          `json:"rotationX,omitempty"`
	RotationY       *float64          `json:"rotationY,omitempty"`
	RotationZ       *float64          `json:"rotationZ,omitempty"`
	RotationW       *float64          `json:"rotationW,omitempty"`
	PositionIsRoot  bool              `json:"positionIsRoot,omitempty"`
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
	s.playersMu.Lock()
	defer s.playersMu.Unlock()

	// An older connection must not alter a newer session or its current state.
	if current, ok := s.playerSessions[playerID]; !ok || current != conn {
		return
	}
	delete(s.playerSessions, playerID)
	if state, ok := s.playerStates[playerID]; ok {
		state.Online = false
		s.playerStates[playerID] = state
	}
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

	state := PlayerState{
		PlayerID: playerID,
		Position: worldmap.Vec3{X: x, Y: y, Z: z},
		Rotation: PlayerRotation{
			X: qx / quaternionLength,
			Y: qy / quaternionLength,
			Z: qz / quaternionLength,
			W: qw / quaternionLength,
		},
		UpdatedAt: time.Now().UTC(),
		Online:    true,
	}

	s.playersMu.Lock()
	_, alreadyKnown := s.playerStates[playerID]
	s.playerStates[playerID] = state
	s.stateVersion++
	s.playersMu.Unlock()

	s.logPlayerState(state)

	// Acknowledge only the first transform from a player without an existing state.
	// A reply for every tick would double steady-state network traffic.
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

func finiteNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (s *Server) writeResponse(conn net.Conn, value response) error {
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

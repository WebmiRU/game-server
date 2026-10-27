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
	"net"
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
)

var developmentCredentials = map[string]string{
	"player1": "player1",
	"player2": "player2",
	"player3": "player3",
}

// Config configures the TCP game server.
type Config struct {
	Address         string
	World           *worldmap.Map
	Logger          *log.Logger
	MaxMessageBytes int
}

// Server accepts TCP clients, authenticates them, and exposes read-only world queries.
type Server struct {
	address         string
	world           *worldmap.Map
	logger          *log.Logger
	maxMessageBytes int

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

	return &Server{
		address:         address,
		world:           config.World,
		logger:          logger,
		maxMessageBytes: maxMessageBytes,
		connections:     make(map[net.Conn]struct{}),
	}
}

type request struct {
	Type     string   `json:"type"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	X        *float64 `json:"x,omitempty"`
	Z        *float64 `json:"z,omitempty"`
}

type response struct {
	Type            string            `json:"type"`
	Success         *bool             `json:"success,omitempty"`
	Message         string            `json:"message,omitempty"`
	ProtocolVersion int               `json:"protocolVersion,omitempty"`
	PlayerID        string            `json:"playerId,omitempty"`
	World           *worldmap.Summary `json:"world,omitempty"`
	Height          *float64          `json:"height,omitempty"`
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
	defer func() {
		close(finished)
		_ = listener.Close()
		s.closeConnections()
		s.handlers.Wait()
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
	defer func() {
		_ = conn.Close()
		s.unregisterConnection(conn)
	}()

	s.logger.Printf("TCP client connected: %s", conn.RemoteAddr())
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024), s.maxMessageBytes)

	authenticated := false
	username := ""
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

		if err := s.handleAuthenticatedRequest(conn, req); err != nil {
			return
		}
	}
}

func (s *Server) handleAuthenticatedRequest(conn net.Conn, req request) error {
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

func isClosedNetworkError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed) ||
		strings.Contains(strings.ToLower(err.Error()), "broken pipe") ||
		strings.Contains(strings.ToLower(err.Error()), "connection reset by peer")
}

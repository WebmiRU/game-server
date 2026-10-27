package tcpserver

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"game-server/internal/worldmap"
)

const (
	// Visibility ranges are client preferences, but the server always enforces these limits.
	MinVisibilityRange     = 50.0
	DefaultVisibilityRange = 100.0
	MaxVisibilityRange     = 500.0
	monsterGravity         = 9.81
	monsterPhysicsTick     = 50 * time.Millisecond
)

// NearbyPlayer is the compact representation sent to clients for another active player.
type NearbyPlayer struct {
	PlayerID string              `json:"playerId"`
	Position worldmap.Vec3       `json:"position"`
	Rotation PlayerRotation      `json:"rotation"`
}

// MonsterState is a server-owned monster entry. AIState is "idle" for the current
// stationary test monsters; movement/combat behavior can be added without changing
// the world-interest protocol.
type MonsterState struct {
	ID         string            `json:"id"`
	TemplateID string            `json:"templateId"`
	Name       string            `json:"name"`
	Position   worldmap.Vec3     `json:"position"`
	Rotation   PlayerRotation    `json:"rotation"`
	Health     int               `json:"health"`
	MaxHealth  int               `json:"maxHealth"`
	Alive      bool              `json:"alive"`
	Action     string            `json:"action"`
	AIState    string            `json:"aiState"`

	// Physics bookkeeping stays server-side; clients receive only Position and Action.
	groundY          float64
	verticalVelocity float64
	falling          bool
}

// GroundItemState is a server-owned item lying in the world.
type GroundItemState struct {
	ID           string        `json:"id"`
	ItemTemplate string        `json:"itemTemplate"`
	Name         string        `json:"name"`
	Position     worldmap.Vec3 `json:"position"`
	Quantity     int           `json:"quantity"`
}

// clampVisibilityRange applies server-side limits even when a client sends a bad value.
func clampVisibilityRange(value float64) float64 {
	if !finiteNumber(value) {
		return DefaultVisibilityRange
	}
	return math.Max(MinVisibilityRange, math.Min(MaxVisibilityRange, value))
}

func (s *Server) seedTestMonsters() {
	if s.world == nil {
		return
	}

	// These coordinates are around the pit called out in the Unity click-to-move log.
	// The authoritative ground height is sampled from the server's world map. Test
	// monsters spawn five units above that surface and then fall under server gravity.
	seeds := []struct {
		id, name, templateID string
		x, z                 float64
	}{
		{id: "1", name: "Enemy 1", templateID: "test-cube-enemy", x: 392.0, z: 478.0},
		{id: "2", name: "Enemy 2", templateID: "test-sphere-enemy", x: 401.0, z: 482.0},
	}

	for _, seed := range seeds {
		y, ok := s.world.HeightAtWorld(seed.x, seed.z)
		if !ok {
			s.logger.Printf(
				"WARNING: skipping test monster %q because its coordinates are outside the loaded world",
				seed.id,
			)
			continue
		}

		monster := MonsterState{
			ID:         seed.id,
			TemplateID: seed.templateID,
			Name:       seed.name,
			Position:   worldmap.Vec3{X: seed.x, Y: y + 5, Z: seed.z},
			// Identity quaternion is explicit: the smiley face's local +Z direction
			// is its fixed forward direction until a different rotation is configured.
			Rotation:          PlayerRotation{X: 0, Y: 0, Z: 0, W: 1},
			Health:            100,
			MaxHealth:         100,
			Alive:             true,
			Action:            "falling",
			AIState:           "falling",
			groundY:           y,
			verticalVelocity:  0,
			falling:           true,
		}
		s.monsters[monster.ID] = monster
	}

	s.logger.Printf("test monsters initialized: count=%d", len(s.monsters))
}

// UpsertMonster adds a monster to the server registry or replaces its current state.
// It returns false when the entry lacks an ID or an invalid position.
func (s *Server) UpsertMonster(monster MonsterState) bool {
	monster.ID = strings.TrimSpace(monster.ID)
	if monster.ID == "" || !s.validEntityPosition(monster.Position) {
		return false
	}
	if monster.TemplateID == "" {
		monster.TemplateID = "test-monster"
	}
	if monster.MaxHealth < 0 || monster.Health < 0 {
		return false
	}
	if monster.MaxHealth > 0 && monster.Health > monster.MaxHealth {
		monster.Health = monster.MaxHealth
	}
	if strings.TrimSpace(monster.Action) == "" {
		monster.Action = "idle"
	}
	if strings.TrimSpace(monster.AIState) == "" {
		monster.AIState = "idle"
	}

	monster.groundY = monster.Position.Y
	if s.world != nil {
		if terrainHeight, ok := s.world.HeightAtWorld(monster.Position.X, monster.Position.Z); ok {
			monster.groundY = terrainHeight
		}
	}
	monster.falling = strings.EqualFold(monster.Action, "falling")
	monster.verticalVelocity = 0

	s.entitiesMu.Lock()
	s.monsters[monster.ID] = monster
	s.entitiesMu.Unlock()
	return true
}

// RemoveMonster removes a monster from the server registry.
func (s *Server) RemoveMonster(id string) bool {
	s.entitiesMu.Lock()
	_, exists := s.monsters[id]
	delete(s.monsters, id)
	s.entitiesMu.Unlock()
	return exists
}


// runMonsterSimulation applies simple server-authoritative gravity to test monsters.
// Keeping the simulation on the server means clients only render the received transform.
func (s *Server) runMonsterSimulation(ctx context.Context, finished <-chan struct{}) {
	ticker := time.NewTicker(monsterPhysicsTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-finished:
			return
		case <-ticker.C:
			s.simulateMonsterFall(monsterPhysicsTick.Seconds())
		}
	}
}

// simulateMonsterFall advances all falling monsters and clamps them to their server
// heightmap surface so the model's base cannot fall through or hover above the ground.
func (s *Server) simulateMonsterFall(deltaSeconds float64) {
	if deltaSeconds <= 0 || math.IsNaN(deltaSeconds) || math.IsInf(deltaSeconds, 0) {
		return
	}

	s.entitiesMu.Lock()
	defer s.entitiesMu.Unlock()

	for id, monster := range s.monsters {
		if !monster.Alive || !monster.falling {
			continue
		}

		monster.verticalVelocity -= monsterGravity * deltaSeconds
		monster.Position.Y += monster.verticalVelocity * deltaSeconds
		if monster.Position.Y <= monster.groundY {
			monster.Position.Y = monster.groundY
			monster.verticalVelocity = 0
			monster.falling = false
			monster.Action = "idle"
			monster.AIState = "idle"
		}
		s.monsters[id] = monster
	}
}

// AddGroundItem adds or replaces an item lying on the ground.
func (s *Server) AddGroundItem(item GroundItemState) bool {
	item.ID = strings.TrimSpace(item.ID)
	if item.ID == "" || !s.validEntityPosition(item.Position) || item.Quantity < 1 {
		return false
	}

	s.entitiesMu.Lock()
	s.groundItems[item.ID] = item
	s.entitiesMu.Unlock()
	return true
}

// RemoveGroundItem removes a ground item, for example after a player picks it up.
func (s *Server) RemoveGroundItem(id string) bool {
	s.entitiesMu.Lock()
	_, exists := s.groundItems[id]
	delete(s.groundItems, id)
	s.entitiesMu.Unlock()
	return exists
}

func (s *Server) validEntityPosition(position worldmap.Vec3) bool {
	if !finiteNumber(position.X) || !finiteNumber(position.Y) || !finiteNumber(position.Z) {
		return false
	}
	if s.world == nil {
		return true
	}
	if _, insideWorld := s.world.HeightAtWorld(position.X, position.Z); !insideWorld {
		return false
	}
	world := s.world.Summary()
	return position.Y >= world.Origin.Y-5 &&
		position.Y <= world.Origin.Y+world.Size.Y+5
}

func (s *Server) worldSnapshot(requestingPlayerID string, center worldmap.Vec3, radius float64) ([]NearbyPlayer, []MonsterState, []GroundItemState) {
	radius = clampVisibilityRange(radius)
	nearbyPlayers := make([]NearbyPlayer, 0)
	for _, state := range s.PlayerStates() {
		if state.PlayerID == requestingPlayerID || !positionWithinRange(center, state.Position, radius) {
			continue
		}
		nearbyPlayers = append(nearbyPlayers, NearbyPlayer{
			PlayerID: state.PlayerID,
			Position: state.Position,
			Rotation: state.Rotation,
		})
	}

	nearbyMonsters := make([]MonsterState, 0)
	nearbyItems := make([]GroundItemState, 0)
	s.entitiesMu.RLock()
	for _, monster := range s.monsters {
		if monster.Alive && positionWithinRange(center, monster.Position, radius) {
			nearbyMonsters = append(nearbyMonsters, monster)
		}
	}
	for _, item := range s.groundItems {
		if positionWithinRange(center, item.Position, radius) {
			nearbyItems = append(nearbyItems, item)
		}
	}
	s.entitiesMu.RUnlock()

	sort.Slice(nearbyPlayers, func(i, j int) bool {
		return nearbyPlayers[i].PlayerID < nearbyPlayers[j].PlayerID
	})
	sort.Slice(nearbyMonsters, func(i, j int) bool {
		return nearbyMonsters[i].ID < nearbyMonsters[j].ID
	})
	sort.Slice(nearbyItems, func(i, j int) bool {
		return nearbyItems[i].ID < nearbyItems[j].ID
	})
	return nearbyPlayers, nearbyMonsters, nearbyItems
}

func positionWithinRange(center, position worldmap.Vec3, radius float64) bool {
	dx := center.X - position.X
	dy := center.Y - position.Y
	dz := center.Z - position.Z
	return dx*dx+dy*dy+dz*dz <= radius*radius
}

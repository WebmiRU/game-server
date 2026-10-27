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
	// Death animation (1s) + five seconds on the ground + one-second fade = seven seconds total.
	MonsterCorpseRetention = 7 * time.Second
	// Respawn is timed from the moment a monster dies, not from when its corpse is removed.
	MonsterRespawnDelay = 10 * time.Second
	MonsterSpawnVisualDuration = 1200 * time.Millisecond
	MonsterSpawnHeight = 5.0
)

const (
	ColliderCylinder = "cylinder"
	ColliderBox      = "box"
)

// ColliderDefinition is shared by server simulation and client collision proxies.
// Positions refer to the bottom-centre of each vertical collider.
type ColliderDefinition struct {
	Shape  string        `json:"shape"`
	Radius float64       `json:"radius,omitempty"`
	Height float64       `json:"height"`
	Size   worldmap.Vec3 `json:"size,omitempty"`
}

// NearbyPlayer is the compact representation sent to clients for another active player.
type NearbyPlayer struct {
	PlayerID  string              `json:"playerId"`
	Position  worldmap.Vec3       `json:"position"`
	Rotation  PlayerRotation      `json:"rotation"`
	Health    int                 `json:"health"`
	MaxHealth int                 `json:"maxHealth"`
	Collider  ColliderDefinition  `json:"collider"`
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
	Health               int               `json:"health"`
	MaxHealth            int               `json:"maxHealth"`
	Alive                bool              `json:"alive"`
	Action               string            `json:"action"`
	AIState              string            `json:"aiState"`
	Collider             ColliderDefinition `json:"collider"`
	DeathElapsedSeconds float64           `json:"deathElapsedSeconds,omitempty"`
	Spawning            bool              `json:"spawning,omitempty"`
	SpawnElapsedSeconds float64           `json:"spawnElapsedSeconds,omitempty"`

	// Server-only bookkeeping. Spawn/death timing is derived for each world snapshot.
	groundY          float64
	verticalVelocity float64
	falling           bool
	deathAt           time.Time
	spawnAt           time.Time
}

// MonsterSpawnPoint stores only immutable respawn configuration and the next due time.
// It is deliberately separate from the monster registry so removed corpses are not retained.
type MonsterSpawnPoint struct {
	ID         string
	TemplateID string
	Name       string
	Position   worldmap.Vec3
	Rotation   PlayerRotation
	MaxHealth  int
	Collider   ColliderDefinition
	RespawnAt  time.Time
}

// GroundItemState is a server-owned item lying in the world.
type GroundItemState struct {
	ID           string        `json:"id"`
	ItemTemplate string        `json:"itemTemplate"`
	Name         string        `json:"name"`
	Position     worldmap.Vec3 `json:"position"`
	Quantity     int           `json:"quantity"`
}

// TargetReference identifies a live entity selected by one player.
// References are server-owned runtime state and are intentionally not persisted with transforms.
type TargetReference struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
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

		maxHealth := defaultMonsterMaxHealth(seed.templateID)
		rotation := PlayerRotation{X: 0, Y: 0, Z: 0, W: 1}
		collider := colliderForMonster(MonsterState{TemplateID: seed.templateID})
		spawnedAt := time.Now()
		monster := MonsterState{
			ID:                  seed.id,
			TemplateID:          seed.templateID,
			Name:                seed.name,
			Position:            worldmap.Vec3{X: seed.x, Y: y + MonsterSpawnHeight, Z: seed.z},
			// Identity quaternion is explicit: the dog's face points +Z and its tail points -Z.
			Rotation:          rotation,
			Health:            maxHealth,
			MaxHealth:         maxHealth,
			Alive:             true,
			Action:            "falling",
			AIState:           "falling",
			Collider:          collider,
			groundY:           y,
			verticalVelocity:  0,
			falling:           true,
			spawnAt:           spawnedAt,
		}
		s.monsters[monster.ID] = monster
		s.monsterSpawnPoints[monster.ID] = MonsterSpawnPoint{
			ID:         monster.ID,
			TemplateID: monster.TemplateID,
			Name:       monster.Name,
			Position:   worldmap.Vec3{X: seed.x, Y: y, Z: seed.z},
			Rotation:   rotation,
			MaxHealth:  maxHealth,
			Collider:   collider,
		}
	}

	s.logger.Printf("test monsters initialized: count=%d", len(s.monsters))
}

// UpsertMonster adds a monster to the server registry or replaces its current state.
// It returns false when the entry lacks an ID or an invalid position.
func defaultMonsterMaxHealth(templateID string) int {
	if templateID == "test-sphere-enemy" {
		return 50
	}
	return 100
}

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
	if monster.MaxHealth == 0 {
		monster.MaxHealth = defaultMonsterMaxHealth(monster.TemplateID)
	}
	if monster.Alive && monster.Health == 0 {
		monster.Health = monster.MaxHealth
	}
	if monster.Health > monster.MaxHealth {
		monster.Health = monster.MaxHealth
	}
	if monster.Health <= 0 {
		monster.Health = 0
		monster.Alive = false
	}
	if !monster.Alive {
		monster.Action = "dead"
		monster.AIState = "dead"
		monster.falling = false
		if monster.deathAt.IsZero() {
			monster.deathAt = time.Now()
		}
	} else {
		monster.deathAt = time.Time{}
	}
	if strings.TrimSpace(monster.Action) == "" {
		monster.Action = "idle"
	}
	if strings.TrimSpace(monster.AIState) == "" {
		monster.AIState = "idle"
	}
	monster.Collider = colliderForMonster(monster)

	monster.groundY = monster.Position.Y
	if s.world != nil {
		if terrainHeight, ok := s.world.HeightAtWorld(monster.Position.X, monster.Position.Z); ok {
			monster.groundY = terrainHeight
		}
	}
	monster.falling = strings.EqualFold(monster.Action, "falling")
	monster.verticalVelocity = 0
	if monster.falling {
		monster.spawnAt = time.Now()
	} else {
		monster.spawnAt = time.Time{}
	}

	spawnPoint := MonsterSpawnPoint{
		ID:         monster.ID,
		TemplateID: monster.TemplateID,
		Name:       monster.Name,
		Position:   worldmap.Vec3{X: monster.Position.X, Y: monster.groundY, Z: monster.Position.Z},
		Rotation:   monster.Rotation,
		MaxHealth:  monster.MaxHealth,
		Collider:   monster.Collider,
	}
	if !monster.Alive {
		spawnPoint.RespawnAt = monster.deathAt.Add(MonsterRespawnDelay)
	}

	s.entitiesMu.Lock()
	s.monsters[monster.ID] = monster
	s.monsterSpawnPoints[monster.ID] = spawnPoint
	s.entitiesMu.Unlock()
	if !monster.Alive {
		s.clearTargetReferences("monster", monster.ID)
	}
	return true
}

// RemoveMonster removes a monster from the server registry.
func (s *Server) RemoveMonster(id string) bool {
	id = strings.TrimSpace(id)
	s.entitiesMu.Lock()
	_, exists := s.monsters[id]
	_, hasSpawnPoint := s.monsterSpawnPoints[id]
	delete(s.monsters, id)
	delete(s.monsterSpawnPoints, id)
	s.entitiesMu.Unlock()
	if exists || hasSpawnPoint {
		s.clearTargetReferences("monster", id)
	}
	return exists || hasSpawnPoint
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

	now := time.Now()
	s.pruneExpiredMonsterCorpses(now)
	s.respawnDueMonsters(now)
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
	id = strings.TrimSpace(id)
	s.entitiesMu.Lock()
	_, exists := s.groundItems[id]
	delete(s.groundItems, id)
	s.entitiesMu.Unlock()
	if exists {
		s.clearTargetReferences("item", id)
	}
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
	now := time.Now()
	s.pruneExpiredMonsterCorpses(now)
	s.respawnDueMonsters(now)
	radius = clampVisibilityRange(radius)
	nearbyPlayers := make([]NearbyPlayer, 0)
	for _, state := range s.PlayerStates() {
		if state.PlayerID == requestingPlayerID || !positionWithinRange(center, state.Position, radius) {
			continue
		}
		state = normalizePlayerHealth(state)
		nearbyPlayers = append(nearbyPlayers, NearbyPlayer{
			PlayerID:  state.PlayerID,
			Position:  state.Position,
			Rotation:  state.Rotation,
			Health:    state.Health,
			MaxHealth: state.MaxHealth,
			Collider:  playerColliderDefinition(),
		})
	}

	nearbyMonsters := make([]MonsterState, 0)
	nearbyItems := make([]GroundItemState, 0)
	s.entitiesMu.RLock()
	for _, monster := range s.monsters {
		if !positionWithinRange(center, monster.Position, radius) {
			continue
		}
		if !monster.Alive {
			if monster.deathAt.IsZero() {
				continue
			}
			monster.DeathElapsedSeconds = math.Max(0, now.Sub(monster.deathAt).Seconds())
		} else if !monster.spawnAt.IsZero() {
			spawnElapsed := now.Sub(monster.spawnAt)
			if spawnElapsed >= 0 && spawnElapsed < MonsterSpawnVisualDuration {
				monster.Spawning = true
				monster.SpawnElapsedSeconds = spawnElapsed.Seconds()
			}
		}
		monster.Collider = colliderForMonster(monster)
		nearbyMonsters = append(nearbyMonsters, monster)
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

// scheduleMonsterRespawnLocked records the due time while entitiesMu is held.
func (s *Server) scheduleMonsterRespawnLocked(monster MonsterState) {
	spawnPoint, exists := s.monsterSpawnPoints[monster.ID]
	if !exists || monster.deathAt.IsZero() {
		return
	}
	spawnPoint.RespawnAt = monster.deathAt.Add(MonsterRespawnDelay)
	s.monsterSpawnPoints[monster.ID] = spawnPoint
}

// respawnDueMonsters creates fresh entities from spawn configuration. It does not revive
// or retain an old corpse: the old entity is removed after seven seconds, and only this
// separate spawn-point configuration remains until the ten-second respawn time.
func (s *Server) respawnDueMonsters(now time.Time) {
	s.entitiesMu.Lock()
	defer s.entitiesMu.Unlock()

	for id, spawnPoint := range s.monsterSpawnPoints {
		if spawnPoint.RespawnAt.IsZero() || now.Before(spawnPoint.RespawnAt) {
			continue
		}
		if _, exists := s.monsters[id]; exists {
			// A live entity may have been administratively recreated before the due time.
			spawnPoint.RespawnAt = time.Time{}
			s.monsterSpawnPoints[id] = spawnPoint
			continue
		}

		groundY := spawnPoint.Position.Y
		if s.world != nil {
			if terrainHeight, ok := s.world.HeightAtWorld(spawnPoint.Position.X, spawnPoint.Position.Z); ok {
				groundY = terrainHeight
			}
		}
		spawnPoint.Position.Y = groundY
		maxHealth := spawnPoint.MaxHealth
		if maxHealth <= 0 {
			maxHealth = defaultMonsterMaxHealth(spawnPoint.TemplateID)
		}
		collider := spawnPoint.Collider
		if collider.Shape == "" {
			collider = colliderForMonster(MonsterState{TemplateID: spawnPoint.TemplateID})
		}
		spawnedAt := now
		monster := MonsterState{
			ID:                  spawnPoint.ID,
			TemplateID:          spawnPoint.TemplateID,
			Name:                spawnPoint.Name,
			Position:            worldmap.Vec3{X: spawnPoint.Position.X, Y: groundY + MonsterSpawnHeight, Z: spawnPoint.Position.Z},
			Rotation:            spawnPoint.Rotation,
			Health:              maxHealth,
			MaxHealth:           maxHealth,
			Alive:               true,
			Action:              "falling",
			AIState:             "falling",
			Collider:            collider,
			groundY:             groundY,
			verticalVelocity:    0,
			falling:             true,
			spawnAt:             spawnedAt,
		}
		s.monsters[id] = monster
		spawnPoint.RespawnAt = time.Time{}
		s.monsterSpawnPoints[id] = spawnPoint
		s.logger.Printf("monster respawned: id=%q template=%q health=%d position=(%.2f, %.2f, %.2f)",
			monster.ID, monster.TemplateID, monster.Health,
			monster.Position.X, monster.Position.Y, monster.Position.Z)
	}
}

func (s *Server) pruneExpiredMonsterCorpses(now time.Time) {
	var expiredIDs []string
	s.entitiesMu.Lock()
	for id, monster := range s.monsters {
		if !monster.Alive && !monster.deathAt.IsZero() && now.Sub(monster.deathAt) >= MonsterCorpseRetention {
			delete(s.monsters, id)
			expiredIDs = append(expiredIDs, id)
		}
	}
	s.entitiesMu.Unlock()
	for _, id := range expiredIDs {
		s.clearTargetReferences("monster", id)
	}
}

func positionWithinRange(center, position worldmap.Vec3, radius float64) bool {
	dx := center.X - position.X
	dy := center.Y - position.Y
	dz := center.Z - position.Z
	return dx*dx+dy*dy+dz*dz <= radius*radius
}

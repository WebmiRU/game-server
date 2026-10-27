package tcpserver

import (
	"math"

	"game-server/internal/worldmap"
)

const (
	// PlayerColliderRadius and PlayerColliderHeight define a 1 x 2 metre
	// vertical cylinder. PlayerState.Position is the bottom-centre of this collider.
	PlayerColliderRadius = 0.5
	PlayerColliderHeight = 2.0

	// Collision uses small deterministic substeps to avoid tunnelling through thin obstacles.
	collisionSubstepDistance = 0.1
	maxPlayerMovePerUpdate   = 2.0
	collisionEpsilon         = 1e-6
)

// playerColliderDefinition is also sent with nearby player records so clients use the same size.
func playerColliderDefinition() ColliderDefinition {
	return ColliderDefinition{
		Shape:  ColliderCylinder,
		Radius: PlayerColliderRadius,
		Height: PlayerColliderHeight,
	}
}

// colliderForMonster returns a validated default if an older call site did not specify a collider.
func colliderForMonster(monster MonsterState) ColliderDefinition {
	collider := monster.Collider
	if collider.Shape == ColliderBox {
		if collider.Size.X <= 0 {
			collider.Size.X = 2
		}
		if collider.Size.Y <= 0 {
			collider.Size.Y = 2
		}
		if collider.Size.Z <= 0 {
			collider.Size.Z = 2
		}
		return collider
	}
	if collider.Shape == ColliderCylinder {
		if collider.Radius <= 0 {
			collider.Radius = 1
		}
		if collider.Height <= 0 {
			collider.Height = 2
		}
		return collider
	}

	switch monster.TemplateID {
	case "test-cube-enemy":
		return ColliderDefinition{
			Shape:  ColliderBox,
			Height: 2,
			Size:   worldmap.Vec3{X: 2, Y: 2, Z: 2},
		}
	default:
		return ColliderDefinition{
			Shape:  ColliderCylinder,
			Radius: 1,
			Height: 2,
		}
	}
}

// resolvePlayerMovement checks the whole path between the last accepted position and
// the requested position. Axis-separated fallback permits sliding along an obstacle
// instead of making movement feel like it hit an invisible wall.
func (s *Server) resolvePlayerMovement(playerID string, from, requested worldmap.Vec3) worldmap.Vec3 {
	groundPosition := func(position worldmap.Vec3) worldmap.Vec3 {
		// In the current prototype players cannot jump or fly. The server's heightmap
		// owns player Y and prevents a client from lifting itself over a collider.
		if s.world != nil {
			if terrainY, ok := s.world.HeightAtWorld(position.X, position.Z); ok {
				position.Y = terrainY + 0.05 // client's CharacterController foot offset
			}
		}
		return position
	}

	from = groundPosition(from)

	// A player may already overlap an entity because of older movement code, latency,
	// or a collider introduced while the player was standing still. Do not treat that
	// invalid starting overlap as a wall in every direction: first find the nearest
	// valid point around it, then resolve the requested movement from there.
	if s.playerPositionCollides(playerID, from) {
		if escaped, ok := s.findNearbyFreePosition(playerID, from, groundPosition); ok {
			from = escaped
		}
	}

	dx := requested.X - from.X
	dz := requested.Z - from.Z
	dy := requested.Y - from.Y
	horizontalDistance := math.Hypot(dx, dz)

	// Bound malformed/teleport-like movement updates to keep collision work predictable.
	if horizontalDistance > maxPlayerMovePerUpdate {
		scale := maxPlayerMovePerUpdate / horizontalDistance
		dx *= scale
		dz *= scale
		requested.X = from.X + dx
		requested.Z = from.Z + dz
		horizontalDistance = maxPlayerMovePerUpdate
	}
	if s.world == nil && math.Abs(dy) > maxPlayerMovePerUpdate {
		dy = math.Copysign(maxPlayerMovePerUpdate, dy)
		requested.Y = from.Y + dy
	}

	travelDistance := horizontalDistance
	if s.world == nil {
		travelDistance = math.Max(horizontalDistance, math.Abs(dy))
	}
	steps := int(math.Ceil(travelDistance / collisionSubstepDistance))
	if steps < 1 {
		steps = 1
	}

	current := from
	stepX := dx / float64(steps)
	stepY := (requested.Y - from.Y) / float64(steps)
	stepZ := dz / float64(steps)
	for step := 0; step < steps; step++ {
		// Each substep starts at the latest accepted point to prevent tunnelling.
		next := groundPosition(worldmap.Vec3{
			X: current.X + stepX,
			Y: current.Y + stepY,
			Z: current.Z + stepZ,
		})
		if !s.playerPositionCollides(playerID, next) {
			current = next
			continue
		}

		// Try sliding along each horizontal axis independently.
		xOnly := groundPosition(worldmap.Vec3{
			X: current.X + stepX,
			Y: current.Y + stepY,
			Z: current.Z,
		})
		if !s.playerPositionCollides(playerID, xOnly) {
			current = xOnly
		}

		zOnly := groundPosition(worldmap.Vec3{
			X: current.X,
			Y: current.Y + stepY,
			Z: current.Z + stepZ,
		})
		if !s.playerPositionCollides(playerID, zOnly) {
			current = zOnly
		}
	}
	return current
}

// recoverSpawnOverlap reconciles the saved position with the current terrain and,
// when it overlaps an entity, moves it by the shortest local displacement to a free point.
// Player facing is deliberately irrelevant: a reconnect must not push someone through a
// gate simply because they turned around before disconnecting.
func (s *Server) recoverSpawnOverlap(
	playerID string,
	position worldmap.Vec3,
) (worldmap.Vec3, bool, bool) {
	original := position
	groundPosition := func(candidate worldmap.Vec3) worldmap.Vec3 {
		if s.world != nil {
			if terrainY, ok := s.world.HeightAtWorld(candidate.X, candidate.Z); ok {
				candidate.Y = terrainY + 0.05
			}
		}
		return candidate
	}
	position = groundPosition(position)
	changed := math.Abs(position.Y-original.Y) > 0.001 ||
		math.Abs(position.X-original.X) > 0.001 ||
		math.Abs(position.Z-original.Z) > 0.001

	if !s.playerPositionCollides(playerID, position) {
		return position, changed, false
	}

	// Search all directions around the overlap before considering a distant fallback.
	// The local search chooses the shortest free displacement, not a facing-based path.
	if candidate, ok := s.findNearbyFreePosition(playerID, position, groundPosition); ok {
		return candidate, true, true
	}
	if candidate, ok := s.findFreeSpawnPosition(playerID, position, groundPosition); ok {
		return candidate, true, true
	}

	return position, changed, true
}

const spawnRecoveryStep = 0.1
const maxSpawnRecoveryDistance = 50.0

// findNearbyFreePosition searches around an overlapping entity in all horizontal
// directions. It finds the first radius with free candidates, then binary-refines their
// distances so the returned point is as close as possible to the original position.
func (s *Server) findNearbyFreePosition(
	playerID string,
	origin worldmap.Vec3,
	groundPosition func(worldmap.Vec3) worldmap.Vec3,
) (worldmap.Vec3, bool) {
	return s.findNearestFreePosition(playerID, origin, groundPosition, 6.0, 0.05, 128)
}

// findFreeSpawnPosition is the broader emergency fallback used when nearby obstacles
// leave no free point within the local recovery radius.
func (s *Server) findFreeSpawnPosition(
	playerID string,
	origin worldmap.Vec3,
	groundPosition func(worldmap.Vec3) worldmap.Vec3,
) (worldmap.Vec3, bool) {
	return s.findNearestFreePosition(
		playerID, origin, groundPosition, maxSpawnRecoveryDistance, spawnRecoveryStep, 64)
}

func (s *Server) findNearestFreePosition(
	playerID string,
	origin worldmap.Vec3,
	groundPosition func(worldmap.Vec3) worldmap.Vec3,
	maxRadius, radiusStep float64,
	directions int,
) (worldmap.Vec3, bool) {
	if maxRadius <= 0 || radiusStep <= 0 || directions < 1 {
		return origin, false
	}

	candidateAt := func(distance, angle float64) (worldmap.Vec3, bool) {
		candidate := groundPosition(worldmap.Vec3{
			X: origin.X + math.Cos(angle)*distance,
			Y: origin.Y,
			Z: origin.Z + math.Sin(angle)*distance,
		})
		if s.world != nil {
			if _, insideWorld := s.world.HeightAtWorld(candidate.X, candidate.Z); !insideWorld {
				return candidate, false
			}
		}
		return candidate, !s.playerPositionCollides(playerID, candidate)
	}

	steps := int(math.Ceil(maxRadius / radiusStep))
	for ring := 1; ring <= steps; ring++ {
		radius := math.Min(float64(ring)*radiusStep, maxRadius)
		previousRadius := math.Max(0, float64(ring-1)*radiusStep)
		freeAngles := make([]float64, 0, directions)
		for direction := 0; direction < directions; direction++ {
			angle := 2 * math.Pi * float64(direction) / float64(directions)
			if _, free := candidateAt(radius, angle); free {
				freeAngles = append(freeAngles, angle)
			}
		}
		if len(freeAngles) == 0 {
			continue
		}

		// Refine each free direction from the previous (blocked) ring and choose
		// the smallest horizontal displacement among all of the newly free points.
		bestDistance := math.Inf(1)
		var bestPosition worldmap.Vec3
		for _, angle := range freeAngles {
			low, high := previousRadius, radius
			for iteration := 0; iteration < 18; iteration++ {
				middle := (low + high) * 0.5
				if _, free := candidateAt(middle, angle); free {
					high = middle
				} else {
					low = middle
				}
			}
			candidate, free := candidateAt(high, angle)
			if free && high < bestDistance {
				bestDistance = high
				bestPosition = candidate
			}
		}
		if !math.IsInf(bestDistance, 1) {
			return bestPosition, true
		}
	}
	return origin, false
}

func (s *Server) playerPositionCollides(playerID string, position worldmap.Vec3) bool {
	// Players are vertical cylinders. Only active sessions block movement.
	for _, other := range s.PlayerStates() {
		if other.PlayerID == playerID {
			continue
		}
		if verticalIntervalsOverlap(position.Y, PlayerColliderHeight, other.Position.Y, PlayerColliderHeight) &&
			circlesOverlapXZ(position.X, position.Z, PlayerColliderRadius,
				other.Position.X, other.Position.Z, PlayerColliderRadius) {
			return true
		}
	}

	s.entitiesMu.RLock()
	defer s.entitiesMu.RUnlock()
	for _, monster := range s.monsters {
		if !monster.Alive {
			continue
		}
		collider := colliderForMonster(monster)
		height := collider.Height
		if collider.Shape == ColliderBox && collider.Size.Y > 0 {
			height = collider.Size.Y
		}
		if height <= 0 {
			height = 2
		}
		if !verticalIntervalsOverlap(position.Y, PlayerColliderHeight, monster.Position.Y, height) {
			continue
		}

		if collider.Shape == ColliderBox {
			if circleOverlapsRotatedBox(
				position.X, position.Z, PlayerColliderRadius,
				monster.Position.X, monster.Position.Z,
				collider.Size.X, collider.Size.Z, monster.Rotation) {
				return true
			}
		} else {
			radius := collider.Radius
			if radius <= 0 {
				radius = 1
			}
			if circlesOverlapXZ(position.X, position.Z, PlayerColliderRadius,
				monster.Position.X, monster.Position.Z, radius) {
				return true
			}
		}
	}
	return false
}

func verticalIntervalsOverlap(aBottom, aHeight, bBottom, bHeight float64) bool {
	return aBottom < bBottom+bHeight-collisionEpsilon &&
		bBottom < aBottom+aHeight-collisionEpsilon
}

func circlesOverlapXZ(ax, az, ar, bx, bz, br float64) bool {
	dx := ax - bx
	dz := az - bz
	radius := ar + br
	return dx*dx+dz*dz < radius*radius-collisionEpsilon
}

// circleOverlapsRotatedBox performs a circle-vs-oriented-box test in the X/Z plane.
// The box rotation comes from the server-owned quaternion; pitch and roll do not alter
// the ground footprint, so only its yaw participates in horizontal collision.
func circleOverlapsRotatedBox(
	circleX, circleZ, circleRadius,
	boxX, boxZ, boxWidth, boxDepth float64,
	rotation PlayerRotation,
) bool {
	if boxWidth <= 0 {
		boxWidth = 2
	}
	if boxDepth <= 0 {
		boxDepth = 2
	}

	yaw := math.Atan2(
		2*(rotation.Y*rotation.W+rotation.X*rotation.Z),
		1-2*(rotation.Y*rotation.Y+rotation.Z*rotation.Z),
	)
	cosYaw := math.Cos(yaw)
	sinYaw := math.Sin(yaw)
	dx := circleX - boxX
	dz := circleZ - boxZ

	// Rotate the circle centre into the box's local X/Z coordinate system.
	localX := dx*cosYaw - dz*sinYaw
	localZ := dx*sinYaw + dz*cosYaw
	halfWidth := boxWidth * 0.5
	halfDepth := boxDepth * 0.5
	closestX := math.Max(-halfWidth, math.Min(localX, halfWidth))
	closestZ := math.Max(-halfDepth, math.Min(localZ, halfDepth))
	offsetX := localX - closestX
	offsetZ := localZ - closestZ

	// The player centre inside the rectangle is always a collision. Otherwise use
	// squared distance to the closest point to avoid an expensive square root.
	if math.Abs(offsetX) < collisionEpsilon && math.Abs(offsetZ) < collisionEpsilon {
		return true
	}
	return offsetX*offsetX+offsetZ*offsetZ < circleRadius*circleRadius-collisionEpsilon
}

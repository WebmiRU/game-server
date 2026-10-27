package tcpserver

import (
	"math"

	"game-server/internal/worldmap"
)

const (
	// PlayerColliderRadius and PlayerColliderHeight define a 0.5 x 2 metre
	// vertical cylinder. PlayerState.Position is the bottom-centre of this collider.
	PlayerColliderRadius = 0.25
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
	dx := requested.X - from.X
	dz := requested.Z - from.Z
	dy := requested.Y - from.Y
	horizontalDistance := math.Hypot(dx, dz)

	// Limit an individual update to a generous distance above normal client movement.
	// This also bounds the amount of collision work for malformed/teleport-like requests.
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

	groundPosition := func(position worldmap.Vec3) worldmap.Vec3 {
		// In the current game prototype players cannot jump or fly. The server's
		// heightmap therefore owns player Y as well as X/Z; never trust a client's
		// vertical coordinate to lift it over a monster or another player.
		if s.world != nil {
			if terrainY, ok := s.world.HeightAtWorld(position.X, position.Z); ok {
				position.Y = terrainY + 0.05 // client CharacterController foot offset
			}
			return position
		}
		return position
	}

	for step := 0; step < steps; step++ {
		// Every substep starts at the last accepted location. Do not sample the original
		// straight line after a block: doing so could jump to the far side of a collider.
		next := worldmap.Vec3{
			X: current.X + stepX,
			Y: current.Y + stepY,
			Z: current.Z + stepZ,
		}
		next = groundPosition(next)
		if !s.playerPositionCollides(playerID, next) {
			current = next
			continue
		}

		// Try sliding along each horizontal axis independently.
		xOnly := worldmap.Vec3{
			X: current.X + stepX,
			Y: current.Y + stepY,
			Z: current.Z,
		}
		xOnly = groundPosition(xOnly)
		if !s.playerPositionCollides(playerID, xOnly) {
			current = xOnly
		}

		zOnly := worldmap.Vec3{
			X: current.X,
			Y: current.Y + stepY,
			Z: current.Z + stepZ,
		}
		zOnly = groundPosition(zOnly)
		if !s.playerPositionCollides(playerID, zOnly) {
			current = zOnly
		}
	}
	return current
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

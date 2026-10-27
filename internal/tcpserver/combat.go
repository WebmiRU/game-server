package tcpserver

import (
	"math"
	"net"
	"strings"
	"time"

	"game-server/internal/worldmap"
)

const (
	// DefaultAttackSpeed is the default number of melee swings per second.
	DefaultAttackSpeed = 1.0
	MinAttackSpeed     = 0.1
	MaxAttackSpeed     = 10.0

	// MeleeAttackRangeAllowance includes the intentional small gap between colliders,
	// client click-to-move stopping tolerance, and a small network/physics margin.
	// Players should not need to physically push into one another to attack.
	MeleeAttackRangeAllowance = 0.4
	MeleeAttackDamage         = 10
	MeleeSideDamageMultiplier = 1.5
	MeleeRearDamageMultiplier = 2.0
	meleeFrontRearDotThreshold = 0.7071067811865476 // cos(45 degrees)
	attackCooldownRetryFloor  = 0.05
	outOfRangeRetryDelay      = 0.15
)

// SetPlayerAttackSpeed sets a player's current effective attack frequency in attacks per
// second. Buff/debuff systems should call this whenever the effective stat changes.
// Values are clamped to a safe, supported range; invalid values are rejected.
func (s *Server) SetPlayerAttackSpeed(playerID string, attacksPerSecond float64) bool {
	playerID = strings.TrimSpace(playerID)
	if s == nil || playerID == "" || math.IsNaN(attacksPerSecond) ||
		math.IsInf(attacksPerSecond, 0) || attacksPerSecond <= 0 {
		return false
	}

	attacksPerSecond = math.Max(MinAttackSpeed, math.Min(MaxAttackSpeed, attacksPerSecond))
	s.combatStatsMu.Lock()
	if s.attackSpeeds == nil {
		s.attackSpeeds = make(map[string]float64)
	}
	s.attackSpeeds[playerID] = attacksPerSecond
	s.combatStatsMu.Unlock()
	return true
}

// PlayerAttackSpeed returns the effective attack frequency that is sent in world snapshots.
// Temporary modifiers are intentionally kept out of persisted PlayerState.
func (s *Server) PlayerAttackSpeed(playerID string) float64 {
	if s == nil {
		return DefaultAttackSpeed
	}
	s.combatStatsMu.RLock()
	attackSpeed, exists := s.attackSpeeds[strings.TrimSpace(playerID)]
	s.combatStatsMu.RUnlock()
	if !exists || math.IsNaN(attackSpeed) || math.IsInf(attackSpeed, 0) || attackSpeed <= 0 {
		return DefaultAttackSpeed
	}
	return attackSpeed
}

// handleAttack validates one strike against the target already selected on the server.
// The client does not send a target ID here: set_target owns selection, and this command
// always acts on the server's current target. Damage and hit direction are server-authoritative.
func (s *Server) handleAttack(conn net.Conn, playerID string, req request) error {
	target := s.currentPlayerTarget(playerID)
	if target == nil {
		return s.writeAttackResult(conn, playerID, req.RequestID, false,
			"target_not_selected", nil, 0)
	}

	// Only entities with implemented combat reach can be attacked in this step.
	// Adding NPCs or other targetable entities later requires an explicit combat rule.
	if target.Type != "monster" && target.Type != "player" {
		return s.writeAttackResult(conn, playerID, req.RequestID, false,
			"target_type_not_attackable", target, 0)
	}

	attacker, exists := s.storedPlayerState(playerID)
	if !exists || !attacker.Online {
		return s.writeAttackResult(conn, playerID, req.RequestID, false,
			"attacker_state_unavailable", target, 0)
	}

	targetPosition, targetRadius, exists := s.attackTargetInfo(*target, attacker.Position)
	if !exists {
		return s.writeAttackResult(conn, playerID, req.RequestID, false,
			"target_not_found", nil, 0)
	}

	distance := math.Hypot(
		attacker.Position.X-targetPosition.X,
		attacker.Position.Z-targetPosition.Z)
	maxDistance := PlayerColliderRadius + targetRadius + MeleeAttackRangeAllowance
	if distance > maxDistance {
		return s.writeAttackResult(conn, playerID, req.RequestID, false,
			"attack_out_of_range", target, outOfRangeRetryDelay)
	}

	accepted, speed, retryAfter := s.acceptAttackAttempt(playerID, time.Now())
	if !accepted {
		return s.writeAttackResult(conn, playerID, req.RequestID, false,
			"attack_cooldown", target, retryAfter)
	}

	var damagedMonster *MonsterState
	damage := 0
	attackSide := ""
	damageMultiplier := 1.0
	if target.Type == "monster" {
		monster, appliedDamage, side, multiplier, ok :=
			s.applyMonsterMeleeDamage(target.ID, attacker.Position)
		if !ok {
			return s.writeAttackResult(conn, playerID, req.RequestID, false,
				"target_not_found", nil, 0)
		}
		damagedMonster = &monster
		damage = appliedDamage
		attackSide = side
		damageMultiplier = multiplier
	}

	s.logger.Printf(
		"melee attack accepted: player=%q targetType=%q targetID=%q damage=%d direction=%q multiplier=%.1f distance=%.3f range=%.3f attacksPerSecond=%.3f",
		playerID, target.Type, target.ID, damage, attackSide, damageMultiplier, distance, maxDistance, speed)
	return s.writeResponse(conn, response{
		Type:             "attack_result",
		Success:          boolPointer(true),
		Message:          "attack_accepted",
		PlayerID:         playerID,
		Target:           target,
		Monster:          damagedMonster,
		RequestID:        req.RequestID,
		AttackSpeed:      speed,
		Damage:           damage,
		DamageMultiplier: damageMultiplier,
		AttackSide:       attackSide,
	})
}

// calculateMeleeDamage classifies the horizontal direction of an attacker relative to a
// target's server-owned facing. The front and rear arcs are 90 degrees wide; the remaining
// two side arcs cover 90 degrees each. A zero/invalid target rotation defaults to +Z facing.
func calculateMeleeDamage(
	attackerPosition, targetPosition worldmap.Vec3,
	targetRotation PlayerRotation,
) (int, string, float64) {
	dx := attackerPosition.X - targetPosition.X
	dz := attackerPosition.Z - targetPosition.Z
	distance := math.Hypot(dx, dz)
	if distance <= 1e-6 {
		return MeleeAttackDamage, "front", 1.0
	}

	qx, qy, qz, qw := targetRotation.X, targetRotation.Y, targetRotation.Z, targetRotation.W
	rotationLength := math.Sqrt(qx*qx + qy*qy + qz*qz + qw*qw)
	if !finiteNumber(rotationLength) || rotationLength < 1e-9 {
		qx, qy, qz, qw = 0, 0, 0, 1
	} else {
		qx /= rotationLength
		qy /= rotationLength
		qz /= rotationLength
		qw /= rotationLength
	}

	// Equivalent to rotating Unity's local Vector3.forward by the target quaternion,
	// then projecting that direction onto the horizontal XZ plane.
	forwardX := 2 * (qx*qz + qw*qy)
	forwardZ := 1 - 2*(qx*qx+qy*qy)
	forwardLength := math.Hypot(forwardX, forwardZ)
	if forwardLength <= 1e-6 || !finiteNumber(forwardLength) {
		forwardX, forwardZ = 0, 1
	} else {
		forwardX /= forwardLength
		forwardZ /= forwardLength
	}

	dot := (dx*forwardX + dz*forwardZ) / distance
	dot = math.Max(-1, math.Min(1, dot))

	direction := "side"
	multiplier := MeleeSideDamageMultiplier
	if dot >= meleeFrontRearDotThreshold {
		direction = "front"
		multiplier = 1.0
	} else if dot <= -meleeFrontRearDotThreshold {
		direction = "back"
		multiplier = MeleeRearDamageMultiplier
	}

	return int(math.Round(float64(MeleeAttackDamage) * multiplier)), direction, multiplier
}

// applyMonsterMeleeDamage calculates the attack angle and applies its damage atomically,
// using the monster's current server-owned position and orientation.
func (s *Server) applyMonsterMeleeDamage(
	monsterID string,
	attackerPosition worldmap.Vec3,
) (MonsterState, int, string, float64, bool) {
	s.entitiesMu.Lock()
	monster, exists := s.monsters[monsterID]
	if !exists || !monster.Alive {
		s.entitiesMu.Unlock()
		return MonsterState{}, 0, "", 1.0, false
	}

	damage, direction, multiplier := calculateMeleeDamage(
		attackerPosition, monster.Position, monster.Rotation)
	monster.Health -= damage
	if monster.Health <= 0 {
		monster.Health = 0
		monster.Alive = false
		monster.Action = "dead"
		monster.AIState = "dead"
		monster.falling = false
		monster.verticalVelocity = 0
		monster.deathAt = time.Now()
		monster.DeathElapsedSeconds = 0
		s.scheduleMonsterRespawnLocked(monster)
	}
	s.monsters[monsterID] = monster
	s.entitiesMu.Unlock()

	if !monster.Alive {
		s.clearTargetReferences("monster", monsterID)
	}
	return monster, damage, direction, multiplier, true
}

// applyMonsterDamage applies a flat server-authoritative hit and starts the corpse timer once
// health reaches zero. Dead monsters are immutable until the corpse cleanup removes them.
func (s *Server) applyMonsterDamage(monsterID string, damage int) (MonsterState, bool) {
	if damage <= 0 {
		return MonsterState{}, false
	}

	s.entitiesMu.Lock()
	monster, exists := s.monsters[monsterID]
	if !exists || !monster.Alive {
		s.entitiesMu.Unlock()
		return MonsterState{}, false
	}

	monster.Health -= damage
	if monster.Health <= 0 {
		monster.Health = 0
		monster.Alive = false
		monster.Action = "dead"
		monster.AIState = "dead"
		monster.falling = false
		monster.verticalVelocity = 0
		monster.deathAt = time.Now()
		monster.DeathElapsedSeconds = 0
		s.scheduleMonsterRespawnLocked(monster)
	}
	s.monsters[monsterID] = monster
	s.entitiesMu.Unlock()

	if !monster.Alive {
		s.clearTargetReferences("monster", monsterID)
	}
	return monster, true
}

// writeAttackResult includes the target and current effective attack speed when available,
// allowing the client to correlate the answer and refresh its cadence after a buff/debuff.
func (s *Server) writeAttackResult(
	conn net.Conn,
	playerID string,
	requestID int,
	success bool,
	message string,
	target *TargetReference,
	retryAfter float64,
) error {
	return s.writeResponse(conn, response{
		Type:        "attack_result",
		Success:     boolPointer(success),
		Message:     message,
		PlayerID:    playerID,
		Target:      target,
		RequestID:   requestID,
		AttackSpeed: s.PlayerAttackSpeed(playerID),
		RetryAfter:  retryAfter,
	})
}

// acceptAttackAttempt enforces the current server-owned attack interval. Changing attack
// speed takes effect on the next request, so temporary buffs/debuffs dynamically alter cadence.
func (s *Server) acceptAttackAttempt(playerID string, now time.Time) (bool, float64, float64) {
	s.combatStatsMu.Lock()
	defer s.combatStatsMu.Unlock()

	speed, exists := s.attackSpeeds[playerID]
	if !exists || math.IsNaN(speed) || math.IsInf(speed, 0) || speed <= 0 {
		speed = DefaultAttackSpeed
	}
	speed = math.Max(MinAttackSpeed, math.Min(MaxAttackSpeed, speed))

	interval := time.Duration(float64(time.Second) / speed)
	if lastAttack, hasLastAttack := s.lastAttackAt[playerID]; hasLastAttack {
		remaining := interval - now.Sub(lastAttack)
		if remaining > 0 {
			return false, speed, math.Max(attackCooldownRetryFloor, remaining.Seconds())
		}
	}

	if s.lastAttackAt == nil {
		s.lastAttackAt = make(map[string]time.Time)
	}
	s.lastAttackAt[playerID] = now
	return true, speed, 0
}

// attackTargetInfo returns the target's accepted world position and horizontal reach.
// For boxes, reach is the support radius of the rotated footprint along the attack direction.
func (s *Server) attackTargetInfo(target TargetReference, attackerPosition worldmap.Vec3) (worldmap.Vec3, float64, bool) {
	switch target.Type {
	case "monster":
		s.entitiesMu.RLock()
		monster, exists := s.monsters[target.ID]
		s.entitiesMu.RUnlock()
		if !exists || !monster.Alive {
			return worldmap.Vec3{}, 0, false
		}

		collider := colliderForMonster(monster)
		dx := attackerPosition.X - monster.Position.X
		dz := attackerPosition.Z - monster.Position.Z
		distance := math.Hypot(dx, dz)
		if distance > 1e-6 {
			dx /= distance
			dz /= distance
		} else {
			dx, dz = 1, 0
		}

		if collider.Shape == ColliderBox {
			return monster.Position, rotatedBoxSupportRadius(collider.Size, monster.Rotation, dx, dz), true
		}
		return monster.Position, collider.Radius, true

	case "player":
		if target.ID == "" {
			return worldmap.Vec3{}, 0, false
		}
		s.playersMu.RLock()
		state, exists := s.playerStates[target.ID]
		_, connected := s.playerSessions[target.ID]
		s.playersMu.RUnlock()
		if !exists || !connected || !state.Online {
			return worldmap.Vec3{}, 0, false
		}
		return state.Position, PlayerColliderRadius, true

	default:
		return worldmap.Vec3{}, 0, false
	}
}

func rotatedBoxSupportRadius(size worldmap.Vec3, rotation PlayerRotation, dirX, dirZ float64) float64 {
	yaw := math.Atan2(
		2*(rotation.W*rotation.Y+rotation.X*rotation.Z),
		1-2*(rotation.Y*rotation.Y+rotation.Z*rotation.Z))
	cosYaw := math.Cos(yaw)
	sinYaw := math.Sin(yaw)

	// Rotate world direction into the target's local horizontal coordinate frame.
	localX := dirX*cosYaw - dirZ*sinYaw
	localZ := dirX*sinYaw + dirZ*cosYaw
	halfX := math.Max(0, size.X) * 0.5
	halfZ := math.Max(0, size.Z) * 0.5
	return math.Abs(localX)*halfX + math.Abs(localZ)*halfZ
}

package tcpserver

import (
	"math"
	"strings"
)

const (
	// DefaultAttackSpeed is the default number of melee swings per second.
	DefaultAttackSpeed = 1.0
	MinAttackSpeed     = 0.1
	MaxAttackSpeed     = 10.0
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

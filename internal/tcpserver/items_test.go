package tcpserver

import (
	"encoding/json"
	"io"
	"log"
	"math"
	"net"
	"testing"
	"time"

	"game-server/internal/worldmap"
)

func newItemTestServer() *Server {
	return New(Config{Logger: log.New(io.Discard, "", 0)})
}

func addSoulshotForTest(t *testing.T, server *Server, id string, position worldmap.Vec3, quantity int) {
	t.Helper()
	if !server.AddGroundItem(GroundItemState{
		ID:           id,
		ItemTemplate: "soulshot-d-grade",
		Name:         "Soulshot (D-grade)",
		Position:     position,
		Quantity:     quantity,
	}) {
		t.Fatalf("AddGroundItem(%q) failed", id)
	}
}

func TestMonsterDeathDropsOneTenCountSoulshotCapsule(t *testing.T) {
	tests := []struct {
		name       string
		templateID string
		wantItem   string
		wantName   string
	}{
		{name: "Enemy-2 drops D-grade", templateID: "test-sphere-enemy", wantItem: "soulshot-d-grade", wantName: "Soulshot (D-grade)"},
		{name: "box dog drops C-grade", templateID: "test-cube-enemy", wantItem: "soulshot-c-grade", wantName: "Soulshot (C-grade)"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newItemTestServer()
			position := worldmap.Vec3{X: 10, Y: 2, Z: 20}
			monster := MonsterState{
				ID:         "loot-test-monster",
				TemplateID: test.templateID,
				Name:       test.name,
				Position:   position,
				Health:     20,
				MaxHealth:  20,
				Alive:      true,
				Rotation:   PlayerRotation{W: 1},
			}
			if !server.UpsertMonster(monster) {
				t.Fatal("could not add test monster")
			}

			// Non-lethal damage must not produce loot.
			if hit, ok := server.applyMonsterDamage(monster.ID, 1); !ok || !hit.Alive {
				t.Fatalf("first hit unexpectedly killed monster: %+v, ok=%v", hit, ok)
			}
			server.entitiesMu.RLock()
			preKillDrops := len(server.groundItems)
			server.entitiesMu.RUnlock()
			if preKillDrops != 0 {
				t.Fatalf("monster dropped loot before death: %d ground items", preKillDrops)
			}

			dead, ok := server.applyMonsterDamage(monster.ID, 100)
			if !ok || dead.Alive {
				t.Fatalf("lethal hit did not kill monster: %+v, ok=%v", dead, ok)
			}
			if _, ok := server.applyMonsterDamage(monster.ID, 100); ok {
				t.Fatal("dead monster accepted damage again")
			}

			server.entitiesMu.RLock()
			drops := make([]GroundItemState, 0, len(server.groundItems))
			for _, item := range server.groundItems {
				drops = append(drops, item)
			}
			server.entitiesMu.RUnlock()

			if len(drops) != 1 {
				t.Fatalf("death created %d physical drops, want exactly one capsule: %+v", len(drops), drops)
			}
			drop := drops[0]
			if drop.ItemTemplate != test.wantItem || drop.Name != test.wantName || drop.Quantity != 10 {
				t.Fatalf("unexpected loot stack: %+v", drop)
			}
			if drop.SpawnPosition == nil {
				t.Fatal("loot has no launch position at monster head height")
			}

			wantHeadY := position.Y + colliderForMonster(monster).Height
			if math.Abs(drop.SpawnPosition.Y-wantHeadY) > 1e-9 {
				t.Fatalf("launch height = %.3f, want monster top at %.3f", drop.SpawnPosition.Y, wantHeadY)
			}
			dx := drop.Position.X - position.X
			dz := drop.Position.Z - position.Z
			distance := math.Hypot(dx, dz)
			boundaryDistance := colliderForMonster(monster).Radius
			if colliderForMonster(monster).Shape == ColliderBox && distance > 1e-9 {
				boundaryDistance = boxBoundaryDistanceAlongDirection(
					colliderForMonster(monster).Size, monster.Rotation, dx/distance, dz/distance)
			}
			scatterDistance := distance - boundaryDistance
			if scatterDistance < 0.15-1e-6 || scatterDistance > 1.0+1e-6 {
				t.Fatalf("landing point is %.3fm beyond collider boundary, want 0.15-1.0m (radial=%.3f boundary=%.3f)",
					scatterDistance, distance, boundaryDistance)
			}
			if math.Abs(drop.Position.Y-position.Y) > 1e-9 {
				t.Fatalf("landing Y = %.3f, want ground Y %.3f in test world", drop.Position.Y, position.Y)
			}
		})
	}
}

func TestGroundItemSpawnBroadcastsImmediatelyToInterestedClient(t *testing.T) {
	server := newItemTestServer()
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	server.playersMu.Lock()
	server.playerSessions["player1"] = serverConn
	server.playerStates["player1"] = PlayerState{
		PlayerID: "player1",
		Position: worldmap.Vec3{X: 10, Y: 2, Z: 20},
		Online:   true,
	}
	server.worldInterestRanges["player1"] = 50
	server.playersMu.Unlock()

	item := GroundItemState{
		ID:           "loot-event-test",
		ItemTemplate: "soulshot-c-grade",
		Name:         "Soulshot (C-grade)",
		Position:     worldmap.Vec3{X: 11, Y: 2, Z: 20},
		Quantity:     10,
	}
	writeDone := make(chan struct{})
	go func() {
		server.broadcastGroundItemSpawned(item)
		close(writeDone)
	}()

	if err := clientConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var got response
	if err := json.NewDecoder(clientConn).Decode(&got); err != nil {
		t.Fatalf("read ground item spawn event: %v", err)
	}
	<-writeDone

	if got.Type != "ground_item_spawned" {
		t.Fatalf("event type = %q, want ground_item_spawned", got.Type)
	}
	if got.Item == nil || got.Item.ID != item.ID ||
		got.Item.ItemTemplate != item.ItemTemplate || got.Item.Quantity != item.Quantity {
		t.Fatalf("unexpected ground item in spawn event: %+v", got.Item)
	}
}

func TestGroundItemIsNotPickedUpWithoutExplicitTarget(t *testing.T) {
	server := newItemTestServer()
	position := worldmap.Vec3{X: 10, Y: 2, Z: 20}
	addSoulshotForTest(t, server, "soulshot-1", position, 1)

	if _, _, pickedUp := server.completePendingPickup("player1", position); pickedUp {
		t.Fatal("walking within pickup radius picked up an item without an explicit target")
	}
	if _, exists := server.groundItems["soulshot-1"]; !exists {
		t.Fatal("ground item disappeared without a pickup command")
	}
	if inventory := server.inventorySnapshot("player1"); len(inventory) != 0 {
		t.Fatalf("inventory changed without a pickup command: %+v", inventory)
	}
}

func TestGroundItemPickupRequiresApproachWithinHalfMetre(t *testing.T) {
	server := newItemTestServer()
	position := worldmap.Vec3{X: 10, Y: 2, Z: 20}
	addSoulshotForTest(t, server, "soulshot-2", position, 1)

	if _, success, message := server.setPlayerTarget("player1", "item", "soulshot-2"); !success {
		t.Fatalf("could not set item pickup target: %s", message)
	}

	outsideRadius := worldmap.Vec3{X: position.X + ItemPickupRadius + 0.01, Y: position.Y, Z: position.Z}
	if _, _, pickedUp := server.completePendingPickup("player1", outsideRadius); pickedUp {
		t.Fatal("item was picked up while the player was outside the configured radius")
	}
	if _, exists := server.groundItems["soulshot-2"]; !exists {
		t.Fatal("ground item disappeared while the player was outside pickup radius")
	}

	// The accepted player position represents the character root, not ground level.
	// Keep a realistic vertical offset to ensure pickup uses horizontal ground distance.
	insideRadius := worldmap.Vec3{
		X: position.X + ItemPickupRadius - 0.01,
		Y: position.Y + 1.05,
		Z: position.Z,
	}
	item, inventory, pickedUp := server.completePendingPickup("player1", insideRadius)
	if !pickedUp {
		t.Fatal("item was not picked up after the commanded player reached within 0.5 m")
	}
	if item.ID != "soulshot-2" {
		t.Fatalf("picked up item ID = %q, want soulshot-2", item.ID)
	}
	if _, exists := server.groundItems["soulshot-2"]; exists {
		t.Fatal("picked-up item remains in the ground-item registry")
	}
	if len(inventory) != 1 || inventory[0].ItemTemplate != "soulshot-d-grade" ||
		inventory[0].Category != "consumable" || !inventory[0].Stackable || inventory[0].Quantity != 1 {
		t.Fatalf("unexpected inventory after pickup: %+v", inventory)
	}
	if _, _, pickedUp := server.completePendingPickup("player1", insideRadius); pickedUp {
		t.Fatal("same ground-item instance was picked up more than once")
	}
}

func TestSoulshotsStackIntoOneInventoryEntry(t *testing.T) {
	server := newItemTestServer()
	firstPosition := worldmap.Vec3{X: 10, Y: 2, Z: 20}
	secondPosition := worldmap.Vec3{X: 12, Y: 2, Z: 20}
	addSoulshotForTest(t, server, "soulshot-stack-a", firstPosition, 2)
	addSoulshotForTest(t, server, "soulshot-stack-b", secondPosition, 3)

	if _, success, message := server.setPlayerTarget("player1", "item", "soulshot-stack-a"); !success {
		t.Fatalf("could not set first pickup target: %s", message)
	}
	if _, _, pickedUp := server.completePendingPickup("player1", firstPosition); !pickedUp {
		t.Fatal("first soulshot stack was not picked up")
	}
	if _, success, message := server.setPlayerTarget("player1", "item", "soulshot-stack-b"); !success {
		t.Fatalf("could not set second pickup target: %s", message)
	}
	_, inventory, pickedUp := server.completePendingPickup("player1", secondPosition)
	if !pickedUp {
		t.Fatal("second soulshot stack was not picked up")
	}
	if len(inventory) != 1 {
		t.Fatalf("inventory has %d entries, want one stack: %+v", len(inventory), inventory)
	}
	if inventory[0].ItemTemplate != "soulshot-d-grade" || inventory[0].Quantity != 5 {
		t.Fatalf("inventory stack = %+v, want five D-grade soulshots", inventory[0])
	}
}

func TestClearingItemTargetCancelsPickupIntent(t *testing.T) {
	server := newItemTestServer()
	position := worldmap.Vec3{X: 10, Y: 2, Z: 20}
	addSoulshotForTest(t, server, "soulshot-cancel", position, 1)

	if _, success, message := server.setPlayerTarget("player1", "item", "soulshot-cancel"); !success {
		t.Fatalf("could not set item target: %s", message)
	}
	if _, success, message := server.setPlayerTarget("player1", "", ""); !success {
		t.Fatalf("could not clear target: %s", message)
	}
	if _, _, pickedUp := server.completePendingPickup("player1", position); pickedUp {
		t.Fatal("cleared pickup intent still picked up the item")
	}
	if _, exists := server.groundItems["soulshot-cancel"]; !exists {
		t.Fatal("item disappeared after pickup was cancelled")
	}
}

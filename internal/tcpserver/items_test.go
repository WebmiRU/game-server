package tcpserver

import (
	"io"
	"log"
	"testing"

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

	insideRadius := worldmap.Vec3{X: position.X + ItemPickupRadius - 0.01, Y: position.Y, Z: position.Z}
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

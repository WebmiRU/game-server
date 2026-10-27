package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"game-server/internal/tcpserver"
	"game-server/internal/worldmap"
)

// worldMapDirectory is the single fixed location of the Unity terrain export.
// The game-client and game-server repositories are expected to be sibling directories.
const worldMapDirectory = "../game-client/WorldExport/SampleScene/Terrain"

func main() {
	address := flag.String(
		"addr",
		envOrDefault("GAME_SERVER_ADDR", ":7777"),
		"TCP listen address",
	)
	flag.Parse()

	loadedWorld, err := worldmap.LoadDir(worldMapDirectory)
	if err != nil {
		log.Fatalf("load world map from %q: %v", worldMapDirectory, err)
	}
	log.Printf("world map loaded from %q", worldMapDirectory)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := tcpserver.New(tcpserver.Config{
		Address: *address,
		World:   loadedWorld,
		Logger:  log.Default(),
	})

	log.Printf("starting game server (protocol v%d)", tcpserver.ProtocolVersion)
	if err := server.ListenAndServe(ctx); err != nil {
		log.Fatalf("game server stopped with error: %v", err)
	}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

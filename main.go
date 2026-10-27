package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"os/signal"
	"syscall"

	"game-server/internal/tcpserver"
	"game-server/internal/worldmap"
)

func main() {
	address := flag.String(
		"addr",
		envOrDefault("GAME_SERVER_ADDR", ":7777"),
		"TCP listen address",
	)
	mapDir := flag.String(
		"map-dir",
		envOrDefault("GAME_WORLD_DIR", "data"),
		"directory containing GWM manifest.json and terrain.u16",
	)
	requireMap := flag.Bool(
		"require-map",
		false,
		"exit when the configured world map cannot be loaded",
	)
	flag.Parse()

	var loadedWorld *worldmap.Map
	loadedWorld, err := worldmap.LoadDir(*mapDir)
	if err != nil {
		_, manifestStatErr := os.Stat(filepath.Join(*mapDir, "manifest.json"))
		mapIsNotConfigured := errors.Is(manifestStatErr, fs.ErrNotExist)
		if mapIsNotConfigured && !*requireMap {
			log.Printf(
				"WARNING: no world export found in %q: %v; TCP authentication will still be available",
				*mapDir,
				err,
			)
		} else {
			log.Fatalf("load world map from %q: %v", *mapDir, err)
		}
	}

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

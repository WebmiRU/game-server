package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"game-server/internal/logging"
	"game-server/internal/tcpserver"
	"game-server/internal/worldmap"
)

// appLogger writes ordinary messages to stdout so GoLand does not mark every
// log entry as an error. Only logging.Errorf adds the red ANSI color sequence.
var appLogger = log.New(os.Stdout, "", log.LstdFlags)

func main() {
	address := flag.String(
		"addr",
		envOrDefault("GAME_SERVER_ADDR", ":7777"),
		"TCP listen address",
	)
	flag.Parse()

	mapDir, err := fixedWorldMapDirectory()
	if err != nil {
		fatalErrorf("resolve fixed world map directory: %v", err)
	}

	var loadedWorld *worldmap.Map
	loadedWorld, err = worldmap.LoadDir(mapDir)
	if err != nil {
		logging.Errorf(appLogger, "load world map from fixed project data directory %q: %v", mapDir, err)
		appLogger.Printf("starting without a world map; TCP authentication stays available, world queries are disabled")
	} else {
		appLogger.Printf("world map loaded from %q", mapDir)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := tcpserver.New(tcpserver.Config{
		Address: *address,
		World:   loadedWorld,
		Logger:  appLogger,
	})

	appLogger.Printf("starting game server (protocol v%d)", tcpserver.ProtocolVersion)
	// ListenAndServe blocks here and keeps the process alive until shutdown or a real listener error.
	if err := server.ListenAndServe(ctx); err != nil {
		fatalErrorf("game server stopped with error: %v", err)
	}
}

func fatalErrorf(format string, args ...any) {
	logging.Errorf(appLogger, format, args...)
	os.Exit(1)
}

// fixedWorldMapDirectory always resolves to data/ beside this project's main.go.
// It deliberately does not depend on GoLand's Working directory or executable location.
func fixedWorldMapDirectory() (string, error) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", os.ErrNotExist
	}

	projectDir := filepath.Dir(sourceFile)
	if !filepath.IsAbs(projectDir) {
		absoluteDir, err := filepath.Abs(projectDir)
		if err != nil {
			return "", err
		}
		projectDir = absoluteDir
	}

	return filepath.Join(projectDir, "data"), nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

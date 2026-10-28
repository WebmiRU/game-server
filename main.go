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
	"time"

	"game-server/internal/logging"
	"game-server/internal/tcpserver"
	"game-server/internal/worldmap"
)

// appLogger writes ordinary messages to stdout. Errors are written to stderr
// by logging.Errorf, so IDEs and shell pipelines can distinguish the streams.
var appLogger = log.New(os.Stdout, "", log.LstdFlags)

func main() {
	// The default Go logger writes to stderr, so explicitly redirect normal log
	// output to stdout as well.
	log.SetOutput(os.Stdout)

	address := flag.String(
		"addr",
		envOrDefault("GAME_SERVER_ADDR", ":7777"),
		"TCP listen address",
	)
	movementRetryTimeout := flag.Duration(
		"movement-retry-timeout", 5*time.Second,
		"How long a move can make no useful progress before the server cancels it (e.g. 5s or 10s)",
	)
	flag.Parse()

	mapDir, err := fixedWorldMapDirectory()
	if err != nil {
		fatalErrorf("resolve fixed world map directory: %v", err)
	}

	loadedWorld, err := worldmap.LoadDir(mapDir)
	if err != nil {
		fatalErrorf("load required world map from %q: %v", mapDir, err)
	}
	appLogger.Printf("world map loaded from %q", mapDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := tcpserver.New(tcpserver.Config{
		Address: *address,
		World:   loadedWorld,
		Logger:    appLogger,
		StateFile: filepath.Join(mapDir, "player_states.json"),
		MovementRetryTimeout: *movementRetryTimeout,
	})

    // Preview a blue D-grade soulshot near the default player spawn while inventory work is in progress.
    if y, ok := loadedWorld.HeightAtWorld(496.0, 500.0); ok {
        if !server.AddGroundItem(tcpserver.GroundItemState{
            ID: "demo-soulshot-d",
            ItemTemplate: "soulshot-d-grade",
            Name: "Soulshot (D-grade)",
            Position: worldmap.Vec3{X: 496.0, Y: y, Z: 500.0},
            Quantity: 1,
        }) {
            appLogger.Printf("WARNING: could not add demo D-grade soulshot to the world")
        }
    }

	appLogger.Printf("starting game server (protocol v%d)", tcpserver.ProtocolVersion)
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

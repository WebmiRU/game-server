# Game Server

Minimal Go TCP server for the Unity MMO prototype.

## Requirements

- The Go version declared in `go.mod` (standard library only).
- The Unity Terrain export in GWM v1 format.

## Run

The server has one fixed map location: the `data` directory beside `main.go` in the `game-server` project. The location is resolved from the compiled source path, so it does not depend on GoLand's Working directory or the temporary location of the generated executable.

Place these two Unity exporter files directly in `game-server/data/`:

- `manifest.json`
- `terrain.u16`

Then run the project normally from GoLand. No map path or map-related run arguments are required. If the map is missing or invalid, the server exits with an error rather than starting without a world.

The TCP listen address defaults to `:7777`; it can be changed with `-addr` or `GAME_SERVER_ADDR`.

## Export the world map

1. In the Unity client, open the scene containing the Terrain.
2. Run **Tools → Game Client → Export World for Go Server**.
3. Unity exports to `WorldExport/<scene>/<terrain>/`.
4. Copy `manifest.json` and `terrain.u16` from that export directly into this project's `data/` directory.

The loader validates the GWM version, dimensions, byte length, SHA-256 checksum, sample range, and metadata before exposing the heightmap. It also supports a bilinear world-height query.

## Development login accounts

Temporary prototype credentials are hardcoded in `internal/tcpserver/server.go`:

| Username | Password |
|---|---|
| `player1` | `player1` |
| `player2` | `player2` |
| `player3` | `player3` |

These are development credentials only. TCP traffic is currently unencrypted and the credentials are intentionally simple; do not expose this prototype login endpoint to an untrusted network. Add TLS and a proper account store before production use.

## TCP protocol v1

One JSON object per line, UTF-8, terminated by a newline. The connection stays open after successful authentication.

Authenticate first:

```json
{"type":"auth","username":"player1","password":"player1"}
```

Example success response:

```json
{"type":"auth_result","success":true,"message":"authenticated","protocolVersion":1,"playerId":"player1"}
```

After authentication the prototype supports:

- Ping: `{"type":"ping"}` → `{"type":"pong","success":true}`
- World metadata: `{"type":"world_info"}`
- Surface height: `{"type":"height_at","x":100,"z":200}`

Coordinates in `height_at` are Unity **world X/Z coordinates**. Height results are world-space Y values. A query outside the terrain returns `coordinates_outside_world`.

Before authentication, non-auth messages receive `authentication_required`. Invalid credentials are rejected; a connection is closed after five failed login attempts. Messages over 4096 bytes are rejected.

## GWM v1 format

See [GWM_FORMAT.md](https://github.com/WebmiRU/game-client/blob/master/GWM_FORMAT.md) in the Unity client repository for the export format definition. The server interprets each sample as an unsigned 16-bit little-endian normalized height and uses the manifest's origin and size to convert it to world coordinates.

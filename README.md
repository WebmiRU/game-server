# Game Server

Minimal Go TCP server for the Unity MMO prototype.

## Requirements

- The Go version declared in `go.mod` (standard library only).
- A Unity Terrain export in the GWM v1 format to enable world queries.

## Run

Start the server from the repository root:

```sh
go test ./...
go run . -addr :7777
```

The default listen address is `:7777`. Override it with `-addr` or `GAME_SERVER_ADDR`.

The server attempts to load a map from `data/world/starter` by default. If no export is present, it logs a warning and still starts so TCP authentication can be tested. To require a valid map at startup:

```sh
go run . -addr :7777 -map-dir data/world/starter -require-map
```

The map directory must contain the Unity exporter outputs `manifest.json` and `terrain.u16`. Override the path with `-map-dir` or `GAME_WORLD_DIR`. A corrupt or incompatible map is rejected.

## Export and install a world map

1. In the Unity client, open the scene containing the Terrain.
2. Run **Tools → Game Client → Export World for Go Server**.
3. Copy the resulting `manifest.json` and `terrain.u16` from `WorldExport/<scene>/<terrain>/` into this repository's `data/world/starter/` directory.
4. Run with `-require-map` so startup fails immediately if the map cannot be loaded.

For example, from the game-server repository root on Windows PowerShell, assuming `game-client` and `game-server` are sibling folders:

```powershell
New-Item -ItemType Directory -Force data\world\starter
Copy-Item ..\game-client\WorldExport\SampleScene\Terrain\manifest.json data\world\starter\
Copy-Item ..\game-client\WorldExport\SampleScene\Terrain\terrain.u16 data\world\starter\
go run . -addr :7777 -map-dir data\world\starter -require-map
```

Change `SampleScene\Terrain` if Unity exported under a different scene or Terrain name.

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

Coordinates in `height_at` are Unity **world X/Z coordinates**. Height results are world-space Y values. A query outside the terrain returns `coordinates_outside_world`. If no map is loaded, world queries return `world_map_not_loaded`.

Before authentication, non-auth messages receive `authentication_required`. Invalid credentials are rejected; a connection is closed after five failed login attempts. Messages over 4096 bytes are rejected.

## GWM v1 format

See [GWM_FORMAT.md](https://github.com/WebmiRU/game-client/blob/master/GWM_FORMAT.md) in the Unity client repository for the export format definition. The server interprets each sample as an unsigned 16-bit little-endian normalized height and uses the manifest's origin and size to convert it to world coordinates.

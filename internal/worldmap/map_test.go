package worldmap

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func writeFixture(t *testing.T, dir string) []byte {
	t.Helper()

	samples := []uint16{
		0, 16384, 32768,
		16384, 32768, 49151,
		32768, 49151, 65535,
	}
	payload := make([]byte, len(samples)*2)
	minimum, maximum := 65535, 0
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(payload[i*2:i*2+2], sample)
		value := int(sample)
		if value < minimum {
			minimum = value
		}
		if value > maximum {
			maximum = value
		}
	}
	sum := sha256.Sum256(payload)
	manifest := Manifest{
		Format:             FormatName,
		FormatVersion:      FormatVersion,
		SceneName:          "TestScene",
		TerrainName:        "Terrain",
		CoordinateSystem:   "Unity world coordinates; +Y is up; Terrain axes are unrotated (+X and +Z).",
		Origin:             Vec3{X: 100, Y: 10, Z: -20},
		Size:               Vec3{X: 2, Y: 100, Z: 2},
		WidthSamples:       3,
		HeightSamples:      3,
		SampleSpacing:      Vec2{X: 1, Z: 1},
		Encoding:           expectedEncoding,
		ByteOrder:          expectedByteOrder,
		RowOrder:           expectedRowOrder,
		FirstSample:        "(x=0, z=0)",
		HeightFormula:      "worldY = origin.y + (sample / 65535.0) * size.y",
		DataFile:           "terrain.u16",
		DataLengthBytes:    int64(len(payload)),
		SHA256:             hex.EncodeToString(sum[:]),
		MinimumSample:      minimum,
		MaximumSample:      maximum,
		MinimumHeightWorld: 10,
		MaximumHeightWorld: 110,
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "terrain.u16"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestLoadDirAndHeightAtWorld(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir)

	terrain, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir() error = %v", err)
	}

	tests := []struct {
		name string
		x, z float64
		want float64
	}{
		{name: "south-west corner", x: 100, z: -20, want: 10},
		{name: "north-east corner", x: 102, z: -18, want: 110},
		{name: "centre sample", x: 101, z: -19, want: 10 + (32768.0/65535.0)*100},
		{name: "halfway between four samples", x: 100.5, z: -19.5, want: 10 + (16384.0/65535.0)*100},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := terrain.HeightAtWorld(test.x, test.z)
			if !ok {
				t.Fatalf("HeightAtWorld(%v, %v) returned ok=false", test.x, test.z)
			}
			if math.Abs(got-test.want) > 1e-6 {
				t.Fatalf("HeightAtWorld(%v, %v) = %.9f, want %.9f", test.x, test.z, got, test.want)
			}
		})
	}

	if _, ok := terrain.HeightAtWorld(99.99, -19); ok {
		t.Fatal("HeightAtWorld returned a height outside the terrain")
	}
	if _, ok := terrain.HeightAtWorld(math.NaN(), -19); ok {
		t.Fatal("HeightAtWorld accepted NaN coordinates")
	}

	info := terrain.Summary()
	if info.WidthSamples != 3 || info.HeightSamples != 3 {
		t.Fatalf("unexpected summary dimensions: %+v", info)
	}
}

func TestLoadDirRejectsChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir)

	payloadPath := filepath.Join(dir, "terrain.u16")
	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)-1] ^= 0x01
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadDir(dir); err == nil {
		t.Fatal("LoadDir succeeded with a corrupt heightmap checksum")
	}
}

func TestLoadDirRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir)

	manifestPath := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.DataFile = "../outside.u16"
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadDir(dir); err == nil {
		t.Fatal("LoadDir accepted a dataFile path outside the world directory")
	}
}

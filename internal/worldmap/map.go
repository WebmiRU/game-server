package worldmap

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	FormatName    = "GWM"
	FormatVersion = 1
	maxSamples    = int64(100_000_000)
	maxSample     = 65535
)

const (
	expectedEncoding  = "uint16"
	expectedByteOrder = "little-endian"
	expectedRowOrder  = "z-ascending; x-ascending within each row"
)

// Vec2 describes spacing in world units along the X and Z axes.
type Vec2 struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
}

// Vec3 is a world-space position or size.
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Manifest is the GWM v1 metadata document written by the Unity exporter.
type Manifest struct {
	Format              string  `json:"format"`
	FormatVersion       int     `json:"formatVersion"`
	SceneName           string  `json:"sceneName"`
	TerrainName         string  `json:"terrainName"`
	CoordinateSystem    string  `json:"coordinateSystem"`
	Origin              Vec3    `json:"origin"`
	Size                Vec3    `json:"size"`
	WidthSamples        int     `json:"widthSamples"`
	HeightSamples       int     `json:"heightSamples"`
	SampleSpacing       Vec2    `json:"sampleSpacing"`
	Encoding            string  `json:"encoding"`
	ByteOrder           string  `json:"byteOrder"`
	RowOrder            string  `json:"rowOrder"`
	FirstSample         string  `json:"firstSample"`
	HeightFormula       string  `json:"heightFormula"`
	DataFile            string  `json:"dataFile"`
	DataLengthBytes     int64   `json:"dataLengthBytes"`
	SHA256              string  `json:"sha256"`
	MinimumSample       int     `json:"minimumSample"`
	MaximumSample       int     `json:"maximumSample"`
	MinimumHeightWorld  float64 `json:"minimumHeightWorld"`
	MaximumHeightWorld  float64 `json:"maximumHeightWorld"`
}

// Summary contains the non-binary world metadata returned by the TCP server.
type Summary struct {
	SceneName     string `json:"sceneName"`
	TerrainName   string `json:"terrainName"`
	Origin        Vec3   `json:"origin"`
	Size          Vec3   `json:"size"`
	WidthSamples  int    `json:"widthSamples"`
	HeightSamples int    `json:"heightSamples"`
	SampleSpacing Vec2   `json:"sampleSpacing"`
	SHA256        string `json:"sha256"`
}

// Map is an immutable, validated GWM heightmap in memory.
type Map struct {
	manifest Manifest
	heights  []uint16
}

// LoadDir reads manifest.json and its referenced binary heightmap from dir.
func LoadDir(dir string) (*Map, error) {
	manifestPath := filepath.Join(dir, "manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read world manifest %q: %w", manifestPath, err)
	}

	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("decode world manifest %q: %w", manifestPath, err)
	}
	if err := validateManifest(manifest); err != nil {
		return nil, fmt.Errorf("validate world manifest %q: %w", manifestPath, err)
	}

	// Do not allow a manifest to make the loader read files outside the configured world folder.
	if manifest.DataFile == "" ||
		filepath.IsAbs(manifest.DataFile) ||
		filepath.Base(manifest.DataFile) != manifest.DataFile ||
		strings.ContainsAny(manifest.DataFile, "/\\") ||
		manifest.DataFile == "." || manifest.DataFile == ".." {
		return nil, fmt.Errorf("invalid dataFile %q: expected a filename in the world folder", manifest.DataFile)
	}

	dataPath := filepath.Join(dir, manifest.DataFile)
	payload, err := os.ReadFile(dataPath)
	if err != nil {
		return nil, fmt.Errorf("read world heightmap %q: %w", dataPath, err)
	}

	expectedLength := int64(manifest.WidthSamples) * int64(manifest.HeightSamples) * 2
	if int64(len(payload)) != expectedLength {
		return nil, fmt.Errorf(
			"heightmap size mismatch: manifest expects %d bytes, got %d",
			expectedLength, len(payload),
		)
	}
	if manifest.DataLengthBytes != expectedLength {
		return nil, fmt.Errorf(
			"manifest dataLengthBytes mismatch: expects %d bytes from dimensions, reports %d",
			expectedLength, manifest.DataLengthBytes,
		)
	}

	sum := sha256.Sum256(payload)
	actualSHA := hex.EncodeToString(sum[:])
	if !strings.EqualFold(actualSHA, manifest.SHA256) {
		return nil, fmt.Errorf("heightmap SHA-256 mismatch: manifest=%s actual=%s", manifest.SHA256, actualSHA)
	}

	samples := make([]uint16, len(payload)/2)
	minimum, maximum := maxSample, 0
	for i := range samples {
		samples[i] = binary.LittleEndian.Uint16(payload[i*2 : i*2+2])
		value := int(samples[i])
		if value < minimum {
			minimum = value
		}
		if value > maximum {
			maximum = value
		}
	}
	if minimum != manifest.MinimumSample || maximum != manifest.MaximumSample {
		return nil, fmt.Errorf(
			"heightmap sample range mismatch: manifest says [%d,%d], actual [%d,%d]",
			manifest.MinimumSample, manifest.MaximumSample, minimum, maximum,
		)
	}

	return &Map{manifest: manifest, heights: samples}, nil
}

func validateManifest(m Manifest) error {
	if m.Format != FormatName {
		return fmt.Errorf("unsupported format %q; expected %q", m.Format, FormatName)
	}
	if m.FormatVersion != FormatVersion {
		return fmt.Errorf("unsupported formatVersion %d; supported version is %d", m.FormatVersion, FormatVersion)
	}
	if m.Encoding != expectedEncoding {
		return fmt.Errorf("unsupported encoding %q; expected %q", m.Encoding, expectedEncoding)
	}
	if m.ByteOrder != expectedByteOrder {
		return fmt.Errorf("unsupported byteOrder %q; expected %q", m.ByteOrder, expectedByteOrder)
	}
	if m.RowOrder != expectedRowOrder {
		return fmt.Errorf("unsupported rowOrder %q; expected %q", m.RowOrder, expectedRowOrder)
	}
	if m.WidthSamples < 2 || m.HeightSamples < 2 {
		return errors.New("widthSamples and heightSamples must both be at least 2")
	}
	sampleCount := int64(m.WidthSamples) * int64(m.HeightSamples)
	if sampleCount <= 0 || sampleCount > maxSamples {
		return fmt.Errorf("heightmap has too many samples (%d); limit is %d", sampleCount, maxSamples)
	}
	if !finite(m.Origin.X) || !finite(m.Origin.Y) || !finite(m.Origin.Z) {
		return errors.New("origin contains a non-finite number")
	}
	if !finite(m.Size.X) || !finite(m.Size.Y) || !finite(m.Size.Z) ||
		m.Size.X <= 0 || m.Size.Y <= 0 || m.Size.Z <= 0 {
		return errors.New("size must contain finite, positive X, Y and Z values")
	}
	if !finite(m.SampleSpacing.X) || !finite(m.SampleSpacing.Z) ||
		m.SampleSpacing.X <= 0 || m.SampleSpacing.Z <= 0 {
		return errors.New("sampleSpacing must contain finite, positive X and Z values")
	}

	expectedStepX := m.Size.X / float64(m.WidthSamples-1)
	expectedStepZ := m.Size.Z / float64(m.HeightSamples-1)
	if !approximatelyEqual(m.SampleSpacing.X, expectedStepX) ||
		!approximatelyEqual(m.SampleSpacing.Z, expectedStepZ) {
		return errors.New("sampleSpacing does not match size and sample dimensions")
	}

	expectedLength := sampleCount * 2
	if m.DataLengthBytes != expectedLength {
		return fmt.Errorf("dataLengthBytes must be %d, got %d", expectedLength, m.DataLengthBytes)
	}
	if len(m.SHA256) != sha256.Size*2 {
		return errors.New("sha256 must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(m.SHA256); err != nil {
		return fmt.Errorf("invalid sha256: %w", err)
	}
	if m.MinimumSample < 0 || m.MinimumSample > maxSample ||
		m.MaximumSample < 0 || m.MaximumSample > maxSample ||
		m.MinimumSample > m.MaximumSample {
		return errors.New("minimumSample/maximumSample are outside the uint16 range")
	}
	if !finite(m.MinimumHeightWorld) || !finite(m.MaximumHeightWorld) ||
		m.MinimumHeightWorld > m.MaximumHeightWorld {
		return errors.New("minimumHeightWorld/maximumHeightWorld are invalid")
	}
	return nil
}

// Summary returns a copy of the public map metadata.
func (m *Map) Summary() Summary {
	return Summary{
		SceneName:     m.manifest.SceneName,
		TerrainName:   m.manifest.TerrainName,
		Origin:        m.manifest.Origin,
		Size:          m.manifest.Size,
		WidthSamples:  m.manifest.WidthSamples,
		HeightSamples: m.manifest.HeightSamples,
		SampleSpacing: m.manifest.SampleSpacing,
		SHA256:        m.manifest.SHA256,
	}
}

// HeightAtWorld returns the bilinearly interpolated world-space Y height at X/Z.
// It returns ok=false when the coordinate is outside the terrain.
func (m *Map) HeightAtWorld(worldX, worldZ float64) (height float64, ok bool) {
	if m == nil || !finite(worldX) || !finite(worldZ) {
		return 0, false
	}

	origin := m.manifest.Origin
	size := m.manifest.Size
	const boundaryEpsilon = 1e-7

	if worldX < origin.X-boundaryEpsilon ||
		worldX > origin.X+size.X+boundaryEpsilon ||
		worldZ < origin.Z-boundaryEpsilon ||
		worldZ > origin.Z+size.Z+boundaryEpsilon {
		return 0, false
	}

	gridX := (worldX-origin.X)/size.X*float64(m.manifest.WidthSamples-1)
	gridZ := (worldZ-origin.Z)/size.Z*float64(m.manifest.HeightSamples-1)
	gridX = math.Max(0, math.Min(gridX, float64(m.manifest.WidthSamples-1)))
	gridZ = math.Max(0, math.Min(gridZ, float64(m.manifest.HeightSamples-1)))

	x0 := int(math.Floor(gridX))
	z0 := int(math.Floor(gridZ))
	x1 := min(x0+1, m.manifest.WidthSamples-1)
	z1 := min(z0+1, m.manifest.HeightSamples-1)
	tx := gridX - float64(x0)
	tz := gridZ - float64(z0)

	h00 := m.heightAtSample(x0, z0)
	h10 := m.heightAtSample(x1, z0)
	h01 := m.heightAtSample(x0, z1)
	h11 := m.heightAtSample(x1, z1)

	h0 := h00 + (h10-h00)*tx
	h1 := h01 + (h11-h01)*tx
	return h0 + (h1-h0)*tz, true
}

func (m *Map) heightAtSample(x, z int) float64 {
	index := z*m.manifest.WidthSamples + x
	normalized := float64(m.heights[index]) / float64(maxSample)
	return m.manifest.Origin.Y + normalized*m.manifest.Size.Y
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func approximatelyEqual(a, b float64) bool {
	difference := math.Abs(a - b)
	tolerance := math.Max(1e-6, math.Max(math.Abs(a), math.Abs(b))*1e-5)
	return difference <= tolerance
}

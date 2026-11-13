package tagging

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
)

func TestBuildClusterKey(t *testing.T) {
	tests := []struct {
		name     string
		exif     *FileEXIF
		modTime  time.Time
		expected string
	}{
		{
			name: "device present, GPS present",
			exif: &FileEXIF{
				Model:            "Canon EOS R5",
				DateTimeOriginal: time.Date(2024, 5, 6, 12, 0, 0, 0, time.UTC),
				Latitude:         12.3456,
				Longitude:        56.7890,
				HasGPS:           true,
			},
			modTime:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			expected: "Canon EOS R5|12.35,56.79|2024-05-06",
		},
		{
			name: "device absent, GPS present",
			exif: &FileEXIF{
				Model:            "",
				DateTimeOriginal: time.Date(2024, 5, 6, 12, 0, 0, 0, time.UTC),
				Latitude:         12.3456,
				Longitude:        56.7890,
				HasGPS:           true,
			},
			modTime:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			expected: "unknown|12.35,56.79|2024-05-06",
		},
		{
			name: "device present, GPS absent",
			exif: &FileEXIF{
				Model:            "Canon EOS R5",
				DateTimeOriginal: time.Date(2024, 5, 6, 12, 0, 0, 0, time.UTC),
				Latitude:         0,
				Longitude:        0,
				HasGPS:           false,
			},
			modTime:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			expected: "Canon EOS R5|no-gps|2024-05-06",
		},
		{
			name: "device absent, GPS absent, date from EXIF",
			exif: &FileEXIF{
				Model:            "",
				DateTimeOriginal: time.Date(2024, 5, 6, 12, 0, 0, 0, time.UTC),
				Latitude:         0,
				Longitude:        0,
				HasGPS:           false,
			},
			modTime:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			expected: "unknown|no-gps|2024-05-06",
		},
		{
			name: "device absent, GPS absent, date from mtime",
			exif: &FileEXIF{
				Model:            "",
				DateTimeOriginal: time.Time{},
				Latitude:         0,
				Longitude:        0,
				HasGPS:           false,
			},
			modTime:  time.Date(2023, 3, 4, 12, 0, 0, 0, time.UTC),
			expected: "unknown|no-gps|2023-03-04",
		},
		{
			name: "GPS 2dp bucket rounding",
			exif: &FileEXIF{
				Model:            "Test",
				DateTimeOriginal: time.Date(2024, 5, 6, 12, 0, 0, 0, time.UTC),
				Latitude:         12.344,
				Longitude:        56.784,
				HasGPS:           true,
			},
			modTime:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			expected: "Test|12.34,56.78|2024-05-06",
		},
		{
			name: "GPS 2dp bucket rounding (banker's)",
			exif: &FileEXIF{
				Model:            "Test",
				DateTimeOriginal: time.Date(2024, 5, 6, 12, 0, 0, 0, time.UTC),
				Latitude:         12.345,
				Longitude:        56.785,
				HasGPS:           true,
			},
			modTime:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			expected: "Test|12.35,56.78|2024-05-06",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildClusterKey(tt.exif, tt.modTime)
			if result != tt.expected {
				t.Errorf("BuildClusterKey() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestParseClusterKey(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		expected ClusterKeyComponents
		ok       bool
	}{
		{
			name: "valid key",
			key:  "Canon EOS R5|12.35,56.79|2024-05-06",
			expected: ClusterKeyComponents{
				DeviceModel: "Canon EOS R5",
				GPSBucket:   "12.35,56.79",
				Date:        "2024-05-06",
			},
			ok: true,
		},
		{
			name: "valid key with no-gps",
			key:  "unknown|no-gps|2023-03-04",
			expected: ClusterKeyComponents{
				DeviceModel: "unknown",
				GPSBucket:   "no-gps",
				Date:        "2023-03-04",
			},
			ok: true,
		},
		{
			name: "invalid key - too few parts",
			key:  "Canon EOS R5|12.35,56.79",
			expected: ClusterKeyComponents{},
			ok:   false,
		},
		{
			name: "invalid key - too many parts",
			key:  "Canon EOS R5|12.35,56.79|2024-05-06|extra",
			expected: ClusterKeyComponents{},
			ok:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := ParseClusterKey(tt.key)
			if ok != tt.ok {
				t.Errorf("ParseClusterKey() ok = %v, want %v", ok, tt.ok)
			}
			if ok {
				if result.DeviceModel != tt.expected.DeviceModel {
					t.Errorf("DeviceModel = %q, want %q", result.DeviceModel, tt.expected.DeviceModel)
				}
				if result.GPSBucket != tt.expected.GPSBucket {
					t.Errorf("GPSBucket = %q, want %q", result.GPSBucket, tt.expected.GPSBucket)
				}
				if result.Date != tt.expected.Date {
					t.Errorf("Date = %q, want %q", result.Date, tt.expected.Date)
				}
			}
		})
	}
}

func TestIsMediaFile(t *testing.T) {
	tests := []struct {
		name     string
		relPath  string
		expected bool
	}{
		{"jpg lowercase", "photo.jpg", true},
		{"jpeg lowercase", "photo.jpeg", true},
		{"png lowercase", "photo.png", true},
		{"heic lowercase", "photo.heic", true},
		{"mp4 lowercase", "video.mp4", true},
		{"mov lowercase", "video.mov", true},
		{"mkv lowercase", "video.mkv", true},
		{"avi lowercase", "video.avi", true},
		{"webm lowercase", "video.webm", true},
		{"JPG uppercase", "photo.JPG", true},
		{"PNG uppercase", "photo.PNG", true},
		{"MP4 uppercase", "video.MP4", true},
		{"in subdir", "2024/05/photo.jpg", true},
		{"not media", "document.pdf", false},
		{"no extension", "photo", false},
		{"dotfile", ".hidden.jpg", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsMediaFile(tt.relPath)
			if result != tt.expected {
				t.Errorf("IsMediaFile(%q) = %v, want %v", tt.relPath, result, tt.expected)
			}
		})
	}
}

func TestShouldSkipPath(t *testing.T) {
	tests := []struct {
		name     string
		relPath  string
		expected bool
	}{
		{"dotfile at root", ".hidden", true},
		{"dotdir", ".hidden/photo.jpg", true},
		{"dotdir in path", "photos/.hidden/photo.jpg", true},
		{"kelvios-shadow dir", ".kelvios-shadow/photo.jpg", true},
		{"kelvios-shadow at root", ".kelvios-shadow", true},
		{"kelvios-shadow nested", "photos/.kelvios-shadow/photo.jpg", true},
		{"normal file", "photo.jpg", false},
		{"normal in subdir", "2024/05/photo.jpg", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ShouldSkipPath(tt.relPath)
			if result != tt.expected {
				t.Errorf("ShouldSkipPath(%q) = %v, want %v", tt.relPath, result, tt.expected)
			}
		})
	}
}

func TestRuleTagger_TagFile(t *testing.T) {
	tagger := NewRuleTagger(0.9)
	captureDate := time.Date(2024, 5, 6, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name           string
		exif           *FileEXIF
		expectedTags   []string
		expectedCount  int
		checkConfidence float64
	}{
		{
			name: "device tag only when EXIF Model present",
			exif: &FileEXIF{
				Model:            "Canon EOS R5",
				DateTimeOriginal: captureDate,
				Latitude:         0,
				Longitude:        0,
				HasGPS:           false,
			},
			expectedTags:    []string{"device:Canon_EOS_R5", "month:2024-05"},
			expectedCount:   2,
			checkConfidence: 0.9,
		},
		{
			name: "no device tag when EXIF Model absent",
			exif: &FileEXIF{
				Model:            "",
				DateTimeOriginal: captureDate,
				Latitude:         0,
				Longitude:        0,
				HasGPS:           false,
			},
			expectedTags:    []string{"month:2024-05"},
			expectedCount:   1,
			checkConfidence: 0.9,
		},
		{
			name: "place tag only when GPS present",
			exif: &FileEXIF{
				Model:            "Canon EOS R5",
				DateTimeOriginal: captureDate,
				Latitude:         12.3456,
				Longitude:        56.7890,
				HasGPS:           true,
			},
			expectedTags:    []string{"device:Canon_EOS_R5", "place:12.35,56.79", "month:2024-05"},
			expectedCount:   3,
			checkConfidence: 0.9,
		},
		{
			name: "no place tag when GPS absent",
			exif: &FileEXIF{
				Model:            "Canon EOS R5",
				DateTimeOriginal: captureDate,
				Latitude:         0,
				Longitude:        0,
				HasGPS:           false,
			},
			expectedTags:    []string{"device:Canon_EOS_R5", "month:2024-05"},
			expectedCount:   2,
			checkConfidence: 0.9,
		},
		{
			name: "month tag always from capture date",
			exif: &FileEXIF{
				Model:            "",
				DateTimeOriginal: time.Date(2023, 12, 25, 12, 0, 0, 0, time.UTC),
				Latitude:         0,
				Longitude:        0,
				HasGPS:           false,
			},
			expectedTags:    []string{"month:2023-12"},
			expectedCount:   1,
			checkConfidence: 0.9,
		},
		{
			name: "month tag from mtime when no EXIF date",
			exif: &FileEXIF{
				Model:            "",
				DateTimeOriginal: time.Time{},
				Latitude:         0,
				Longitude:        0,
				HasGPS:           false,
			},
			expectedTags:    []string{"month:2024-05"},
			expectedCount:   1,
			checkConfidence: 0.9,
		},
		{
			name: "confidence is 0.9",
			exif: &FileEXIF{
				Model:            "Test",
				DateTimeOriginal: captureDate,
				Latitude:         12.34,
				Longitude:        56.78,
				HasGPS:           true,
			},
			expectedTags:    []string{"device:Test", "place:12.34,56.78", "month:2024-05"},
			expectedCount:   3,
			checkConfidence: 0.9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Determine capture date like the pipeline does
			capDate := captureDate
			if !tt.exif.DateTimeOriginal.IsZero() {
				capDate = tt.exif.DateTimeOriginal
			}
			tags := tagger.TagFile(tt.exif, capDate)
			if len(tags) != tt.expectedCount {
				t.Errorf("TagFile() returned %d tags, want %d: %v", len(tags), tt.expectedCount, tags)
			}
			for i, tag := range tags {
				if i < len(tt.expectedTags) && tag.Tag != tt.expectedTags[i] {
					t.Errorf("TagFile()[%d].Tag = %q, want %q", i, tag.Tag, tt.expectedTags[i])
				}
				if tag.Confidence != tt.checkConfidence {
					t.Errorf("TagFile()[%d].Confidence = %v, want %v", i, tag.Confidence, tt.checkConfidence)
				}
				if tag.Origin != "rule" {
					t.Errorf("TagFile()[%d].Origin = %q, want \"rule\"", i, tag.Origin)
				}
				if tag.Source != "individual" {
					t.Errorf("TagFile()[%d].Source = %q, want \"individual\"", i, tag.Source)
				}
			}
		})
	}
}

func TestSanitizeTagValue(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Canon EOS R5", "Canon_EOS_R5"},
		{"Canon/EOS", "Canon_EOS"},
		{"Canon\\EOS", "Canon_EOS"},
		{"Canon:EOS", "Canon_EOS"},
		{"Canon,EOS", "Canon_EOS"},
		{"Simple", "Simple"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := sanitizeTagValue(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeTagValue(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestFmtPlaceTag(t *testing.T) {
	tests := []struct {
		name     string
		lat, lon float64
		expected string
	}{
		{"positive coords", 12.3456, 56.7890, "place:12.35,56.79"},
		{"negative coords", -12.3456, -56.7890, "place:-12.35,-56.79"},
		{"zero coords", 0, 0, "place:0.00,0.00"},
		{"rounding (banker's)", 12.345, 56.785, "place:12.35,56.78"},
		{"rounding down", 12.344, 56.784, "place:12.34,56.78"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := fmtPlaceTag(tt.lat, tt.lon)
			if result != tt.expected {
				t.Errorf("fmtPlaceTag(%v, %v) = %q, want %q", tt.lat, tt.lon, result, tt.expected)
			}
		})
	}
}

func TestPipeline_ClusterFiles_SampleSelection(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tmpDir := t.TempDir()
	collRoot := filepath.Join(tmpDir, "photos")
	if err := os.MkdirAll(collRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create test files with different mtimes
	mtime1 := time.Date(2024, 5, 6, 12, 0, 0, 0, time.UTC)
	mtime2 := time.Date(2024, 5, 6, 13, 0, 0, 0, time.UTC)
	mtime3 := time.Date(2024, 5, 6, 11, 0, 0, 0, time.UTC)

	writeTestFile(t, collRoot, "a.jpg", "content-a", mtime1)
	writeTestFile(t, collRoot, "b.jpg", "content-b", mtime2)
	writeTestFile(t, collRoot, "c.jpg", "content-c", mtime3)

	// Insert into catalog
	now := time.Now().Unix()
	_, err = db.Exec(`
		INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at)
		VALUES ('pool-1', 'warm', 'local', 'warm', 'single', 'single', ?, 1000000000, 100000000, 0, '', 'active', ?, ?)
	`, collRoot, now, now)
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, created_at, updated_at)
		VALUES ('coll-1', 'photos', 'pool-1', 'photos', '', 0, ?, ?)
	`, now, now)
	if err != nil {
		t.Fatalf("insert collection: %v", err)
	}

	files := []catalog.FileRow{
		{ID: "f1", CollectionID: "coll-1", RelPath: "a.jpg", SizeBytes: 100, ModTimeUnix: mtime1.Unix(), DiskName: "", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{ID: "f2", CollectionID: "coll-1", RelPath: "b.jpg", SizeBytes: 100, ModTimeUnix: mtime2.Unix(), DiskName: "", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{ID: "f3", CollectionID: "coll-1", RelPath: "c.jpg", SizeBytes: 100, ModTimeUnix: mtime3.Unix(), DiskName: "", CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}

	pipeline := NewPipeline(db, "coll-1", collRoot, DefaultPipelineConfig())
	clusters, members := pipeline.clusterFiles(files)

	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}

	// Sample should be earliest by mtime (c.jpg at 11:00)
	if clusters[0].SampleRelPath != "c.jpg" {
		t.Errorf("sample = %q, want \"c.jpg\" (earliest mtime)", clusters[0].SampleRelPath)
	}

	// All three should be members
	memberPaths := members[clusters[0].ClusterKey]
	if len(memberPaths) != 3 {
		t.Errorf("expected 3 members, got %d: %v", len(memberPaths), memberPaths)
	}
}

func TestPipeline_PersistTags_Idempotency(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tmpDir := t.TempDir()
	collRoot := filepath.Join(tmpDir, "photos")
	if err := os.MkdirAll(collRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	now := time.Now().Unix()
	_, err = db.Exec(`
		INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at)
		VALUES ('pool-1', 'warm', 'local', 'warm', 'single', 'single', ?, 1000000000, 100000000, 0, '', 'active', ?, ?)
	`, collRoot, now, now)
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, created_at, updated_at)
		VALUES ('coll-1', 'photos', 'pool-1', 'photos', '', 0, ?, ?)
	`, now, now)
	if err != nil {
		t.Fatalf("insert collection: %v", err)
	}

	pipeline := NewPipeline(db, "coll-1", collRoot, DefaultPipelineConfig())

	tags := []MediaTag{
		{Tag: "device:Test", Origin: "rule", Confidence: 0.9, Source: "sample", CreatedAt: time.Now()},
		{Tag: "month:2024-05", Origin: "rule", Confidence: 0.9, Source: "sample", CreatedAt: time.Now()},
	}

	// First insert
	count1, err := pipeline.persistTags("test.jpg", tags)
	if err != nil {
		t.Fatalf("first persistTags: %v", err)
	}
	if count1 != 2 {
		t.Errorf("first insert count = %d, want 2", count1)
	}

	// Second identical insert should be ignored and count 0
	count2, err := pipeline.persistTags("test.jpg", tags)
	if err != nil {
		t.Fatalf("second persistTags: %v", err)
	}
	if count2 != 0 {
		t.Errorf("second insert count = %d, want 0 (idempotent)", count2)
	}

	// Verify only 2 tags in DB
	var total int
	err = db.QueryRow(`SELECT COUNT(*) FROM media_tags WHERE collection_id = ? AND rel_path = ?`, "coll-1", "test.jpg").Scan(&total)
	if err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if total != 2 {
		t.Errorf("total tags in DB = %d, want 2", total)
	}
}

func TestSidecar_XMPEscaping(t *testing.T) {
	writer := NewSidecarWriter()
	tmpDir := t.TempDir()

	tests := []struct {
		name     string
		tags     []MediaTag
		check    func(t *testing.T, content string)
	}{
		{
			name: "ampersand escaped",
			tags: []MediaTag{{Tag: "tag:with&ampersand", Origin: "rule", Confidence: 0.9, Source: "sample"}},
			check: func(t *testing.T, content string) {
				if !strings.Contains(content, "&") {
					t.Errorf("XMP missing & escape: %s", content)
				}
				if strings.Contains(content, "with&ampersand") && !strings.Contains(content, "with&amp;ersand") {
					t.Errorf("ampersand not properly escaped in: %s", content)
				}
			},
		},
		{
			name: "less-than escaped",
			tags: []MediaTag{{Tag: "tag:<script>", Origin: "rule", Confidence: 0.9, Source: "sample"}},
			check: func(t *testing.T, content string) {
				if !strings.Contains(content, "<") {
					t.Errorf("XMP missing < escape: %s", content)
				}
			},
		},
		{
			name: "greater-than escaped",
			tags: []MediaTag{{Tag: "tag:>", Origin: "rule", Confidence: 0.9, Source: "sample"}},
			check: func(t *testing.T, content string) {
				if !strings.Contains(content, ">") {
					t.Errorf("XMP missing > escape: %s", content)
				}
			},
		},
		{
			name: "quote escaped",
			tags: []MediaTag{{Tag: `tag:"quoted"`, Origin: "rule", Confidence: 0.9, Source: "sample"}},
			check: func(t *testing.T, content string) {
				if !strings.Contains(content, "\"") {
					t.Errorf("XMP missing quote escape: %s", content)
				}
			},
		},
		{
			name: "all special chars",
			tags: []MediaTag{{Tag: `tag:&<>"`, Origin: "rule", Confidence: 0.9, Source: "sample"}},
			check: func(t *testing.T, content string) {
				if !strings.Contains(content, "&") || !strings.Contains(content, "<") || !strings.Contains(content, ">") || !strings.Contains(content, "\"") {
					t.Errorf("XMP missing escapes: %s", content)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mediaPath := filepath.Join(tmpDir, "test.jpg")
			if err := os.WriteFile(mediaPath, []byte("fake jpeg"), 0o644); err != nil {
				t.Fatalf("write media: %v", err)
			}

			err := writer.WritePhotoSidecar(mediaPath, tt.tags)
			if err != nil {
				t.Fatalf("WritePhotoSidecar: %v", err)
			}

			xmpPath := mediaPath + ".xmp"
			content, err := os.ReadFile(xmpPath)
			if err != nil {
				t.Fatalf("read xmp: %v", err)
			}

			tt.check(t, string(content))
		})
	}
}

func TestSidecar_JSONShape(t *testing.T) {
	writer := NewSidecarWriter()
	tmpDir := t.TempDir()

	mediaPath := filepath.Join(tmpDir, "test.mp4")
	if err := os.WriteFile(mediaPath, []byte("fake video"), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}

	tags := []MediaTag{
		{Tag: "device:Test", Origin: "rule", Confidence: 0.9, Source: "sample", CreatedAt: time.Now()},
		{Tag: "month:2024-05", Origin: "rule", Confidence: 0.9, Source: "sample", CreatedAt: time.Now()},
	}

	err := writer.WriteVideoSidecar(mediaPath, tags)
	if err != nil {
		t.Fatalf("WriteVideoSidecar: %v", err)
	}

	jsonPath := mediaPath + ".json"
	content, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read json: %v", err)
	}

	var data struct {
		Tags      []struct {
			Tag        string  `json:"tag"`
			Confidence float64 `json:"confidence"`
			Origin     string  `json:"origin"`
		} `json:"tags"`
		UpdatedAt string `json:"updated_at"`
	}

	if err := json.Unmarshal(content, &data); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}

	if len(data.Tags) != 2 {
		t.Errorf("expected 2 tags in JSON, got %d", len(data.Tags))
	}

	for _, tag := range data.Tags {
		if tag.Tag == "" {
			t.Error("tag field empty")
		}
		if tag.Confidence <= 0 {
			t.Errorf("confidence not positive: %v", tag.Confidence)
		}
		if tag.Origin == "" {
			t.Error("origin field empty")
		}
	}

	if data.UpdatedAt == "" {
		t.Error("updated_at field empty")
	}
	// Verify RFC3339 format
	if _, err := time.Parse(time.RFC3339, data.UpdatedAt); err != nil {
		t.Errorf("updated_at not RFC3339: %v", err)
	}
}

func TestPipeline_PropagationMath(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tmpDir := t.TempDir()
	collRoot := filepath.Join(tmpDir, "photos")
	if err := os.MkdirAll(collRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	now := time.Now().Unix()
	_, err = db.Exec(`
		INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at)
		VALUES ('pool-1', 'warm', 'local', 'warm', 'single', 'single', ?, 1000000000, 100000000, 0, '', 'active', ?, ?)
	`, collRoot, now, now)
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, created_at, updated_at)
		VALUES ('coll-1', 'photos', 'pool-1', 'photos', '', 0, ?, ?)
	`, now, now)
	if err != nil {
		t.Fatalf("insert collection: %v", err)
	}

	// Test with default config: multiplier=0.8, floor=0.5
	config := DefaultPipelineConfig()

	// Rule tag confidence = 0.9
	// Propagated = 0.9 * 0.8 = 0.72 (above floor 0.5)
	propConfidence := 0.9 * config.PropagationMultiplier
	if propConfidence < config.PropagationFloor {
		propConfidence = config.PropagationFloor
	}
	if math.Abs(propConfidence-0.72) > 1e-10 {
		t.Errorf("propagated confidence = %v, want ~0.72", propConfidence)
	}

	// Test floor: if rule confidence was 0.5, propagated = 0.5 * 0.8 = 0.4 -> floor to 0.5
	lowConfidence := 0.5 * config.PropagationMultiplier
	if lowConfidence < config.PropagationFloor {
		lowConfidence = config.PropagationFloor
	}
	if math.Abs(lowConfidence-0.5) > 1e-10 {
		t.Errorf("floor confidence = %v, want ~0.5", lowConfidence)
	}
}

func writeTestFile(t *testing.T, root, rel, content string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("chtime %s: %v", rel, err)
		}
	}
}
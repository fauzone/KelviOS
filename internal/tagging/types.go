package tagging

import (
	"time"
)

// TagCluster represents a cluster of media files with similar characteristics.
type TagCluster struct {
	ID             string    `json:"id"`
	CollectionID   string    `json:"collection_id"`
	ClusterKey     string    `json:"cluster_key"`
	SampleRelPath  string    `json:"sample_rel_path"`
	CreatedAt      time.Time `json:"created_at"`
}

// TagClusterMember represents a file belonging to a cluster.
type TagClusterMember struct {
	ClusterID string `json:"cluster_id"`
	RelPath   string `json:"rel_path"`
}

// MediaTag represents a tag applied to a media file.
// Origin = the tag's producer ∈ {"model", "rule", "manual"}.
// Source = the tag's role ∈ {"sample", "cluster", "individual"}.
//   - Sample's own tags: origin="rule" (or "model"), source="sample"
//   - Propagated tags: origin="rule" (inherited from producer), source="cluster"
//   - Manual tags: origin="manual", source="individual"
type MediaTag struct {
	ID           string    `json:"id"`
	CollectionID string    `json:"collection_id"`
	RelPath      string    `json:"rel_path"`
	Tag          string    `json:"tag"`
	Origin       string    `json:"origin"`       // "model" | "rule" | "manual"
	Confidence   float64   `json:"confidence"`   // 0.0-1.0
	Source       string    `json:"source"`       // "sample" | "cluster" | "individual"
	CreatedAt    time.Time `json:"created_at"`
}

// FileEXIF holds EXIF data needed for clustering and rule tagging.
type FileEXIF struct {
	Model            string
	DateTimeOriginal time.Time
	Latitude         float64
	Longitude        float64
	HasGPS           bool
}

// ClusterKeyComponents holds the components used to build a cluster key.
type ClusterKeyComponents struct {
	DeviceModel string
	GPSBucket   string // "lat,lon" at 2dp or "no-gps"
	Date        string // YYYY-MM-DD
}

// Engine interfaces for Phase 3 LLM registry (no implementations in this workstream).

// Embedder generates vector embeddings for media files.
type Embedder interface {
	// EmbedImage returns an embedding vector for an image file.
	EmbedImage(path string) ([]float32, error)
	// EmbedVideo returns an embedding vector for a video file (keyframe-based).
	EmbedVideo(path string) ([]float32, error)
	// Dim returns the embedding dimension.
	Dim() int
}

// VisionTagger generates descriptive tags for media files using vision-LLM.
type VisionTagger interface {
	// TagImage returns tags for an image file.
	TagImage(path string) ([]TagResult, error)
	// TagVideo returns tags for a video file (keyframe-based).
	TagVideo(path string) ([]TagResult, error)
}

// TagResult is a single tag with confidence from a vision tagger.
type TagResult struct {
	Tag       string  `json:"tag"`
	Confidence float64 `json:"confidence"`
}

// PipelineConfig holds configuration for the tagging pipeline.
type PipelineConfig struct {
	// RuleTagConfidence is the confidence for rule-generated tags (default 0.9).
	RuleTagConfidence float64
	// PropagationMultiplier is the multiplier for propagated tag confidence (default 0.8).
	PropagationMultiplier float64
	// PropagationFloor is the minimum confidence for propagated tags (default 0.5).
	PropagationFloor float64
}

// DefaultPipelineConfig returns the default pipeline configuration.
func DefaultPipelineConfig() PipelineConfig {
	return PipelineConfig{
		RuleTagConfidence:     0.9,
		PropagationMultiplier: 0.8,
		PropagationFloor:      0.5,
	}
}
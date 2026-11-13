package tagging

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
)

// Pipeline orchestrates the tagging process: clustering, rule tagging, propagation, sidecars.
type Pipeline struct {
	db            *sql.DB
	collectionID  string
	collectionRoot string
	config        PipelineConfig
	ruleTagger    *RuleTagger
	sidecarWriter *SidecarWriter
	embedder      Embedder
	visionTagger  VisionTagger
	mu            sync.Mutex
}

// PipelineResult holds the results of a tagging run.
type PipelineResult struct {
	Clusters        int `json:"clusters"`
	SamplesTagged   int `json:"samples_tagged"`
	TagsCreated     int `json:"tags_created"`
	Propagated      int `json:"propagated"`
	SidecarsWritten int `json:"sidecars_written"`
	SidecarErrors   int `json:"sidecar_errors"`
}

// NewPipeline creates a new tagging pipeline for a collection.
func NewPipeline(db *sql.DB, collectionID, collectionRoot string, config PipelineConfig) *Pipeline {
	if config.RuleTagConfidence == 0 {
		config = DefaultPipelineConfig()
	}
	return &Pipeline{
		db:             db,
		collectionID:   collectionID,
		collectionRoot: collectionRoot,
		config:         config,
		ruleTagger:     NewRuleTagger(config.RuleTagConfidence),
		sidecarWriter:  NewSidecarWriter(),
	}
}

// SetEmbedder registers an embedder (Phase 3). Nil means not available.
func (p *Pipeline) SetEmbedder(e Embedder) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.embedder = e
}

// SetVisionTagger registers a vision tagger (Phase 3). Nil means not available.
func (p *Pipeline) SetVisionTagger(v VisionTagger) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.visionTagger = v
}

// Run executes the full tagging pipeline for the collection.
// Returns result counts and error if a critical failure occurs.
func (p *Pipeline) Run(ctx context.Context) (PipelineResult, error) {
	var result PipelineResult

	// 1. Fetch all media files from catalog
	files, err := p.fetchMediaFiles()
	if err != nil {
		return result, fmt.Errorf("fetch media files: %w", err)
	}
	if len(files) == 0 {
		return result, nil // nothing to do
	}

	// 2. Cluster files
	clusters, clusterMembers := p.clusterFiles(files)
	result.Clusters = len(clusters)

	// 3. For each cluster, tag the sample and propagate
	for _, cluster := range clusters {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}

		members := clusterMembers[cluster.ClusterKey]
		sampleTagged, tagsCreated, propagated, sidecarsWritten, sidecarErrors, err := p.processCluster(cluster, files, members)
		if err != nil {
			slog.Error("process cluster failed", "cluster_key", cluster.ClusterKey, "error", err)
			continue // best-effort: continue with other clusters
		}
		result.SamplesTagged += sampleTagged
		result.TagsCreated += tagsCreated
		result.Propagated += propagated
		result.SidecarsWritten += sidecarsWritten
		result.SidecarErrors += sidecarErrors
	}

	return result, nil
}

// fetchMediaFiles returns all media files in the collection with their metadata.
func (p *Pipeline) fetchMediaFiles() ([]catalog.FileRow, error) {
	rows, err := p.db.Query(`
		SELECT id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, created_at, updated_at
		FROM files
		WHERE collection_id = ?
		ORDER BY rel_path
	`, p.collectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []catalog.FileRow
	for rows.Next() {
		var f catalog.FileRow
		var createdAt, updatedAt int64
		if err := rows.Scan(&f.ID, &f.CollectionID, &f.RelPath, &f.SizeBytes, &f.ModTimeUnix, &f.DiskName, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		f.CreatedAt = time.Unix(createdAt, 0)
		f.UpdatedAt = time.Unix(updatedAt, 0)
		files = append(files, f)
	}
	return files, rows.Err()
}

// clusterFiles groups files by cluster key.
// Returns clusters and a map of clusterKey -> member relPaths.
func (p *Pipeline) clusterFiles(files []catalog.FileRow) ([]TagCluster, map[string][]string) {
	clusterMap := make(map[string]*TagCluster)
	clusterMembers := make(map[string][]string) // clusterKey -> []relPath

	for _, f := range files {
		if !IsMediaFile(f.RelPath) {
			continue
		}
		if ShouldSkipPath(f.RelPath) {
			continue
		}

		// Build absolute path for EXIF extraction
		absPath := filepath.Join(p.collectionRoot, f.RelPath)

		// Extract EXIF
		exif, err := ExtractEXIFForTagging(absPath)
		if err != nil {
			slog.Debug("EXIF extract failed", "path", absPath, "error", err)
			exif = &FileEXIF{}
		}

		// Build cluster key
		modTime := time.Unix(f.ModTimeUnix, 0)
		key := BuildClusterKey(exif, modTime)

		// Get or create cluster
		cluster, exists := clusterMap[key]
		if !exists {
			cluster = &TagCluster{
				ID:            generateClusterID(),
				CollectionID:  p.collectionID,
				ClusterKey:    key,
				SampleRelPath: f.RelPath,
				CreatedAt:     time.Now(),
			}
			clusterMap[key] = cluster
		}

		// Track member
		clusterMembers[key] = append(clusterMembers[key], f.RelPath)

		// Update sample if this file is earlier (by mtime, then rel_path)
		if f.ModTimeUnix < getFileModTime(files, cluster.SampleRelPath) ||
			(f.ModTimeUnix == getFileModTime(files, cluster.SampleRelPath) && f.RelPath < cluster.SampleRelPath) {
			cluster.SampleRelPath = f.RelPath
		}
	}

	// Convert to slice
	clusters := make([]TagCluster, 0, len(clusterMap))
	for _, c := range clusterMap {
		clusters = append(clusters, *c)
	}
	return clusters, clusterMembers
}

// getFileModTime returns the modtime for a rel_path from the files slice.
func getFileModTime(files []catalog.FileRow, relPath string) int64 {
	for _, f := range files {
		if f.RelPath == relPath {
			return f.ModTimeUnix
		}
	}
	return 0
}

// processCluster tags the sample file and propagates to members.
// members is the list of file relPaths in this cluster (from clusterFiles).
func (p *Pipeline) processCluster(cluster TagCluster, allFiles []catalog.FileRow, members []string) (sampleTagged, tagsCreated, propagated, sidecarsWritten, sidecarErrors int, err error) {
	// Find sample file
	var sampleFile *catalog.FileRow
	for i := range allFiles {
		if allFiles[i].RelPath == cluster.SampleRelPath {
			sampleFile = &allFiles[i]
			break
		}
	}
	if sampleFile == nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("sample file not found: %s", cluster.SampleRelPath)
	}

	// Extract EXIF for sample
	sampleAbsPath := filepath.Join(p.collectionRoot, sampleFile.RelPath)
	exif, err := ExtractEXIFForTagging(sampleAbsPath)
	if err != nil {
		slog.Debug("EXIF extract failed for sample", "path", sampleAbsPath, "error", err)
		exif = &FileEXIF{}
	}

	// Determine capture date
	var captureDate time.Time
	if !exif.DateTimeOriginal.IsZero() {
		captureDate = exif.DateTimeOriginal
	} else {
		captureDate = time.Unix(sampleFile.ModTimeUnix, 0)
	}

	// 1. Rule-tag the sample
	ruleTags := p.ruleTagger.TagFile(exif, captureDate)
	if len(ruleTags) == 0 {
		return 0, 0, 0, 0, 0, nil // no rule tags, nothing to propagate
	}

	// Persist sample tags: origin="rule" (producer), source="sample" (role)
	sampleTags := make([]MediaTag, len(ruleTags))
	for i, t := range ruleTags {
		sampleTags[i] = MediaTag{
			Tag:        t.Tag,
			Origin:     "rule",  // producer: rule tagger
			Confidence: t.Confidence,
			Source:     "sample", // role: sample file
			CreatedAt:  time.Now(),
		}
	}
	sampleTagCount, err := p.persistTags(sampleFile.RelPath, sampleTags)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("persist sample tags: %w", err)
	}
	sampleTagged = 1
	tagsCreated += sampleTagCount

	// Write sidecar for sample
	written, err := p.writeSidecar(sampleAbsPath, sampleTags)
	if written {
		sidecarsWritten++
		if err != nil {
			sidecarErrors++
			slog.Error("write sample sidecar failed", "path", sampleAbsPath, "error", err)
			catalog.WriteAuditLog(p.db, "system", "tag.sidecar.write", "file", sampleFile.RelPath, "error", err.Error())
		} else {
			catalog.WriteAuditLog(p.db, "system", "tag.sidecar.write", "file", sampleFile.RelPath, "ok", "")
		}
	}

	// 2. Propagate to cluster members: origin="rule" (inherited producer), source="cluster" (role)
	propConfidence := ruleTags[0].Confidence * p.config.PropagationMultiplier
	if propConfidence < p.config.PropagationFloor {
		propConfidence = p.config.PropagationFloor
	}

	for _, memberRelPath := range members {
		if memberRelPath == cluster.SampleRelPath {
			continue // sample already tagged
		}

		propTags := make([]MediaTag, len(ruleTags))
		for i, t := range ruleTags {
			propTags[i] = MediaTag{
				Tag:        t.Tag,
				Origin:     "rule",   // inherited from producer (rule tagger)
				Confidence: propConfidence,
				Source:     "cluster", // role: propagated to cluster member
				CreatedAt:  time.Now(),
			}
		}

		count, err := p.persistTags(memberRelPath, propTags)
		if err != nil {
			slog.Error("persist propagated tags failed", "path", memberRelPath, "error", err)
			continue
		}
		tagsCreated += count
		if count > 0 {
			propagated++
		}

		// Write sidecar for member
		memberAbsPath := filepath.Join(p.collectionRoot, memberRelPath)
		written, err := p.writeSidecar(memberAbsPath, propTags)
		if written {
			sidecarsWritten++
			if err != nil {
				sidecarErrors++
				slog.Error("write member sidecar failed", "path", memberAbsPath, "error", err)
				catalog.WriteAuditLog(p.db, "system", "tag.sidecar.write", "file", memberRelPath, "error", err.Error())
			} else {
				catalog.WriteAuditLog(p.db, "system", "tag.sidecar.write", "file", memberRelPath, "ok", "")
			}
		}
	}

	// 3. Persist cluster and members to catalog
	if err := p.persistCluster(cluster, members); err != nil {
		slog.Error("persist cluster failed", "cluster_key", cluster.ClusterKey, "error", err)
	}

	return sampleTagged, tagsCreated, propagated, sidecarsWritten, sidecarErrors, nil
}

// persistTags inserts media_tags for a file using each tag's own Origin and Source.
// Returns the number of rows actually inserted (respects UNIQUE constraint).
func (p *Pipeline) persistTags(relPath string, tags []MediaTag) (int, error) {
	if len(tags) == 0 {
		return 0, nil
	}

	tx, err := p.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	count := 0
	now := time.Now().Unix()
	for _, t := range tags {
		// UNIQUE(collection_id, rel_path, tag, origin) - use INSERT OR IGNORE
		result, err := tx.Exec(`
			INSERT OR IGNORE INTO media_tags (id, collection_id, rel_path, tag, origin, confidence, source, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, generateTagID(), p.collectionID, relPath, t.Tag, t.Origin, t.Confidence, t.Source, now)
		if err != nil {
			return count, err
		}
		rowsAffected, _ := result.RowsAffected()
		count += int(rowsAffected)
	}

	if err := tx.Commit(); err != nil {
		return count, err
	}
	return count, nil
}

// persistCluster inserts the cluster and its members.
func (p *Pipeline) persistCluster(cluster TagCluster, members []string) error {
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	// Insert cluster
	_, err = tx.Exec(`
		INSERT OR REPLACE INTO tag_clusters (id, collection_id, cluster_key, sample_rel_path, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, cluster.ID, cluster.CollectionID, cluster.ClusterKey, cluster.SampleRelPath, now)
	if err != nil {
		return err
	}

	// Insert members
	for _, relPath := range members {
		_, err = tx.Exec(`
			INSERT OR IGNORE INTO tag_cluster_members (cluster_id, rel_path)
			VALUES (?, ?)
		`, cluster.ID, relPath)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// writeSidecar writes a sidecar file for the given media file and tags.
func (p *Pipeline) writeSidecar(mediaPath string, tags []MediaTag) (bool, error) {
	return p.sidecarWriter.WriteSidecarForFile(mediaPath, tags)
}

// generateClusterID returns a collision-proof cluster ID.
func generateClusterID() string {
	now := time.Now().UnixNano()
	// Use crypto/rand in production; for now use time-based
	return fmt.Sprintf("clu-%d-%08x", now, now&0xffffffff)
}

// generateTagID returns a collision-proof tag ID.
func generateTagID() string {
	now := time.Now().UnixNano()
	return fmt.Sprintf("tag-%d-%08x", now, now&0xffffffff)
}
package tagging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SidecarWriter handles writing sidecar files for tagged media.
type SidecarWriter struct{}

// NewSidecarWriter creates a new sidecar writer.
func NewSidecarWriter() *SidecarWriter {
	return &SidecarWriter{}
}

// WritePhotoSidecar writes an XMP sidecar for a photo file.
// Returns error if write fails (caller decides whether to treat as fatal).
func (w *SidecarWriter) WritePhotoSidecar(mediaPath string, tags []MediaTag) error {
	// Determine XMP path: <file>.xmp
	xmpPath := mediaPath + ".xmp"

	// Build dc:subject tags
	var subjects []string
	for _, t := range tags {
		subjects = append(subjects, t.Tag)
	}

	// Minimal valid XMP with dc:subject
	xmpContent := buildXMP(subjects)

	// Write atomically: write to temp, then rename
	tmpPath := xmpPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(xmpContent), 0o644); err != nil {
		return fmt.Errorf("write temp xmp: %w", err)
	}
	if err := os.Rename(tmpPath, xmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename xmp: %w", err)
	}
	return nil
}

// WriteVideoSidecar writes a JSON sidecar for a video file.
// Returns error if write fails (caller decides whether to treat as fatal).
func (w *SidecarWriter) WriteVideoSidecar(mediaPath string, tags []MediaTag) error {
	// Determine JSON path: <file>.json
	jsonPath := mediaPath + ".json"

	// Build JSON structure
	type sidecarTag struct {
		Tag        string  `json:"tag"`
		Confidence float64 `json:"confidence"`
		Origin     string  `json:"origin"`
	}
	var stags []sidecarTag
	for _, t := range tags {
		stags = append(stags, sidecarTag{
			Tag:        t.Tag,
			Confidence: t.Confidence,
			Origin:     t.Origin,
		})
	}

	data := struct {
		Tags      []sidecarTag `json:"tags"`
		UpdatedAt string       `json:"updated_at"`
	}{
		Tags:      stags,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}

	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}

	// Write atomically
	tmpPath := jsonPath + ".tmp"
	if err := os.WriteFile(tmpPath, jsonBytes, 0o644); err != nil {
		return fmt.Errorf("write temp json: %w", err)
	}
	if err := os.Rename(tmpPath, jsonPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename json: %w", err)
	}
	return nil
}

// WriteSidecarForFile writes the appropriate sidecar based on file extension.
// Returns (written bool, err error). written=true means a sidecar was attempted.
func (w *SidecarWriter) WriteSidecarForFile(mediaPath string, tags []MediaTag) (bool, error) {
	ext := strings.ToLower(filepath.Ext(mediaPath))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".heic":
		err := w.WritePhotoSidecar(mediaPath, tags)
		return true, err
	case ".mp4", ".mov", ".mkv", ".avi", ".webm":
		err := w.WriteVideoSidecar(mediaPath, tags)
		return true, err
	default:
		return false, nil // not a media type we handle
	}
}

// buildXMP creates a minimal valid XMP packet with dc:subject tags.
func buildXMP(subjects []string) string {
	// XMP header
	var sb strings.Builder
	sb.WriteString(`<?xpacket begin="" id="W5M0MpCehiHzreSzNTczkc9d"?>` + "\n")
	sb.WriteString(`<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="kelvios-tagging">` + "\n")
	sb.WriteString(`  <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` + "\n")
	sb.WriteString(`    <rdf:Description rdf:about="" xmlns:dc="http://purl.org/dc/elements/1.1/">` + "\n")
	sb.WriteString(`      <dc:subject>` + "\n")
	sb.WriteString(`        <rdf:Bag>` + "\n")
	for _, subj := range subjects {
		// Escape XML special chars
		escaped := strings.ReplaceAll(subj, "&", "&amp;")
		escaped = strings.ReplaceAll(escaped, "<", "&lt;")
		escaped = strings.ReplaceAll(escaped, ">", "&gt;")
		escaped = strings.ReplaceAll(escaped, "\"", "&quot;")
		sb.WriteString(`          <rdf:li>` + escaped + `</rdf:li>` + "\n")
	}
	sb.WriteString(`        </rdf:Bag>` + "\n")
	sb.WriteString(`      </dc:subject>` + "\n")
	sb.WriteString(`    </rdf:Description>` + "\n")
	sb.WriteString(`  </rdf:RDF>` + "\n")
	sb.WriteString(`</x:xmpmeta>` + "\n")
	sb.WriteString(`<?xpacket end="w"?>` + "\n")
	return sb.String()
}
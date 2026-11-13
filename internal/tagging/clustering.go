package tagging

import (
	"fmt"
	"strings"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
)

// BuildClusterKey creates a deterministic cluster key from EXIF data and file modtime.
// Format: device-model|gps-bucket|date
// device-model: EXIF Model or "unknown"
// gps-bucket: "lat,lon" at 2dp or "no-gps"
// date: YYYY-MM-DD from EXIF DateTimeOriginal or file mtime
func BuildClusterKey(exif *FileEXIF, modTime time.Time) string {
	device := exif.Model
	if device == "" {
		device = "unknown"
	}

	var gpsBucket string
	if exif.HasGPS {
		gpsBucket = fmt.Sprintf("%.2f,%.2f", exif.Latitude, exif.Longitude)
	} else {
		gpsBucket = "no-gps"
	}

	var captureDate time.Time
	if !exif.DateTimeOriginal.IsZero() {
		captureDate = exif.DateTimeOriginal
	} else {
		captureDate = modTime
	}
	dateStr := captureDate.Format("2006-01-02")

	return fmt.Sprintf("%s|%s|%s", device, gpsBucket, dateStr)
}

// ParseClusterKey parses a cluster key into its components.
func ParseClusterKey(key string) (ClusterKeyComponents, bool) {
	parts := strings.Split(key, "|")
	if len(parts) != 3 {
		return ClusterKeyComponents{}, false
	}
	return ClusterKeyComponents{
		DeviceModel: parts[0],
		GPSBucket:   parts[1],
		Date:        parts[2],
	}, true
}

// ExtractEXIFForTagging extracts EXIF data from a file using the catalog's EXIF parser.
// Returns FileEXIF with zero values if not a JPEG or no EXIF.
func ExtractEXIFForTagging(path string) (*FileEXIF, error) {
	exifData, err := catalog.ParseEXIF(path)
	if err != nil {
		// Not a JPEG or parse error - return zero EXIF
		return &FileEXIF{}, nil
	}
	if exifData == nil {
		return &FileEXIF{}, nil
	}
	return &FileEXIF{
		Model:            exifData.Model,
		DateTimeOriginal: exifData.DateTimeOriginal,
		Latitude:         exifData.Latitude,
		Longitude:        exifData.Longitude,
		HasGPS:           exifData.HasGPS,
	}, nil
}

// IsMediaFile checks if a file extension is a supported media type.
func IsMediaFile(relPath string) bool {
	ext := strings.ToLower(relPath)
	// Find the last dot
	lastDot := strings.LastIndex(ext, ".")
	if lastDot == -1 {
		return false
	}
	ext = ext[lastDot:]
	switch ext {
	case ".jpg", ".jpeg", ".png", ".heic",
		".mp4", ".mov", ".mkv", ".avi", ".webm":
		return true
	default:
		return false
	}
}

// ShouldSkipPath checks if a path should be skipped (dotfiles, .kelvios-shadow, etc.)
func ShouldSkipPath(relPath string) bool {
	// Skip dotfiles and dot-dirs
	parts := strings.Split(relPath, "/")
	for _, p := range parts {
		if strings.HasPrefix(p, ".") {
			return true
		}
	}
	// Skip .kelvios-shadow directory
	if strings.Contains(relPath, "/.kelvios-shadow/") ||
		strings.HasPrefix(relPath, ".kelvios-shadow/") ||
		relPath == ".kelvios-shadow" {
		return true
	}
	return false
}
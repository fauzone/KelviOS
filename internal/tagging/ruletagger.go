package tagging

import (
	"fmt"
	"strings"
	"time"
)

// RuleTagger generates tags from EXIF metadata using deterministic rules.
type RuleTagger struct {
	confidence float64
}

// NewRuleTagger creates a new rule tagger with the given confidence.
func NewRuleTagger(confidence float64) *RuleTagger {
	if confidence <= 0 {
		confidence = 0.9
	}
	return &RuleTagger{confidence: confidence}
}

// TagFile generates rule-based tags for a file from its EXIF data.
// Returns tags with origin "rule" and the configured confidence.
func (r *RuleTagger) TagFile(exif *FileEXIF, captureDate time.Time) []MediaTag {
	var tags []MediaTag
	now := time.Now()

	// Device tag: device:<Model> when EXIF Model present
	if exif.Model != "" {
		tags = append(tags, MediaTag{
			Tag:        "device:" + sanitizeTagValue(exif.Model),
			Origin:     "rule",
			Confidence: r.confidence,
			Source:     "individual",
			CreatedAt:  now,
		})
	}

	// Place tag: place:<lat2dp>,<lon2dp> when GPS present
	if exif.HasGPS {
		placeTag := fmtPlaceTag(exif.Latitude, exif.Longitude)
		tags = append(tags, MediaTag{
			Tag:        placeTag,
			Origin:     "rule",
			Confidence: r.confidence,
			Source:     "individual",
			CreatedAt:  now,
		})
	}

	// Month tag: month:<YYYY-MM> from capture date
	if !captureDate.IsZero() {
		monthTag := captureDate.Format("2006-01")
		tags = append(tags, MediaTag{
			Tag:        "month:" + monthTag,
			Origin:     "rule",
			Confidence: r.confidence,
			Source:     "individual",
			CreatedAt:  now,
		})
	}

	return tags
}

// sanitizeTagValue replaces characters that are not safe in tag values.
func sanitizeTagValue(s string) string {
	// Replace spaces and special chars with underscore
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.ReplaceAll(s, ",", "_")
	return s
}

// fmtPlaceTag formats a place tag from coordinates at 2dp.
func fmtPlaceTag(lat, lon float64) string {
	return fmt.Sprintf("place:%.2f,%.2f", lat, lon)
}
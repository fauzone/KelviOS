package catalog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// EXIFData holds parsed EXIF information from a JPEG file.
type EXIFData struct {
	DateTimeOriginal time.Time // EXIF tag 0x9003
	Model            string    // IFD0 tag 0x0110
	Latitude         float64   // GPS IFD tag 0x0002
	Longitude        float64   // GPS IFD tag 0x0004
	HasGPS           bool
}

// ParseEXIF reads a JPEG file and extracts EXIF data.
// Returns nil EXIFData if no EXIF block found or parsing fails.
// Only reads the first ~64KB of the file (EXIF is always near the start).
func ParseEXIF(path string) (*EXIFData, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Read first 64KB - EXIF APP1 segment is always near the start of JPEG
	buf := make([]byte, 65536)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return nil, err
	}
	buf = buf[:n]

	// JPEG SOI marker
	if len(buf) < 2 || buf[0] != 0xFF || buf[1] != 0xD8 {
		return nil, errors.New("not a JPEG file")
	}

	// Scan for APP1 marker (0xFFE1) which contains EXIF
	offset := 2
	for offset < len(buf)-1 {
		if buf[offset] != 0xFF {
			offset++
			continue
		}
		marker := buf[offset+1]
		// APP1 marker = 0xE1
		if marker == 0xE1 {
			// APP1 segment: length (2 bytes, big-endian) follows marker
			if offset+4 > len(buf) {
				break
			}
			segLen := binary.BigEndian.Uint16(buf[offset+2 : offset+4])
			if segLen < 2 {
				break
			}
			segStart := offset + 4
			segEnd := segStart + int(segLen) - 2 // -2 because length includes itself
			if segEnd > len(buf) {
				segEnd = len(buf)
			}
			segment := buf[segStart:segEnd]
			// Check for "Exif\0\0" header
			if len(segment) >= 6 && bytes.Equal(segment[:6], []byte("Exif\x00\x00")) {
				return parseTIFF(segment[6:])
			}
			return nil, nil // APP1 found but not EXIF
		}
		// Skip segment: length at offset+2
		if offset+3 >= len(buf) {
			break
		}
		segLen := binary.BigEndian.Uint16(buf[offset+2 : offset+4])
		offset += 2 + int(segLen)
		if segLen == 0 {
			break
		}
	}
	return nil, nil // No EXIF APP1 found
}

// parseTIFF parses the TIFF structure inside EXIF APP1 segment.
// Returns EXIFData with DateTimeOriginal, Model, GPS coordinates.
func parseTIFF(data []byte) (*EXIFData, error) {
	if len(data) < 8 {
		return nil, errors.New("TIFF header too short")
	}

	// Byte order: II = little-endian, MM = big-endian
	var byteOrder binary.ByteOrder = binary.BigEndian
	if data[0] == 'I' && data[1] == 'I' {
		byteOrder = binary.LittleEndian
	} else if data[0] == 'M' && data[1] == 'M' {
		byteOrder = binary.BigEndian
	} else {
		return nil, errors.New("invalid TIFF byte order")
	}

	// Check magic number (42)
	magic := byteOrder.Uint16(data[2:4])
	if magic != 42 {
		return nil, errors.New("invalid TIFF magic")
	}

	// First IFD offset (from start of TIFF header)
	firstIFDOffset := int(byteOrder.Uint32(data[4:8]))
	if firstIFDOffset < 8 || firstIFDOffset >= len(data) {
		return nil, errors.New("invalid first IFD offset")
	}

	exif := &EXIFData{}

	// Parse IFD0 (main image directory) - contains Model tag 0x0110 and ExifSubIFD pointer
	if err := parseIFD(data, firstIFDOffset, byteOrder, exif, true); err != nil {
		return nil, err
	}

	return exif, nil
}

// parseIFD parses a single Image File Directory.
// isIFD0 indicates whether this is the root IFD0 (which has ExifSubIFD and GPS IFD pointers).
func parseIFD(data []byte, ifdOffset int, byteOrder binary.ByteOrder, exif *EXIFData, isIFD0 bool) error {
	if ifdOffset+2 > len(data) {
		return errors.New("IFD offset out of bounds")
	}

	numEntries := int(byteOrder.Uint16(data[ifdOffset : ifdOffset+2]))
	entryStart := ifdOffset + 2
	entrySize := 12

	var exifSubIFDOffset uint32
	var gpsIFDOffset uint32

	for i := 0; i < numEntries; i++ {
		entryOffset := entryStart + i*entrySize
		if entryOffset+entrySize > len(data) {
			break
		}

		tag := byteOrder.Uint16(data[entryOffset : entryOffset+2])
		fieldType := byteOrder.Uint16(data[entryOffset+2 : entryOffset+4])
		count := byteOrder.Uint32(data[entryOffset+4 : entryOffset+8])
		valueOffset := byteOrder.Uint32(data[entryOffset+8 : entryOffset+12])

		// Field type sizes
		typeSize := fieldTypeSize(fieldType)
		if typeSize == 0 {
			continue
		}
		totalSize := uint64(count) * uint64(typeSize)

		var valueData []byte
		if totalSize <= 4 {
			// Value fits in the offset field itself
			valueData = data[entryOffset+8 : entryOffset+12]
		} else {
			// Value is at offset from TIFF header start
			absOffset := int(valueOffset)
			if absOffset < 0 || absOffset+int(totalSize) > len(data) {
				continue
			}
			valueData = data[absOffset : absOffset+int(totalSize)]
		}

		switch tag {
		case 0x0110: // Model (IFD0)
			if isIFD0 && fieldType == 2 { // ASCII
				exif.Model = readString(valueData, count)
			}
		case 0x8769: // ExifSubIFD pointer (IFD0)
			if isIFD0 {
				exifSubIFDOffset = valueOffset
			}
		case 0x8825: // GPS IFD pointer (IFD0)
			if isIFD0 {
				gpsIFDOffset = valueOffset
			}
		case 0x9003: // DateTimeOriginal (ExifSubIFD)
			if fieldType == 2 { // ASCII
				dtStr := readString(valueData, count)
				if t, err := parseEXIFDateTime(dtStr); err == nil {
					exif.DateTimeOriginal = t
				}
			}
		case 0x0002: // GPSLatitude (GPS IFD)
			if fieldType == 5 { // RATIONAL (2x LONG per value)
				lat := parseGPSCoordinate(valueData, count, byteOrder)
				if lat != nil {
					exif.Latitude = *lat
					exif.HasGPS = true
				}
			}
		case 0x0004: // GPSLongitude (GPS IFD)
			if fieldType == 5 { // RATIONAL
				lon := parseGPSCoordinate(valueData, count, byteOrder)
				if lon != nil {
					exif.Longitude = *lon
					exif.HasGPS = true
				}
			}
		case 0x0001: // GPSLatitudeRef (N/S)
			if fieldType == 2 && exif.HasGPS {
				ref := readString(valueData, count)
				if ref == "S" {
					exif.Latitude = -exif.Latitude
				}
			}
		case 0x0003: // GPSLongitudeRef (E/W)
			if fieldType == 2 && exif.HasGPS {
				ref := readString(valueData, count)
				if ref == "W" {
					exif.Longitude = -exif.Longitude
				}
			}
		}
	}

	// Recursively parse ExifSubIFD
	if exifSubIFDOffset > 0 {
		if err := parseIFD(data, int(exifSubIFDOffset), byteOrder, exif, false); err != nil {
			// Non-fatal: log and continue
		}
	}

	// Recursively parse GPS IFD
	if gpsIFDOffset > 0 {
		if err := parseIFD(data, int(gpsIFDOffset), byteOrder, exif, false); err != nil {
			// Non-fatal
		}
	}

	return nil
}

// fieldTypeSize returns the byte size for a TIFF field type.
func fieldTypeSize(fieldType uint16) int {
	switch fieldType {
	case 1: // BYTE
		return 1
	case 2: // ASCII
		return 1
	case 3: // SHORT
		return 2
	case 4: // LONG
		return 4
	case 5: // RATIONAL (2x LONG)
		return 8
	case 6: // SBYTE
		return 1
	case 7: // UNDEFINED
		return 1
	case 8: // SSHORT
		return 2
	case 9: // SLONG
		return 4
	case 10: // SRATIONAL
		return 8
	case 11: // FLOAT
		return 4
	case 12: // DOUBLE
		return 8
	default:
		return 0
	}
}

// readString reads an ASCII string from value data, respecting count.
func readString(data []byte, count uint32) string {
	if count == 0 {
		return ""
	}
	// ASCII strings are null-terminated; count includes null
	end := int(count)
	if end > len(data) {
		end = len(data)
	}
	s := string(data[:end])
	// Trim trailing nulls
	return strings.TrimRight(s, "\x00")
}

// parseEXIFDateTime parses EXIF DateTime format: "YYYY:MM:DD HH:MM:SS"
func parseEXIFDateTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if len(s) < 19 {
		return time.Time{}, errors.New("datetime string too short")
	}
	// Format: "2023:12:25 14:30:45"
	year, _ := strconv.Atoi(s[0:4])
	month, _ := strconv.Atoi(s[5:7])
	day, _ := strconv.Atoi(s[8:10])
	hour, _ := strconv.Atoi(s[11:13])
	min, _ := strconv.Atoi(s[14:16])
	sec, _ := strconv.Atoi(s[17:19])
	return time.Date(year, time.Month(month), day, hour, min, sec, 0, time.Local), nil
}

// parseGPSCoordinate parses a GPS coordinate from RATIONAL values (degrees, minutes, seconds).
// Returns decimal degrees.
func parseGPSCoordinate(data []byte, count uint32, byteOrder binary.ByteOrder) *float64 {
	if count < 3 || len(data) < 24 { // 3 RATIONALs = 3*8 = 24 bytes
		return nil
	}
	var coords [3]float64
	for i := 0; i < 3; i++ {
		num := int64(byteOrder.Uint32(data[i*8 : i*8+4]))
		den := int64(byteOrder.Uint32(data[i*8+4 : i*8+8]))
		if den == 0 {
			return nil
		}
		coords[i] = float64(num) / float64(den)
	}
	// Convert DMS to decimal degrees
	degrees := coords[0]
	minutes := coords[1]
	seconds := coords[2]
	decimal := degrees + minutes/60.0 + seconds/3600.0
	return &decimal
}
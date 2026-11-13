package catalog

import (
	"database/sql"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupArrangeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// arrange_plans.collection_id FK -> collections(id) -> pools(id):
	// tests use collection "coll-1" on pool "pool-1".
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES ('pool-1', 'warm', 'local', 'warm', 'single', 'single', '/mnt/warm', 1000000000, 100000000, 0, '', 'active', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert pool: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, created_at, updated_at) VALUES ('coll-1', 'photos', 'pool-1', 'photos', '', 0, ?, ?)`, now, now); err != nil {
		t.Fatalf("insert collection: %v", err)
	}
	return db
}

func writeArrangeFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return p
}

func chtimeArrange(t *testing.T, p string, tm time.Time) {
	t.Helper()
	if err := os.Chtimes(p, tm, tm); err != nil {
		t.Fatalf("chtime %s: %v", p, err)
	}
}

// --- Synthetic JPEG / TIFF fixture builder (little-endian) ---

type tiffEntry struct {
	tag   uint16
	typ   uint16
	count uint32
	// either inline value bytes (<=4) or offset to blob
	inline []byte
	blob   []byte
}

func buildTIFF(t *testing.T, model, datetime string, gps bool) []byte {
	t.Helper()
	// Data blobs appended after all IFDs; offsets computed dynamically.
	var tiff []byte
	put16 := func(v uint16) { var b [2]byte; binary.LittleEndian.PutUint16(b[:], v); tiff = append(tiff, b[:]...) }
	put32 := func(v uint32) { var b [4]byte; binary.LittleEndian.PutUint32(b[:], v); tiff = append(tiff, b[:]...) }

	// Header: "II", 42, IFD0 offset = 8
	tiff = append(tiff, 'I', 'I')
	put16(42)
	put32(8)

	// Decide IFD0 entries.
	type entrySpec struct {
		tag   uint16
		typ   uint16
		count uint32
		blob  []byte // nil => LONG value stored inline (offset patched later)
	}
	var ifd0 []entrySpec
	if model != "" {
		ifd0 = append(ifd0, entrySpec{tag: 0x0110, typ: 2, count: uint32(len(model) + 1), blob: append([]byte(model), 0)})
	}
	ifd0 = append(ifd0, entrySpec{tag: 0x8769, typ: 4, count: 1}) // ExifSubIFD pointer
	if gps {
		ifd0 = append(ifd0, entrySpec{tag: 0x8825, typ: 4, count: 1}) // GPS IFD pointer
	}

	// Sizes: IFD0 block = 2 + 12*n + 4. ExifIFD block = 2 + 12 + 4 (1 entry).
	ifd0Size := 2 + 12*len(ifd0) + 4
	exifIFDOff := 8 + ifd0Size
	exifIFDSize := 2 + 12 + 4
	gpsIFDOff := exifIFDOff + exifIFDSize
	gpsIFDSize := 2 + 24 + 4
	dataOff := gpsIFDOff + gpsIFDSize
	if !gps {
		// No GPS IFD: data starts right after ExifIFD.
		dataOff = exifIFDOff + exifIFDSize
	}

	// Write IFD0.
	put16(uint16(len(ifd0)))
	// Reserve entry slots; patch offsets after blobs are placed.
	blobPos := dataOff
	blobOffsets := map[uint16]int{}
	for _, e := range ifd0 {
		put16(e.tag)
		put16(e.typ)
		put32(e.count)
		if e.blob != nil && len(e.blob) <= 4 {
			// Inline value, padded to 4 bytes.
			padded := make([]byte, 4)
			copy(padded, e.blob)
			tiff = append(tiff, padded...)
		} else if e.blob != nil {
			blobOffsets[e.tag] = blobPos
			put32(uint32(blobPos))
			blobPos += len(e.blob)
		} else {
			// LONG pointer: value = target IFD offset.
			if e.tag == 0x8769 {
				put32(uint32(exifIFDOff))
			} else {
				put32(uint32(gpsIFDOff))
			}
		}
	}
	put32(0) // next IFD

	// Write ExifIFD (1 entry: DateTimeOriginal).
	put16(1)
	put16(0x9003)
	put16(2)
	put32(uint32(len(datetime) + 1))
	dtOff := blobPos
	put32(uint32(dtOff))
	blobPos += len(datetime) + 1
	put32(0)

	// Write GPS IFD if requested.
	var latOff, lonOff int
	if gps {
		put16(2)
		// Latitude rationals: 12/1, 20/1, 24/1 => 12.34
		latOff = blobPos
		put16(0x0002)
		put16(5)
		put32(3)
		put32(uint32(latOff))
		blobPos += 24
		// Longitude rationals: 56/1, 46/1, 48/1 => 56.78
		lonOff = blobPos
		put16(0x0004)
		put16(5)
		put32(3)
		put32(uint32(lonOff))
		blobPos += 24
		put32(0)
	}

	// Sanity: current length must equal dataOff.
	if len(tiff) != dataOff {
		t.Fatalf("TIFF layout mismatch: len=%d want dataOff=%d", len(tiff), dataOff)
	}

	// Append blobs in order: model string, datetime string, lat rationals, lon rationals.
	for _, e := range ifd0 {
		if e.blob != nil && len(e.blob) > 4 {
			if len(tiff) != blobOffsets[e.tag] {
				t.Fatalf("blob offset mismatch for tag %04x", e.tag)
			}
			tiff = append(tiff, e.blob...)
		}
	}
	if len(tiff) != dtOff {
		t.Fatalf("datetime blob offset mismatch: len=%d want %d", len(tiff), dtOff)
	}
	tiff = append(tiff, []byte(datetime)...)
	tiff = append(tiff, 0)
	if gps {
		if len(tiff) != latOff {
			t.Fatalf("lat blob offset mismatch")
		}
		putRat := func(num, den uint32) {
			var b [8]byte
			binary.LittleEndian.PutUint32(b[0:4], num)
			binary.LittleEndian.PutUint32(b[4:8], den)
			tiff = append(tiff, b[:]...)
		}
		putRat(12, 1)
		putRat(20, 1)
		putRat(24, 1)
		if len(tiff) != lonOff {
			t.Fatalf("lon blob offset mismatch")
		}
		putRat(56, 1)
		putRat(46, 1)
		putRat(48, 1)
	}
	return tiff
}

func buildSyntheticJPEG(t *testing.T, model, datetime string, gps bool) []byte {
	t.Helper()
	tiffData := buildTIFF(t, model, datetime, gps)
	var jpg []byte
	jpg = append(jpg, 0xFF, 0xD8)             // SOI
	jpg = append(jpg, 0xFF, 0xE1)             // APP1
	segLen := 2 + 6 + len(tiffData)            // length includes itself
	jpg = append(jpg, byte(segLen>>8), byte(segLen))
	jpg = append(jpg, []byte("Exif\x00\x00")...)
	jpg = append(jpg, tiffData...)
	jpg = append(jpg, 0xFF, 0xD9) // EOI
	return jpg
}

func TestParseEXIFSyntheticJPEG(t *testing.T) {
	dir := t.TempDir()
	p := writeArrangeFile(t, dir, "photo.jpg", "")
	// Overwrite with the real JPEG bytes (writeArrangeFile wrote a string placeholder).
	jpeg := buildSyntheticJPEG(t, "Canon EOS", "2024:05:06 12:00:00", true)
	if err := os.WriteFile(p, jpeg, 0o644); err != nil {
		t.Fatalf("write jpeg: %v", err)
	}
	exif, err := ParseEXIF(p)
	if err != nil {
		t.Fatalf("ParseEXIF: %v", err)
	}
	if exif == nil {
		t.Fatalf("expected EXIF data, got nil")
	}
	if exif.Model != "Canon EOS" {
		t.Errorf("Model: got %q, want %q", exif.Model, "Canon EOS")
	}
	want := time.Date(2024, 5, 6, 12, 0, 0, 0, time.Local)
	if !exif.DateTimeOriginal.Equal(want) {
		t.Errorf("DateTimeOriginal: got %v, want %v", exif.DateTimeOriginal, want)
	}
	if !exif.HasGPS {
		t.Fatalf("expected HasGPS true")
	}
	if math.Abs(exif.Latitude-12.34) > 1e-6 {
		t.Errorf("Latitude: got %v, want 12.34", exif.Latitude)
	}
	if math.Abs(exif.Longitude-56.78) > 1e-6 {
		t.Errorf("Longitude: got %v, want 56.78", exif.Longitude)
	}
}

func TestParseEXIFNoEXIF(t *testing.T) {
	dir := t.TempDir()
	p := writeArrangeFile(t, dir, "plain.jpg", "")
	// Minimal JPEG: SOI + EOI only, no APP1.
	if err := os.WriteFile(p, []byte{0xFF, 0xD8, 0xFF, 0xD9}, 0o644); err != nil {
		t.Fatalf("write jpeg: %v", err)
	}
	exif, err := ParseEXIF(p)
	if err != nil {
		t.Fatalf("ParseEXIF on EXIF-less JPEG should not error, got: %v", err)
	}
	if exif != nil {
		t.Errorf("expected nil EXIF, got %+v", exif)
	}
}

func TestParseEXIFNotJPEG(t *testing.T) {
	dir := t.TempDir()
	p := writeArrangeFile(t, dir, "garbage.jpg", "this is not a jpeg at all")
	if _, err := ParseEXIF(p); err == nil {
		t.Errorf("expected error for non-JPEG input, got nil")
	}
}

func TestComputeDestinationRules(t *testing.T) {
	mtime := time.Date(2023, 3, 4, 12, 0, 0, 0, time.UTC)
	exifDate := time.Date(2024, 5, 6, 12, 0, 0, 0, time.Local)
	if got := computeDestination("f.png", nil, mtime); got != "2023/2023-03-04/f.png" {
		t.Errorf("dated-mtime: got %q", got)
	}
	if got := computeDestination("f.jpg", &EXIFData{Model: "Canon EOS", DateTimeOriginal: exifDate}, mtime); got != "Devices/Canon EOS/2024/2024-05-06/f.jpg" {
		t.Errorf("exif-date-model: got %q", got)
	}
	if got := computeDestination("f.jpg", &EXIFData{Model: "Canon EOS", DateTimeOriginal: exifDate, Latitude: 12.34, Longitude: 56.78, HasGPS: true}, mtime); got != "Places/12.34,56.78/2024/2024-05-06/f.jpg" {
		t.Errorf("gps-wins: got %q", got)
	}
	if got := computeDestination("f.png", nil, time.Time{}); got != "Undated/f.png" {
		t.Errorf("undated: got %q", got)
	}
	if got := computeDestination("f.jpg", &EXIFData{Latitude: 12.34, Longitude: 56.78, HasGPS: true}, time.Time{}); got != "Places/12.34,56.78/Undated/f.jpg" {
		t.Errorf("gps-undated: got %q", got)
	}
}

func TestGenerateArrangePlanRules(t *testing.T) {
	db := setupArrangeDB(t)
	root := t.TempDir()
	mtime := time.Date(2023, 3, 4, 12, 0, 0, 0, time.UTC)

	// Dated via mtime.
	p1 := writeArrangeFile(t, root, "dated.png", "png-bytes")
	chtimeArrange(t, p1, mtime)
	// EXIF photo: GPS wins over device.
	p2 := writeArrangeFile(t, root, "photo.jpg", "")
	if err := os.WriteFile(p2, buildSyntheticJPEG(t, "Canon EOS", "2024:05:06 12:00:00", true), 0o644); err != nil {
		t.Fatalf("write photo.jpg: %v", err)
	}
	// Skipped: non-media, dotfile, dot-dir, shadow dir.
	writeArrangeFile(t, root, "notes.txt", "text")
	writeArrangeFile(t, root, ".hidden.jpg", "hidden")
	writeArrangeFile(t, root, ".hiddendir/y.jpg", "hidden-dir")
	writeArrangeFile(t, root, ".kelvios-shadow/coll-1/z.jpg", "shadow")
	// Idempotent: already at its destination.
	p3 := writeArrangeFile(t, root, "2023/2023-03-04/placed.png", "placed")
	chtimeArrange(t, p3, mtime)

	res, err := GenerateArrangePlan(db, "coll-1", root, "test")
	if err != nil {
		t.Fatalf("GenerateArrangePlan: %v", err)
	}
	if res.Plan.Status != ArrangeStatusPending {
		t.Errorf("plan status: got %q, want pending", res.Plan.Status)
	}
	if res.ConflictCount != 0 {
		t.Errorf("conflicts: got %d, want 0", res.ConflictCount)
	}
	want := map[string]string{
		"dated.png": "2023/2023-03-04/dated.png",
		"photo.jpg": "Places/12.34,56.78/2024/2024-05-06/photo.jpg",
	}
	if len(res.Moves) != len(want) {
		t.Fatalf("moves: got %d, want %d (%v)", len(res.Moves), len(want), res.Moves)
	}
	for _, m := range res.Moves {
		w, ok := want[m.SourceRel]
		if !ok {
			t.Errorf("unexpected move: %s -> %s", m.SourceRel, m.DestRel)
			continue
		}
		if m.DestRel != w {
			t.Errorf("move %s: got dest %q, want %q", m.SourceRel, m.DestRel, w)
		}
		if m.Conflict != "" {
			t.Errorf("move %s: unexpected conflict %q", m.SourceRel, m.Conflict)
		}
	}
}

func TestGenerateArrangePlanConflicts(t *testing.T) {
	db := setupArrangeDB(t)
	root := t.TempDir()
	mtime := time.Date(2023, 3, 4, 12, 0, 0, 0, time.UTC)

	// Duplicate dest: same filename + same EXIF date/model in two dirs.
	for _, dir := range []string{"dir1", "dir2"} {
		p := writeArrangeFile(t, root, dir+"/same.jpg", "")
		if err := os.WriteFile(p, buildSyntheticJPEG(t, "Nikon Z6", "2024:05:06 12:00:00", false), 0o644); err != nil {
			t.Fatalf("write %s: %v", dir, err)
		}
	}
	// Dest exists: pre-create the destination file.
	writeArrangeFile(t, root, "2023/2023-03-04/exists.png", "already there")
	p := writeArrangeFile(t, root, "exists.png", "source")
	chtimeArrange(t, p, mtime)

	res, err := GenerateArrangePlan(db, "coll-1", root, "test")
	if err != nil {
		t.Fatalf("GenerateArrangePlan: %v", err)
	}
	if res.ConflictCount != 3 {
		t.Fatalf("conflicts: got %d, want 3", res.ConflictCount)
	}
	conflicts := map[string]string{}
	for _, m := range res.Moves {
		conflicts[m.SourceRel] = m.Conflict
	}
	if conflicts["dir1/same.jpg"] != ArrangeConflictDuplicateDest {
		t.Errorf("dir1/same.jpg conflict: got %q, want duplicate_dest", conflicts["dir1/same.jpg"])
	}
	if conflicts["dir2/same.jpg"] != ArrangeConflictDuplicateDest {
		t.Errorf("dir2/same.jpg conflict: got %q, want duplicate_dest", conflicts["dir2/same.jpg"])
	}
	if conflicts["exists.png"] != ArrangeConflictDestExists {
		t.Errorf("exists.png conflict: got %q, want dest_exists", conflicts["exists.png"])
	}
}

func TestArrangeDecideApplyUndo(t *testing.T) {
	db := setupArrangeDB(t)
	root := t.TempDir()
	mtime := time.Date(2023, 3, 4, 12, 0, 0, 0, time.UTC)
	p1 := writeArrangeFile(t, root, "a.png", "aaa")
	chtimeArrange(t, p1, mtime)
	p2 := writeArrangeFile(t, root, "b.png", "bbb")
	chtimeArrange(t, p2, mtime)

	// Apply on pending must fail.
	res, err := GenerateArrangePlan(db, "coll-1", root, "test")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := ApplyArrangePlan(db, res.Plan.ID, root, time.Now(), "test", DefaultMoveFunc); err == nil {
		t.Fatalf("apply on pending plan should fail")
	}

	// Approve -> apply -> files actually moved.
	if err := DecideArrangePlan(db, res.Plan.ID, ArrangeStatusApproved, time.Now(), "test"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := ApplyArrangePlan(db, res.Plan.ID, root, time.Now(), "test", DefaultMoveFunc); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "2023/2023-03-04/a.png")); err != nil {
		t.Errorf("a.png not at destination after apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.png")); !os.IsNotExist(err) {
		t.Errorf("a.png still at source after apply")
	}
	// Move rows marked moved with sha256.
	plan, moves, err := GetArrangePlan(db, res.Plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if plan.Status != ArrangeStatusApplied {
		t.Errorf("plan status: got %q, want applied", plan.Status)
	}
	for _, m := range moves {
		if m.Status != ArrangeMoveMoved {
			t.Errorf("move %s status: got %q, want moved", m.SourceRel, m.Status)
		}
		if m.SHA256 == "" {
			t.Errorf("move %s: sha256 empty after apply", m.SourceRel)
		}
	}

	// Undo -> files back.
	if err := UndoArrangePlan(db, res.Plan.ID, root, time.Now(), "test", DefaultMoveFunc); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.png")); err != nil {
		t.Errorf("a.png not back at source after undo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "b.png")); err != nil {
		t.Errorf("b.png not back at source after undo: %v", err)
	}
	plan, _, err = GetArrangePlan(db, res.Plan.ID)
	if err != nil {
		t.Fatalf("get plan after undo: %v", err)
	}
	if plan.Status != ArrangeStatusUndone {
		t.Errorf("plan status after undo: got %q, want undone", plan.Status)
	}
}

func TestArrangeApplyConflictPlanMovesNothing(t *testing.T) {
	db := setupArrangeDB(t)
	root := t.TempDir()
	for _, dir := range []string{"dir1", "dir2"} {
		p := writeArrangeFile(t, root, dir+"/same.jpg", "")
		if err := os.WriteFile(p, buildSyntheticJPEG(t, "Nikon Z6", "2024:05:06 12:00:00", false), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	res, err := GenerateArrangePlan(db, "coll-1", root, "test")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.ConflictCount == 0 {
		t.Fatalf("expected conflicts, got 0")
	}
	if err := DecideArrangePlan(db, res.Plan.ID, ArrangeStatusApproved, time.Now(), "test"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := ApplyArrangePlan(db, res.Plan.ID, root, time.Now(), "test", DefaultMoveFunc); err == nil {
		t.Fatalf("apply on conflict plan should fail")
	}
	// Nothing moved.
	if _, err := os.Stat(filepath.Join(root, "dir1/same.jpg")); err != nil {
		t.Errorf("dir1/same.jpg moved despite failed apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dir2/same.jpg")); err != nil {
		t.Errorf("dir2/same.jpg moved despite failed apply: %v", err)
	}
}

func TestV27MigrationHasArrangeAndTaggingTables(t *testing.T) {
	db := setupArrangeDB(t)
	var version int
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != 41 {
		t.Errorf("schema_version: got %d, want 41", version)
	}
	for _, tbl := range []string{"arrange_plans", "arrange_moves", "tag_clusters", "tag_cluster_members", "media_tags", "backup_repos", "backup_snapshots", "wake_checklist_runs", "scrub_state", "compose_artifacts", "compose_artifact_versions", "compose_runs", "app_installs"} {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", tbl, err)
		}
	}
}

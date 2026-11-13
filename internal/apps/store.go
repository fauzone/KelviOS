package apps

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

// InstalledInfo holds the installed image information for an app.
type InstalledInfo struct {
	AppID             string
	Status            Status
	ImageRef          string
	VersionLabel      string
	Digest            string
	PreviousImageRef  string
	UpdatedAt         time.Time
}

// GetInstalledInfo retrieves the full installed info for an app.
// Returns (info, false) if app is not installed (no row or status=available).
func GetInstalledInfo(db *sql.DB, appID string) (InstalledInfo, bool, error) {
	row := db.QueryRow(`
		SELECT app_id, status, image_ref, version_label, digest, previous_image_ref, updated_at
		FROM app_installs WHERE app_id = ?
	`, appID)

	var info InstalledInfo
	var statusStr string
	var imageRef, versionLabel, digest, prevImageRef sql.NullString
	var updatedAt int64

	err := row.Scan(&info.AppID, &statusStr, &imageRef, &versionLabel, &digest, &prevImageRef, &updatedAt)
	if err == sql.ErrNoRows {
		return InstalledInfo{}, false, nil
	}
	if err != nil {
		return InstalledInfo{}, false, err
	}

	info.Status = Status(statusStr)
	if imageRef.Valid {
		info.ImageRef = imageRef.String
	}
	if versionLabel.Valid {
		info.VersionLabel = versionLabel.String
	}
	if digest.Valid {
		info.Digest = digest.String
	}
	if prevImageRef.Valid {
		info.PreviousImageRef = prevImageRef.String
	}
	info.UpdatedAt = time.Unix(updatedAt, 0)

	// If status is available, treat as not installed
	if info.Status == StatusAvailable {
		return InstalledInfo{}, false, nil
	}

	return info, true, nil
}

// GetAllInstalledInfo retrieves installed info for all apps with a row in app_installs.
func GetAllInstalledInfo(db *sql.DB) (map[string]InstalledInfo, error) {
	rows, err := db.Query(`
		SELECT app_id, status, image_ref, version_label, digest, previous_image_ref, updated_at
		FROM app_installs
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]InstalledInfo)
	for rows.Next() {
		var info InstalledInfo
		var statusStr string
		var imageRef, versionLabel, digest, prevImageRef sql.NullString
		var updatedAt int64

		if err := rows.Scan(&info.AppID, &statusStr, &imageRef, &versionLabel, &digest, &prevImageRef, &updatedAt); err != nil {
			return nil, err
		}

		info.Status = Status(statusStr)
		if imageRef.Valid {
			info.ImageRef = imageRef.String
		}
		if versionLabel.Valid {
			info.VersionLabel = versionLabel.String
		}
		if digest.Valid {
			info.Digest = digest.String
		}
		if prevImageRef.Valid {
			info.PreviousImageRef = prevImageRef.String
		}
		info.UpdatedAt = time.Unix(updatedAt, 0)

		// Skip available status (treated as not installed)
		if info.Status != StatusAvailable {
			result[info.AppID] = info
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// SetInstalledInfo upserts the installed info for an app.
// For install: sets image_ref from catalog (or provided), clears previous_image_ref.
// For update: sets previous_image_ref to current image_ref, then updates image_ref/version_label/digest to target.
// For rollback: swaps image_ref with previous_image_ref, clears previous_image_ref.
// For uninstall: sets status=available, keeps image_ref for history (or clears per decision-9).
func SetInstalledInfo(db *sql.DB, appID string, st Status, imageRef, versionLabel, digest, previousImageRef string) error {
	// Validate app exists in catalog
	if _, ok := Get(appID); !ok {
		return ErrAppNotFound(appID)
	}

	now := time.Now().Unix()
	_, err := db.Exec(`
		INSERT INTO app_installs (app_id, status, image_ref, version_label, digest, previous_image_ref, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(app_id) DO UPDATE SET
			status = excluded.status,
			image_ref = excluded.image_ref,
			version_label = excluded.version_label,
			digest = excluded.digest,
			previous_image_ref = excluded.previous_image_ref,
			updated_at = excluded.updated_at
	`, appID, string(st), imageRef, versionLabel, digest, previousImageRef, now)
	return err
}

// ClearPreviousImageRef clears the previous_image_ref for an app (called after successful update, uninstall, or kill).
func ClearPreviousImageRef(db *sql.DB, appID string) error {
	_, err := db.Exec(`
		UPDATE app_installs SET previous_image_ref = '', updated_at = ? WHERE app_id = ?
	`, time.Now().Unix(), appID)
	return err
}

// GetStatuses retrieves the installation status for all apps from the database.
// Apps without a row in app_installs are considered "available" (not returned in the map).
func GetStatuses(db *sql.DB) (map[string]Status, error) {
	rows, err := db.Query(`SELECT app_id, status FROM app_installs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	statuses := make(map[string]Status)
	for rows.Next() {
		var appID, statusStr string
		if err := rows.Scan(&appID, &statusStr); err != nil {
			return nil, err
		}
		statuses[appID] = Status(statusStr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return statuses, nil
}

// SetStatus upserts the installation status for an app.
// Returns error if appID is not in the catalog.
// Uses current timestamp for updated_at.
// For uninstall (target available), the row is kept with status=available (history preserved).
func SetStatus(db *sql.DB, appID string, st Status) error {
	// Validate app exists in catalog
	if _, ok := Get(appID); !ok {
		return ErrAppNotFound(appID)
	}

	now := time.Now().Unix()
	_, err := db.Exec(`
		INSERT INTO app_installs (app_id, status, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(app_id) DO UPDATE SET
			status = excluded.status,
			updated_at = excluded.updated_at
	`, appID, string(st), now)
	return err
}

// ErrAppNotFound is returned when an app ID is not in the catalog.
type ErrAppNotFound string

func (e ErrAppNotFound) Error() string {
	return "app not found: " + string(e)
}
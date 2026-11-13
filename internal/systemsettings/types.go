package systemsettings

// SystemSettings holds the system-level settings stored in meta table.
type SystemSettings struct {
	Language      string // "en" | "id", empty = not set
	Timezone      string // IANA timezone name, empty = not set
	SetupCompletedAt int64 // Unix epoch seconds, 0 = not completed
}

// Meta keys used for persistence.
const (
	MetaKeyLanguage       = "system_language"
	MetaKeyTimezone       = "system_timezone"
	MetaKeySetupCompleted = "setup_completed_at"
)

// ValidLanguages is the allow-list for language values.
var ValidLanguages = map[string]bool{
	"en": true,
	"id": true,
}
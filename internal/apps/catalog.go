package apps

// App represents an application in the catalog.
type App struct {
	ID          string
	Name        string
	Category    string
	Description string
	Wave        int
	Optional    bool
	Group       string
	Image       string
	RAMMB       int
	Ports       []int
	Notes       string
}

// Catalog returns all apps in stable order: wave ascending, then name ascending.
func Catalog() []App {
	apps := make([]App, len(catalogData))
	copy(apps, catalogData)
	return apps
}

// Get returns the app with the given ID and a boolean indicating if found.
func Get(id string) (App, bool) {
	for _, a := range catalogData {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// ByWave returns all apps for the given wave (1 or 2).
func ByWave(wave int) []App {
	var result []App
	for _, a := range catalogData {
		if a.Wave == wave {
			result = append(result, a)
		}
	}
	return result
}

// ByCategory returns all apps for the given category.
func ByCategory(category string) []App {
	var result []App
	for _, a := range catalogData {
		if a.Category == category {
			result = append(result, a)
		}
	}
	return result
}

// ValidateCatalog validates the catalog data.
// Returns error if: duplicate ID, empty ID/Name, Wave not 1/2, RAMMB <= 0,
// duplicate port within same app, port outside 1-65535.
func ValidateCatalog() error {
	seenID := make(map[string]bool)
	for _, a := range catalogData {
		if a.ID == "" {
			return ErrValidation("empty ID")
		}
		if seenID[a.ID] {
			return ErrValidation("duplicate ID: " + a.ID)
		}
		seenID[a.ID] = true

		if a.Name == "" {
			return ErrValidation("empty Name for ID " + a.ID)
		}
		if a.Wave != 1 && a.Wave != 2 {
			return ErrValidation("invalid Wave for " + a.ID + ": must be 1 or 2")
		}
		if a.RAMMB <= 0 {
			return ErrValidation("RAMMB must be > 0 for " + a.ID)
		}

		seenPort := make(map[int]bool)
		for _, p := range a.Ports {
			if p < 1 || p > 65535 {
				return ErrValidation("port out of range 1-65535 for " + a.ID + ": " + itoa(p))
			}
			if seenPort[p] {
				return ErrValidation("duplicate port within app " + a.ID + ": " + itoa(p))
			}
			seenPort[p] = true
		}
	}
	return nil
}

// PortCollisions returns a map of ports claimed by more than one app
// to the list of app IDs claiming that port. Empty map if no collisions.
func PortCollisions() map[int][]string {
	portToApps := make(map[int][]string)
	for _, a := range catalogData {
		for _, p := range a.Ports {
			portToApps[p] = append(portToApps[p], a.ID)
		}
	}
	collisions := make(map[int][]string)
	for p, ids := range portToApps {
		if len(ids) > 1 {
			collisions[p] = ids
		}
	}
	return collisions
}

// ErrValidation is a validation error.
type ErrValidation string

func (e ErrValidation) Error() string {
	return "catalog validation: " + string(e)
}

// itoa converts int to string without fmt dependency.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}

// catalogData holds the wave-1 and wave-2 app catalog.
// Order: wave ascending, then name ascending.
var catalogData = []App{
	// Wave 1 (9 entries) - sorted by name
	{
		ID:          "adguard-home",
		Name:        "AdGuard Home",
		Category:    "network",
		Description: "Network-wide ad blocking and DNS filtering",
		Wave:        1,
		Optional:    false,
		Group:       "",
		Image:       "adguard/adguardhome:latest",
		RAMMB:       128,
		Ports:       []int{53, 67, 68, 80, 443, 3000, 853, 784, 8853, 5443},
		Notes:       "",
	},
	{
		ID:          "esphome",
		Name:        "ESPHome",
		Category:    "iot",
		Description: "Firmware generator for ESP8266/ESP32 devices",
		Wave:        1,
		Optional:    false,
		Group:       "home-automation",
		Image:       "ghcr.io/esphome/esphome:latest",
		RAMMB:       256,
		Ports:       []int{6052, 6053},
		Notes:       "",
	},
	{
		ID:          "frigate",
		Name:        "Frigate",
		Category:    "cctv",
		Description: "NVR with real-time AI object detection for IP cameras",
		Wave:        1,
		Optional:    false,
		Group:       "",
		Image:       "ghcr.io/blakeblackshear/frigate:stable",
		RAMMB:       1024,
		Ports:       []int{5000, 8554, 8555},
		Notes:       "",
	},
	{
		ID:          "home-assistant",
		Name:        "Home Assistant",
		Category:    "iot",
		Description: "Home automation hub integrating devices and services",
		Wave:        1,
		Optional:    false,
		Group:       "home-automation",
		Image:       "ghcr.io/home-assistant/home-assistant:stable",
		RAMMB:       1024,
		Ports:       []int{8123},
		Notes:       "",
	},
	{
		ID:          "immich",
		Name:        "Immich",
		Category:    "photos",
		Description: "Self-hosted photo and video backup solution",
		Wave:        1,
		Optional:    false,
		Group:       "",
		Image:       "ghcr.io/immich-app/immich-server:release",
		RAMMB:       1024,
		Ports:       []int{2283},
		Notes:       "",
	},
	{
		ID:          "jellyfin",
		Name:        "Jellyfin",
		Category:    "media",
		Description: "Media server for streaming movies, TV shows, and music",
		Wave:        1,
		Optional:    false,
		Group:       "",
		Image:       "jellyfin/jellyfin:latest",
		RAMMB:       512,
		Ports:       []int{8096},
		Notes:       "",
	},
	{
		ID:          "mosquitto",
		Name:        "Mosquitto",
		Category:    "iot",
		Description: "MQTT broker for IoT messaging",
		Wave:        1,
		Optional:    false,
		Group:       "home-automation",
		Image:       "eclipse-mosquitto:2",
		RAMMB:       64,
		Ports:       []int{1883, 9001},
		Notes:       "",
	},
	{
		ID:          "syncthing",
		Name:        "Syncthing",
		Category:    "sync",
		Description: "Continuous file synchronization between devices",
		Wave:        1,
		Optional:    false,
		Group:       "",
		Image:       "syncthing/syncthing:latest",
		RAMMB:       256,
		Ports:       []int{8384, 22000, 21027},
		Notes:       "",
	},
	{
		ID:          "zigbee2mqtt",
		Name:        "Zigbee2MQTT",
		Category:    "iot",
		Description: "Zigbee to MQTT bridge for Zigbee devices",
		Wave:        1,
		Optional:    false,
		Group:       "home-automation",
		Image:       "koenkk/zigbee2mqtt:latest",
		RAMMB:       256,
		Ports:       []int{8080},
		Notes:       "",
	},
	// Wave 2 (17 entries) - sorted by name
	{
		ID:          "concat",
		Name:        "Concat",
		Category:    "media",
		Description: "Video concatenation and processing tool",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "ghcr.io/concat/concat:latest",
		RAMMB:       512,
		Ports:       []int{8080},
		Notes:       "",
	},
	{
		ID:          "handbrake",
		Name:        "HandBrake",
		Category:    "media",
		Description: "Video transcoder with batch processing",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "jlesage/handbrake:latest",
		RAMMB:       1024,
		Ports:       []int{5800},
		Notes:       "",
	},
	{
		ID:          "navidrome",
		Name:        "Navidrome",
		Category:    "media",
		Description: "Lightweight music server and streamer",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "deluan/navidrome:latest",
		RAMMB:       256,
		Ports:       []int{4533},
		Notes:       "",
	},
	{
		ID:          "nextcloud",
		Name:        "Nextcloud",
		Category:    "productivity",
		Description: "Self-hosted productivity platform (files, calendar, contacts)",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "nextcloud:latest",
		RAMMB:       1024,
		Ports:       []int{8080},
		Notes:       "",
	},
	{
		ID:          "node-red",
		Name:        "Node-RED",
		Category:    "automation",
		Description: "Flow-based development tool for visual programming",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "nodered/node-red:latest",
		RAMMB:       256,
		Ports:       []int{1880},
		Notes:       "",
	},
	{
		ID:          "ollama",
		Name:        "Ollama",
		Category:    "ai",
		Description: "Run LLMs locally with simple API",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "ollama/ollama:latest",
		RAMMB:       4096,
		Ports:       []int{11434},
		Notes:       "Requires significant RAM and GPU for larger models; minimum 4GB RAM for small models, 8GB+ recommended",
	},
	{
		ID:          "open-webui",
		Name:        "Open WebUI",
		Category:    "ai",
		Description: "Self-hosted WebUI for LLMs (Ollama compatible)",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "ghcr.io/open-webui/open-webui:main",
		RAMMB:       1024,
		Ports:       []int{8080},
		Notes:       "",
	},
	{
		ID:          "paperless-ngx",
		Name:        "Paperless-ngx",
		Category:    "documents",
		Description: "Document management system with OCR",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "ghcr.io/paperless-ngx/paperless-ngx:latest",
		RAMMB:       1024,
		Ports:       []int{8000},
		Notes:       "",
	},
	{
		ID:          "photoprism",
		Name:        "PhotoPrism",
		Category:    "photos",
		Description: "AI-powered photo management and browsing",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "photoprism/photoprism:latest",
		RAMMB:       2048,
		Ports:       []int{2342},
		Notes:       "",
	},
	{
		ID:          "remotion",
		Name:        "Remotion",
		Category:    "media",
		Description: "Programmatic video creation with React",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "ghcr.io/remotion/remotion:latest",
		RAMMB:       2048,
		Ports:       []int{3000},
		Notes:       "",
	},
	{
		ID:          "tdarr",
		Name:        "Tdarr",
		Category:    "media",
		Description: "Distributed media transcoding and remuxing",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "ghcr.io/haveagitgattd1/tdarr:latest",
		RAMMB:       1024,
		Ports:       []int{8265, 8266},
		Notes:       "",
	},
	{
		ID:          "tube-archivist",
		Name:        "Tube Archivist",
		Category:    "media",
		Description: "Self-hosted YouTube media server",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "bbilly1/tubearchivist:latest",
		RAMMB:       1024,
		Ports:       []int{8000},
		Notes:       "",
	},
	{
		ID:          "vaultwarden",
		Name:        "Vaultwarden",
		Category:    "security",
		Description: "Lightweight Bitwarden-compatible password manager",
		Wave:        2,
		Optional:    true,
		Group:       "",
		Image:       "vaultwarden/server:latest",
		RAMMB:       128,
		Ports:       []int{8080},
		Notes:       "",
	},
	{
		ID:          "vidbee",
		Name:        "VidBee",
		Category:    "media",
		Description: "Video processing and analysis platform",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "ghcr.io/vidbee/vidbee:latest",
		RAMMB:       1024,
		Ports:       []int{8080},
		Notes:       "",
	},
	{
		ID:          "videorag",
		Name:        "VideoRAG",
		Category:    "ai",
		Description: "Video retrieval-augmented generation pipeline",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "ghcr.io/videorag/videorag:latest",
		RAMMB:       2048,
		Ports:       []int{8080},
		Notes:       "",
	},
	{
		ID:          "wireguard",
		Name:        "WireGuard",
		Category:    "network",
		Description: "Modern VPN tunnel (kernel module + userspace tools)",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "linuxserver/wireguard:latest",
		RAMMB:       64,
		Ports:       []int{51820},
		Notes:       "",
	},
	{
		ID:          "qbittorrent",
		Name:        "qBittorrent",
		Category:    "download",
		Description: "BitTorrent client with web UI",
		Wave:        2,
		Optional:    false,
		Group:       "",
		Image:       "linuxserver/qbittorrent:latest",
		RAMMB:       512,
		Ports:       []int{8080, 6881},
		Notes:       "",
	},
}
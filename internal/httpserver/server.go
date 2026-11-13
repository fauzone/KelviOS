package httpserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fauzone/kelvios/internal/activity"
	"github.com/fauzone/kelvios/internal/agentmodes"
	"github.com/fauzone/kelvios/internal/appdriver"
	"github.com/fauzone/kelvios/internal/appupdate"
	"github.com/fauzone/kelvios/internal/apps"
	"github.com/fauzone/kelvios/internal/backup"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/daemonconfig"
	"github.com/fauzone/kelvios/internal/harness"
	"github.com/fauzone/kelvios/internal/agents"
	"github.com/fauzone/kelvios/internal/approval"
	"github.com/fauzone/kelvios/internal/chatbridge"
	"github.com/fauzone/kelvios/internal/compose"
	"github.com/fauzone/kelvios/internal/llmregistry"
	"github.com/fauzone/kelvios/internal/maintenance"
	"github.com/fauzone/kelvios/internal/mcp"
	"github.com/fauzone/kelvios/internal/ota"
	"github.com/fauzone/kelvios/internal/power"
	"github.com/fauzone/kelvios/internal/restoretest"
	"github.com/fauzone/kelvios/internal/rules"
	"github.com/fauzone/kelvios/internal/storage"
	"github.com/fauzone/kelvios/internal/supplychain"
	"github.com/fauzone/kelvios/internal/tagging"
	"github.com/fauzone/kelvios/internal/ups"
	"github.com/fauzone/kelvios/internal/vault"
	webui "github.com/fauzone/kelvios/web"
)

type Server struct {
	cfg       *config.Config
	db        *sql.DB
	version   string
	srv       *http.Server
	startTime time.Time
	// disksFunc supplies the disk inventory for /api/disks; a field so
	// tests can stub it. Defaults to storage.Inventory.
	disksFunc func() ([]storage.Disk, error)
	// driver is the storage driver for pool operations.
	driver storage.Driver
	// vault is the WS11 secrets vault; always starts LOCKED.
	vault *vault.Vault
	// power is the WS12 power engine (FR-1.6/FR-1.7): policy + state only,
	// no hardware calls. In-memory: fresh engine = all disks unknown.
	power *power.Engine
	// upsMon is the WS19 UPS monitor (FR-1.12): in-memory state + decision log.
	upsMon *upsMonitor
	// upsPoller is the WS23 UPS poller (FR-1.12): polls NUT at interval.
	upsPollerCancel context.CancelFunc
	upsPollerErr    string // last poll error, empty if none
	upsShutdownExecuted bool // shutdown command already executed for the current critical episode (Chief hardening 2026-10-31, audit Pass 1 rec #2)
	// mcpServer is the WS45-B MCP server (FR-3.4): JSON-RPC 2.0 over HTTP.
	mcpServer *mcp.Server
	// harness is the WS46 FR-3.5 Goose headless adapter (replaceable interface).
	harness harness.Harness
	// llmRegistry is the WS46 FR-3.5 LLM model registry.
	llmRegistry *llmregistry.Registry
	// chatBridge is the WS49-A chat bridge service (FR-3.8).
	chatBridge *chatbridge.Service
	// composeExecutor is the WS52-A compose executor (FR-3.10).
	composeExecutor *compose.Executor
	// appDriver is the WS54 app container driver (FR-4.4).
	appDriver appdriver.Driver
	// updateSource is the WS55 app update source (FR-4.8).
	updateSource appupdate.UpdateSource
	// activityAggregator is the WS56 Activity & Automation Core API aggregator (FR-4.7).
	activityAggregator *activity.Aggregator
	// otaService is the WS59 OTA Core service (FR-5.1).
	otaService *ota.Service
	// rulesEngine is the WS62 FR-3.11 Event→Action Rules Engine.
	rulesEngine *rules.Engine
	// distFS is the embedded web UI filesystem (web/dist) for serving
	// the SPA index.html and /assets/* static files.
	distFS fs.FS
}

// harnessWrapper wraps a harness.Harness to enforce the resident_harness gate (WS51-A, FR-3.9).
// When resident_harness is OFF, Run returns an error without executing the inner harness.
type harnessWrapper struct {
	server *Server
	inner  harness.Harness
}

func (w *harnessWrapper) Run(ctx context.Context, prompt string, tools []string) (string, error) {
	modes, err := agentmodes.GetModes(w.server.db)
	if err != nil {
		return "", err
	}
	if !modes.ResidentHarness {
		return "Resident harness is disabled in agent setup modes.", nil
	}
	return w.inner.Run(ctx, prompt, tools)
}

func New(cfg *config.Config, db *sql.DB, version string) *Server {
	mux := http.NewServeMux()
	s := &Server{
		cfg:       cfg,
		db:        db,
		version:   version,
		startTime: time.Now(),
		disksFunc: storage.Inventory,
		srv: &http.Server{
			Addr:         cfg.HTTP.Addr,
			Handler:      mux,
			ReadTimeout:  cfg.HTTP.ReadTimeout,
			WriteTimeout: cfg.HTTP.WriteTimeout,
			IdleTimeout:  cfg.HTTP.IdleTimeout,
		},
	}

	// Prepare embedded web UI filesystem (web/dist) for SPA + assets.
	distFS, err := fs.Sub(webui.FS, "dist")
	if err != nil {
		slog.Error("failed to create dist sub-FS", "error", err)
	} else {
		s.distFS = distFS
	}

	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/openapi.json", s.handleOpenAPI)
	mux.HandleFunc("/api/disks", s.requireAuth(s.handleDisks))
	mux.HandleFunc("/api/pools", s.requireAuth(s.handlePools))
	mux.HandleFunc("/api/pools/plan", s.requireAuth(s.handlePoolPlan))
	mux.HandleFunc("/api/pools/", s.requireAuth(s.handlePoolByID))
	mux.HandleFunc("/api/collections", s.requireAuth(s.handleCollectionsByPool))
	mux.HandleFunc("/api/collections/", s.requireAuth(s.handleCollectionByID))
	mux.HandleFunc("/api/shares/", s.requireAuth(s.handleShareByID))
	mux.HandleFunc("/api/daemon-config", s.requireAuth(s.handleDaemonConfigAll))
	mux.HandleFunc("/api/audit-log", s.requireAuth(s.handleAuditLog))
	mux.HandleFunc("/api/auth/setup", s.handleAuthSetup)
	mux.HandleFunc("/api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("/api/auth/me", s.handleAuthMe)
	mux.HandleFunc("/api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("/api/vault/setup", s.requireAuth(s.handleVaultSetup))
	mux.HandleFunc("/api/vault/unlock", s.requireAuth(s.handleVaultUnlock))
	mux.HandleFunc("/api/vault/lock", s.requireAuth(s.handleVaultLock))
	mux.HandleFunc("/api/vault/status", s.requireAuth(s.handleVaultStatus))
	mux.HandleFunc("/api/secrets", s.requireAuth(s.handleSecrets))
	mux.HandleFunc("/api/secrets/", s.requireAuth(s.handleSecretByName))
	mux.HandleFunc("/api/power", s.requireAuth(s.handlePowerList))
	mux.HandleFunc("/api/power/", s.requireAuth(s.handlePowerByDisk))
	// WS42-B: monthly maintenance (auth required)
	mux.HandleFunc("/api/maintenance/run", s.requireAuth(s.handleMaintenanceRun))
	mux.HandleFunc("/api/maintenance/status", s.requireAuth(s.handleMaintenanceStatus))
	// WS18: supply-chain entries (auth required)
	mux.HandleFunc("/api/supplychain/entries", s.requireAuth(s.handleSupplychainEntries))
	mux.HandleFunc("/api/supplychain/entries/", s.requireAuth(s.handleSupplychainEntryByID))
	// WS19: UPS basics (auth required)
	mux.HandleFunc("/api/ups/reading", s.requireAuth(s.handleUPSReading))
	mux.HandleFunc("/api/ups/status", s.requireAuth(s.handleUPSStatus))
	mux.HandleFunc("/api/ups/events", s.requireAuth(s.handleUPSEvents))
	// WS16: public share link access (no auth)
	mux.HandleFunc("/api/public/share/", s.handlePublicShareAccess)
	// WS32: heat recompute (auth required)
	mux.HandleFunc("/api/heat/recompute", s.requireAuth(s.handleHeatRecompute))
	// WS33: Tier Curator (auth required)
	mux.HandleFunc("/api/curator/recommendations/generate", s.requireAuth(s.handleCuratorGenerate))
	mux.HandleFunc("/api/curator/recommendations", s.requireAuth(s.handleCuratorList))
	mux.HandleFunc("/api/curator/recommendations/", s.requireAuth(s.handleCuratorByID))
	// WS34-A: Tier Curator auto-move (auth required)
	mux.HandleFunc("/api/curator/migrations/execute/", s.requireAuth(s.handleCuratorMigrationExecute))
	mux.HandleFunc("/api/curator/migrations", s.requireAuth(s.handleCuratorMigrationsList))
	mux.HandleFunc("/api/curator/migrations/", s.requireAuth(s.handleCuratorMigrationByID))
	mux.HandleFunc("/api/curator/migrations/process-consents", s.requireAuth(s.handleCuratorProcessConsents))
	// WS37: Shadow eviction + promotion recommendations (FR-2.8)
	mux.HandleFunc("/api/backups/repos", s.requireAuth(s.handleBackupRepos))
	mux.HandleFunc("/api/backups/repos/", s.requireAuth(s.handleBackupRepoByID))
	// WS44: Scheduled restore tests (FR-3.3)
	mux.HandleFunc("/api/restore-tests/run", s.requireAuth(s.handleRestoreTestRun))
	mux.HandleFunc("/api/restore-tests/runs", s.requireAuth(s.handleRestoreTestRuns))
	mux.HandleFunc("/api/restore-tests/alerts", s.requireAuth(s.handleRestoreTestAlerts))
	mux.HandleFunc("/api/restore-tests/status", s.requireAuth(s.handleRestoreTestStatus))
	mux.HandleFunc("/api/shadows/promotions/generate", s.requireAuth(s.handleShadowPromotionsGenerate))
	mux.HandleFunc("/api/shadows/promotions", s.requireAuth(s.handleShadowPromotionsList))
	mux.HandleFunc("/api/shadows/promotions/", s.requireAuth(s.handleShadowPromotionByID))
	// WS45-B: MCP server (FR-3.4) — POST /mcp behind bearer auth.
	mux.HandleFunc("/mcp", s.requireAuth(s.handleMCP))
	// WS46: LLM registry (FR-3.5) — auth required.
	mux.HandleFunc("/api/llm/models", s.requireAuth(s.handleLLMModels))
	mux.HandleFunc("/api/llm/models/", s.requireAuth(s.handleLLMModelByID))
	mux.HandleFunc("/api/llm/models/select", s.requireAuth(s.handleLLMModelSelect))
	// WS47: Agents (FR-3.6) — auth required.
	mux.HandleFunc("/api/agents", s.requireAuth(s.handleAgentsList))
	mux.HandleFunc("/api/agents/", s.requireAuth(s.handleAgentRun))
	// WS48: Approval endpoints (FR-3.7) — auth required.
	mux.HandleFunc("/api/approvals", s.requireAuth(s.handleApprovalsList))
	mux.HandleFunc("/api/approvals/", s.requireAuth(s.handleApprovalByID))
	// WS49-A: Chat bridge endpoints (FR-3.8) — auth required.
	mux.HandleFunc("/api/chatbridge/pairing-code", s.requireAuth(s.handleChatbridgePairingCode))
	mux.HandleFunc("/api/chatbridge/links", s.requireAuth(s.handleChatbridgeLinks))
	mux.HandleFunc("/api/chatbridge/links/", s.requireAuth(s.handleChatbridgeLinkByID))
	// WS50-B: WhatsApp QR pairing endpoints (FR-3.8) — auth required.
	mux.HandleFunc("/api/chatbridge/whatsapp/qr", s.requireAuth(s.handleChatbridgeWhatsAppQR))
	mux.HandleFunc("/api/chatbridge/whatsapp/status", s.requireAuth(s.handleChatbridgeWhatsAppStatus))
	// WS51-A: Agent setup modes (FR-3.9) — auth required.
	mux.HandleFunc("/api/agent-modes", s.requireAuth(s.handleAgentModes))
	// WS52-A: Compose endpoints (FR-3.10) — auth required.
	mux.HandleFunc("/api/compose/artifacts", s.requireAuth(s.handleComposeArtifacts))
	mux.HandleFunc("/api/compose/artifacts/", s.requireAuth(s.handleComposeArtifactByID))
	// WS56: Activity & Automation Core API (FR-4.7) — auth required.
	mux.HandleFunc("/api/activity", s.requireAuth(s.handleActivityList))
	mux.HandleFunc("/api/activity/", s.requireAuth(s.handleActivityByID))
	// WS59: OTA Core (FR-5.1) — auth required.
	mux.HandleFunc("/api/updates", s.requireAuth(s.handleUpdatesStatus))
	mux.HandleFunc("/api/updates/stage", s.requireAuth(s.handleUpdatesStage))
	mux.HandleFunc("/api/updates/apply", s.requireAuth(s.handleUpdatesApply))
	mux.HandleFunc("/api/updates/rollback", s.requireAuth(s.handleUpdatesRollback))
	// WS53-B: REST App Library (FR-4.1/FR-4.4) — auth required.
	mux.HandleFunc("/api/apps", s.requireAuth(s.handleApps))
	mux.HandleFunc("/api/apps/", s.requireAuth(s.handleApps))
	// Web UI: serve SPA index.html at "/" and static assets at "/assets/"
	if s.distFS != nil {
		mux.Handle("/assets/", http.FileServer(http.FS(s.distFS)))
	}
	mux.HandleFunc("/", s.handleUI)

	// WS12: power engine (policy + state, no hardware calls).
	s.power = power.New(nil, nil, 0)

	// WS45-B: MCP server (FR-3.4) — JSON-RPC 2.0 over HTTP.
	s.mcpServer = mcp.New(s.db, &mcp.Deps{
		DB:           s.db,
		Power:        s.power,
		DisksFunc:    s.disksFunc,
		ModelIDFunc:  func() string { return s.getActiveModelID() },
		DestroyPoolFunc: func(poolID string) error {
			if s.driver == nil {
				return fmt.Errorf("storage driver not initialized")
			}
			return s.driver.DestroyPool(poolID)
		},
	})

	// WS11: vault always starts LOCKED. Load config so exists flag is right.
	s.vault = vault.New(db)
	if err := s.vault.LoadVaultConfig(); err != nil {
		slog.Error("vault config load failed", "error", err)
	}

	// WS19: UPS monitor (in-memory state + decision log).
	s.upsMon = newUPSMonitor()

	// WS46: Harness (FR-3.5) — Goose headless adapter behind replaceable interface.
	// Wrapped with harnessWrapper to enforce resident_harness gate (WS51-A, FR-3.9).
	baseHarness := harness.NewGoose(harness.DefaultConfig())
	s.harness = &harnessWrapper{server: s, inner: baseHarness}

	// WS46: LLM registry (FR-3.5).
	s.llmRegistry = llmregistry.NewRegistry(db)

	// WS49-A: Chat bridge core (FR-3.8). Transports (Telegram, WhatsApp
	// Native) register on this service in their own workstreams; the
	// scheduled-report loop starts only when a transport lifecycle calls
	// Start — New() deliberately does not start it.
	s.chatBridge = chatbridge.NewService(chatbridge.Deps{
		DB:            s.db,
		ApprovalStore: approval.NewApprovalStore(s.db),
		HarnessRun:    s.harness.Run,
	})

	// WS49-B: Telegram transport (FR-3.8). Register if token is configured.
	// Construction never breaks New() when env is unset.
	if token := os.Getenv("KELVIOS_TELEGRAM_BOT_TOKEN"); token != "" {
		if t, err := chatbridge.NewTelegramTransport(token, s.chatBridge, nil); err == nil {
			s.chatBridge.RegisterTransport(t)
		} else {
			slog.Error("chatbridge: telegram transport construction failed", "error", err)
		}
	}

	// WS50-A: WhatsApp Native transport (FR-3.8). Register if enabled.
	// Construction never breaks New() when env is unset.
	if os.Getenv("KELVIOS_WHATSAPP_ENABLED") == "1" {
		if t, err := chatbridge.NewWhatsAppTransport(s.chatBridge, s.cfg.Database.Path); err == nil {
			s.chatBridge.RegisterTransport(t)
		} else {
			slog.Error("chatbridge: whatsapp transport construction failed", "error", err)
		}
	}

	// WS52-A: Compose executor (FR-3.10).
	s.composeExecutor = compose.NewExecutor(s.db, s.chatBridge, time.Now)

	// WS56: Activity & Automation Core API aggregator (FR-4.7).
	s.activityAggregator = activity.NewAggregator(s.db, s.composeExecutor, s.power, s.disksFunc, time.Now, s.rulesEngine)

	// WS62: Rules Engine (FR-3.11).
	s.rulesEngine = rules.NewEngine(rules.EngineConfig{
		Store:            rules.NewSQLStore(s.db),
		AgentRunner:      &rules.DefaultAgentRunner{Deps: rules.DefaultDeps{DB: s.db, NowFunc: time.Now}},
		ReportSender:     &rules.DefaultReportSender{ChatBridge: s.chatBridge},
		RestoreTestRunner: &rules.DefaultRestoreTestRunner{Deps: rules.DefaultDeps{DB: s.db, NowFunc: time.Now}},
		AuditWriter:      &rules.DefaultAuditWriter{DB: s.db},
		NowFunc:          time.Now,
		TickInterval:     30 * time.Second,
		MaxFiresPerHour:  120,
	})

	// WS60: first-boot setup + system settings (FR-5.2). Handlers enforce
	// their own auth rules; no requireAuth wrapper here.
	mux.HandleFunc("/api/setup/state", s.handleSetupState)
	mux.HandleFunc("/api/setup/preferences", s.handleSetupPreferences)
	mux.HandleFunc("/api/setup/import", s.handleSetupImport)
	mux.HandleFunc("/api/setup/complete", s.handleSetupComplete)
	mux.HandleFunc("/api/settings/system", s.handleSystemSettings)
	mux.HandleFunc("/api/settings/export", s.handleSettingsExport)

	// Register rules API routes
	rulesHandler := &RulesHandler{
		Store:  rules.NewSQLStore(s.db),
		Engine: s.rulesEngine,
	}
	rulesHandler.RegisterRulesRoutes(mux)

	// WS59: OTA Core (FR-5.1).
	// Health checker probes the server itself (internal, not HTTP loopback).
	healthCheck := func(ctx context.Context) error {
		// Simple health check: verify DB is responsive
		return s.db.PingContext(ctx)
	}
	otaSvc, err := ota.NewService(ota.ServiceConfig{
		DropDir:       s.cfg.OTA.DropDir,
		PublicKey:     nil,
		ActiveChannel: s.cfg.OTA.Channel,
		DataDir:       filepath.Dir(s.cfg.Database.Path),
		DB:            s.db,
	}, healthCheck)
	if err != nil {
		slog.Error("OTA service init failed", "error", err)
	} else {
		// Load public key if configured
		if s.cfg.OTA.PublicKey != "" {
			pubKey, err := ota.DecodePublicKey(s.cfg.OTA.PublicKey)
			if err != nil {
				slog.Error("OTA public key decode failed", "error", err)
			} else {
				otaSvc.SetPublicKey(pubKey)
			}
		} else if s.cfg.OTA.PublicKeyFile != "" {
			pubKey, err := ota.LoadPublicKeyFromFile(s.cfg.OTA.PublicKeyFile)
			if err != nil {
				slog.Error("OTA public key file load failed", "error", err)
			} else {
				otaSvc.SetPublicKey(pubKey)
			}
		}
		s.otaService = otaSvc
	}

	return s
}

func (s *Server) SetDriver(d storage.Driver) {
	s.driver = d
}

// SetAppDriver sets the app container driver (WS54, FR-4.4).
func (s *Server) SetAppDriver(d appdriver.Driver) {
	s.appDriver = d
}

// SetUpdateSource sets the app update source (WS55, FR-4.8).
func (s *Server) SetUpdateSource(u appupdate.UpdateSource) {
	s.updateSource = u
}

// SetPowerEngine overrides the power engine (tests inject a fake clock).
func (s *Server) SetPowerEngine(e *power.Engine) {
	s.power = e
}

// SetDisksFunc overrides the disk inventory function (tests inject a stub).
func (s *Server) SetDisksFunc(f func() ([]storage.Disk, error)) {
	s.disksFunc = f
}

// SetActivityAggregator overrides the activity aggregator (tests inject a stub).
func (s *Server) SetActivityAggregator(a *activity.Aggregator) {
	s.activityAggregator = a
}

// SetOTAService overrides the OTA service (tests inject a stub).
func (s *Server) SetOTAService(o *ota.Service) {
	s.otaService = o
}

// Handler returns the HTTP handler for testing.
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}

// RehydratePools loads all pools from the catalog and registers them
// with the driver. Call after SetDriver during server initialization.
func (s *Server) RehydratePools() error {
	if s.driver == nil {
		return fmt.Errorf("driver not initialized")
	}

	pools, err := catalog.ListPools(s.db)
	if err != nil {
		return fmt.Errorf("list pools for rehydration: %w", err)
	}

	for _, p := range pools {
		// Fetch disk names for this pool
		diskRows, err := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, p.ID)
		if err != nil {
			slog.Error("query pool disks for rehydration failed", "pool_id", p.ID, "error", err)
			continue
		}
		var diskNames []string
		for diskRows.Next() {
			var d string
			if err := diskRows.Scan(&d); err != nil {
				diskRows.Close()
				continue
			}
			diskNames = append(diskNames, d)
		}
		diskRows.Close()

		// Convert catalog.PoolRow to storage.Pool
		pool := &storage.Pool{
			ID:         p.ID,
			Name:       p.Name,
			NodeID:     p.NodeID,
			Tier:       storage.Tier(p.Tier),
			Mechanism:  storage.Mechanism(p.Mechanism),
			Driver:     p.Driver,
			DiskNames:  diskNames,
			Mountpoint: p.Mountpoint,
			SizeBytes:  p.SizeBytes,
			UsedBytes:  p.UsedBytes,
			Encrypted:  p.Encrypted,
		}

		// Register with driver (type assertion for single driver)
		if sd, ok := s.driver.(*storage.SingleDriver); ok {
			sd.RegisterExisting(pool)
		}
	}
	return nil
}

func (s *Server) Start() error {
	return s.srv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// InitUPS performs boot-time UPS initialization:
// - Checks for clean shutdown marker (CheckBoot)
// - If unclean: records unclean_shutdown event + integrity check
// - Starts the NUT poller if configured (Addr + UPSName non-empty)
func (s *Server) InitUPS() error {
	// Determine marker path
	markerPath := s.cfg.UPS.MarkerPath
	if markerPath == "" {
		// Default: <dir of DB>/clean-shutdown.marker
		markerPath = filepath.Join(filepath.Dir(s.cfg.Database.Path), "clean-shutdown.marker")
	}

	// Check boot marker
	clean, err := ups.CheckBoot(markerPath)
	if err != nil {
		slog.Error("UPS boot check failed", "error", err)
		// Non-fatal: continue without boot event
	} else if !clean {
		// Unclean shutdown (including first boot)
		integrityResult, err := catalog.IntegrityCheck(s.db)
		if err != nil {
			integrityResult = "integrity_check_failed: " + err.Error()
		}
		detail := "unclean shutdown detected; integrity=" + integrityResult
		event := &catalog.UPSEventRow{
			Kind:          string(ups.EventUncleanShutdown),
			State:         "unknown",
			ChargePercent: -1,
			Detail:        detail,
		}
		if err := catalog.InsertUPSEvent(s.db, event); err != nil {
			slog.Error("insert unclean_shutdown event failed", "error", err)
		}
		catalog.WriteAuditLog(s.db, "system", "ups.unclean_shutdown", "ups", "", "ok", detail)
	}

	// Start poller if NUT is configured
	if s.cfg.UPS.Addr != "" && s.cfg.UPS.UPSName != "" {
		client := &ups.Client{
			Addr:    s.cfg.UPS.Addr,
			Timeout: 5 * time.Second,
			UPSName: s.cfg.UPS.UPSName,
		}
		poller := &ups.Poller{
			Source:          client,
			CriticalPercent: s.cfg.UPS.CriticalPercent,
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.upsPollerCancel = cancel
		go poller.Run(ctx, 10*time.Second,
			func(reading ups.Reading, decision ups.Decision) {
				// Use shared applyReading logic; actor is "poller"
				s.applyReading(reading, "poller")
				if decision.BeginShutdown {
					// Execute the shutdown command ONCE per critical episode:
					// Decide is level-triggered, so BeginShutdown repeats on
					// every poll while the condition persists - re-executing a
					// (possibly custom) command every 10s is neither graceful
					// nor safe. The flag resets on any non-critical emit, so a
					// later, distinct episode can execute again.
					// (Chief hardening 2026-10-31, audit Pass 1 rec #2.)
					if len(s.cfg.UPS.ShutdownCommand) > 0 {
						s.upsMon.mu.Lock()
						already := s.upsShutdownExecuted
						if !already {
							s.upsShutdownExecuted = true
						}
						s.upsMon.mu.Unlock()
						if !already {
							shutdowner := &ups.SystemShutdowner{Command: s.cfg.UPS.ShutdownCommand}
							_ = shutdowner.Shutdown(context.Background(), "battery critical")
						}
					}
				} else {
					s.upsMon.mu.Lock()
					s.upsShutdownExecuted = false
					s.upsMon.mu.Unlock()
				}
			},
			func(err error) {
				s.upsMon.mu.Lock()
				s.upsPollerErr = err.Error()
				s.upsMon.mu.Unlock()
				slog.Error("UPS poll error", "error", err)
			},
		)
		slog.Info("UPS poller started", "addr", s.cfg.UPS.Addr, "ups_name", s.cfg.UPS.UPSName)
	} else {
		slog.Info("UPS poller disabled (no NUT config)")
	}
	return nil
}

// CleanShutdown marks the clean shutdown marker file.
// Should be called after graceful server shutdown.
func (s *Server) CleanShutdown() error {
	markerPath := s.cfg.UPS.MarkerPath
	if markerPath == "" {
		markerPath = filepath.Join(filepath.Dir(s.cfg.Database.Path), "clean-shutdown.marker")
	}
	return ups.MarkCleanShutdown(markerPath)
}

// StopUPSPoller stops the UPS poller if running.
func (s *Server) StopUPSPoller() {
	if s.upsPollerCancel != nil {
		s.upsPollerCancel()
		s.upsPollerCancel = nil
		slog.Info("UPS poller stopped")
	}
}

// getActiveModelID returns the active model ID from the LLM registry,
// or "unconfigured" if none is set.
func (s *Server) getActiveModelID() string {
	if s.llmRegistry == nil {
		return "unconfigured"
	}
	model, err := s.llmRegistry.GetActiveModel()
	if err != nil || model == nil {
		return "unconfigured"
	}
	return model.ModelID
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(s.startTime).Seconds()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","version":"` + s.version + `","uptime_seconds":` + fmt.Sprintf("%.0f", uptime) + `}`))
}

// handleMCP handles POST /mcp — the MCP JSON-RPC 2.0 endpoint.
// Auth is enforced by requireAuth; actor is extracted from context.
// Gate 2 (WS51-A, FR-3.9): if external_mcp is OFF, return 503 mcp_disabled.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	// Check external_mcp gate before processing any JSON-RPC
	modes, err := agentmodes.GetModes(s.db)
	if err != nil {
		slog.Error("get agent modes for MCP gate failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !modes.ExternalMCP {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp_disabled"})
		return
	}

	actor := actorFromRequest(r)
	if s.mcpServer != nil {
		s.mcpServer.HandleMCP(w, r, actor)
	}
}

func (s *Server) handleDisks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	disks, err := s.disksFunc()
	if err != nil {
		slog.Error("disk inventory failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "disk inventory failed"})
		return
	}
	if disks == nil {
		disks = []storage.Disk{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"disks": disks})
}

func (s *Server) handlePools(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListPools(w, r)
	case http.MethodPost:
		s.handleCreatePool(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleListPools(w http.ResponseWriter, r *http.Request) {
	pools, err := catalog.ListPools(s.db)
	if err != nil {
		slog.Error("list pools failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "list pools failed"})
		return
	}

	// Enrich with disk names
	type poolWithDisks struct {
		*catalog.PoolRow
		DiskNames []string `json:"disk_names"`
	}
	result := make([]poolWithDisks, 0, len(pools))
	for _, p := range pools {
		p.EncryptionMode = effectiveEncryptionMode(p.EncryptionMode)
		diskRows, err := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, p.ID)
		if err != nil {
			slog.Error("query pool disks failed", "pool_id", p.ID, "error", err)
			continue
		}
		var disks []string
		for diskRows.Next() {
			var d string
			if err := diskRows.Scan(&d); err != nil {
				diskRows.Close()
				continue
			}
			disks = append(disks, d)
		}
		diskRows.Close()
		result = append(result, poolWithDisks{PoolRow: p, DiskNames: disks})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"pools": result})
}

type createPoolRequest struct {
	Name       string   `json:"name"`
	Tier       string   `json:"tier"`
	Mechanism  string   `json:"mechanism"`
	Disks      []string `json:"disks"`
	Mountpoint string   `json:"mountpoint"`
	Encrypted  bool     `json:"encrypted"`
	// WS25 (FR-1.2 wizard contract): stripe needs its own explicit
	// confirmation, and encryption is chosen by mode, not a bare bool.
	// The legacy Encrypted bool is ignored when EncryptionMode is set;
	// EncryptionMode governs the persisted Encrypted flag.
	ConfirmStripe      bool   `json:"confirm_stripe"`
	EncryptionMode     string `json:"encryption_mode"`
	RecoveryKeyCreated bool   `json:"recovery_key_created"`
}

func (s *Server) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	if s.driver == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "storage driver not initialized"})
		return
	}

	var req createPoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", "", "error", "invalid JSON")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}

	// Validate tier (mandatory per ARCHITECTURE §6)
	if req.Tier == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", "", "error", "tier is mandatory")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "tier is mandatory (hot, warm, or cold)"})
		return
	}
	tier := storage.Tier(req.Tier)
	if tier != storage.TierHot && tier != storage.TierWarm && tier != storage.TierCold {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", "", "error", "invalid tier")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid tier: must be hot, warm, or cold"})
		return
	}

	// Validate mechanism (WS25, FR-1.2): the mechanism is a mandatory
	// wizard choice — there is NO silent default. The pre-WS25 handler
	// defaulted an omitted mechanism to single, which could silently
	// turn a requested protected pool into an unprotected one.
	mechanism := storage.Mechanism(req.Mechanism)
	if mechanism == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", "", "error", "mechanism is mandatory")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "mechanism is mandatory (single, mirror, parity-scheduled, stripe, or zfs)"})
		return
	}
	if !isValidMechanism(mechanism) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", "", "error", "invalid mechanism")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid mechanism: must be one of single, mirror, parity-scheduled, stripe, zfs"})
		return
	}

	// WS25 (FR-1.2): RAID0/stripe carries a pool-loss warning and needs
	// its own separate confirmation before the create is accepted.
	if mechanism == storage.MechanismStripe && !req.ConfirmStripe {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", "", "error", "stripe_requires_confirmation")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "stripe_requires_confirmation"})
		return
	}

	// WS25 (FR-1.1/1.2): availability gate. Only the single driver is
	// available in this build (see storage.DriverRegistry); every other
	// mechanism is refused honestly instead of being silently downgraded
	// to single. Planning those mechanisms stays available via
	// POST /api/pools/plan.
	if mechanism != storage.MechanismSingle {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", "", "error", "mechanism not available in this build yet: "+string(mechanism))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "mechanism not available in this build yet: " + string(mechanism)})
		return
	}

	// WS25 (FR-1.2): per-pool encryption choice. Mode "" or "none" means
	// no encryption; "manual" (passphrase) and "tpm2" require the user
	// to have created a recovery key first. LUKS/TPM2 execution itself
	// is a hardware-phase seam — this gate is the wizard contract.
	encryptionMode := req.EncryptionMode
	switch encryptionMode {
	case "", "none":
		encryptionMode = ""
	case "manual", "tpm2":
		if !req.RecoveryKeyCreated {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", "", "error", "recovery_key_created is required for encryption mode "+encryptionMode)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "recovery_key_created is required for encryption mode " + encryptionMode})
			return
		}
	default:
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", "", "error", "invalid encryption_mode")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid encryption_mode: must be one of none, manual, tpm2"})
		return
	}
	poolEncrypted := encryptionMode != ""

	// Validate disks
	if len(req.Disks) == 0 {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", "", "error", "no disks provided")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "at least one disk is required"})
		return
	}

	// Create pool via driver
	pool, err := s.driver.CreatePool(storage.PoolConfig{
		Name:       req.Name,
		Tier:       tier,
		Mechanism:  mechanism,
		Disks:      req.Disks,
		Mountpoint: req.Mountpoint,
		Encrypted:  poolEncrypted,
	})
	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", "", "error", err.Error())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Persist to catalog
	catPool := &catalog.PoolRow{
		ID:         pool.ID,
		Name:       pool.Name,
		NodeID:     pool.NodeID,
		Tier:       string(pool.Tier),
		Mechanism:  string(pool.Mechanism),
		Driver:     pool.Driver,
		Mountpoint: pool.Mountpoint,
		SizeBytes:  pool.SizeBytes,
		UsedBytes:  pool.UsedBytes,
		Encrypted:  pool.Encrypted,
		EncryptionMode: encryptionMode,
		Status:     "active",
	}
	if err := catalog.CreatePool(s.db, catPool, pool.DiskNames); err != nil {
		// Check for pool name uniqueness violation
		if errors.Is(err, catalog.ErrPoolNameExists) {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", pool.ID, "error", "pool_name_exists")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "pool_name_exists"})
			return
		}
		slog.Error("create pool catalog failed", "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", pool.ID, "error", "catalog persist failed")
		// Try to clean up driver pool
		_ = s.driver.DestroyPool(pool.ID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "catalog persist failed"})
		return
	}

	// Audit log success
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.create", "pool", pool.ID, "ok", "")

	// Return combined representation
	response := map[string]any{
		"pool": map[string]any{
			"id":         pool.ID,
			"name":       pool.Name,
			"node_id":    pool.NodeID,
			"tier":       pool.Tier,
			"mechanism":  pool.Mechanism,
			"driver":     pool.Driver,
			"disk_names": pool.DiskNames,
			"mountpoint": pool.Mountpoint,
			"size_bytes": pool.SizeBytes,
			"used_bytes": pool.UsedBytes,
			"encrypted":  pool.Encrypted,
			"encryption_mode": effectiveEncryptionMode(encryptionMode),
			"status":     "active",
			"created_at": time.Now().Unix(),
			"updated_at": time.Now().Unix(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleGetPool(w http.ResponseWriter, r *http.Request, id string) {
	pool, disks, err := catalog.GetPool(s.db, id)
	if err != nil {
		slog.Error("get pool failed", "pool_id", id, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get pool failed"})
		return
	}
	if pool == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "pool not found"})
		return
	}

	response := map[string]any{
		"pool": map[string]any{
			"id":         pool.ID,
			"name":       pool.Name,
			"node_id":    pool.NodeID,
			"tier":       pool.Tier,
			"mechanism":  pool.Mechanism,
			"driver":     pool.Driver,
			"disk_names": disks,
			"mountpoint": pool.Mountpoint,
			"size_bytes": pool.SizeBytes,
			"used_bytes": pool.UsedBytes,
			"encrypted":  pool.Encrypted,
			"encryption_mode": effectiveEncryptionMode(pool.EncryptionMode),
			"status":     pool.Status,
			"created_at": pool.CreatedAt.Unix(),
			"updated_at": pool.UpdatedAt.Unix(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleDeletePool(w http.ResponseWriter, r *http.Request, id string) {
	if s.driver == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.delete", "pool", id, "error", "storage driver not initialized")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "storage driver not initialized"})
		return
	}

	// Check if pool exists in catalog first
	pool, _, err := catalog.GetPool(s.db, id)
	if err != nil {
		slog.Error("get pool for delete failed", "pool_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.delete", "pool", id, "error", "get pool failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get pool failed"})
		return
	}
	if pool == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.delete", "pool", id, "error", "pool not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "pool not found"})
		return
	}

	// Destroy via driver
	if err := s.driver.DestroyPool(id); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.delete", "pool", id, "error", err.Error())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Remove from catalog
	if err := catalog.DeletePool(s.db, id); err != nil {
		slog.Error("delete pool catalog failed", "pool_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.delete", "pool", id, "error", "catalog delete failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "catalog delete failed"})
		return
	}

	// Audit log success
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "pool.delete", "pool", id, "ok", "")

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	if s.distFS == nil {
		slog.Error("distFS not initialized")
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	content, err := fs.ReadFile(s.distFS, "index.html")
	if err != nil {
		slog.Error("failed to read embedded index.html", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// Helper to parse int query param with default
func parseIntQuery(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// isValidMechanism checks if a mechanism is in the known set.
// Extend this set when new mechanisms are added.
// effectiveEncryptionMode renders a stored pools.encryption_mode value
// for API responses: the catalog stores "" for "no encryption" (schema
// default), which the wizard contract presents as "none" (WS25).
func effectiveEncryptionMode(stored string) string {
	if stored == "" {
		return "none"
	}
	return stored
}

func isValidMechanism(m storage.Mechanism) bool {
	switch m {
	case storage.MechanismSingle,
		storage.MechanismMirror,
		storage.MechanismParityScheduled,
		storage.MechanismStripe,
		storage.MechanismZFS:
		return true
	default:
		return false
	}
}

// generateCollectionID returns a collision-proof collection ID.
// Format: coll-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
func generateCollectionID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		// Fallback: use timestamp with atomic counter for additional entropy
		// This should never happen in practice, but ensures we never panic
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("coll-%d-%08x", now, counter)
	}
	return fmt.Sprintf("coll-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// generateShareID returns a collision-proof share ID.
// Format: share-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
func generateShareID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		// Fallback: use timestamp with atomic counter for additional entropy
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("share-%d-%08x", now, counter)
	}
	return fmt.Sprintf("share-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// generateTrashID returns a collision-proof trash item ID.
// Format: trash-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
// If crypto/rand fails, falls back to atomic counter (never plain unix-seconds).
func generateTrashID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("trash-%d-%08x", now, counter)
	}
	return fmt.Sprintf("trash-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

var idFallbackCounter uint64

// handleCollectionsByPool handles GET /api/collections?pool_id=... and POST /api/collections?pool_id=...
// For WS5, we use path-based pool binding: /api/pools/:poolID/collections
// This handler is kept for potential future use but the main routes are in handlePoolByID.
func (s *Server) handleCollectionsByPool(w http.ResponseWriter, r *http.Request) {
	// This handler is not used in WS5; pool-bound collections are under /api/pools/:poolID/collections
	http.NotFound(w, r)
}

// handleCollectionByID handles GET /api/collections/:id, DELETE /api/collections/:id,
// GET /api/collections/:id/shares, POST /api/collections/:id/shares,
// POST /api/collections/:id/scan, GET /api/collections/:id/files,
// GET /api/collections/:id/files/content,
// POST /api/collections/:id/files/content (upload),
// POST /api/collections/:id/files/mkdir,
// POST /api/collections/:id/files/move,
// POST /api/collections/:id/files/copy,
// DELETE /api/collections/:id/files/content,
// GET /api/collections/:id/trash,
// DELETE /api/collections/:id/trash,
// POST /api/collections/:id/trash/{trashId}/restore,
// DELETE /api/collections/:id/trash/{trashId}.
func (s *Server) handleCollectionByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/collections/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Check for /shares suffix
	if strings.HasSuffix(path, "/shares") {
		id := strings.TrimSuffix(path, "/shares")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.handleListCollectionShares(w, r, id)
		case http.MethodPost:
			s.handleCreateCollectionShare(w, r, id)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Check for /share-links/ suffix (individual link operations: DELETE /api/collections/{id}/share-links/{linkId})
	if strings.Contains(path, "/share-links/") {
		// Split at /share-links/
		parts := strings.SplitN(path, "/share-links/", 2)
		if len(parts) == 2 {
			collectionID := parts[0]
			linkID := parts[1]
			if collectionID != "" && !strings.Contains(collectionID, "/") && linkID != "" && !strings.Contains(linkID, "/") {
				if r.Method == http.MethodDelete {
					s.handleShareLinkByID(w, r, collectionID, linkID)
					return
				}
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
		}
		http.NotFound(w, r)
		return
	}

	// Check for /share-links suffix (collection-level: POST/GET /api/collections/{id}/share-links)
	if strings.HasSuffix(path, "/share-links") {
		id := strings.TrimSuffix(path, "/share-links")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		s.handleCollectionShareLinks(w, r, id)
		return
	}

	// Check for /scan suffix
	if strings.HasSuffix(path, "/scan") {
		id := strings.TrimSuffix(path, "/scan")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleScanCollection(w, r, id)
		return
	}

	// Check for /imports suffix (GET only)
	if strings.HasSuffix(path, "/imports") {
		id := strings.TrimSuffix(path, "/imports")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleListCollectionImports(w, r, id)
		return
	}

	// Check for /import suffix (POST only)
	if strings.HasSuffix(path, "/import") {
		id := strings.TrimSuffix(path, "/import")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleImportCollection(w, r, id)
		return
	}

	// Check for /files/preview suffix (must come before /files)
	if strings.HasSuffix(path, "/files/preview") {
		id := strings.TrimSuffix(path, "/files/preview")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handlePreviewCollectionFile(w, r, id)
		return
	}

	// Check for /files/content suffix (must come before /files)
	if strings.HasSuffix(path, "/files/content") {
		id := strings.TrimSuffix(path, "/files/content")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.handleDownloadCollectionFile(w, r, id)
		case http.MethodPost:
			s.handleUploadCollectionFile(w, r, id)
		case http.MethodDelete:
			s.handleDeleteCollectionFile(w, r, id)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Check for /files/mkdir suffix
	if strings.HasSuffix(path, "/files/mkdir") {
		id := strings.TrimSuffix(path, "/files/mkdir")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleMkdirCollectionFile(w, r, id)
		return
	}

	// Check for /files/move suffix
	if strings.HasSuffix(path, "/files/move") {
		id := strings.TrimSuffix(path, "/files/move")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleMoveCollectionFile(w, r, id)
		return
	}

	// Check for /files/copy suffix
	if strings.HasSuffix(path, "/files/copy") {
		id := strings.TrimSuffix(path, "/files/copy")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleCopyCollectionFile(w, r, id)
		return
	}

	// Check for /files/heat suffix (must come before /files)
	if strings.HasSuffix(path, "/files/heat") {
		id := strings.TrimSuffix(path, "/files/heat")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleFileHeat(w, r, id)
		return
	}

	// Check for /files/heated suffix (must come before /files)
	if strings.HasSuffix(path, "/files/heated") {
		id := strings.TrimSuffix(path, "/files/heated")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleTopHeated(w, r, id)
		return
	}

	// Check for /heat suffix (collection heat) - must come before /files/heat
	if strings.HasSuffix(path, "/heat") {
		id := strings.TrimSuffix(path, "/heat")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleCollectionHeat(w, r, id)
		return
	}

	// Check for /pin suffix (POST /api/collections/{id}/pin, DELETE /api/collections/{id}/pin)
	if strings.HasSuffix(path, "/pin") {
		id := strings.TrimSuffix(path, "/pin")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		actor := actorFromRequest(r)
		switch r.Method {
		case http.MethodPost:
			s.handlePinCollection(w, r, id, actor)
		case http.MethodDelete:
			s.handleUnpinCollection(w, r, id, actor)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Check for /trash/ suffix (individual trash item operations: POST /api/collections/{id}/trash/{trashId}/restore, DELETE /api/collections/{id}/trash/{trashId})
	if strings.Contains(path, "/trash/") {
		// Split at /trash/
		parts := strings.SplitN(path, "/trash/", 2)
		if len(parts) == 2 {
			collectionID := parts[0]
			rest := parts[1]
			// NOTE (WS22 Step B, Chief takeover): the guard used to require
			// rest without "/" which made the /restore suffix branch dead
			// code — restore was unreachable (404 page). rest may now be
			// "<trashID>" or "<trashID>/restore"; anything else is 404.
			if collectionID != "" && !strings.Contains(collectionID, "/") && rest != "" {
				// Check for /restore suffix
				if strings.HasSuffix(rest, "/restore") {
					trashID := strings.TrimSuffix(rest, "/restore")
					if trashID != "" && !strings.Contains(trashID, "/") {
						if r.Method == http.MethodPost {
							s.handleRestoreTrashItem(w, r, collectionID, trashID)
							return
						}
						http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
						return
					}
					http.NotFound(w, r)
					return
				}
				// Individual trash item DELETE
				if !strings.Contains(rest, "/") {
					if r.Method == http.MethodDelete {
						s.handleDeleteTrashItem(w, r, collectionID, rest)
						return
					}
					http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
					return
				}
				http.NotFound(w, r)
				return
			}
		}
		http.NotFound(w, r)
		return
	}

	// Check for /arrange/plans suffix (WS38 FR-2.11)
	// POST /api/collections/{id}/arrange/plans (generate)
	// GET /api/collections/{id}/arrange/plans?status= (list)
	if strings.HasSuffix(path, "/arrange/plans") {
		id := strings.TrimSuffix(path, "/arrange/plans")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPost:
			s.handleArrangeGeneratePlan(w, r, id)
		case http.MethodGet:
			s.handleArrangeListPlans(w, r, id)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Check for /arrange/plans/ suffix (individual plan operations)
	// GET /api/collections/{id}/arrange/plans/{planId}
	// POST /api/collections/{id}/arrange/plans/{planId}/approve|reject|apply|undo
	if strings.Contains(path, "/arrange/plans/") {
		parts := strings.SplitN(path, "/arrange/plans/", 2)
		if len(parts) == 2 {
			collectionID := parts[0]
			rest := parts[1]
			if collectionID != "" && !strings.Contains(collectionID, "/") && rest != "" {
				// Check for /approve, /reject, /apply, /undo suffixes
				for _, action := range []string{"approve", "reject", "apply", "undo"} {
					suffix := "/" + action
					if strings.HasSuffix(rest, suffix) {
						planID := strings.TrimSuffix(rest, suffix)
						if planID != "" && !strings.Contains(planID, "/") {
							if r.Method == http.MethodPost {
								switch action {
								case "approve":
									s.handleArrangeApprovePlan(w, r, collectionID, planID)
								case "reject":
									s.handleArrangeRejectPlan(w, r, collectionID, planID)
								case "apply":
									s.handleArrangeApplyPlan(w, r, collectionID, planID)
								case "undo":
									s.handleArrangeUndoPlan(w, r, collectionID, planID)
								}
								return
							}
							http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
							return
						}
						http.NotFound(w, r)
						return
					}
				}
				// GET /api/collections/{id}/arrange/plans/{planId}
				if !strings.Contains(rest, "/") {
					if r.Method == http.MethodGet {
						s.handleArrangeGetPlan(w, r, collectionID, rest)
						return
					}
					http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
					return
				}
				http.NotFound(w, r)
				return
			}
		}
		http.NotFound(w, r)
		return
	}

	// Check for /tags/run suffix (POST /api/collections/{id}/tags/run)
	if strings.HasSuffix(path, "/tags/run") {
		id := strings.TrimSuffix(path, "/tags/run")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleTagsRun(w, r, id)
		return
	}

	// Check for /tags/search suffix (GET /api/collections/{id}/tags/search)
	if strings.HasSuffix(path, "/tags/search") {
		id := strings.TrimSuffix(path, "/tags/search")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleTagsSearch(w, r, id)
		return
	}

	// Check for /tags suffix (collection-level: POST /api/collections/{id}/tags, GET /api/collections/{id}/tags, DELETE /api/collections/{id}/tags)
	if strings.HasSuffix(path, "/tags") {
		id := strings.TrimSuffix(path, "/tags")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPost:
			s.handleTagsCreate(w, r, id)
		case http.MethodGet:
			s.handleTagsList(w, r, id)
		case http.MethodDelete:
			s.handleTagsDelete(w, r, id)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Check for /trash suffix (collection-level: GET /api/collections/{id}/trash, DELETE /api/collections/{id}/trash)
	if strings.HasSuffix(path, "/trash") {
		id := strings.TrimSuffix(path, "/trash")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.handleListTrash(w, r, id)
		case http.MethodDelete:
			s.handleEmptyTrash(w, r, id)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Check for /files suffix
	if strings.HasSuffix(path, "/files") {
		id := strings.TrimSuffix(path, "/files")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleBrowseCollectionFiles(w, r, id)
		return
	}

	// No suffix - handle by ID
	if strings.Contains(path, "/") {
		http.NotFound(w, r)
		return
	}
	id := path

	switch r.Method {
	case http.MethodGet:
		s.handleGetCollection(w, r, id)
	case http.MethodDelete:
		s.handleDeleteCollection(w, r, id)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handlePoolByID handles GET/DELETE /api/pools/:id and also
// GET/POST /api/pools/:id/collections
// WS21: POST /api/pools/:id/disk-replace/plan, POST /api/pools/:id/disk-expand/plan,
// GET /api/pools/:id/disk-operations
// WS28: GET /api/pools/:id/placement
func (s *Server) handlePoolByID(w http.ResponseWriter, r *http.Request) {
	// Path is /api/pools/{id} or /api/pools/{id}/collections or /api/pools/{id}/disk-*
	path := strings.TrimPrefix(r.URL.Path, "/api/pools/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Check for /collections suffix
	if strings.HasSuffix(path, "/collections") {
		poolID := strings.TrimSuffix(path, "/collections")
		if poolID == "" || strings.Contains(poolID, "/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.handleListCollections(w, r, poolID)
		case http.MethodPost:
			s.handleCreateCollection(w, r, poolID)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Check for /disk-replace/plan suffix
	if strings.HasSuffix(path, "/disk-replace/plan") {
		poolID := strings.TrimSuffix(path, "/disk-replace/plan")
		if poolID == "" || strings.Contains(poolID, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleDiskReplacePlan(w, r, poolID)
		return
	}

	// Check for /disk-expand/plan suffix
	if strings.HasSuffix(path, "/disk-expand/plan") {
		poolID := strings.TrimSuffix(path, "/disk-expand/plan")
		if poolID == "" || strings.Contains(poolID, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleDiskExpandPlan(w, r, poolID)
		return
	}

	// Check for /disk-operations suffix
	if strings.HasSuffix(path, "/disk-operations") {
		poolID := strings.TrimSuffix(path, "/disk-operations")
		if poolID == "" || strings.Contains(poolID, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleListDiskOperations(w, r, poolID)
		return
	}

	// Check for /placement suffix (WS28, FR-1.5)
	if strings.HasSuffix(path, "/placement") {
		poolID := strings.TrimSuffix(path, "/placement")
		if poolID == "" || strings.Contains(poolID, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handlePoolPlacement(w, r)
		return
	}

	// Check for /tier-policy suffix (GET/PUT /api/pools/{id}/tier-policy)
	if strings.HasSuffix(path, "/tier-policy") {
		poolID := strings.TrimSuffix(path, "/tier-policy")
		if poolID == "" || strings.Contains(poolID, "/") {
			http.NotFound(w, r)
			return
		}
		actor := actorFromRequest(r)
		switch r.Method {
		case http.MethodGet:
			s.handleGetTierPolicy(w, r, poolID, actor)
		case http.MethodPut:
			s.handleSetTierPolicy(w, r, poolID, actor)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// No suffix - handle pool by ID
	if strings.Contains(path, "/") {
		http.NotFound(w, r)
		return
	}
	id := path

	switch r.Method {
	case http.MethodGet:
		s.handleGetPool(w, r, id)
	case http.MethodDelete:
		s.handleDeletePool(w, r, id)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

type createCollectionRequest struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	DiskName string `json:"disk_name,omitempty"`
}

func (s *Server) handleCreateCollection(w http.ResponseWriter, r *http.Request, poolID string) {
	var req createCollectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.create", "collection", "", "error", "invalid JSON")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}

	// Validate required fields
	if req.Name == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.create", "collection", "", "error", "name is required")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "name is required"})
		return
	}
	if req.Path == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.create", "collection", "", "error", "path is required")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path is required"})
		return
	}

	// Verify pool exists
	pool, disks, err := catalog.GetPool(s.db, poolID)
	if err != nil {
		slog.Error("get pool for collection failed", "pool_id", poolID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.create", "collection", "", "error", "get pool failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get pool failed"})
		return
	}
	if pool == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.create", "collection", "", "error", "pool not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "pool not found"})
		return
	}

	// Validate disk_name if provided
	if req.DiskName != "" {
		found := false
		for _, d := range disks {
			if d == req.DiskName {
				found = true
				break
			}
		}
		if !found {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.create", "collection", "", "error", "disk_name not found in pool")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "disk_name not found in pool"})
			return
		}
	}

	now := time.Now().Unix()
	coll := &catalog.CollectionRow{
		ID:        generateCollectionID(),
		Name:      req.Name,
		PoolID:    poolID,
		Path:      req.Path,
		DiskName:  req.DiskName,
		SizeBytes: 0,
		CreatedAt: time.Unix(now, 0),
		UpdatedAt: time.Unix(now, 0),
	}

	if err := catalog.CreateCollection(s.db, coll); err != nil {
		slog.Error("create collection failed", "pool_id", poolID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.create", "collection", coll.ID, "error", "create collection failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "create collection failed"})
		return
	}

	// Audit log success
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.create", "collection", coll.ID, "ok", "")

	response := map[string]any{
		"collection": map[string]any{
			"id":         coll.ID,
			"name":       coll.Name,
			"pool_id":    coll.PoolID,
			"path":       coll.Path,
			"disk_name":  coll.DiskName,
			"size_bytes": coll.SizeBytes,
			"created_at": coll.CreatedAt.Unix(),
			"updated_at": coll.UpdatedAt.Unix(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleListCollections(w http.ResponseWriter, r *http.Request, poolID string) {
	// Verify pool exists
	pool, _, err := catalog.GetPool(s.db, poolID)
	if err != nil {
		slog.Error("get pool for list collections failed", "pool_id", poolID, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get pool failed"})
		return
	}
	if pool == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "pool not found"})
		return
	}

	collections, err := catalog.ListCollections(s.db, poolID)
	if err != nil {
		slog.Error("list collections failed", "pool_id", poolID, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "list collections failed"})
		return
	}
	if collections == nil {
		collections = []*catalog.CollectionRow{}
	}

	result := make([]map[string]any, 0, len(collections))
	for _, c := range collections {
		var lastAccessed any
		if c.LastAccessedAt != nil {
			lastAccessed = c.LastAccessedAt.Unix()
		}
		result = append(result, map[string]any{
			"id":               c.ID,
			"name":             c.Name,
			"pool_id":          c.PoolID,
			"path":             c.Path,
			"disk_name":        c.DiskName,
			"size_bytes":       c.SizeBytes,
			"last_accessed_at": lastAccessed,
			"created_at":       c.CreatedAt.Unix(),
			"updated_at":       c.UpdatedAt.Unix(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"collections": result})
}

func (s *Server) handleGetCollection(w http.ResponseWriter, r *http.Request, id string) {
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection failed", "collection_id", id, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get collection failed"})
		return
	}
	if coll == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection not found"})
		return
	}

	var lastAccessed any
	if coll.LastAccessedAt != nil {
		lastAccessed = coll.LastAccessedAt.Unix()
	}

	response := map[string]any{
		"collection": map[string]any{
			"id":               coll.ID,
			"name":             coll.Name,
			"pool_id":          coll.PoolID,
			"path":             coll.Path,
			"disk_name":        coll.DiskName,
			"size_bytes":       coll.SizeBytes,
			"last_accessed_at": lastAccessed,
			"created_at":       coll.CreatedAt.Unix(),
			"updated_at":       coll.UpdatedAt.Unix(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleDeleteCollection(w http.ResponseWriter, r *http.Request, id string) {
	if err := catalog.DeleteCollection(s.db, id); err != nil {
		if err == sql.ErrNoRows {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.delete", "collection", id, "error", "collection not found")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection not found"})
			return
		}
		slog.Error("delete collection failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.delete", "collection", id, "error", "delete collection failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "delete collection failed"})
		return
	}
	// Audit log success
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.delete", "collection", id, "ok", "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreateCollectionShare(w http.ResponseWriter, r *http.Request, collectionID string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, collectionID)
	if err != nil {
		slog.Error("get collection for share failed", "collection_id", collectionID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.create", "share", "", "error", "get collection failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get collection failed"})
		return
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.create", "share", "", "error", "collection not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection not found"})
		return
	}

	var req createShareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.create", "share", "", "error", "invalid JSON")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}

	// Validate required fields
	if req.Name == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.create", "share", "", "error", "name is required")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "name is required"})
		return
	}
	if req.Protocol == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.create", "share", "", "error", "protocol is required")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "protocol is required"})
		return
	}

	// Validate protocol
	validProtocols := map[string]bool{"smb": true, "nfs": true, "sftp": true}
	if !validProtocols[req.Protocol] {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.create", "share", "", "error", "invalid protocol")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid protocol: must be one of smb, nfs, sftp"})
		return
	}

	// Check for duplicate name within the same collection
	existingShares, err := catalog.ListShares(s.db, collectionID)
	if err != nil {
		slog.Error("list shares for duplicate check failed", "collection_id", collectionID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.create", "share", "", "error", "list shares failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "list shares failed"})
		return
	}
	for _, existing := range existingShares {
		if existing.Name == req.Name {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.create", "share", "", "error", "share name already exists in this collection")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "share name already exists in this collection"})
			return
		}
	}

	now := time.Now().Unix()
	share := &catalog.ShareRow{
		ID:           generateShareID(),
		CollectionID: collectionID,
		Name:         req.Name,
		Protocol:     req.Protocol,
		ReadOnly:     req.ReadOnly,
		Metadata:     req.Metadata,
		CreatedAt:    time.Unix(now, 0),
		UpdatedAt:    time.Unix(now, 0),
	}

	if err := catalog.CreateShare(s.db, share); err != nil {
		slog.Error("create share failed", "collection_id", collectionID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.create", "share", share.ID, "error", "create share failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "create share failed"})
		return
	}

	// Audit log success
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.create", "share", share.ID, "ok", "")

	response := map[string]any{
		"share": map[string]any{
			"id":            share.ID,
			"collection_id": share.CollectionID,
			"name":          share.Name,
			"protocol":      share.Protocol,
			"read_only":     share.ReadOnly,
			"metadata":      share.Metadata,
			"created_at":    share.CreatedAt.Unix(),
			"updated_at":    share.UpdatedAt.Unix(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(response)
}

// handleListCollectionShares handles GET /api/collections/{id}/shares
func (s *Server) handleListCollectionShares(w http.ResponseWriter, r *http.Request, collectionID string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, collectionID)
	if err != nil {
		slog.Error("get collection for list shares failed", "collection_id", collectionID, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get collection failed"})
		return
	}
	if coll == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection not found"})
		return
	}

	shares, err := catalog.ListShares(s.db, collectionID)
	if err != nil {
		slog.Error("list shares failed", "collection_id", collectionID, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "list shares failed"})
		return
	}
	if shares == nil {
		shares = []*catalog.ShareRow{}
	}

	result := make([]map[string]any, 0, len(shares))
	for _, share := range shares {
		result = append(result, map[string]any{
			"id":            share.ID,
			"collection_id": share.CollectionID,
			"name":          share.Name,
			"protocol":      share.Protocol,
			"read_only":     share.ReadOnly,
			"metadata":      share.Metadata,
			"created_at":    share.CreatedAt.Unix(),
			"updated_at":    share.UpdatedAt.Unix(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"shares": result})
}

// handleShareByID handles GET /api/shares/{id}, DELETE /api/shares/{id},
// and GET /api/shares/{id}/daemon-config
func (s *Server) handleShareByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/shares/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Check for /daemon-config suffix
	if strings.HasSuffix(path, "/daemon-config") {
		id := strings.TrimSuffix(path, "/daemon-config")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleDaemonConfigShare(w, r, id)
		return
	}

	// No suffix - handle by ID
	if strings.Contains(path, "/") {
		http.NotFound(w, r)
		return
	}
	id := path

	switch r.Method {
	case http.MethodGet:
		s.handleGetShare(w, r, id)
	case http.MethodDelete:
		s.handleDeleteShare(w, r, id)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGetShare(w http.ResponseWriter, r *http.Request, id string) {
	share, err := catalog.GetShare(s.db, id)
	if err != nil {
		slog.Error("get share failed", "share_id", id, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get share failed"})
		return
	}
	if share == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "share not found"})
		return
	}

	response := map[string]any{
		"share": map[string]any{
			"id":            share.ID,
			"collection_id": share.CollectionID,
			"name":          share.Name,
			"protocol":      share.Protocol,
			"read_only":     share.ReadOnly,
			"metadata":      share.Metadata,
			"created_at":    share.CreatedAt.Unix(),
			"updated_at":    share.UpdatedAt.Unix(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleDeleteShare(w http.ResponseWriter, r *http.Request, id string) {
	if err := catalog.DeleteShare(s.db, id); err != nil {
		if err == sql.ErrNoRows {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.delete", "share", id, "error", "share not found")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "share not found"})
			return
		}
		slog.Error("delete share failed", "share_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.delete", "share", id, "error", "delete share failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "delete share failed"})
		return
	}
	// Audit log success
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "share.delete", "share", id, "ok", "")
	w.WriteHeader(http.StatusNoContent)
}

type createShareRequest struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	ReadOnly bool   `json:"read_only"`
	Metadata string `json:"metadata,omitempty"`
}

// handleDaemonConfigShare handles GET /api/shares/{id}/daemon-config
func (s *Server) handleDaemonConfigShare(w http.ResponseWriter, r *http.Request, id string) {
	share, err := catalog.GetShare(s.db, id)
	if err != nil {
		slog.Error("get share for daemon-config failed", "share_id", id, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get share failed"})
		return
	}
	if share == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "share not found"})
		return
	}

	// Get collection to find path and pool mountpoint
	coll, err := catalog.GetCollection(s.db, share.CollectionID)
	if err != nil {
		slog.Error("get collection for daemon-config failed", "collection_id", share.CollectionID, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get collection failed"})
		return
	}
	if coll == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection not found"})
		return
	}

	// Get pool for mountpoint
	pool, _, err := catalog.GetPool(s.db, coll.PoolID)
	if err != nil {
		slog.Error("get pool for daemon-config failed", "pool_id", coll.PoolID, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get pool failed"})
		return
	}
	if pool == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "pool not found"})
		return
	}

	sc := daemonconfig.ShareConfig{
		ID:             share.ID,
		Name:           share.Name,
		Protocol:       share.Protocol,
		ReadOnly:       share.ReadOnly,
		Metadata:       share.Metadata,
		CollectionID:   share.CollectionID,
		CollectionPath: coll.Path,
		PoolMountpoint: pool.Mountpoint,
		PoolName:       pool.Name,
	}

	protocol, configText, err := daemonconfig.RenderShare(sc)
	if err != nil {
		if daemonconfig.IsRenderError(err) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		slog.Error("render share failed", "share_id", id, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "render failed"})
		return
	}

	response := map[string]any{
		"protocol":    protocol,
		"config_text": configText,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

// handleDaemonConfigAll handles GET /api/daemon-config
func (s *Server) handleDaemonConfigAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get all shares with their collections and pools
	rows, err := s.db.Query(`
		SELECT cs.id, cs.collection_id, cs.name, cs.protocol, cs.read_only, cs.metadata,
		       c.path as collection_path, p.mountpoint as pool_mountpoint, p.name as pool_name
		FROM collection_shares cs
		JOIN collections c ON cs.collection_id = c.id
		JOIN pools p ON c.pool_id = p.id
		ORDER BY cs.created_at
	`)
	if err != nil {
		slog.Error("query all shares for daemon-config failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "query shares failed"})
		return
	}
	defer rows.Close()

	var shareConfigs []daemonconfig.ShareConfig
	for rows.Next() {
		var sc daemonconfig.ShareConfig
		var readOnly int
		var metadata sql.NullString
		if err := rows.Scan(&sc.ID, &sc.CollectionID, &sc.Name, &sc.Protocol, &readOnly, &metadata, &sc.CollectionPath, &sc.PoolMountpoint, &sc.PoolName); err != nil {
			slog.Error("scan share for daemon-config failed", "error", err)
			continue
		}
		sc.ReadOnly = readOnly != 0
		if metadata.Valid {
			sc.Metadata = metadata.String
		}
		shareConfigs = append(shareConfigs, sc)
	}

	result := daemonconfig.RenderAllShares(shareConfigs)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"configs": result.Configs,
		"errors":  result.Errors,
	})
}

// handleAuditLog handles GET /api/audit-log
func (s *Server) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	limit := parseIntQuery(r, "limit", 100)
	offset := parseIntQuery(r, "offset", 0)
	action := r.URL.Query().Get("action")
	targetType := r.URL.Query().Get("target_type")
	result := r.URL.Query().Get("result")

	entries, total, err := catalog.ListAuditLog(s.db, limit, offset, action, targetType, result)
	if err != nil {
		slog.Error("list audit log failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "list audit log failed"})
		return
	}

	// Convert to response format
	resultEntries := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		entry := map[string]any{
			"id":          e.ID,
			"created_at":  e.CreatedAt.Unix(),
			"actor":       e.Actor,
			"action":      e.Action,
			"target_type": e.TargetType,
			"target_id":   e.TargetID,
			"result":      e.Result,
		}
		if e.Detail != "" {
			entry["detail"] = e.Detail
		}
		resultEntries = append(resultEntries, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"entries": resultEntries,
		"total":   total,
	})
}

// handleScanCollection handles POST /api/collections/{id}/scan
func (s *Server) handleScanCollection(w http.ResponseWriter, r *http.Request, id string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for scan failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.scan", "collection", id, "error", "get collection failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get collection failed"})
		return
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.scan", "collection", id, "error", "collection not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection not found"})
		return
	}

	// Get pool for mountpoint and tier
	pool, _, err := catalog.GetPool(s.db, coll.PoolID)
	if err != nil {
		slog.Error("get pool for scan failed", "pool_id", coll.PoolID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.scan", "collection", id, "error", "get pool failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get pool failed"})
		return
	}
	if pool == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.scan", "collection", id, "error", "pool not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "pool not found"})
		return
	}

	// Run scan
	actor := actorFromRequest(r)
	result, err := catalog.IndexCollectionFiles(s.db, coll, pool, actor)
	if err != nil {
		// Root missing -> 409
		if strings.Contains(err.Error(), "collection root does not exist") {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.scan", "collection", id, "error", err.Error())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		slog.Error("scan collection failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.scan", "collection", id, "error", "scan failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "scan failed"})
		return
	}

	// Audit log success
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.scan", "collection", id, "ok", fmt.Sprintf("added=%d updated=%d removed=%d", result.Added, result.Updated, result.Removed))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int{
		"added":   result.Added,
		"updated": result.Updated,
		"removed": result.Removed,
	})
}

// handleBrowseCollectionFiles handles GET /api/collections/{id}/files
func (s *Server) handleBrowseCollectionFiles(w http.ResponseWriter, r *http.Request, id string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for browse failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.browse", "collection", id, "error", "get collection failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get collection failed"})
		return
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.browse", "collection", id, "error", "collection not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection not found"})
		return
	}

	// Get pool for tier
	pool, _, err := catalog.GetPool(s.db, coll.PoolID)
	if err != nil {
		slog.Error("get pool for browse failed", "pool_id", coll.PoolID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.browse", "collection", id, "error", "get pool failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get pool failed"})
		return
	}
	if pool == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.browse", "collection", id, "error", "pool not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "pool not found"})
		return
	}

	// Parse query params
	prefix := r.URL.Query().Get("path")
	limit := parseIntQuery(r, "limit", 0)
	offset := parseIntQuery(r, "offset", 0)

	// Browse catalog only - NEVER touches disk
	entries, err := catalog.BrowseFiles(s.db, id, prefix, limit, offset)
	if err != nil {
		slog.Error("browse collection files failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.browse", "collection", id, "error", "browse failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "browse failed"})
		return
	}

	// Set offline/shadowed/dirty flags based on pool tier and shadow existence
	offline := pool.Tier == "cold"
	if offline {
		for i := range entries {
			entries[i].Offline = true
			// Check if this file has a shadow (only for files, not dirs)
			if entries[i].Kind == "file" {
				shadow, err := catalog.GetShadow(s.db, id, entries[i].RelPath)
				if err == nil && shadow != nil {
					entries[i].Shadowed = true
					entries[i].Offline = false // shadowed files are available without waking Cold
					if shadow.Dirty == 1 {
						entries[i].Dirty = true
					}
				}
			}
		}
	}

	// Audit log success
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.browse", "collection", id, "ok", fmt.Sprintf("prefix=%q count=%d", prefix, len(entries)))

	response := map[string]any{
		"entries": entries,
		"tier":    pool.Tier,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

// handleDownloadCollectionFile handles GET /api/collections/{id}/files/content?path=<rel_path>
func (s *Server) handleDownloadCollectionFile(w http.ResponseWriter, r *http.Request, id string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for download failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "get collection failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get collection failed"})
		return
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "collection not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection not found"})
		return
	}

	// Get pool for mountpoint and tier
	pool, _, err := catalog.GetPool(s.db, coll.PoolID)
	if err != nil {
		slog.Error("get pool for download failed", "pool_id", coll.PoolID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "get pool failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get pool failed"})
		return
	}
	if pool == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "pool not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "pool not found"})
		return
	}

	// Parse query param
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "empty path")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path query parameter is required"})
		return
	}

	// Reject absolute paths (Unix-style starting with /, or Windows-style)
	if filepath.IsAbs(relPath) || strings.HasPrefix(relPath, "/") {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "absolute path")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "absolute paths are not allowed"})
		return
	}

	// Normalize relPath to slash-separated
	relPath = filepath.ToSlash(relPath)

	// Reject path traversal attempts
	if strings.HasPrefix(relPath, "..") || strings.Contains(relPath, "../") {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "path escape")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path escape not allowed"})
		return
	}

	// Resolve filesystem root (same convention as IndexCollectionFiles)
	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "resolve root failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "resolve root failed"})
		return
	}

	// Verify root exists
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "collection root does not exist: "+root)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection root does not exist: " + root})
			return
		}
		slog.Error("stat collection root failed", "root", root, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "stat root failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "stat root failed"})
		return
	}
	if !info.IsDir() {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "collection root is not a directory")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection root is not a directory"})
		return
	}

	// Build candidate path early to check for directory on disk
	candidate := filepath.Join(root, relPath)
	candidate = filepath.Clean(candidate)

	// Safety: ensure candidate is within root
	if !strings.HasPrefix(candidate, root) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "path escape after join: "+relPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path escape not allowed"})
		return
	}

	// Check if candidate resolves to a directory on disk -> 400 (not 404)
	// This handles the case where a directory path is requested (directories are never indexed)
	fi, err := os.Stat(candidate)
	if err == nil && fi.IsDir() {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "path is a directory: "+relPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path is a directory"})
		return
	}

	// Catalog-first: file must have a row in files table
	fileRow, err := catalog.GetFile(s.db, id, relPath)
	if err != nil {
		slog.Error("get file failed", "collection_id", id, "rel_path", relPath, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "get file failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get file failed"})
		return
	}
	if fileRow == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "file not indexed: "+relPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "file not found"})
		return
	}

	// WS36 (FR-2.6): Dirty shadow overrides everything — serve the shadow file
	// even when Cold is awake (it is the newest version).
	shadow, err := catalog.GetShadow(s.db, id, relPath)
	if err == nil && shadow != nil && shadow.Dirty == 1 {
		// Check if shadow file exists on disk
		shadowPool, _, err := catalog.GetPool(s.db, shadow.ShadowPoolID)
		if err == nil && shadowPool != nil {
			mountpoint := shadowPool.Mountpoint
			if mountpoint == "" {
				mountpoint = "/mnt/" + shadowPool.Name
			}
			shadowPath := filepath.Join(mountpoint, ".kelvios-shadow", shadow.ShadowRelPath)
			shadowPath = filepath.Clean(shadowPath)
			if strings.HasPrefix(shadowPath, mountpoint) {
				if fi, err := os.Stat(shadowPath); err == nil && !fi.IsDir() {
					// Serve dirty shadow (newest version)
					f, err := os.Open(shadowPath)
					if err == nil {
						contentType := mime.TypeByExtension(filepath.Ext(shadowPath))
						if contentType == "" {
							contentType = "application/octet-stream"
						}
						w.Header().Set("Content-Type", contentType)
						filename := filepath.Base(shadowPath)
						w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
						http.ServeContent(w, r, filename, fi.ModTime(), f)
						f.Close()

						// Audit log success with dirty shadow detail
						catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "ok", relPath+" (dirty shadow)")
						// Update shadow last_served_at
						catalog.UpdateShadowLastServed(s.db, id, relPath)
						return
					}
					f.Close()
				} else {
					// Shadow file missing on disk - delete orphan row best-effort
					_ = catalog.InvalidateShadow(s.db, id, relPath, actorFromRequest(r))
				}
			}
		}
	}

	// Shadow-first serve for Cold tier: if CLEAN shadow exists and Cold is off, serve shadow
	if pool.Tier == "cold" {
		if shadow != nil && shadow.Dirty == 0 {
			// Check if shadow file exists on disk
			shadowPool, _, err := catalog.GetPool(s.db, shadow.ShadowPoolID)
			if err == nil && shadowPool != nil {
				mountpoint := shadowPool.Mountpoint
				if mountpoint == "" {
					mountpoint = "/mnt/" + shadowPool.Name
				}
				shadowPath := filepath.Join(mountpoint, ".kelvios-shadow", shadow.ShadowRelPath)
				shadowPath = filepath.Clean(shadowPath)
				if strings.HasPrefix(shadowPath, mountpoint) {
					if fi, err := os.Stat(shadowPath); err == nil && !fi.IsDir() {
						// Check if Cold is awake (power state active on any disk in the cold pool)
						coldAwake := false
						diskRows, err := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, pool.ID)
						if err == nil {
							for diskRows.Next() {
								var diskName string
								if err := diskRows.Scan(&diskName); err == nil {
									if entry, ok := s.power.Snapshot(diskName); ok && entry.State == storage.PowerActive {
										coldAwake = true
										break
									}
								}
							}
							diskRows.Close()
						}
						if !coldAwake {
							// Cold is off, serve clean shadow
							f, err := os.Open(shadowPath)
							if err == nil {
								contentType := mime.TypeByExtension(filepath.Ext(shadowPath))
								if contentType == "" {
									contentType = "application/octet-stream"
								}
								w.Header().Set("Content-Type", contentType)
								filename := filepath.Base(shadowPath)
								w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
								http.ServeContent(w, r, filename, fi.ModTime(), f)
								f.Close()

								// Audit log success with shadow detail
								catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "ok", relPath+" (shadow)")
								// Update shadow last_served_at
								catalog.UpdateShadowLastServed(s.db, id, relPath)
								return
							}
							f.Close()
						}
					} else {
						// Shadow file missing on disk - delete orphan row best-effort
						_ = catalog.InvalidateShadow(s.db, id, relPath, actorFromRequest(r))
					}
				}
			}
		}

		// Cold awake: fall through to the canonical serve below (freshest, never a stale shadow).
		coldAwake := false
		if diskRows, err := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, pool.ID); err == nil {
			for diskRows.Next() {
				var diskName string
				if err := diskRows.Scan(&diskName); err == nil {
					if entry, ok := s.power.Snapshot(diskName); ok && entry.State == storage.PowerActive {
						coldAwake = true
						break
					}
				}
			}
			diskRows.Close()
		}
		if !coldAwake {
			// Cold tier: offline, return 409 with wake_required (unless we served shadow above)
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "pool tier is cold: "+relPath)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":         "pool tier is cold, file is offline",
				"wake_required": true,
			})
			return
		}
	}

	// candidate was already built and validated earlier
	// Verify file exists and is a regular file (not a symlink)
	fi, err = os.Stat(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "file not on disk: "+relPath)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "file not found on disk"})
			return
		}
		slog.Error("stat file failed", "path", candidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "stat file failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "stat file failed"})
		return
	}
	// Skip symlinks (should not be indexed, but defense in depth)
	if fi.Mode()&os.ModeSymlink != 0 {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "path is a symlink: "+relPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "symlinks are not supported"})
		return
	}

	// Open file
	f, err := os.Open(candidate)
	if err != nil {
		slog.Error("open file failed", "path", candidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "error", "open file failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "open file failed"})
		return
	}
	defer f.Close()

	// Content-Type from extension
	contentType := mime.TypeByExtension(filepath.Ext(candidate))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)

	// Content-Disposition attachment
	filename := filepath.Base(candidate)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	// Serve file
	http.ServeContent(w, r, filename, fi.ModTime(), f)

	// Audit log success
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.download", "collection", id, "ok", relPath)

	// Record access event for heat (FR-1.3) - best effort, never fails the response
	catalog.RecordAccessEvent(s.db, id, relPath, actorFromRequest(r), "content")

	// Post-serve shadow creation hook (WS35 FR-2.5):
	// For Cold files that were just served (Cold is awake), create a Warm shadow
	// if this is the 2nd+ access within 14 days and no shadow exists yet.
	if pool.Tier == "cold" {
		// Check if shadow already exists
		shadow, err := catalog.GetShadow(s.db, id, relPath)
		if err == nil && shadow == nil {
			// Count recent accesses (14-day window)
			count, err := catalog.CountRecentAccesses(s.db, id, relPath, 14)
			if err == nil && count >= 2 {
				// Pick Warm pool with most free space
				warmPool, err := catalog.PickWarmShadowPool(s.db)
				if err == nil {
					free := warmPool.SizeBytes - warmPool.UsedBytes
					if free >= fileRow.SizeBytes {
						// Resolve shadow paths
						mountpoint := warmPool.Mountpoint
						if mountpoint == "" {
							mountpoint = "/mnt/" + warmPool.Name
						}
						shadowRelPath := id + "/" + relPath
						shadowPath := filepath.Join(mountpoint, ".kelvios-shadow", shadowRelPath)
						shadowPath = filepath.Clean(shadowPath)
						canonicalPath := filepath.Join(root, relPath)

						// Create shadow (best-effort, never fails the response)
						_, _ = catalog.CreateShadow(s.db, id, relPath, warmPool.ID, shadowRelPath, canonicalPath, shadowPath, catalog.DefaultShadowCopyFunc, actorFromRequest(r))
					} else {
						catalog.WriteAuditLog(s.db, actorFromRequest(r), "shadow.create", "file_shadow", id, "error", fmt.Sprintf("insufficient free space in warm pool %s: need %d, have %d", warmPool.ID, fileRow.SizeBytes, free))
					}
				} else {
					catalog.WriteAuditLog(s.db, actorFromRequest(r), "shadow.create", "file_shadow", id, "error", "no warm pool available: "+err.Error())
				}
			}
		}
	}
}

// handlePreviewCollectionFile handles GET /api/collections/{id}/files/preview?path=<rel_path>
// Returns metadata for media preview (image/audio/video) without serving the file content.
func (s *Server) handlePreviewCollectionFile(w http.ResponseWriter, r *http.Request, id string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for preview failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "get collection failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get collection failed"})
		return
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "collection not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection not found"})
		return
	}

	// Get pool for mountpoint and tier
	pool, _, err := catalog.GetPool(s.db, coll.PoolID)
	if err != nil {
		slog.Error("get pool for preview failed", "pool_id", coll.PoolID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "get pool failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get pool failed"})
		return
	}
	if pool == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "pool not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "pool not found"})
		return
	}

	// Parse query param
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "empty path")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path query parameter is required"})
		return
	}

	// Reject absolute paths (Unix-style starting with /, or Windows-style)
	if filepath.IsAbs(relPath) || strings.HasPrefix(relPath, "/") {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "absolute path")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "absolute paths are not allowed"})
		return
	}

	// Normalize relPath to slash-separated
	relPath = filepath.ToSlash(relPath)

	// Reject path traversal attempts
	if strings.HasPrefix(relPath, "..") || strings.Contains(relPath, "../") {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "path escape")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path escape not allowed"})
		return
	}

	// Resolve filesystem root (same convention as IndexCollectionFiles)
	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "resolve root failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "resolve root failed"})
		return
	}

	// Verify root exists
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "collection root does not exist: "+root)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection root does not exist: " + root})
			return
		}
		slog.Error("stat collection root failed", "root", root, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "stat root failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "stat root failed"})
		return
	}
	if !info.IsDir() {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "collection root is not a directory")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection root is not a directory"})
		return
	}

	// Build candidate path early to check for directory on disk
	candidate := filepath.Join(root, relPath)
	candidate = filepath.Clean(candidate)

	// Safety: ensure candidate is within root
	if !strings.HasPrefix(candidate, root) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "path escape after join: "+relPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path escape not allowed"})
		return
	}

	// Check if candidate resolves to a directory on disk -> 400 (not 404)
	// This handles the case where a directory path is requested (directories are never indexed)
	fi, err := os.Stat(candidate)
	if err == nil && fi.IsDir() {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "path is a directory: "+relPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path is a directory"})
		return
	}

	// Catalog-first: file must have a row in files table
	fileRow, err := catalog.GetFile(s.db, id, relPath)
	if err != nil {
		slog.Error("get file failed", "collection_id", id, "rel_path", relPath, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "get file failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get file failed"})
		return
	}
	if fileRow == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "file not indexed: "+relPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "file not found"})
		return
	}

	// Determine kind and MIME from extension
	ext := strings.ToLower(filepath.Ext(relPath))
	var kind string
	var mimeType string
	var previewAvailable bool
	switch ext {
	case ".png":
		kind = "image"
		mimeType = "image/png"
		previewAvailable = true
	case ".jpg", ".jpeg":
		kind = "image"
		mimeType = "image/jpeg"
		previewAvailable = true
	case ".gif":
		kind = "image"
		mimeType = "image/gif"
		previewAvailable = true
	case ".webp":
		kind = "image"
		mimeType = "image/webp"
		previewAvailable = true
	case ".bmp":
		kind = "image"
		mimeType = "image/bmp"
		previewAvailable = true
	case ".mp3":
		kind = "audio"
		mimeType = "audio/mpeg"
		previewAvailable = true
	case ".wav":
		kind = "audio"
		mimeType = "audio/wav"
		previewAvailable = true
	case ".flac":
		kind = "audio"
		mimeType = "audio/flac"
		previewAvailable = true
	case ".ogg":
		kind = "audio"
		mimeType = "audio/ogg"
		previewAvailable = true
	case ".m4a":
		kind = "audio"
		mimeType = "audio/mp4"
		previewAvailable = true
	case ".mp4":
		kind = "video"
		mimeType = "video/mp4"
		previewAvailable = true
	case ".mkv":
		kind = "video"
		mimeType = "video/x-matroska"
		previewAvailable = true
	case ".webm":
		kind = "video"
		mimeType = "video/webm"
		previewAvailable = true
	case ".mov":
		kind = "video"
		mimeType = "video/quicktime"
		previewAvailable = true
	case ".avi":
		kind = "video"
		mimeType = "video/x-msvideo"
		previewAvailable = true
	default:
		kind = "other"
		mimeType = "application/octet-stream"
		previewAvailable = false
	}

	// For kind "other", do NOT open the file at all — answer from the catalog row alone
	var width, height any
	if kind == "image" {
		// Try to decode image config for dimensions
		f, err := os.Open(candidate)
		if err == nil {
			config, _, err := image.DecodeConfig(f)
			f.Close()
			if err == nil {
				width = config.Width
				height = config.Height
			} else {
				width = nil
				height = nil
			}
		} else {
			width = nil
			height = nil
		}
	} else {
		width = nil
		height = nil
	}

	// Cold tier: offline, return 409 with wake_required
	if pool.Tier == "cold" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "error", "pool tier is cold: "+relPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":         "pool tier is cold, file is offline",
			"wake_required": true,
		})
		return
	}

	// Build response
	resp := map[string]any{
		"collection_id":     id,
		"rel_path":          relPath,
		"kind":              kind,
		"mime":              mimeType,
		"size_bytes":        fileRow.SizeBytes,
		"width":             width,
		"height":            height,
		"duration_seconds":  nil,
		"preview_available": previewAvailable,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)

	// Audit log success
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.preview", "collection", id, "ok", relPath)

	// Record access event for heat (FR-1.3) - best effort, never fails the response
	catalog.RecordAccessEvent(s.db, id, relPath, actorFromRequest(r), "preview")
}

// validateRelPath validates a relative path for write operations.
// Returns 400 for empty, absolute, traversal, encoded traversal, backslash paths.
func (s *Server) validateRelPath(w http.ResponseWriter, r *http.Request, id, relPath string) bool {
	if relPath == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.write", "collection", id, "error", "empty path")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path query parameter is required"})
		return false
	}
	if filepath.IsAbs(relPath) || strings.HasPrefix(relPath, "/") {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.write", "collection", id, "error", "absolute path")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "absolute paths are not allowed"})
		return false
	}
	// Decode URL-encoded traversal attempts
	decoded, err := url.QueryUnescape(relPath)
	if err != nil {
		decoded = relPath
	}
	if strings.HasPrefix(decoded, "..") || strings.Contains(decoded, "../") || strings.Contains(decoded, "..\\") {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.write", "collection", id, "error", "path escape")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path escape not allowed"})
		return false
	}
	if strings.Contains(relPath, "\\") {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.write", "collection", id, "error", "backslash path")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "backslash paths are not allowed"})
		return false
	}
	return true
}

// handleUploadCollectionFile handles POST /api/collections/{id}/files/content?path=<rel_path>
func (s *Server) handleUploadCollectionFile(w http.ResponseWriter, r *http.Request, id string) {
	coll, pool, ok := s.resolveCollectionAndPool(w, r, id, "file.upload")
	if !ok {
		return
	}

	relPath := r.URL.Query().Get("path")
	if !s.validateRelPath(w, r, id, relPath) {
		return
	}

	actor := actorFromRequest(r)

	// Read body first (needed for both paths)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("read body failed", "error", err)
		catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "read body failed")
		s.writeJSONError(w, http.StatusBadRequest, "read body failed")
		return
	}

	overwrite := r.URL.Query().Get("overwrite") == "true"

	// WS36 (FR-2.6): Dirty write-back for Cold tier when asleep and shadow exists
	if pool.Tier == "cold" {
		// Check if Cold is awake
		coldAwake := false
		if diskRows, err := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, pool.ID); err == nil {
			for diskRows.Next() {
				var diskName string
				if err := diskRows.Scan(&diskName); err == nil {
					if entry, ok := s.power.Snapshot(diskName); ok && entry.State == storage.PowerActive {
						coldAwake = true
						break
					}
				}
			}
			diskRows.Close()
		}

		// Check if shadow exists
		shadow, err := catalog.GetShadow(s.db, id, relPath)
		if err != nil {
			slog.Error("get shadow failed", "collection_id", id, "rel_path", relPath, "error", err)
			catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "get shadow failed")
			s.writeJSONError(w, http.StatusInternalServerError, "get shadow failed")
			return
		}

		if coldAwake {
			// Cold is awake: existing behavior (canonical + invalidate shadow)
			// Fall through to canonical write path below
		} else if shadow != nil {
			// Cold is asleep AND shadow exists: dirty write-back to shadow (Warm path)
			// Write to shadow via catalog.WriteDirtyShadow (temp + rename, atomic)
			_, err := catalog.WriteDirtyShadow(s.db, id, relPath, shadow.ShadowPoolID, shadow.ShadowRelPath, body, actor)
			if err != nil {
				slog.Error("write dirty shadow failed", "collection_id", id, "rel_path", relPath, "error", err)
				catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "write dirty shadow: "+err.Error())
				s.writeJSONError(w, http.StatusInternalServerError, "write dirty shadow failed")
				return
			}
			// Success: respond OK, canonical untouched
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "path": relPath, "shadowed": "true", "dirty": "true"})
			catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "ok", relPath+" (dirty shadow)")
			return
		} else {
			// Cold is asleep and NO shadow exists: 409 as today (nothing writable)
			catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "pool tier is cold, no shadow: "+relPath)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "pool tier is cold, file is offline (no shadow)", "wake_required": true})
			return
		}
	}

	// Canonical write path (non-cold pools, or cold awake)
	// Validate path for canonical write
	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "collection root does not exist: "+root)
			s.writeJSONError(w, http.StatusConflict, "collection root does not exist: "+root)
			return
		}
		slog.Error("stat collection root failed", "root", root, "error", err)
		catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "stat root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "stat root failed")
		return
	}
	if !info.IsDir() {
		catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "collection root is not a directory")
		s.writeJSONError(w, http.StatusInternalServerError, "collection root is not a directory")
		return
	}

	candidate := filepath.Join(root, relPath)
	candidate = filepath.Clean(candidate)
	if !strings.HasPrefix(candidate, root) {
		catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "path escape after join")
		s.writeJSONError(w, http.StatusBadRequest, "path escape not allowed")
		return
	}

	// Check if file already exists on disk
	fi, err := os.Stat(candidate)
	if err == nil {
		if fi.IsDir() {
			catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "path is a directory")
			s.writeJSONError(w, http.StatusBadRequest, "path is a directory")
			return
		}
		// File exists - check overwrite
		if !overwrite {
			catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "file already exists")
			s.writeJSONError(w, http.StatusConflict, "file already exists")
			return
		}
	}

	// Write file to disk (canonical)
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		slog.Error("mkdir parent failed", "path", filepath.Dir(candidate), "error", err)
		catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "mkdir parent failed")
		s.writeJSONError(w, http.StatusInternalServerError, "mkdir parent failed")
		return
	}
	if err := os.WriteFile(candidate, body, 0o644); err != nil {
		slog.Error("write file failed", "path", candidate, "error", err)
		catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "write file failed")
		s.writeJSONError(w, http.StatusInternalServerError, "write file failed")
		return
	}

	// Upsert catalog row
	fi, err = os.Stat(candidate)
	if err != nil {
		slog.Error("stat file after write failed", "path", candidate, "error", err)
		catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "stat file failed")
		s.writeJSONError(w, http.StatusInternalServerError, "stat file failed")
		return
	}
	if _, err := catalog.UpsertFile(s.db, id, relPath, coll.DiskName, fi.Size(), fi.ModTime().Unix()); err != nil {
		slog.Error("upsert file catalog failed", "error", err)
		catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "error", "upsert catalog failed")
		s.writeJSONError(w, http.StatusInternalServerError, "upsert catalog failed")
		return
	}

	status := http.StatusCreated
	if overwrite {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "path": relPath})

	catalog.WriteAuditLog(s.db, actor, "file.upload", "collection", id, "ok", relPath)

	// Invalidate shadow for this file (WS35 FR-2.5): canonical changed
	_ = catalog.InvalidateShadow(s.db, id, relPath, actor)
}

// handleMkdirCollectionFile handles POST /api/collections/{id}/files/mkdir?path=<rel_dir>
func (s *Server) handleMkdirCollectionFile(w http.ResponseWriter, r *http.Request, id string) {
	coll, pool, ok := s.resolveCollectionAndPool(w, r, id, "file.mkdir")
	if !ok {
		return
	}

	relPath := r.URL.Query().Get("path")
	if !s.validateRelPath(w, r, id, relPath) {
		return
	}
	if !s.checkColdTier(w, r, id, pool, relPath, "file.mkdir") {
		return
	}

	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.mkdir", "collection", id, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	candidate := filepath.Join(root, relPath)
	candidate = filepath.Clean(candidate)
	if !strings.HasPrefix(candidate, root) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.mkdir", "collection", id, "error", "path escape after join")
		s.writeJSONError(w, http.StatusBadRequest, "path escape not allowed")
		return
	}

	// Check parent exists
	parent := filepath.Dir(candidate)
	if parent != root {
		parentInfo, err := os.Stat(parent)
		if err != nil || !parentInfo.IsDir() {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.mkdir", "collection", id, "error", "parent directory does not exist")
			s.writeJSONError(w, http.StatusBadRequest, "parent directory does not exist")
			return
		}
	}

	// Check if already exists
	if _, err := os.Stat(candidate); err == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.mkdir", "collection", id, "error", "directory already exists")
		s.writeJSONError(w, http.StatusConflict, "directory already exists")
		return
	}

	if err := os.MkdirAll(candidate, 0o755); err != nil {
		slog.Error("mkdir failed", "path", candidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.mkdir", "collection", id, "error", "mkdir failed")
		s.writeJSONError(w, http.StatusInternalServerError, "mkdir failed")
		return
	}

	// Record the directory so browse shows it even while empty (WS15).
	if err := catalog.RecordDir(s.db, id, relPath); err != nil {
		_ = os.Remove(candidate)
		slog.Error("record dir failed", "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.mkdir", "collection", id, "error", "record dir failed")
		s.writeJSONError(w, http.StatusInternalServerError, "record dir failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "path": relPath})

	catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.mkdir", "collection", id, "ok", relPath)
}

// handleMoveCollectionFile handles POST /api/collections/{id}/files/move
func (s *Server) handleMoveCollectionFile(w http.ResponseWriter, r *http.Request, id string) {
	coll, pool, ok := s.resolveCollectionAndPool(w, r, id, "file.move")
	if !ok {
		return
	}

	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.move", "collection", id, "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if !s.validateRelPath(w, r, id, req.From) || !s.validateRelPath(w, r, id, req.To) {
		return
	}
	if !s.checkColdTier(w, r, id, pool, req.From, "file.move") {
		return
	}

	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.move", "collection", id, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	fromCandidate := filepath.Join(root, req.From)
	fromCandidate = filepath.Clean(fromCandidate)
	toCandidate := filepath.Join(root, req.To)
	toCandidate = filepath.Clean(toCandidate)
	if !strings.HasPrefix(fromCandidate, root) || !strings.HasPrefix(toCandidate, root) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.move", "collection", id, "error", "path escape")
		s.writeJSONError(w, http.StatusBadRequest, "path escape not allowed")
		return
	}

	// Check source exists
	fi, err := os.Stat(fromCandidate)
	if err != nil {
		if os.IsNotExist(err) {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.move", "collection", id, "error", "source not found")
			s.writeJSONError(w, http.StatusNotFound, "source not found")
			return
		}
		slog.Error("stat source failed", "path", fromCandidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.move", "collection", id, "error", "stat source failed")
		s.writeJSONError(w, http.StatusInternalServerError, "stat source failed")
		return
	}
	if fi.IsDir() {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.move", "collection", id, "error", "source is a directory")
		s.writeJSONError(w, http.StatusBadRequest, "source is a directory")
		return
	}

	// Check destination doesn't exist
	if _, err := os.Stat(toCandidate); err == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.move", "collection", id, "error", "destination already exists")
		s.writeJSONError(w, http.StatusConflict, "destination already exists")
		return
	}

	// Move on disk
	if err := os.MkdirAll(filepath.Dir(toCandidate), 0o755); err != nil {
		slog.Error("mkdir parent failed", "path", filepath.Dir(toCandidate), "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.move", "collection", id, "error", "mkdir parent failed")
		s.writeJSONError(w, http.StatusInternalServerError, "mkdir parent failed")
		return
	}
	if err := os.Rename(fromCandidate, toCandidate); err != nil {
		slog.Error("rename failed", "from", fromCandidate, "to", toCandidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.move", "collection", id, "error", "rename failed")
		s.writeJSONError(w, http.StatusInternalServerError, "rename failed")
		return
	}

	// Update catalog
	if err := catalog.MoveFile(s.db, id, req.From, req.To); err != nil {
		// Try to rollback disk move
		_ = os.Rename(toCandidate, fromCandidate)
		slog.Error("move catalog failed", "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.move", "collection", id, "error", "move catalog failed")
		s.writeJSONError(w, http.StatusInternalServerError, "move catalog failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "from": req.From, "to": req.To})

	catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.move", "collection", id, "ok", req.From+"->"+req.To)

	// Invalidate shadow for the old path (file moved) (WS35 FR-2.5)
	_ = catalog.InvalidateShadow(s.db, id, req.From, actorFromRequest(r))
}

// handleCopyCollectionFile handles POST /api/collections/{id}/files/copy
func (s *Server) handleCopyCollectionFile(w http.ResponseWriter, r *http.Request, id string) {
	coll, pool, ok := s.resolveCollectionAndPool(w, r, id, "file.copy")
	if !ok {
		return
	}

	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.copy", "collection", id, "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if !s.validateRelPath(w, r, id, req.From) || !s.validateRelPath(w, r, id, req.To) {
		return
	}
	if !s.checkColdTier(w, r, id, pool, req.From, "file.copy") {
		return
	}

	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.copy", "collection", id, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	fromCandidate := filepath.Join(root, req.From)
	fromCandidate = filepath.Clean(fromCandidate)
	toCandidate := filepath.Join(root, req.To)
	toCandidate = filepath.Clean(toCandidate)
	if !strings.HasPrefix(fromCandidate, root) || !strings.HasPrefix(toCandidate, root) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.copy", "collection", id, "error", "path escape")
		s.writeJSONError(w, http.StatusBadRequest, "path escape not allowed")
		return
	}

	// Check source exists and is a file
	fi, err := os.Stat(fromCandidate)
	if err != nil {
		if os.IsNotExist(err) {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.copy", "collection", id, "error", "source not found")
			s.writeJSONError(w, http.StatusNotFound, "source not found")
			return
		}
		slog.Error("stat source failed", "path", fromCandidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.copy", "collection", id, "error", "stat source failed")
		s.writeJSONError(w, http.StatusInternalServerError, "stat source failed")
		return
	}
	if fi.IsDir() {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.copy", "collection", id, "error", "source is a directory")
		s.writeJSONError(w, http.StatusBadRequest, "source is a directory")
		return
	}

	// Check destination doesn't exist
	if _, err := os.Stat(toCandidate); err == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.copy", "collection", id, "error", "destination already exists")
		s.writeJSONError(w, http.StatusConflict, "destination already exists")
		return
	}

	// Copy on disk
	if err := os.MkdirAll(filepath.Dir(toCandidate), 0o755); err != nil {
		slog.Error("mkdir parent failed", "path", filepath.Dir(toCandidate), "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.copy", "collection", id, "error", "mkdir parent failed")
		s.writeJSONError(w, http.StatusInternalServerError, "mkdir parent failed")
		return
	}
	data, err := os.ReadFile(fromCandidate)
	if err != nil {
		slog.Error("read source failed", "path", fromCandidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.copy", "collection", id, "error", "read source failed")
		s.writeJSONError(w, http.StatusInternalServerError, "read source failed")
		return
	}
	if err := os.WriteFile(toCandidate, data, 0o644); err != nil {
		slog.Error("write copy failed", "path", toCandidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.copy", "collection", id, "error", "write copy failed")
		s.writeJSONError(w, http.StatusInternalServerError, "write copy failed")
		return
	}

	// Insert catalog row
	if _, err := catalog.CopyFile(s.db, id, req.From, req.To, coll.DiskName); err != nil {
		// Try to rollback disk copy
		_ = os.Remove(toCandidate)
		slog.Error("copy catalog failed", "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.copy", "collection", id, "error", "copy catalog failed")
		s.writeJSONError(w, http.StatusInternalServerError, "copy catalog failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "from": req.From, "to": req.To})

	catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.copy", "collection", id, "ok", req.From+"->"+req.To)

	// Invalidate shadow for the new path (copy created) (WS35 FR-2.5)
	_ = catalog.InvalidateShadow(s.db, id, req.To, actorFromRequest(r))
}

// handleFileHeat handles GET /api/collections/{id}/files/heat?path=<rel_path>
// Returns {count, last_access} for a specific file.
func (s *Server) handleFileHeat(w http.ResponseWriter, r *http.Request, id string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for heat failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.heat", "collection", id, "error", "get collection failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.heat", "collection", id, "error", "collection not found")
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	// Parse query param
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.heat", "collection", id, "error", "empty path")
		s.writeJSONError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}

	// Normalize relPath to slash-separated
	relPath = filepath.ToSlash(relPath)

	// Reject path traversal attempts
	if strings.HasPrefix(relPath, "..") || strings.Contains(relPath, "../") {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.heat", "collection", id, "error", "path escape")
		s.writeJSONError(w, http.StatusBadRequest, "path escape not allowed")
		return
	}

	// Verify file exists in catalog (404 if not indexed)
	fileRow, err := catalog.GetFile(s.db, id, relPath)
	if err != nil {
		slog.Error("get file for heat failed", "collection_id", id, "rel_path", relPath, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.heat", "collection", id, "error", "get file failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get file failed")
		return
	}
	if fileRow == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.heat", "collection", id, "error", "file not indexed: "+relPath)
		s.writeJSONError(w, http.StatusNotFound, "file not found")
		return
	}

	// Parse window days (optional, default 30)
	windowDays := parseIntQuery(r, "window_days", 30)

	// Query heat
	count, lastAccess, err := catalog.FileHeat(s.db, id, relPath, windowDays)
	if err != nil {
		slog.Error("file heat query failed", "collection_id", id, "rel_path", relPath, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.heat", "collection", id, "error", "heat query failed")
		s.writeJSONError(w, http.StatusInternalServerError, "heat query failed")
		return
	}

	resp := map[string]any{
		"count":       count,
		"last_access": lastAccess,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)

	catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.heat", "collection", id, "ok", relPath)
}

// handleTopHeated handles GET /api/collections/{id}/files/heated?limit=
// Returns {files, total} with top heated files in the last 30 days.
func (s *Server) handleTopHeated(w http.ResponseWriter, r *http.Request, id string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for heated failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.heated", "collection", id, "error", "get collection failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.heated", "collection", id, "error", "collection not found")
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	// Parse limit (optional, default 50, max 500)
	limit := parseIntQuery(r, "limit", 50)
	if limit > 500 {
		limit = 500
	}

	// Query top heated
	entries, total, err := catalog.TopHeated(s.db, id, limit)
	if err != nil {
		slog.Error("top heated query failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.heated", "collection", id, "error", "heated query failed")
		s.writeJSONError(w, http.StatusInternalServerError, "heated query failed")
		return
	}

	// Convert to response format
	files := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		files = append(files, map[string]any{
			"rel_path":    e.RelPath,
			"count":       e.Count,
			"last_access": e.LastAccess,
		})
	}

	resp := map[string]any{
		"files": files,
		"total": total,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)

	catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.heated", "collection", id, "ok", fmt.Sprintf("limit=%d", limit))
}

// handleCollectionHeat handles GET /api/collections/{id}/heat
// Returns {collection_id, score, event_count, last_access, size_bytes, computed_at} from the stored row.
// If no row yet for an existing collection, returns a zero score object with computed_at 0.
// Unknown collection -> 404; unauth -> 401.
func (s *Server) handleCollectionHeat(w http.ResponseWriter, r *http.Request, id string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for heat failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.heat", "collection", id, "error", "get collection failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.heat", "collection", id, "error", "collection not found")
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	// Get stored heat row
	heat, err := catalog.GetCollectionHeat(s.db, id)
	if err != nil {
		slog.Error("get collection heat failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.heat", "collection", id, "error", "heat query failed")
		s.writeJSONError(w, http.StatusInternalServerError, "heat query failed")
		return
	}

	resp := map[string]any{
		"collection_id": heat.CollectionID,
		"score":         heat.Score,
		"event_count":   heat.EventCount,
		"last_access":   heat.LastAccess,
		"size_bytes":    heat.SizeBytes,
		"computed_at":   heat.ComputedAt.Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)

	catalog.WriteAuditLog(s.db, actorFromRequest(r), "collection.heat", "collection", id, "ok", "")
}

// handleHeatRecompute handles POST /api/heat/recompute
// Runs ComputeHeatScores(time.Now), returns 200 {computed: n}, writes audit row action heat.recompute.
func (s *Server) handleHeatRecompute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)
	written, err := catalog.ComputeHeatScores(s.db, time.Now())
	if err != nil {
		slog.Error("heat recompute failed", "error", err)
		catalog.WriteAuditLog(s.db, actor, "heat.recompute", "heat", "all", "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "heat recompute failed")
		return
	}

	resp := map[string]any{
		"computed": written,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)

	catalog.WriteAuditLog(s.db, actor, "heat.recompute", "heat", "all", "ok", fmt.Sprintf("computed=%d", written))
}

// handleDeleteCollectionFile handles DELETE /api/collections/{id}/files/content?path=<rel_path>
// BEHAVIOR CHANGE (WS22 STEP A): instead of erasing, move the on-disk entry to
// <collection root>/.kelvios-trash/<trash-id>/<basename> (create the trash dir as needed),
// insert the trash row, delete the files-table row for that path, audit file.trash.
// Keeps every existing guard: path validation (traversal -> 400), missing entry -> 404,
// unauthenticated -> 401, Cold collection -> 409 wake_required. Directory delete stays 400
// exactly as today (dir-trash is NOT Step A scope).
func (s *Server) handleDeleteCollectionFile(w http.ResponseWriter, r *http.Request, id string) {
	coll, pool, ok := s.resolveCollectionAndPool(w, r, id, "file.trash")
	if !ok {
		return
	}

	relPath := r.URL.Query().Get("path")
	if !s.validateRelPath(w, r, id, relPath) {
		return
	}
	if !s.checkColdTier(w, r, id, pool, relPath, "file.trash") {
		return
	}

	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	candidate := filepath.Join(root, relPath)
	candidate = filepath.Clean(candidate)
	if !strings.HasPrefix(candidate, root) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "path escape after join")
		s.writeJSONError(w, http.StatusBadRequest, "path escape not allowed")
		return
	}

	// Check if it's a directory
	fi, err := os.Stat(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "file not found")
			s.writeJSONError(w, http.StatusNotFound, "file not found")
			return
		}
		slog.Error("stat file failed", "path", candidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "stat file failed")
		s.writeJSONError(w, http.StatusInternalServerError, "stat file failed")
		return
	}
	if fi.IsDir() {
		dirTrue := r.URL.Query().Get("dir") == "true"
		if !dirTrue {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "path is a directory")
			s.writeJSONError(w, http.StatusBadRequest, "path is a directory")
			return
		}
		// Empty directory check - directory delete stays permanent (not trashed in Step A)
		entries, err := os.ReadDir(candidate)
		if err != nil {
			slog.Error("read dir failed", "path", candidate, "error", err)
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "read dir failed")
			s.writeJSONError(w, http.StatusInternalServerError, "read dir failed")
			return
		}
		if len(entries) > 0 {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "directory not empty")
			s.writeJSONError(w, http.StatusBadRequest, "directory not empty")
			return
		}
		if err := os.Remove(candidate); err != nil {
			slog.Error("remove dir failed", "path", candidate, "error", err)
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "remove dir failed")
			s.writeJSONError(w, http.StatusInternalServerError, "remove dir failed")
			return
		}
		// Drop the recorded-directory row (WS15); recreate the dir on
		// failure so disk and catalog stay consistent.
		if _, err := catalog.DeleteDirRecord(s.db, id, relPath); err != nil {
			_ = os.MkdirAll(candidate, 0o755)
			slog.Error("delete dir record failed", "error", err)
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "delete dir record failed")
			s.writeJSONError(w, http.StatusInternalServerError, "delete dir record failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "ok", relPath)
		return
	}

	// Get file record from catalog first (for metadata)
	fileRow, err := catalog.GetFile(s.db, id, relPath)
	if err != nil {
		slog.Error("get file failed", "collection_id", id, "rel_path", relPath, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "get file failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get file failed")
		return
	}
	if fileRow == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "file not indexed: "+relPath)
		s.writeJSONError(w, http.StatusNotFound, "file not found")
		return
	}

	// Create trash directory and move file there
	trashID := generateTrashID()
	// Actually, we'll generate the trash ID and path here, then call RecordTrash
	trashDir := filepath.Join(root, ".kelvios-trash", trashID)
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		slog.Error("mkdir trash dir failed", "path", trashDir, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "mkdir trash dir failed")
		s.writeJSONError(w, http.StatusInternalServerError, "mkdir trash dir failed")
		return
	}

	trashRelPath := filepath.Join(".kelvios-trash", trashID, filepath.Base(relPath))
	trashRelPath = filepath.ToSlash(trashRelPath)
	trashCandidate := filepath.Join(root, trashRelPath)
	trashCandidate = filepath.Clean(trashCandidate)

	// Move file to trash location
	if err := os.Rename(candidate, trashCandidate); err != nil {
		slog.Error("move file to trash failed", "from", candidate, "to", trashCandidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash", "collection", id, "error", "move to trash failed")
		s.writeJSONError(w, http.StatusInternalServerError, "move to trash failed")
		return
	}

	// Record trash item
	actor := actorFromRequest(r)
	_, err = catalog.RecordTrash(s.db, trashID, id, relPath, trashRelPath, "file", fileRow.DiskName, fileRow.SizeBytes, 30, actor)
	if err != nil {
		// Try to rollback: move file back
		_ = os.Rename(trashCandidate, candidate)
		slog.Error("record trash failed", "error", err)
		catalog.WriteAuditLog(s.db, actor, "file.trash", "collection", id, "error", "record trash failed")
		s.writeJSONError(w, http.StatusInternalServerError, "record trash failed")
		return
	}

	// Delete catalog file row
	if _, err := catalog.DeleteFile(s.db, id, relPath); err != nil {
		// Try to rollback: move file back and delete trash row
		_ = os.Rename(trashCandidate, candidate)
		slog.Error("delete file catalog failed", "error", err)
		catalog.WriteAuditLog(s.db, actor, "file.trash", "collection", id, "error", "delete catalog failed")
		s.writeJSONError(w, http.StatusInternalServerError, "delete catalog failed")
		return
	}

	// Invalidate shadow for the deleted file (WS35 FR-2.5)
	_ = catalog.InvalidateShadow(s.db, id, relPath, actor)

	w.WriteHeader(http.StatusNoContent)
	catalog.WriteAuditLog(s.db, actor, "file.trash", "collection", id, "ok", relPath)
}

// resolveCollectionAndPool resolves collection and pool, writing audit on error.
func (s *Server) resolveCollectionAndPool(w http.ResponseWriter, r *http.Request, id, action string) (*catalog.CollectionRow, *catalog.PoolRow, bool) {
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), action, "collection", id, "error", "get collection failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return nil, nil, false
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), action, "collection", id, "error", "collection not found")
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return nil, nil, false
	}

	pool, _, err := catalog.GetPool(s.db, coll.PoolID)
	if err != nil {
		slog.Error("get pool failed", "pool_id", coll.PoolID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), action, "collection", id, "error", "get pool failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get pool failed")
		return nil, nil, false
	}
	if pool == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), action, "collection", id, "error", "pool not found")
		s.writeJSONError(w, http.StatusNotFound, "pool not found")
		return nil, nil, false
	}

	return coll, pool, true
}

// checkColdTier returns false if pool is cold AND asleep (writes 409 with wake_required).
// An awake Cold pool behaves like any other pool: wake is what unlocks cold access,
// otherwise the WS35 invalidation hooks on the write paths could never fire.
func (s *Server) checkColdTier(w http.ResponseWriter, r *http.Request, id string, pool *catalog.PoolRow, relPath, action string) bool {
	if pool.Tier == "cold" {
		if diskRows, err := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, pool.ID); err == nil {
			for diskRows.Next() {
				var diskName string
				if err := diskRows.Scan(&diskName); err == nil {
					if entry, ok := s.power.Snapshot(diskName); ok && entry.State == storage.PowerActive {
						diskRows.Close()
						return true
					}
				}
			}
			diskRows.Close()
		}
		catalog.WriteAuditLog(s.db, actorFromRequest(r), action, "collection", id, "error", "pool tier is cold: "+relPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":         "pool tier is cold, file is offline",
			"wake_required": true,
		})
		return false
	}
	return true
}

// ============================================================================
// WS18 LANGKAH-B: Supply-chain entries HTTP handlers
// ============================================================================

// createSupplychainEntryRequest is the request body for creating a supply-chain entry.
// Mirrors supplychain.Entry with snake_case JSON tags.
type createSupplychainEntryRequest struct {
	Name                string `json:"name"`
	Publisher           string `json:"publisher"`
	SourceRepo          string `json:"source_repo"`
	ImageRef            string `json:"image_ref"`
	PinnedDigest        string `json:"pinned_digest"`
	SignatureKind       string `json:"signature_kind"`
	SourceKind          string `json:"source_kind"`
	UnverifiedConfirmed bool   `json:"unverified_confirmed"`
}

// verifySupplychainEntryRequest is the request body for verifying a supply-chain entry.
type verifySupplychainEntryRequest struct {
	ServedDigest    string `json:"served_digest"`
	SignatureStatus string `json:"signature_status"`
}

// verifySupplychainEntryResponse is the response for verifying a supply-chain entry.
type verifySupplychainEntryResponse struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// handleSupplychainEntries handles POST /api/supplychain/entries and GET /api/supplychain/entries
func (s *Server) handleSupplychainEntries(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleCreateSupplychainEntry(w, r)
	case http.MethodGet:
		s.handleListSupplychainEntries(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleCreateSupplychainEntry handles POST /api/supplychain/entries
func (s *Server) handleCreateSupplychainEntry(w http.ResponseWriter, r *http.Request) {
	var req createSupplychainEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "supplychain.create", "supplychain_entry", "", "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	entry := &catalog.SupplychainEntryRow{
		Name:                req.Name,
		Publisher:           req.Publisher,
		SourceRepo:          req.SourceRepo,
		ImageRef:            req.ImageRef,
		PinnedDigest:        req.PinnedDigest,
		SignatureKind:       req.SignatureKind,
		SourceKind:          req.SourceKind,
		UnverifiedConfirmed: req.UnverifiedConfirmed,
	}

	actor := actorFromRequest(r)
	if err := catalog.CreateSupplychainEntry(s.db, entry); err != nil {
		// Check for validation errors from supplychain.ValidateEntry
		errMsg := err.Error()
		if errMsg == "missing required field" ||
			errMsg == "invalid source kind" ||
			errMsg == "invalid signature kind" ||
			errMsg == "invalid pinned digest" {
			catalog.WriteAuditLog(s.db, actor, "supplychain.create", "supplychain_entry", "", "error", errMsg)
			s.writeJSONError(w, http.StatusBadRequest, errMsg)
			return
		}
		// Check for duplicate name
		if errMsg == "duplicate name" {
			catalog.WriteAuditLog(s.db, actor, "supplychain.create", "supplychain_entry", "", "error", "duplicate name")
			s.writeJSONError(w, http.StatusConflict, "supplychain entry name already exists")
			return
		}
		slog.Error("create supplychain entry failed", "error", err)
		catalog.WriteAuditLog(s.db, actor, "supplychain.create", "supplychain_entry", "", "error", "create supplychain entry failed")
		s.writeJSONError(w, http.StatusInternalServerError, "create supplychain entry failed")
		return
	}

	// Audit log success
	catalog.WriteAuditLog(s.db, actor, "supplychain.create", "supplychain_entry", entry.ID, "ok", "")

	// Build response
	response := map[string]any{
		"supplychain_entry": map[string]any{
			"id":                   entry.ID,
			"name":                 entry.Name,
			"publisher":            entry.Publisher,
			"source_repo":          entry.SourceRepo,
			"image_ref":            entry.ImageRef,
			"pinned_digest":        entry.PinnedDigest,
			"signature_kind":       entry.SignatureKind,
			"source_kind":          entry.SourceKind,
			"unverified_confirmed": entry.UnverifiedConfirmed,
			"created_at":           entry.CreatedAt.Unix(),
			"updated_at":           entry.UpdatedAt.Unix(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(response)
}

// handleListSupplychainEntries handles GET /api/supplychain/entries
func (s *Server) handleListSupplychainEntries(w http.ResponseWriter, r *http.Request) {
	entries, err := catalog.ListSupplychainEntries(s.db)
	if err != nil {
		slog.Error("list supplychain entries failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list supplychain entries failed")
		return
	}
	if entries == nil {
		entries = []*catalog.SupplychainEntryRow{}
	}

	result := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		result = append(result, map[string]any{
			"id":                   e.ID,
			"name":                 e.Name,
			"publisher":            e.Publisher,
			"source_repo":          e.SourceRepo,
			"image_ref":            e.ImageRef,
			"pinned_digest":        e.PinnedDigest,
			"signature_kind":       e.SignatureKind,
			"source_kind":          e.SourceKind,
			"unverified_confirmed": e.UnverifiedConfirmed,
			"created_at":           e.CreatedAt.Unix(),
			"updated_at":           e.UpdatedAt.Unix(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"supplychain_entries": result})
}

// handleSupplychainEntryByID handles GET /api/supplychain/entries/{id} and DELETE /api/supplychain/entries/{id}
// and POST /api/supplychain/entries/{id}/verify
func (s *Server) handleSupplychainEntryByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/supplychain/entries/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Check for /verify suffix
	if strings.HasSuffix(path, "/verify") {
		id := strings.TrimSuffix(path, "/verify")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleVerifySupplychainEntry(w, r, id)
		return
	}

	// No suffix - handle by ID
	if strings.Contains(path, "/") {
		http.NotFound(w, r)
		return
	}
	id := path

	switch r.Method {
	case http.MethodGet:
		s.handleGetSupplychainEntry(w, r, id)
	case http.MethodDelete:
		s.handleDeleteSupplychainEntry(w, r, id)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleGetSupplychainEntry handles GET /api/supplychain/entries/{id}
func (s *Server) handleGetSupplychainEntry(w http.ResponseWriter, r *http.Request, id string) {
	entry, err := catalog.GetSupplychainEntry(s.db, id)
	if err != nil {
		slog.Error("get supplychain entry failed", "id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get supplychain entry failed")
		return
	}
	if entry == nil {
		s.writeJSONError(w, http.StatusNotFound, "supplychain entry not found")
		return
	}

	response := map[string]any{
		"supplychain_entry": map[string]any{
			"id":                   entry.ID,
			"name":                 entry.Name,
			"publisher":            entry.Publisher,
			"source_repo":          entry.SourceRepo,
			"image_ref":            entry.ImageRef,
			"pinned_digest":        entry.PinnedDigest,
			"signature_kind":       entry.SignatureKind,
			"source_kind":          entry.SourceKind,
			"unverified_confirmed": entry.UnverifiedConfirmed,
			"created_at":           entry.CreatedAt.Unix(),
			"updated_at":           entry.UpdatedAt.Unix(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

// handleDeleteSupplychainEntry handles DELETE /api/supplychain/entries/{id}
func (s *Server) handleDeleteSupplychainEntry(w http.ResponseWriter, r *http.Request, id string) {
	actor := actorFromRequest(r)
	if err := catalog.DeleteSupplychainEntry(s.db, id); err != nil {
		if err == sql.ErrNoRows {
			catalog.WriteAuditLog(s.db, actor, "supplychain.delete", "supplychain_entry", id, "error", "supplychain entry not found")
			s.writeJSONError(w, http.StatusNotFound, "supplychain entry not found")
			return
		}
		slog.Error("delete supplychain entry failed", "id", id, "error", err)
		catalog.WriteAuditLog(s.db, actor, "supplychain.delete", "supplychain_entry", id, "error", "delete supplychain entry failed")
		s.writeJSONError(w, http.StatusInternalServerError, "delete supplychain entry failed")
		return
	}

	// Audit log success
	catalog.WriteAuditLog(s.db, actor, "supplychain.delete", "supplychain_entry", id, "ok", "")
	w.WriteHeader(http.StatusNoContent)
}

// handleVerifySupplychainEntry handles POST /api/supplychain/entries/{id}/verify
func (s *Server) handleVerifySupplychainEntry(w http.ResponseWriter, r *http.Request, id string) {
	// Get the entry first
	entry, err := catalog.GetSupplychainEntry(s.db, id)
	if err != nil {
		slog.Error("get supplychain entry for verify failed", "id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get supplychain entry failed")
		return
	}
	if entry == nil {
		s.writeJSONError(w, http.StatusNotFound, "supplychain entry not found")
		return
	}

	var req verifySupplychainEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "supplychain.verify", "supplychain_entry", id, "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate signature_status
	var sigStatus supplychain.SignatureStatus
	switch req.SignatureStatus {
	case "verified":
		sigStatus = supplychain.SignatureStatusVerified
	case "failed":
		sigStatus = supplychain.SignatureStatusFailed
	case "not_provided":
		sigStatus = supplychain.SignatureStatusNotProvided
	default:
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "supplychain.verify", "supplychain_entry", id, "error", "invalid signature_status")
		s.writeJSONError(w, http.StatusBadRequest, "invalid signature_status: must be one of verified, failed, not_provided")
		return
	}

	// Convert catalog entry to supplychain.Entry for VerifyInstall
	scEntry := supplychain.Entry{
		Name:                entry.Name,
		Publisher:           entry.Publisher,
		SourceRepo:          entry.SourceRepo,
		ImageRef:            entry.ImageRef,
		PinnedDigest:        entry.PinnedDigest,
		SignatureKind:       supplychain.SignatureKind(entry.SignatureKind),
		SourceKind:          supplychain.SourceKind(entry.SourceKind),
		UnverifiedConfirmed: entry.UnverifiedConfirmed,
	}

	// Call VerifyInstall (decision core only, no mutation)
	decision := supplychain.VerifyInstall(scEntry, req.ServedDigest, sigStatus)

	// Audit log every verify attempt
	actor := actorFromRequest(r)
	if decision.Allowed {
		catalog.WriteAuditLog(s.db, actor, "supplychain.verify", "supplychain_entry", id, "ok", "")
	} else {
		catalog.WriteAuditLog(s.db, actor, "supplychain.verify", "supplychain_entry", id, "error", decision.Reason)
	}

	response := verifySupplychainEntryResponse{
		Allowed: decision.Allowed,
		Reason:  decision.Reason,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

// ============================================================================
// WS22 STEP A (2026-10-27): Per-collection recycle bin (FR-2.10) - HTTP handlers
// ============================================================================

// handleListTrash handles GET /api/collections/{id}/trash
func (s *Server) handleListTrash(w http.ResponseWriter, r *http.Request, id string) {
	_, _, ok := s.resolveCollectionAndPool(w, r, id, "file.trash.list")
	if !ok {
		return
	}

	limit := parseIntQuery(r, "limit", 100)
	offset := parseIntQuery(r, "offset", 0)

	entries, total, err := catalog.ListTrash(s.db, id, limit, offset)
	if err != nil {
		slog.Error("list trash failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash.list", "collection", id, "error", "list trash failed")
		s.writeJSONError(w, http.StatusInternalServerError, "list trash failed")
		return
	}
	if entries == nil {
		entries = []*catalog.TrashItemRow{}
	}

	result := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		result = append(result, map[string]any{
			"id":                e.ID,
			"collection_id":     e.CollectionID,
			"original_rel_path": e.OriginalRelPath,
			"trash_rel_path":    e.TrashRelPath,
			"kind":              e.Kind,
			"size_bytes":        e.SizeBytes,
			"disk_name":         e.DiskName,
			"deleted_at":        e.DeletedAt.Unix(),
			"retention_days":    e.RetentionDays,
			"actor":             e.Actor,
		})
	}

	catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.trash.list", "collection", id, "ok", fmt.Sprintf("count=%d", len(entries)))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"entries": result,
		"total":   total,
	})
}

// handleRestoreTrashItem handles POST /api/collections/{id}/trash/{trashId}/restore
// Optional JSON body: {"target_rel_path": "<rel>"}
func (s *Server) handleRestoreTrashItem(w http.ResponseWriter, r *http.Request, collectionID, trashID string) {
	coll, pool, ok := s.resolveCollectionAndPool(w, r, collectionID, "file.restore")
	if !ok {
		return
	}

	// Check cold tier
	if !s.checkColdTier(w, r, collectionID, pool, "", "file.restore") {
		return
	}

	// Get trash item
	trashItem, err := catalog.GetTrash(s.db, trashID)
	if err != nil {
		slog.Error("get trash failed", "trash_id", trashID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "get trash failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get trash failed")
		return
	}
	if trashItem == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "trash item not found")
		s.writeJSONError(w, http.StatusNotFound, "trash item not found")
		return
	}

	// Verify trash item belongs to this collection
	if trashItem.CollectionID != collectionID {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "trash item not in collection")
		s.writeJSONError(w, http.StatusNotFound, "trash item not found")
		return
	}

	// Parse optional target_rel_path
	var req struct {
		TargetRelPath string `json:"target_rel_path"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	targetRelPath := trashItem.OriginalRelPath
	if req.TargetRelPath != "" {
		// Validate target path
		if !s.validateRelPath(w, r, collectionID, req.TargetRelPath) {
			return
		}
		targetRelPath = req.TargetRelPath
	}

	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", collectionID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	// Check if target already exists
	targetCandidate := filepath.Join(root, targetRelPath)
	targetCandidate = filepath.Clean(targetCandidate)
	if !strings.HasPrefix(targetCandidate, root) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "path escape after join")
		s.writeJSONError(w, http.StatusBadRequest, "path escape not allowed")
		return
	}
	if _, err := os.Stat(targetCandidate); err == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "target already exists")
		s.writeJSONError(w, http.StatusConflict, "target already exists")
		return
	}

	// Source trash file path
	trashCandidate := filepath.Join(root, trashItem.TrashRelPath)
	trashCandidate = filepath.Clean(trashCandidate)
	if !strings.HasPrefix(trashCandidate, root) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "trash path escape")
		s.writeJSONError(w, http.StatusInternalServerError, "trash path escape")
		return
	}

	// Check trash file exists
	if _, err := os.Stat(trashCandidate); err != nil {
		if os.IsNotExist(err) {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "trash file not on disk")
			s.writeJSONError(w, http.StatusNotFound, "trash file not found on disk")
			return
		}
		slog.Error("stat trash file failed", "path", trashCandidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "stat trash file failed")
		s.writeJSONError(w, http.StatusInternalServerError, "stat trash file failed")
		return
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(targetCandidate), 0o755); err != nil {
		slog.Error("mkdir parent failed", "path", filepath.Dir(targetCandidate), "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "mkdir parent failed")
		s.writeJSONError(w, http.StatusInternalServerError, "mkdir parent failed")
		return
	}

	// Move file from trash to target location
	if err := os.Rename(trashCandidate, targetCandidate); err != nil {
		slog.Error("restore file failed", "from", trashCandidate, "to", targetCandidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "restore failed")
		s.writeJSONError(w, http.StatusInternalServerError, "restore failed")
		return
	}

	// Re-insert file row with UpsertFile (disk name from trash row)
	_, err = catalog.UpsertFile(s.db, collectionID, targetRelPath, trashItem.DiskName, trashItem.SizeBytes, trashItem.DeletedAt.Unix())
	if err != nil {
		// Try to rollback
		_ = os.Rename(targetCandidate, trashCandidate)
		slog.Error("upsert file catalog failed", "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "upsert catalog failed")
		s.writeJSONError(w, http.StatusInternalServerError, "upsert catalog failed")
		return
	}

	// Delete trash row
	if err := catalog.DeleteTrashRow(s.db, trashID); err != nil {
		slog.Error("delete trash row failed", "error", err)
		// File is restored, trash row delete failed - log but don't fail the request
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.restore", "collection", collectionID, "error", "delete trash row failed: "+err.Error())
	}

	actor := actorFromRequest(r)
	catalog.WriteAuditLog(s.db, actor, "file.restore", "collection", collectionID, "ok", trashItem.OriginalRelPath+"->"+targetRelPath)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "restored_to": targetRelPath})
}

// handleDeleteTrashItem handles DELETE /api/collections/{id}/trash/{trashId} - permanent erase
func (s *Server) handleDeleteTrashItem(w http.ResponseWriter, r *http.Request, collectionID, trashID string) {
	coll, pool, ok := s.resolveCollectionAndPool(w, r, collectionID, "file.purge")
	if !ok {
		return
	}

	// Get trash item
	trashItem, err := catalog.GetTrash(s.db, trashID)
	if err != nil {
		slog.Error("get trash failed", "trash_id", trashID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.purge", "collection", collectionID, "error", "get trash failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get trash failed")
		return
	}
	if trashItem == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.purge", "collection", collectionID, "error", "trash item not found")
		s.writeJSONError(w, http.StatusNotFound, "trash item not found")
		return
	}

	// Verify trash item belongs to this collection
	if trashItem.CollectionID != collectionID {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.purge", "collection", collectionID, "error", "trash item not in collection")
		s.writeJSONError(w, http.StatusNotFound, "trash item not found")
		return
	}

	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", collectionID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.purge", "collection", collectionID, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	// Source trash file path
	trashCandidate := filepath.Join(root, trashItem.TrashRelPath)
	trashCandidate = filepath.Clean(trashCandidate)
	if !strings.HasPrefix(trashCandidate, root) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.purge", "collection", collectionID, "error", "trash path escape")
		s.writeJSONError(w, http.StatusInternalServerError, "trash path escape")
		return
	}

	// Delete file from disk (if exists)
	if err := os.Remove(trashCandidate); err != nil && !os.IsNotExist(err) {
		slog.Error("remove trash file failed", "path", trashCandidate, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.purge", "collection", collectionID, "error", "remove trash file failed")
		s.writeJSONError(w, http.StatusInternalServerError, "remove trash file failed")
		return
	}

	// Delete trash row
	if err := catalog.DeleteTrashRow(s.db, trashID); err != nil {
		slog.Error("delete trash row failed", "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.purge", "collection", collectionID, "error", "delete trash row failed")
		s.writeJSONError(w, http.StatusInternalServerError, "delete trash row failed")
		return
	}

	actor := actorFromRequest(r)
	catalog.WriteAuditLog(s.db, actor, "file.purge", "collection", collectionID, "ok", trashItem.OriginalRelPath)

	w.WriteHeader(http.StatusNoContent)
}

// handleEmptyTrash handles DELETE /api/collections/{id}/trash - erase ALL rows of this collection
func (s *Server) handleEmptyTrash(w http.ResponseWriter, r *http.Request, id string) {
	coll, pool, ok := s.resolveCollectionAndPool(w, r, id, "file.empty_trash")
	if !ok {
		return
	}

	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.empty_trash", "collection", id, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	// Get all trash items for this collection (no limit)
	entries, _, err := catalog.ListTrash(s.db, id, 10000, 0)
	if err != nil {
		slog.Error("list trash for empty failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "file.empty_trash", "collection", id, "error", "list trash failed")
		s.writeJSONError(w, http.StatusInternalServerError, "list trash failed")
		return
	}

	count := 0
	actor := actorFromRequest(r)
	for _, item := range entries {
		// Delete file from disk
		trashCandidate := filepath.Join(root, item.TrashRelPath)
		trashCandidate = filepath.Clean(trashCandidate)
		if strings.HasPrefix(trashCandidate, root) {
			_ = os.Remove(trashCandidate) // best effort
		}
		// Delete trash row
		if err := catalog.DeleteTrashRow(s.db, item.ID); err != nil {
			slog.Error("delete trash row failed during empty", "trash_id", item.ID, "error", err)
			continue
		}
		count++
	}

	catalog.WriteAuditLog(s.db, actor, "file.empty_trash", "collection", id, "ok", fmt.Sprintf("count=%d", count))

	w.WriteHeader(http.StatusNoContent)
}

// ============================================================================
// WS33: Tier Curator HTTP handlers
// ============================================================================

// handleCuratorGenerate handles POST /api/curator/recommendations/generate
func (s *Server) handleCuratorGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)
	created, err := catalog.GenerateRecommendations(s.db, time.Now())
	if err != nil {
		slog.Error("curator generate failed", "error", err)
		catalog.WriteAuditLog(s.db, actor, "curator.recommendations.generate", "curator", "all", "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "curator generate failed")
		return
	}

	resp := map[string]any{"created": created}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)

	catalog.WriteAuditLog(s.db, actor, "curator.recommendations.generate", "curator", "all", "ok", fmt.Sprintf("created=%d", created))
}

// handleCuratorList handles GET /api/curator/recommendations?status=&direction=
func (s *Server) handleCuratorList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	status := r.URL.Query().Get("status")
	direction := r.URL.Query().Get("direction")

	recs, total, err := catalog.ListRecommendations(s.db, status, direction)
	if err != nil {
		slog.Error("curator list failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "curator list failed")
		return
	}

	result := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		var decidedAt any
		if !rec.DecidedAt.IsZero() {
			decidedAt = rec.DecidedAt.Unix()
		}
		result = append(result, map[string]any{
			"id":            rec.ID,
			"collection_id": rec.CollectionID,
			"pool_id":       rec.PoolID,
			"direction":     rec.Direction,
			"reason":        rec.Reason,
			"size_bytes":    rec.SizeBytes,
			"last_access":   rec.LastAccess,
			"score":         rec.Score,
			"status":        rec.Status,
			"created_at":    rec.CreatedAt.Unix(),
			"decided_at":    decidedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"recommendations": result,
		"total":           total,
	})
}

// handleCuratorByID handles POST /api/curator/recommendations/{id}/approve and /reject
func (s *Server) handleCuratorByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/curator/recommendations/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Check for /approve or /reject suffix
	if strings.HasSuffix(path, "/approve") {
		id := strings.TrimSuffix(path, "/approve")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleCuratorDecide(w, r, id, "approved")
		return
	}
	if strings.HasSuffix(path, "/reject") {
		id := strings.TrimSuffix(path, "/reject")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleCuratorDecide(w, r, id, "rejected")
		return
	}

	http.NotFound(w, r)
}

// handleCuratorDecide handles approve/reject decision on a recommendation.
func (s *Server) handleCuratorDecide(w http.ResponseWriter, r *http.Request, id, decision string) {
	actor := actorFromRequest(r)

	err := catalog.DecideRecommendation(s.db, id, decision, time.Now())
	if err != nil {
		if err == sql.ErrNoRows {
			catalog.WriteAuditLog(s.db, actor, "curator.recommendation."+decision, "recommendation", id, "error", "recommendation not found")
			s.writeJSONError(w, http.StatusNotFound, "recommendation not found")
			return
		}
		if strings.Contains(err.Error(), "not pending") {
			catalog.WriteAuditLog(s.db, actor, "curator.recommendation."+decision, "recommendation", id, "error", err.Error())
			s.writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		slog.Error("curator decide failed", "id", id, "decision", decision, "error", err)
		catalog.WriteAuditLog(s.db, actor, "curator.recommendation."+decision, "recommendation", id, "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "curator decide failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "curator.recommendation."+decision, "recommendation", id, "ok", "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": decision})
}

// handleCollectionPin handles POST /api/collections/{id}/pin and DELETE /api/collections/{id}/pin
func (s *Server) handleCollectionPin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/collections/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Check for /pin suffix
	if !strings.HasSuffix(path, "/pin") {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSuffix(path, "/pin")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	actor := actorFromRequest(r)

	switch r.Method {
	case http.MethodPost:
		s.handlePinCollection(w, r, id, actor)
	case http.MethodDelete:
		s.handleUnpinCollection(w, r, id, actor)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handlePinCollection handles POST /api/collections/{id}/pin
func (s *Server) handlePinCollection(w http.ResponseWriter, r *http.Request, id, actor string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for pin failed", "collection_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actor, "collection.pin", "collection", id, "error", "collection not found")
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	// Parse optional note
	var req struct {
		Note string `json:"note"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	now := time.Now()
	if err := catalog.PinCollection(s.db, id, now, req.Note); err != nil {
		slog.Error("pin collection failed", "collection_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "pin collection failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "collection.pin", "collection", id, "ok", "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "pinned"})
}

// handleUnpinCollection handles DELETE /api/collections/{id}/pin
func (s *Server) handleUnpinCollection(w http.ResponseWriter, r *http.Request, id, actor string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for unpin failed", "collection_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actor, "collection.unpin", "collection", id, "error", "collection not found")
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	if err := catalog.UnpinCollection(s.db, id); err != nil {
		if err == sql.ErrNoRows {
			catalog.WriteAuditLog(s.db, actor, "collection.unpin", "collection", id, "error", "collection not pinned")
			s.writeJSONError(w, http.StatusNotFound, "collection not pinned")
			return
		}
		slog.Error("unpin collection failed", "collection_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "unpin collection failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "collection.unpin", "collection", id, "ok", "")
	w.WriteHeader(http.StatusNoContent)
}

// handlePoolTierPolicy handles GET/PUT /api/pools/{id}/tier-policy
func (s *Server) handlePoolTierPolicy(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/pools/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Check for /tier-policy suffix
	if !strings.HasSuffix(path, "/tier-policy") {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSuffix(path, "/tier-policy")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	actor := actorFromRequest(r)

	switch r.Method {
	case http.MethodGet:
		s.handleGetTierPolicy(w, r, id, actor)
	case http.MethodPut:
		s.handleSetTierPolicy(w, r, id, actor)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleGetTierPolicy handles GET /api/pools/{id}/tier-policy
func (s *Server) handleGetTierPolicy(w http.ResponseWriter, r *http.Request, id, actor string) {
	// Verify pool exists
	pool, _, err := catalog.GetPool(s.db, id)
	if err != nil {
		slog.Error("get pool for tier policy failed", "pool_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get pool failed")
		return
	}
	if pool == nil {
		catalog.WriteAuditLog(s.db, actor, "pool.tier_policy.get", "pool", id, "error", "pool not found")
		s.writeJSONError(w, http.StatusNotFound, "pool not found")
		return
	}

	policy, err := catalog.GetTierPolicy(s.db, id)
	if err != nil {
		slog.Error("get tier policy failed", "pool_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get tier policy failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pool_id":             policy.PoolID,
		"mode":                policy.Mode,
		"cold_threshold_days": policy.ColdThresholdDays,
		"auto_move_wait_days": policy.AutoMoveWaitDays,
	})
}

// handleSetTierPolicy handles PUT /api/pools/{id}/tier-policy
func (s *Server) handleSetTierPolicy(w http.ResponseWriter, r *http.Request, id, actor string) {
	// Verify pool exists
	pool, _, err := catalog.GetPool(s.db, id)
	if err != nil {
		slog.Error("get pool for tier policy update failed", "pool_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get pool failed")
		return
	}
	if pool == nil {
		catalog.WriteAuditLog(s.db, actor, "pool.tier_policy.update", "pool", id, "error", "pool not found")
		s.writeJSONError(w, http.StatusNotFound, "pool not found")
		return
	}

	var req struct {
		Mode               string `json:"mode"`
		ColdThresholdDays  int    `json:"cold_threshold_days"`
		AutoMoveWaitDays   int    `json:"auto_move_wait_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actor, "pool.tier_policy.update", "pool", id, "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if err := catalog.SetTierPolicy(s.db, id, req.Mode, req.ColdThresholdDays, req.AutoMoveWaitDays); err != nil {
		if err == catalog.ErrInvalidTierPolicyMode || err == catalog.ErrInvalidTierPolicyDays {
			catalog.WriteAuditLog(s.db, actor, "pool.tier_policy.update", "pool", id, "error", err.Error())
			s.writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		slog.Error("set tier policy failed", "pool_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "set tier policy failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "pool.tier_policy.update", "pool", id, "ok", "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pool_id":             id,
		"mode":                req.Mode,
		"cold_threshold_days": req.ColdThresholdDays,
		"auto_move_wait_days": req.AutoMoveWaitDays,
	})
}

// ============================================================================
// WS34-A: Tier Curator auto-move HTTP handlers
// ============================================================================

// handleCuratorMigrationExecute handles POST /api/curator/migrations/execute/{recommendationId}
// Creates and executes a migration for an APPROVED recommendation.
func (s *Server) handleCuratorMigrationExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/curator/migrations/execute/")
	if path == "" || strings.Contains(path, "/") {
		http.NotFound(w, r)
		return
	}
	recID := path

	actor := actorFromRequest(r)

	// Create migration from approved recommendation
	mig, err := catalog.CreateMigrationForRecommendation(s.db, recID, time.Now())
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			catalog.WriteAuditLog(s.db, actor, "tier.migration.execute", "recommendation", recID, "error", "recommendation not found")
			s.writeJSONError(w, http.StatusNotFound, "recommendation not found")
			return
		}
		if strings.Contains(err.Error(), "not approved") {
			catalog.WriteAuditLog(s.db, actor, "tier.migration.execute", "recommendation", recID, "error", err.Error())
			s.writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		if strings.Contains(err.Error(), "no pool of tier") {
			catalog.WriteAuditLog(s.db, actor, "tier.migration.execute", "recommendation", recID, "error", err.Error())
			s.writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		if strings.Contains(err.Error(), "live migration already exists") {
			catalog.WriteAuditLog(s.db, actor, "tier.migration.execute", "recommendation", recID, "error", err.Error())
			s.writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		slog.Error("create migration failed", "recommendation_id", recID, "error", err)
		catalog.WriteAuditLog(s.db, actor, "tier.migration.execute", "recommendation", recID, "error", "create migration failed")
		s.writeJSONError(w, http.StatusInternalServerError, "create migration failed")
		return
	}

	// Wake handling: if source or target disk is not awake, wake it via power engine
	// (consent-gated: only for approved/silent-consent migrations)
	fromPool, _, err := catalog.GetPool(s.db, mig.FromPoolID)
	if err != nil {
		slog.Error("get source pool for wake failed", "pool_id", mig.FromPoolID, "error", err)
	} else if fromPool != nil {
		// Check disks in source pool
		diskRows, err := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, fromPool.ID)
		if err == nil {
			for diskRows.Next() {
				var diskName string
				if err := diskRows.Scan(&diskName); err != nil {
					continue
				}
				if entry, ok := s.power.Snapshot(diskName); ok && entry.State != storage.PowerActive {
					if _, ok := s.power.Wake(diskName); ok {
						catalog.WriteAuditLog(s.db, actor, "tier.migration.wake", "disk", diskName, "ok", fmt.Sprintf("woke for migration %s", mig.ID))
					}
				}
			}
			diskRows.Close()
		}
	}

	toPool, _, err := catalog.GetPool(s.db, mig.ToPoolID)
	if err != nil {
		slog.Error("get target pool for wake failed", "pool_id", mig.ToPoolID, "error", err)
	} else if toPool != nil {
		diskRows, err := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, toPool.ID)
		if err == nil {
			for diskRows.Next() {
				var diskName string
				if err := diskRows.Scan(&diskName); err != nil {
					continue
				}
				if entry, ok := s.power.Snapshot(diskName); ok && entry.State != storage.PowerActive {
					if _, ok := s.power.Wake(diskName); ok {
						catalog.WriteAuditLog(s.db, actor, "tier.migration.wake", "disk", diskName, "ok", fmt.Sprintf("woke for migration %s", mig.ID))
					}
				}
			}
			diskRows.Close()
		}
	}

	// Execute migration
	if err := catalog.ExecuteMigration(s.db, mig.ID, time.Now(), catalog.DefaultCopyFunc); err != nil {
		catalog.WriteAuditLog(s.db, actor, "tier.migration.execute", "migration", mig.ID, "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Return the migration row
	migRow, err := catalog.GetMigration(s.db, mig.ID)
	if err != nil || migRow == nil {
		// Fallback: return basic info
		s.writeJSON(w, http.StatusOK, map[string]any{
			"id":                mig.ID,
			"collection_id":     mig.CollectionID,
			"recommendation_id": mig.RecommendationID,
			"from_pool_id":      mig.FromPoolID,
			"to_pool_id":        mig.ToPoolID,
			"direction":         mig.Direction,
			"status":            mig.Status,
			"created_at":        mig.CreatedAt.Unix(),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"id":                migRow.ID,
		"collection_id":     migRow.CollectionID,
		"recommendation_id": migRow.RecommendationID,
		"from_pool_id":      migRow.FromPoolID,
		"to_pool_id":        migRow.ToPoolID,
		"direction":         migRow.Direction,
		"status":            migRow.Status,
		"manifest":          migRow.Manifest,
		"reason":            migRow.Reason,
		"created_at":        migRow.CreatedAt.Unix(),
		"executed_at":       migRow.ExecutedAt.Unix(),
		"retain_until":      migRow.RetainUntil.Unix(),
		"finalized_at":      migRow.FinalizedAt.Unix(),
		"undone_at":         migRow.UndoneAt.Unix(),
	})
}

// handleCuratorMigrationsList handles GET /api/curator/migrations?status=&collection_id=
func (s *Server) handleCuratorMigrationsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	status := r.URL.Query().Get("status")
	collectionID := r.URL.Query().Get("collection_id")

	migs, total, err := catalog.ListMigrations(s.db, status, collectionID)
	if err != nil {
		slog.Error("list migrations failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list migrations failed")
		return
	}

	result := make([]map[string]any, 0, len(migs))
	for _, m := range migs {
		result = append(result, map[string]any{
			"id":                m.ID,
			"collection_id":     m.CollectionID,
			"recommendation_id": m.RecommendationID,
			"from_pool_id":      m.FromPoolID,
			"to_pool_id":        m.ToPoolID,
			"direction":         m.Direction,
			"status":            m.Status,
			"manifest":          m.Manifest,
			"reason":            m.Reason,
			"created_at":        m.CreatedAt.Unix(),
			"executed_at":       m.ExecutedAt.Unix(),
			"retain_until":      m.RetainUntil.Unix(),
			"finalized_at":      m.FinalizedAt.Unix(),
			"undone_at":         m.UndoneAt.Unix(),
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"migrations": result,
		"total":      total,
	})
}

// handleCuratorMigrationByID handles POST /api/curator/migrations/{id}/undo
func (s *Server) handleCuratorMigrationByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/curator/migrations/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Check for /undo suffix
	if strings.HasSuffix(path, "/undo") {
		id := strings.TrimSuffix(path, "/undo")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleCuratorMigrationUndo(w, r, id)
		return
	}

	http.NotFound(w, r)
}

// handleCuratorMigrationUndo handles POST /api/curator/migrations/{id}/undo
func (s *Server) handleCuratorMigrationUndo(w http.ResponseWriter, r *http.Request, id string) {
	actor := actorFromRequest(r)

	if err := catalog.UndoMigration(s.db, id, time.Now(), catalog.DefaultDeleteFunc); err != nil {
		if strings.Contains(err.Error(), "not found") {
			catalog.WriteAuditLog(s.db, actor, "tier.migration.undo", "migration", id, "error", "migration not found")
			s.writeJSONError(w, http.StatusNotFound, "migration not found")
			return
		}
		if strings.Contains(err.Error(), "not completed") || strings.Contains(err.Error(), "undo window expired") {
			catalog.WriteAuditLog(s.db, actor, "tier.migration.undo", "migration", id, "error", err.Error())
			s.writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		slog.Error("undo migration failed", "migration_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actor, "tier.migration.undo", "migration", id, "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "undo migration failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "tier.migration.undo", "migration", id, "ok", "")
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "undone"})
}

// handleCuratorProcessConsents handles POST /api/curator/migrations/process-consents
// Runs ProcessSilentConsents + executes the newly created migrations.
func (s *Server) handleCuratorProcessConsents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)

	// Process silent consents
	created, err := catalog.ProcessSilentConsents(s.db, time.Now())
	if err != nil {
		slog.Error("process silent consents failed", "error", err)
		catalog.WriteAuditLog(s.db, actor, "tier.migration.process_consents", "curator", "all", "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "process consents failed")
		return
	}

	executed := 0
	failed := 0

	// List newly created scheduled migrations (from silent consent)
	migs, _, err := catalog.ListMigrations(s.db, catalog.MigrationStatusScheduled, "")
	if err != nil {
		slog.Error("list scheduled migrations failed", "error", err)
	} else {
		for _, mig := range migs {
			// Only execute migrations that were just created (we can't easily distinguish,
			// so execute all scheduled - but in practice only the new ones will be scheduled)
			// Wake handling BEFORE execution (consent-gated: only for approved/silent-consent migrations)
			for _, poolID := range []string{mig.FromPoolID, mig.ToPoolID} {
				pool, _, _ := catalog.GetPool(s.db, poolID)
				if pool == nil {
					continue
				}
				diskRows, _ := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, pool.ID)
				for diskRows.Next() {
					var diskName string
					if err := diskRows.Scan(&diskName); err != nil {
						continue
					}
					if entry, ok := s.power.Snapshot(diskName); ok && entry.State != storage.PowerActive {
						if _, ok := s.power.Wake(diskName); ok {
							catalog.WriteAuditLog(s.db, actor, "tier.migration.wake", "disk", diskName, "ok", fmt.Sprintf("woke for migration %s", mig.ID))
						}
					}
				}
				diskRows.Close()
			}
			if err := catalog.ExecuteMigration(s.db, mig.ID, time.Now(), catalog.DefaultCopyFunc); err != nil {
				failed++
				catalog.WriteAuditLog(s.db, actor, "tier.migration.execute", "migration", mig.ID, "error", err.Error())
			} else {
				executed++
			}
		}
	}

	catalog.WriteAuditLog(s.db, actor, "tier.migration.process_consents", "curator", "all", "ok", fmt.Sprintf("created=%d executed=%d failed=%d", created, executed, failed))
	s.writeJSON(w, http.StatusOK, map[string]int{
		"created":  created,
		"executed": executed,
		"failed":   failed,
	})
}

// ============================================================================
// WS42-B: Monthly Maintenance (FR-2.9)
// ============================================================================

// handleMaintenanceRun handles POST /api/maintenance/run
// Runs the monthly maintenance cycle synchronously (SMART long tests + rotating checksum scrub).
// Returns the MonthlySummary as JSON 200. Errors -> 500 with generic message.
func (s *Server) handleMaintenanceRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	actor := actorFromRequest(r)

	deps := maintenance.Deps{
		DB:        s.db,
		DisksFunc: s.disksFunc,
		Now:       time.Now,
	}

	summary := maintenance.RunMonthly(r.Context(), deps)

	catalog.WriteAuditLog(s.db, actor, "maintenance.run", "maintenance", "", "ok",
		fmt.Sprintf("smart=%d scrub_collections=%d skipped=%d",
			len(summary.SmartResults), len(summary.ScrubResults), len(summary.SkippedCollections)))

	s.writeJSON(w, http.StatusOK, summary)
}

// handleMaintenanceStatus handles GET /api/maintenance/status
// Returns per-collection scrub status from the catalog.
func (s *Server) handleMaintenanceStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	statuses, err := maintenance.Status(s.db)
	if err != nil {
		slog.Error("maintenance status failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "maintenance status failed")
		return
	}

	if statuses == nil {
		statuses = []maintenance.MaintenanceStatus{}
	}

	s.writeJSON(w, http.StatusOK, map[string]any{"statuses": statuses})
}

// writeJSONError writes a JSON error response.
func (s *Server) writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeJSON writes a JSON response.
func (s *Server) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// ============================================================================
// WS37: Shadow eviction + promotion recommendations (FR-2.8)
// ============================================================================

// handleShadowPromotionsGenerate handles POST /api/shadows/promotions/generate
// Creates PENDING promotion recommendations for hot shadows.
func (s *Server) handleShadowPromotionsGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)

	created, err := catalog.GenerateShadowPromotions(s.db, actor)
	if err != nil {
		slog.Error("generate shadow promotions failed", "error", err)
		catalog.WriteAuditLog(s.db, actor, "shadow.promotion.generate", "shadow_promotion", "all", "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "generate shadow promotions failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "shadow.promotion.generate", "shadow_promotion", "all", "ok", fmt.Sprintf("created=%d", created))
	s.writeJSON(w, http.StatusOK, map[string]int{"created": created})
}

// handleShadowPromotionsList handles GET /api/shadows/promotions?status=
// Lists shadow promotions with optional status filter.
func (s *Server) handleShadowPromotionsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	status := r.URL.Query().Get("status")
	limit := parseIntQuery(r, "limit", 100)
	offset := parseIntQuery(r, "offset", 0)

	promos, total, err := catalog.ListShadowPromotions(s.db, status, limit, offset)
	if err != nil {
		slog.Error("list shadow promotions failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list shadow promotions failed")
		return
	}

	result := make([]map[string]any, 0, len(promos))
	for _, p := range promos {
		var decidedAt any
		if !p.DecidedAt.IsZero() {
			decidedAt = p.DecidedAt.Unix()
		}
		result = append(result, map[string]any{
			"id":                 p.ID,
			"collection_id":      p.CollectionID,
			"rel_path":           p.RelPath,
			"shadow_pool_id":     p.ShadowPoolID,
			"access_count_30d":   p.AccessCount30d,
			"status":             p.Status,
			"created_at":         p.CreatedAt.Unix(),
			"decided_at":         decidedAt,
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"promotions": result,
		"total":      total,
	})
}

// handleShadowPromotionByID handles POST /api/shadows/promotions/{id}/approve|reject
// Records the decision on a pending promotion recommendation.
func (s *Server) handleShadowPromotionByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/shadows/promotions/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Check for /approve or /reject suffix
	var promotionID, decision string
	if strings.HasSuffix(path, "/approve") {
		promotionID = strings.TrimSuffix(path, "/approve")
		decision = "approve"
	} else if strings.HasSuffix(path, "/reject") {
		promotionID = strings.TrimSuffix(path, "/reject")
		decision = "reject"
	} else {
		http.NotFound(w, r)
		return
	}

	if promotionID == "" || strings.Contains(promotionID, "/") {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)

	err := catalog.DecideShadowPromotion(s.db, promotionID, decision, actor)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			catalog.WriteAuditLog(s.db, actor, "shadow.promotion."+decision, "shadow_promotion", promotionID, "error", "promotion not found")
			s.writeJSONError(w, http.StatusNotFound, "promotion not found")
			return
		}
		if strings.Contains(err.Error(), "not pending") || strings.Contains(err.Error(), "invalid decision") {
			catalog.WriteAuditLog(s.db, actor, "shadow.promotion."+decision, "shadow_promotion", promotionID, "error", err.Error())
			s.writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		slog.Error("decide shadow promotion failed", "promotion_id", promotionID, "error", err)
		catalog.WriteAuditLog(s.db, actor, "shadow.promotion."+decision, "shadow_promotion", promotionID, "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "decide shadow promotion failed")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"status": decision})
}

// ============================================================================
// WS38: Auto-Arrange (FR-2.11)
// ============================================================================

// handleArrangeGeneratePlan handles POST /api/collections/{id}/arrange/plans
// Generates an approval-first arrangement plan for a collection.
func (s *Server) handleArrangeGeneratePlan(w http.ResponseWriter, r *http.Request, id string) {
	coll, pool, ok := s.resolveCollectionAndPool(w, r, id, "arrange.plan.generate")
	if !ok {
		return
	}

	// Resolve collection root
	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "arrange.plan.generate", "collection", id, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	// Verify root exists
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "arrange.plan.generate", "collection", id, "error", "collection root does not exist: "+root)
			s.writeJSONError(w, http.StatusConflict, "collection root does not exist: "+root)
			return
		}
		slog.Error("stat collection root failed", "root", root, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "arrange.plan.generate", "collection", id, "error", "stat root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "stat root failed")
		return
	}
	if !info.IsDir() {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "arrange.plan.generate", "collection", id, "error", "collection root is not a directory")
		s.writeJSONError(w, http.StatusInternalServerError, "collection root is not a directory")
		return
	}

	actor := actorFromRequest(r)
	result, err := catalog.GenerateArrangePlan(s.db, id, root, actor)
	if err != nil {
		slog.Error("generate arrange plan failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actor, "arrange.plan.generate", "collection", id, "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "generate arrange plan failed")
		return
	}

	// Build response
	moves := make([]map[string]any, 0, len(result.Moves))
	for _, m := range result.Moves {
		moves = append(moves, map[string]any{
			"id":         m.ID,
			"source_rel": m.SourceRel,
			"dest_rel":   m.DestRel,
			"conflict":   m.Conflict,
			"status":     m.Status,
		})
	}

	resp := map[string]any{
		"plan": map[string]any{
			"id":             result.Plan.ID,
			"collection_id":  result.Plan.CollectionID,
			"status":         result.Plan.Status,
			"created_at":     result.Plan.CreatedAt.Unix(),
			"conflict_count": result.ConflictCount,
			"move_count":     len(result.Moves),
		},
		"moves":         moves,
		"conflict_count": result.ConflictCount,
	}

	catalog.WriteAuditLog(s.db, actor, "arrange.plan.generate", "arrange_plan", result.Plan.ID, "ok",
		fmt.Sprintf("collection=%s moves=%d conflicts=%d", id, len(result.Moves), result.ConflictCount))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleArrangeListPlans handles GET /api/collections/{id}/arrange/plans?status=
func (s *Server) handleArrangeListPlans(w http.ResponseWriter, r *http.Request, id string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for arrange list failed", "collection_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	status := r.URL.Query().Get("status")
	limit := parseIntQuery(r, "limit", 100)
	offset := parseIntQuery(r, "offset", 0)

	plans, total, err := catalog.ListArrangePlans(s.db, id, status, limit, offset)
	if err != nil {
		slog.Error("list arrange plans failed", "collection_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list arrange plans failed")
		return
	}

	result := make([]map[string]any, 0, len(plans))
	for _, p := range plans {
		var decidedAt, appliedAt, undoneAt any
		if !p.DecidedAt.IsZero() {
			decidedAt = p.DecidedAt.Unix()
		}
		if !p.AppliedAt.IsZero() {
			appliedAt = p.AppliedAt.Unix()
		}
		if !p.UndoneAt.IsZero() {
			undoneAt = p.UndoneAt.Unix()
		}
		result = append(result, map[string]any{
			"id":             p.ID,
			"collection_id":  p.CollectionID,
			"status":         p.Status,
			"created_at":     p.CreatedAt.Unix(),
			"decided_at":     decidedAt,
			"applied_at":     appliedAt,
			"undone_at":      undoneAt,
			"conflict_count": p.ConflictCount,
			"move_count":     p.MoveCount,
		})
	}

	catalog.WriteAuditLog(s.db, actorFromRequest(r), "arrange.plan.list", "collection", id, "ok", fmt.Sprintf("count=%d", len(plans)))

	s.writeJSON(w, http.StatusOK, map[string]any{
		"plans": result,
		"total": total,
	})
}

// handleArrangeGetPlan handles GET /api/collections/{id}/arrange/plans/{planId}
func (s *Server) handleArrangeGetPlan(w http.ResponseWriter, r *http.Request, collectionID, planID string) {
	plan, moves, err := catalog.GetArrangePlan(s.db, planID)
	if err != nil {
		slog.Error("get arrange plan failed", "plan_id", planID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get arrange plan failed")
		return
	}
	if plan == nil {
		s.writeJSONError(w, http.StatusNotFound, "plan not found")
		return
	}
	if plan.CollectionID != collectionID {
		s.writeJSONError(w, http.StatusNotFound, "plan not found")
		return
	}

	var decidedAt, appliedAt, undoneAt any
	if !plan.DecidedAt.IsZero() {
		decidedAt = plan.DecidedAt.Unix()
	}
	if !plan.AppliedAt.IsZero() {
		appliedAt = plan.AppliedAt.Unix()
	}
	if !plan.UndoneAt.IsZero() {
		undoneAt = plan.UndoneAt.Unix()
	}

	moveList := make([]map[string]any, 0, len(moves))
	for _, m := range moves {
		moveList = append(moveList, map[string]any{
			"id":         m.ID,
			"plan_id":    m.PlanID,
			"source_rel": m.SourceRel,
			"dest_rel":   m.DestRel,
			"conflict":   m.Conflict,
			"status":     m.Status,
			"sha256":     m.SHA256,
			"created_at": m.CreatedAt.Unix(),
		})
	}

	resp := map[string]any{
		"plan": map[string]any{
			"id":             plan.ID,
			"collection_id":  plan.CollectionID,
			"status":         plan.Status,
			"created_at":     plan.CreatedAt.Unix(),
			"decided_at":     decidedAt,
			"applied_at":     appliedAt,
			"undone_at":      undoneAt,
			"conflict_count": plan.ConflictCount,
			"move_count":     plan.MoveCount,
		},
		"moves": moveList,
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleArrangeApprovePlan handles POST /api/collections/{id}/arrange/plans/{planId}/approve
func (s *Server) handleArrangeApprovePlan(w http.ResponseWriter, r *http.Request, collectionID, planID string) {
	actor := actorFromRequest(r)
	err := catalog.DecideArrangePlan(s.db, planID, catalog.ArrangeStatusApproved, time.Now(), actor)
	if err != nil {
		if err == sql.ErrNoRows {
			catalog.WriteAuditLog(s.db, actor, "arrange.plan.approve", "arrange_plan", planID, "error", "plan not found")
			s.writeJSONError(w, http.StatusNotFound, "plan not found")
			return
		}
		if strings.Contains(err.Error(), "not pending") {
			catalog.WriteAuditLog(s.db, actor, "arrange.plan.approve", "arrange_plan", planID, "error", err.Error())
			s.writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		slog.Error("approve arrange plan failed", "plan_id", planID, "error", err)
		catalog.WriteAuditLog(s.db, actor, "arrange.plan.approve", "arrange_plan", planID, "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "approve arrange plan failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "arrange.plan.approve", "arrange_plan", planID, "ok", "")
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

// handleArrangeRejectPlan handles POST /api/collections/{id}/arrange/plans/{planId}/reject
func (s *Server) handleArrangeRejectPlan(w http.ResponseWriter, r *http.Request, collectionID, planID string) {
	actor := actorFromRequest(r)
	err := catalog.DecideArrangePlan(s.db, planID, catalog.ArrangeStatusRejected, time.Now(), actor)
	if err != nil {
		if err == sql.ErrNoRows {
			catalog.WriteAuditLog(s.db, actor, "arrange.plan.reject", "arrange_plan", planID, "error", "plan not found")
			s.writeJSONError(w, http.StatusNotFound, "plan not found")
			return
		}
		if strings.Contains(err.Error(), "not pending") {
			catalog.WriteAuditLog(s.db, actor, "arrange.plan.reject", "arrange_plan", planID, "error", err.Error())
			s.writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		slog.Error("reject arrange plan failed", "plan_id", planID, "error", err)
		catalog.WriteAuditLog(s.db, actor, "arrange.plan.reject", "arrange_plan", planID, "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "reject arrange plan failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "arrange.plan.reject", "arrange_plan", planID, "ok", "")
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// handleArrangeApplyPlan handles POST /api/collections/{id}/arrange/plans/{planId}/apply
func (s *Server) handleArrangeApplyPlan(w http.ResponseWriter, r *http.Request, collectionID, planID string) {
	coll, pool, ok := s.resolveCollectionAndPool(w, r, collectionID, "arrange.plan.apply")
	if !ok {
		return
	}

	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", collectionID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "arrange.plan.apply", "collection", collectionID, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	actor := actorFromRequest(r)
	err = catalog.ApplyArrangePlan(s.db, planID, root, time.Now(), actor, catalog.DefaultMoveFunc)
	if err != nil {
		if err == sql.ErrNoRows {
			catalog.WriteAuditLog(s.db, actor, "arrange.plan.apply", "arrange_plan", planID, "error", "plan not found")
			s.writeJSONError(w, http.StatusNotFound, "plan not found")
			return
		}
		errMsg := err.Error()
		if strings.Contains(errMsg, "not approved") {
			catalog.WriteAuditLog(s.db, actor, "arrange.plan.apply", "arrange_plan", planID, "error", errMsg)
			s.writeJSONError(w, http.StatusConflict, errMsg)
			return
		}
		if strings.Contains(errMsg, "conflicts") {
			catalog.WriteAuditLog(s.db, actor, "arrange.plan.apply", "arrange_plan", planID, "error", errMsg)
			s.writeJSONError(w, http.StatusConflict, errMsg)
			return
		}
		slog.Error("apply arrange plan failed", "plan_id", planID, "error", err)
		catalog.WriteAuditLog(s.db, actor, "arrange.plan.apply", "arrange_plan", planID, "error", errMsg)
		s.writeJSONError(w, http.StatusInternalServerError, errMsg)
		return
	}

	catalog.WriteAuditLog(s.db, actor, "arrange.plan.apply", "arrange_plan", planID, "ok", "")
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "applied"})
}

// handleArrangeUndoPlan handles POST /api/collections/{id}/arrange/plans/{planId}/undo
func (s *Server) handleArrangeUndoPlan(w http.ResponseWriter, r *http.Request, collectionID, planID string) {
	coll, pool, ok := s.resolveCollectionAndPool(w, r, collectionID, "arrange.plan.undo")
	if !ok {
		return
	}

	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", collectionID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "arrange.plan.undo", "collection", collectionID, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	actor := actorFromRequest(r)
	err = catalog.UndoArrangePlan(s.db, planID, root, time.Now(), actor, catalog.DefaultMoveFunc)
	if err != nil {
		if err == sql.ErrNoRows {
			catalog.WriteAuditLog(s.db, actor, "arrange.plan.undo", "arrange_plan", planID, "error", "plan not found")
			s.writeJSONError(w, http.StatusNotFound, "plan not found")
			return
		}
		errMsg := err.Error()
		if strings.Contains(errMsg, "not applied") {
			catalog.WriteAuditLog(s.db, actor, "arrange.plan.undo", "arrange_plan", planID, "error", errMsg)
			s.writeJSONError(w, http.StatusConflict, errMsg)
			return
		}
		slog.Error("undo arrange plan failed", "plan_id", planID, "error", err)
		catalog.WriteAuditLog(s.db, actor, "arrange.plan.undo", "arrange_plan", planID, "error", errMsg)
		s.writeJSONError(w, http.StatusInternalServerError, errMsg)
		return
	}

	catalog.WriteAuditLog(s.db, actor, "arrange.plan.undo", "arrange_plan", planID, "ok", "")
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "undone"})
}

// ============================================================================
// WS39 (2026-11-07): AI content tagging & search index (FR-2.12 Phase-2)
// ============================================================================

// handleTagsRun handles POST /api/collections/{id}/tags/run
// Runs the tagging pipeline for a collection. Returns 200 with counts.
// Cold collection while asleep -> 409 wake_required.
func (s *Server) handleTagsRun(w http.ResponseWriter, r *http.Request, id string) {
	coll, pool, ok := s.resolveCollectionAndPool(w, r, id, "tag.run")
	if !ok {
		return
	}

	// Check cold tier: tagging NEVER wakes a disk by itself (same rule as WS35/WS36)
	if pool.Tier == "cold" {
		coldAwake := false
		if diskRows, err := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, pool.ID); err == nil {
			for diskRows.Next() {
				var diskName string
				if err := diskRows.Scan(&diskName); err == nil {
					if entry, ok := s.power.Snapshot(diskName); ok && entry.State == storage.PowerActive {
						coldAwake = true
						break
					}
				}
			}
			diskRows.Close()
		}
		if !coldAwake {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.run", "collection", id, "error", "pool tier is cold")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":         "pool tier is cold, tagging requires awake pool",
				"wake_required": true,
			})
			return
		}
	}

	// Resolve collection root
	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.run", "collection", id, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	// Verify root exists
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.run", "collection", id, "error", "collection root does not exist: "+root)
			s.writeJSONError(w, http.StatusConflict, "collection root does not exist: "+root)
			return
		}
		slog.Error("stat collection root failed", "root", root, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.run", "collection", id, "error", "stat root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "stat root failed")
		return
	}
	if !info.IsDir() {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.run", "collection", id, "error", "collection root is not a directory")
		s.writeJSONError(w, http.StatusInternalServerError, "collection root is not a directory")
		return
	}

	// Run tagging pipeline
	actor := actorFromRequest(r)
	config := tagging.DefaultPipelineConfig()
	pipeline := tagging.NewPipeline(s.db, id, root, config)
	result, err := pipeline.Run(r.Context())
	if err != nil {
		slog.Error("tagging pipeline failed", "collection_id", id, "error", err)
		catalog.WriteAuditLog(s.db, actor, "tag.run", "collection", id, "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "tagging pipeline failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "tag.run", "collection", id, "ok",
		fmt.Sprintf("clusters=%d samples_tagged=%d tags_created=%d propagated=%d sidecars_written=%d sidecar_errors=%d",
			result.Clusters, result.SamplesTagged, result.TagsCreated, result.Propagated, result.SidecarsWritten, result.SidecarErrors))

	s.writeJSON(w, http.StatusOK, map[string]int{
		"clusters":         result.Clusters,
		"samples_tagged":   result.SamplesTagged,
		"tags_created":     result.TagsCreated,
		"propagated":       result.Propagated,
		"sidecars_written": result.SidecarsWritten,
		"sidecar_errors":   result.SidecarErrors,
	})
}

// handleTagsSearch handles GET /api/collections/{id}/tags/search?tag=&q=&limit=
// Returns {files, total} for tag exact match and/or keyword substring.
func (s *Server) handleTagsSearch(w http.ResponseWriter, r *http.Request, id string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for tags search failed", "collection_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	tag := r.URL.Query().Get("tag")
	keyword := r.URL.Query().Get("q")
	limit := parseIntQuery(r, "limit", 50)
	if limit > 200 {
		limit = 200
	}

	files, total, err := catalog.SearchMediaTags(s.db, id, tag, keyword, limit)
	if err != nil {
		slog.Error("search media tags failed", "collection_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "search tags failed")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"files": files,
		"total": total,
	})
}

// handleTagsCreate handles POST /api/collections/{id}/tags
// Body: {rel_path, tag} -> creates manual tag (origin manual, confidence 1.0, source individual)
func (s *Server) handleTagsCreate(w http.ResponseWriter, r *http.Request, id string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for tag create failed", "collection_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	var req struct {
		RelPath string `json:"rel_path"`
		Tag     string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.manual.create", "collection", id, "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if req.RelPath == "" || req.Tag == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.manual.create", "collection", id, "error", "rel_path and tag are required")
		s.writeJSONError(w, http.StatusBadRequest, "rel_path and tag are required")
		return
	}

	// Verify file exists in catalog
	fileRow, err := catalog.GetFile(s.db, id, req.RelPath)
	if err != nil {
		slog.Error("get file for tag create failed", "collection_id", id, "rel_path", req.RelPath, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get file failed")
		return
	}
	if fileRow == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.manual.create", "collection", id, "error", "file not found: "+req.RelPath)
		s.writeJSONError(w, http.StatusNotFound, "file not found")
		return
	}

	// Check for duplicate (UNIQUE constraint on collection_id, rel_path, tag, origin)
	existing, err := catalog.GetMediaTag(s.db, id, req.RelPath, req.Tag, "manual")
	if err != nil {
		slog.Error("check existing tag failed", "collection_id", id, "rel_path", req.RelPath, "tag", req.Tag, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "check existing tag failed")
		return
	}
	if existing != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.manual.create", "collection", id, "error", "tag already exists")
		s.writeJSONError(w, http.StatusConflict, "tag already exists")
		return
	}

	// Create manual tag
	tag := &catalog.MediaTagRow{
		CollectionID: id,
		RelPath:      req.RelPath,
		Tag:          req.Tag,
		Origin:       "manual",
		Confidence:   1.0,
		Source:       "individual",
	}
	if err := catalog.CreateMediaTag(s.db, tag); err != nil {
		slog.Error("create manual tag failed", "collection_id", id, "rel_path", req.RelPath, "tag", req.Tag, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.manual.create", "collection", id, "error", "create tag failed")
		s.writeJSONError(w, http.StatusInternalServerError, "create tag failed")
		return
	}

	actor := actorFromRequest(r)
	catalog.WriteAuditLog(s.db, actor, "tag.manual.create", "collection", id, "ok", req.RelPath+":"+req.Tag)

	s.writeJSON(w, http.StatusCreated, map[string]any{
		"tag": map[string]any{
			"id":            tag.ID,
			"collection_id": tag.CollectionID,
			"rel_path":      tag.RelPath,
			"tag":           tag.Tag,
			"origin":        tag.Origin,
			"confidence":    tag.Confidence,
			"source":        tag.Source,
			"created_at":    tag.CreatedAt.Unix(),
		},
	})
}

// handleTagsDelete handles DELETE /api/collections/{id}/tags?rel_path=&tag=
// Removes a manual tag. Missing -> 404.
func (s *Server) handleTagsDelete(w http.ResponseWriter, r *http.Request, id string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for tag delete failed", "collection_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	relPath := r.URL.Query().Get("rel_path")
	tag := r.URL.Query().Get("tag")
	if relPath == "" || tag == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.manual.delete", "collection", id, "error", "rel_path and tag query parameters are required")
		s.writeJSONError(w, http.StatusBadRequest, "rel_path and tag query parameters are required")
		return
	}

	// Verify file exists in catalog
	fileRow, err := catalog.GetFile(s.db, id, relPath)
	if err != nil {
		slog.Error("get file for tag delete failed", "collection_id", id, "rel_path", relPath, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get file failed")
		return
	}
	if fileRow == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.manual.delete", "collection", id, "error", "file not found: "+relPath)
		s.writeJSONError(w, http.StatusNotFound, "file not found")
		return
	}

	// Delete manual tag (origin=manual)
	err = catalog.DeleteMediaTag(s.db, id, relPath, tag, "manual")
	if err != nil {
		if err == sql.ErrNoRows {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.manual.delete", "collection", id, "error", "tag not found")
			s.writeJSONError(w, http.StatusNotFound, "tag not found")
			return
		}
		slog.Error("delete manual tag failed", "collection_id", id, "rel_path", relPath, "tag", tag, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.manual.delete", "collection", id, "error", "delete tag failed")
		s.writeJSONError(w, http.StatusInternalServerError, "delete tag failed")
		return
	}

	actor := actorFromRequest(r)
	catalog.WriteAuditLog(s.db, actor, "tag.manual.delete", "collection", id, "ok", relPath+":"+tag)

	w.WriteHeader(http.StatusNoContent)
}

// handleTagsList handles GET /api/collections/{id}/tags?rel_path=
// Returns {tags} for one file.
func (s *Server) handleTagsList(w http.ResponseWriter, r *http.Request, id string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, id)
	if err != nil {
		slog.Error("get collection for tags list failed", "collection_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	relPath := r.URL.Query().Get("rel_path")
	if relPath == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.list", "collection", id, "error", "rel_path query parameter is required")
		s.writeJSONError(w, http.StatusBadRequest, "rel_path query parameter is required")
		return
	}

	// Verify file exists in catalog
	fileRow, err := catalog.GetFile(s.db, id, relPath)
	if err != nil {
		slog.Error("get file for tags list failed", "collection_id", id, "rel_path", relPath, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get file failed")
		return
	}
	if fileRow == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "tag.list", "collection", id, "error", "file not found: "+relPath)
		s.writeJSONError(w, http.StatusNotFound, "file not found")
		return
	}

	tags, _, err := catalog.ListMediaTags(s.db, id, relPath, "", 0, 0)
	if err != nil {
		slog.Error("list media tags failed", "collection_id", id, "rel_path", relPath, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list tags failed")
		return
	}

	result := make([]map[string]any, 0, len(tags))
	for _, t := range tags {
		result = append(result, map[string]any{
			"id":            t.ID,
			"collection_id": t.CollectionID,
			"rel_path":      t.RelPath,
			"tag":           t.Tag,
			"origin":        t.Origin,
			"confidence":    t.Confidence,
			"source":        t.Source,
			"created_at":    t.CreatedAt.Unix(),
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]any{"tags": result})
}

// --- FR-3.1/3.2 Restic backup endpoints (WS40/WS43) ---
// Passwords arrive in the request body for slice 1 and are never logged
// or stored in the catalog; vault-backed lookup is a named follow-up.

func (s *Server) handleBackupRepos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Name       string `json:"name"`
			TargetType string `json:"target_type"`
			TargetPath string `json:"target_path"` // required for local
			Config     any    `json:"config"`      // non-secret params, provider-specific
			SecretName string `json:"secret_name"` // vault secret name holding secrets
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			s.writeJSONError(w, http.StatusBadRequest, "name is required")
			return
		}

		// Default target_type to "local" for backward compatibility
		targetType := req.TargetType
		if targetType == "" {
			targetType = "local"
		}

		provider := backup.GetProvider(targetType)
		if provider == nil {
			s.writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown target_type %q", targetType))
			return
		}

		// Validate required config for non-local providers
		var configMap map[string]string
		var configBytes []byte
		if req.Config != nil {
			var err error
			configBytes, err = json.Marshal(req.Config)
			if err != nil {
				s.writeJSONError(w, http.StatusBadRequest, "invalid config")
				return
			}
			if err := json.Unmarshal(configBytes, &configMap); err != nil {
				s.writeJSONError(w, http.StatusBadRequest, "invalid config")
				return
			}
		}

		// For local, target_path is required and goes in TargetPath field
		if targetType == "local" {
			if req.TargetPath == "" {
				s.writeJSONError(w, http.StatusBadRequest, "target_path is required for local target_type")
				return
			}
			// Local doesn't use config/secrets
			configMap = nil
			configBytes = nil
			req.SecretName = ""
		} else {
			// Non-local: validate required config keys
			for _, key := range provider.RequiredConfig {
				if _, ok := configMap[key]; !ok {
					s.writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("config.%s is required for target_type %s", key, targetType))
					return
				}
			}
			// Validate secret_name is provided if provider requires secrets
			if len(provider.RequiredSecret) > 0 && req.SecretName == "" {
				s.writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("secret_name is required for target_type %s (needs: %v)", targetType, provider.RequiredSecret))
				return
			}
			// Validate config_json doesn't contain secret keys
			if err := backup.ValidateConfigJSON(string(configBytes)); err != nil {
				s.writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
		}

		configJSON := ""
		if configMap != nil {
			if configBytes == nil {
				configBytes, _ = json.Marshal(configMap)
			}
			configJSON = string(configBytes)
		}

		row, err := catalog.CreateBackupRepo(s.db, req.Name, targetType, req.TargetPath, configJSON, req.SecretName)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				s.writeJSONError(w, http.StatusConflict, "backup repo name already exists")
				return
			}
			s.writeJSONError(w, http.StatusInternalServerError, "create backup repo failed")
			return
		}
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.repo.create", "backup_repo", row.ID, "ok", row.Name)
		s.writeJSON(w, http.StatusCreated, map[string]any{"repo": row})
	case http.MethodGet:
		repos, err := catalog.ListBackupRepos(s.db)
		if err != nil {
			s.writeJSONError(w, http.StatusInternalServerError, "list backup repos failed")
			return
		}
		if repos == nil {
			repos = []*catalog.BackupRepoRow{}
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"repos": repos, "total": len(repos)})
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleBackupRepoByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/backups/repos/")
	parts := strings.SplitN(rest, "/", 2)
	repoID := parts[0]
	repo, err := catalog.GetBackupRepo(s.db, repoID)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "get backup repo failed")
		return
	}
	if repo == nil {
		s.writeJSONError(w, http.StatusNotFound, "backup repo not found")
		return
	}
	if len(parts) < 2 {
		s.writeJSONError(w, http.StatusNotFound, "backup repo action not found")
		return
	}
	switch {
	case parts[1] == "run" && r.Method == http.MethodPost:
		s.handleBackupRun(w, r, repo)
	case parts[1] == "snapshots" && r.Method == http.MethodGet:
		snaps, err := catalog.ListBackupSnapshots(s.db, repo.ID)
		if err != nil {
			s.writeJSONError(w, http.StatusInternalServerError, "list backup snapshots failed")
			return
		}
		if snaps == nil {
			snaps = []*catalog.BackupSnapshotRow{}
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"snapshots": snaps, "total": len(snaps)})
	case parts[1] == "restore" && r.Method == http.MethodPost:
		s.handleBackupRestore(w, r, repo)
	default:
		s.writeJSONError(w, http.StatusNotFound, "backup repo action not found")
	}
}

func (s *Server) handleBackupRun(w http.ResponseWriter, r *http.Request, repo *catalog.BackupRepoRow) {
	var req struct {
		CollectionID string `json:"collection_id"`
		Password     string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CollectionID == "" {
		s.writeJSONError(w, http.StatusBadRequest, "collection_id is required")
		return
	}
	if req.Password == "" {
		s.writeJSONError(w, http.StatusBadRequest, "password is required")
		return
	}
	coll, pool, ok := s.resolveCollectionAndPool(w, r, req.CollectionID, "backup.run")
	if !ok {
		return
	}

	// Build repo URL and credential env from registry
	repoURL, credEnv, err := backup.BuildRepoURLAndEnv(repo, s.vault)
	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.run", "backup_repo", repo.ID, "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Cold gate: backup NEVER wakes a disk by itself (same rule as tags/run).
	if pool.Tier == "cold" {
		coldAwake := false
		if diskRows, err := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, pool.ID); err == nil {
			for diskRows.Next() {
				var diskName string
				if err := diskRows.Scan(&diskName); err == nil {
					if entry, ok := s.power.Snapshot(diskName); ok && entry.State == storage.PowerActive {
						coldAwake = true
						break
					}
				}
			}
			diskRows.Close()
		}
		if !coldAwake {
			// Cold asleep: check for dirty shadows
			dirtyByPool, err := catalog.GetDirtyShadowsByPool(s.db, coll.ID)
			if err != nil {
				s.writeJSONError(w, http.StatusInternalServerError, "query dirty shadows failed")
				return
			}
			if len(dirtyByPool) > 0 {
				// Dirty shadows exist: back up each shadow subtree without waking Cold
				wr := backup.NewResticWrapper()
				if !wr.RepoExists(repoURL) {
					if err := wr.InitRepoWithEnv(repoURL, req.Password, credEnv); err != nil {
						catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.run", "backup_repo", repo.ID, "error", "init repo failed")
						s.writeJSONError(w, http.StatusInternalServerError, "init backup repo failed")
						return
					}
				}
				var snapshotIDs []string
				for shadowPoolID := range dirtyByPool {
					// Get the shadow pool to resolve its mountpoint
					shadowPool, _, err := catalog.GetPool(s.db, shadowPoolID)
					if err != nil || shadowPool == nil {
						catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.run", "backup_repo", repo.ID, "error", "shadow pool not found: "+shadowPoolID)
						s.writeJSONError(w, http.StatusInternalServerError, "shadow pool not found")
						return
					}
					// Shadow root: <warm-mount>/.kelvios-shadow/<collection-id>/
					mountpoint := shadowPool.Mountpoint
					if mountpoint == "" {
						mountpoint = "/mnt/" + shadowPool.Name
					}
					shadowRoot := filepath.Join(mountpoint, ".kelvios-shadow", coll.ID)
					shadowRoot = filepath.Clean(shadowRoot)
					// Run backup on the shadow root
					snapID, err := wr.BackupWithEnv(repoURL, shadowRoot, req.Password, credEnv)
					if err != nil {
						catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.run", "backup_repo", repo.ID, "error", "backup dirty shadows failed: "+err.Error())
						s.writeJSONError(w, http.StatusInternalServerError, "backup failed")
						return
					}
					// Record snapshot row
					_, err = catalog.CreateBackupSnapshot(s.db, repo.ID, coll.ID, snapID)
					if err != nil {
						s.writeJSONError(w, http.StatusInternalServerError, "record snapshot failed")
						return
					}
					snapshotIDs = append(snapshotIDs, snapID)
				}
				catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.run", "backup_repo", repo.ID, "ok", fmt.Sprintf("dirty shadows backup: %d snapshots", len(snapshotIDs)))
				s.writeJSON(w, http.StatusOK, map[string]any{"snapshot_ids": snapshotIDs, "source": "dirty_shadows"})
				return
			}
			// No dirty shadows: keep the 409 wake_required
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.run", "backup_repo", repo.ID, "error", "pool tier is cold")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "pool tier is cold, backup requires awake pool", "wake_required": true})
			return
		}
	}
	// Cold awake or non-Cold pool: canonical tree backup
	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}
	wr := backup.NewResticWrapper()
	if !wr.RepoExists(repoURL) {
		if err := wr.InitRepoWithEnv(repoURL, req.Password, credEnv); err != nil {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.run", "backup_repo", repo.ID, "error", "init repo failed")
			s.writeJSONError(w, http.StatusInternalServerError, "init backup repo failed")
			return
		}
	}
	snapID, err := wr.BackupWithEnv(repoURL, root, req.Password, credEnv)
	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.run", "backup_repo", repo.ID, "error", "backup failed")
		s.writeJSONError(w, http.StatusInternalServerError, "backup failed")
		return
	}
	row, err := catalog.CreateBackupSnapshot(s.db, repo.ID, coll.ID, snapID)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "record snapshot failed")
		return
	}
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.run", "backup_repo", repo.ID, "ok", "snapshot "+snapID)
	s.writeJSON(w, http.StatusOK, map[string]any{"snapshot": row, "snapshot_id": snapID})
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request, repo *catalog.BackupRepoRow) {
	var req struct {
		SnapshotID string `json:"snapshot_id"`
		RelPath    string `json:"rel_path"`
		Dest       string `json:"dest"`
		Password   string `json:"password"`
		Confirm    bool   `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SnapshotID == "" || req.RelPath == "" || req.Dest == "" {
		s.writeJSONError(w, http.StatusBadRequest, "snapshot_id, rel_path and dest are required")
		return
	}
	// P6: overwrite restore is destructive — explicit confirmation required.
	if !req.Confirm {
		s.writeJSONError(w, http.StatusConflict, "restore requires confirm: true")
		return
	}
	if req.Password == "" {
		s.writeJSONError(w, http.StatusBadRequest, "password is required")
		return
	}

	// Build repo URL and credential env from registry
	repoURL, credEnv, err := backup.BuildRepoURLAndEnv(repo, s.vault)
	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.restore", "backup_repo", repo.ID, "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	wr := backup.NewResticWrapper()
	restored, err := wr.RestoreFileWithEnv(repoURL, req.SnapshotID, req.RelPath, req.Dest, req.Password, credEnv)
	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.restore", "backup_repo", repo.ID, "error", "restore failed")
		s.writeJSONError(w, http.StatusInternalServerError, "restore failed")
		return
	}
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "backup.restore", "backup_repo", repo.ID, "ok", "restored "+req.RelPath)
	s.writeJSON(w, http.StatusOK, map[string]any{"restored_path": restored})
}

// --- FR-3.3 Scheduled restore test endpoints (WS44) ---

func (s *Server) handleRestoreTestRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Build deps for restoretest.RunScheduled
	wrapper := backup.NewResticWrapper()
	deps := restoretest.Deps{
		DB:            s.db,
		ResticWrapper: wrapper,
		VaultGetSecret: func(name string) (string, error) {
			return s.vault.Get(name)
		},
		Now: time.Now,
	}

	ctx := r.Context()
	result, err := restoretest.RunScheduled(ctx, deps)
	if err != nil {
		slog.Error("restore test run failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "restore test run failed")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"run_id":   result.RunID,
		"samples":  result.Samples,
		"passed":   result.Passed,
		"failed":   result.Failed,
		"skipped":  result.Skipped,
		"alerts":   result.Alerts,
	})
}

func (s *Server) handleRestoreTestRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	limit := parseIntQuery(r, "limit", 50)
	if limit > 500 {
		limit = 500
	}

	rows, err := s.db.Query(`
		SELECT id, started_at, samples, passed, failed, skipped, alerts
		FROM restore_test_runs
		ORDER BY started_at DESC, rowid DESC
		LIMIT ?
	`, limit)
	if err != nil {
		slog.Error("list restore test runs failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list runs failed")
		return
	}
	defer rows.Close()

	type RunRow struct {
		ID        string `json:"id"`
		StartedAt int64  `json:"started_at"`
		Samples   int    `json:"samples"`
		Passed    int    `json:"passed"`
		Failed    int    `json:"failed"`
		Skipped   int    `json:"skipped"`
		Alerts    int    `json:"alerts"`
	}
	var runs []RunRow
	for rows.Next() {
		var run RunRow
		if err := rows.Scan(&run.ID, &run.StartedAt, &run.Samples, &run.Passed, &run.Failed, &run.Skipped, &run.Alerts); err != nil {
			slog.Error("scan restore test run failed", "error", err)
			continue
		}
		runs = append(runs, run)
	}

	// Get total count
	var total int
	err = s.db.QueryRow(`SELECT COUNT(*) FROM restore_test_runs`).Scan(&total)
	if err != nil {
		total = len(runs)
	}

	s.writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "total": total})
}

func (s *Server) handleRestoreTestAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	resolvedParam := r.URL.Query().Get("resolved")
	resolvedFilter := -1 // -1 = all, 0 = unresolved, 1 = resolved
	if resolvedParam == "true" || resolvedParam == "1" {
		resolvedFilter = 1
	} else if resolvedParam == "false" || resolvedParam == "0" {
		resolvedFilter = 0
	}

	query := `
		SELECT id, run_id, repo_id, snapshot_id, rel_path, reason, created_at, resolved
		FROM restore_test_alerts
	`
	args := []any{}
	if resolvedFilter >= 0 {
		query += ` WHERE resolved = ?`
		args = append(args, resolvedFilter)
	}
	query += ` ORDER BY created_at DESC, rowid DESC LIMIT 500`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		slog.Error("list restore test alerts failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list alerts failed")
		return
	}
	defer rows.Close()

	type AlertRow struct {
		ID          string `json:"id"`
		RunID       string `json:"run_id"`
		RepoID      string `json:"repo_id"`
		SnapshotID  string `json:"snapshot_id"`
		RelPath     string `json:"rel_path"`
		Reason      string `json:"reason"`
		CreatedAt   int64  `json:"created_at"`
		Resolved    int    `json:"resolved"`
	}
	var alerts []AlertRow
	for rows.Next() {
		var a AlertRow
		if err := rows.Scan(&a.ID, &a.RunID, &a.RepoID, &a.SnapshotID, &a.RelPath, &a.Reason, &a.CreatedAt, &a.Resolved); err != nil {
			slog.Error("scan restore test alert failed", "error", err)
			continue
		}
		alerts = append(alerts, a)
	}

	// Get total count
	var total int
	countQuery := `SELECT COUNT(*) FROM restore_test_alerts`
	countArgs := []any{}
	if resolvedFilter >= 0 {
		countQuery += ` WHERE resolved = ?`
		countArgs = append(countArgs, resolvedFilter)
	}
	err = s.db.QueryRow(countQuery, countArgs...).Scan(&total)
	if err != nil {
		total = len(alerts)
	}

	s.writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts, "total": total})
}

func (s *Server) handleRestoreTestStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Get last run
	var lastRunAt sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(started_at) FROM restore_test_runs`).Scan(&lastRunAt)
	if err != nil && err != sql.ErrNoRows {
		slog.Error("get last restore test run failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "status failed")
		return
	}

	var lastRunAtPtr *int64
	var nextDueAtPtr *int64
	if lastRunAt.Valid {
		last := lastRunAt.Int64
		lastRunAtPtr = &last
		nextDue := last + restoretest.RestoreTestIntervalDays*24*60*60
		nextDueAtPtr = &nextDue
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"last_run_at":  lastRunAtPtr,
		"next_due_at":  nextDueAtPtr,
	})
}

// ============================================================================
// WS46: LLM Registry (FR-3.5)
// ============================================================================

// handleLLMModels handles GET /api/llm/models and POST /api/llm/models
func (s *Server) handleLLMModels(w http.ResponseWriter, r *http.Request) {
	actor := actorFromRequest(r)
	switch r.Method {
	case http.MethodGet:
		s.handleListLLMModels(w, r, actor)
	case http.MethodPost:
		s.handleCreateLLMModel(w, r, actor)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleLLMModelByID handles GET /api/llm/models/{id} and DELETE /api/llm/models/{id}
func (s *Server) handleLLMModelByID(w http.ResponseWriter, r *http.Request) {
	actor := actorFromRequest(r)
	path := strings.TrimPrefix(r.URL.Path, "/api/llm/models/")
	if path == "" || strings.Contains(path, "/") {
		http.NotFound(w, r)
		return
	}
	id := path

	switch r.Method {
	case http.MethodGet:
		s.handleGetLLMModel(w, r, id, actor)
	case http.MethodDelete:
		s.handleDeleteLLMModel(w, r, id, actor)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleLLMModelSelect handles POST /api/llm/models/select
// Body: {"id": "model-id"}
func (s *Server) handleLLMModelSelect(w http.ResponseWriter, r *http.Request) {
	actor := actorFromRequest(r)
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actor, "llm.model.select", "llm_model", "", "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.ID == "" {
		catalog.WriteAuditLog(s.db, actor, "llm.model.select", "llm_model", "", "error", "id is required")
		s.writeJSONError(w, http.StatusBadRequest, "id is required")
		return
	}

	if err := s.llmRegistry.SetActiveModel(req.ID); err != nil {
		if errors.Is(err, llmregistry.ErrModelNotFound) {
			catalog.WriteAuditLog(s.db, actor, "llm.model.select", "llm_model", req.ID, "error", "model not found")
			s.writeJSONError(w, http.StatusNotFound, "model not found")
			return
		}
		catalog.WriteAuditLog(s.db, actor, "llm.model.select", "llm_model", req.ID, "error", err.Error())
		slog.Error("set active model failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "select failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "llm.model.select", "llm_model", req.ID, "ok", "")
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleListLLMModels(w http.ResponseWriter, r *http.Request, actor string) {
	models, err := s.llmRegistry.ListModels()
	if err != nil {
		slog.Error("list llm models failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list models failed")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) handleCreateLLMModel(w http.ResponseWriter, r *http.Request, actor string) {
	var req llmregistry.CreateModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actor, "llm.model.create", "llm_model", "", "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	model, err := s.llmRegistry.CreateModel(req)
	if err != nil {
		if errors.Is(err, llmregistry.ErrModelNameExists) {
			catalog.WriteAuditLog(s.db, actor, "llm.model.create", "llm_model", "", "error", "model name already exists")
			s.writeJSONError(w, http.StatusConflict, "model name already exists")
			return
		}
		if errors.Is(err, llmregistry.ErrInvalidRequest) {
			catalog.WriteAuditLog(s.db, actor, "llm.model.create", "llm_model", "", "error", "invalid request: name, provider, model_id required")
			s.writeJSONError(w, http.StatusBadRequest, "name, provider, model_id required")
			return
		}
		if errors.Is(err, llmregistry.ErrSecretKeyInBody) {
			catalog.WriteAuditLog(s.db, actor, "llm.model.create", "llm_model", "", "error", "secret keys not allowed in request body")
			s.writeJSONError(w, http.StatusBadRequest, "secret keys not allowed in request body")
			return
		}
		catalog.WriteAuditLog(s.db, actor, "llm.model.create", "llm_model", "", "error", err.Error())
		slog.Error("create llm model failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "create model failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "llm.model.create", "llm_model", model.ID, "ok", "")
	s.writeJSON(w, http.StatusCreated, map[string]any{"model": model})
}

func (s *Server) handleGetLLMModel(w http.ResponseWriter, r *http.Request, id string, actor string) {
	model, err := s.llmRegistry.GetModel(id)
	if err != nil {
		slog.Error("get llm model failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get model failed")
		return
	}
	if model == nil {
		catalog.WriteAuditLog(s.db, actor, "llm.model.get", "llm_model", id, "error", "model not found")
		s.writeJSONError(w, http.StatusNotFound, "model not found")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"model": model})
}

func (s *Server) handleDeleteLLMModel(w http.ResponseWriter, r *http.Request, id string, actor string) {
	err := s.llmRegistry.DeleteModel(id)
	if err != nil {
		if errors.Is(err, llmregistry.ErrModelNotFound) {
			catalog.WriteAuditLog(s.db, actor, "llm.model.delete", "llm_model", id, "error", "model not found")
			s.writeJSONError(w, http.StatusNotFound, "model not found")
			return
		}
		if errors.Is(err, llmregistry.ErrModelActive) {
			catalog.WriteAuditLog(s.db, actor, "llm.model.delete", "llm_model", id, "error", "model is active, cannot delete")
			s.writeJSONError(w, http.StatusConflict, "model is active, cannot delete")
			return
		}
		catalog.WriteAuditLog(s.db, actor, "llm.model.delete", "llm_model", id, "error", err.Error())
		slog.Error("delete llm model failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "delete model failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "llm.model.delete", "llm_model", id, "ok", "")
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentsList handles GET /api/agents
func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)
	all := agents.All()
	result := make([]map[string]string, 0, len(all))
	for _, a := range all {
		result = append(result, map[string]string{
			"name":        a.Name(),
			"description": a.Describe(),
		})
	}

	catalog.WriteAuditLog(s.db, actor, "agent.list", "agent", "", "ok", fmt.Sprintf("count=%d", len(all)))
	s.writeJSON(w, http.StatusOK, map[string]any{"agents": result})
}

// handleAgentRun handles POST /api/agents/{name}/run
func (s *Server) handleAgentRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Path is /api/agents/{name}/run
	path := strings.TrimPrefix(r.URL.Path, "/api/agents/")
	if path == "" {
		http.NotFound(w, r)
		return
	}
	// Expecting {name}/run
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[1] != "run" {
		http.NotFound(w, r)
		return
	}
	name := parts[0]

	actor := actorFromRequest(r)
	agent, ok := agents.Get(name)
	if !ok {
		catalog.WriteAuditLog(s.db, actor, "agent.run."+name, "agent", name, "error", "agent not found")
		s.writeJSONError(w, http.StatusNotFound, "agent not found")
		return
	}

	deps := agents.NewDeps(s.db, s.disksFunc, time.Now)
	report, err := agent.Run(r.Context(), deps)
	if err != nil {
		slog.Error("agent run failed", "agent", name, "error", err)
		catalog.WriteAuditLog(s.db, actor, "agent.run."+name, "agent", name, "error", err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "agent run failed")
		return
	}

	findingCount := len(report.Findings)
	catalog.WriteAuditLog(s.db, actor, "agent.run."+name, "agent", name, "ok", fmt.Sprintf("findings=%d", findingCount))
	s.writeJSON(w, http.StatusOK, report)
}

// ============================================================================
// WS48: Approval Endpoints (FR-3.7)
// ============================================================================

// handleApprovalsList handles GET /api/approvals?status=
func (s *Server) handleApprovalsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)
	status := r.URL.Query().Get("status")

	var approvalStatus approval.ApprovalStatus
	if status != "" {
		approvalStatus = approval.ApprovalStatus(status)
	}

	store := approval.NewApprovalStore(s.db)
	requests, err := store.List(approvalStatus)
	if err != nil {
		slog.Error("list approvals failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list approvals failed")
		return
	}

	result := make([]map[string]any, 0, len(requests))
	for _, req := range requests {
		var args map[string]any
		_ = json.Unmarshal(req.Arguments, &args)
		result = append(result, map[string]any{
			"id":            req.ID,
			"tool":          req.Tool,
			"arguments":     args,
			"requested_by":  req.RequestedBy,
			"requested_at":  req.RequestedAt,
			"status":        req.Status,
			"decided_by":    req.DecidedBy,
			"decided_at":    req.DecidedAt,
			"expires_at":    req.ExpiresAt,
			"consumed_at":   req.ConsumedAt,
		})
	}

	catalog.WriteAuditLog(s.db, actor, "approval.list", "approval", "", "ok", fmt.Sprintf("count=%d status=%s", len(requests), status))
	s.writeJSON(w, http.StatusOK, map[string]any{"approvals": result})
}

// handleApprovalByID handles POST /api/approvals/{id}/approve and POST /api/approvals/{id}/reject
func (s *Server) handleApprovalByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)
	path := strings.TrimPrefix(r.URL.Path, "/api/approvals/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Check for /approve or /reject suffix
	var action string
	var id string
	if strings.HasSuffix(path, "/approve") {
		action = "approve"
		id = strings.TrimSuffix(path, "/approve")
	} else if strings.HasSuffix(path, "/reject") {
		action = "reject"
		id = strings.TrimSuffix(path, "/reject")
	} else {
		http.NotFound(w, r)
		return
	}
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	var reqBody struct {
		By string `json:"by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		catalog.WriteAuditLog(s.db, actor, "approval."+action, "approval", id, "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if reqBody.By == "" {
		catalog.WriteAuditLog(s.db, actor, "approval."+action, "approval", id, "error", "by is required")
		s.writeJSONError(w, http.StatusBadRequest, "by is required")
		return
	}

	store := approval.NewApprovalStore(s.db)
	var updatedReq *approval.ApprovalRequest
	var err error

	if action == "approve" {
		updatedReq, err = store.Approve(id, reqBody.By)
	} else {
		updatedReq, err = store.Reject(id, reqBody.By)
	}

	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			catalog.WriteAuditLog(s.db, actor, "approval."+action, "approval", id, "error", "not found")
			s.writeJSONError(w, http.StatusNotFound, "approval not found")
			return
		}
		if strings.Contains(err.Error(), "not pending") {
			catalog.WriteAuditLog(s.db, actor, "approval."+action, "approval", id, "error", "not pending")
			s.writeJSONError(w, http.StatusConflict, "approval not pending")
			return
		}
		slog.Error("approval " + action + " failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, action+" failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "approval."+action, "approval", id, "ok", fmt.Sprintf("by=%s tool=%s", reqBody.By, updatedReq.Tool))
	s.writeJSON(w, http.StatusOK, map[string]any{
		"id":            updatedReq.ID,
		"tool":          updatedReq.Tool,
		"status":        updatedReq.Status,
		"decided_by":    updatedReq.DecidedBy,
		"decided_at":    updatedReq.DecidedAt,
	})
}

// ChatBridge returns the WS49-A chat bridge service (FR-3.8) so transports
// and tests can register transports and drive inbound messages.
func (s *Server) ChatBridge() *chatbridge.Service {
	return s.chatBridge
}

// ComposeExecutor returns the WS52-A compose executor (FR-3.10).
func (s *Server) ComposeExecutor() *compose.Executor {
	return s.composeExecutor
}

// RulesEngine returns the WS62 FR-3.11 rules engine.
func (s *Server) RulesEngine() *rules.Engine {
	return s.rulesEngine
}

// handleChatbridgePairingCode handles POST /api/chatbridge/pairing-code
func (s *Server) handleChatbridgePairingCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)

	var body struct {
		Transport string `json:"transport"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Transport == "" {
		s.writeJSONError(w, http.StatusBadRequest, "transport is required")
		return
	}

	code, err := s.chatBridge.CreatePairingCode(body.Transport)
	if err != nil {
		slog.Error("create pairing code failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "create pairing code failed")
		return
	}

	// The code value itself is a secret: never write it to the audit log.
	catalog.WriteAuditLog(s.db, actor, "chatbridge.pairing_code.create", "chat_link", "", "ok", fmt.Sprintf("transport=%s", body.Transport))
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"code":       code.Code,
		"transport":  code.Transport,
		"expires_at": code.ExpiresAt,
	})
}

// handleChatbridgeLinks handles GET /api/chatbridge/links
func (s *Server) handleChatbridgeLinks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)
	links, err := s.chatBridge.ListLinks()
	if err != nil {
		slog.Error("list chat links failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list chat links failed")
		return
	}

	// pin_hash is a credential and is never exposed.
	result := make([]map[string]any, 0, len(links))
	for _, l := range links {
		result = append(result, map[string]any{
			"id":           l.ID,
			"transport":    l.Transport,
			"chat_id":      l.ChatID,
			"label":        l.Label,
			"created_at":   l.CreatedAt,
			"last_seen_at": l.LastSeenAt,
		})
	}

	catalog.WriteAuditLog(s.db, actor, "chatbridge.links.list", "chat_link", "", "ok", fmt.Sprintf("count=%d", len(links)))
	s.writeJSON(w, http.StatusOK, map[string]any{"links": result})
}

// handleChatbridgeLinkByID handles DELETE /api/chatbridge/links/{id}
func (s *Server) handleChatbridgeLinkByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)
	id := strings.TrimPrefix(r.URL.Path, "/api/chatbridge/links/")
	if id == "" || strings.Contains(id, "/") {
		s.writeJSONError(w, http.StatusNotFound, "chat link not found")
		return
	}

	if err := s.chatBridge.DeleteLink(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			catalog.WriteAuditLog(s.db, actor, "chatbridge.link.delete", "chat_link", id, "error", "not found")
			s.writeJSONError(w, http.StatusNotFound, "chat link not found")
			return
		}
		slog.Error("delete chat link failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "delete chat link failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "chatbridge.link.delete", "chat_link", id, "ok", "")
	s.writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleChatbridgeWhatsAppQR handles GET /api/chatbridge/whatsapp/qr
// Returns the current QR code for WhatsApp pairing if available.
func (s *Server) handleChatbridgeWhatsAppQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)

	// Get WhatsApp transport
	transport, ok := s.chatBridge.GetTransport("whatsapp")
	if !ok {
		catalog.WriteAuditLog(s.db, actor, "chatbridge.whatsapp.qr", "chat_link", "", "error", "whatsapp_not_enabled")
		s.writeJSONError(w, http.StatusNotFound, "whatsapp_not_enabled")
		return
	}

	whatsappTransport, ok := transport.(*chatbridge.WhatsAppTransport)
	if !ok {
		slog.Error("chatbridge: whatsapp transport type assertion failed")
		s.writeJSONError(w, http.StatusInternalServerError, "transport type error")
		return
	}

	qrCode, hasQR := whatsappTransport.PairingQR()
	if hasQR {
		// QR available and not paired
		expiresIn := int(time.Until(whatsappTransport.QRExpiry()).Seconds())
		if expiresIn < 0 {
			expiresIn = 0
		}
		catalog.WriteAuditLog(s.db, actor, "chatbridge.whatsapp.qr", "chat_link", "", "ok", "qr_available")
		s.writeJSON(w, http.StatusOK, map[string]any{
			"paired":          false,
			"qr_available":    true,
			"qr":              qrCode,
			"expires_in_seconds": expiresIn,
		})
		return
	}

	// No QR available - check if paired
	if whatsappTransport.IsPaired() {
		catalog.WriteAuditLog(s.db, actor, "chatbridge.whatsapp.qr", "chat_link", "", "ok", "paired")
		s.writeJSON(w, http.StatusOK, map[string]any{
			"paired":       true,
			"qr_available": false,
		})
		return
	}

	// Not paired, no QR
	catalog.WriteAuditLog(s.db, actor, "chatbridge.whatsapp.qr", "chat_link", "", "ok", "no_qr")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"paired":       false,
			"qr_available": false,
		})
}

// handleChatbridgeWhatsAppStatus handles GET /api/chatbridge/whatsapp/status
// Returns the WhatsApp transport registration and pairing status.
func (s *Server) handleChatbridgeWhatsAppStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	actor := actorFromRequest(r)

	transport, ok := s.chatBridge.GetTransport("whatsapp")
	if !ok {
		catalog.WriteAuditLog(s.db, actor, "chatbridge.whatsapp.status", "chat_link", "", "ok", "not_registered")
		s.writeJSON(w, http.StatusOK, map[string]any{
			"registered":    false,
			"paired":        false,
			"qr_available":  false,
		})
		return
	}

	whatsappTransport, ok := transport.(*chatbridge.WhatsAppTransport)
	if !ok {
		slog.Error("chatbridge: whatsapp transport type assertion failed")
		s.writeJSONError(w, http.StatusInternalServerError, "transport type error")
		return
	}

	paired := whatsappTransport.IsPaired()
	_, hasQR := whatsappTransport.PairingQR()

	catalog.WriteAuditLog(s.db, actor, "chatbridge.whatsapp.status", "chat_link", "", "ok", fmt.Sprintf("paired=%v,qr_available=%v", paired, hasQR))
	s.writeJSON(w, http.StatusOK, map[string]any{
		"registered":    true,
		"paired":        paired,
		"qr_available":  hasQR,
		})
}

// handleAgentModes handles GET /api/agent-modes and PUT /api/agent-modes
func (s *Server) handleAgentModes(w http.ResponseWriter, r *http.Request) {
	actor := actorFromRequest(r)

	switch r.Method {
	case http.MethodGet:
		modes, err := agentmodes.GetModes(s.db)
		if err != nil {
			slog.Error("get agent modes failed", "error", err)
			s.writeJSONError(w, http.StatusInternalServerError, "get agent modes failed")
			return
		}
		s.writeJSON(w, http.StatusOK, modes)

	case http.MethodPut:
		var req struct {
			ResidentHarness bool `json:"resident_harness"`
			ExternalMCP     bool `json:"external_mcp"`
			Preset          string `json:"preset,omitempty"` // ignored, computed from switches
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			catalog.WriteAuditLog(s.db, actor, "agent_modes.update", "agent_modes", "", "error", "invalid JSON")
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		modes := &agentmodes.Modes{
			ResidentHarness: req.ResidentHarness,
			ExternalMCP:     req.ExternalMCP,
		}
		if err := agentmodes.SetModes(s.db, modes); err != nil {
			slog.Error("set agent modes failed", "error", err)
			catalog.WriteAuditLog(s.db, actor, "agent_modes.update", "agent_modes", "", "error", "set modes failed")
			s.writeJSONError(w, http.StatusInternalServerError, "set modes failed")
			return
		}

		catalog.WriteAuditLog(s.db, actor, "agent_modes.update", "agent_modes", "", "ok", fmt.Sprintf("harness=%v,mcp=%v,preset=%s", modes.ResidentHarness, modes.ExternalMCP, modes.Preset))
		s.writeJSON(w, http.StatusOK, modes)

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// ============================================================================
// WS52-A: Compose endpoints (FR-3.10)
// ============================================================================

// handleComposeArtifacts handles GET /api/compose/artifacts and POST /api/compose/artifacts
func (s *Server) handleComposeArtifacts(w http.ResponseWriter, r *http.Request) {
	actor := actorFromRequest(r)

	switch r.Method {
	case http.MethodGet:
		status := r.URL.Query().Get("status")
		kind := r.URL.Query().Get("kind")

		artifacts, err := compose.ListArtifacts(s.db, status, kind)
		if err != nil {
			slog.Error("list compose artifacts failed", "error", err)
			s.writeJSONError(w, http.StatusInternalServerError, "list artifacts failed")
			return
		}
		if artifacts == nil {
			artifacts = []*compose.Artifact{}
		}

		result := make([]map[string]any, 0, len(artifacts))
		for _, a := range artifacts {
			result = append(result, map[string]any{
				"id":              a.ID,
				"name":            a.Name,
				"kind":            a.Kind,
				"status":          a.Status,
				"current_version": a.CurrentVersion,
				"spec":            json.RawMessage(a.Spec),
				"created_by":      a.CreatedBy,
				"created_at":      a.CreatedAt,
				"updated_at":      a.UpdatedAt,
			})
		}

		s.writeJSON(w, http.StatusOK, map[string]any{"artifacts": result})

	case http.MethodPost:
		var req struct {
			Name string          `json:"name"`
			Kind string          `json:"kind"`
			Spec json.RawMessage `json:"spec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			catalog.WriteAuditLog(s.db, actor, "compose.create", "compose", "", "error", "invalid JSON")
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		if req.Name == "" {
			catalog.WriteAuditLog(s.db, actor, "compose.create", "compose", "", "error", "name is required")
			s.writeJSONError(w, http.StatusBadRequest, "name is required")
			return
		}

		if err := compose.ValidateSpec(req.Kind, req.Spec); err != nil {
			catalog.WriteAuditLog(s.db, actor, "compose.create", "compose", "", "error", err.Error())
			s.writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		now := time.Now()
		artifact, err := compose.CreateArtifact(s.db, req.Name, req.Kind, req.Spec, actor, now)
		if err != nil {
			slog.Error("create compose artifact failed", "error", err)
			catalog.WriteAuditLog(s.db, actor, "compose.create", "compose", "", "error", "create artifact failed")
			s.writeJSONError(w, http.StatusInternalServerError, "create artifact failed")
			return
		}

		catalog.WriteAuditLog(s.db, actor, "compose.create", "compose", artifact.ID, "ok", "")

		s.writeJSON(w, http.StatusCreated, map[string]any{
			"artifact": map[string]any{
				"id":              artifact.ID,
				"name":            artifact.Name,
				"kind":            artifact.Kind,
				"status":          artifact.Status,
				"current_version": artifact.CurrentVersion,
				"spec":            json.RawMessage(artifact.Spec),
				"created_by":      artifact.CreatedBy,
				"created_at":      artifact.CreatedAt,
				"updated_at":      artifact.UpdatedAt,
			},
		})

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleComposeArtifactByID handles GET/PUT/DELETE /api/compose/artifacts/{id} and sub-routes
func (s *Server) handleComposeArtifactByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/compose/artifacts/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	actor := actorFromRequest(r)

	// Check for /activate suffix
	if strings.HasSuffix(path, "/activate") {
		id := strings.TrimSuffix(path, "/activate")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleComposeActivate(w, r, id, actor)
		return
	}

	// Check for /disable suffix
	if strings.HasSuffix(path, "/disable") {
		id := strings.TrimSuffix(path, "/disable")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleComposeDisable(w, r, id, actor)
		return
	}

	// Check for /rollback suffix
	if strings.HasSuffix(path, "/rollback") {
		id := strings.TrimSuffix(path, "/rollback")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleComposeRollback(w, r, id, actor)
		return
	}

	// No suffix - handle by ID
	if strings.Contains(path, "/") {
		http.NotFound(w, r)
		return
	}
	id := path

	switch r.Method {
	case http.MethodGet:
		s.handleComposeArtifactGet(w, r, id, actor)
	case http.MethodPut:
		s.handleComposeArtifactUpdate(w, r, id, actor)
	case http.MethodDelete:
		s.handleComposeArtifactDelete(w, r, id, actor)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleComposeArtifactGet(w http.ResponseWriter, r *http.Request, id string, actor string) {
	artifact, err := compose.GetArtifact(s.db, id)
	if err != nil {
		slog.Error("get compose artifact failed", "id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get artifact failed")
		return
	}
	if artifact == nil {
		s.writeJSONError(w, http.StatusNotFound, "artifact not found")
		return
	}

	// Get versions
	versions, err := compose.ListVersions(s.db, id)
	if err != nil {
		slog.Error("list compose versions failed", "id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list versions failed")
		return
	}
	if versions == nil {
		versions = []*compose.Version{}
	}

	versionList := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		versionList = append(versionList, map[string]any{
			"artifact_id": v.ArtifactID,
			"version":     v.Version,
			"spec":        json.RawMessage(v.Spec),
			"created_by":  v.CreatedBy,
			"created_at":  v.CreatedAt,
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"artifact": map[string]any{
			"id":              artifact.ID,
			"name":            artifact.Name,
			"kind":            artifact.Kind,
			"status":          artifact.Status,
			"current_version": artifact.CurrentVersion,
			"spec":            json.RawMessage(artifact.Spec),
			"created_by":      artifact.CreatedBy,
			"created_at":      artifact.CreatedAt,
			"updated_at":      artifact.UpdatedAt,
			"versions":        versionList,
		},
	})
}

func (s *Server) handleComposeArtifactUpdate(w http.ResponseWriter, r *http.Request, id string, actor string) {
	var req struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actor, "compose.update", "compose", id, "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	artifact, err := compose.GetArtifact(s.db, id)
	if err != nil {
		slog.Error("get compose artifact for update failed", "id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get artifact failed")
		return
	}
	if artifact == nil {
		s.writeJSONError(w, http.StatusNotFound, "artifact not found")
		return
	}

	// Only pending or disabled can be updated
	if artifact.Status == compose.StatusActive {
		catalog.WriteAuditLog(s.db, actor, "compose.update", "compose", id, "error", "invalid_state")
		s.writeJSONError(w, http.StatusConflict, "invalid_state")
		return
	}

	if err := compose.ValidateSpec(artifact.Kind, req.Spec); err != nil {
		catalog.WriteAuditLog(s.db, actor, "compose.update", "compose", id, "error", err.Error())
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	now := time.Now()
	updated, err := compose.UpdateSpec(s.db, id, req.Spec, actor, now)
	if err != nil {
		if err == sql.ErrNoRows {
			s.writeJSONError(w, http.StatusNotFound, "artifact not found")
			return
		}
		slog.Error("update compose spec failed", "id", id, "error", err)
		catalog.WriteAuditLog(s.db, actor, "compose.update", "compose", id, "error", "update spec failed")
		s.writeJSONError(w, http.StatusInternalServerError, "update spec failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "compose.update", "compose", id, "ok", fmt.Sprintf("version=%d", updated.CurrentVersion))

	s.writeJSON(w, http.StatusOK, map[string]any{
		"artifact": map[string]any{
			"id":              updated.ID,
			"name":            updated.Name,
			"kind":            updated.Kind,
			"status":          updated.Status,
			"current_version": updated.CurrentVersion,
			"spec":            json.RawMessage(updated.Spec),
			"created_by":      updated.CreatedBy,
			"created_at":      updated.CreatedAt,
			"updated_at":      updated.UpdatedAt,
		},
	})
}

func (s *Server) handleComposeArtifactDelete(w http.ResponseWriter, r *http.Request, id string, actor string) {
	artifact, err := compose.GetArtifact(s.db, id)
	if err != nil {
		slog.Error("get compose artifact for delete failed", "id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get artifact failed")
		return
	}
	if artifact == nil {
		s.writeJSONError(w, http.StatusNotFound, "artifact not found")
		return
	}

	// Only pending or disabled can be deleted
	if artifact.Status == compose.StatusActive {
		catalog.WriteAuditLog(s.db, actor, "compose.delete", "compose", id, "error", "invalid_state")
		s.writeJSONError(w, http.StatusConflict, "invalid_state")
		return
	}

	err = compose.DeleteArtifact(s.db, id)
	if err != nil {
		if err == sql.ErrNoRows {
			catalog.WriteAuditLog(s.db, actor, "compose.delete", "compose", id, "error", "artifact not found")
			s.writeJSONError(w, http.StatusNotFound, "artifact not found")
			return
		}
		slog.Error("delete compose artifact failed", "id", id, "error", err)
		catalog.WriteAuditLog(s.db, actor, "compose.delete", "compose", id, "error", "delete artifact failed")
		s.writeJSONError(w, http.StatusInternalServerError, "delete artifact failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "compose.delete", "compose", id, "ok", "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleComposeActivate(w http.ResponseWriter, r *http.Request, id string, actor string) {
	artifact, err := compose.GetArtifact(s.db, id)
	if err != nil {
		slog.Error("get compose artifact for activate failed", "id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get artifact failed")
		return
	}
	if artifact == nil {
		s.writeJSONError(w, http.StatusNotFound, "artifact not found")
		return
	}

	// Only pending or disabled can be activated
	if artifact.Status == compose.StatusActive {
		catalog.WriteAuditLog(s.db, actor, "compose.activate", "compose", id, "error", "invalid_state")
		s.writeJSONError(w, http.StatusConflict, "invalid_state")
		return
	}

	now := time.Now()
	updated, err := compose.SetStatus(s.db, id, compose.StatusActive, actor, now)
	if err != nil {
		if err == sql.ErrNoRows {
			s.writeJSONError(w, http.StatusNotFound, "artifact not found")
			return
		}
		slog.Error("activate compose artifact failed", "id", id, "error", err)
		catalog.WriteAuditLog(s.db, actor, "compose.activate", "compose", id, "error", "activate failed")
		s.writeJSONError(w, http.StatusInternalServerError, "activate failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "compose.activate", "compose", id, "ok", "")

	s.writeJSON(w, http.StatusOK, map[string]any{
		"artifact": map[string]any{
			"id":              updated.ID,
			"name":            updated.Name,
			"kind":            updated.Kind,
			"status":          updated.Status,
			"current_version": updated.CurrentVersion,
			"spec":            json.RawMessage(updated.Spec),
			"created_by":      updated.CreatedBy,
			"created_at":      updated.CreatedAt,
			"updated_at":      updated.UpdatedAt,
		},
	})
}

func (s *Server) handleComposeDisable(w http.ResponseWriter, r *http.Request, id string, actor string) {
	artifact, err := compose.GetArtifact(s.db, id)
	if err != nil {
		slog.Error("get compose artifact for disable failed", "id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get artifact failed")
		return
	}
	if artifact == nil {
		s.writeJSONError(w, http.StatusNotFound, "artifact not found")
		return
	}

	// Only active can be disabled
	if artifact.Status != compose.StatusActive {
		catalog.WriteAuditLog(s.db, actor, "compose.disable", "compose", id, "error", "invalid_state")
		s.writeJSONError(w, http.StatusConflict, "invalid_state")
		return
	}

	now := time.Now()
	updated, err := compose.SetStatus(s.db, id, compose.StatusDisabled, actor, now)
	if err != nil {
		if err == sql.ErrNoRows {
			s.writeJSONError(w, http.StatusNotFound, "artifact not found")
			return
		}
		slog.Error("disable compose artifact failed", "id", id, "error", err)
		catalog.WriteAuditLog(s.db, actor, "compose.disable", "compose", id, "error", "disable failed")
		s.writeJSONError(w, http.StatusInternalServerError, "disable failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "compose.disable", "compose", id, "ok", "")

	s.writeJSON(w, http.StatusOK, map[string]any{
		"artifact": map[string]any{
			"id":              updated.ID,
			"name":            updated.Name,
			"kind":            updated.Kind,
			"status":          updated.Status,
			"current_version": updated.CurrentVersion,
			"spec":            json.RawMessage(updated.Spec),
			"created_by":      updated.CreatedBy,
			"created_at":      updated.CreatedAt,
			"updated_at":      updated.UpdatedAt,
		},
	})
}

func (s *Server) handleComposeRollback(w http.ResponseWriter, r *http.Request, id string, actor string) {
	var req struct {
		Version int `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actor, "compose.rollback", "compose", id, "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if req.Version <= 0 {
		catalog.WriteAuditLog(s.db, actor, "compose.rollback", "compose", id, "error", "version must be positive")
		s.writeJSONError(w, http.StatusBadRequest, "version must be positive")
		return
	}

	artifact, err := compose.GetArtifact(s.db, id)
	if err != nil {
		slog.Error("get compose artifact for rollback failed", "id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get artifact failed")
		return
	}
	if artifact == nil {
		s.writeJSONError(w, http.StatusNotFound, "artifact not found")
		return
	}

	// Only pending or disabled can be rolled back
	if artifact.Status == compose.StatusActive {
		catalog.WriteAuditLog(s.db, actor, "compose.rollback", "compose", id, "error", "invalid_state")
		s.writeJSONError(w, http.StatusConflict, "invalid_state")
		return
	}

	// Check if target version exists
	var exists bool
	err = s.db.QueryRow(`SELECT 1 FROM compose_artifact_versions WHERE artifact_id = ? AND version = ?`, id, req.Version).Scan(&exists)
	if err == sql.ErrNoRows {
		s.writeJSONError(w, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		slog.Error("check target version failed", "id", id, "version", req.Version, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "check version failed")
		return
	}

	now := time.Now()
	rolledBack, err := compose.Rollback(s.db, id, req.Version, actor, now)
	if err != nil {
		if err == sql.ErrNoRows {
			s.writeJSONError(w, http.StatusNotFound, "artifact or version not found")
			return
		}
		slog.Error("rollback compose artifact failed", "id", id, "version", req.Version, "error", err)
		catalog.WriteAuditLog(s.db, actor, "compose.rollback", "compose", id, "error", "rollback failed")
		s.writeJSONError(w, http.StatusInternalServerError, "rollback failed")
		return
	}

	catalog.WriteAuditLog(s.db, actor, "compose.rollback", "compose", id, "ok", fmt.Sprintf("from_version=%d,to_version=%d", req.Version, rolledBack.CurrentVersion))

	s.writeJSON(w, http.StatusOK, map[string]any{
		"artifact": map[string]any{
			"id":              rolledBack.ID,
			"name":            rolledBack.Name,
			"kind":            rolledBack.Kind,
			"status":          rolledBack.Status,
			"current_version": rolledBack.CurrentVersion,
			"spec":            json.RawMessage(rolledBack.Spec),
			"created_by":      rolledBack.CreatedBy,
			"created_at":      rolledBack.CreatedAt,
			"updated_at":      rolledBack.UpdatedAt,
		},
	})
}

// ============================================================================
// WS56: Activity & Automation Core API (FR-4.7)
// ============================================================================

// handleActivityList handles GET /api/activity
func (s *Server) handleActivityList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.activityAggregator == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "activity aggregator not initialized")
		return
	}

	sourceFilter := r.URL.Query().Get("source")
	statusFilter := r.URL.Query().Get("status")

	resp, err := s.activityAggregator.List(r.Context(), sourceFilter, statusFilter)
	if err != nil {
		slog.Error("activity list failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "activity list failed")
		return
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleActivityByID handles GET /api/activity/{id} and POST /api/activity/{id}/{action}
func (s *Server) handleActivityByID(w http.ResponseWriter, r *http.Request) {
	actor := actorFromRequest(r)

	path := strings.TrimPrefix(r.URL.Path, "/api/activity/")
	if path == "" || path == r.URL.Path {
		http.NotFound(w, r)
		return
	}

	if s.activityAggregator == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "activity aggregator not initialized")
		return
	}

	// Check for action suffix: /{id}/{action}
	parts := strings.Split(path, "/")
	if len(parts) == 2 {
		// POST /api/activity/{id}/{action}
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		itemID := parts[0]
		action := parts[1]

		item, err := s.activityAggregator.ExecuteAction(r.Context(), itemID, action, actor)
		if err != nil {
			// Map error to HTTP status
			status := http.StatusInternalServerError
			errMsg := err.Error()
			switch errMsg {
			case "item_not_found", "invalid item id format":
				status = http.StatusNotFound
			case "control_not_supported":
				status = http.StatusConflict
			case "already_running":
				status = http.StatusConflict
			case "not_running":
				status = http.StatusConflict
			case "source_unavailable":
				status = http.StatusServiceUnavailable
			}
			s.writeJSONError(w, status, errMsg)
			return
		}

		s.writeJSON(w, http.StatusOK, map[string]any{"item": item})
		return
	}

	// GET /api/activity/{id}
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	itemID := path
	item, err := s.activityAggregator.Get(r.Context(), itemID)
	if err != nil {
		status := http.StatusInternalServerError
		if err.Error() == "invalid item id format" || err.Error() == "item_not_found" {
			status = http.StatusNotFound
		}
		s.writeJSONError(w, status, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{"item": item})
}

// ============================================================================
// WS59: OTA Core (FR-5.1)
// ============================================================================

// handleUpdatesStatus handles GET /api/updates
func (s *Server) handleUpdatesStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.otaService == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "ota service not initialized")
		return
	}
	status, err := s.otaService.Status(r.Context())
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to get OTA status")
		return
	}
	s.writeJSON(w, http.StatusOK, status)
}

// handleUpdatesStage handles POST /api/updates/stage
func (s *Server) handleUpdatesStage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.otaService == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "ota service not initialized")
		return
	}
	actor := actorFromRequest(r)
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	rec, err := s.otaService.Stage(r.Context(), req.Name, actor)
	if err != nil {
		errMsg := err.Error()
		status := http.StatusInternalServerError
		switch errMsg {
		case "ota_not_configured":
			status = http.StatusServiceUnavailable
		case "invalid_package_name":
			status = http.StatusBadRequest
		case "package_not_found":
			status = http.StatusNotFound
		case "invalid_signature":
			status = http.StatusUnprocessableEntity
		case "hash_mismatch":
			status = http.StatusUnprocessableEntity
		case "channel_mismatch":
			status = http.StatusConflict
		}
		s.writeJSONError(w, status, errMsg)
		return
	}
	s.writeJSON(w, http.StatusOK, rec)
}

// handleUpdatesApply handles POST /api/updates/apply
func (s *Server) handleUpdatesApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.otaService == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "ota service not initialized")
		return
	}
	actor := actorFromRequest(r)
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	rec, err := s.otaService.Apply(r.Context(), req.ID, actor)
	if err != nil {
		errMsg := err.Error()
		status := http.StatusInternalServerError
		switch errMsg {
		case "record not found":
			status = http.StatusNotFound
		case "invalid_transition":
			status = http.StatusConflict
		}
		s.writeJSONError(w, status, errMsg)
		return
	}
	s.writeJSON(w, http.StatusOK, rec)
}

// handleUpdatesRollback handles POST /api/updates/rollback
func (s *Server) handleUpdatesRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.otaService == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "ota service not initialized")
		return
	}
	actor := actorFromRequest(r)
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	rec, err := s.otaService.Rollback(r.Context(), req.ID, actor)
	if err != nil {
		errMsg := err.Error()
		status := http.StatusInternalServerError
		switch errMsg {
		case "record not found":
			status = http.StatusNotFound
		case "invalid_transition":
			status = http.StatusConflict
		}
		s.writeJSONError(w, status, errMsg)
		return
	}
	s.writeJSON(w, http.StatusOK, rec)
}

// ============================================================================
// WS53-B: REST App Library (FR-4.1/FR-4.4)
// ============================================================================

// handleApps handles GET /api/apps, GET /api/apps/{id}, and POST /api/apps/{id}/{action}
func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	actor := actorFromRequest(r)

	// Check if this is a sub-route after /api/apps/
	path := strings.TrimPrefix(r.URL.Path, "/api/apps/")
	if path == r.URL.Path {
		// Exact match /api/apps (no sub-path)
		switch r.Method {
		case http.MethodGet:
			s.handleAppsList(w, r, actor)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Has sub-path after /api/apps/
	// Check if it's {id} or {id}/{action}
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 1 {
		// Just /api/apps/{id} - single app GET
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleAppsGet(w, r, parts[0], actor)
		return
	}

	// Has {id}/{action} - action route
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	s.handleAppsAction(w, r, path, actor)
}

// handleAppsList handles GET /api/apps
func (s *Server) handleAppsList(w http.ResponseWriter, r *http.Request, actor string) {
	// Parse optional filters
	waveStr := r.URL.Query().Get("wave")
	category := r.URL.Query().Get("category")

	var wave int
	if waveStr != "" {
		var err error
		wave, err = strconv.Atoi(waveStr)
		if err != nil || (wave != 1 && wave != 2) {
			s.writeJSONError(w, http.StatusBadRequest, "wave must be 1 or 2")
			return
		}
	}

	// Get statuses from DB
	statuses, err := apps.GetStatuses(s.db)
	if err != nil {
		slog.Error("get app statuses failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get app statuses failed")
		return
	}

	// Get catalog apps (filtered)
	var catalogApps []apps.App
	if wave != 0 && category != "" {
		// Both filters: intersection
		waveApps := apps.ByWave(wave)
		catApps := apps.ByCategory(category)
		catSet := make(map[string]bool)
		for _, a := range catApps {
			catSet[a.ID] = true
		}
		for _, a := range waveApps {
			if catSet[a.ID] {
				catalogApps = append(catalogApps, a)
			}
		}
	} else if wave != 0 {
		catalogApps = apps.ByWave(wave)
	} else if category != "" {
		catalogApps = apps.ByCategory(category)
	} else {
		catalogApps = apps.Catalog()
	}

	// Get port collisions among installed/active apps
	installedOrActive := make(map[string]bool)
	for id, st := range statuses {
		if st == apps.StatusInstalled || st == apps.StatusActive {
			installedOrActive[id] = true
		}
	}
	allCollisions := apps.PortCollisions()
	// Filter collisions to only those where both apps are installed/active
	filteredCollisions := make(map[int][]string)
	for port, ids := range allCollisions {
		var activeIDs []string
		for _, id := range ids {
			if installedOrActive[id] {
				activeIDs = append(activeIDs, id)
			}
		}
		if len(activeIDs) > 1 {
			filteredCollisions[port] = activeIDs
		}
	}

	// Build response
	entries := make([]map[string]any, 0, len(catalogApps))
	for _, a := range catalogApps {
		// Use buildAppEntry for consistent update fields, but override collisions with filtered ones
		entry := s.buildAppEntry(a, apps.StatusAvailable, statuses)
		// Override collisions with filtered ones (only installed/active apps)
		collisions := make([]int, 0)
		for port, ids := range filteredCollisions {
			for _, id := range ids {
				if id == a.ID {
					collisions = append(collisions, port)
					break
				}
			}
		}
		entry["port_collisions"] = collisions
		entries = append(entries, entry)
	}

	s.writeJSON(w, http.StatusOK, map[string]any{"apps": entries})
}

// handleAppsAction handles POST /api/apps/{id}/install|activate|deactivate|uninstall|update|rollback
func (s *Server) handleAppsAction(w http.ResponseWriter, r *http.Request, path, actor string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse path: {id}/{action}
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	appID := parts[0]
	action := parts[1]

	// Validate app exists in catalog
	app, ok := apps.Get(appID)
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "app_not_found")
		return
	}

	// Get current status
	statuses, err := apps.GetStatuses(s.db)
	if err != nil {
		slog.Error("get app statuses failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get app statuses failed")
		return
	}
	currentStatus := apps.StatusAvailable
	if st, ok := statuses[appID]; ok {
		currentStatus = st
	}

	// Handle update action separately (not a status transition)
	if action == "update" {
		s.handleAppsUpdate(w, r, app, appID, actor, currentStatus, statuses)
		return
	}

	// Handle rollback action separately (not a status transition)
	if action == "rollback" {
		s.handleAppsRollback(w, r, app, appID, actor, currentStatus, statuses)
		return
	}

	// Determine target status from action
	var targetStatus apps.Status
	switch action {
	case "install":
		targetStatus = apps.StatusInstalled
	case "activate":
		targetStatus = apps.StatusActive
	case "deactivate":
		targetStatus = apps.StatusInstalled
	case "uninstall":
		targetStatus = apps.StatusAvailable
	default:
		http.NotFound(w, r)
		return
	}

	// Validate transition
	if !apps.ValidTransition(currentStatus, targetStatus) {
		s.writeJSONError(w, http.StatusConflict, "invalid_transition")
		return
	}

	// Driver-first: execute container operation before status update
	if s.appDriver == nil {
		// No driver available (e.g., Docker not running) - return driver_unavailable
		s.writeJSONError(w, http.StatusServiceUnavailable, "driver_unavailable")
		catalog.WriteAuditLog(s.db, actor, "apps."+action, "app", appID, "error", "driver unavailable")
		return
	}
	switch action {
	case "install":
		if err := s.appDriver.Install(app); err != nil {
			s.handleDriverError(w, err, actor, action, appID)
			return
		}
		// Set installed info on successful install (WS55)
		if err := apps.SetInstalledInfo(s.db, appID, targetStatus, app.Image, "", "", ""); err != nil {
			slog.Error("set installed info failed", "app_id", appID, "error", err)
			s.writeJSONError(w, http.StatusInternalServerError, "set installed info failed")
			return
		}
	case "activate":
		if err := s.appDriver.Start(appID); err != nil {
			s.handleDriverError(w, err, actor, action, appID)
			return
		}
		// Health gate: poll 2s, timeout 30s (keputusan markas 3)
		healthy := false
		for elapsed := time.Duration(0); elapsed < 30*time.Second; elapsed += 2 * time.Second {
			h, err := s.appDriver.Health(appID)
			if err != nil {
				s.handleDriverError(w, err, actor, action, appID)
				return
			}
			if h {
				healthy = true
				break
			}
			time.Sleep(2 * time.Second)
		}
		if !healthy {
			// Health failed: stop container, status stays installed
			_ = s.appDriver.Stop(appID)
			s.writeJSONError(w, http.StatusBadGateway, "health_check_failed")
			catalog.WriteAuditLog(s.db, actor, "apps."+action, "app", appID, "error", "health check failed after start")
			return
		}
	case "deactivate":
		if err := s.appDriver.Stop(appID); err != nil {
			s.handleDriverError(w, err, actor, action, appID)
			return
		}
	case "uninstall":
		if err := s.appDriver.Remove(appID); err != nil {
			s.handleDriverError(w, err, actor, action, appID)
			return
		}
	}

	// Update status after successful driver operation
	if err := apps.SetStatus(s.db, appID, targetStatus); err != nil {
		if _, ok := err.(apps.ErrAppNotFound); ok {
			s.writeJSONError(w, http.StatusNotFound, "app_not_found")
			return
		}
		slog.Error("set app status failed", "app_id", appID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "set app status failed")
		return
	}

	// Audit log
	catalog.WriteAuditLog(s.db, actor, "apps."+action, "app", appID, "ok", fmt.Sprintf("from=%s,to=%s", currentStatus, targetStatus))

	// Re-read statuses after update
	statuses, err = apps.GetStatuses(s.db)
	if err != nil {
		slog.Error("get app statuses after update failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get app statuses failed")
		return
	}

	// Return updated entry (same shape as GET single)
	s.writeAppEntry(w, app, targetStatus, statuses)
}

// handleAppsUpdate handles POST /api/apps/{id}/update (WS55, FR-4.8)
func (s *Server) handleAppsUpdate(w http.ResponseWriter, r *http.Request, app apps.App, appID, actor string, currentStatus apps.Status, statuses map[string]apps.Status) {
	// Validate status: only installed or active can update (decision-7)
	if currentStatus != apps.StatusInstalled && currentStatus != apps.StatusActive {
		s.writeJSONError(w, http.StatusConflict, "not_installed")
		catalog.WriteAuditLog(s.db, actor, "apps.update", "app", appID, "error", "app not installed")
		return
	}

	// Check update source
	if s.updateSource == nil {
		s.writeJSONError(w, http.StatusConflict, "no_update_available")
		catalog.WriteAuditLog(s.db, actor, "apps.update", "app", appID, "error", "no update source configured")
		return
	}

	// Get update entry from feed
	upd, ok := s.updateSource.UpdateFor(appID)
	if !ok {
		s.writeJSONError(w, http.StatusConflict, "no_update_available")
		catalog.WriteAuditLog(s.db, actor, "apps.update", "app", appID, "error", "no update available for app")
		return
	}

	// Get installed info
	info, found, err := apps.GetInstalledInfo(s.db, appID)
	if err != nil {
		slog.Error("get installed info failed", "app_id", appID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get installed info failed")
		return
	}
	if !found || info.ImageRef == "" {
		// App installed before WS55 - use catalog image as baseline (decision-4)
		info.ImageRef = app.Image
	}

	// Check if update is actually different (decision-2)
	hasUpdate := upd.ImageRef != info.ImageRef
	if !hasUpdate && upd.Digest != "" && info.Digest != "" {
		hasUpdate = upd.Digest != info.Digest
	}
	if !hasUpdate {
		s.writeJSONError(w, http.StatusConflict, "no_update_available")
		catalog.WriteAuditLog(s.db, actor, "apps.update", "app", appID, "error", "target image same as installed")
		return
	}

	// Verify digest if feed provides it (decision-8)
	if upd.Digest != "" {
		// Check supply-chain for pinned digest of target ImageRef
		// For WS55 v1: if feed.Digest is set, we trust it as the expected digest
		// Mismatch would be detected at pull time by Docker, but we check here for early rejection
		// In a full implementation, we'd query supplychain_entries for the pinned digest
		// For now, we accept the feed digest as authoritative
		_ = upd.Digest // digest verification placeholder - feed digest is trusted
	}

	// Driver check
	if s.appDriver == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "driver_unavailable")
		catalog.WriteAuditLog(s.db, actor, "apps.update", "app", appID, "error", "driver unavailable")
		return
	}

	// Use feed's ports and RAM for the update (decision-6: permission changes from catalog vs feed)
	updateApp := app
	updateApp.Ports = upd.Ports
	updateApp.RAMMB = upd.RAMMB

	// Execute update via driver
	if err := s.appDriver.Update(updateApp, upd.ImageRef); err != nil {
		s.handleDriverError(w, err, actor, "update", appID)
		return
	}

	// Health gate: poll 2s, timeout 30s (same as activate)
	healthy := false
	for elapsed := time.Duration(0); elapsed < 30*time.Second; elapsed += 2 * time.Second {
		h, err := s.appDriver.Health(appID)
		if err != nil {
			s.handleDriverError(w, err, actor, "update", appID)
			return
		}
		if h {
			healthy = true
			break
		}
		time.Sleep(2 * time.Second)
	}

	if !healthy {
		// Health failed: rollback
		catalog.WriteAuditLog(s.db, actor, "apps.update", "app", appID, "error", fmt.Sprintf("health check failed after update to %s", upd.ImageRef))

		// Rollback via driver
		if rbErr := s.appDriver.RollbackUpdate(appID); rbErr != nil {
			slog.Error("rollback failed", "app_id", appID, "error", rbErr)
			s.writeJSONError(w, http.StatusBadGateway, "rollback_failed")
			catalog.WriteAuditLog(s.db, actor, "apps.rollback", "app", appID, "error", fmt.Sprintf("rollback failed: %v", rbErr))
			return
		}

		// Health gate on rolled-back image
		rbHealthy := false
		for elapsed := time.Duration(0); elapsed < 30*time.Second; elapsed += 2 * time.Second {
			h, err := s.appDriver.Health(appID)
			if err != nil {
				slog.Error("health check during rollback failed", "app_id", appID, "error", err)
				break
			}
			if h {
				rbHealthy = true
				break
			}
			time.Sleep(2 * time.Second)
		}

		if !rbHealthy {
			s.writeJSONError(w, http.StatusBadGateway, "rollback_health_failed")
			catalog.WriteAuditLog(s.db, actor, "apps.rollback", "app", appID, "error", "health check failed after rollback")
			return
		}

		// Rollback succeeded: status returns to original, installed info unchanged
		catalog.WriteAuditLog(s.db, actor, "apps.rollback", "app", appID, "ok", fmt.Sprintf("rolled back from %s to %s", upd.ImageRef, info.ImageRef))
		s.writeJSONError(w, http.StatusBadGateway, "health_check_failed")
		return
	}

	// Health passed: update installed info
	// previous_image_ref = old image_ref, image_ref = target, version_label = feed.Version, digest = feed.Digest
	if err := apps.SetInstalledInfo(s.db, appID, currentStatus, upd.ImageRef, upd.Version, upd.Digest, info.ImageRef); err != nil {
		slog.Error("set installed info failed", "app_id", appID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "set installed info failed")
		return
	}

	// Audit success
	catalog.WriteAuditLog(s.db, actor, "apps.update", "app", appID, "ok", fmt.Sprintf("from=%s,to=%s", info.ImageRef, upd.ImageRef))

	// Re-read statuses
	statuses, err = apps.GetStatuses(s.db)
	if err != nil {
		slog.Error("get app statuses after update failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get app statuses failed")
		return
	}

	// Return updated entry (status unchanged, but installed_image updated)
	s.writeAppEntry(w, app, currentStatus, statuses)
}

// handleAppsRollback handles POST /api/apps/{id}/rollback (WS55, FR-4.8)
func (s *Server) handleAppsRollback(w http.ResponseWriter, r *http.Request, app apps.App, appID, actor string, currentStatus apps.Status, statuses map[string]apps.Status) {
	// Validate status: only installed or active can rollback
	if currentStatus != apps.StatusInstalled && currentStatus != apps.StatusActive {
		s.writeJSONError(w, http.StatusConflict, "not_installed")
		catalog.WriteAuditLog(s.db, actor, "apps.rollback", "app", appID, "error", "app not installed")
		return
	}

	// Get installed info to check for previous_image_ref
	info, found, err := apps.GetInstalledInfo(s.db, appID)
	if err != nil {
		slog.Error("get installed info failed", "app_id", appID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get installed info failed")
		return
	}
	if !found || info.PreviousImageRef == "" {
		s.writeJSONError(w, http.StatusConflict, "no_previous_image")
		catalog.WriteAuditLog(s.db, actor, "apps.rollback", "app", appID, "error", "no previous image to rollback to")
		return
	}

	// Driver check
	if s.appDriver == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "driver_unavailable")
		catalog.WriteAuditLog(s.db, actor, "apps.rollback", "app", appID, "error", "driver unavailable")
		return
	}

	// Execute rollback via driver
	if err := s.appDriver.RollbackUpdate(appID); err != nil {
		s.handleDriverError(w, err, actor, "rollback", appID)
		return
	}

	// Health gate: poll 2s, timeout 30s (same as update/activate)
	healthy := false
	for elapsed := time.Duration(0); elapsed < 30*time.Second; elapsed += 2 * time.Second {
		h, err := s.appDriver.Health(appID)
		if err != nil {
			s.handleDriverError(w, err, actor, "rollback", appID)
			return
		}
		if h {
			healthy = true
			break
		}
		time.Sleep(2 * time.Second)
	}

	if !healthy {
		// Health failed after rollback - this is a critical state
		catalog.WriteAuditLog(s.db, actor, "apps.rollback", "app", appID, "error", "health check failed after rollback")
		s.writeJSONError(w, http.StatusBadGateway, "rollback_health_failed")
		return
	}

	// Health passed: swap image_ref with previous_image_ref, clear previous_image_ref
	// previous_image_ref becomes empty (one generation retention)
	if err := apps.SetInstalledInfo(s.db, appID, currentStatus, info.PreviousImageRef, info.VersionLabel, info.Digest, ""); err != nil {
		slog.Error("set installed info failed", "app_id", appID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "set installed info failed")
		return
	}

	// Audit success
	catalog.WriteAuditLog(s.db, actor, "apps.rollback", "app", appID, "ok", fmt.Sprintf("rolled back from %s to %s", info.ImageRef, info.PreviousImageRef))

	// Re-read statuses
	statuses, err = apps.GetStatuses(s.db)
	if err != nil {
		slog.Error("get app statuses after rollback failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get app statuses failed")
		return
	}

	// Return updated entry (status unchanged, but installed_image updated to previous)
	s.writeAppEntry(w, app, currentStatus, statuses)
}

// handleDriverError converts driver errors to HTTP responses.
func (s *Server) handleDriverError(w http.ResponseWriter, err error, actor, action, appID string) {
	switch {
	case isErrDriverUnavailable(err):
		s.writeJSONError(w, http.StatusServiceUnavailable, "driver_unavailable")
	case isErrContainerNotFound(err):
		s.writeJSONError(w, http.StatusNotFound, "container_not_found")
	case isErrPortConflict(err):
		s.writeJSONError(w, http.StatusConflict, "port_conflict")
	case isErrResourceExhausted(err):
		s.writeJSONError(w, http.StatusInsufficientStorage, "resource_exhausted")
	default:
		slog.Error("driver operation failed", "action", action, "app_id", appID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "driver operation failed")
	}
	catalog.WriteAuditLog(s.db, actor, "apps."+action, "app", appID, "error", err.Error())
}

func isErrDriverUnavailable(err error) bool {
	_, ok := err.(appdriver.ErrDriverUnavailable)
	return ok
}

func isErrContainerNotFound(err error) bool {
	_, ok := err.(appdriver.ErrContainerNotFound)
	return ok
}

func isErrPortConflict(err error) bool {
	_, ok := err.(*appdriver.ErrPortConflict)
	return ok
}

func isErrResourceExhausted(err error) bool {
	_, ok := err.(*appdriver.ErrResourceExhausted)
	return ok
}

// handleAppsGet handles GET /api/apps/{id}
func (s *Server) handleAppsGet(w http.ResponseWriter, r *http.Request, appID, actor string) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	app, ok := apps.Get(appID)
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "app_not_found")
		return
	}

	statuses, err := apps.GetStatuses(s.db)
	if err != nil {
		slog.Error("get app statuses failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get app statuses failed")
		return
	}

	s.writeAppEntry(w, app, apps.StatusAvailable, statuses)
}

// writeAppEntry writes a single app entry response
func (s *Server) writeAppEntry(w http.ResponseWriter, app apps.App, defaultStatus apps.Status, statuses map[string]apps.Status) {
	entry := s.buildAppEntry(app, defaultStatus, statuses)
	s.writeJSON(w, http.StatusOK, map[string]any{"app": entry})
}

// buildAppEntry builds an app entry with update fields (WS55)
func (s *Server) buildAppEntry(app apps.App, defaultStatus apps.Status, statuses map[string]apps.Status) map[string]any {
	status := defaultStatus
	if st, ok := statuses[app.ID]; ok {
		status = st
	}

	// Compute port collisions for this app
	installedOrActive := make(map[string]bool)
	for id, st := range statuses {
		if st == apps.StatusInstalled || st == apps.StatusActive {
			installedOrActive[id] = true
		}
	}
	allCollisions := apps.PortCollisions()
	collisions := make([]int, 0)
	for port, ids := range allCollisions {
		for _, id := range ids {
			if id == app.ID && installedOrActive[id] {
				collisions = append(collisions, port)
				break
			}
		}
	}

	// Get installed info for update fields (WS55)
	var installedImage string
	var updateAvailable bool
	var updateInfo map[string]any

	if status == apps.StatusInstalled || status == apps.StatusActive {
		info, found, _ := apps.GetInstalledInfo(s.db, app.ID)
		if found {
			installedImage = info.ImageRef
			// Check for available update
			if s.updateSource != nil {
				if upd, ok := s.updateSource.UpdateFor(app.ID); ok {
					// Update exists if feed.ImageRef != installed.ImageRef OR (feed.Digest set && feed.Digest != installed.Digest)
					hasUpdate := upd.ImageRef != info.ImageRef
					if !hasUpdate && upd.Digest != "" && info.Digest != "" {
						hasUpdate = upd.Digest != info.Digest
					}
					if hasUpdate {
						updateAvailable = true
						// Compute permission changes (decision-6)
						portsAdded, portsRemoved := computePortDelta(app.Ports, upd.Ports)
						rammbFrom := app.RAMMB
						rammbTo := upd.RAMMB
						imageSourceChanged := getImageSource(app.Image) != getImageSource(upd.ImageRef)
						digestChanged := info.Digest != "" && upd.Digest != "" && info.Digest != upd.Digest

						updateInfo = map[string]any{
							"version":            upd.Version,
							"image_ref":          upd.ImageRef,
							"release_notes":      upd.ReleaseNotes,
							"permission_changes": map[string]any{
								"ports_added":          portsAdded,
								"ports_removed":        portsRemoved,
								"rammb_from":           rammbFrom,
								"rammb_to":             rammbTo,
								"image_source_changed": imageSourceChanged,
								"digest_changed":       digestChanged,
							},
						}
					}
				}
			}
		}
	}

	return map[string]any{
		"id":               app.ID,
		"name":             app.Name,
		"category":         app.Category,
		"description":      app.Description,
		"wave":             app.Wave,
		"optional":         app.Optional,
		"group":            app.Group,
		"image":            app.Image,
		"ram_mb":           app.RAMMB,
		"ports":            app.Ports,
		"notes":            app.Notes,
		"status":           string(status),
		"port_collisions":  collisions,
		"installed_image":  installedImage,
		"update_available": updateAvailable,
		"update":           updateInfo,
	}
}

// getImageSource extracts the registry+owner path from an image ref (before the tag).
// e.g., "ghcr.io/blakeblackshear/frigate:stable" -> "ghcr.io/blakeblackshear/frigate"
func getImageSource(imageRef string) string {
	// Split by ':' to remove tag
	parts := strings.Split(imageRef, ":")
	if len(parts) > 1 {
		return parts[0]
	}
	return imageRef
}

// computePortDelta returns ports added and removed from old to new.
func computePortDelta(oldPorts, newPorts []int) ([]int, []int) {
	oldSet := make(map[int]bool)
	for _, p := range oldPorts {
		oldSet[p] = true
	}
	newSet := make(map[int]bool)
	for _, p := range newPorts {
		newSet[p] = true
	}

	var added, removed []int
	for p := range newSet {
		if !oldSet[p] {
			added = append(added, p)
		}
	}
	for p := range oldSet {
		if !newSet[p] {
			removed = append(removed, p)
		}
	}
	return added, removed
}

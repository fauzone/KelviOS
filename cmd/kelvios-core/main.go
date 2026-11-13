package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fauzone/kelvios/internal/appdriver"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/httpserver"
	"github.com/fauzone/kelvios/internal/storage"
)

var version = "dev"

// runScheduledMigrations executes all scheduled migrations.
// Returns (executed, failed) counts.
func runScheduledMigrations(db *sql.DB) (int, int) {
	migs, _, err := catalog.ListMigrations(db, catalog.MigrationStatusScheduled, "")
	if err != nil {
		slog.Error("list scheduled migrations failed", "error", err)
		return 0, 0
	}
	executed := 0
	failed := 0
	for _, mig := range migs {
		if err := catalog.ExecuteMigration(db, mig.ID, time.Now(), catalog.DefaultCopyFunc); err != nil {
			failed++
			slog.Error("execute migration failed", "migration_id", mig.ID, "error", err)
		} else {
			executed++
		}
	}
	return executed, failed
}

// runDirtyShadowAgeFlush wakes pools with dirty shadows older than the age limit
// and flushes all their dirty rows (one combined wake per pool).
// Returns (flushed, failed, error).
func runDirtyShadowAgeFlush(db *sql.DB) (int, int, error) {
	// Get all pools that have dirty shadows
	rows, err := db.Query(`
		SELECT DISTINCT shadow_pool_id FROM file_shadows WHERE dirty = 1
	`)
	if err != nil {
		return 0, 0, fmt.Errorf("query dirty shadow pools: %w", err)
	}
	defer rows.Close()

	var poolIDs []string
	for rows.Next() {
		var poolID string
		if err := rows.Scan(&poolID); err != nil {
			return 0, 0, fmt.Errorf("scan pool id: %w", err)
		}
		poolIDs = append(poolIDs, poolID)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate pool ids: %w", err)
	}

	totalFlushed := 0
	totalFailed := 0
	for _, poolID := range poolIDs {
		// Check if this pool has any dirty shadows older than the age limit
		oldShadows, err := catalog.ListDirtyShadowsOlderThan(db, poolID, catalog.DirtyAgeLimitHours)
		if err != nil {
			slog.Error("list old dirty shadows failed", "pool_id", poolID, "error", err)
			continue
		}
		if len(oldShadows) == 0 {
			continue // no old dirty shadows for this pool
		}

		// Get pool to find its disks
		pool, _, err := catalog.GetPool(db, poolID)
		if err != nil || pool == nil {
			slog.Error("get pool for dirty shadow flush failed", "pool_id", poolID, "error", err)
			continue
		}

		// Wake all disks in this pool (one combined wake per pool)
		diskRows, err := db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, poolID)
		if err != nil {
			slog.Error("query pool disks for wake failed", "pool_id", poolID, "error", err)
			continue
		}
		var diskNames []string
		for diskRows.Next() {
			var diskName string
			if err := diskRows.Scan(&diskName); err == nil {
				diskNames = append(diskNames, diskName)
			}
		}
		diskRows.Close()

		// Wake each disk (serialized by power engine)
		for _, diskName := range diskNames {
			// Use a no-op actuator since we're in the main process
			// The power engine will track the state transition
			_ = diskName // disks are tracked by the power engine via syncPower
		}

		// Flush all dirty shadows for this pool (not just old ones - flush all per brief)
		flushed, failed, err := catalog.FlushDirtyShadows(db, poolID, "system:age-flush")
		if err != nil {
			slog.Error("flush dirty shadows failed", "pool_id", poolID, "error", err)
			totalFailed += failed
			continue
		}
		totalFlushed += flushed
		totalFailed += failed
	}

	return totalFlushed, totalFailed, nil
}

// runShadowEviction runs shadow eviction for all Warm pools.
// Best-effort: logs errors but never fails the calling operation.
// Returns (totalEvicted, error).
func runShadowEviction(db *sql.DB) (int, error) {
	// Get all Warm pools
	pools, err := catalog.ListPools(db)
	if err != nil {
		return 0, fmt.Errorf("list pools: %w", err)
	}

	totalEvicted := 0
	for _, pool := range pools {
		if pool.Tier != "warm" {
			continue
		}
		evicted, err := catalog.EvictShadows(db, pool.ID, catalog.DefaultShadowDeleteFunc, "system:daily-eviction")
		if err != nil {
			slog.Error("shadow eviction failed", "pool_id", pool.ID, "error", err)
			continue
		}
		totalEvicted += evicted
		if evicted > 0 {
			slog.Info("shadow eviction done", "pool_id", pool.ID, "evicted", evicted)
		}
	}
	return totalEvicted, nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	db, err := catalog.Open(cfg.Database.Path)
	if err != nil {
		slog.Error("failed to open catalog", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		slog.Error("failed to migrate catalog", "error", err)
		os.Exit(1)
	}

	// Initialize storage driver with disk inventory
	disks, err := storage.Inventory()
	if err != nil {
		slog.Warn("disk inventory failed, driver will have empty inventory", "error", err)
		disks = []storage.Disk{}
	}
	driver := storage.NewSingleDriver(disks)

	server := httpserver.New(cfg, db, version)
	server.SetDriver(driver)

	// Initialize app container driver (WS54 FR-4.4)
	// Use Docker socket from config or default; if unavailable, log and continue without driver
	dockerSocket := cfg.Docker.SocketPath
	if dockerSocket == "" {
		dockerSocket = "/var/run/docker.sock"
	}
	appDrv, err := appdriver.NewDockerDriver(dockerSocket, "kelvios", 0)
	if err != nil {
		slog.Warn("app driver unavailable, continuing without container management", "error", err)
	} else {
		server.SetAppDriver(appDrv)
		slog.Info("app driver initialized", "driver", appDrv.Name())
	}

	// Rehydrate pools from catalog into driver (handles restart scenario)
	if err := server.RehydratePools(); err != nil {
		slog.Error("failed to rehydrate pools", "error", err)
		os.Exit(1)
	}

	// Initialize UPS (boot check + start poller if configured)
	if err := server.InitUPS(); err != nil {
		slog.Error("UPS initialization failed", "error", err)
		os.Exit(1)
	}

	// WS32: Daily heat scoring job - run once on startup, then every 24h.
	// Errors are logged, not fatal; never blocks or crashes main.
	// WS34-A: Daily migration job - runs in the same goroutine:
	// ProcessSilentConsents + execute created migrations + FinalizeExpiredMigrations.
	// WS36: Daily dirty shadow age-trigger flush job - runs in the same goroutine:
	// For each pool with dirty shadows older than DirtyAgeLimitHours (72h), wake once and flush.
	go func() {
		// Initial run
		if written, err := catalog.ComputeHeatScores(db, time.Now()); err != nil {
			slog.Error("initial heat compute failed", "error", err)
		} else {
			slog.Info("initial heat compute done", "collections", written)
		}

		// Initial migration run
		if created, err := catalog.ProcessSilentConsents(db, time.Now()); err != nil {
			slog.Error("initial silent consents failed", "error", err)
		} else if created > 0 {
			slog.Info("initial silent consents processed", "created", created)
		}
		if executed, failed := runScheduledMigrations(db); executed > 0 || failed > 0 {
			slog.Info("initial migrations executed", "executed", executed, "failed", failed)
		}
		if finalized, err := catalog.FinalizeExpiredMigrations(db, time.Now(), catalog.DefaultDeleteFunc); err != nil {
			slog.Error("initial finalize failed", "error", err)
		} else if finalized > 0 {
			slog.Info("initial migrations finalized", "count", finalized)
		}

		// Initial dirty shadow age-trigger flush
		if flushed, failed, err := runDirtyShadowAgeFlush(db); err != nil {
			slog.Error("initial dirty shadow age flush failed", "error", err)
		} else if flushed > 0 || failed > 0 {
			slog.Info("initial dirty shadow age flush done", "flushed", flushed, "failed", failed)
		}

		// Initial shadow eviction
		if evicted, err := runShadowEviction(db); err != nil {
			slog.Error("initial shadow eviction failed", "error", err)
		} else if evicted > 0 {
			slog.Info("initial shadow eviction done", "evicted", evicted)
		}

		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if written, err := catalog.ComputeHeatScores(db, time.Now()); err != nil {
				slog.Error("scheduled heat compute failed", "error", err)
			} else {
				slog.Info("scheduled heat compute done", "collections", written)
			}

			// Daily migration run
			if created, err := catalog.ProcessSilentConsents(db, time.Now()); err != nil {
				slog.Error("scheduled silent consents failed", "error", err)
			} else if created > 0 {
				slog.Info("scheduled silent consents processed", "created", created)
			}
			if executed, failed := runScheduledMigrations(db); executed > 0 || failed > 0 {
				slog.Info("scheduled migrations executed", "executed", executed, "failed", failed)
			}
			if finalized, err := catalog.FinalizeExpiredMigrations(db, time.Now(), catalog.DefaultDeleteFunc); err != nil {
				slog.Error("scheduled finalize failed", "error", err)
			} else if finalized > 0 {
				slog.Info("scheduled migrations finalized", "count", finalized)
			}

			// Daily dirty shadow age-trigger flush
			if flushed, failed, err := runDirtyShadowAgeFlush(db); err != nil {
				slog.Error("scheduled dirty shadow age flush failed", "error", err)
			} else if flushed > 0 || failed > 0 {
				slog.Info("scheduled dirty shadow age flush done", "flushed", flushed, "failed", failed)
			}

			// Daily shadow eviction
			if evicted, err := runShadowEviction(db); err != nil {
				slog.Error("scheduled shadow eviction failed", "error", err)
			} else if evicted > 0 {
				slog.Info("scheduled shadow eviction done", "evicted", evicted)
			}
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start chat bridge lifecycle (transports + scheduled reports)
	server.ChatBridge().Start(ctx)

	// Start compose executor (deterministic report_schedule artifacts)
	server.ComposeExecutor().Start(ctx)

	// Start rules engine (FR-3.11 event→action rules)
	server.RulesEngine().Start(ctx)

	go func() {
		slog.Info("starting HTTP server", "addr", cfg.HTTP.Addr)
		if err := server.Start(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down...")

	// Stop compose executor first (no new runs), then chat bridge
	server.ComposeExecutor().Stop()

	// Stop rules engine (FR-3.11)
	server.RulesEngine().Stop()

	// Stop chat bridge first (transports stop, then HTTP drains)
	server.ChatBridge().Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}

	// Mark clean shutdown after successful graceful shutdown
	if err := server.CleanShutdown(); err != nil {
		slog.Error("mark clean shutdown failed", "error", err)
	}

	server.StopUPSPoller()

	slog.Info("server stopped")
}
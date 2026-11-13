package chatbridge

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/approval"
	"github.com/fauzone/kelvios/internal/catalog"
	_ "modernc.org/sqlite"
)

// fakeTransport captures replies for assertions.
type fakeTransport struct {
	name string
	sent map[string][]string
}

// fakeLifecycleTransport implements LifecycleTransport for testing.
type fakeLifecycleTransport struct {
	name       string
	sent       map[string][]string
	startCount int
	stopCount  int
	mu         sync.Mutex
	started    bool
	wg         sync.WaitGroup
}

func (f *fakeLifecycleTransport) Name() string { return f.name }

func (f *fakeLifecycleTransport) Send(chatID, text string) error {
	if f.sent == nil {
		f.sent = map[string][]string{}
	}
	f.sent[chatID] = append(f.sent[chatID], text)
	return nil
}

func (f *fakeLifecycleTransport) Start(ctx context.Context) {
	f.mu.Lock()
	if f.started {
		f.mu.Unlock()
		return
	}
	f.started = true
	f.startCount++
	f.mu.Unlock()
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		<-ctx.Done()
	}()
}

func (f *fakeLifecycleTransport) Stop() {
	f.mu.Lock()
	if !f.started {
		f.mu.Unlock()
		return
	}
	f.started = false
	f.stopCount++
	f.mu.Unlock()
	f.wg.Wait()
}

func (f *fakeTransport) Name() string { return f.name }

func (f *fakeTransport) Send(chatID, text string) error {
	if f.sent == nil {
		f.sent = map[string][]string{}
	}
	f.sent[chatID] = append(f.sent[chatID], text)
	return nil
}

func (f *fakeTransport) last(chatID string) string {
	msgs := f.sent[chatID]
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1]
}

func (f *fakeTransport) count(chatID string) int { return len(f.sent[chatID]) }

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestService(t *testing.T, db *sql.DB) (*Service, *fakeTransport) {
	t.Helper()
	svc := NewService(Deps{
		DB:            db,
		ApprovalStore: approval.NewApprovalStore(db),
	})
	fake := &fakeTransport{name: "telegram", sent: map[string][]string{}}
	svc.RegisterTransport(fake)
	return svc, fake
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func inbound(svc *Service, transport, chatID, text string) {
	_ = svc.HandleInbound(context.Background(), InboundMessage{
		Transport: transport,
		ChatID:    chatID,
		SenderID:  "user-1",
		Text:      text,
		At:        time.Now(),
	})
}

// pair links chatID with the given PIN and returns the pairing code used.
func pair(t *testing.T, svc *Service, fake *fakeTransport, chatID, pin string) string {
	t.Helper()
	pc, err := svc.CreatePairingCode("telegram")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	inbound(svc, "telegram", chatID, "/pair "+pc.Code+" "+pin)
	reply := fake.last(chatID)
	if !strings.Contains(reply, "Paired successfully") {
		t.Fatalf("pair reply = %q, want Paired successfully", reply)
	}
	return pc.Code
}

type auditRow struct {
	actor, action, targetType, targetID, result, detail string
}

func auditRows(t *testing.T, db *sql.DB, action string) []auditRow {
	t.Helper()
	rows, err := db.Query(`SELECT actor, action, target_type, target_id, result, detail FROM audit_log WHERE action = ? ORDER BY rowid`, action)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.actor, &r.action, &r.targetType, &r.targetID, &r.result, &r.detail); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func seedPoolCollectionRec(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, status, created_at, updated_at)
		VALUES ('pool-1','pool-a','node-1','hot','single','native','/mnt/pool-1',1000,0,0,'active',1700000000,1700000000)`)
	mustExec(t, db, `INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, created_at, updated_at)
		VALUES ('coll-1','photos','pool-1','/mnt/pool-1/photos','sda',0,1700000000,1700000000)`)
	mustExec(t, db, `INSERT INTO tier_recommendations (id, collection_id, pool_id, direction, reason, size_bytes, last_access, score, status, created_at, decided_at)
		VALUES ('rec-1','coll-1','pool-1','to-cold','stale',100,1699000000,0.9,'pending',1700000000,0)`)
}

func TestPairingCodeFormat(t *testing.T) {
	db := setupTestDB(t)
	svc, _ := newTestService(t, db)

	notDoubled := 0
	for i := 0; i < 20; i++ {
		pc, err := svc.CreatePairingCode("telegram")
		if err != nil {
			t.Fatalf("CreatePairingCode: %v", err)
		}
		if len(pc.Code) != 8 {
			t.Errorf("code %q: len %d, want 8", pc.Code, len(pc.Code))
		}
		for _, c := range pc.Code {
			if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
				t.Errorf("code %q: char %c outside A-Z0-9", pc.Code, c)
			}
		}
		if pc.Code[:4] != pc.Code[4:] {
			notDoubled++
		}
	}
	if notDoubled == 0 {
		t.Error("all 20 codes were doubled 4-char patterns (code[:4] == code[4:])")
	}
}

func TestPairingHappyPath(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	code := pair(t, svc, fake, "chat-1", "1234")

	link, err := svc.GetLink("telegram", "chat-1")
	if err != nil || link == nil {
		t.Fatalf("GetLink: link=%v err=%v", link, err)
	}
	if link.Label != "telegram:chat-1" {
		t.Errorf("label = %q, want telegram:chat-1", link.Label)
	}
	if link.PinHash == "1234" || !strings.Contains(link.PinHash, ":") {
		t.Errorf("pin_hash = %q, want salted salt:hash, not plaintext", link.PinHash)
	}
	// The code is consumed: a second lookup returns nil.
	if pc, _ := svc.GetPairingCode(code); pc != nil {
		t.Error("pairing code still valid after use, want consumed")
	}
	rows := auditRows(t, db, "chat.command.pair")
	if len(rows) != 1 || rows[0].result != "ok" || rows[0].actor != "chat:telegram:chat-1" {
		t.Errorf("pair audit rows = %+v, want one ok row with actor chat:telegram:chat-1", rows)
	}
}

func TestPairingWrongCode(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	inbound(svc, "telegram", "chat-1", "/pair NOTACODE 1234")
	if got := fake.last("chat-1"); got != "Invalid or expired pairing code." {
		t.Errorf("reply = %q, want invalid/expired", got)
	}
	if link, _ := svc.GetLink("telegram", "chat-1"); link != nil {
		t.Error("link created from a wrong code")
	}
	rows := auditRows(t, db, "chat.command.pair")
	if len(rows) != 1 || rows[0].result != "refused" {
		t.Errorf("pair audit rows = %+v, want one refused row", rows)
	}
}

func TestPairingExpiredCode(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	pc, err := svc.CreatePairingCode("telegram")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	mustExec(t, db, `UPDATE chat_pairing_codes SET expires_at = ? WHERE code = ?`, time.Now().Add(-time.Minute).Unix(), pc.Code)

	inbound(svc, "telegram", "chat-1", "/pair "+pc.Code+" 1234")
	if got := fake.last("chat-1"); got != "Invalid or expired pairing code." {
		t.Errorf("reply = %q, want invalid/expired", got)
	}
	if link, _ := svc.GetLink("telegram", "chat-1"); link != nil {
		t.Error("link created from an expired code")
	}
}

func TestPairingUsedCode(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	code := pair(t, svc, fake, "chat-1", "1234")

	// A second chat reusing the same code must be refused.
	inbound(svc, "telegram", "chat-2", "/pair "+code+" 9999")
	if got := fake.last("chat-2"); got != "Invalid or expired pairing code." {
		t.Errorf("reply = %q, want invalid/expired", got)
	}
	if link, _ := svc.GetLink("telegram", "chat-2"); link != nil {
		t.Error("second link created from a used code")
	}
}

func TestPairingTransportMismatch(t *testing.T) {
	db := setupTestDB(t)
	svc, _ := newTestService(t, db)
	wa := &fakeTransport{name: "whatsapp", sent: map[string][]string{}}
	svc.RegisterTransport(wa)

	pc, err := svc.CreatePairingCode("telegram")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	inbound(svc, "whatsapp", "chat-9", "/pair "+pc.Code+" 1234")
	if got := wa.last("chat-9"); got != "Pairing code is for a different transport." {
		t.Errorf("reply = %q, want transport mismatch", got)
	}
	if link, _ := svc.GetLink("whatsapp", "chat-9"); link != nil {
		t.Error("link created from a code for another transport")
	}
}

func TestPairingPINValidation(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	pc, err := svc.CreatePairingCode("telegram")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	inbound(svc, "telegram", "chat-1", "/pair "+pc.Code+" 12")
	if got := fake.last("chat-1"); got != "PIN must be 4-8 digits." {
		t.Errorf("short PIN reply = %q", got)
	}
	inbound(svc, "telegram", "chat-1", "/pair "+pc.Code+" 12ab")
	if got := fake.last("chat-1"); got != "PIN must be 4-8 digits." {
		t.Errorf("non-digit PIN reply = %q", got)
	}
	inbound(svc, "telegram", "chat-1", "/pair "+pc.Code)
	if got := fake.last("chat-1"); !strings.Contains(got, "Usage: /pair") {
		t.Errorf("missing PIN reply = %q, want usage", got)
	}
	if link, _ := svc.GetLink("telegram", "chat-1"); link != nil {
		t.Error("link created with an invalid PIN")
	}
}

func TestAllowlistRefusal(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	inbound(svc, "telegram", "chat-1", "/status")
	if got := fake.last("chat-1"); got != "This chat is not paired. Use /pair <code> <pin> to pair, or /help for help." {
		t.Errorf("reply = %q, want unpaired refusal", got)
	}
	rows := auditRows(t, db, "chat.command.refused")
	if len(rows) != 1 || rows[0].result != "refused" || rows[0].detail != "unpaired" {
		t.Errorf("refused audit rows = %+v", rows)
	}
}

func TestUnlinkedHelpAllowed(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	inbound(svc, "telegram", "chat-1", "/help")
	if got := fake.last("chat-1"); !strings.Contains(got, "/status") {
		t.Errorf("help reply = %q, want command list", got)
	}
	rows := auditRows(t, db, "chat.command.help")
	if len(rows) != 1 || rows[0].result != "ok" {
		t.Errorf("help audit rows = %+v", rows)
	}
}

func TestUnlinkedConversationRefused(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	inbound(svc, "telegram", "chat-1", "hello, are you there?")
	if got := fake.last("chat-1"); !strings.Contains(got, "not paired") {
		t.Errorf("reply = %q, want unpaired refusal, not passthrough", got)
	}
	rows := auditRows(t, db, "chat.conversation")
	if len(rows) != 0 {
		t.Errorf("conversation audit rows = %+v, want none for unlinked chat", rows)
	}
}

func TestPINStorageAndVerify(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	pair(t, svc, fake, "chat-1", "1234")
	link, _ := svc.GetLink("telegram", "chat-1")
	parts := strings.Split(link.PinHash, ":")
	if len(parts) != 2 || len(parts[0]) != 32 || len(parts[1]) != 64 {
		t.Errorf("pin_hash = %q, want 32-hex salt : 64-hex sha256", link.PinHash)
	}
	if !verifyPIN("1234", link.PinHash) {
		t.Error("verifyPIN(correct) = false")
	}
	if verifyPIN("0000", link.PinHash) {
		t.Error("verifyPIN(wrong) = true")
	}
	if verifyPIN("1234", "not-a-hash") {
		t.Error("verifyPIN(malformed hash) = true")
	}
}

func TestPINGateWrongPIN(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	pair(t, svc, fake, "chat-1", "1234")
	inbound(svc, "telegram", "chat-1", "/approve some-id 0000")
	if got := fake.last("chat-1"); got != "Invalid PIN." {
		t.Errorf("reply = %q, want Invalid PIN.", got)
	}
	rows := auditRows(t, db, "chat.command.approve")
	if len(rows) != 1 || rows[0].result != "error" || !strings.Contains(rows[0].detail, "PIN verification failed") {
		t.Errorf("approve audit rows = %+v", rows)
	}
}

func TestPINLockout(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	pair(t, svc, fake, "chat-1", "1234")
	store := approval.NewApprovalStore(db)
	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "mcp:test")
	if err != nil {
		t.Fatalf("Create approval: %v", err)
	}
	// Three consecutive wrong PINs.
	for i := 0; i < 3; i++ {
		inbound(svc, "telegram", "chat-1", "/approve "+req.ID+" 0000")
	}
	rows := auditRows(t, db, "chat.command.approve")
	if len(rows) != 3 || !strings.Contains(rows[2].detail, "LINK LOCKED") {
		t.Errorf("third failure audit = %+v, want LINK LOCKED detail", rows)
	}
	link, _ := svc.GetLink("telegram", "chat-1")
	if !svc.pinTracker.IsLocked(link.ID) {
		t.Error("link not locked after 3 failures")
	}
	// The correct PIN is still refused while locked, and the approval stays pending.
	inbound(svc, "telegram", "chat-1", "/approve "+req.ID+" 1234")
	if got := fake.last("chat-1"); !strings.Contains(strings.ToLower(got), "locked") {
		t.Errorf("reply = %q, want locked refusal", got)
	}
	after, err := store.Get(req.ID)
	if err != nil {
		t.Fatalf("Get approval: %v", err)
	}
	if after.Status != "pending" {
		t.Errorf("approval status = %q after locked attempt, want pending", after.Status)
	}
}

func TestCommandsEmptyState(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	pair(t, svc, fake, "chat-1", "1234")

	inbound(svc, "telegram", "chat-1", "/status")
	if got := fake.last("chat-1"); got != "No pools configured." {
		t.Errorf("/status = %q", got)
	}
	inbound(svc, "telegram", "chat-1", "/health")
	if got := fake.last("chat-1"); got != "No collections found." {
		t.Errorf("/health = %q", got)
	}
	inbound(svc, "telegram", "chat-1", "/backups")
	if got := fake.last("chat-1"); got != "No backup repositories configured." {
		t.Errorf("/backups = %q", got)
	}
	inbound(svc, "telegram", "chat-1", "/report")
	report := fake.last("chat-1")
	for _, want := range []string{
		"KelviOS System Report",
		"=== DISK HEALTH (Maintenance Status) ===",
		"=== BACKUP STATUS ===",
		"=== AWAITING APPROVAL ===",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("/report missing section %q in:\n%s", want, report)
		}
	}
	rows := auditRows(t, db, "chat.command.status")
	if len(rows) != 1 || rows[0].actor != "chat:telegram:chat-1" || rows[0].result != "ok" {
		t.Errorf("status audit rows = %+v", rows)
	}
}

func TestCommandsWithPool(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	seedPoolCollectionRec(t, db)
	pair(t, svc, fake, "chat-1", "1234")

	inbound(svc, "telegram", "chat-1", "/status")
	if got := fake.last("chat-1"); !strings.Contains(got, "Pools & Disks Summary:") || !strings.Contains(got, "pool-a") {
		t.Errorf("/status = %q, want pool summary with pool-a", got)
	}
	inbound(svc, "telegram", "chat-1", "/health")
	if got := fake.last("chat-1"); !strings.Contains(got, "coll-1") {
		t.Errorf("/health = %q, want collection coll-1", got)
	}
}

func TestApprovalsListAndApprove(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	seedPoolCollectionRec(t, db)
	store := approval.NewApprovalStore(db)
	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "mcp:test")
	if err != nil {
		t.Fatalf("Create approval: %v", err)
	}
	pair(t, svc, fake, "chat-1", "1234")

	inbound(svc, "telegram", "chat-1", "/approvals")
	listed := fake.last("chat-1")
	if !strings.Contains(listed, req.ID) || !strings.Contains(listed, "rec-1") {
		t.Errorf("/approvals = %q, want both %s and rec-1", listed, req.ID)
	}

	inbound(svc, "telegram", "chat-1", "/approve "+req.ID+" 1234")
	if got := fake.last("chat-1"); !strings.Contains(got, "marked as approved") {
		t.Errorf("/approve reply = %q", got)
	}
	after, err := store.Get(req.ID)
	if err != nil {
		t.Fatalf("Get approval: %v", err)
	}
	if after.Status != "approved" {
		t.Errorf("approval status = %q, want approved", after.Status)
	}
	if after.DecidedBy != "telegram:chat-1" {
		t.Errorf("decided_by = %q, want link label telegram:chat-1", after.DecidedBy)
	}
	// Chat approval must NEVER execute the destructive action itself.
	var poolCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pools WHERE id = 'pool-1'`).Scan(&poolCount); err != nil || poolCount != 1 {
		t.Errorf("pool-1 count = %d (err %v), want 1: chat approval deleted the pool", poolCount, err)
	}
}

func TestApproveTierRecommendation(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	seedPoolCollectionRec(t, db)
	pair(t, svc, fake, "chat-1", "1234")

	inbound(svc, "telegram", "chat-1", "/approve rec-1 1234")
	if got := fake.last("chat-1"); got != "Tier migration recommendation rec-1 approved." {
		t.Errorf("/approve rec reply = %q", got)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM tier_recommendations WHERE id = 'rec-1'`).Scan(&status); err != nil || status != "approved" {
		t.Errorf("rec status = %q (err %v), want approved", status, err)
	}
}

func TestRejectApproval(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	store := approval.NewApprovalStore(db)
	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "mcp:test")
	if err != nil {
		t.Fatalf("Create approval: %v", err)
	}
	pair(t, svc, fake, "chat-1", "1234")

	inbound(svc, "telegram", "chat-1", "/reject "+req.ID+" 1234")
	if got := fake.last("chat-1"); !strings.Contains(got, "rejected") {
		t.Errorf("/reject reply = %q", got)
	}
	after, err := store.Get(req.ID)
	if err != nil {
		t.Fatalf("Get approval: %v", err)
	}
	if after.Status != "rejected" {
		t.Errorf("approval status = %q, want rejected", after.Status)
	}
}

func TestApproveUsage(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	pair(t, svc, fake, "chat-1", "1234")
	inbound(svc, "telegram", "chat-1", "/approve only-id")
	if got := fake.last("chat-1"); got != "Usage: /approve <id> <pin>" {
		t.Errorf("reply = %q, want usage", got)
	}
}

func TestUnknownCommand(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	pair(t, svc, fake, "chat-1", "1234")
	inbound(svc, "telegram", "chat-1", "/bogus")
	if got := fake.last("chat-1"); got != "Unknown command. Use /help for available commands." {
		t.Errorf("reply = %q", got)
	}
	rows := auditRows(t, db, "chat.command.error")
	if len(rows) != 1 || rows[0].result != "error" {
		t.Errorf("error audit rows = %+v", rows)
	}
}

func TestConversationPassthrough(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	pair(t, svc, fake, "chat-1", "1234")
	inbound(svc, "telegram", "chat-1", "what is the weather?")
	if got := fake.last("chat-1"); !strings.Contains(got, "Agent not configured") {
		t.Errorf("default passthrough reply = %q", got)
	}
	rows := auditRows(t, db, "chat.conversation")
	if len(rows) != 1 || rows[0].result != "ok" {
		t.Errorf("conversation audit rows = %+v", rows)
	}

	// With a stub harness the stub's answer is the reply.
	db2 := setupTestDB(t)
	svc2 := NewService(Deps{
		DB:            db2,
		ApprovalStore: approval.NewApprovalStore(db2),
		HarnessRun: func(ctx context.Context, prompt string, tools []string) (string, error) {
			return "stub-answer for " + prompt, nil
		},
	})
	fake2 := &fakeTransport{name: "telegram", sent: map[string][]string{}}
	svc2.RegisterTransport(fake2)
	pair(t, svc2, fake2, "chat-1", "1234")
	inbound(svc2, "telegram", "chat-1", "hello agent")
	if got := fake2.last("chat-1"); got != "stub-answer for hello agent" {
		t.Errorf("stub passthrough reply = %q", got)
	}
}

func TestNoSecretsInAuditLog(t *testing.T) {
	db := setupTestDB(t)
	svc, _ := newTestService(t, db)

	pc, err := svc.CreatePairingCode("telegram")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	pin := "1234"
	inbound(svc, "telegram", "chat-1", "/pair "+pc.Code+" "+pin)
	inbound(svc, "telegram", "chat-1", "/approve some-id 0000")

	rows, err := db.Query(`SELECT actor, detail FROM audit_log`)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var actor, detail string
		if err := rows.Scan(&actor, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(actor, pc.Code) || strings.Contains(detail, pc.Code) {
			t.Errorf("pairing code leaked into audit row: actor=%q detail=%q", actor, detail)
		}
		if strings.Contains(actor, pin) || strings.Contains(detail, pin) {
			t.Errorf("PIN leaked into audit row: actor=%q detail=%q", actor, detail)
		}
	}
}

func TestReportBuildDirect(t *testing.T) {
	db := setupTestDB(t)
	seedPoolCollectionRec(t, db)
	store := approval.NewApprovalStore(db)
	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "mcp:test")
	if err != nil {
		t.Fatalf("Create approval: %v", err)
	}
	report, err := BuildReport(db, time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	for _, want := range []string{
		"KelviOS System Report",
		"=== DISK HEALTH (Maintenance Status) ===",
		"=== BACKUP STATUS ===",
		"=== AWAITING APPROVAL ===",
		"FR-3.7 Approvals (pending): 1",
		req.ID,
		"rec-1",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q in:\n%s", want, report)
		}
	}
}

func TestServiceLifecycle_StartStopTransports(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(Deps{
		DB:            db,
		ApprovalStore: approval.NewApprovalStore(db),
	})
	fake := &fakeLifecycleTransport{name: "telegram", sent: map[string][]string{}}
	svc.RegisterTransport(fake)

	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)
	if fake.startCount != 1 {
		t.Errorf("startCount = %d, want 1", fake.startCount)
	}

	cancel() // cancel context so transport goroutine exits
	svc.Stop()
	if fake.stopCount != 1 {
		t.Errorf("stopCount = %d, want 1", fake.stopCount)
	}
}

func TestServiceLifecycle_DoubleStartIsNoop(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(Deps{
		DB:            db,
		ApprovalStore: approval.NewApprovalStore(db),
	})
	fake := &fakeLifecycleTransport{name: "telegram", sent: map[string][]string{}}
	svc.RegisterTransport(fake)

	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)
	svc.Start(ctx) // second call should be no-op
	if fake.startCount != 1 {
		t.Errorf("startCount = %d after double Start, want 1", fake.startCount)
	}
	cancel()
	svc.Stop()
}

func TestServiceLifecycle_DoubleStopNoPanic(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(Deps{
		DB:            db,
		ApprovalStore: approval.NewApprovalStore(db),
	})
	fake := &fakeLifecycleTransport{name: "telegram", sent: map[string][]string{}}
	svc.RegisterTransport(fake)

	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)
	cancel()
	svc.Stop()
	svc.Stop() // second call should not panic
	if fake.stopCount != 1 {
		t.Errorf("stopCount = %d after double Stop, want 1", fake.stopCount)
	}
}

func TestServiceLifecycle_StartWithZeroTransports(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(Deps{
		DB:            db,
		ApprovalStore: approval.NewApprovalStore(db),
	})
	// No transports registered

	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx) // should not panic
	cancel()
	svc.Stop()     // should not panic
}

func TestServiceLifecycle_StopWithoutStart(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(Deps{
		DB:            db,
		ApprovalStore: approval.NewApprovalStore(db),
	})
	fake := &fakeLifecycleTransport{name: "telegram", sent: map[string][]string{}}
	svc.RegisterTransport(fake)

	// Stop without Start should not panic
	svc.Stop()
	if fake.stopCount != 0 {
		t.Errorf("stopCount = %d after Stop without Start, want 0", fake.stopCount)
	}
}

func TestServiceLifecycle_StartWithReportsDisabledStillStartsTransports(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(Deps{
		DB:            db,
		ApprovalStore: approval.NewApprovalStore(db),
	})
	fake := &fakeLifecycleTransport{name: "telegram", sent: map[string][]string{}}
	svc.RegisterTransport(fake)

	// Ensure reports are disabled (default)
	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)
	if fake.startCount != 1 {
		t.Errorf("startCount = %d with reports disabled, want 1", fake.startCount)
	}
	cancel()
	svc.Stop()
}

func TestServiceLifecycle_NonLifecycleTransportIgnored(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(Deps{
		DB:            db,
		ApprovalStore: approval.NewApprovalStore(db),
	})
	// Regular transport (no Start/Stop)
	fake := &fakeTransport{name: "plain", sent: map[string][]string{}}
	svc.RegisterTransport(fake)

	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx) // should not panic
	svc.Stop()     // should not panic
	cancel()
}

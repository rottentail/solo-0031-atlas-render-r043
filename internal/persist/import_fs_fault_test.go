package persist_test

import (
	"atlas-render/internal/persist"
	"atlas-render/internal/service"
	"os"
	"path/filepath"
	"testing"
)

// TestImport_ReadOnlyFilesystemFailsCleanly is a black-box fault injection
// that needs no test-only seam: the state directory is made read-only, so the
// production atomic writer genuinely cannot create its temp file. The import
// must fail while the in-memory state stays consistent and readable. This
// complements the injected-writer fault in import_fault_test.go with a real
// operating-system level failure.
func TestImport_ReadOnlyFilesystemFailsCleanly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits; fault cannot be injected")
	}
	// Build the package in a writable source store.
	srcStore, e := persist.Open(filepath.Join(t.TempDir(), "src", "state.json"))
	if e != nil {
		t.Fatalf("open source: %v", e)
	}
	srcSvc := &service.Service{Store: srcStore}
	pkg := seededPackage(t, srcSvc)
	if e = srcStore.Close(); e != nil {
		t.Fatalf("close source: %v", e)
	}

	// Target: create and close a writable store so the directory and lock
	// exist, then remove write permission from the directory.
	targetDir := filepath.Join(t.TempDir(), "ro-state")
	if e = os.MkdirAll(targetDir, 0o755); e != nil {
		t.Fatalf("mkdir: %v", e)
	}
	statePath := filepath.Join(targetDir, "atlas-state.json")
	seed, e := persist.Open(statePath)
	if e != nil {
		t.Fatalf("open target: %v", e)
	}
	before := seed.Read()
	if e = seed.Close(); e != nil {
		t.Fatalf("close target: %v", e)
	}
	if e = os.Chmod(targetDir, 0o555); e != nil {
		t.Fatalf("chmod: %v", e)
	}
	t.Cleanup(func() { _ = os.Chmod(targetDir, 0o755) })

	ro, e := persist.Open(statePath)
	if e != nil {
		// Opening itself may fail if the lock file cannot be reopened; that is
		// also an acceptable rejection as long as no state was written.
		t.Logf("open on read-only dir rejected: %v", e)
		return
	}
	defer ro.Close()
	svc := &service.Service{Store: ro}

	if _, e = svc.Import("ro-import", "ro", pkg); e == nil {
		t.Fatal("import succeeded on a read-only state directory")
	}

	// Live in-memory state must remain the pre-failure state and be readable.
	after := ro.Read()
	assertInventoryUnchanged(t, before, after)
	if _, ok := after.Scenes["ro-import"]; ok {
		t.Fatal("partial scene visible after filesystem fault")
	}

	// Restore permissions and prove the same store then commits normally.
	if e = os.Chmod(targetDir, 0o755); e != nil {
		t.Fatalf("restore chmod: %v", e)
	}
	if _, e = svc.Import("recovered-import", "ok", pkg); e != nil {
		t.Fatalf("import after restoring writability failed: %v", e)
	}
	got := ro.Read()
	if _, ok := got.Scenes["recovered-import"]; !ok {
		t.Fatal("recovered import did not land")
	}
	// Reopening the store must pass production state validation.
	if e = ro.Close(); e != nil {
		t.Fatalf("close: %v", e)
	}
	reopened, e := persist.Open(statePath)
	if e != nil {
		t.Fatalf("reopen after fault: %v", e)
	}
	defer reopened.Close()
	if _, ok := reopened.Read().Scenes["recovered-import"]; !ok {
		t.Fatal("durable state missing after recovery")
	}
}

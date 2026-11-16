package persist_test

import (
	"atlas-render/internal/bundle"
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"atlas-render/internal/service"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func openFaultStore(t *testing.T) (*persist.Store, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state", "atlas-state.json")
	st, e := persist.Open(statePath)
	if e != nil {
		t.Fatalf("open: %v", e)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, statePath
}

func seededPackage(t *testing.T, svc *service.Service) bundle.Package {
	t.Helper()
	img := solidPNG(t, 6, 6, color.NRGBA{R: 30, G: 120, B: 200, A: 255})
	asset, e := svc.ImportAsset("fault-tex", img)
	if e != nil {
		t.Fatalf("asset: %v", e)
	}
	doc := model.Document{Width: 24, Height: 18, Background: "#334455", Layers: []model.Layer{{
		ID: "img", Kind: "image", Width: 24, Height: 18, Opacity: 1, AssetID: asset.ID,
		Transform: model.Transform{ScaleX: 1, ScaleY: 1},
	}}}
	if _, e = svc.PutScene("fault-source", "seed", 0, doc); e != nil {
		t.Fatalf("scene: %v", e)
	}
	snap, e := svc.Freeze("fault-source", 1)
	if e != nil {
		t.Fatalf("freeze: %v", e)
	}
	return bundle.Export(snap)
}

// TestImport_DiskWriteFailsMidCommit proves the write-failure branch of the
// transaction: validation has already passed when the disk fault fires, yet no
// partial scene or asset may become visible, the old in-memory state stays
// readable, the persisted file on disk stays byte-identical, and no temp file
// is left behind.
func TestImport_DiskWriteFailsMidCommit(t *testing.T) {
	t.Run("empty instance keeps zero landing", func(t *testing.T) {
		// Package is produced in a separate source instance; the target store
		// starts empty so any surviving scene or asset would be a partial land.
		srcStore := openStoreFor(t)
		srcSvc := &service.Service{Store: srcStore}
		pkg := seededPackage(t, srcSvc)
		_ = srcStore.Close()

		dst, statePath := openFaultStore(t)
		dstSvc := &service.Service{Store: dst}
		if pre := dst.Read(); len(pre.Scenes)+len(pre.Assets) != 0 {
			t.Fatal("destination fixture is not empty")
		}
		runWriteFaultImport(t, dst, statePath, dstSvc, pkg, "doomed-import")
		if got := dst.Read(); len(got.Scenes) != 0 || len(got.Assets) != 0 {
			t.Fatalf("partial landing on empty instance: %d scenes, %d assets", len(got.Scenes), len(got.Assets))
		}
	})

	t.Run("populated instance keeps prior state intact", func(t *testing.T) {
		st, statePath := openFaultStore(t)
		svc := &service.Service{Store: st}
		pkg := seededPackage(t, svc)
		runWriteFaultImport(t, st, statePath, svc, pkg, "doomed-import")
		// The pre-existing scene and asset remain exactly as they were.
		got := st.Read()
		if _, ok := got.Scenes["doomed-import"]; ok {
			t.Fatal("partial scene visible after disk failure")
		}
		if _, ok := got.Scenes["fault-source"]; !ok {
			t.Fatal("pre-existing scene lost after disk failure")
		}
		if a := got.Assets["fault-tex"]; a.State != "available" {
			t.Fatalf("pre-existing asset degraded after disk failure: %+v", a)
		}
	})
}

// runWriteFaultImport arms the disk fault, runs one import, and asserts the
// generic failure plus full on-disk and in-memory preservation guarantees.
func runWriteFaultImport(t *testing.T, st *persist.Store, statePath string, svc *service.Service, pkg bundle.Package, sceneID string) {
	t.Helper()
	beforeMem := st.Read()
	// A never-committed store has no state file yet; that is itself a valid
	// precondition and the failed import must not create one.
	beforeDisk, beforeReadErr := os.ReadFile(statePath)
	beforeExisted := beforeReadErr == nil
	if beforeReadErr != nil && !os.IsNotExist(beforeReadErr) {
		t.Fatalf("read persisted file: %v", beforeReadErr)
	}
	beforeEntries := listDir(t, filepath.Dir(statePath))

	restore, tripped := st.SetWriteFault(true)
	_, err := svc.Import(sceneID, "doomed", pkg)
	restore()

	if err == nil {
		t.Fatal("import unexpectedly succeeded with write fault armed")
	}
	if !tripped() {
		t.Fatal("write fault was never exercised; test does not cover the failure path")
	}
	// A disk failure surfaces as a generic internal error rather than a domain
	// rejection; either way the commit must not land.
	var de *model.Error
	if errors.As(err, &de) && (de.Code == "invalid" || de.Code == "conflict") {
		t.Fatalf("disk failure misclassified as domain error: %+v", de)
	}

	// 1. In-memory state is untouched and still fully readable.
	afterMem := st.Read()
	assertInventoryUnchanged(t, beforeMem, afterMem)
	if _, ok := afterMem.Scenes[sceneID]; ok {
		t.Fatalf("partial scene %q visible in memory after disk failure", sceneID)
	}

	// 2. The persisted file on disk is unchanged (or still absent).
	afterDisk, afterReadErr := os.ReadFile(statePath)
	switch {
	case !beforeExisted && os.IsNotExist(afterReadErr):
		// Good: the failed commit created no state file.
	case beforeExisted && afterReadErr == nil && string(afterDisk) == string(beforeDisk):
		// Good: the existing file is byte-identical.
	default:
		t.Fatalf("persisted state changed despite failed commit (beforeExisted=%v afterErr=%v)", beforeExisted, afterReadErr)
	}

	// 3. No leftover temp files from the aborted atomic write.
	afterEntries := listDir(t, filepath.Dir(statePath))
	if len(afterEntries) != len(beforeEntries) {
		t.Fatalf("directory entries changed: before=%v after=%v", beforeEntries, afterEntries)
	}
	for _, name := range afterEntries {
		if strings.HasPrefix(name, ".atlas-write-") || strings.HasSuffix(name, ".tmp") {
			t.Fatalf("aborted write left temp file: %s", name)
		}
	}
}

// openStoreFor creates an additional isolated store (used as the package
// source while another store is the fault-injection target).
func openStoreFor(t *testing.T) *persist.Store {
	t.Helper()
	st, e := persist.Open(filepath.Join(t.TempDir(), "source", "atlas-state.json"))
	if e != nil {
		t.Fatalf("open source store: %v", e)
	}
	return st
}

// TestImport_StoreRecoversAfterWriteFault proves a failed disk commit does not
// poison the store: disarming the fault restores normal durable commits, and
// the same package then imports cleanly.
func TestImport_StoreRecoversAfterWriteFault(t *testing.T) {
	st, _ := openFaultStore(t)
	svc := &service.Service{Store: st}
	pkg := seededPackage(t, svc)
	beforeCount := len(st.Read().Scenes)

	restore, tripped := st.SetWriteFault(true)
	if _, e := svc.Import("first-attempt", "x", pkg); e == nil {
		t.Fatal("expected write failure")
	}
	restore()
	if !tripped() {
		t.Fatal("fault not exercised")
	}

	if _, e := svc.Import("second-attempt", "recovered", pkg); e != nil {
		t.Fatalf("import after fault recovery failed: %v", e)
	}
	got := st.Read()
	if len(got.Scenes) != beforeCount+1 {
		t.Fatalf("scene count after recovery = %d, want %d", len(got.Scenes), beforeCount+1)
	}
	if _, ok := got.Scenes["second-attempt"]; !ok {
		t.Fatal("recovered import did not land")
	}
	if _, ok := got.Scenes["first-attempt"]; ok {
		t.Fatal("failed attempt landed after fault was disarmed")
	}
	if got.Assets["fault-tex"].State != "available" {
		t.Fatal("imported asset not available after recovery")
	}
}

// TestImport_ReopenAfterFaultKeepsOldState verifies durability semantics: with
// the commit rejected, closing and reopening the store yields exactly the
// pre-failure state, and the reopened store validates successfully.
func TestImport_ReopenAfterFaultKeepsOldState(t *testing.T) {
	st, statePath := openFaultStore(t)
	svc := &service.Service{Store: st}
	pkg := seededPackage(t, svc)
	before := st.Read()

	restore, _ := st.SetWriteFault(true)
	if _, e := svc.Import("ghost", "x", pkg); e == nil {
		t.Fatal("expected write failure")
	}
	restore()
	if e := st.Close(); e != nil {
		t.Fatalf("close: %v", e)
	}

	reopened, e := persist.Open(statePath)
	if e != nil {
		t.Fatalf("reopen failed (persisted file may be corrupt): %v", e)
	}
	defer reopened.Close()
	assertInventoryUnchanged(t, before, reopened.Read())
	if _, ok := reopened.Read().Scenes["ghost"]; ok {
		t.Fatal("ghost scene survived in persisted state")
	}
}

// TestWriteFaultDoesNotBypassValidation makes the "no relaxing production
// checks" guarantee explicit: while the fault is armed, an invalid package
// still fails at validation, before any write is even attempted.
func TestWriteFaultDoesNotBypassValidation(t *testing.T) {
	st, _ := openFaultStore(t)
	svc := &service.Service{Store: st}
	pkg := seededPackage(t, svc)
	bad := pkg
	bad.Digest = model.Digest([]byte("tampered"))

	restore, tripped := st.SetWriteFault(true)
	defer restore()
	_, e := svc.Import("invalid-under-fault", "x", bad)
	var de *model.Error
	if !errors.As(e, &de) || de.Code != "invalid" {
		t.Fatalf("invalid package not rejected by production validation under fault: %v", e)
	}
	if tripped() {
		t.Fatal("validation failure should never reach the disk writer")
	}
}

func assertInventoryUnchanged(t *testing.T, before, after model.State) {
	t.Helper()
	if len(after.Scenes) != len(before.Scenes) ||
		len(after.Assets) != len(before.Assets) ||
		len(after.Snapshots) != len(before.Snapshots) ||
		len(after.Jobs) != len(before.Jobs) ||
		len(after.Events) != len(before.Events) ||
		after.Sequence != before.Sequence {
		t.Fatalf("state changed after failed disk commit:\n before=%d scenes/%d assets/%d snaps seq=%d\n after =%d scenes/%d assets/%d snaps seq=%d",
			len(before.Scenes), len(before.Assets), len(before.Snapshots), before.Sequence,
			len(after.Scenes), len(after.Assets), len(after.Snapshots), after.Sequence)
	}
	for id, s := range before.Scenes {
		if !reflect.DeepEqual(after.Scenes[id], s) {
			t.Fatalf("scene %s mutated", id)
		}
	}
	for id, a := range before.Assets {
		if !reflect.DeepEqual(after.Assets[id], a) {
			t.Fatalf("asset %s mutated", id)
		}
	}
	for id, snap := range before.Snapshots {
		if !reflect.DeepEqual(after.Snapshots[id], snap) {
			t.Fatalf("snapshot %s mutated", id)
		}
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, e := os.ReadDir(dir)
	if e != nil {
		t.Fatalf("list dir: %v", e)
	}
	names := make([]string, 0, len(entries))
	for _, en := range entries {
		names = append(names, en.Name())
	}
	return names
}

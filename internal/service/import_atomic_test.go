package service_test

import (
	"atlas-render/internal/bundle"
	"atlas-render/internal/model"
	"atlas-render/internal/service"
	"errors"
	"image/color"
	"reflect"
	"sort"
	"sync"
	"testing"
)

// fixture builds a store containing one scene with an image asset and a frozen
// snapshot, then exports an exchange package from that snapshot. It returns
// everything tests need to craft conflicts and re-imports.
func fixture(t *testing.T) (*service.Service, bundle.Package, model.Asset) {
	t.Helper()
	st := newStore(t)
	svc := &service.Service{Store: st}
	pngData := makePNG(t, 8, 8, color.NRGBA{R: 12, G: 160, B: 220, A: 255})
	asset := importAsset(t, svc, "tex-ribbon", pngData)
	doc := buildDocument(asset.ID)
	_, snap := seedSceneAndSnapshot(t, svc, "source-scene", doc)
	pkg := exportPackage(t, svc, snap.ID)
	return svc, pkg, asset
}

func assertDomainError(t *testing.T, e error, status int, code string) {
	t.Helper()
	if e == nil {
		t.Fatalf("expected %d/%s error, call succeeded", status, code)
	}
	var de *model.Error
	if !errors.As(e, &de) {
		t.Fatalf("expected model.Error, got %T: %v", e, e)
	}
	if de.Status != status || de.Code != code {
		t.Fatalf("expected %d/%s, got %d/%s: %v", status, code, de.Status, de.Code, e)
	}
}

// TestImport_ValidPackageLandsAtomically is the happy path baseline: importing
// a valid package into an empty instance creates exactly one scene plus the
// closure assets in a single commit.
func TestImport_ValidPackageLandsAtomically(t *testing.T) {
	_, pkg, asset := fixture(t)
	// Simulate an empty destination instance with a fresh store.
	dst := newStore(t)
	dstSvc := &service.Service{Store: dst}

	sc, e := dstSvc.Import("restored-scene", "restored", pkg)
	if e != nil {
		t.Fatalf("import: %v", e)
	}
	if sc.Revision != 1 || sc.State != "active" {
		t.Fatalf("imported scene revision/state wrong: %+v", sc)
	}
	if sc.Document.Background != pkg.Snapshot.Document.Background {
		t.Fatal("imported document differs from package document")
	}
	state := dst.Read()
	if len(state.Scenes) != 1 || len(state.Assets) != 1 || len(state.Snapshots) != 0 || len(state.Jobs) != 0 {
		t.Fatalf("unexpected destination inventory: %+v", inventory(state))
	}
	gotAsset, ok := state.Assets[asset.ID]
	if !ok {
		t.Fatal("package asset not present after import")
	}
	if gotAsset.Digest != asset.Digest || gotAsset.State != "available" {
		t.Fatalf("imported asset degraded: %+v", gotAsset)
	}
	events := lastEvents(state, 1)
	if len(events) != 1 || events[0].Kind != "scene.imported" {
		t.Fatalf("expected single scene.imported event, got %+v", events)
	}
}

func TestImport_InvalidDestinationRejectedBeforeTouchingState(t *testing.T) {
	svc, pkg, _ := fixture(t)
	before := svc.Store.Read()
	cases := map[string]struct {
		id   string
		name string
	}{
		"empty id":            {"", "name"},
		"illegal id chars":    {"bad/id", "name"},
		"id too long":         {"a012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789", "name"},
		"empty name":          {"target", ""},
		"name over 160 chars": {"target", string(make([]byte, 161))},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, e := svc.Import(c.id, c.name, pkg)
			assertDomainError(t, e, 400, "invalid")
			assertSameState(t, svc.Store.Read(), before)
		})
	}
}

func TestImport_CorruptPackageLeavesNoTrace(t *testing.T) {
	svc, pkg, _ := fixture(t)
	before := svc.Store.Read()

	t.Run("outer digest mismatch", func(t *testing.T) {
		bad := pkg
		bad.Digest = model.Digest([]byte("tampered"))
		_, e := svc.Import("would-be-scene", "x", bad)
		assertDomainError(t, e, 400, "invalid")
		assertNoPartialLanding(t, svc.Store, before, "would-be-scene")
	})
	t.Run("document mutated", func(t *testing.T) {
		bad := pkg
		bad.Snapshot.Document.Height += 4
		_, e := svc.Import("would-be-scene-2", "x", bad)
		assertDomainError(t, e, 400, "invalid")
		assertNoPartialLanding(t, svc.Store, before, "would-be-scene-2")
	})
	t.Run("asset bytes mutated", func(t *testing.T) {
		bad := pkg
		a := bad.Snapshot.Assets[firstAssetID(t, bad)]
		a.PNG = append(append([]byte(nil), a.PNG...), 0x01)
		bad.Snapshot.Assets[a.ID] = a
		_, e := svc.Import("would-be-scene-3", "x", bad)
		assertDomainError(t, e, 400, "invalid")
		assertNoPartialLanding(t, svc.Store, before, "would-be-scene-3")
	})
	t.Run("closure missing asset", func(t *testing.T) {
		bad := pkg
		delete(bad.Snapshot.Assets, firstAssetID(t, bad))
		_, e := svc.Import("would-be-scene-4", "x", bad)
		assertDomainError(t, e, 400, "invalid")
		assertNoPartialLanding(t, svc.Store, before, "would-be-scene-4")
	})
}

// TestImport_DestinationSceneExistsRejectsWholeCommit covers the same-name
// destination conflict. Existing scene, assets and historical snapshots must
// stay readable and byte-identical.
func TestImport_DestinationSceneExistsRejectsWholeCommit(t *testing.T) {
	svc, pkg, _ := fixture(t)
	// Re-import once successfully to create a destination scene with assets.
	if _, e := svc.Import("dup-scene", "first landing", pkg); e != nil {
		t.Fatalf("initial import: %v", e)
	}
	before := svc.Store.Read()

	_, e := svc.Import("dup-scene", "second attempt", pkg)
	assertDomainError(t, e, 409, "conflict")

	after := svc.Store.Read()
	assertSameState(t, after, before)
	if got := after.Scenes["dup-scene"].Name; got != "first landing" {
		t.Fatalf("existing scene overwritten: name=%q", got)
	}
	// No extra event from the failed commit.
	if len(after.Events) != len(before.Events) {
		t.Fatalf("failed import emitted events: before=%d after=%d", len(before.Events), len(after.Events))
	}
}

// TestImport_SameAssetIDDifferentContent is the shared-resource protection
// rule: an asset ID already present with different bytes must abort the whole
// import, leaving neither the new scene nor any partial asset state.
func TestImport_SameAssetIDDifferentContent(t *testing.T) {
	svc, _, original := fixture(t)
	before := svc.Store.Read()

	// Build a second package whose asset reuses the same ID but carries
	// different PNG bytes (same dimensions, so only the content differs).
	otherPNG := makePNG(t, 8, 8, color.NRGBA{R: 200, G: 30, B: 90, A: 255})
	st2 := newStore(t)
	svc2 := &service.Service{Store: st2}
	otherAsset := importAsset(t, svc2, original.ID, otherPNG)
	otherDoc := buildDocument(otherAsset.ID)
	_, otherSnap := seedSceneAndSnapshot(t, svc2, "other-source", otherDoc)
	otherPkg := exportPackage(t, svc2, otherSnap.ID)

	if otherPkg.Snapshot.Assets[original.ID].Digest == original.Digest {
		t.Fatal("fixture error: conflicting assets need different digests")
	}

	_, e := svc.Import("conflict-scene", "collision", otherPkg)
	assertDomainError(t, e, 409, "conflict")
	assertNoPartialLanding(t, svc.Store, before, "conflict-scene")

	// The pre-existing asset must remain the original bytes and stay available.
	got := svc.Store.Read().Assets[original.ID]
	if got.Digest != original.Digest || got.State != "available" {
		t.Fatalf("existing asset mutated by failed import: %+v", got)
	}
}

// TestImport_RetiredDestinationAssetRejects ensures a retired asset with a
// matching ID blocks an import even when the bytes would otherwise match.
func TestImport_RetiredDestinationAssetRejects(t *testing.T) {
	svc, pkg, original := fixture(t)
	// Land the package once so the destination owns the asset, then retire it.
	if _, e := svc.Import("retire-scene", "landed", pkg); e != nil {
		t.Fatalf("initial import: %v", e)
	}
	if e := svc.RetireAsset(original.ID); e != nil {
		t.Fatalf("retire: %v", e)
	}
	before := svc.Store.Read()

	_, e := svc.Import("retire-target", "retry", pkg)
	assertDomainError(t, e, 409, "conflict")
	assertNoPartialLanding(t, svc.Store, before, "retire-target")
	if svc.Store.Read().Assets[original.ID].State != "retired" {
		t.Fatal("retired asset state changed after failed import")
	}
}

// TestImport_SameContentIsIdempotentSharing: identical bytes under the same ID
// must be accepted and must reuse the existing asset rather than replace it.
func TestImport_SameContentReusesExistingAsset(t *testing.T) {
	svc, pkg, original := fixture(t)
	if _, e := svc.Import("share-a", "a", pkg); e != nil {
		t.Fatalf("first import: %v", e)
	}
	if _, e := svc.Import("share-b", "b", pkg); e != nil {
		t.Fatalf("second import of identical package: %v", e)
	}
	state := svc.Store.Read()
	if len(state.Assets) != 1 {
		t.Fatalf("identical asset duplicated: %d assets", len(state.Assets))
	}
	if state.Assets[original.ID].Digest != original.Digest {
		t.Fatal("shared asset digest changed")
	}
	if len(state.Scenes) != 3 { // source + two imports
		t.Fatalf("scene count = %d, want 3", len(state.Scenes))
	}
}

// TestImport_ParallelContention serializes through the store transaction.
// Many goroutines race imports that collide on scene ID, asset ID, or run
// concurrently against an empty instance; exactly one writer per key wins and
// no partial state ever becomes visible.
func TestImport_ParallelContention(t *testing.T) {
	t.Run("same scene id raced", func(t *testing.T) {
		_, pkg, _ := fixture(t)
		dst := newStore(t)
		svc := &service.Service{Store: dst}
		const n = 24
		var wg sync.WaitGroup
		errs := make([]error, n)
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				_, errs[i] = svc.Import("raced-scene", "race", pkg)
			}(i)
		}
		wg.Wait()
		ok, conflict := 0, 0
		for _, e := range errs {
			switch {
			case e == nil:
				ok++
			default:
				var de *model.Error
				if errors.As(e, &de) && de.Status == 409 {
					conflict++
				} else {
					t.Fatalf("unexpected race error: %v", e)
				}
			}
		}
		if ok != 1 || conflict != n-1 {
			t.Fatalf("race outcomes: success=%d conflict=%d, want 1/%d", ok, conflict, n-1)
		}
		state := dst.Read()
		if len(state.Scenes) != 1 || len(state.Assets) != 1 {
			t.Fatalf("partial state after race: %+v", inventory(state))
		}
	})

	t.Run("distinct scenes raced against empty instance", func(t *testing.T) {
		_, pkg, asset := fixture(t)
		dst := newStore(t)
		svc := &service.Service{Store: dst}
		const n = 16
		var wg sync.WaitGroup
		errs := make([]error, n)
		ids := make([]string, n)
		wg.Add(n)
		for i := 0; i < n; i++ {
			ids[i] = "parallel-scene-" + digits(i)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = svc.Import(ids[i], "parallel", pkg)
			}(i)
		}
		wg.Wait()
		for i, e := range errs {
			if e != nil {
				t.Fatalf("import %s failed under contention: %v", ids[i], e)
			}
		}
		state := dst.Read()
		if len(state.Scenes) != n {
			t.Fatalf("scenes = %d, want %d", len(state.Scenes), n)
		}
		// The shared asset closure is reused, never duplicated or corrupted.
		if len(state.Assets) != 1 || state.Assets[asset.ID].Digest != asset.Digest {
			t.Fatalf("shared asset corrupted under contention: %+v", inventory(state))
		}
		sorted := append([]string(nil), ids...)
		sort.Strings(sorted)
		for _, id := range sorted {
			if _, ok := state.Scenes[id]; !ok {
				t.Fatalf("committed scene missing: %s", id)
			}
		}
	})

	t.Run("mixed valid and corrupt raced", func(t *testing.T) {
		_, good, _ := fixture(t)
		dst := newStore(t)
		svc := &service.Service{Store: dst}
		const n = 20
		var wg sync.WaitGroup
		wg.Add(n * 2)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				_, _ = svc.Import("good-"+digits(i), "ok", good)
			}(i)
			go func(i int) {
				defer wg.Done()
				bad := good
				bad.Digest = model.Digest([]byte("nope"))
				_, _ = svc.Import("bad-"+digits(i), "no", bad)
			}(i)
		}
		wg.Wait()
		state := dst.Read()
		if len(state.Scenes) != n {
			t.Fatalf("expected exactly %d good scenes, got inventory %+v", n, inventory(state))
		}
		for i := 0; i < n; i++ {
			if _, ok := state.Scenes["good-"+digits(i)]; !ok {
				t.Fatal("successful import lost under contention")
			}
			if _, ok := state.Scenes["bad-"+digits(i)]; ok {
				t.Fatal("rejected import left a scene behind")
			}
		}
	})
}

// TestImport_HistoricalSnapshotsSurviveFailedImports proves pre-existing
// targets and historical snapshots stay fully usable after a rejected import.
func TestImport_HistoricalSnapshotsSurviveFailedImports(t *testing.T) {
	svc, pkg, original := fixture(t)
	before := svc.Store.Read()
	var historicalID string
	for id := range before.Snapshots {
		historicalID = id
	}

	// Repeated adversarial failures against the same instance.
	for k := 0; k < 5; k++ {
		bad := pkg
		bad.Snapshot.Document.Width++
		if _, e := svc.Import("doomed-"+digits(k), "x", bad); e == nil {
			t.Fatal("corrupt package accepted")
		}
	}
	_, e := svc.Import("source-scene", "same-name", pkg)
	assertDomainError(t, e, 409, "conflict")

	after := svc.Store.Read()
	snap, ok := after.Snapshots[historicalID]
	if !ok {
		t.Fatal("historical snapshot disappeared after failed imports")
	}
	if snap.Digest != before.Snapshots[historicalID].Digest {
		t.Fatal("historical snapshot digest changed")
	}
	if snap.State != "active" {
		t.Fatal("historical snapshot state changed")
	}
	if after.Scenes["source-scene"].Revision != before.Scenes["source-scene"].Revision {
		t.Fatal("existing scene revision moved after failed imports")
	}
	if after.Assets[original.ID].Digest != original.Digest {
		t.Fatal("existing asset bytes changed after failed imports")
	}
	// A subsequent legitimate import still succeeds on the same store.
	if _, e = svc.Import("after-storm", "fine", pkg); e != nil {
		t.Fatalf("store unusable after failed imports: %v", e)
	}
}

// ---- helpers ----------------------------------------------------------------

func firstAssetID(t *testing.T, p bundle.Package) string {
	t.Helper()
	for id := range p.Snapshot.Assets {
		return id
	}
	t.Fatal("package has no assets")
	return ""
}

func assertNoPartialLanding(t *testing.T, st interface {
	Read() model.State
}, before model.State, rejectedSceneID string) {
	t.Helper()
	after := st.Read()
	if _, ok := after.Scenes[rejectedSceneID]; ok {
		t.Fatalf("rejected import left scene %s behind", rejectedSceneID)
	}
	assertSameState(t, after, before)
	// No import event may have been committed.
	for _, ev := range after.Events[len(before.Events):] {
		if ev.Kind == "scene.imported" {
			t.Fatalf("rejected import emitted scene.imported: %+v", ev)
		}
	}
}

func assertSameState(t *testing.T, after, before model.State) {
	t.Helper()
	if len(after.Scenes) != len(before.Scenes) ||
		len(after.Assets) != len(before.Assets) ||
		len(after.Snapshots) != len(before.Snapshots) ||
		len(after.Jobs) != len(before.Jobs) ||
		len(after.Artifacts) != len(before.Artifacts) ||
		len(after.Receipts) != len(before.Receipts) ||
		len(after.Events) != len(before.Events) ||
		after.Sequence != before.Sequence {
		t.Fatalf("state changed despite failed commit:\n  before=%+v\n  after =%+v", inventory(before), inventory(after))
	}
	for id, s := range before.Scenes {
		if !reflect.DeepEqual(after.Scenes[id], s) {
			t.Fatalf("scene %s mutated by failed commit", id)
		}
	}
	for id, a := range before.Assets {
		if !reflect.DeepEqual(after.Assets[id], a) {
			t.Fatalf("asset %s mutated by failed commit", id)
		}
	}
	for id, snap := range before.Snapshots {
		if !reflect.DeepEqual(after.Snapshots[id], snap) {
			t.Fatalf("snapshot %s mutated by failed commit", id)
		}
	}
}

func inventory(s model.State) map[string]int {
	return map[string]int{
		"scenes": len(s.Scenes), "assets": len(s.Assets), "snapshots": len(s.Snapshots),
		"jobs": len(s.Jobs), "artifacts": len(s.Artifacts), "receipts": len(s.Receipts),
		"events": len(s.Events), "sequence": s.Sequence,
	}
}

func lastEvents(s model.State, n int) []model.Event {
	if len(s.Events) <= n {
		return s.Events
	}
	return s.Events[len(s.Events)-n:]
}

func digits(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

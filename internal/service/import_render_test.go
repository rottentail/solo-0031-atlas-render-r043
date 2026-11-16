package service_test

import (
	"atlas-render/internal/codec"
	"atlas-render/internal/model"
	"atlas-render/internal/observe"
	"atlas-render/internal/raster"
	"atlas-render/internal/service"
	"context"
	"image/color"
	"reflect"
	"testing"
)

// renderSnapshot drives the exact production raster pipeline (Compile ->
// Render -> PNG encode) used by the worker, so test pixels are production
// pixels rather than a reimplementation.
func renderSnapshot(t *testing.T, snap model.Snapshot, scale int) model.Artifact {
	t.Helper()
	plan, e := raster.Compile(snap, scale)
	if e != nil {
		t.Fatalf("compile scale=%d: %v", scale, e)
	}
	img, e := plan.Render(context.Background())
	if e != nil {
		t.Fatalf("render scale=%d: %v", scale, e)
	}
	data, e := codec.Encode(img)
	if e != nil {
		t.Fatalf("encode: %v", e)
	}
	return model.Artifact{ID: "probe", State: "live", Digest: model.Digest(data), Width: plan.Width, Height: plan.Height, PNG: data}
}

// richDocument exercises shapes, gradients, opacity, nested clipping and an
// image layer so render consistency covers every paint path, not just a flat
// bitmap copy.
func richDocument(assetID string) model.Document {
	return model.Document{Width: 96, Height: 72, Background: "#F5F1E8", Layers: []model.Layer{
		{
			ID: "plate", Kind: "rectangle", Width: 70, Height: 50,
			Transform: model.Transform{X: 10, Y: 8, ScaleX: 1, ScaleY: 1, Degrees: 4},
			Opacity:   1,
			Fill: model.Paint{Kind: "linear", Color: "#145A70", EndColor: "#32A5A0",
				From: model.Point{X: 0, Y: 0}, To: model.Point{X: 70, Y: 50}},
		},
		{
			ID: "accent", Kind: "ellipse", Width: 40, Height: 40,
			Transform: model.Transform{X: 40, Y: 26, ScaleX: 1, ScaleY: 1},
			Opacity:   .85, Fill: model.Paint{Kind: "solid", Color: "#E87E4F"},
		},
		{
			ID: "inset", Kind: "group",
			Transform: model.Transform{X: 48, Y: 34, ScaleX: 1, ScaleY: 1, Degrees: -6},
			Opacity: .9, Clip: &model.Rect{Width: 36, Height: 28},
			Children: []model.Layer{
				{
					ID: "facet", Kind: "polygon", Width: 40, Height: 40,
					Transform:  model.Transform{ScaleX: 1, ScaleY: 1},
					Opacity:    .8, Fill: model.Paint{Kind: "solid", Color: "#FFF4D5"},
					Points: []model.Point{
						{X: 4, Y: 36}, {X: 20, Y: 2}, {X: 38, Y: 36},
					},
				},
				{
					ID: "stamp", Kind: "image", Width: 20, Height: 20, AssetID: assetID,
					Transform: model.Transform{X: 8, Y: 6, ScaleX: 1, ScaleY: 1}, Opacity: 1,
				},
			},
		},
	}}
}

// TestImport_RendersIdenticallyOnEmptyInstance is the end-to-end portability
// guarantee: a package exported from one instance and imported into a totally
// empty instance must produce byte-identical PNG output through the production
// render pipeline, at both supported scales.
func TestImport_RendersIdenticallyOnEmptyInstance(t *testing.T) {
	// Source instance: build scene + asset, freeze, export.
	srcStore := newStore(t)
	srcSvc := &service.Service{Store: srcStore}
	pngData := makePNG(t, 10, 10, color.NRGBA{R: 240, G: 40, B: 80, A: 255})
	asset := importAsset(t, srcSvc, "portable-tex", pngData)
	doc := richDocument(asset.ID)
	_, sourceSnap := seedSceneAndSnapshot(t, srcSvc, "rich-source", doc)
	pkg := exportPackage(t, srcSvc, sourceSnap.ID)

	// Destination instance starts completely empty.
	dstStore := newStore(t)
	dstSvc := &service.Service{Store: dstStore}
	if pre := dstStore.Read(); len(pre.Scenes)+len(pre.Assets)+len(pre.Snapshots) != 0 {
		t.Fatal("destination fixture is not an empty instance")
	}

	imported, e := dstSvc.Import("rich-dest", "ported", pkg)
	if e != nil {
		t.Fatalf("import into empty instance: %v", e)
	}
	if !reflect.DeepEqual(imported.Document, sourceSnap.Document) {
		t.Fatal("imported document differs from the snapshot document")
	}

	// Freeze the imported scene so its render goes through the exact same
	// snapshot path as the source (asset closure captured in-state).
	dstSnap, e := dstSvc.Freeze("rich-dest", imported.Revision)
	if e != nil {
		t.Fatalf("freeze imported scene: %v", e)
	}
	if dstSnap.Digest != sourceSnap.Digest {
		t.Fatalf("snapshot digest differs across instances: source=%s dest=%s", sourceSnap.Digest, dstSnap.Digest)
	}

	for _, scale := range []int{1, 2} {
		sourceArt := renderSnapshot(t, sourceSnap, scale)
		destArt := renderSnapshot(t, dstSnap, scale)

		if sourceArt.Width != destArt.Width || sourceArt.Height != destArt.Height {
			t.Fatalf("scale=%d dimensions differ: %dx%d vs %dx%d", scale,
				sourceArt.Width, sourceArt.Height, destArt.Width, destArt.Height)
		}
		// Byte-identical encoding is the strongest claim; the renderer is
		// deterministic and takes no instance-specific input.
		if sourceArt.Digest != destArt.Digest {
			cmp, ce := observe.Compare(sourceArt, destArt)
			t.Fatalf("scale=%d render diverged: digest source=%s dest=%s pixelCmp=%+v err=%v",
				scale, sourceArt.Digest, destArt.Digest, cmp, ce)
		}
		cmp, e := observe.Compare(sourceArt, destArt)
		if e != nil {
			t.Fatalf("pixel compare: %v", e)
		}
		if cmp.ChangedPixels != 0 || cmp.MaxDelta != 0 || cmp.MeanDelta != 0 {
			t.Fatalf("scale=%d unexpected pixel differences: %+v", scale, cmp)
		}
	}
}

// TestImport_RenderIsStableAfterFailedImports proves failures do not corrupt
// the renderability of either the pre-existing instance or a later successful
// import sharing the same store.
func TestImport_RenderIsStableAfterFailedImports(t *testing.T) {
	svc, pkg, asset := fixture(t)
	baseline := svc.Store.Read().Snapshots
	var sourceSnap model.Snapshot
	for _, s := range baseline {
		sourceSnap = s
	}
	beforeArt := renderSnapshot(t, sourceSnap, 1)

	// A chain of rejected imports: corrupt package, same-name collision,
	// same-asset different content, and a write-level failure is covered
	// separately in the persist package tests.
	bad := pkg
	bad.Digest = model.Digest([]byte("nope"))
	if _, e := svc.Import("source-scene", "collision", bad); e == nil {
		t.Fatal("corrupt import accepted")
	}
	if _, e := svc.Import("source-scene", "collision", pkg); e == nil {
		t.Fatal("same-name import accepted")
	}
	otherPNG := makePNG(t, 8, 8, color.NRGBA{R: 1, G: 2, B: 3, A: 255})
	st2 := newStore(t)
	svc2 := &service.Service{Store: st2}
	otherAsset := importAsset(t, svc2, asset.ID, otherPNG)
	_, otherSnap := seedSceneAndSnapshot(t, svc2, "other", buildDocument(otherAsset.ID))
	otherPkg := exportPackage(t, svc2, otherSnap.ID)
	if _, e := svc.Import("content-clash", "x", otherPkg); e == nil {
		t.Fatal("different-content same-id import accepted")
	}

	// Historical snapshot still renders to the identical PNG.
	afterSnap := svc.Store.Read().Snapshots[sourceSnap.ID]
	afterArt := renderSnapshot(t, afterSnap, 1)
	if afterArt.Digest != beforeArt.Digest {
		t.Fatal("historical render changed after failed imports")
	}

	// A good import afterwards still renders identically to the source package.
	imported, e := svc.Import("late-good", "fine", pkg)
	if e != nil {
		t.Fatalf("post-failure import: %v", e)
	}
	newSnap, e := svc.Freeze("late-good", imported.Revision)
	if e != nil {
		t.Fatalf("freeze after recovery: %v", e)
	}
	if renderSnapshot(t, newSnap, 1).Digest != beforeArt.Digest {
		t.Fatal("post-recovery render differs from source render")
	}
}

// TestImport_WorkerEndToEndRender runs an actual worker goroutine against the
// imported snapshot, covering job submission, claiming, execution and artifact
// commit on a fresh instance.
func TestImport_WorkerEndToEndRender(t *testing.T) {
	_, pkg, _ := fixture(t)
	dst := newStore(t)
	svc := &service.Service{Store: dst}

	imported, e := svc.Import("worker-scene", "worker", pkg)
	if e != nil {
		t.Fatalf("import: %v", e)
	}
	snap, e := svc.Freeze("worker-scene", imported.Revision)
	if e != nil {
		t.Fatalf("freeze: %v", e)
	}
	job, e := svc.Submit(snap.ID, 1, "e2e-render-key")
	if e != nil {
		t.Fatalf("submit: %v", e)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := newWorker(t, dst)
	go func() { worker.Run(ctx) }()

	deadline := deadlineChannel()
	for {
		select {
		case <-deadline:
			t.Fatal("worker did not finish imported render in time")
		default:
		}
		cur := dst.Read().Jobs[job.ID]
		if cur.State == "succeeded" {
			art, ok := dst.Read().Artifacts[cur.ArtifactID]
			if !ok {
				t.Fatal("succeeded job missing artifact")
			}
			if art.Digest == "" || len(art.PNG) == 0 {
				t.Fatal("artifact has no bytes")
			}
			if string(art.PNG[:8]) != pngSignature {
				t.Fatal("artifact is not a PNG")
			}
			return
		}
		if cur.State == "failed" || cur.State == "cancelled" {
			t.Fatalf("imported render ended in %s: %s", cur.State, cur.Error)
		}
		waitShort()
	}
}

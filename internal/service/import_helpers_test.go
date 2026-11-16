package service_test

import (
	"atlas-render/internal/bundle"
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"atlas-render/internal/service"
	"bytes"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"testing"
)

// newStore opens an isolated store in a fresh temporary directory. Each test
// gets its own directory so advisory locks never collide and the on-disk file
// can be inspected independently.
func newStore(t *testing.T) *persist.Store {
	t.Helper()
	st, e := persist.Open(filepath.Join(t.TempDir(), "state", "atlas-state.json"))
	if e != nil {
		t.Fatalf("open store: %v", e)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// makePNG encodes a solid-color RGBA PNG of the requested size. Colors vary
// per call so distinct assets have distinct bytes and digests.
func makePNG(t *testing.T, width, height int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	if e := png.Encode(&b, img); e != nil {
		t.Fatalf("encode png: %v", e)
	}
	return b.Bytes()
}

// importAsset persists a PNG asset through the production service path so its
// digest and dimensions are derived exactly as production code derives them.
func importAsset(t *testing.T, svc *service.Service, id string, pngData []byte) model.Asset {
	t.Helper()
	a, e := svc.ImportAsset(id, pngData)
	if e != nil {
		t.Fatalf("import asset %s: %v", id, e)
	}
	return a
}

// buildDocument returns a valid document whose single image layer references
// assetID. Keeping the fixture small keeps render work bounded.
func buildDocument(assetID string) model.Document {
	return model.Document{
		Width:      64,
		Height:     48,
		Background: "#203040",
		Layers: []model.Layer{{
			ID:        "plate",
			Kind:      "image",
			Width:     64,
			Height:    48,
			Opacity:   1,
			AssetID:   assetID,
			Transform: model.Transform{X: 0, Y: 0, ScaleX: 1, ScaleY: 1},
		}},
	}
}

// seedSceneAndSnapshot drives the real production flow (PUT scene -> freeze)
// and returns the frozen snapshot plus the scene.
func seedSceneAndSnapshot(t *testing.T, svc *service.Service, sceneID string, doc model.Document) (model.Scene, model.Snapshot) {
	t.Helper()
	sc, e := svc.PutScene(sceneID, "seed scene", 0, doc)
	if e != nil {
		t.Fatalf("put scene: %v", e)
	}
	snap, e := svc.Freeze(sceneID, sc.Revision)
	if e != nil {
		t.Fatalf("freeze: %v", e)
	}
	return sc, snap
}

// exportPackage pulls a package through the production export path.
func exportPackage(t *testing.T, svc *service.Service, snapshotID string) bundle.Package {
	t.Helper()
	p, e := svc.Export(snapshotID)
	if e != nil {
		t.Fatalf("export: %v", e)
	}
	return p
}

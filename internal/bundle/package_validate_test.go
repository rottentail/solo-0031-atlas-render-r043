package bundle_test

import (
	"atlas-render/internal/bundle"
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"atlas-render/internal/service"
	"bytes"
	"encoding/json"
	"errors"
	"image/color"
	"path/filepath"
	"testing"
)

func openStore(t *testing.T) *persist.Store {
	t.Helper()
	st, e := persist.Open(filepath.Join(t.TempDir(), "state", "atlas-state.json"))
	if e != nil {
		t.Fatalf("open store: %v", e)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func validPackage(t *testing.T) bundle.Package {
	t.Helper()
	st := openStore(t)
	svc := &service.Service{Store: st}
	var buf bytes.Buffer
	img := newSolidPNG(4, 4, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	if e := pngEncode(&buf, img); e != nil {
		t.Fatal(e)
	}
	asset, e := svc.ImportAsset("tex-main", buf.Bytes())
	if e != nil {
		t.Fatalf("asset: %v", e)
	}
	doc := model.Document{Width: 32, Height: 24, Background: "#101010", Layers: []model.Layer{{
		ID: "img-1", Kind: "image", Width: 32, Height: 24, Opacity: 1, AssetID: asset.ID,
		Transform: model.Transform{ScaleX: 1, ScaleY: 1},
	}}}
	if _, e = svc.PutScene("origin", "origin", 0, doc); e != nil {
		t.Fatalf("scene: %v", e)
	}
	snap, e := svc.Freeze("origin", 1)
	if e != nil {
		t.Fatalf("freeze: %v", e)
	}
	return bundle.Export(snap)
}

func TestValidate_AcceptsWellformedPackage(t *testing.T) {
	p := validPackage(t)
	if e := bundle.Validate(p); e != nil {
		t.Fatalf("valid package rejected: %v", e)
	}
}

func TestValidate_RejectsFormatAndVersion(t *testing.T) {
	base := validPackage(t)
	cases := map[string]func(*bundle.Package){
		"wrong format":  func(p *bundle.Package) { p.Format = "atlas-other" },
		"empty format":  func(p *bundle.Package) { p.Format = "" },
		"future major":  func(p *bundle.Package) { p.Version = 2 },
		"zero version":  func(p *bundle.Package) { p.Version = 0 },
		"negative ver.": func(p *bundle.Package) { p.Version = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := base
			mutate(&p)
			assertInvalid(t, bundle.Validate(p))
		})
	}
}

func TestValidate_RejectsOuterDigestTampering(t *testing.T) {
	p := validPackage(t)
	p.Digest = "deadbeef"
	assertInvalid(t, bundle.Validate(p))

	// A structurally valid digest string over different content must also fail.
	p2 := validPackage(t)
	p2.Digest = model.Digest([]byte("different content"))
	assertInvalid(t, bundle.Validate(p2))
}

func TestValidate_RejectsSnapshotDigestTampering(t *testing.T) {
	p := validPackage(t)
	p.Snapshot.Digest = "0000000000000000000000000000000000000000000000000000000000000000"
	// Outer digest no longer matches either, but the failure must still be a
	// deterministic rejection (never an importable package).
	assertInvalid(t, bundle.Validate(p))
}

func TestValidate_RejectsDocumentTampering(t *testing.T) {
	p := validPackage(t)
	p.Snapshot.Document.Width++
	assertInvalid(t, bundle.Validate(p))
}

func TestValidate_RejectsLayerGraphViolations(t *testing.T) {
	base := validPackage(t)
	t.Run("zero scale collapses layer", func(t *testing.T) {
		p := base
		p.Snapshot.Document.Layers[0].Transform.ScaleX = 0
		assertInvalid(t, bundle.Validate(p))
	})
	t.Run("duplicate layer ids", func(t *testing.T) {
		p := base
		l := p.Snapshot.Document.Layers[0]
		l.ID = "img-1"
		p.Snapshot.Document.Layers = append(p.Snapshot.Document.Layers, l)
		assertInvalid(t, bundle.Validate(p))
	})
	t.Run("canvas out of bounds", func(t *testing.T) {
		p := base
		p.Snapshot.Document.Width = 9999
		assertInvalid(t, bundle.Validate(p))
	})
	t.Run("nonfinite geometry", func(t *testing.T) {
		p := base
		p.Snapshot.Document.Layers[0].Transform.Degrees = 1e308
		assertInvalid(t, bundle.Validate(p))
	})
}

func TestValidate_RejectsAssetClosureMismatch(t *testing.T) {
	t.Run("missing referenced asset", func(t *testing.T) {
		p := validPackage(t)
		delete(p.Snapshot.Assets, "tex-main")
		assertInvalid(t, bundle.Validate(p))
	})
	t.Run("extra undeclared asset in closure", func(t *testing.T) {
		p := validPackage(t)
		extra := p.Snapshot.Assets["tex-main"]
		extra.ID = "tex-extra"
		p.Snapshot.Assets["tex-extra"] = extra
		assertInvalid(t, bundle.Validate(p))
	})
	t.Run("document references absent asset id", func(t *testing.T) {
		p := validPackage(t)
		p.Snapshot.Document.Layers[0].AssetID = "tex-ghost"
		assertInvalid(t, bundle.Validate(p))
	})
	t.Run("asset id key disagrees with record", func(t *testing.T) {
		p := validPackage(t)
		a := p.Snapshot.Assets["tex-main"]
		a.ID = "tex-renamed"
		p.Snapshot.Assets["tex-main"] = a
		assertInvalid(t, bundle.Validate(p))
	})
}

func TestValidate_RejectsAssetContentTampering(t *testing.T) {
	t.Run("png bytes flipped", func(t *testing.T) {
		p := validPackage(t)
		a := p.Snapshot.Assets["tex-main"]
		a.PNG = append(append([]byte(nil), a.PNG[:len(a.PNG)-2]...), 0xAA, 0x55)
		p.Snapshot.Assets["tex-main"] = a
		assertInvalid(t, bundle.Validate(p))
	})
	t.Run("digest field spoofed without bytes", func(t *testing.T) {
		p := validPackage(t)
		a := p.Snapshot.Assets["tex-main"]
		a.Digest = model.Digest([]byte("spoofed"))
		p.Snapshot.Assets["tex-main"] = a
		assertInvalid(t, bundle.Validate(p))
	})
	t.Run("dimensions disagree with png", func(t *testing.T) {
		p := validPackage(t)
		a := p.Snapshot.Assets["tex-main"]
		a.Width = 40
		p.Snapshot.Assets["tex-main"] = a
		assertInvalid(t, bundle.Validate(p))
	})
	t.Run("png payload is not an image", func(t *testing.T) {
		p := validPackage(t)
		a := p.Snapshot.Assets["tex-main"]
		a.PNG = []byte("definitely not a png")
		p.Snapshot.Assets["tex-main"] = a
		assertInvalid(t, bundle.Validate(p))
	})
	t.Run("oversized png rejected", func(t *testing.T) {
		p := validPackage(t)
		a := p.Snapshot.Assets["tex-main"]
		a.PNG = bytes.Repeat([]byte{1}, 2<<20+1)
		p.Snapshot.Assets["tex-main"] = a
		assertInvalid(t, bundle.Validate(p))
	})
}

func TestDecode_RejectsAdversarialEncoding(t *testing.T) {
	good := validPackage(t)
	raw, e := json.Marshal(good)
	if e != nil {
		t.Fatal(e)
	}

	t.Run("trailing second json value", func(t *testing.T) {
		if _, e = bundle.Decode(append(append([]byte(nil), raw...), []byte("\n{}\n")...)); e == nil {
			t.Fatal("trailing value accepted")
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		if _, e = bundle.Decode([]byte("{not json")); e == nil {
			t.Fatal("malformed json accepted")
		}
	})
	t.Run("oversized package rejected before parsing", func(t *testing.T) {
		blob := bytes.Repeat([]byte(" "), 12<<20+1)
		if _, e = bundle.Decode(blob); e == nil {
			t.Fatal("oversized package accepted")
		}
	})
	t.Run("empty body", func(t *testing.T) {
		if _, e = bundle.Decode(nil); e == nil {
			t.Fatal("empty package accepted")
		}
	})
}

func assertInvalid(t *testing.T, e error) {
	t.Helper()
	if e == nil {
		t.Fatal("expected rejection, package validated")
	}
	var de *model.Error
	if !errors.As(e, &de) {
		t.Fatalf("expected domain invalid error, got %T: %v", e, e)
	}
	if de.Status != 400 || de.Code != "invalid" {
		t.Fatalf("expected 400/invalid, got %d/%s: %v", de.Status, de.Code, e)
	}
}

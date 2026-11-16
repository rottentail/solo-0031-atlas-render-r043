package httpapi_test

import (
	"atlas-render/internal/bundle"
	"atlas-render/internal/engine"
	"atlas-render/internal/httpapi"
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"atlas-render/internal/service"
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func newServer(t *testing.T) (*httptest.Server, *service.Service, *persist.Store, bundle.Package) {
	t.Helper()
	st, e := persist.Open(filepath.Join(t.TempDir(), "state", "atlas-state.json"))
	if e != nil {
		t.Fatalf("open: %v", e)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := &service.Service{Store: st}
	worker := engine.New(st)
	if e = worker.Recover(); e != nil {
		t.Fatalf("recover: %v", e)
	}
	srv := httptest.NewServer(httpapi.API{Service: svc, Worker: worker}.Handler())
	t.Cleanup(srv.Close)

	// Build a valid package through the real HTTP surface.
	pngData := encodeSolidPNG(t, 5, 5, color.NRGBA{R: 90, G: 150, B: 210, A: 255})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/assets/web-tex", bytes.NewReader(pngData))
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatalf("put asset: %v", e)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("asset setup status = %d", resp.StatusCode)
	}
	doc := model.Document{Width: 40, Height: 30, Background: "#121212", Layers: []model.Layer{{
		ID: "img", Kind: "image", Width: 40, Height: 30, Opacity: 1, AssetID: "web-tex",
		Transform: model.Transform{ScaleX: 1, ScaleY: 1},
	}}}
	putBody, _ := json.Marshal(map[string]any{"name": "web source", "expectedRevision": 0, "document": doc})
	resp = postJSON(t, srv, http.MethodPut, "/v1/scenes/web-source", putBody, http.StatusOK)
	resp.Body.Close()
	resp = postJSON(t, srv, http.MethodPost, "/v1/scenes/web-source/snapshots",
		[]byte(`{"expectedRevision":1}`), http.StatusCreated)
	var snap model.Snapshot
	decodeBody(t, resp, &snap)
	resp.Body.Close()
	resp = postJSON(t, srv, http.MethodGet, "/v1/snapshots/"+snap.ID+"/package", nil, http.StatusOK)
	var pkg bundle.Package
	decodeBody(t, resp, &pkg)
	resp.Body.Close()
	return srv, svc, st, pkg
}

func postJSON(t *testing.T, srv *httptest.Server, method, path string, body []byte, want int) *http.Response {
	t.Helper()
	req, e := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	if e != nil {
		t.Fatalf("request: %v", e)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatalf("do: %v", e)
	}
	if resp.StatusCode != want {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		resp.Body.Close()
		t.Fatalf("%s %s: status = %d, want %d; body=%s", method, path, resp.StatusCode, want, buf.String())
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	if e := json.NewDecoder(resp.Body).Decode(dst); e != nil {
		t.Fatalf("decode response: %v", e)
	}
}

func encodeSolidPNG(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	if e := png.Encode(&b, img); e != nil {
		t.Fatalf("png: %v", e)
	}
	return b.Bytes()
}

// TestImportEndpoint_StrictJSON exercises the production HTTP decoder: unknown
// fields and trailing second JSON values are rejected with 400, and the
// rejection leaves no destination scene behind.
func TestImportEndpoint_StrictJSON(t *testing.T) {
	srv, _, _, pkg := newServer(t)
	valid, e := json.Marshal(map[string]any{"name": "valid dest", "package": pkg})
	if e != nil {
		t.Fatal(e)
	}

	t.Run("well formed request imports", func(t *testing.T) {
		resp := postJSON(t, srv, http.MethodPost, "/v1/imports/web-dest", valid, http.StatusCreated)
		resp.Body.Close()
	})

	t.Run("unknown top level field rejected", func(t *testing.T) {
		var probe map[string]any
		if e = json.Unmarshal(valid, &probe); e != nil {
			t.Fatal(e)
		}
		probe["intruder"] = true
		hostile, _ := json.Marshal(probe)
		resp := postJSON(t, srv, http.MethodPost, "/v1/imports/web-hostile", hostile, http.StatusBadRequest)
		assertCode(t, resp, "invalid")
		resp.Body.Close()
		assertAbsent(t, srv, "/v1/scenes/web-hostile")
	})

	t.Run("unknown nested package field rejected", func(t *testing.T) {
		var probe map[string]any
		if e = json.Unmarshal(valid, &probe); e != nil {
			t.Fatal(e)
		}
		pkgMap := probe["package"].(map[string]any)
		pkgMap["sneaky"] = 1
		hostile, _ := json.Marshal(probe)
		resp := postJSON(t, srv, http.MethodPost, "/v1/imports/web-sneaky", hostile, http.StatusBadRequest)
		assertCode(t, resp, "invalid")
		resp.Body.Close()
		assertAbsent(t, srv, "/v1/scenes/web-sneaky")
	})

	t.Run("trailing second value rejected", func(t *testing.T) {
		hostile := append(append([]byte(nil), valid...), []byte("\n{\"extra\":1}")...)
		resp := postJSON(t, srv, http.MethodPost, "/v1/imports/web-trailing", hostile, http.StatusBadRequest)
		assertCode(t, resp, "invalid")
		resp.Body.Close()
		assertAbsent(t, srv, "/v1/scenes/web-trailing")
	})

	t.Run("malformed json rejected", func(t *testing.T) {
		resp := postJSON(t, srv, http.MethodPost, "/v1/imports/web-broken", []byte("{nope"), http.StatusBadRequest)
		assertCode(t, resp, "invalid")
		resp.Body.Close()
	})
}

// TestImportEndpoint_CorruptPackageNoPartialLanding verifies end-to-end that a
// checksum-tampered package is rejected through HTTP with no scene created.
func TestImportEndpoint_CorruptPackageNoPartialLanding(t *testing.T) {
	srv, _, _, pkg := newServer(t)
	bad := pkg
	bad.Digest = model.Digest([]byte("tampered over the wire"))
	body, _ := json.Marshal(map[string]any{"name": "corrupt", "package": bad})
	resp := postJSON(t, srv, http.MethodPost, "/v1/imports/web-corrupt", body, http.StatusBadRequest)
	assertCode(t, resp, "invalid")
	resp.Body.Close()
	assertAbsent(t, srv, "/v1/scenes/web-corrupt")
}

// TestImportEndpoint_OversizedBody enforces the 12 MiB request ceiling at the
// HTTP boundary without invoking any domain logic.
func TestImportEndpoint_OversizedBody(t *testing.T) {
	srv, _, _, _ := newServer(t)
	blob := bytes.Repeat([]byte("a"), 12<<20+1024)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/imports/web-huge", bytes.NewReader(blob))
	req.Header.Set("Content-Type", "application/json")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatalf("do: %v", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want 400", resp.StatusCode)
	}
}

func assertCode(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	var body map[string]string
	if e := json.NewDecoder(resp.Body).Decode(&body); e != nil {
		t.Fatalf("decode error body: %v", e)
	}
	if body["code"] != want {
		t.Fatalf("error code = %q, want %q (error=%q)", body["code"], want, body["error"])
	}
}

func assertAbsent(t *testing.T, srv *httptest.Server, path string) {
	t.Helper()
	resp, e := http.Get(srv.URL + path)
	if e != nil {
		t.Fatalf("get %s: %v", path, e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("%s: status = %d, want 404 (failed request left state behind)", path, resp.StatusCode)
	}
}

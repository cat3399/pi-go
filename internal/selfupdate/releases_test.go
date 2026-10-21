package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cat3399/pi-go/internal/product"
)

type releaseFixture struct {
	server   *httptest.Server
	mu       sync.Mutex
	routes   map[string][]byte
	requests []string
}

func newReleaseFixture(t *testing.T) *releaseFixture {
	t.Helper()
	f := &releaseFixture{routes: map[string][]byte{}}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.RequestURI())
		body, ok := f.routes[r.URL.RequestURI()]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *releaseFixture) set(path string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = body
}

func (f *releaseFixture) setJSON(path string, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	f.set(path, body)
}

func (f *releaseFixture) publish(build product.BuildInfo, version string, archive []byte) release {
	name := assetName(build, version)
	base := "/download/v" + version + "/"
	f.set(base+name, archive)
	f.set(base+"SHA256SUMS", fmt.Appendf(nil, "%x  %s\n", sha256.Sum256(archive), name))
	r := release{Tag: "v" + version, Assets: []releaseAsset{
		{Name: name, URL: f.server.URL + base + name},
		{Name: "SHA256SUMS", URL: f.server.URL + base + "SHA256SUMS"},
	}}
	f.setJSON("/releases/tags/v"+version, r)
	return r
}

func (f *releaseFixture) updater(t *testing.T, build product.BuildInfo) *Updater {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pi-go")
	if err := os.WriteFile(path, []byte("installed fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Updater{Build: build, Executable: path, APIURL: f.server.URL, Client: f.server.Client()}
}

func testBuild() product.BuildInfo {
	return product.BuildInfo{Version: "0.1.0", Surface: "terminal", OS: runtime.GOOS, Arch: runtime.GOARCH, Distribution: "archive"}
}

func TestCheckSelectsCompleteSurfaceAcrossPagesWithoutDownloadingOrWriting(t *testing.T) {
	f := newReleaseFixture(t)
	web := testBuild()
	web.Surface = "web"
	terminal := testBuild()
	newerTerminal := f.publish(terminal, "0.15.0", []byte("terminal"))
	draft := f.publish(web, "0.99.0", []byte("draft"))
	draft.Draft = true
	prerelease := f.publish(web, "0.98.0", []byte("pre"))
	prerelease.Prerelease = true
	incomplete := f.publish(web, "0.14.0", []byte("partial"))
	f.set("/download/v0.14.0/SHA256SUMS", []byte("other surface not yet merged\n"))
	old := f.publish(web, "0.9.0", []byte("old"))
	matching := f.publish(web, "0.12.0", []byte("matching"))
	first := []release{newerTerminal, draft, prerelease, incomplete, old}
	for len(first) < 100 {
		first = append(first, release{Tag: "non-semver"})
	}
	f.setJSON("/releases?per_page=100&page=1", first)
	f.setJSON("/releases?per_page=100&page=2", []release{matching})
	u := f.updater(t, web)
	plan, err := u.Check(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TargetVersion != "0.12.0" || plan.Surface != "web" || plan.Asset != assetName(web, "0.12.0") || !plan.Available || !plan.CanUpdate {
		t.Fatalf("incorrect plan: %+v", plan)
	}
	entries, err := os.ReadDir(filepath.Dir(u.Executable))
	if err != nil || len(entries) != 1 {
		t.Fatalf("check wrote installation files: %v, %v", entries, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests {
		if strings.HasSuffix(request, ".zip") || strings.HasSuffix(request, ".tar.gz") {
			t.Fatalf("check downloaded an archive: %s", request)
		}
	}
}

func TestVersionSelectionAndInstallationOwnership(t *testing.T) {
	f := newReleaseFixture(t)
	r := f.publish(testBuild(), "0.2.0", []byte("archive"))
	f.setJSON("/releases?per_page=100&page=1", []release{r})
	for _, test := range []struct {
		name, current, distribution, requested, action string
		force, canUpdate                               bool
	}{
		{"upgrade", "0.1.0", "archive", "", "upgrade", false, true},
		{"current", "0.2.0", "archive", "", "none", false, true},
		{"reinstall", "0.2.0", "archive", "", "reinstall", true, true},
		{"no implicit downgrade", "0.3.0", "archive", "", "none", true, true},
		{"explicit downgrade", "0.3.0", "archive", "v0.2.0", "downgrade", false, true},
		{"dev release", "0.2.0-dev.abc", "archive", "", "upgrade", false, true},
		{"local source", "0.1.0", "source", "", "upgrade", false, false},
		{"explicit official release", "0.1.0", "source", "0.2.0", "upgrade", false, true},
		{"go install", "0.1.0", "go-install", "0.2.0", "upgrade", false, false},
		{"unknown", "custom", "archive", "", "none", false, false},
		{"explicit unknown", "custom", "source", "0.2.0", "upgrade", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			build := testBuild()
			build.Version, build.Distribution = test.current, test.distribution
			u := f.updater(t, build)
			plan, err := u.Check(context.Background(), Options{Version: test.requested, Force: test.force})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Action != test.action || plan.CanUpdate != test.canUpdate || plan.Available != (test.action != "none") {
				t.Fatalf("plan = %+v", plan)
			}
			if !plan.CanUpdate && plan.Reason == "" {
				t.Fatal("missing actionable explanation")
			}
		})
	}
}

func TestMissingOrInvalidReleaseFailsExplicitly(t *testing.T) {
	for _, test := range []struct {
		name, sums, expected string
	}{
		{"missing", "other asset\n", "missing from SHA256SUMS"},
		{"bad hash", "bad  %s\n", "invalid SHA-256"},
		{"bad record", "invalid hash  %s\n", "invalid checksum record"},
		{"duplicate", "%064d  %s\n%064d  %s\n", "duplicate checksum"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReleaseFixture(t)
			r := f.publish(testBuild(), "0.2.0", []byte("archive"))
			name := r.Assets[0].Name
			sums := test.sums
			switch test.name {
			case "bad hash", "bad record":
				sums = fmt.Sprintf(sums, name)
			case "duplicate":
				sums = fmt.Sprintf(sums, 0, name, 0, name)
			}
			f.set("/download/v0.2.0/SHA256SUMS", []byte(sums))
			u := f.updater(t, testBuild())
			if _, err := u.Check(context.Background(), Options{Version: "0.2.0"}); err == nil || !strings.Contains(err.Error(), test.expected) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	f := newReleaseFixture(t)
	web := testBuild()
	web.Surface = "web"
	r := f.publish(web, "0.2.0", []byte("web only"))
	f.setJSON("/releases?per_page=100&page=1", []release{r})
	u := f.updater(t, testBuild())
	if _, err := u.Check(context.Background(), Options{Version: "0.2.0"}); err == nil || !strings.Contains(err.Error(), "no complete") {
		t.Fatalf("missing surface: %v", err)
	}
}

func TestStableVersionOrdering(t *testing.T) {
	for _, invalid := range []string{"1", "1.2", "1.02.3", "01.2.3", "1.2.3-rc.1", "1.2.3+build", "1.2.3/other", "latest"} {
		if _, ok := stableVersion(invalid); ok {
			t.Errorf("accepted %q", invalid)
		}
	}
	for _, pair := range [][2]string{{"0.10.0", "0.9.9"}, {"2.0.0", "1.99.99"}, {"1.1.11", "1.1.9"}, {"999999999999999999999.0.0", "99999999999999999999.9.9"}} {
		if compareStable(pair[0], pair[1]) <= 0 {
			t.Errorf("%s should be newer than %s", pair[0], pair[1])
		}
	}
}

func TestHTTPSAndMetadataFailures(t *testing.T) {
	for _, address := range []string{"http://example.com/file", "https://user:pass@example.com/file", "file:///tmp/update", "https://example.com/file#fragment"} {
		if err := secureURL(address); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
	t.Run("insecure redirect", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://example.invalid/update", http.StatusFound)
		}))
		defer server.Close()
		u := &Updater{Client: server.Client()}
		if _, err := u.read(context.Background(), server.URL, 10); err == nil || !strings.Contains(err.Error(), "HTTPS") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("metadata limit", func(t *testing.T) {
		f := newReleaseFixture(t)
		f.set("/large", []byte("long response"))
		u := &Updater{Client: f.server.Client()}
		if _, err := u.read(context.Background(), f.server.URL+"/large", 3); err == nil {
			t.Fatal("oversized metadata accepted")
		}
	})
	t.Run("network failure is not up to date", func(t *testing.T) {
		f := newReleaseFixture(t)
		u := f.updater(t, testBuild())
		if _, err := u.Check(context.Background(), Options{}); err == nil {
			t.Fatal("missing API response accepted")
		}
	})
}

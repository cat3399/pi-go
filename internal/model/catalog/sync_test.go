package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

// These excerpts retain the published model objects, including a chat and
// classifier model with the same ID, plus Azure's two distinct API groups.
type syncFixture struct {
	Legacy  Document `json:"legacy"`
	Current struct {
		Sources   Sources                    `json:"sources"`
		Providers map[string]json.RawMessage `json:"providers"`
		Resolver  string                     `json:"resolver"`
	} `json:"current"`
}

func loadSyncFixture(t *testing.T) syncFixture {
	t.Helper()
	raw, err := os.ReadFile("../testdata/catalog-sync.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture syncFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestSyncPublishedCatalogWithRenamedAzureAndModelTypes(t *testing.T) {
	fixture := loadSyncFixture(t)
	files := map[string][]byte{}
	for id, raw := range fixture.Current.Providers {
		files[providerPath(id)] = raw
	}
	for _, version := range []string{"", "latest", fixture.Current.Sources.Models.Version} {
		t.Run("version="+version, func(t *testing.T) {
			client := syncFixtureClient(t, fixture.Current.Sources, files, fixture.Current.Resolver, version)
			updated, err := syncCatalog(context.Background(), client, fixture.Legacy, version)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Sources.Models.Version != fixture.Current.Sources.Models.Version || updated.Sources.Defaults.Version != updated.Sources.Models.Version {
				t.Fatalf("mixed release sources: %#v", updated.Sources)
			}
			for _, p := range updated.Providers {
				sourceID := p.ID
				if p.ID == legacyAzureProvider {
					sourceID = "azure"
					if p.API != legacyAzureProvider || p.BaseURL != "" || len(p.Models) != 1 {
						t.Fatalf("Azure scope changed: %#v", p)
					}
				}
				var original map[string]map[string]json.RawMessage
				if err := json.Unmarshal(fixture.Current.Providers[sourceID], &original); err != nil {
					t.Fatal(err)
				}
				count := 0
				for api, entries := range p.Models {
					for key, raw := range entries {
						count++
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(raw, &fields); err != nil {
							t.Fatal(err)
						}
						if string(fields["type"]) != `"chat"` {
							t.Fatalf("non-chat model exposed: %s", raw)
						}
						fields["provider"], _ = json.Marshal(sourceID)
						restored, err := json.Marshal(fields)
						if err != nil || !equalJSON(restored, original[api][key]) {
							t.Fatalf("upstream fields lost for %s/%s: %s", p.ID, key, raw)
						}
					}
				}
				if count != 1 {
					t.Fatalf("%s has %d chat models, want 1", p.ID, count)
				}
			}
			defaults, err := parseDefaults(fixture.Current.Resolver)
			if err != nil {
				t.Fatal(err)
			}
			for i := range defaults {
				if defaults[i].Provider == "azure" {
					defaults[i].Provider = legacyAzureProvider
				}
			}
			if !reflect.DeepEqual(updated.Defaults, defaults) {
				t.Fatalf("defaults lost identity or ordering: %#v", updated.Defaults)
			}
		})
	}
}

func TestSyncLegacyRelease(t *testing.T) {
	fixture := loadSyncFixture(t)
	files := map[string][]byte{}
	for _, p := range fixture.Legacy.Providers {
		raw, err := json.Marshal(p.Models)
		if err != nil {
			t.Fatal(err)
		}
		files[providerPath(p.ID)] = raw
	}
	var resolver strings.Builder
	resolver.WriteString("export const defaultModelPerProvider = {\n")
	for _, preference := range fixture.Legacy.Defaults {
		fmt.Fprintf(&resolver, "%q: %q,\n", preference.Provider, preference.ModelID)
	}
	resolver.WriteString("};")
	version := fixture.Legacy.Sources.Models.Version
	client := syncFixtureClient(t, fixture.Legacy.Sources, files, resolver.String(), version)
	updated, err := syncCatalog(context.Background(), client, fixture.Legacy, version)
	if err != nil {
		t.Fatal(err)
	}
	diff := Compare(fixture.Legacy, updated)
	if len(diff.Added)+len(diff.Removed)+len(diff.Changed)+len(diff.Defaults)+len(diff.Providers) != 0 {
		t.Fatalf("legacy release changed: %#v", diff)
	}
}

func TestSyncEmptyProviderCatalogCanRemoveAllModels(t *testing.T) {
	fixture := loadSyncFixture(t)
	files := map[string][]byte{}
	for id, raw := range fixture.Current.Providers {
		files[providerPath(id)] = raw
	}
	files[providerPath("openai")] = []byte(`{"openai-responses":{}}`)
	client := syncFixtureClient(t, fixture.Current.Sources, files, fixture.Current.Resolver, "latest")
	updated, err := syncCatalog(context.Background(), client, fixture.Legacy, "latest")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range updated.Providers {
		if p.ID == "openai" && (p.API != "openai-responses" || len(p.Models) != 1 || len(p.Models[p.API]) != 0) {
			t.Fatalf("empty provider catalog was not preserved: %#v", p)
		}
	}
	for _, removed := range Compare(fixture.Legacy, updated).Removed {
		if removed == "openai/gpt-5.4" {
			return
		}
	}
	t.Fatal("empty upstream catalog retained the previous model")
}

func TestSyncRejectsIncompleteOrInvalidRelease(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string][]byte)
		want   string
	}{
		{"missing provider", func(files map[string][]byte) { delete(files, providerPath("xai")) }, "missing " + providerPath("xai")},
		{"missing Azure", func(files map[string][]byte) { delete(files, providerPath("azure")) }, "missing " + providerPath(legacyAzureProvider)},
		{"malformed primary cannot fall back", func(files map[string][]byte) { files[providerPath(legacyAzureProvider)] = []byte("{") }, "decode upstream provider"},
		{"wrong renamed identity", func(files map[string][]byte) {
			files[providerPath("azure")] = bytes.ReplaceAll(files[providerPath("azure")], []byte(`"provider": "azure"`), []byte(`"provider": "other"`))
		}, "invalid model identity"},
		{"missing Responses group", func(files map[string][]byte) { files[providerPath("azure")] = []byte(`{"openai-completions":{}}`) }, "needs an explicit default API"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := loadSyncFixture(t)
			files := map[string][]byte{}
			for id, raw := range fixture.Current.Providers {
				files[providerPath(id)] = raw
			}
			test.change(files)
			client := syncFixtureClient(t, fixture.Current.Sources, files, fixture.Current.Resolver, "latest")
			updated, err := syncCatalog(context.Background(), client, fixture.Legacy, "latest")
			if err == nil || !strings.Contains(err.Error(), test.want) || len(updated.Providers) != 0 {
				t.Fatalf("invalid release returned %#v, %v; want %q", updated, err, test.want)
			}
		})
	}
}

type syncRoundTripper func(*http.Request) (*http.Response, error)

func (f syncRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func syncFixtureClient(t *testing.T, sources Sources, files map[string][]byte, resolver, requestedVersion string) *http.Client {
	t.Helper()
	if requestedVersion == "" {
		requestedVersion = "latest"
	}
	responses := map[string][]byte{}
	for i, source := range []Source{sources.Models, sources.Defaults} {
		packageFiles := files
		version := requestedVersion
		if i == 1 {
			packageFiles = map[string][]byte{"package/dist/core/model-resolver.js": []byte(resolver)}
			version = sources.Models.Version
		}
		var archive bytes.Buffer
		gz := gzip.NewWriter(&archive)
		tarWriter := tar.NewWriter(gz)
		for path, data := range packageFiles {
			if err := tarWriter.WriteHeader(&tar.Header{Name: path, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tarWriter.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		if err := tarWriter.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		digest := sha512.Sum512(archive.Bytes())
		metadata := packageMetadata{Name: source.Package, Version: source.Version, Commit: source.Commit}
		metadata.Dist.Tarball = "https://registry.npmjs.org/" + source.Package + "/package.tgz"
		metadata.Dist.Integrity = "sha512-" + base64.StdEncoding.EncodeToString(digest[:])
		raw, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		responses["/"+source.Package+"/"+version] = raw
		responses["/"+source.Package+"/package.tgz"] = archive.Bytes()
	}
	return &http.Client{Transport: syncRoundTripper(func(request *http.Request) (*http.Response, error) {
		raw, ok := responses[request.URL.Path]
		if !ok || request.URL.Host != "registry.npmjs.org" || request.Method != http.MethodGet {
			return nil, fmt.Errorf("unexpected upstream request: %s %s", request.Method, request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
	})}
}

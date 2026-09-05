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
	"net/url"
	"strconv"
	"strings"
	"text/scanner"
	"time"
)

type packageMetadata struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"gitHead"`
	Dist    struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

// Sync consumes published upstream data without executing JavaScript or
// reproducing pi-ai's model-generation rules. Provider scope comes from the
// shipped document; adding a provider still requires its Go implementation.
func Sync(ctx context.Context, baseline Document, version string) (Document, error) {
	if err := baseline.Validate(); err != nil {
		return Document{}, err
	}
	if version == "" {
		version = "latest"
	}
	client := &http.Client{Timeout: 90 * time.Second}
	paths := make([]string, 0, len(baseline.Providers))
	for _, p := range baseline.Providers {
		paths = append(paths, "package/dist/providers/data/"+p.ID+".json")
	}
	modelsSource, files, err := downloadPackage(ctx, client, baseline.Sources.Models.Package, version, paths)
	if err != nil {
		return Document{}, err
	}
	const resolverPath = "package/dist/core/model-resolver.js"
	defaultsSource, resolverFiles, err := downloadPackage(ctx, client, baseline.Sources.Defaults.Package, modelsSource.Version, []string{resolverPath})
	if err != nil {
		return Document{}, err
	}
	if modelsSource.Commit != "" && defaultsSource.Commit != "" && modelsSource.Commit != defaultsSource.Commit {
		return Document{}, fmt.Errorf("upstream models and defaults came from different commits")
	}
	defaults, err := parseDefaults(string(resolverFiles[resolverPath]))
	if err != nil {
		return Document{}, err
	}
	result := Document{SchemaVersion: SchemaVersion, Sources: Sources{Models: modelsSource, Defaults: defaultsSource}, Defaults: defaults}
	for _, previous := range baseline.Providers {
		p := Provider{ID: previous.ID, API: previous.API, BaseURL: previous.BaseURL}
		if err := json.Unmarshal(files["package/dist/providers/data/"+p.ID+".json"], &p.Models); err != nil {
			return Document{}, fmt.Errorf("decode upstream provider %s: %w", p.ID, err)
		}
		// A sole API is unambiguous (for example a provider moving from Chat
		// Completions to Responses). Multiple dialects retain the existing
		// default only when it is still present; never guess a new protocol.
		if len(p.Models) == 1 {
			for api := range p.Models {
				p.API = api
			}
		}
		if _, ok := p.Models[p.API]; !ok {
			return Document{}, fmt.Errorf("upstream provider %s needs an explicit default API", p.ID)
		}
		baseURLs := map[string]bool{}
		for _, entries := range p.Models {
			for _, raw := range entries {
				var wire struct {
					BaseURL string `json:"baseUrl"`
				}
				if err := json.Unmarshal(raw, &wire); err != nil {
					return Document{}, err
				}
				baseURLs[wire.BaseURL] = true
			}
		}
		if len(baseURLs) == 1 {
			for baseURL := range baseURLs {
				p.BaseURL = baseURL
			}
		} else {
			// No provider-wide URL may overwrite model-specific endpoints.
			p.BaseURL = ""
		}
		result.Providers = append(result.Providers, p)
	}
	return result, result.Validate()
}

func downloadPackage(ctx context.Context, client *http.Client, name, version string, paths []string) (Source, map[string][]byte, error) {
	metadataURL := "https://registry.npmjs.org/" + url.PathEscape(name) + "/" + url.PathEscape(version)
	raw, err := download(ctx, client, metadataURL, 2<<20)
	if err != nil {
		return Source{}, nil, err
	}
	var metadata packageMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return Source{}, nil, err
	}
	if metadata.Name != name || metadata.Version == "" || (version != "latest" && metadata.Version != version) {
		return Source{}, nil, fmt.Errorf("unexpected npm package identity for %s@%s", name, version)
	}
	archive, err := download(ctx, client, metadata.Dist.Tarball, 64<<20)
	if err != nil {
		return Source{}, nil, err
	}
	digest := sha512.Sum512(archive)
	want := "sha512-" + base64.StdEncoding.EncodeToString(digest[:])
	verified := false
	for _, integrity := range strings.Fields(metadata.Dist.Integrity) {
		if integrity == want {
			verified = true
		}
	}
	if !verified {
		return Source{}, nil, fmt.Errorf("npm archive integrity mismatch for %s@%s", name, metadata.Version)
	}
	files, err := readPackageFiles(archive, paths)
	return Source{Package: name, Version: metadata.Version, Commit: metadata.Commit, Integrity: want}, files, err
}

func download(ctx context.Context, client *http.Client, address string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid upstream package URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", address, response.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("upstream package response exceeds size limit")
	}
	return raw, nil
}

func readPackageFiles(archive []byte, paths []string) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	reader := tar.NewReader(io.LimitReader(gz, 256<<20))
	wanted := map[string]bool{}
	for _, path := range paths {
		wanted[path] = true
	}
	files := map[string][]byte{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if !wanted[header.Name] {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size > maxDocumentBytes {
			return nil, fmt.Errorf("invalid upstream package entry %s", header.Name)
		}
		if _, exists := files[header.Name]; exists {
			return nil, fmt.Errorf("duplicate upstream package entry %s", header.Name)
		}
		raw, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		files[header.Name] = raw
	}
	for _, path := range paths {
		if _, ok := files[path]; !ok {
			return nil, fmt.Errorf("upstream package is missing %s", path)
		}
	}
	return files, nil
}

// Upstream publishes defaults as a string-literal object in its resolver.
// Parse only that grammar, preserving property order. If upstream switches to
// computed defaults, fail explicitly instead of evaluating JS or inventing data.
func parseDefaults(source string) ([]Preference, error) {
	const declaration = "export const defaultModelPerProvider"
	index := strings.Index(source, declaration)
	if index < 0 {
		return nil, fmt.Errorf("upstream default-model declaration was not found")
	}
	var scan scanner.Scanner
	scan.Init(strings.NewReader(source[index+len(declaration):]))
	scan.Mode = scanner.ScanIdents | scanner.ScanStrings | scanner.ScanComments | scanner.SkipComments
	var scanError error
	scan.Error = func(_ *scanner.Scanner, message string) {
		scanError = fmt.Errorf("parse upstream default models: %s", message)
	}
	invalid := func() ([]Preference, error) {
		return nil, fmt.Errorf("upstream default-model format changed near %q", scan.TokenText())
	}
	if scan.Scan() != '=' || scan.Scan() != '{' {
		return invalid()
	}
	var result []Preference
	for token := scan.Scan(); token != '}'; token = scan.Scan() {
		key := scan.TokenText()
		if token == scanner.String {
			var err error
			key, err = strconv.Unquote(key)
			if err != nil {
				return invalid()
			}
		} else if token != scanner.Ident {
			return invalid()
		}
		if scan.Scan() != ':' || scan.Scan() != scanner.String {
			return invalid()
		}
		value, err := strconv.Unquote(scan.TokenText())
		if err != nil {
			return invalid()
		}
		result = append(result, Preference{Provider: key, ModelID: value})
		separator := scan.Scan()
		if separator == '}' {
			break
		}
		if separator != ',' {
			return invalid()
		}
	}
	if scan.Scan() != ';' || len(result) == 0 {
		return invalid()
	}
	return result, scanError
}

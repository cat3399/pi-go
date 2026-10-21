package selfupdate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type release struct {
	Tag        string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

type resolvedRelease struct {
	version  string
	asset    releaseAsset
	checksum [sha256.Size]byte
}

var errMissingChecksum = errors.New("asset is missing from SHA256SUMS")

func (u *Updater) resolve(ctx context.Context, requested string) (resolvedRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	api := strings.TrimSuffix(u.APIURL, "/")
	if api == "" {
		api = defaultAPIURL
	}
	var releases []release
	if requested != "" {
		version, _ := stableVersion(requested)
		body, err := u.read(ctx, api+"/releases/tags/v"+version, 8<<20)
		if err != nil {
			return resolvedRelease{}, err
		}
		var r release
		if err := json.Unmarshal(body, &r); err != nil {
			return resolvedRelease{}, fmt.Errorf("decode release: %w", err)
		}
		if r.Tag != "v"+version {
			return resolvedRelease{}, fmt.Errorf("release response does not match v%s", version)
		}
		releases = append(releases, r)
	} else {
		// GitHub orders by creation time, not SemVer. Enumerate every page so
		// late additions to older releases cannot change the selected version.
		for page := 1; ; page++ {
			body, err := u.read(ctx, fmt.Sprintf("%s/releases?per_page=100&page=%d", api, page), 8<<20)
			if err != nil {
				return resolvedRelease{}, err
			}
			var batch []release
			if err := json.Unmarshal(body, &batch); err != nil {
				return resolvedRelease{}, fmt.Errorf("decode releases: %w", err)
			}
			releases = append(releases, batch...)
			if len(batch) < 100 {
				break
			}
		}
	}
	eligible := make([]release, 0, len(releases))
	for _, r := range releases {
		v, ok := stableVersion(r.Tag)
		if ok && r.Tag == "v"+v && !r.Draft && !r.Prerelease {
			eligible = append(eligible, r)
		}
	}
	slices.SortFunc(eligible, func(a, b release) int {
		return -compareStable(strings.TrimPrefix(a.Tag, "v"), strings.TrimPrefix(b.Tag, "v"))
	})
	for _, r := range eligible {
		version := strings.TrimPrefix(r.Tag, "v")
		name := assetName(u.Build, version)
		asset, err := uniqueAsset(r.Assets, name)
		if err != nil {
			return resolvedRelease{}, err
		}
		sums, err := uniqueAsset(r.Assets, "SHA256SUMS")
		if err != nil {
			return resolvedRelease{}, err
		}
		if asset.URL == "" || sums.URL == "" {
			continue
		}
		if err := secureURL(asset.URL); err != nil {
			return resolvedRelease{}, err
		}
		body, err := u.read(ctx, sums.URL, 2<<20)
		if err != nil {
			return resolvedRelease{}, err
		}
		checksum, err := readChecksum(body, name)
		if errors.Is(err, errMissingChecksum) && requested == "" {
			// Assets and merged checksums are uploaded separately when a
			// surface is appended. That surface is not ready until both exist.
			continue
		}
		if err != nil {
			return resolvedRelease{}, fmt.Errorf("%s: %w", r.Tag, err)
		}
		return resolvedRelease{version: version, asset: asset, checksum: checksum}, nil
	}
	if requested != "" {
		return resolvedRelease{}, fmt.Errorf("release %s has no complete stable %s/%s/%s asset and SHA256SUMS", requested, u.Build.Surface, u.Build.OS, u.Build.Arch)
	}
	return resolvedRelease{}, fmt.Errorf("no complete stable release for %s/%s/%s", u.Build.Surface, u.Build.OS, u.Build.Arch)
}

func uniqueAsset(assets []releaseAsset, name string) (releaseAsset, error) {
	var found releaseAsset
	matches := 0
	for _, a := range assets {
		if a.Name == name {
			found = a
			matches++
		}
	}
	if matches > 1 {
		return releaseAsset{}, fmt.Errorf("duplicate release asset %s", name)
	}
	return found, nil
}

func readChecksum(body []byte, name string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	found := false
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || strings.TrimPrefix(fields[len(fields)-1], "*") != name {
			continue
		}
		if len(fields) != 2 {
			return result, fmt.Errorf("invalid checksum record for %s", name)
		}
		if found {
			return result, fmt.Errorf("duplicate checksum for %s", name)
		}
		hash, err := hex.DecodeString(fields[0])
		if err != nil || len(hash) != sha256.Size {
			return result, fmt.Errorf("invalid SHA-256 for %s", name)
		}
		copy(result[:], hash)
		found = true
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	if !found {
		return result, errMissingChecksum
	}
	return result, nil
}

func secureURL(address string) error {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("update URL must use HTTPS without credentials or fragments")
	}
	return nil
}

func (u *Updater) get(ctx context.Context, address string) (*http.Response, error) {
	if err := secureURL(address); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "pi-go/"+u.Build.Version)
	client := http.Client{Timeout: 5 * time.Minute}
	if u.Client != nil {
		client = *u.Client
	}
	originalRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := secureURL(req.URL.String()); err != nil {
			return err
		}
		if originalRedirect != nil {
			return originalRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("too many update redirects")
		}
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch update: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		if response.Header.Get("X-RateLimit-Remaining") == "0" {
			return nil, fmt.Errorf("GitHub API rate limit exceeded; try again later")
		}
		return nil, fmt.Errorf("fetch update: %s (%s)", response.Status, address)
	}
	return response, nil
}

func (u *Updater) read(ctx context.Context, address string, limit int64) ([]byte, error) {
	response, err := u.get(ctx, address)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("update metadata exceeds %d bytes", limit)
	}
	return body, nil
}

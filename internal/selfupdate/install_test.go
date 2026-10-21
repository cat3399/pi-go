package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/filelock"
)

// This fixture is a real executable with the same Go package identity and
// packaging layout as the CLI. It exercises OS image locks, build identity and
// version probes without requiring any provider, credentials or remote release.
func buildFixture(t *testing.T, version, surface string) []byte {
	t.Helper()
	root := t.TempDir()
	module := "module github.com/cat3399/pi-go/cmd/pi-go\n\ngo 1.25.8\n"
	if surface == "gui" {
		module = "module github.com/cat3399/pi-go\n\ngo 1.25.8\n"
	}
	source := "package main\nimport(\"bufio\";\"fmt\";\"os\")\nvar version string\n" +
		"func main(){if len(os.Args)>1 && os.Args[1]==\"hold\" {fmt.Println(\"ready\");bufio.NewReader(os.Stdin).ReadString('\\n');return};fmt.Println(version)}\n"
	sources := map[string]string{"go.mod": module, "main.go": source}
	sourceDir := root
	if surface == "gui" {
		source = strings.Replace(source, "import(", "import(\"github.com/cat3399/pi-go/internal/product\";", 1)
		source = strings.Replace(source, "fmt.Println(version)", "fmt.Println(product.Version)", 1)
		sources = map[string]string{"go.mod": module, "surface/gui/main.go": source, "internal/product/product.go": "package product\nvar Version string\n"}
		sourceDir = filepath.Join(root, "surface", "gui")
	}
	for name, body := range sources {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	name := filepath.Join(root, "fixture")
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	args := []string{"build", "-trimpath", "-ldflags", "-X=main.version=" + version + " -X=github.com/cat3399/pi-go/internal/product.Version=" + version, "-o", name}
	if surface == "web" {
		args = append(args, "-tags", "pi_go_webui")
	}
	args = append(args, ".")
	command := exec.Command("go", args...)
	command.Dir = sourceDir
	command.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	if surface == "gui" {
		actual, err := guiLinkedVersion(name, runtime.GOOS)
		if err != nil || actual != version {
			t.Fatalf("read GUI linker version: %q %v", actual, err)
		}
	}
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func archiveFixture(t *testing.T, body []byte, goos, name string) []byte {
	t.Helper()
	var output bytes.Buffer
	if goos == "windows" {
		archive := zip.NewWriter(&output)
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o755)
		writer, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(&output)
		archive := tar.NewWriter(gz)
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return output.Bytes()
}

func executableName() string {
	if runtime.GOOS == "windows" {
		return "pi-go.exe"
	}
	return "pi-go"
}

func assertVersion(t *testing.T, path, version string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != version {
		t.Fatalf("%s: version = %q, error = %v; want %s", path, out, err, version)
	}
}

func holdExecutable(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	command := exec.CommandContext(ctx, path, "hold")
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		_ = command.Wait()
		cancel()
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("start running executable: %q, %v", line, err)
	}
}

func TestUpdateRunningExecutablePreservesSurfaceAndSupportsReinstallAndDowngrade(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes portable fixtures")
	}
	for _, surface := range []string{"terminal", "web", "gui"} {
		t.Run(surface, func(t *testing.T) {
			oldBinary := buildFixture(t, "0.1.0", surface)
			newBinary := buildFixture(t, "0.2.0", surface)
			build := testBuild()
			build.Surface = surface
			f := newReleaseFixture(t)
			latest := f.publish(build, "0.2.0", archiveFixture(t, newBinary, build.OS, binaryName(surface, runtime.GOOS)))
			f.publish(build, "0.1.0", archiveFixture(t, oldBinary, build.OS, binaryName(surface, runtime.GOOS)))
			f.setJSON("/releases?per_page=100&page=1", []release{latest})
			deployment := t.TempDir()
			path := filepath.Join(deployment, binaryName(surface, runtime.GOOS))
			if err := os.WriteFile(path, oldBinary, 0o755); err != nil {
				t.Fatal(err)
			}
			holdExecutable(t, path)
			u := &Updater{Build: build, Executable: path, APIURL: f.server.URL, Client: f.server.Client()}
			// The symlink itself must survive updating its resolved target.
			link := filepath.Join(t.TempDir(), binaryName(surface, runtime.GOOS))
			if err := os.Symlink(path, link); err == nil {
				u.Executable = link
			}
			ctx := context.Background()
			plan, err := u.Check(ctx, Options{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := u.Apply(ctx, plan)
			if err != nil || !result.Updated || result.Backup == "" {
				t.Fatalf("update: %+v, %v", result, err)
			}
			assertVersion(t, path, "0.2.0")
			assertVersion(t, result.Backup, "0.1.0")
			if u.Executable == link {
				info, err := os.Lstat(link)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("symlink was replaced: %v", err)
				}
			}
			if _, err := u.Apply(ctx, plan); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("stale update plan accepted: %v", err)
			}
			u.Build.Version = "0.2.0"
			current, err := u.Check(ctx, Options{})
			if err != nil || current.Available {
				t.Fatalf("current release plan: %+v, %v", current, err)
			}
			noOp, err := u.Apply(ctx, current)
			if err != nil || noOp.Updated {
				t.Fatalf("current version was replaced: %+v, %v", noOp, err)
			}
			force, err := u.Check(ctx, Options{Force: true})
			if err != nil || force.Action != "reinstall" {
				t.Fatalf("reinstall plan: %+v, %v", force, err)
			}
			reinstalled, err := u.Apply(ctx, force)
			if err != nil || !reinstalled.Updated || reinstalled.Backup == result.Backup {
				t.Fatalf("reinstall: %+v, %v", reinstalled, err)
			}
			assertVersion(t, reinstalled.Backup, "0.2.0")
			downgrade, err := u.Check(ctx, Options{Version: "v0.1.0"})
			if err != nil || downgrade.Action != "downgrade" {
				t.Fatalf("downgrade plan: %+v, %v", downgrade, err)
			}
			reverted, err := u.Apply(ctx, downgrade)
			if err != nil || !reverted.Updated {
				t.Fatalf("downgrade: %+v, %v", reverted, err)
			}
			assertVersion(t, path, "0.1.0")
			assertVersion(t, reverted.Backup, "0.2.0")
		})
	}
}

func TestInvalidUpdatesPreserveInstalledExecutable(t *testing.T) {
	if testing.Short() {
		t.Skip("builds version and surface probe fixtures")
	}
	terminal := buildFixture(t, "0.2.0", "terminal")
	web := buildFixture(t, "0.2.0", "web")
	for _, test := range []struct {
		name, version, entry, expected string
		binary                         []byte
		corrupt                        bool
	}{
		{"checksum", "0.2.0", executableName(), "SHA-256 mismatch", terminal, true},
		{"wrong surface", "0.2.0", executableName(), "does not match", web, false},
		{"wrong version", "0.3.0", executableName(), "expected 0.3.0", terminal, false},
		{"not a program", "0.2.0", executableName(), "Go build identity", []byte("not executable"), false},
		{"traversal", "0.2.0", "../" + executableName(), "archive must contain", terminal, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReleaseFixture(t)
			archive := archiveFixture(t, test.binary, runtime.GOOS, test.entry)
			r := f.publish(testBuild(), test.version, archive)
			if test.corrupt {
				f.set("/download/v"+test.version+"/"+r.Assets[0].Name, []byte("corrupted download"))
			}
			u := f.updater(t, testBuild())
			original, _ := os.ReadFile(u.Executable)
			plan, err := u.Check(context.Background(), Options{Version: test.version})
			if err != nil {
				t.Fatal(err)
			}
			if result, err := u.Apply(context.Background(), plan); err == nil || !strings.Contains(err.Error(), test.expected) || result.Updated {
				t.Fatalf("result = %+v; error = %v", result, err)
			}
			after, err := os.ReadFile(u.Executable)
			if err != nil || !bytes.Equal(original, after) {
				t.Fatalf("failed update changed installation: %v", err)
			}
		})
	}
}

func TestReplaceFailureRestoresOriginal(t *testing.T) {
	dir := t.TempDir()
	target, next, backup := filepath.Join(dir, executableName()), filepath.Join(dir, "missing"), filepath.Join(dir, "previous")
	if err := os.WriteFile(target, []byte("original"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceExecutable(target, next, backup); err == nil {
		t.Fatal("expected replacement failure")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "original" {
		t.Fatalf("original not restored: %q, %v", data, err)
	}
}

func TestUpdateLockIsCancellable(t *testing.T) {
	f := newReleaseFixture(t)
	r := f.publish(testBuild(), "0.2.0", []byte("archive"))
	f.setJSON("/releases?per_page=100&page=1", []release{r})
	u := f.updater(t, testBuild())
	plan, err := u.Check(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := filelock.Acquire(context.Background(), plan.Executable+".update.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := u.Apply(ctx, plan); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock did not honor cancellation: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCancelledDownloadPreservesInstallation(t *testing.T) {
	f := newReleaseFixture(t)
	r := f.publish(testBuild(), "0.2.0", []byte("archive"))
	f.setJSON("/releases?per_page=100&page=1", []release{r})
	u := f.updater(t, testBuild())
	plan, err := u.Check(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	u.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := u.Apply(ctx, plan); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("download cancellation = %v", err)
	}
	if data, err := os.ReadFile(u.Executable); err != nil || string(data) != "installed fixture" {
		t.Fatalf("cancelled update changed installation: %q, %v", data, err)
	}
}

func TestBothArchiveFormatsRejectLinksAndDuplicateEntries(t *testing.T) {
	for _, format := range []string{"zip", "tar"} {
		for _, invalid := range []string{"symlink", "duplicate"} {
			t.Run(format+"/"+invalid, func(t *testing.T) {
				var data bytes.Buffer
				name := "pi-go"
				plan := Plan{OS: "linux", Asset: "update.tar.gz"}
				if format == "zip" {
					name, plan.OS, plan.Asset = "pi-go.exe", "windows", "update.zip"
					writer := zip.NewWriter(&data)
					header := &zip.FileHeader{Name: name}
					header.SetMode(0o755)
					if invalid == "symlink" {
						header.SetMode(os.ModeSymlink | 0o777)
					}
					entry, err := writer.CreateHeader(header)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = io.WriteString(entry, "outside")
					if invalid == "duplicate" {
						duplicate := *header
						entry, err = writer.CreateHeader(&duplicate)
						if err != nil {
							t.Fatal(err)
						}
						_, _ = io.WriteString(entry, "duplicate")
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					gz := gzip.NewWriter(&data)
					writer := tar.NewWriter(gz)
					header := &tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeReg, Size: 1}
					if invalid == "symlink" {
						header.Typeflag, header.Size, header.Linkname = tar.TypeSymlink, 0, "../outside"
					}
					if err := writer.WriteHeader(header); err != nil {
						t.Fatal(err)
					}
					if invalid == "duplicate" {
						_, _ = writer.Write([]byte("a"))
						if err := writer.WriteHeader(header); err != nil {
							t.Fatal(err)
						}
						_, _ = writer.Write([]byte("b"))
					}
					_ = writer.Close()
					_ = gz.Close()
				}
				dir := t.TempDir()
				archive := filepath.Join(dir, "download")
				if err := os.WriteFile(archive, data.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := extractExecutable(context.Background(), archive, filepath.Join(dir, name), plan); err == nil {
					t.Fatalf("accepted %s %s", invalid, format)
				}
			})
		}
	}
}

func TestVersionProbeOutputRemainsBoundedWhenCopied(t *testing.T) {
	var output boundedOutput
	input := io.LimitReader(strings.NewReader(strings.Repeat("x", 8192)), 8192)
	if _, err := io.Copy(&output, input); err == nil {
		t.Fatal("unbounded version output accepted")
	}
	if len(output.String()) > 4096 {
		t.Fatal("version output limit was bypassed through io.ReaderFrom")
	}
}

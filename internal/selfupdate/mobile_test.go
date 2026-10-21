package selfupdate

import (
	"context"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/product"
)

func TestMobileSelectsAPKAndOnlyExposesCheckedDownload(t *testing.T) {
	f := newReleaseFixture(t)
	build := product.BuildInfo{Version: "0.3.0", Surface: "mobile", OS: "android", Arch: "arm64", Distribution: "source"}
	mobile := f.publish(build, "0.2.0", []byte("APK fixture"))
	terminal := f.publish(testBuild(), "0.4.0", []byte("terminal fixture"))
	f.setJSON("/releases?per_page=100&page=1", []release{terminal, mobile})
	u := &Updater{Build: build, Client: f.server.Client(), APIURL: f.server.URL}
	c := NewController(u)
	ctx := context.Background()
	plan, err := c.Check(ctx, Options{})
	if err != nil || plan.TargetVersion != "0.2.0" || plan.Asset != "pi-go-mobile_0.2.0_android_arm64.apk" || plan.Executable != "" || !plan.CanUpdate {
		t.Fatalf("mobile plan: %+v %v", plan, err)
	}
	// Downloading remains available for local builds and when the installed
	// version is newer. It does not imply replacing or downgrading that app.
	url, err := c.DownloadURL(plan.ID)
	if err != nil || url != f.server.URL+"/download/v0.2.0/"+plan.Asset {
		t.Fatalf("download URL = %q, %v", url, err)
	}
	if _, err := c.Apply(ctx, plan.ID); err == nil {
		t.Fatal("mobile applied an executable update")
	}
	if c.Status().Updated {
		t.Fatal("download reported an installation")
	}
	if _, err := c.DownloadURL("unknown"); err == nil {
		t.Fatal("accepted unreviewed plan")
	}
	if _, err := c.Check(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DownloadURL(plan.ID); err == nil {
		t.Fatal("accepted expired plan")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests {
		if strings.HasSuffix(request, ".apk") {
			t.Fatal("host downloaded APK instead of handing its URL to the browser")
		}
	}
}

func TestReleaseTargetsFollowPublishedMatrix(t *testing.T) {
	for _, build := range []product.BuildInfo{
		{Surface: "gui", OS: "linux", Arch: "amd64"}, {Surface: "gui", OS: "darwin", Arch: "amd64"},
		{Surface: "gui", OS: "darwin", Arch: "arm64"}, {Surface: "gui", OS: "windows", Arch: "amd64"},
		{Surface: "mobile", OS: "android", Arch: "arm64"},
	} {
		if err := validateTarget(build); err != nil {
			t.Errorf("%+v: %v", build, err)
		}
	}
	for _, build := range []product.BuildInfo{
		{Surface: "gui", OS: "linux", Arch: "arm64"}, {Surface: "mobile", OS: "android", Arch: "amd64"},
		{Surface: "mobile", OS: "darwin", Arch: "arm64"},
	} {
		if err := validateTarget(build); err == nil {
			t.Errorf("accepted unsupported target %+v", build)
		}
	}
}

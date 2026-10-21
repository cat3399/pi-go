package product

import (
	"runtime/debug"
	"testing"
)

func TestCLISurfaceUsesExactBuildTag(t *testing.T) {
	for _, test := range []struct{ tags, surface string }{
		{"", "terminal"},
		{"production,pi_go_webui", "web"},
		{"pi_go_webui production", "web"},
		{"not_pi_go_webui", "terminal"},
	} {
		if got := CLISurface([]debug.BuildSetting{{Key: "-tags", Value: test.tags}}); got != test.surface {
			t.Errorf("tags %q: got %s, want %s", test.tags, got, test.surface)
		}
	}
}

func TestCLIInstallationIdentityDistinguishesSourceModuleVersions(t *testing.T) {
	for _, test := range []struct {
		name, distribution, moduleVersion, moduleSum, wantVersion, wantDistribution string
	}{
		{"local VCS build", "source", "v0.2.0-20260930073323-6faaada09e79+dirty", "", "0.1.0-dev", "source"},
		{"local tagged build", "source", "v0.2.0", "", "0.1.0-dev", "source"},
		{"downloaded Go module", "source", "v0.2.0", "h1:fixture-module", "0.2.0", "go-install"},
		{"release archive", "archive", "v0.2.0", "", "0.1.0-dev", "archive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			build := &debug.BuildInfo{Main: debug.Module{Version: test.moduleVersion, Sum: test.moduleSum}}
			info := cliBuildInfo("0.1.0-dev", test.distribution, build)
			if info.Version != test.wantVersion || info.Distribution != test.wantDistribution {
				t.Fatalf("build identity = %+v", info)
			}
		})
	}
}

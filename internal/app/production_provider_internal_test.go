package app

import (
	"testing"

	modelcatalog "github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestProductionProviderStreamOptionsMatchUpstreamDefaultsAndExplicitZeros(t *testing.T) {
	defaults, err := productionProviderStreamOptions(modelcatalog.Settings{}, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if defaults.SessionID != "session-1" || defaults.Transport != provider.TransportAuto || defaults.TimeoutMS == nil || *defaults.TimeoutMS != 300_000 ||
		defaults.WebsocketConnectTimeoutMS != nil || defaults.MaxRetries != nil ||
		defaults.MaxRetryDelayMS == nil || *defaults.MaxRetryDelayMS != 60_000 {
		t.Fatalf("default stream options = %#v", defaults)
	}
	zero := uint64(0)
	settings := modelcatalog.Settings{
		Transport:         provider.TransportSSE,
		HTTPIdleTimeoutMS: &zero, WebsocketConnectTimeoutMS: &zero,
		Retry: modelcatalog.RetrySettings{Provider: modelcatalog.ProviderRetrySettings{
			TimeoutMS: &zero, MaxRetries: &zero, MaxRetryDelayMS: &zero,
		}},
	}
	explicit, err := productionProviderStreamOptions(settings, "session-2")
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Transport != provider.TransportSSE || explicit.TimeoutMS == nil || *explicit.TimeoutMS != 0 ||
		explicit.WebsocketConnectTimeoutMS == nil || *explicit.WebsocketConnectTimeoutMS != 0 ||
		explicit.MaxRetries == nil || *explicit.MaxRetries != 0 ||
		explicit.MaxRetryDelayMS == nil || *explicit.MaxRetryDelayMS != 0 {
		t.Fatalf("explicit-zero stream options = %#v", explicit)
	}
	settings.Retry.Provider.TimeoutMS = nil
	disabledHTTP, err := productionProviderStreamOptions(settings, "")
	if err != nil {
		t.Fatal(err)
	}
	if disabledHTTP.TimeoutMS == nil || *disabledHTTP.TimeoutMS != uint64(1<<31-1) {
		t.Fatalf("disabled HTTP timeout = %#v", disabledHTTP.TimeoutMS)
	}
}

func TestProductionProviderEnvWhitelistsAndPrioritizesScopedRetention(t *testing.T) {
	got := productionProviderEnv(
		map[string]string{"PI_CACHE_RETENTION": "long", "AUTH_SECRET": "must-not-leak"},
		map[string]string{"PI_CACHE_RETENTION": "short", "AMBIENT_SECRET": "must-not-leak"},
	)
	if len(got) != 1 || got["PI_CACHE_RETENTION"] != "long" {
		t.Fatalf("provider env = %#v", got)
	}
	got = productionProviderEnv(nil, map[string]string{"PI_CACHE_RETENTION": "short", "AMBIENT_SECRET": "must-not-leak"})
	if len(got) != 1 || got["PI_CACHE_RETENTION"] != "short" {
		t.Fatalf("ambient provider env = %#v", got)
	}
	if got := productionProviderEnv(map[string]string{"AUTH_SECRET": "must-not-leak"}, map[string]string{"AMBIENT_SECRET": "must-not-leak"}); got != nil {
		t.Fatalf("unrelated env leaked = %#v", got)
	}
}

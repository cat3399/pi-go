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

func TestProductionProviderEnvPreservesCredentialScopeAndWhitelistsAdapterConfiguration(t *testing.T) {
	got := productionProviderEnv(
		map[string]string{
			"PI_CACHE_RETENTION": "long", "AZURE_OPENAI_API_VERSION": "scoped-version",
			"AUTH_SECRET": "credential-scoped",
		},
		map[string]string{
			"PI_CACHE_RETENTION": "short", "AZURE_OPENAI_API_VERSION": "ambient-version",
			"AZURE_OPENAI_BASE_URL":            "https://resource.openai.azure.com",
			"AZURE_OPENAI_RESOURCE_NAME":       "resource",
			"AZURE_OPENAI_DEPLOYMENT_NAME_MAP": "gpt-5.4=deployment",
			"AMBIENT_SECRET":                   "must-not-leak",
		},
	)
	if len(got) != 6 || got["PI_CACHE_RETENTION"] != "long" || got["AZURE_OPENAI_API_VERSION"] != "scoped-version" ||
		got["AZURE_OPENAI_BASE_URL"] != "https://resource.openai.azure.com" || got["AZURE_OPENAI_RESOURCE_NAME"] != "resource" ||
		got["AZURE_OPENAI_DEPLOYMENT_NAME_MAP"] != "gpt-5.4=deployment" || got["AUTH_SECRET"] != "credential-scoped" || got["AMBIENT_SECRET"] != "" {
		t.Fatalf("provider env = %#v", got)
	}
	got = productionProviderEnv(nil, map[string]string{"PI_CACHE_RETENTION": "short", "AMBIENT_SECRET": "must-not-leak"})
	if len(got) != 1 || got["PI_CACHE_RETENTION"] != "short" {
		t.Fatalf("ambient provider env = %#v", got)
	}
	if got := productionProviderEnv(map[string]string{"AUTH_SECRET": "credential-scoped"}, map[string]string{"AMBIENT_SECRET": "must-not-leak"}); len(got) != 1 || got["AUTH_SECRET"] != "credential-scoped" {
		t.Fatalf("credential-scoped env was not preserved: %#v", got)
	}
}

func TestAuthHeadersAuthorizeAzureOnlyWithAPIKeyHeader(t *testing.T) {
	if !authHeadersAuthorizeModel(modelcatalog.AzureOpenAIResponsesAPI, map[string]string{"API-Key": "configured"}) {
		t.Fatal("Azure api-key header was not recognized")
	}
	if authHeadersAuthorizeModel(modelcatalog.AzureOpenAIResponsesAPI, map[string]string{"Authorization": "Bearer wrong-dialect"}) {
		t.Fatal("Azure authorization header was accepted in place of api-key")
	}
}

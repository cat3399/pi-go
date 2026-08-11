package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/cat3399/pi-go/internal/application"
)

type oauthProviderWire struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	UsesCallbackServer bool   `json:"usesCallbackServer"`
	LoggedIn           bool   `json:"loggedIn"`
	SupportsAPIKey     bool   `json:"supportsApiKey"`
}

type apiKeyProviderWire struct {
	ID            string `json:"id"`
	DisplayName   string `json:"displayName"`
	Configured    bool   `json:"configured"`
	Source        string `json:"source,omitempty"`
	ModelCount    int    `json:"modelCount"`
	SupportsOAuth bool   `json:"supportsOAuth"`
}

func handleOAuthProviders(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		providers, err := api.ListModelProviders(request.Context(), api.DefaultCWD())
		if err != nil {
			writeApplicationError(writer, err)
			return
		}
		result := make([]oauthProviderWire, 0)
		for _, provider := range providers {
			if !provider.Builtin || !provider.SupportsOAuth {
				continue
			}
			name := provider.OAuthName
			if name == "" {
				name = provider.Name
			}
			result = append(result, oauthProviderWire{
				ID: provider.ID, Name: name, UsesCallbackServer: false,
				LoggedIn: provider.CredentialType == "oauth", SupportsAPIKey: provider.SupportsAPIKey,
			})
		}
		writeJSON(writer, http.StatusOK, map[string]any{"providers": result})
	}
}

func handleAPIKeyProviders(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		providers, err := api.ListModelProviders(request.Context(), api.DefaultCWD())
		if err != nil {
			writeApplicationError(writer, err)
			return
		}
		result := make([]apiKeyProviderWire, 0)
		for _, provider := range providers {
			if !provider.Builtin || !provider.SupportsAPIKey {
				continue
			}
			configured := provider.Configured && provider.CredentialType != "oauth"
			entry := apiKeyProviderWire{
				ID: provider.ID, DisplayName: provider.Name, Configured: configured,
				ModelCount: provider.ModelCount, SupportsOAuth: provider.SupportsOAuth,
			}
			if configured {
				entry.Source = provider.Source
			}
			result = append(result, entry)
		}
		writeJSON(writer, http.StatusOK, map[string]any{"providers": result})
	}
}

func handleAPIKeyStatus(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		providerID := strings.TrimSpace(request.PathValue("provider"))
		providers, err := api.ListModelProviders(request.Context(), api.DefaultCWD())
		if err != nil {
			writeApplicationError(writer, err)
			return
		}
		for _, provider := range providers {
			if provider.ID != providerID || !provider.SupportsAPIKey {
				continue
			}
			configured := provider.Configured && provider.CredentialType != "oauth"
			response := apiKeyProviderWire{
				ID: provider.ID, DisplayName: provider.Name, Configured: configured,
				ModelCount: provider.ModelCount, SupportsOAuth: provider.SupportsOAuth,
			}
			if configured {
				response.Source = provider.Source
			}
			writeJSON(writer, http.StatusOK, map[string]any{
				"provider": response.ID, "displayName": response.DisplayName,
				"configured": response.Configured, "source": response.Source, "models": response.ModelCount,
			})
			return
		}
		writeAPIError(writer, http.StatusNotFound, errors.New("unknown API-key provider"))
	}
}

func handleSetAPIKey(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var input struct {
			APIKey string `json:"apiKey"`
		}
		if err := json.Unmarshal(body, &input); err != nil || strings.TrimSpace(input.APIKey) == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("apiKey is required"))
			return
		}
		if err := api.SetProviderAPIKey(request.Context(), request.PathValue("provider"), input.APIKey); err != nil {
			writeModelConfigurationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"success": true})
	}
}

func handleDeleteAPIKey(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if err := api.DeleteProviderCredential(request.Context(), request.PathValue("provider"), "api_key"); err != nil {
			writeModelConfigurationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"success": true})
	}
}

func handleModelsConfigRead(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		document, err := api.ReadModelsConfig(request.Context())
		if err != nil {
			writeModelConfigurationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, document)
	}
}

func handleModelsConfigWrite(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var document application.ModelsConfigDocument
		if err := json.Unmarshal(body, &document); err != nil || document == nil {
			writeAPIError(writer, http.StatusBadRequest, errors.New("models configuration must be a JSON object"))
			return
		}
		if err := api.WriteModelsConfig(request.Context(), document); err != nil {
			writeModelConfigurationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"success": true})
	}
}

func handleModelDiscovery(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var input struct {
			ProviderName string                         `json:"providerName"`
			Provider     application.ModelProviderDraft `json:"provider"`
		}
		if err := json.Unmarshal(body, &input); err != nil {
			writeAPIError(writer, http.StatusBadRequest, errors.New("providerName and provider are required"))
			return
		}
		result, err := api.DiscoverModels(request.Context(), input.ProviderName, input.Provider)
		if err != nil {
			var upstream *application.ModelDiscoveryUpstreamError
			if errors.As(err, &upstream) {
				status := http.StatusBadGateway
				if upstream.Timeout {
					status = http.StatusGatewayTimeout
				}
				writeJSON(writer, status, map[string]any{"error": err.Error(), "status": upstream.Status})
				return
			}
			writeAPIError(writer, http.StatusBadRequest, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"models": result.Models, "endpoint": result.Endpoint})
	}
}

func handleModelCatalog(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		limit := 50
		if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil {
				limit = parsed
			}
		}
		result, err := api.QueryModelCatalog(
			request.Context(), query.Get("q"), query.Get("provider"), query.Get("baseUrl"), limit,
		)
		if err != nil {
			writeAPIError(writer, http.StatusBadGateway, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"models": result.Models, "recommendation": result.Recommendation, "source": result.Source,
		})
	}
}

func handleModelProbe(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var input map[string]json.RawMessage
		if err := json.Unmarshal(body, &input); err != nil || input == nil {
			writeAPIError(writer, http.StatusBadRequest, errors.New("providerName, provider, and model are required"))
			return
		}
		var providerName string
		var providerObject, modelObject map[string]json.RawMessage
		if json.Unmarshal(input["providerName"], &providerName) != nil || strings.TrimSpace(providerName) == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("providerName is required"))
			return
		}
		if json.Unmarshal(input["provider"], &providerObject) != nil || providerObject == nil {
			writeAPIError(writer, http.StatusBadRequest, errors.New("provider is required"))
			return
		}
		if json.Unmarshal(input["model"], &modelObject) != nil || modelObject == nil {
			writeAPIError(writer, http.StatusBadRequest, errors.New("model is required"))
			return
		}
		result, probeErr := api.TestModel(request.Context(), providerName, input["provider"], input["model"])
		response := map[string]any{"ok": probeErr == nil}
		if result.LatencyMS > 0 {
			response["latencyMs"] = result.LatencyMS
		}
		if result.Status > 0 {
			response["status"] = result.Status
		}
		if result.ResponseText != "" {
			response["responseText"] = result.ResponseText
		}
		if probeErr != nil {
			response["error"] = probeErr.Error()
		}
		writeJSON(writer, http.StatusOK, response)
	}
}

func writeModelConfigurationError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, application.ErrCredentialTypeMismatch):
		writeAPIError(writer, http.StatusConflict, err)
	case errors.Is(err, application.ErrUnknownModelProvider),
		errors.Is(err, application.ErrProviderAuthUnsupported),
		errors.Is(err, application.ErrInvalidModelsConfig):
		writeAPIError(writer, http.StatusBadRequest, err)
	default:
		writeApplicationError(writer, err)
	}
}

package web

import (
	"encoding/json"
	"net/http"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/surfacewire"
)

func handleModelProviders(api application.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := surfacewire.ModelProviders(r.Context(), api, r.URL.Query().Get("cwd"))
		if err != nil {
			writeModelConfigurationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func handleProviderModels(api application.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := surfacewire.ProviderModels(r.Context(), api, r.URL.Query().Get("cwd"), r.PathValue("provider"))
		if err != nil {
			writeModelConfigurationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func handleBuiltinProviderUpdate(api application.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readRequestBody(w, r)
		if err != nil {
			writeRequestBodyError(w, err)
			return
		}
		var input struct {
			Enabled *bool `json:"enabled"`
			Only    bool  `json:"only"`
		}
		if json.Unmarshal(body, &input) != nil || input.Enabled == nil {
			writeModelConfigurationError(w, application.ErrInvalidModelsConfig)
			return
		}
		err = api.SetBuiltinProviderEnabled(r.Context(), r.PathValue("provider"), application.BuiltinProviderUpdate{Enabled: *input.Enabled, Only: input.Only})
		if err != nil {
			writeModelConfigurationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func handleModelProviderWrite(api application.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Method != http.MethodDelete {
			var err error
			body, err = readRequestBody(w, r)
			if err != nil {
				writeRequestBodyError(w, err)
				return
			}
		}
		if err := api.WriteModelProvider(r.Context(), r.PathValue("provider"), body); err != nil {
			writeModelConfigurationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

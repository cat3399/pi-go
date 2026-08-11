package model

// IsBuiltinProvider reports whether id belongs to the production provider
// registry shipped with pi-go. Web configuration surfaces use this to keep
// custom models.json providers in the custom-provider editor instead of
// rendering the same provider a second time in the API-key picker.
func IsBuiltinProvider(id string) bool {
	for _, candidate := range builtinProviderConfigs() {
		if candidate.ID == id {
			return true
		}
	}
	return false
}

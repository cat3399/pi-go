package app

// openAIModelConfig is the narrow adapter input consumed by the existing auth
// resolver. Its value is now projected by internal/model; this file remains so
// the auth boundary does not absorb catalog ownership during the migration.
type openAIModelConfig struct {
	apiKey  *string
	baseURL string
}

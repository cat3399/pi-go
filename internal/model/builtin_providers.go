package model

// builtinProviderConfigs is the isolated registration table for built-in
// Provider metadata and ambient API-key discovery. Provider migrations add an
// entry here plus catalog data; Runtime and Agent wiring remain unchanged.
func builtinProviderConfigs() []ProviderConfig {
	return []ProviderConfig{
		{
			ID: OpenAIProviderID, Name: "OpenAI", API: OpenAIResponsesAPI,
			BaseURL: defaultOpenAIBaseURL, APIKeyEnvironment: []string{"OPENAI_API_KEY"},
		},
		{
			ID: OpenAICodexProviderID, Name: "OpenAI Codex", API: OpenAICodexResponsesAPI,
			BaseURL: defaultOpenAICodexBaseURL,
		},
		{
			ID: AnthropicProviderID, Name: "Anthropic", API: AnthropicMessagesAPI,
			BaseURL:           defaultAnthropicBaseURL,
			APIKeyEnvironment: []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY"},
		},
		{
			ID: "deepseek", Name: "DeepSeek", API: OpenAICompletionsAPI,
			BaseURL: "https://api.deepseek.com", APIKeyEnvironment: []string{"DEEPSEEK_API_KEY"},
		},
		{
			ID: "xai", Name: "xAI", API: OpenAICompletionsAPI,
			BaseURL: "https://api.x.ai/v1", APIKeyEnvironment: []string{"XAI_API_KEY"},
		},
		{
			ID: "groq", Name: "Groq", API: OpenAICompletionsAPI,
			BaseURL: "https://api.groq.com/openai/v1", APIKeyEnvironment: []string{"GROQ_API_KEY"},
		},
		{
			ID: "cerebras", Name: "Cerebras", API: OpenAICompletionsAPI,
			BaseURL: "https://api.cerebras.ai/v1", APIKeyEnvironment: []string{"CEREBRAS_API_KEY"},
		},
		{
			ID: "together", Name: "Together", API: OpenAICompletionsAPI,
			BaseURL: "https://api.together.ai/v1", APIKeyEnvironment: []string{"TOGETHER_API_KEY"},
		},
	}
}

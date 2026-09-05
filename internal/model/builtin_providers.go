package model

// builtinProviderRegistrations binds supported providers to authentication
// behavior. Model metadata, default APIs and endpoints live in catalog data.
func builtinProviderRegistrations() []ProviderConfig {
	return []ProviderConfig{
		{
			ID: OpenAIProviderID, Name: "OpenAI", APIKeyEnvironment: []string{"OPENAI_API_KEY"},
		},
		{
			ID: AzureOpenAIProviderID, Name: "Azure OpenAI",
			APIKeyEnvironment: []string{"AZURE_OPENAI_API_KEY"},
		},
		{
			ID: OpenAICodexProviderID, Name: "OpenAI Codex",
		},
		{
			ID: AnthropicProviderID, Name: "Anthropic",
			APIKeyEnvironment: []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY"},
		},
		{
			ID: "deepseek", Name: "DeepSeek", APIKeyEnvironment: []string{"DEEPSEEK_API_KEY"},
		},
		{
			ID: "xai", Name: "xAI", APIKeyEnvironment: []string{"XAI_API_KEY"},
		},
		{
			ID: "groq", Name: "Groq", APIKeyEnvironment: []string{"GROQ_API_KEY"},
		},
		{
			ID: "cerebras", Name: "Cerebras", APIKeyEnvironment: []string{"CEREBRAS_API_KEY"},
		},
		{
			ID: "together", Name: "Together", APIKeyEnvironment: []string{"TOGETHER_API_KEY"},
		},
	}
}

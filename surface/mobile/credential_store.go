package main

// remoteCredentialStore keeps bearer tokens outside the WebView. Tokens are
// keyed by the normalized endpoint so credentials can never be reused for a
// different server, and the design naturally supports more than one endpoint.
type remoteCredentialStore interface {
	Load(endpoint string) (string, error)
	Save(endpoint, token string) error
	Delete(endpoint string) error
}

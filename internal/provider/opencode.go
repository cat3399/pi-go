package provider

import (
	"net/url"
	"strings"

	"github.com/cat3399/pi-go/internal/product"
)

// OpenCode routes a conversation consistently using its session header, even
// when the caller has disabled prompt caching.
func openCodeHeaders(model Model, sessionID string) map[string]string {
	base, _ := url.Parse(model.BaseURL())
	if model.Provider() != "opencode" && model.Provider() != "opencode-go" &&
		(base == nil || !strings.EqualFold(base.Hostname(), "opencode.ai")) {
		return nil
	}
	headers := map[string]string{"User-Agent": "pi-go/" + product.Version, "x-opencode-client": "pi-go"}
	if sessionID != "" {
		headers["x-opencode-session"] = sessionID
	}
	return headers
}

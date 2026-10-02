package httpapp

import (
	"fmt"
	"strings"
)

// openClawBootstrap is returned only at connector creation/rotation time.
// Treat Config as secret-bearing output because it contains the relay secret.
type openClawBootstrap struct {
	InstallCommand string
	Config         string
}

func buildOpenClawBootstrap(baseURL, gatewayID, secret, deliveryKey string) openClawBootstrap {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")

	config := fmt.Sprintf(`channels: {
  mailmoose: {
    enabled: true,
    baseUrl: %q,
    gatewayId: %q,
    secret: %q,
    deliveryKey: %q
  }
}
`, baseURL, gatewayID, secret, deliveryKey)

	return openClawBootstrap{
		InstallCommand: "openclaw plugins install clawhub:mailmoose",
		Config:         config,
	}
}

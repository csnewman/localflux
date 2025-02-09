package cluster

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

const fluxInstallManifests = "https://github.com/fluxcd/flux2/releases/latest/download/install.yaml"

func FetchFluxManifests(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fluxInstallManifests, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create http request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute http request: %w", err)
	}

	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	return string(raw), nil
}

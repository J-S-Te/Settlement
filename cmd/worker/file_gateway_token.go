package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func requestFileGatewayToken(ctx context.Context, client *http.Client, endpoint, clientID, secret, scope string) (string, error) {
	if client == nil || strings.TrimSpace(clientID) == "" || secret == "" {
		return "", errors.New("file gateway OAuth configuration incomplete")
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {scope}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("file gateway OAuth endpoint invalid")
	}
	// Business machine clients require Basic; form secrets are broker-only.
	request.SetBasicAuth(clientID, secret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("file gateway OAuth transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return "", fmt.Errorf("file gateway OAuth returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil || strings.TrimSpace(payload.AccessToken) == "" {
		return "", errors.New("file gateway OAuth token response invalid")
	}
	return payload.AccessToken, nil
}

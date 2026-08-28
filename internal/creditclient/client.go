package creditclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/oauth2/clientcredentials"
)

type Client struct {
	endpoint string
	client   *http.Client
}

func New(baseURL, tokenURL, clientID, clientSecret, scope string, httpClient *http.Client) (*Client, error) {
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("CRM credit endpoint must be HTTP(S)")
	}
	if tokenURL == "" || clientID == "" || clientSecret == "" || scope == "" {
		return nil, fmt.Errorf("CRM credit OAuth configuration is incomplete")
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	tokenConfig := &clientcredentials.Config{ClientID: clientID, ClientSecret: clientSecret, TokenURL: tokenURL, Scopes: []string{scope}}
	tokenClient := tokenConfig.Client(context.Background())
	return &Client{endpoint: strings.TrimRight(baseURL, "/"), client: tokenClient}, nil
}

func (c *Client) Publish(ctx context.Context, tenant, eventID string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", eventID)
	req.Header.Set("X-Tenant-ID", tenant)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("CRM credit endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

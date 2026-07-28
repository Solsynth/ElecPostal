package solar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a thin HTTP client for the Solar Network REST API.
type Client struct {
	baseURL     string
	accessToken string
	httpClient  *http.Client
}

// NewClient creates a Solar Network client.
func NewClient(baseURL, accessToken string) *Client {
	return &Client{
		baseURL:     strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		accessToken: strings.TrimSpace(accessToken),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Enabled returns true when the client is configured with a base URL.
func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != ""
}

// GetAccount resolves a Solar Network account by ID.
func (c *Client) GetAccount(ctx context.Context, accountID string) (*Account, error) {
	var out Account
	if err := c.doJSON(ctx, http.MethodGet, "/passport/accounts/"+url.PathEscape(strings.TrimSpace(accountID)), nil, nil, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.ID) == "" {
		return nil, fmt.Errorf("solar account lookup for %q returned empty id", accountID)
	}
	return &out, nil
}

// ResolveAccountByName resolves a Solar Network account by username.
func (c *Client) ResolveAccountByName(ctx context.Context, accountName string) (*Account, error) {
	var out Account
	if err := c.doJSON(ctx, http.MethodGet, "/passport/accounts/"+url.PathEscape(strings.TrimSpace(accountName)), nil, nil, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.ID) == "" {
		return nil, fmt.Errorf("solar account lookup for %q returned empty id", accountName)
	}
	return &out, nil
}

// SendDirectMessage opens or reuses a DM room and sends a message.
func (c *Client) SendDirectMessage(ctx context.Context, targetAccountID, content string) (*ChatMessage, error) {
	body := map[string]any{
		"related_user_id": strings.TrimSpace(targetAccountID),
		"encryption_mode": 0,
	}
	var room ChatRoom
	if err := c.doJSON(ctx, http.MethodPost, "/messager/chat/direct", nil, body, &room); err != nil {
		return nil, err
	}
	if strings.TrimSpace(room.ID) == "" {
		return nil, fmt.Errorf("solar direct message creation returned empty room id")
	}

	msgBody := map[string]any{"content": strings.TrimSpace(content)}
	var out ChatMessage
	path := fmt.Sprintf("/messager/chat/%s/messages", url.PathEscape(room.ID))
	if err := c.doJSON(ctx, http.MethodPost, path, nil, msgBody, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	requestURL := c.baseURL + path
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}

	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("solar %s %s failed with status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil || len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode solar %s %s response: %w", method, path, err)
	}
	return nil
}

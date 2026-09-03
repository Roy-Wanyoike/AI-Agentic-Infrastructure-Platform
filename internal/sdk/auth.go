package sdk

import (
	"context"
	"fmt"
)

// Login exchanges email+password for the HMAC bearer token issued by
// POST /v1/auth/login ({"token":"…"}).
func (c *Client) Login(ctx context.Context, email, password string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	body := map[string]string{"email": email, "password": password}
	if err := c.do(ctx, httpMethodPost, "/auth/login", nil, body, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("login response carried no token")
	}
	return out.Token, nil
}

// RegisterResult is the 201 body of POST /v1/auth/register. The handler emits
// the nested objects from map literals, so the nested keys are the Go field
// names (ID/Name) — mirrored here exactly.
type RegisterResult struct {
	Organization struct {
		ID   string `json:"ID"`
		Name string `json:"Name"`
	} `json:"organization"`
	User struct {
		ID           string `json:"ID"`
		Organization string `json:"Organization"`
		Email        string `json:"Email"`
		Role         string `json:"Role"`
	} `json:"user"`
}

// Register creates a new organization + owner user (POST /v1/auth/register).
func (c *Client) Register(ctx context.Context, organization, email, password string) (*RegisterResult, error) {
	body := map[string]string{
		"organization": organization,
		"email":        email,
		"password":     password,
	}
	var out RegisterResult
	if err := c.do(ctx, httpMethodPost, "/auth/register", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

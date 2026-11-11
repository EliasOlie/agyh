package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/hook"
)

// Client is what hooks use to talk to a running bridge.
type Client struct {
	base, secret string
	http         *http.Client
}

func NewClient(cfg *config.Config) *Client {
	return &Client{base: "http://" + cfg.Bridge.Addr, secret: cfg.Bridge.Secret, http: &http.Client{}}
}

// Available reports whether a bridge answers on the configured address.
func (c *Client) Available(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	return c.do(ctx, http.MethodGet, "/v1/health", nil, nil) == nil
}

// Ask blocks until the bridge has a decision for the request (or ctx ends).
func (c *Client) Ask(ctx context.Context, id string, req hook.Request) (ApprovalResponse, error) {
	var out ApprovalResponse
	err := c.do(ctx, http.MethodPost, "/v1/approvals", ApprovalRequest{ID: id, Request: req}, &out)
	return out, err
}

// Decide reports a decision taken elsewhere (the terminal) so phones get updated.
func (c *Client) Decide(ctx context.Context, id, verdict, by string, auto bool) error {
	return c.do(ctx, http.MethodPost, "/v1/approvals/"+id+"/decision",
		map[string]any{"verdict": verdict, "by": by, "auto": auto}, nil)
}

// Notify sends a message to every connected channel.
func (c *Client) Notify(ctx context.Context, text string) error {
	return c.do(ctx, http.MethodPost, "/v1/notify", map[string]string{"text": text}, nil)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

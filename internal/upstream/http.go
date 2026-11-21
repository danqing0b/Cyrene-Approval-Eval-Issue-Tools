package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client holds an operator-selected origin and credential. It exposes no
// request-controlled URLs and never forwards credentials through redirects.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

func New(rawURL, token string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("expected an HTTPS URL without credentials, query, or fragment")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())) {
		return nil, errors.New("plain HTTP is available only on loopback")
	}
	if strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("invalid service credential")
	}
	return &Client{base: u, token: token, http: &http.Client{
		Timeout:       12 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) JSON(ctx context.Context, method, path string, query url.Values, body io.Reader, out any, maxBytes int64) error {
	u := c.base.JoinPath(path)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return errors.New("could not build upstream request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if u.Hostname() == "api.github.com" {
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("upstream %s request failed", u.Hostname())
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream %s returned HTTP %d", u.Hostname(), res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil || int64(len(raw)) > maxBytes {
		return fmt.Errorf("upstream %s response could not be read within the size limit", u.Hostname())
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("upstream %s returned invalid JSON", u.Hostname())
	}
	return nil
}

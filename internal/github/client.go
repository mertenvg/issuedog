// Package github talks to the GitHub API as the issuedog GitHub App.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"
)

// Config identifies the App installation to act as.
type Config struct {
	AppID          int64
	InstallationID int64
	// Key is the App's PEM-encoded private key (see internal/appkey).
	Key []byte
	// BaseURL is the API root. Empty means https://api.github.com.
	BaseURL string
	// Transport carries the requests. Nil means http.DefaultTransport.
	Transport http.RoundTripper
}

// Client is a go-github client authenticated as an App installation. It
// mints an installation token on first use and mints a new one shortly
// before the cached token expires. Its GETs are conditional (see etagCache).
type Client struct {
	*gogithub.Client
	installation *ghinstallation.Transport
}

// New returns a client for the installation in cfg. It makes no requests.
func New(cfg Config) (*Client, error) {
	base := cfg.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	itr, err := ghinstallation.New(base, cfg.AppID, cfg.InstallationID, cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("github app %d installation %d: %w", cfg.AppID, cfg.InstallationID, err)
	}
	// The ETag cache wraps the auth transport, so each installation has its own.
	opts := []gogithub.ClientOptionsFunc{gogithub.WithTransport(newETagCache(closeMintErrors{itr}))}
	if cfg.BaseURL != "" {
		root := strings.TrimRight(cfg.BaseURL, "/")
		itr.BaseURL = root
		base := root + "/"
		opts = append(opts, gogithub.WithURLs(&base, &base))
	}
	client, err := gogithub.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}
	return &Client{Client: client, installation: itr}, nil
}

// Token returns a current installation token, minting one if needed. git
// uses it to fetch the repo.
func (c *Client) Token(ctx context.Context) (string, error) {
	tok, err := c.installation.Token(ctx)
	if err != nil {
		closeMintResponse(err)
		return "", fmt.Errorf("github installation token: %w", err)
	}
	return tok, nil
}

// closeMintErrors closes the response a failed token mint leaves open.
// ghinstallation returns it inside an *ghinstallation.HTTPError without
// closing its body, which would leak a connection per failure.
type closeMintErrors struct {
	next http.RoundTripper
}

func (t closeMintErrors) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		closeMintResponse(err)
		return nil, fmt.Errorf("github installation transport: %w", err)
	}
	return resp, nil
}

// closeMintResponse closes the body held by a ghinstallation.HTTPError.
func closeMintResponse(err error) {
	var herr *ghinstallation.HTTPError
	if errors.As(err, &herr) && herr.Response != nil && herr.Response.Body != nil {
		if cerr := herr.Response.Body.Close(); cerr != nil {
			logr.Warnf("closing the body of a failed installation token mint: %v", cerr)
		}
	}
}

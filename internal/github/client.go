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
	app          *gogithub.Client // as the App itself, for GET /app
}

// New returns a client for the installation in cfg. It makes no requests.
func New(cfg Config) (*Client, error) {
	base := cfg.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	atr, err := ghinstallation.NewAppsTransport(base, cfg.AppID, cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("github app %d installation %d: %w", cfg.AppID, cfg.InstallationID, err)
	}
	itr := ghinstallation.NewFromAppsTransport(atr, cfg.InstallationID)
	// The transports add the token or JWT to every request, a redirected
	// one too, so no client follows a redirect to another host. Minting a
	// token follows none.
	guarded := &http.Client{CheckRedirect: sameHost}
	// The ETag cache wraps the auth transport, so each installation has its own.
	opts := []gogithub.ClientOptionsFunc{gogithub.WithHTTPClient(guarded), gogithub.WithTransport(newETagCache(closeMintErrors{itr}))}
	appOpts := []gogithub.ClientOptionsFunc{gogithub.WithHTTPClient(guarded), gogithub.WithTransport(atr)}
	if cfg.BaseURL != "" {
		root := strings.TrimRight(cfg.BaseURL, "/")
		atr.BaseURL, itr.BaseURL = root, root
		base := root + "/"
		opts = append(opts, gogithub.WithURLs(&base, &base))
		appOpts = append(appOpts, gogithub.WithURLs(&base, &base))
	}
	client, err := gogithub.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}
	app, err := gogithub.NewClient(appOpts...)
	if err != nil {
		return nil, fmt.Errorf("github app client: %w", err)
	}
	return &Client{Client: client, installation: itr, app: app}, nil
}

// ErrCrossHostRedirect is wrapped by the error for a redirect to another
// host, which no client follows.
var ErrCrossHostRedirect = errors.New("a redirect to another host")

// sameHost is a CheckRedirect that refuses a redirect to another host or
// scheme, and stops after 10, as the default does.
func sameHost(req *http.Request, via []*http.Request) error {
	if from := via[0].URL; !strings.EqualFold(req.URL.Host, from.Host) || req.URL.Scheme != from.Scheme {
		return fmt.Errorf("%w: from %s to %s", ErrCrossHostRedirect, from.Host, req.URL.Host)
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after %d redirects", len(via))
	}
	return nil
}

// AppSlug returns the App's slug from GET /app, which only the App itself,
// not an installation, may call. Its comments are by "<slug>[bot]".
func (c *Client) AppSlug(ctx context.Context) (string, error) {
	app, _, err := c.app.Apps.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("github app: %w", err)
	}
	if app.GetSlug() == "" {
		return "", errors.New("github app: GET /app gave no slug")
	}
	return app.GetSlug(), nil
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

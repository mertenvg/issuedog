// Package agent runs Claude stages as the agent uid.
package agent

import (
	"errors"
	"strings"
)

// The two Claude credentials the daemon accepts (SPEC §3.3).
const (
	OAuthTokenVar = "CLAUDE_CODE_OAUTH_TOKEN"
	APIKeyVar     = "ANTHROPIC_API_KEY"
)

// agentPath is the agent's PATH. It's fixed rather than inherited, so
// nothing in the daemon's environment reaches the agent.
const agentPath = "/usr/local/bin:/usr/bin:/bin"

// Cred is the one Claude credential passed to the agent, as an environment
// variable name and value.
type Cred struct {
	Name  string
	Value string
}

// Credential picks the Claude credential from environ (os.Environ format).
// Exactly one of CLAUDE_CODE_OAUTH_TOKEN and ANTHROPIC_API_KEY must be set;
// a variable set to the empty string counts as unset.
func Credential(environ []string) (Cred, error) {
	oauth := lookup(environ, OAuthTokenVar)
	apiKey := lookup(environ, APIKeyVar)
	switch {
	case oauth != "" && apiKey != "":
		return Cred{}, errors.New("both " + OAuthTokenVar + " and " + APIKeyVar + " are set; set exactly one")
	case oauth != "":
		return Cred{Name: OAuthTokenVar, Value: oauth}, nil
	case apiKey != "":
		return Cred{Name: APIKeyVar, Value: apiKey}, nil
	default:
		return Cred{}, errors.New("no Claude credential: set " + OAuthTokenVar + " or " + APIKeyVar)
	}
}

// Env builds the agent's whole environment: the credential, the egress proxy,
// HOME and a fixed PATH. Nothing else is inherited from the daemon.
func Env(cred Cred, proxyURL, home string) []string {
	return []string{
		cred.Name + "=" + cred.Value,
		"HTTPS_PROXY=" + proxyURL,
		"HTTP_PROXY=" + proxyURL,
		"HOME=" + home,
		"PATH=" + agentPath,
	}
}

// lookup returns the value of the last entry for name in environ, matching
// os.Getenv when an entry is repeated.
func lookup(environ []string, name string) string {
	value := ""
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			value = v
		}
	}
	return value
}

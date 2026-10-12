package agent

import (
	"slices"
	"strings"
	"testing"
)

func TestCredential(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		want    Cred
		wantErr string
	}{
		{
			name:    "oauth token only",
			environ: []string{"PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=oauth"},
			want:    Cred{Name: OAuthTokenVar, Value: "oauth"},
		},
		{
			name:    "api key only",
			environ: []string{"ANTHROPIC_API_KEY=key", "HOME=/work"},
			want:    Cred{Name: APIKeyVar, Value: "key"},
		},
		{
			name:    "both set",
			environ: []string{"CLAUDE_CODE_OAUTH_TOKEN=oauth", "ANTHROPIC_API_KEY=key"},
			wantErr: "both",
		},
		{
			name:    "neither set",
			environ: []string{"PATH=/bin", "GH_TOKEN=gh"},
			wantErr: "no Claude credential",
		},
		{
			name:    "empty counts as unset",
			environ: []string{"CLAUDE_CODE_OAUTH_TOKEN=", "ANTHROPIC_API_KEY=key"},
			want:    Cred{Name: APIKeyVar, Value: "key"},
		},
		{
			name:    "both empty",
			environ: []string{"CLAUDE_CODE_OAUTH_TOKEN=", "ANTHROPIC_API_KEY="},
			wantErr: "no Claude credential",
		},
		{
			name:    "last entry wins",
			environ: []string{"ANTHROPIC_API_KEY=old", "ANTHROPIC_API_KEY=new"},
			want:    Cred{Name: APIKeyVar, Value: "new"},
		},
		{
			name:    "value keeps its equals signs",
			environ: []string{"CLAUDE_CODE_OAUTH_TOKEN=a=b="},
			want:    Cred{Name: OAuthTokenVar, Value: "a=b="},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Credential(tt.environ)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEnvHoldsOnlyTheExplicitList(t *testing.T) {
	daemon := []string{
		"GH_TOKEN=gh",
		"GITHUB_TOKEN=gh",
		"ISSUEDOG_APP_KEY=/state/app-key.pem",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth",
		"ANTHROPIC_API_KEY=",
		"PATH=/daemon/bin",
		"HOME=/work",
	}
	cred, err := Credential(daemon)
	if err != nil {
		t.Fatal(err)
	}
	got := Env(cred, "http://127.0.0.1:3128", "/home/agent")
	want := []string{
		"CLAUDE_CODE_OAUTH_TOKEN=oauth",
		"HTTPS_PROXY=http://127.0.0.1:3128",
		"HTTP_PROXY=http://127.0.0.1:3128",
		"HOME=/home/agent",
		"PATH=" + agentPath,
	}
	if !slices.Equal(got, want) {
		t.Errorf("Env = %q, want %q", got, want)
	}
	for _, kv := range got {
		for _, leaked := range []string{"GH_TOKEN", "GITHUB_TOKEN", "ISSUEDOG_APP_KEY", "ANTHROPIC_API_KEY", "/daemon/bin", "/work"} {
			if strings.Contains(kv, leaked) {
				t.Errorf("agent env entry %q leaks %q from the daemon", kv, leaked)
			}
		}
	}
}

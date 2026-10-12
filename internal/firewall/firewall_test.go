package firewall

import (
	"strings"
	"testing"
)

func TestRules(t *testing.T) {
	want := `*filter
:INPUT ACCEPT [0:0]
:FORWARD ACCEPT [0:0]
:OUTPUT ACCEPT [0:0]
:issuedog-agent - [0:0]
-A OUTPUT -m owner --uid-owner agent -j issuedog-agent
-A issuedog-agent -d 127.0.0.11/32 -j REJECT
-A issuedog-agent -o lo -d 127.0.0.0/8 -j ACCEPT
-A issuedog-agent -p tcp -j REJECT --reject-with tcp-reset
-A issuedog-agent -j REJECT
COMMIT
`
	if got := Rules("agent"); got != want {
		t.Errorf("Rules =\n%s\nwant\n%s", got, want)
	}
}

func TestRules6(t *testing.T) {
	want := `*filter
:INPUT ACCEPT [0:0]
:FORWARD ACCEPT [0:0]
:OUTPUT ACCEPT [0:0]
:issuedog-agent - [0:0]
-A OUTPUT -m owner --uid-owner agent -j issuedog-agent
-A issuedog-agent -o lo -d ::1/128 -j ACCEPT
-A issuedog-agent -p tcp -j REJECT --reject-with tcp-reset
-A issuedog-agent -j REJECT
COMMIT
`
	if got := Rules6("agent"); got != want {
		t.Errorf("Rules6 =\n%s\nwant\n%s", got, want)
	}
}

// TestRulesOnlyMatchTheUser checks that the only rule outside the agent's
// chain is the owner-matched jump, so other users' traffic is untouched.
func TestRulesOnlyMatchTheUser(t *testing.T) {
	for _, rules := range []string{Rules("agent"), Rules6("agent")} {
		for _, line := range strings.Split(rules, "\n") {
			if strings.HasPrefix(line, "-A ") && !strings.HasPrefix(line, "-A "+Chain+" ") &&
				line != "-A OUTPUT -m owner --uid-owner agent -j "+Chain {
				t.Errorf("rule %q isn't limited to the agent user", line)
			}
		}
	}
}

func TestRestoreNamesTheTool(t *testing.T) {
	err := restore("issuedog-no-such-restore", Rules("agent"))
	if err == nil || !strings.HasPrefix(err.Error(), "running issuedog-no-such-restore: ") {
		t.Errorf("err = %v, want it to name the tool", err)
	}
}

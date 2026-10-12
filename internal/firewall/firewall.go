// Package firewall limits the agent uid's network access to the egress proxy
// (SPEC §3.1), with owner-matched iptables rules.
//
// The rules replace the filter table of the container's own network
// namespace. Docker keeps its port-mapping and isolation rules in the host's
// namespace, and inside the container it only adds nat-table rules for its
// embedded DNS server, which a filter-table restore leaves alone. Replacing
// the table also makes a restart replace the rules rather than add a second
// jump, as appending to OUTPUT with --noflush would. So the container must
// have its own network namespace: never --network host.
package firewall

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Chain holds the agent's rules. OUTPUT sends it every packet that the agent
// user's sockets send.
const Chain = "issuedog-agent"

// Rules returns iptables-restore input that lets user reach loopback only:
// the egress proxy, and test servers its own commands start (go test's
// httptest, for one). Docker's embedded DNS on 127.0.0.11 forwards queries
// outside, so it is rejected first. Everything else user sends is rejected,
// including UDP and so DNS. Other users are unaffected.
func Rules(user string) string {
	return filter(user, "-A "+Chain+" -d 127.0.0.11/32 -j REJECT\n"+
		"-A "+Chain+" -o lo -d 127.0.0.0/8 -j ACCEPT\n")
}

// Rules6 is Rules for IPv6: loopback only.
func Rules6(user string) string {
	return filter(user, "-A "+Chain+" -o lo -d ::1/128 -j ACCEPT\n")
}

func filter(user, accept string) string {
	return "*filter\n" +
		":INPUT ACCEPT [0:0]\n" +
		":FORWARD ACCEPT [0:0]\n" +
		":OUTPUT ACCEPT [0:0]\n" +
		":" + Chain + " - [0:0]\n" +
		"-A OUTPUT -m owner --uid-owner " + user + " -j " + Chain + "\n" +
		accept +
		"-A " + Chain + " -p tcp -j REJECT --reject-with tcp-reset\n" +
		"-A " + Chain + " -j REJECT\n" +
		"COMMIT\n"
}

// Apply loads Rules and Rules6 for user with iptables-restore and
// ip6tables-restore.
func Apply(user string) error {
	if err := restore("iptables-restore", Rules(user)); err != nil {
		return fmt.Errorf("applying the IPv4 rules for %s: %w", user, err)
	}
	if err := restore("ip6tables-restore", Rules6(user)); err != nil {
		return fmt.Errorf("applying the IPv6 rules for %s: %w", user, err)
	}
	return nil
}

// restore runs tool with rules on stdin. The tool gets CAP_NET_ADMIN as an
// ambient capability, which the daemon can pass on only because its binary
// holds it as a file capability. No other child of the daemon gets it.
func restore(tool, rules string) error {
	attr, err := netAdmin()
	if err != nil {
		return fmt.Errorf("running %s: %w", tool, err)
	}
	cmd := exec.Command(tool)
	cmd.Stdin = strings.NewReader(rules)
	cmd.SysProcAttr = attr
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("running %s: %w: %s", tool, err, bytes.TrimSpace(out))
	}
	return nil
}

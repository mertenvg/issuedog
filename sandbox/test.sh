#!/bin/sh
# Checks the sandbox from inside the image. `make sandbox-test` builds the
# image and runs this script in it as root, only so it can act as each user.
# The image itself runs as issuedog. Each check prints PASS or FAIL,
# and the script exits non-zero if any check failed.
set -u

failed=0

pass() { echo "PASS $1"; }
fail() { echo "FAIL $1"; failed=1; }

# check <name> <command...> passes when the command succeeds.
check() {
	name=$1
	shift
	if "$@" >/dev/null 2>&1; then pass "$name"; else fail "$name"; fi
}

# check_not <name> <command...> passes when the command fails.
check_not() {
	name=$1
	shift
	if "$@" >/dev/null 2>&1; then fail "$name"; else pass "$name"; fi
}

# expect <name> <want> <command...> passes when the command prints want.
expect() {
	name=$1
	want=$2
	shift 2
	got=$("$@" 2>&1)
	if [ "$got" = "$want" ]; then pass "$name"; else fail "$name: want '$want', got '$got'"; fi
}

# as <user> <command...> runs the command as user, with that user's HOME.
as() {
	user=$1
	shift
	home=$(getent passwd "$user" | cut -d: -f6)
	setpriv --reuid="$user" --regid="$user" --init-groups -- env HOME="$home" "$@"
}

# Users
expect "issuedog uid" 10001 id -u issuedog
expect "agent uid" 10002 id -u agent
expect "issuedog gid" 10001 id -g issuedog
expect "agent gid" 10002 id -g agent
expect "agent groups" agent id -nG agent

# Directories: owner, group and mode
expect "/work" "issuedog:issuedog 755" stat -c '%U:%G %a' /work
expect "/work/runs" "agent:agent 755" stat -c '%U:%G %a' /work/runs
expect "/state" "issuedog:issuedog 700" stat -c '%U:%G %a' /state
expect "/run/issuedog" "issuedog:issuedog 755" stat -c '%U:%G %a' /run/issuedog
expect "/home/agent" "agent:agent 700" stat -c '%U:%G %a' /home/agent

# Daemon binary
bin=/usr/local/bin/issuedog
expect "daemon binary owner and mode" "root:issuedog 750" stat -c '%U:%G %a' "$bin"
caps="$bin cap_setgid,cap_setuid,cap_net_admin=ep"
expect "daemon binary capabilities" "$caps" getcap "$bin"
expect "only the daemon has capabilities" "$caps" sh -c 'find / -xdev -type f -exec getcap {} + 2>/dev/null'
check "issuedog can run the daemon" as issuedog "$bin" --version
check_not "agent can't run the daemon" as agent "$bin" --version
check_not "agent can't read the daemon" as agent cat "$bin"

# App key: a 0600 key file owned by issuedog is readable by issuedog and not
# by agent. It goes in a directory agent can enter, so the file's own mode is
# what's checked.
key=/run/issuedog/sandbox-test-app-key.pem
install -o issuedog -g issuedog -m 0600 /dev/null "$key"
check "issuedog can read the key file" as issuedog cat "$key"
check_not "agent can't read the key file" as agent cat "$key"
rm -f "$key"

# Context files: a 0755 run dir and 0644 files owned by issuedog, which agent
# can read but not change (SPEC §3.2).
ctx=/run/issuedog/sandbox-test-run
install -d -o issuedog -g issuedog -m 0755 "$ctx"
install -o issuedog -g issuedog -m 0644 /dev/null "$ctx/prompt.md"
expect "context dir" "issuedog:issuedog 755" stat -c '%U:%G %a' "$ctx"
expect "context file" "issuedog:issuedog 644" stat -c '%U:%G %a' "$ctx/prompt.md"
check "agent can read a context file" as agent cat "$ctx/prompt.md"
check_not "agent can't write a context file" as agent sh -c 'echo x >>"$1"' sh "$ctx/prompt.md"
check_not "agent can't create files in the context dir" as agent touch "$ctx/new.md"
check_not "agent can't remove a context file" as agent rm -f "$ctx/prompt.md"
rm -rf "$ctx"

# Key import of a throwaway RSA key that make sandbox-test mounts.
key=/state/app-key.pem
check "issuedog imports the key" as issuedog "$bin" key import </test-key.pem
expect "imported key file" "issuedog:issuedog 600" stat -c '%U:%G %a' "$key"
check "issuedog can load the key" as issuedog "$bin" key check
check_not "agent can't read the imported key" as agent cat "$key"
check_not "a non-PEM import is refused" as issuedog "$bin" key import </etc/hostname
check "the refused import kept the key" cmp -s /test-key.pem "$key"

# The daemon loads the key before it applies the firewall. It can't keep
# running yet, so it exits at "not implemented" either way, and its log shows
# whether it got as far as the proxy. This needs --cap-add NET_ADMIN.
log=/run/issuedog/sandbox-test-daemon.log
as issuedog env ANTHROPIC_API_KEY=sandbox-test "$bin" >"$log" 2>&1
check "the daemon starts with a 0600 key" grep -q 'egress proxy listening' "$log"
chmod 0640 "$key"
as issuedog env ANTHROPIC_API_KEY=sandbox-test "$bin" >"$log" 2>&1
check "the daemon refuses a 0640 key" grep -q "loading the App key: app key $key: mode 0640" "$log"
check_not "the daemon stops before egress with a 0640 key" grep -q 'egress proxy listening' "$log"
rm -f "$key" "$log"

# Tools
check "agent can run claude" as agent claude --version
check "git is installed" git --version
check "iptables is installed" iptables --version
check "openssl is installed" openssl version

# Egress (SPEC §3.1). The daemon can't keep running yet, so issuedog applies
# its firewall and serves its proxy with the unlisted `issuedog egress`, which
# runs until the container stops. This needs --cap-add NET_ADMIN.
log=/run/issuedog/sandbox-test-egress.log
as issuedog "$bin" egress >"$log" 2>&1 &
i=0
until grep -q 'egress proxy listening' "$log" || [ "$i" -ge 50 ]; do
	sleep 0.1
	i=$((i + 1))
done
check "issuedog applies the firewall and starts the proxy" grep -q 'egress proxy listening on http://127.0.0.1:3128' "$log"
check "the IPv4 firewall has the agent chain" iptables -S issuedog-agent
check "the IPv6 firewall has the agent chain" ip6tables -S issuedog-agent

# tls <user> <host> [options...] completes a TLS handshake with host:443 as
# user, and verifies the certificate.
tls() {
	user=$1
	host=$2
	shift 2
	as "$user" openssl s_client -connect "$host:443" -servername "$host" \
		-verify_hostname "$host" -verify_return_error "$@" </dev/null
}
# These two need internet access, as on the owner's Mac. Without it they fail
# rather than pass. The refusals below pass offline too, so they only mean
# something when these two pass.
check "agent reaches api.anthropic.com through the proxy" tls agent api.anthropic.com -proxy 127.0.0.1:3128
check "issuedog keeps direct egress" tls issuedog github.com
check_not "agent can't reach example.com through the proxy" tls agent example.com -proxy 127.0.0.1:3128
check_not "agent can't connect directly" tls agent api.anthropic.com
check "issuedog can connect to an address directly" as issuedog openssl s_client -connect 1.1.1.1:443 </dev/null
check_not "agent can't connect to an address directly" as agent openssl s_client -connect 1.1.1.1:443 </dev/null
check_not "agent can't resolve names" as agent getent hosts api.anthropic.com

if [ "$failed" -ne 0 ]; then
	echo "Some checks failed."
	exit 1
fi
echo "All checks passed."

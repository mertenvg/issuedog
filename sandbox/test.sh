#!/bin/sh
# Checks the sandbox from inside the image. `make sandbox-test` builds the
# image and runs this script in it as root. Each check prints PASS or FAIL,
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
expect "daemon binary capabilities" "$bin cap_setgid,cap_setuid=ep" getcap "$bin"
check "issuedog can run the daemon" as issuedog "$bin" --version
check "the entrypoint runs the daemon" /usr/local/lib/issuedog/entrypoint.sh --version
check_not "agent can't run the daemon" as agent "$bin" --version
check_not "agent can't read the daemon" as agent cat "$bin"

# App key: a key file where the daemon keeps it, mode 0600, is readable by
# issuedog and not by agent.
key=/state/sandbox-test-app-key.pem
install -o issuedog -g issuedog -m 0600 /dev/null "$key"
check "issuedog can read the key file" as issuedog cat "$key"
check_not "agent can't read the key file" as agent cat "$key"
rm -f "$key"

# Tools
check "agent can run claude" as agent claude --version
check "git is installed" git --version
check "iptables is installed" iptables --version

if [ "$failed" -ne 0 ]; then
	echo "Some checks failed."
	exit 1
fi
echo "All checks passed."

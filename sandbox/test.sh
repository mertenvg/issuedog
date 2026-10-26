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
check_not "agent started by the daemon can't read the imported key" as issuedog "$bin" as-agent -- cat "$key"
check_not "a non-PEM import is refused" as issuedog "$bin" key import </etc/hostname
check "the refused import kept the key" cmp -s /test-key.pem "$key"

# The daemon loads the key before it calls GitHub or applies the firewall.
# With placeholder settings it gets past them, logs that it loaded the key,
# and then stops at its first GitHub call, which these settings can't pass, so
# its log shows whether it got past the key. That line comes before any
# request, so this needs no internet access.
log=/run/issuedog/sandbox-test-daemon.log
as issuedog env ANTHROPIC_API_KEY=sandbox-test ISSUEDOG_REPO=sandbox/test \
	ISSUEDOG_APP_ID=1 ISSUEDOG_INSTALLATION_ID=1 "$bin" >"$log" 2>&1
check "the daemon starts with a 0600 key" grep -q "loaded the App key from $key" "$log"
chmod 0640 "$key"
as issuedog env ANTHROPIC_API_KEY=sandbox-test ISSUEDOG_REPO=sandbox/test \
	ISSUEDOG_APP_ID=1 ISSUEDOG_INSTALLATION_ID=1 "$bin" >"$log" 2>&1
check "the daemon refuses a 0640 key" grep -q "loading the App key: app key $key: mode 0640" "$log"
check_not "the daemon stops before egress with a 0640 key" grep -q 'egress proxy listening' "$log"
rm -f "$key" "$log"

# Agent processes (SPEC §3, §3.1). The unlisted `issuedog as-agent` starts a
# command as agent through the same uid switch as stage runs, from the
# daemon's cap_setuid and cap_setgid, so these check what an agent run gets.
expect "the daemon starts agent with agent's uid" 10002 as issuedog "$bin" as-agent -- id -u
expect "the daemon starts agent with agent's gid" 10002 as issuedog "$bin" as-agent -- id -g
expect "the daemon starts agent with no groups" 10002 as issuedog "$bin" as-agent -- id -G
zero=$(printf 'Cap%s:\t0000000000000000\n' Inh Prm Eff Amb)
expect "the daemon starts agent with no capabilities" "$zero" as issuedog "$bin" as-agent -- grep -E '^Cap(Eff|Prm|Inh|Amb):' /proc/self/status
check_not "agent started by the daemon can't read /state" as issuedog "$bin" as-agent -- ls /state
check_not "agent started by the daemon can't read the daemon" as issuedog "$bin" as-agent -- cat "$bin"

# Git handoff (SPEC §3.2). The unlisted `issuedog handoff` runs the daemon's
# own handoff of a run's commits: the bundle as agent, through the same uid
# switch as stage runs, then the fetch into the bare repo as issuedog. The
# agent's clone plants a hooks path, every hook, core.fsmonitor,
# core.sshCommand, and a filter and a diff driver its .gitattributes names.
# Each appends the uid it ran as to a marker of its own, so a plant git ran
# as issuedog would leave 10001 in one.
bare=/work/sandbox-test-handoff.git
run=sandbox-test-handoff
clone=/work/runs/$run
planted=$clone/.git/planted
markers=/tmp/sandbox-test-handoff-markers
# The global git config of git_as below: the bare repo, which agent doesn't
# own, is a safe.directory, as in the config the daemon's workspace writes
# for agent's clones.
gitconfig=/tmp/sandbox-test-handoff.gitconfig
printf '[safe]\n\tdirectory = %s\n' "$bare" >"$gitconfig"
chmod 0644 "$gitconfig"
install -d -m 1777 "$markers"

# git_as <user> <git args...> runs git as user, with gitconfig as its
# global config and a committer identity.
git_as() {
	gituser=$1
	shift
	as "$gituser" env GIT_CONFIG_GLOBAL="$gitconfig" GIT_AUTHOR_NAME=sandbox-test \
		GIT_AUTHOR_EMAIL=sandbox-test@example.invalid GIT_COMMITTER_NAME=sandbox-test \
		GIT_COMMITTER_EMAIL=sandbox-test@example.invalid git "$@"
}

# plant <path> <marker> [filter] writes, as agent, a script at path that
# appends the uid it runs as to <marker>.<uid> in markers. A filter then
# passes its input on.
plant() {
	last=
	if [ "$#" -gt 2 ]; then last='exec cat'; fi
	as agent sh -c 'printf "%s\n" "#!/bin/sh" "id -u >>$2/$3.\$(id -u)" "$4" >"$1" && chmod 0755 "$1"' \
		sh "$1" "$markers" "$2" "$last"
}

git_as issuedog init --quiet --bare --initial-branch=main "$bare"
tree=$(git_as issuedog --git-dir="$bare" mktree </dev/null)
start=$(git_as issuedog --git-dir="$bare" commit-tree -m start "$tree")
git_as issuedog --git-dir="$bare" update-ref refs/heads/main "$start"

git_as agent clone --quiet --no-hardlinks "$bare" "$clone"
as agent sh -c 'echo "* filter=planted diff=planted" >"$1/.gitattributes" && echo run >"$1/run.txt"' sh "$clone"
git_as agent -C "$clone" add .gitattributes run.txt
git_as agent -C "$clone" commit --quiet -m run
commit=$(git_as agent -C "$clone" rev-parse HEAD)
as agent mkdir "$planted" "$planted/hooks"
for hook in applypatch-msg pre-applypatch post-applypatch pre-commit pre-merge-commit \
	prepare-commit-msg commit-msg post-commit pre-rebase post-checkout post-merge \
	pre-push pre-receive update proc-receive post-receive post-update \
	reference-transaction push-to-checkout pre-auto-gc post-rewrite sendemail-validate \
	fsmonitor-watchman p4-changelist p4-prepare-changelist p4-post-changelist \
	p4-pre-submit post-index-change; do
	plant "$clone/.git/hooks/$hook" hooks-dir
	plant "$planted/hooks/$hook" hooks-path
done
plant "$planted/fsmonitor" fsmonitor
plant "$planted/ssh" ssh
plant "$planted/filter" filter filter
plant "$planted/diff" diff
git_as agent -C "$clone" config core.hooksPath "$planted/hooks"
git_as agent -C "$clone" config core.fsmonitor "$planted/fsmonitor"
git_as agent -C "$clone" config core.sshCommand "$planted/ssh"
git_as agent -C "$clone" config filter.planted.clean "$planted/filter"
git_as agent -C "$clone" config filter.planted.smudge "$planted/filter"
git_as agent -C "$clone" config diff.planted.command "$planted/diff"

check "issuedog hands off agent's commit" as issuedog "$bin" handoff "$clone" "$start" "$bare" "$run"
expect "the commit lands under the run's ref" "$commit" \
	as issuedog git --git-dir="$bare" rev-parse --verify --quiet "refs/issuedog/runs/$run"
check_not "nothing planted in the clone runs as issuedog" grep -rqx 10001 "$markers"
# --no-verify skips the planted pre-push hook, so only the bare repo's
# permissions can refuse the push.
check_not "agent can't push to the bare repo" git_as agent -C "$clone" push --quiet --no-verify origin HEAD:refs/heads/pushed
rm -rf "$bare" "$clone" "$markers" "$gitconfig"

# Tools
check "agent can run claude" as agent claude --version
check "git is installed" git --version
check "iptables is installed" iptables --version
check "openssl is installed" openssl version

# Egress (SPEC §3.1). The daemon needs GitHub during startup, before it
# applies the firewall, so here issuedog applies its firewall and serves its
# proxy with the unlisted `issuedog egress`, which runs until the container
# stops. This needs --cap-add NET_ADMIN.
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

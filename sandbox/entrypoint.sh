#!/bin/sh
# Runs as root, then runs the daemon as issuedog. The egress firewall for the
# agent uid is applied here, before root is dropped (#25).
#
# The daemon's file capabilities only take effect without no_new_privs, so
# don't run the container with --security-opt no-new-privileges.
set -eu

exec setpriv --reuid=issuedog --regid=issuedog --init-groups --inh-caps=-all -- \
	/usr/local/bin/issuedog "$@"

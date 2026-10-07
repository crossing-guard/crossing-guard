#!/usr/bin/env bash
# Rebuild and restart the running daemon. The console is go:embed'ed into the
# binary, so editing internal/daemon/static/ changes NOTHING until a rebuild —
# a browser refresh serves the assets compiled into the process that is already
# running. This script is the whole edit loop, and it prints the console URL so
# there is no separate "now go find your token" step.
#
# DEVELOPER TOOL, FOR A MACHINE THAT ALREADY RUNS THE INSTALLED SERVICE. It replaces
# the binary that service runs with a build of this checkout, restarts it (which
# applies any store migration in this checkout, and an older binary cannot open the
# store afterwards), and prints a console URL that contains the local API token.
# Do not run it to try out a source checkout.
#
# It writes to the exact path launchd runs (read from the plist, not assumed):
# building to a different path is the silent failure this replaces — a green
# build, an unchanged console, and no error anywhere.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

label=com.crossing-guard.daemon
plist="$HOME/Library/LaunchAgents/$label.plist"
if [ ! -f "$plist" ]; then
	echo "no service installed at $plist — run 'crossing-guard serve' once to install it" >&2
	exit 1
fi

# Read the binary path, data dir and address the SERVICE uses. Anything else is
# a guess, and a guess here rebuilds a binary nobody runs.
read -r exe data addr < <(
	/usr/bin/python3 - "$plist" <<-'PY'
	import plistlib,sys
	a=plistlib.load(open(sys.argv[1],'rb'))["ProgramArguments"]
	def opt(f): return a[a.index(f)+1] if f in a else ""
	print(a[0], opt("--data"), opt("--addr"))
	PY
)
[ -n "$exe" ] || { echo "could not read ProgramArguments from $plist" >&2; exit 1; }

staged_exe=$(mktemp "${exe}.reload.XXXXXX") || exit 1
case "$staged_exe" in
	"${exe}.reload."*) ;;
	*) echo "refusing unexpected staging path: $staged_exe" >&2; exit 1 ;;
esac
cleanup_stage() {
	if [ -n "$staged_exe" ] && [ -f "$staged_exe" ]; then
		rm -f -- "$staged_exe"
	fi
}
trap cleanup_stage EXIT

# Say which commit this binary is. Go's own stamp cannot: it ignores a linked
# worktree's .git file, so a worktree build carries no revision, or the enclosing
# checkout's. The revision is that of the tree being built; "+dirty" means what Go
# means by it — anything `git status` reports, untracked files included, because an
# untracked file under internal/daemon/static/ is compiled into the binary.
# A status that cannot be read is not a clean tree, and untracked files are
# listed whatever the checkout's own status settings say.
revision=$(git rev-parse --verify HEAD 2>/dev/null || true)
if [ -n "$revision" ]; then
	changes=$(git status --porcelain --untracked-files=normal 2>/dev/null) || changes="unreadable"
	[ -z "$changes" ] || revision="$revision+dirty"
fi
link_flags=()
if [ -n "$revision" ]; then
	link_flags=(-ldflags "-X crossing-guard/internal/guardcli.buildRevision=$revision")
fi

echo "building -> $staged_exe${revision:+ ($revision)}"
# ${a[@]+"${a[@]}"}: an empty array is "unbound" to bash 3.2 under set -u.
go build -buildvcs=false ${link_flags[@]+"${link_flags[@]}"} -o "$staged_exe" ./cmd/crossing-guard || exit 1

# launchd rejects a rebuilt executable whose signature does not validate. Sign and
# verify a unique same-directory inode before one atomic promotion; never expose a
# partially built or half-signed binary at the path the service runs.
/usr/bin/codesign --force --sign - --timestamp=none "$staged_exe" || exit 1
/usr/bin/codesign --verify --strict --verbose=2 "$staged_exe" || exit 1
mv -f -- "$staged_exe" "$exe" || exit 1
staged_exe=""
/usr/bin/codesign --verify --strict --verbose=2 "$exe" || exit 1

echo "restarting $label"
# kickstart restarts the job but re-reads NOTHING: launchd keeps the definition it
# loaded, so a plist whose ProgramArguments changed (a renamed binary, a new port)
# restarts the OLD program and looks like it worked. This block is what the comment
# above used to only promise: compare the program launchd currently holds against
# the one the plist now names, and re-register when they differ.
uid=$(id -u)
loaded=$(launchctl print "gui/$uid/$label" 2>/dev/null \
	| sed -n 's/^[[:space:]]*program = //p' | head -1)
if [ -z "$loaded" ]; then
	launchctl bootstrap "gui/$uid" "$plist" || exit 1
elif [ "$loaded" != "$exe" ]; then
	# The plist moved out from under the loaded job. kickstart would silently keep
	# running "$loaded"; only a bootout/bootstrap re-reads ProgramArguments.
	echo "plist changed ($loaded -> $exe) — re-registering"
	launchctl bootout "gui/$uid/$label" 2>/dev/null
	launchctl bootstrap "gui/$uid" "$plist" || exit 1
else
	launchctl kickstart -k "gui/$uid/$label" || exit 1
fi

# Wait for the new process to bind rather than declaring success on a restart
# that crash-looped. KeepAlive would hide that: launchd keeps retrying and the
# service still reports as loaded. Schema migration and bounded startup recovery can
# legitimately exceed six seconds on a mature store, so keep a 30-second outer bound.
token=$(cat "$data/api-token" 2>/dev/null)
for _ in $(seq 150); do
	code=$(curl -s -o /dev/null -w '%{http_code}' "http://$addr/" 2>/dev/null)
	if [ "$code" = "200" ]; then
		echo "up: http://$addr/#t=$token"
		exit 0
	fi
	sleep 0.2
done
echo "daemon did not answer on $addr after restart — check $data/daemon.log" >&2
exit 1

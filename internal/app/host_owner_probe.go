package app

import "path"

// Probe exit codes. They are a contract between these scripts and every caller
// that classifies their answer — preflight, the engine's readHostOwner, status,
// the journal read, and the state-file reads all map the same number to the
// same meaning. They are deliberately not owner-specific: UndeterminedArm is
// shared, so the codes it can raise have to be too.
const (
	// ProbeUnreadable: the thing is there and is the right shape, but this
	// account cannot read it. Distinct from a failed command: the probe
	// established the fact and is reporting it.
	ProbeUnreadable = 2
	// ProbeAbsent: nothing there, and we could see that for ourselves.
	ProbeAbsent = 3
	// ProbeNotRegular: something is there, but it is not a record we will
	// read — a symlink (dangling or not), a directory, a device.
	ProbeNotRegular = 4
	// ProbeUndetermined: a directory on the way exists but cannot be
	// searched, so absence could not be established.
	ProbeUndetermined = 5
	// ProbeStatePathNotDirectory: a directory on the way is not a directory.
	// Kept apart from ProbeNotRegular so callers do not report a broken
	// state directory as a statement about the record inside it.
	ProbeStatePathNotDirectory = 6
)

// UndeterminedArm returns shell that refuses to call a path absent when it
// could not look. Callers run it once the path itself has failed both -e and
// -L, which is true in two very different situations: it is not there, or a
// directory on the way cannot be searched and we never got to see.
//
// Checking the immediate parent is not enough — an unsearchable *grandparent*
// hides the parent too, so `[ -e parent ]` is false and the check passes for
// the same wrong reason. This walks up until it finds a component that is
// visible, then asks two questions about it: is it a directory at all (a plain
// file where the state directory belongs is a broken host, not a permission
// problem, and exit 6 says so — a statement about the directory, never about
// the record inside it), and can we actually search it.
//
// Searchability is tested by entering it rather than by `test -x`, which reads
// permission bits: a mode-0000 directory is searchable by root but has no
// execute bit, so -x would report undetermined for the account that can in
// fact look. A path with no visible ancestor at all bottoms out at / and
// reports absence, which is the fresh-host answer.
func UndeterminedArm(probePath string) string {
	return "__ob_d=" + shellQuote(path.Dir(probePath)) + "; " +
		"while :; do " +
		"if [ -e \"$__ob_d\" ] || [ -L \"$__ob_d\" ]; then " +
		"[ -d \"$__ob_d\" ] || exit 6; " +
		"( cd \"$__ob_d\" ) 2>/dev/null || exit 5; break; fi; " +
		"case \"$__ob_d\" in /|.) break;; esac; " +
		"__ob_d=$(dirname \"$__ob_d\"); done; "
}

// HostOwnerProbe builds the read used for the host owner record, shared so
// preflight, status, and the engine ask the host exactly one question.
// Preflight accepting a record the engine then refuses would make preflight
// pass on a host every subsequent mutation rejects.
func HostOwnerProbe(recordPath string) string {
	p := shellQuote(recordPath)
	// -e follows symlinks, so it is false for a dangling one. Testing it
	// alone reports a dangling owner record as an unclaimed host, and the
	// claim that preflight then promises dies in `set -C` with an opaque
	// message; -L is what distinguishes "no record" from "a record we
	// refuse". The noclobber write is what stops the claim landing through
	// the link — this probe is what makes the refusal legible.
	//
	// Both primaries are also false when the state directory cannot be
	// searched, which is not the same answer: reporting EACCES as an
	// unclaimed host is how one application adopts a machine that already
	// belongs to another. UndeterminedArm separates the two, and only runs
	// once absence is otherwise established, so the common paths — a fresh
	// host with no state directory at all, and a claimed one — keep their
	// answers.
	return "if [ ! -e " + p + " ] && [ ! -L " + p + " ]; then " +
		UndeterminedArm(recordPath) +
		"exit 3; fi; " +
		"if [ ! -f " + p + " ] || [ -L " + p + " ]; then exit 4; fi; " +
		// The record is mode 0600, so "present, regular, and not ours to
		// read" is its most likely refusal. Without this, cat exits 1 and
		// callers that render a report lose everything else on the host to
		// one unreadable file.
		"if [ ! -r " + p + " ]; then exit 2; fi; cat " + p
}

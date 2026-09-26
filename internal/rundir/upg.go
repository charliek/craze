package rundir

import (
	"io"
	"os"
	"strconv"
	"strings"
)

// The account files ProcessEnv reads, on Linux only.
const (
	nsswitchPath = "/etc/nsswitch.conf"
	passwdPath   = "/etc/passwd"
	groupPath    = "/etc/group"
)

// processAccounts is ProcessEnv's PrivateGID and NSSwitch on goos, each file
// read with read (readSystemFile). Off Linux both are empty, so a
// group-writable ancestor is never exempt there: macOS takes its accounts from
// Directory Services, which none of these files describe, and a macOS user's
// primary group is the shared staff. A file that cannot be read leaves what
// it feeds empty, which denies the exemption.
func processAccounts(goos string, euid int, read func(string) ([]byte, error)) (gid int, nsswitch string) {
	if goos != "linux" {
		return 0, ""
	}
	if b, err := read(nsswitchPath); err == nil {
		nsswitch = string(b)
	}
	passwd, err := read(passwdPath)
	if err != nil {
		return 0, nsswitch
	}
	group, err := read(groupPath)
	if err != nil {
		return 0, nsswitch
	}
	return privateGID(euid, string(passwd), string(group)), nsswitch
}

// readSystemFile reads one of the root-owned files above. Links are followed
// (NixOS links /etc/nsswitch.conf into /etc/static), but only a regular file
// is read, and a FIFO is refused without waiting for a writer (openRegular).
func readSystemFile(path string) ([]byte, error) {
	f, err := openRegular(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// localAccounts reports whether nsswitch — /etc/nsswitch.conf's contents —
// takes users and groups from local sources alone, which is what lets
// privateGID's reading of /etc/passwd and /etc/group stand for every account
// on the machine. An account from NIS, LDAP or sssd can share a local user's
// primary gid, and /etc/passwd never shows it.
//
// The passwd and group lines must be present, and each, like an initgroups
// line if there is one (it can grant groups the group line does not name),
// must name at least one service and only the services "files" and
// "systemd"; action items ("[NOTFOUND=return]") are ignored. Anything else is
// false: another service (compat, nis, ldap, sss, winbind, …), a missing or
// repeated line, or syntax this simple parser does not know.
func localAccounts(nsswitch string) bool {
	seen := map[string]bool{}
	for _, line := range strings.Split(nsswitch, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		end := strings.IndexAny(line, ": \t")
		if end <= 0 {
			return false
		}
		rest, ok := strings.CutPrefix(strings.TrimLeft(line[end:], " \t"), ":")
		if !ok {
			return false
		}
		db := strings.ToLower(line[:end])
		if db != "passwd" && db != "group" && db != "initgroups" {
			continue
		}
		if seen[db] || !localServices(rest) {
			return false
		}
		seen[db] = true
	}
	return seen["passwd"] && seen["group"]
}

// localServices reports whether one nsswitch line's service list names one or
// more services, each "files" or "systemd", with any action items after a
// service skipped.
func localServices(list string) bool {
	services := 0
	for {
		list = strings.TrimLeft(list, " \t")
		switch {
		case list == "":
			return services > 0
		case list[0] == '[':
			end := strings.IndexByte(list, ']')
			if services == 0 || end < 0 || strings.IndexByte(list[1:end], '[') >= 0 {
				return false
			}
			list = list[end+1:]
		default:
			end := strings.IndexAny(list, " \t[")
			if end < 0 {
				end = len(list)
			}
			if name := list[:end]; name != "files" && name != "systemd" {
				return false
			}
			services++
			list = list[end:]
		}
	}
}

// privateGID is the gid of uid's user-private group, or 0 when it has none,
// by the rule OpenSSH's Debian user-group-modes patch applies before it
// trusts a group-writable directory: uid's primary group carries the user's
// own name, has no supplementary member but the user, and is no other
// account's primary group. passwd and group are /etc/passwd and /etc/group;
// the accounts they cannot show are localAccounts' concern.
func privateGID(uid int, passwd, group string) int {
	var name string
	gid := -1
	for _, f := range records(passwd, 7) {
		if u, err := strconv.Atoi(f[2]); err == nil && u == uid {
			name = f[0]
			if g, err := strconv.Atoi(f[3]); err == nil {
				gid = g
			}
			break
		}
	}
	if name == "" || gid <= 0 {
		return 0
	}
	for _, f := range records(passwd, 7) {
		u, uerr := strconv.Atoi(f[2])
		g, gerr := strconv.Atoi(f[3])
		if uerr != nil || gerr != nil || (g == gid && u != uid) {
			return 0
		}
	}
	found := false
	for _, f := range records(group, 4) {
		g, err := strconv.Atoi(f[2])
		if err != nil {
			return 0
		}
		if g != gid {
			continue
		}
		if found || f[0] != name {
			return 0
		}
		found = true
		for _, m := range strings.Split(f[3], ",") {
			if m != "" && m != name {
				return 0
			}
		}
	}
	if !found {
		return 0
	}
	return gid
}

// records is the colon-separated records of an account file with exactly n
// fields each; comments, blank lines and NIS "+"/"-" lines are dropped.
func records(file string, n int) [][]string {
	var out [][]string
	for _, line := range strings.Split(file, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == '+' || line[0] == '-' {
			continue
		}
		if f := strings.Split(line, ":"); len(f) == n {
			out = append(out, f)
		}
	}
	return out
}

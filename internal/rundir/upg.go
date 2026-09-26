package rundir

import (
	"os"
	"strconv"
	"strings"
)

// processPrivateGID is the euid's user-private group as the local account
// files describe it (privateGID), or 0. Accounts only a directory service
// knows (LDAP, sssd) get no exemption: a group-writable ancestor is refused
// for them, which is the safe side.
func processPrivateGID(euid int) int {
	passwd, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return 0
	}
	group, err := os.ReadFile("/etc/group")
	if err != nil {
		return 0
	}
	return privateGID(euid, string(passwd), string(group))
}

// privateGID is the gid of uid's user-private group, or 0 when it has none,
// by the rule OpenSSH's Debian user-group-modes patch applies before it
// trusts a group-writable directory: uid's primary group carries the user's
// own name, has no supplementary member but the user, and is no other
// account's primary group. passwd and group are /etc/passwd and /etc/group.
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

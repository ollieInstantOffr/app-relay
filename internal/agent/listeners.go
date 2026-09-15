package agent

import (
	"bufio"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type rawSocket struct {
	proto string
	addr  string
	port  int
	inode string
}

// listListeners returns TCP listening and UDP bound sockets from /proc/net.
// The engine containers use host networking, so these are the host's sockets.
func listListeners(procRoot string) []Listener {
	var raw []rawSocket
	for _, f := range []struct {
		name, proto string
		v6          bool
	}{{"tcp", "tcp", false}, {"tcp6", "tcp", true}, {"udp", "udp", false}, {"udp6", "udp", true}} {
		fh, err := os.Open(filepath.Join(procRoot, "net", f.name))
		if err != nil {
			continue
		}
		raw = append(raw, parseProcNet(fh, f.proto, f.v6)...)
		fh.Close()
	}
	procs := socketOwners(procRoot)
	seen := map[string]bool{}
	out := []Listener{}
	for _, s := range raw {
		key := s.proto + "|" + s.addr + "|" + strconv.Itoa(s.port)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Listener{Proto: s.proto, Address: s.addr, Port: s.port, Process: procs[s.inode]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		if out[i].Proto != out[j].Proto {
			return out[i].Proto < out[j].Proto
		}
		return out[i].Address < out[j].Address
	})
	return out
}

// parseProcNet parses /proc/net/{tcp,tcp6,udp,udp6}.
func parseProcNet(r io.Reader, proto string, v6 bool) []rawSocket {
	sc := bufio.NewScanner(r)
	out := []rawSocket{}
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		addr, port, ok := parseHexAddr(f[1], v6)
		if !ok || port == 0 {
			continue
		}
		if proto == "tcp" {
			if f[3] != "0A" { // LISTEN
				continue
			}
		} else {
			// UDP: skip connected sockets (remote port set).
			if _, rport, ok := parseHexAddr(f[2], v6); ok && rport != 0 {
				continue
			}
		}
		out = append(out, rawSocket{proto: proto, addr: addr, port: port, inode: f[9]})
	}
	return out
}

func parseHexAddr(s string, v6 bool) (string, int, bool) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", 0, false
	}
	port, err := strconv.ParseUint(s[i+1:], 16, 16)
	if err != nil {
		return "", 0, false
	}
	b, err := hex.DecodeString(s[:i])
	if err != nil {
		return "", 0, false
	}
	// Each 4-byte group is in host (little-endian) order.
	for g := 0; g+4 <= len(b); g += 4 {
		b[g], b[g+1], b[g+2], b[g+3] = b[g+3], b[g+2], b[g+1], b[g]
	}
	switch {
	case !v6 && len(b) == 4:
		return net.IP(b).String(), int(port), true
	case v6 && len(b) == 16:
		ip := net.IP(b)
		if v4 := ip.To4(); v4 != nil && !ip.Equal(net.IPv6zero) {
			return v4.String(), int(port), true
		}
		return ip.String(), int(port), true
	}
	return "", 0, false
}

// socketOwners maps socket inodes to process names visible in our PID namespace.
func socketOwners(procRoot string) map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(procRoot, e.Name(), "fd"))
		if err != nil {
			continue
		}
		var comm string
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(procRoot, e.Name(), "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
			if _, ok := out[inode]; ok {
				continue
			}
			if comm == "" {
				b, _ := os.ReadFile(filepath.Join(procRoot, e.Name(), "comm"))
				comm = strings.TrimSpace(string(b))
				if comm == "" {
					comm = "pid " + e.Name()
				}
			}
			out[inode] = comm
		}
	}
	return out
}

// childPIDs lists the direct children of pid from /proc (Linux only).
func childPIDs(pid int) (map[int]bool, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	out := map[int]bool{}
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid …  — comm may contain spaces.
		s := string(b)
		j := strings.LastIndexByte(s, ')')
		if j < 0 {
			continue
		}
		f := strings.Fields(s[j+1:])
		if len(f) < 2 {
			continue
		}
		if ppid, _ := strconv.Atoi(f[1]); ppid == pid && f[0] != "Z" {
			out[n] = true
		}
	}
	return out, true
}

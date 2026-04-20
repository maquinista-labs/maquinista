package dashboard

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// killPortOwner kills any process listening on the given TCP port on
// localhost. It reads /proc/net/tcp to find the inode, then scans
// /proc/*/fd to find the owning PID, and sends SIGKILL.
//
// Best-effort: logs but does not return errors on partial failures
// (missing /proc entries, race between discovery and kill, etc.).
func killPortOwner(port string) {
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return
	}
	inode, err := findTCPPortInode(uint16(p))
	if err != nil || inode == 0 {
		return
	}
	pid, err := findInodePID(inode)
	if err != nil || pid == 0 {
		return
	}
	log.Printf("dashboard: killing stale port-%s owner PID %d", port, pid)
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(syscall.SIGKILL)
}

// findTCPPortInode returns the socket inode for a process LISTENing on
// the given port (local address, any interface) by reading /proc/net/tcp.
func findTCPPortInode(port uint16) (uint64, error) {
	f, err := os.Open("/proc/net/tcp")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Target hex: port in big-endian network order stored as
	// little-endian in /proc/net/tcp. e.g. port 8900=0x22C4 → "C422".
	want := fmt.Sprintf("%04X", ((port&0xFF)<<8)|((port>>8)&0xFF))

	scanner := bufio.NewScanner(f)
	scanner.Scan() // skip header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		// fields[1] = local_address (hex IP:PORT, little-endian)
		// fields[3] = state (0A = LISTEN)
		// fields[9] = inode
		localAddr := fields[1]
		state := fields[3]
		inodeStr := fields[9]
		if state != "0A" {
			continue
		}
		// local_address format: "AABBCCDD:PPPP" — last 4 hex chars are port
		colonIdx := strings.LastIndex(localAddr, ":")
		if colonIdx < 0 {
			continue
		}
		addrPort := localAddr[colonIdx+1:]
		if addrPort == want {
			inode, err := strconv.ParseUint(inodeStr, 10, 64)
			if err != nil {
				continue
			}
			return inode, nil
		}
	}
	return 0, scanner.Err()
}

// findInodePID scans /proc/*/fd for a symlink whose target is
// "socket:[<inode>]" and returns the owning PID.
func findInodePID(inode uint64) (int, error) {
	target := fmt.Sprintf("socket:[%d]", inode)
	matches, err := filepath.Glob("/proc/*/fd/*")
	if err != nil {
		return 0, err
	}
	for _, fdPath := range matches {
		link, err := os.Readlink(fdPath)
		if err != nil {
			continue
		}
		if link != target {
			continue
		}
		// fdPath = /proc/<pid>/fd/<n>
		parts := strings.Split(fdPath, "/")
		if len(parts) < 3 {
			continue
		}
		pid, err := strconv.Atoi(parts[2])
		if err != nil {
			continue
		}
		return pid, nil
	}
	return 0, nil
}

// hexToBytes decodes pairs of hex digits; used only in tests.
func hexToBytes(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

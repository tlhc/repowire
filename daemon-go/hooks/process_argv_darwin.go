//go:build darwin

package hooks

import (
	"encoding/binary"
	"errors"
	"unsafe"

	"golang.org/x/sys/unix"
)

const kernProcArgs2 = 49

func processArgv(pid int) ([]string, error) {
	mib := []int32{unix.CTL_KERN, kernProcArgs2, int32(pid)}
	n := uintptr(0)
	_, _, e := unix.Syscall6(unix.SYS_SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)), 0, uintptr(unsafe.Pointer(&n)), 0, 0)
	if e != 0 || n == 0 {
		return nil, e
	}
	buf := make([]byte, n)
	_, _, e = unix.Syscall6(unix.SYS_SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0, 0)
	if e != 0 {
		return nil, e
	}
	raw := buf[:n]
	if len(raw) < 4 {
		return nil, errors.New("short procargs")
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	if i := indexByte(rest, 0); i >= 0 {
		rest = rest[i+1:]
	}
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	var args []string
	for len(args) < argc && len(rest) > 0 {
		i := indexByte(rest, 0)
		if i < 0 {
			break
		}
		args = append(args, string(rest[:i]))
		rest = rest[i+1:]
	}
	return args, nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

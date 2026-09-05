//go:build linux

package hooks

import (
	"bytes"
	"os"
	"strconv"
)

func processArgv(pid int) ([]string, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, string(p))
	}
	return out, nil
}

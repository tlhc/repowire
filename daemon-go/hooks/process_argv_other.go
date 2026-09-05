//go:build !darwin && !linux

package hooks

import "errors"

func processArgv(int) ([]string, error) {
	return nil, errors.New("process argv unsupported")
}

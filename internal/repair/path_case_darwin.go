//go:build darwin

package repair

import "syscall"

const darwinPathconfCaseSensitive = 11

func platformRepairPathCaseInsensitive(path string) bool {
	parent := existingRepairPathParent(path)
	if parent == "" {
		return false
	}
	caseSensitive, err := syscall.Pathconf(parent, darwinPathconfCaseSensitive)
	return err == nil && caseSensitive == 0
}

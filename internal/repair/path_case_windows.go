//go:build windows

package repair

import (
	"encoding/binary"
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func platformRepairPathCaseInsensitive(path string) bool {
	parent := existingRepairPathParent(path)
	if parent == "" {
		return false
	}
	f, err := os.Open(parent)
	if err != nil {
		return false
	}
	defer f.Close()

	var info [4]byte
	err = windows.GetFileInformationByHandleEx(
		windows.Handle(f.Fd()),
		windows.FileCaseSensitiveInfo,
		&info[0],
		uint32(len(info)),
	)
	if err == nil {
		return binary.LittleEndian.Uint32(info[:])&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR == 0
	}
	// Per-directory case sensitivity was added after the API itself. Systems
	// that do not implement the query retain legacy case-insensitive lookup.
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED)
}

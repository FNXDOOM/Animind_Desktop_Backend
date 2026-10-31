//go:build windows

package downloads

import "golang.org/x/sys/windows"

// freeDiskBytes reports the bytes the process could still write at dir.
func freeDiskBytes(dir string) (int64, error) {
	var callerFree, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(dir), &callerFree, &total, &totalFree); err != nil {
		return 0, err
	}
	return int64(callerFree), nil
}

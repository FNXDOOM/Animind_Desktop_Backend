//go:build !windows

package downloads

import "syscall"

// freeDiskBytes reports the bytes unprivileged processes could still write at
// dir (Bavail, not Bfree).
func freeDiskBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

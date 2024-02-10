package netns

import (
	"fmt"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// fuseSuperMagic is defined in statfs(2)
const fuseSuperMagic = 0x65735546

func isFUSE(dir string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false
	}
	return st.Type == fuseSuperMagic
}

func unmount(target string, flags int) error {
	if isFUSE(target) {
		if err := unmountFUSE(target); err == nil {
			return nil
		}
	}
	for i := 0; i < 50; i++ {
		if err := unix.Unmount(target, flags); err != nil {
			switch err {
			case unix.EBUSY:
				time.Sleep(50 * time.Millisecond)
				continue
			default:
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("failed to unmount target %s: %w", target, unix.EBUSY)
}

// Unmount the provided mount path with the flags
func Unmount(target string, flags int) error {
	if err := unmount(target, flags); err != nil && err != unix.EINVAL {
		return err
	}
	return nil
}

func unmountFUSE(target string) error {
	var err error
	for _, helperBinary := range []string{"fusermount3", "fusermount"} {
		cmd := exec.Command(helperBinary, "-u", target)
		err = cmd.Run()
		if err == nil {
			return nil
		}
	}
	return err
}

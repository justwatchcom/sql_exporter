package compilation

import (
	"debug/elf"
	"fmt"
	"runtime"
	"sync"
)

// LinkingMode describes what linking mode was detected for the current binary.
type LinkingMode int

const (
	// StaticLinking means the static linking.
	StaticLinking LinkingMode = iota
	// DynamicLinking means the dynamic linking.
	DynamicLinking
	// UnknownLinking means driver couldn't determine linking or it is not relevant (it is relevant on Linux only).
	UnknownLinking
)

func (lm *LinkingMode) String() string {
	switch *lm {
	case StaticLinking:
		return "static"
	case DynamicLinking:
		return "dynamic"
	default:
		return "unknown"
	}
}

// CheckDynamicLinking checks whether the current binary has a dynamic linker (PT_INTERP).
// A statically linked glibc binary will crash with SIGFPE if dlopen is called,
// so this check allows us to skip minicore loading gracefully.
// The result is cached so the ELF parsing only happens once.
func CheckDynamicLinking() (LinkingMode, error) {
	linkingModeOnce.Do(func() {
		if runtime.GOOS != "linux" {
			linkingModeCached = UnknownLinking
			return
		}
		f, err := elf.Open("/proc/self/exe")
		if err != nil {
			linkingModeErr = fmt.Errorf("cannot open /proc/self/exe: %v", err)
			return
		}
		defer func() {
			_ = f.Close()
		}()
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				linkingModeCached = DynamicLinking
				return
			}
		}
		linkingModeCached = StaticLinking
	})
	return linkingModeCached, linkingModeErr
}

var (
	linkingModeOnce   sync.Once
	linkingModeCached LinkingMode
	linkingModeErr    error
)

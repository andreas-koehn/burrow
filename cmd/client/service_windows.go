package main

import (
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/ankoehn/burrow/cmd/client/svc"
)

// isElevated reports whether this process runs with an administrator's full
// rights.
func isElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// knownFolder asks Windows where a folder is. The environment is not asked:
// a variable such as %ProgramData% is the user's to set, and an elevated
// process inherits it.
func knownFolder(id *windows.KNOWNFOLDERID) string {
	p, err := windows.KnownFolderPath(id, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return ""
	}
	return p
}

// executableTrusted reports whether the binary at exe is below Program Files,
// which a normal user cannot write to. The service runs as LocalSystem and
// must not start a file such a user can replace.
func executableTrusted(exe string) bool {
	return svc.UnderAny(exe, []string{
		knownFolder(windows.FOLDERID_ProgramFiles),
		knownFolder(windows.FOLDERID_ProgramFilesX86),
		knownFolder(windows.FOLDERID_ProgramFilesX64),
	})
}

// programDataDir is the machine's ProgramData directory.
func programDataDir() string { return knownFolder(windows.FOLDERID_ProgramData) }

// privateDirSDDL gives full access to SYSTEM and to Administrators, to their
// files and directories below too, and takes nothing over from the parent:
// under ProgramData that would let every user of the machine read.
const privateDirSDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// makePrivateDir creates a directory that only SYSTEM and Administrators can
// use. The access rules are part of the creation: there is no moment at which
// the directory exists with wider ones. It fails when the directory exists.
func makePrivateDir(path string) error {
	sd, err := windows.SecurityDescriptorFromString(privateDirSDDL)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return windows.CreateDirectory(p, sa)
}

package main

import (
	"os"
	"syscall"
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

// pathTrusted reports whether the file at path is below Program Files, which
// a normal user cannot write to. The service runs as LocalSystem and must
// not start a file such a user can replace. It is asked about the binary
// only: everything else the service reads is copied into its own directory.
func pathTrusted(path string) bool {
	return svc.UnderAny(path, []string{
		knownFolder(windows.FOLDERID_ProgramFiles),
		knownFolder(windows.FOLDERID_ProgramFilesX86),
		knownFolder(windows.FOLDERID_ProgramFilesX64),
	})
}

// programDataDir is the machine's ProgramData directory.
func programDataDir() string { return knownFolder(windows.FOLDERID_ProgramData) }

// isLink reports whether fi, from Lstat, is a reparse point: a symbolic link,
// a junction or a mount point, rather than a file or a directory of its own.
func isLink(fi os.FileInfo) bool {
	if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return true
	}
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	return !ok || d.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// privateDirSDDL makes Administrators the owner (group: SYSTEM), gives full
// access to SYSTEM and to Administrators, to their files and directories
// below too, and takes nothing over from the parent: under ProgramData that
// would let every user of the machine read. The owner is named because an
// owner may always rewrite the access rules: it must not be whoever happened
// to create the directory.
const privateDirSDDL = "O:BAG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

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

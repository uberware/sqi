// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package configinit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileID identifies a file or directory whatever path reaches it: its
// volume's serial number and its file ID on that volume.
type fileID struct {
	volume uint64
	file   [16]byte
}

// fileIDInfo is FILE_ID_INFO, which GetFileInformationByHandleEx fills for
// the FileIdInfo class.
type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

// identities is the identity of path itself (a junction or symbolic link at
// path is not followed) and, when path is a link that resolves, of what it
// resolves to. ok is false when path does not exist.
func identities(path string) (ids []fileID, ok bool, err error) {
	const plain = windows.FILE_FLAG_BACKUP_SEMANTICS
	for _, flags := range []uint32{plain | windows.FILE_FLAG_OPEN_REPARSE_POINT, plain} {
		id, err := openID(path, flags)
		switch {
		case errors.Is(err, fs.ErrNotExist) && len(ids) == 0:
			return nil, false, nil
		case errors.Is(err, fs.ErrNotExist):
			// A link whose target is missing: only its own identity.
		case err != nil:
			return nil, false, fmt.Errorf("identify %s: %w", path, err)
		default:
			ids = append(ids, id)
		}
	}
	return ids, true, nil
}

func openID(path string, flags uint32) (fileID, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fileID{}, err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return fileID{}, err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // only its identity was needed
	var info fileIDInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileIdInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err == nil {
		return fileID{volume: info.VolumeSerialNumber, file: info.FileID}, nil
	}
	// A file system without 128-bit IDs: the 64-bit file index.
	var bh windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bh); err != nil {
		return fileID{}, err
	}
	id := fileID{volume: uint64(bh.VolumeSerialNumber)}
	binary.LittleEndian.PutUint32(id.file[0:], bh.FileIndexLow)
	binary.LittleEndian.PutUint32(id.file[4:], bh.FileIndexHigh)
	return id, nil
}

// sameFile reports whether any identity in a is one in b.
func sameFile(a, b []fileID) bool {
	return slices.ContainsFunc(a, func(x fileID) bool { return slices.Contains(b, x) })
}

// isDefaultDir reports whether dir is pd\sqi (pd is %ProgramData%), the
// directory every service shares, by spelling or by identity: dir, if it
// exists, against pd\sqi; otherwise dir's parent against pd and its name
// against "sqi".
func isDefaultDir(dir, pd string) (bool, error) {
	def := filepath.Join(pd, "sqi")
	if strings.EqualFold(dir, def) {
		return true, nil
	}
	ids, ok, err := identities(dir)
	if err != nil {
		return false, err
	}
	if ok {
		defIDs, defOK, err := identities(def)
		return defOK && sameFile(ids, defIDs), err
	}
	if !strings.EqualFold(filepath.Base(dir), "sqi") {
		return false, nil
	}
	return sameDir(filepath.Dir(dir), pd)
}

// isUnderDir reports whether dir is pd or inside it: by spelling, or by
// identity, comparing pd with each directory on the path dir (or its nearest
// existing ancestor) resolves to with every junction and symbolic link
// followed. A directory that cannot be opened counts as inside: the answer
// only adds a requirement.
func isUnderDir(dir, pd string) (bool, error) {
	if underDir(dir, pd) {
		return true, nil
	}
	pdIDs, ok, err := identities(pd)
	if err != nil || !ok {
		return false, err
	}
	final, err := finalExisting(dir)
	switch {
	case errors.Is(err, fs.ErrPermission):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("resolve %s: %w", dir, err)
	}
	for p := final; ; p = filepath.Dir(p) {
		ids, ok, err := identities(p)
		switch {
		case errors.Is(err, fs.ErrPermission):
			return true, nil
		case err != nil:
			return false, err
		case ok && sameFile(ids, pdIDs):
			return true, nil
		}
		if filepath.Dir(p) == p {
			return false, nil
		}
	}
}

// finalExisting is the path that dir, or its nearest existing ancestor,
// resolves to with every junction and symbolic link followed.
func finalExisting(dir string) (string, error) {
	for p := dir; ; p = filepath.Dir(p) {
		final, err := finalPath(p)
		if err == nil || !errors.Is(err, fs.ErrNotExist) || filepath.Dir(p) == p {
			return final, err
		}
	}
}

// finalPath is GetFinalPathNameByHandle of path, links followed, without
// its \\?\ prefix.
func finalPath(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // only its name was needed
	final, err := growBuffer(func(buf []uint16, size uint32) (uint32, error) {
		return windows.GetFinalPathNameByHandle(h, &buf[0], size, 0)
	})
	if err != nil {
		return "", err
	}
	if rest, ok := strings.CutPrefix(final, `\\?\UNC\`); ok {
		return `\\` + rest, nil
	}
	return strings.TrimPrefix(final, `\\?\`), nil
}

// sameDir reports whether a and b both exist and are one directory.
func sameDir(a, b string) (bool, error) {
	aIDs, aOK, err := identities(a)
	if err != nil || !aOK {
		return false, err
	}
	bIDs, bOK, err := identities(b)
	return bOK && sameFile(aIDs, bIDs), err
}

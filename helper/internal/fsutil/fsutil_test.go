// SPDX-License-Identifier: AGPL-3.0-or-later

package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAPrivateFileIsMadeOnlyTheUsersAndALinkInItsPlaceIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "helper.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenPrivate(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Errorf("mode %v, %v", info.Mode(), err)
	}

	if runtime.GOOS == "windows" {
		t.Skip("links aren't refused on Windows")
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(elsewhere, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, "planted.log")
	dangling := filepath.Join(dir, "dangling.log")
	if err := os.Symlink(elsewhere, planted); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "nothing-yet"), dangling); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{planted, dangling} {
		if f, err := OpenPrivate(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE); err == nil {
			f.Close()
			t.Errorf("%s was opened through its link", filepath.Base(p))
		}
	}
	if data, _ := os.ReadFile(elsewhere); string(data) != "theirs\n" {
		t.Errorf("written through: %q", data)
	}
	if info, _ := os.Stat(elsewhere); info.Mode().Perm() != 0o644 {
		t.Errorf("the link's target was chmodded: %v", info.Mode())
	}
	if _, err := os.Lstat(filepath.Join(dir, "nothing-yet")); !os.IsNotExist(err) {
		t.Error("a file was made where the dangling link pointed")
	}
	if f, err := OpenPrivate(dir, os.O_RDONLY); err == nil {
		f.Close()
		t.Error("a folder was opened as a file")
	}
}

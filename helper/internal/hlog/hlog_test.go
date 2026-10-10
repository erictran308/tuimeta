// SPDX-License-Identifier: AGPL-3.0-or-later

package hlog

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTheLogIsntWrittenThroughALinkPlantedInItsPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links aren't refused on Windows")
	}
	defer SetOutput(io.Discard)
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "helper.log")
	if err := os.Symlink(target, log); err != nil {
		t.Fatal(err)
	}
	if c, err := Open(log); err == nil {
		c.Close()
		t.Fatal("the log was opened through a link")
	}
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	c, err := Open(log)
	if err != nil {
		t.Fatal(err)
	}
	Info("starting")
	c.Close()
	SetOutput(io.Discard)
	if data, _ := os.ReadFile(log); !strings.Contains(string(data), "INFO starting") {
		t.Errorf("the log: %q", data)
	}
	if data, _ := os.ReadFile(target); len(data) != 0 {
		t.Errorf("written through the link: %q", data)
	}
}

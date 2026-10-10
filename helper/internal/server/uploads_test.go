// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/proto"
)

// folders makes a shared folder holding the file someone left for you to
// send, and a private one holding a file of yours.
func folders(t *testing.T) (decoy, secret string) {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"shared", "private"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	decoy = filepath.Join(dir, "shared", "report.pdf")
	secret = filepath.Join(dir, "private", "id_ed25519")
	if err := os.WriteFile(decoy, []byte("%PDF-1.4 dummy shared report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("DUMMY-PRIVATE-KEY-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	return decoy, secret
}

func listed(t *testing.T, path string) UploadFile {
	t.Helper()
	f, err := Listed(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func refusedAsChanged(t *testing.T, want UploadFile) {
	t.Helper()
	u, err := readUpload(want)
	if err != errChanged {
		t.Fatalf("read %q (%v), want it refused as changed", u.Data, err)
	}
}

func TestTheFileListedIsReadAsItWas(t *testing.T) {
	decoy, _ := folders(t)
	u, err := readUpload(listed(t, decoy))
	if err != nil {
		t.Fatal(err)
	}
	if string(u.Data) != "%PDF-1.4 dummy shared report" || u.Name != "report.pdf" || u.Mime != "application/pdf" {
		t.Errorf("read %q as %s (%s)", u.Data, u.Name, u.Mime)
	}
}

func TestALinkPutInTheListedFilesPlaceIsNotFollowed(t *testing.T) {
	decoy, secret := folders(t)
	want := listed(t, decoy)
	// After tuimeta's check on Enter, before the helper reads it.
	if err := os.Rename(decoy, decoy+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, decoy); err != nil {
		t.Fatal(err)
	}
	refusedAsChanged(t, want)
	// Even one whose target has the listed file's size and times.
	link := listed(t, secret)
	link.Path = decoy
	refusedAsChanged(t, link)
}

func TestAnotherFileRenamedOverTheListedOneIsRefused(t *testing.T) {
	decoy, _ := folders(t)
	want := listed(t, decoy)
	// The same size and modification time: only its inode tells it apart.
	other := decoy + ".other"
	if err := os.WriteFile(other, []byte("%PDF-1.4 another's report!!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(0, want.MtimeNs)
	if err := os.Chtimes(other, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, decoy); err != nil {
		t.Fatal(err)
	}
	if got := listed(t, decoy); got.Size != want.Size || got.MtimeNs != want.MtimeNs {
		t.Fatalf("the stand-in differs in more than its inode: %+v vs %+v", got, want)
	}
	refusedAsChanged(t, want)
}

func TestAFileRewrittenInPlaceWithItsOldTimeIsRefused(t *testing.T) {
	decoy, _ := folders(t)
	want := listed(t, decoy)
	if want.CtimeNs == 0 {
		t.Skip("this system has no status-change time")
	}
	time.Sleep(20 * time.Millisecond) // past the clock's tick, for ctime to move
	// The same size, and its old modification time put back: only the
	// status-change time tells.
	if err := os.WriteFile(decoy, []byte("%PDF-1.4 rewritten in place!"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(0, want.MtimeNs)
	if err := os.Chtimes(decoy, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	refusedAsChanged(t, want)
}

func TestAFolderOrAnEditedFileInTheListedPlaceIsRefused(t *testing.T) {
	decoy, _ := folders(t)
	want := listed(t, decoy)
	if err := os.WriteFile(decoy, []byte("%PDF-1.4 dummy shared report, longer now"), 0o600); err != nil {
		t.Fatal(err)
	}
	refusedAsChanged(t, want)
	if err := os.Remove(decoy); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(decoy, 0o700); err != nil {
		t.Fatal(err)
	}
	refusedAsChanged(t, want)
}

func TestAnIdentityTuimetaCouldntTellIsntCompared(t *testing.T) {
	decoy, _ := folders(t)
	want := listed(t, decoy)
	want.Dev, want.Ino, want.CtimeNs = 0, 0, 0 // as on Windows
	if _, err := readUpload(want); err != nil {
		t.Errorf("refused: %v", err)
	}
	want.MtimeNs++
	refusedAsChanged(t, want)
}

func TestOnlyThisMachinesOwnPathsAreRead(t *testing.T) {
	cases := map[string]bool{
		`\\server\share\report.pdf`:   true,
		`//server/share/report.pdf`:   true,
		`\/server\share\report.pdf`:   true,
		`\\?\UNC\server\share\r.pdf`:  true,
		`\\.\PhysicalDrive0`:          true,
		`\\?\Volume{0f}\r.pdf`:        true,
		`\\?\`:                        true,
		`\??\C:\Users\me\r.pdf`:       true,
		`/??/C:/Users/me/r.pdf`:       true,
		`\\?\C:\Users\me\report.pdf`:  false,
		`\\?\d:\report.pdf`:           false,
		`C:\Users\me\report.pdf`:      false,
		`/Users/me/report.pdf`:        false,
		`/home/me/??/report.pdf`:      false,
		`/home/me/\\server\share.pdf`: false,
	}
	for path, want := range cases {
		if got := onAnotherMachine(path); got != want {
			t.Errorf("onAnotherMachine(%q) = %v, want %v", path, got, want)
		}
	}
	for _, path := range []string{`\\server\share\report.pdf`, `\\?\UNC\server\share\r.pdf`} {
		if _, err := readUpload(UploadFile{Path: path, Size: 1}); err == nil || err.(*proto.Error).Code != proto.BadRequest {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestASwapAfterTuimetasCheckSendsNothing(t *testing.T) {
	h := start(t)
	decoy, secret := folders(t)
	want := listed(t, decoy)
	if err := os.Rename(decoy, decoy+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, decoy); err != nil {
		t.Fatal(err)
	}
	from := h.c.Mark()
	l := h.c.Call("send_files", map[string]any{"chat_id": h.stub.chatID, "files": []UploadFile{want}})
	if l.Error == nil || l.Error.Code != proto.BadRequest || l.Error.Message != "A file changed since it was attached; attach it again." {
		t.Fatalf("got %s", l.Raw)
	}
	for _, line := range h.c.Lines()[from:] {
		if line.Event == "message" {
			t.Errorf("a pending message went out: %s", line.Raw)
		}
	}
	if n := h.stub.sends(); n != 0 {
		t.Errorf("the backend was handed %d sends", n)
	}

	// Put back and attached again (moving it changed it), it goes.
	if err := os.Remove(decoy); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(decoy+".real", decoy); err != nil {
		t.Fatal(err)
	}
	want = listed(t, decoy)
	if l := h.c.Call("send_files", map[string]any{"chat_id": h.stub.chatID, "files": []UploadFile{want}}); l.Error != nil {
		t.Fatalf("the listed file itself: %s", l.Raw)
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.stub.sends() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	h.stub.mu.Lock()
	defer h.stub.mu.Unlock()
	if len(h.stub.sent) != 1 || string(h.stub.sent[0].Files[0].Data) != "%PDF-1.4 dummy shared report" {
		t.Errorf("sent %d", len(h.stub.sent))
	}
}

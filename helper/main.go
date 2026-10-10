// SPDX-License-Identifier: AGPL-3.0-or-later

// Command tuimeta-helper speaks Messenger, Instagram and WhatsApp for tuimeta, which
// runs it as a child process and talks to it in newline-delimited JSON over
// stdin and stdout (see PROTOCOL.md).
//
//	tuimeta-helper --data-dir <dir> [--fake]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/erictran308/tuimeta/helper/internal/backend"
	"github.com/erictran308/tuimeta/helper/internal/fake"
	"github.com/erictran308/tuimeta/helper/internal/fsutil"
	"github.com/erictran308/tuimeta/helper/internal/hlog"
	"github.com/erictran308/tuimeta/helper/internal/ids"
	"github.com/erictran308/tuimeta/helper/internal/instagram"
	"github.com/erictran308/tuimeta/helper/internal/messenger"
	"github.com/erictran308/tuimeta/helper/internal/proto"
	"github.com/erictran308/tuimeta/helper/internal/server"
	"github.com/erictran308/tuimeta/helper/internal/whatsapp"
)

// Version is the helper's own version, which hello reports.
const Version = "0.1.1"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, quietStderr))
}

type options struct {
	dataDir string
	fake    bool
}

func parseArgs(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("tuimeta-helper", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.dataDir, "data-dir", "", "")
	fs.BoolVar(&o.fake, "fake", false, "")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, errors.New("unexpected arguments")
	}
	if o.dataDir == "" {
		return o, errors.New("--data-dir is required")
	}
	if !filepath.IsAbs(o.dataDir) {
		return o, errors.New("--data-dir must be an absolute path")
	}
	return o, nil
}

const usage = "usage: tuimeta-helper --data-dir <absolute dir> [--fake]"

// run is the helper. Startup problems are said on stderr in one line; once
// the helper is serving, quiet (which sends stderr nowhere) has run and only
// helper.log hears anything.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, quiet func()) (code int) {
	defer func() {
		if v := recover(); v != nil {
			hlog.Recovered("main", v)
			code = 2
		}
	}()
	o, err := parseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(stderr, usage)
		return 0
	} else if err != nil {
		fmt.Fprintf(stderr, "tuimeta-helper: %v\n%s\n", err, usage)
		return 2
	}
	restrictUmask()
	if err := dataDir(o.dataDir); err != nil {
		fmt.Fprintf(stderr, "tuimeta-helper: data dir: %v\n", err)
		return 1
	}
	logFile, err := hlog.Open(filepath.Join(o.dataDir, "helper.log"))
	if err != nil {
		fmt.Fprintf(stderr, "tuimeta-helper: can't open helper.log: %v\n", err)
		return 1
	}
	defer logFile.Close()
	mode := "real"
	if o.fake {
		mode = "fake"
	}
	hlog.Info("starting", hlog.Str("version", Version), hlog.Str("mode", mode))
	store, err := ids.Open(filepath.Join(o.dataDir, "ids.json"))
	if err != nil {
		hlog.Error("can't read ids", hlog.Kind(err))
		fmt.Fprintln(stderr, "tuimeta-helper: can't read ids.json in the data dir")
		return 1
	}
	if quiet != nil {
		quiet()
	}

	// Closing the terminal or being told to stop ends the helper as stdin
	// closing does: quietly. A reader gone from stdout is an error to note,
	// not a reason to die before disconnecting.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	signal.Ignore(syscall.SIGPIPE)

	filesDir := "files"
	if o.fake {
		filesDir = "fake"
	}
	srv := server.New(server.Config{DataDir: o.dataDir, HelperVersion: Version, FilesDir: filesDir}, store, stdout)
	for _, n := range proto.Networks {
		switch {
		case o.fake:
			srv.Add(fake.New(n, srv.Deps(n), time.Now()))
		case n == proto.Messenger:
			srv.Add(messenger.New(srv.Deps(n)))
		case n == proto.Instagram:
			srv.Add(instagram.New(srv.Deps(n)))
		case n == proto.WhatsApp:
			srv.Add(whatsapp.New(srv.Deps(n)))
		default:
			srv.Add(&backend.Unavailable{Net: n, Events: srv.Events})
		}
	}
	srv.Run(ctx, stdin)
	hlog.Info("stopped")
	return 0
}

// dataDir makes the data folder private: created 0700 if missing, its mode
// fixed if not, and refused if it's a link or someone else's.
func dataDir(dir string) error {
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("it is a symbolic link")
		}
		if !info.IsDir() {
			return errors.New("it isn't a folder")
		}
	}
	if err := fsutil.PrivateDir(dir); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !ownedByMe(info) {
		return errors.New("it belongs to another user")
	}
	return nil
}

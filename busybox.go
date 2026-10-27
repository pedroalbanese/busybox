// SPDX-License-Identifier: ISC
//
// Copyright (c) 2026 Pedro F. Albanese
//
// Permission to use, copy, modify, and/or distribute this software for any
// purpose with or without fee is hereby granted, provided that the above
// copyright notice and this permission notice appear in all copies.
//
// THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
// WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
// ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
// WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
// ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
// OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
//
// ---------------------------------------------------------------------------
//
// busybox.go — Complete BusyBox in pure Go
//
// Build:
//   go build -ldflags="-s -w" -o busybox busybox.go
//   GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o busybox.exe busybox.go
//
// Usage:
//   busybox <applet> [args...]
//   busybox --list
//   busybox --help [applet]
//   ln -s busybox ls && ./ls -la

package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ===========================================================================
// Applet registry
// ===========================================================================

type appletFunc func(args []string) int

// Map declared empty; filled in init() to break the cycle
// applets -> appletSh -> runScript -> runPipeline -> runCommandWithRedir -> applets.
var applets = map[string]appletFunc{}

func init() {
	applets = map[string]appletFunc{
		// files
		"ls": appletLs, "cp": appletCp, "mv": appletMv, "rm": appletRm,
		"mkdir": appletMkdir, "rmdir": appletRmdir, "touch": appletTouch,
		"find": appletFind, "du": appletDu, "df": appletDf, "dd": appletDd,
		"tar": appletTar, "gzip": appletGzip, "zip": appletZip, "unzip": appletUnzip,
		"stat": appletStat,
		"chmod": appletChmod, "chown": appletChown, "ln": appletLn,
		"readlink": appletReadlink, "split": appletSplit, "hexedit": appletHexedit,
		// text
		"cat": appletCat, "head": appletHead, "tail": appletTail, "wc": appletWc,
		"grep": appletGrep, "sort": appletSort, "uniq": appletUniq, "tr": appletTr,
		"cut": appletCut, "tee": appletTee, "sed": appletSed, "awk": appletAwk,
		"diff": appletDiff, "patch": appletPatch, "strings": appletStrings, "nl": appletNl,
		// system
		"pwd": appletPwd, "env": appletEnv, "sleep": appletSleep, "uname": appletUname,
		"whoami": appletWhoami, "hostname": appletHostname, "date": appletDate,
		"id": appletId, "which": appletWhich, "top": appletTop,
		"cal": appletCal,
		// arithmetic
		"expr": appletExpr, "bc": appletBc,
		// hash / encoding
		"sha256sum": appletSha256sum, "md5sum": appletMd5sum, "sha1sum": appletSha1sum,
		"cksum": appletCksum, "base64": appletBase64, "hexdump": appletHexdump,
		// network
		"wget": appletWget, "ping": appletPing, "netstat": appletNetstat,
		"nc": appletNc, "ifconfig": appletIfconfig, "route": appletRoute,
		// utilities
		"echo": appletEcho, "printf": appletPrintf, "true": appletTrue,
		"false": appletFalse, "yes": appletYes, "basename": appletBasename,
		"dirname": appletDirname, "seq": appletSeq, "xargs": appletXargs,
		"sh": appletSh,
	}
}

func main() {
	prog := filepath.Base(os.Args[0])
	if prog == "busybox" || prog == "busybox.exe" {
		if len(os.Args) < 2 {
			printUsage()
			os.Exit(1)
		}
		name := os.Args[1]
		switch name {
		case "--help", "-h", "help":
			if len(os.Args) >= 3 {
				if fn, ok := applets[os.Args[2]]; ok {
					os.Exit(fn([]string{"-h"}))
				}
				fmt.Fprintf(os.Stderr, "busybox: unknown applet: %s\n", os.Args[2])
				os.Exit(127)
			}
			printUsage()
			os.Exit(0)
		case "--list", "-l":
			listApplets()
			os.Exit(0)
		}
		fn, ok := applets[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "busybox: unknown applet: %s\n", name)
			os.Exit(127)
		}
		os.Exit(fn(os.Args[2:]))
	}
	if fn, ok := applets[prog]; ok {
		os.Exit(fn(os.Args[1:]))
	}
	fmt.Fprintf(os.Stderr, "busybox: unknown invocation name: %s\n", prog)
	os.Exit(127)
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage: busybox <applet> [args...]")
	fmt.Fprintln(os.Stderr, "       busybox --list")
	fmt.Fprintln(os.Stderr, "       busybox --help [applet]")
}

func listApplets() {
	names := make([]string, 0, len(applets))
	for n := range applets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Println(n)
	}
}

// ===========================================================================
// Helpers
// ===========================================================================

func fail(format string, a ...interface{}) int {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	return 1
}

func openInput(name string) (io.ReadCloser, error) {
	if name == "" || name == "-" {
		return io.NopCloser(os.Stdin), nil
	}
	return os.Open(name)
}

func openOutput(name string) (io.WriteCloser, error) {
	if name == "" || name == "-" {
		return nopWriteCloser{os.Stdout}, nil
	}
	return os.Create(name)
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func readLines(name string) ([]string, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, strings.TrimSuffix(sc.Text(), "\r"))
	}
	return lines, sc.Err()
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ===========================================================================
// Applets: files
// ===========================================================================

func appletLs(args []string) int {
	long, all := false, false
	var paths []string
	for _, a := range args {
		switch a {
		case "-l":
			long = true
		case "-a":
			all = true
		case "-la", "-al":
			long, all = true, true
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "Usage: ls [-l] [-a] [paths...]")
			return 0
		default:
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				return fail("ls: invalid option: %s", a)
			}
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	rc := 0
	multi := len(paths) > 1
	for pi, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ls: %s: %v\n", p, err)
			rc = 1
			continue
		}
		if multi {
			if pi > 0 {
				fmt.Println()
			}
			fmt.Printf("%s:\n", p)
		}
		if !info.IsDir() {
			printEntry(p, info, long)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ls: %s: %v\n", p, err)
			rc = 1
			continue
		}
		var names []string
		for _, e := range entries {
			n := e.Name()
			if !all && strings.HasPrefix(n, ".") {
				continue
			}
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			full := filepath.Join(p, n)
			fi, err := os.Stat(full)
			if err != nil {
				continue
			}
			printEntry(full, fi, long)
		}
	}
	return rc
}

func printEntry(path string, info os.FileInfo, long bool) {
	if !long {
		fmt.Println(filepath.Base(path))
		return
	}
	fmt.Printf("%s %10d %s %s\n", info.Mode().String(), info.Size(),
		info.ModTime().Format("2006-01-02 15:04"), filepath.Base(path))
}

func appletCp(args []string) int {
	if len(args) < 2 {
		return fail("cp: usage: cp source destination")
	}
	src, dst := args[0], args[1]
	in, err := os.Open(src)
	if err != nil {
		return fail("cp: %v", err)
	}
	defer in.Close()
	info, _ := in.Stat()
	if di, err := os.Stat(dst); err == nil && di.IsDir() {
		dst = filepath.Join(dst, filepath.Base(src))
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return fail("cp: %v", err)
	}
	defer out.Close()
	io.Copy(out, in)
	return 0
}

func appletMv(args []string) int {
	if len(args) < 2 {
		return fail("mv: usage: mv source destination")
	}
	if err := os.Rename(args[0], args[1]); err != nil {
		if rc := appletCp(args); rc != 0 {
			return rc
		}
		if err := os.Remove(args[0]); err != nil {
			return fail("mv: %v", err)
		}
	}
	return 0
}

func appletRm(args []string) int {
	recursive, force := false, false
	var paths []string
	for _, a := range args {
		switch a {
		case "-r", "-R":
			recursive = true
		case "-f":
			force = true
		case "-rf", "-fr":
			recursive, force = true, true
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "Usage: rm [-r] [-f] files...")
			return 0
		default:
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				return fail("rm: invalid option: %s", a)
			}
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		if force {
			return 0
		}
		return fail("rm: usage: rm [-r] [-f] files...")
	}
	rc := 0
	for _, p := range paths {
		var err error
		if recursive {
			err = os.RemoveAll(p)
		} else {
			err = os.Remove(p)
		}
		if err != nil && !force {
			fmt.Fprintf(os.Stderr, "rm: %s: %v\n", p, err)
			rc = 1
		}
	}
	return rc
}

func appletMkdir(args []string) int {
	parents := false
	var dirs []string
	for _, a := range args {
		if a == "-p" {
			parents = true
			continue
		}
		if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: mkdir [-p] dirs...")
			return 0
		}
		dirs = append(dirs, a)
	}
	if len(dirs) == 0 {
		return fail("mkdir: usage: mkdir [-p] dirs...")
	}
	rc := 0
	for _, d := range dirs {
		var err error
		if parents {
			err = os.MkdirAll(d, 0o755)
		} else {
			err = os.Mkdir(d, 0o755)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "mkdir: %s: %v\n", d, err)
			rc = 1
		}
	}
	return rc
}

func appletRmdir(args []string) int {
	if len(args) == 0 {
		return fail("rmdir: usage: rmdir dirs...")
	}
	rc := 0
	for _, d := range args {
		if d == "-h" || d == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: rmdir dirs...")
			return 0
		}
		if err := os.Remove(d); err != nil {
			fmt.Fprintf(os.Stderr, "rmdir: %s: %v\n", d, err)
			rc = 1
		}
	}
	return rc
}

func appletTouch(args []string) int {
	if len(args) == 0 {
		return fail("touch: usage: touch files...")
	}
	rc := 0
	now := time.Now()
	for _, p := range args {
		if p == "-h" || p == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: touch files...")
			return 0
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "touch: %s: %v\n", p, err)
			rc = 1
			continue
		}
		f.Close()
		os.Chtimes(p, now, now)
	}
	return rc
}

func appletFind(args []string) int {
	root := "."
	pattern := ""
	i := 0
	if i < len(args) && !strings.HasPrefix(args[i], "-") {
		root = args[i]
		i++
	}
	for i < len(args) {
		if args[i] == "-h" || args[i] == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: find [dir] [-name pattern]")
			return 0
		}
		if args[i] == "-name" && i+1 < len(args) {
			pattern = args[i+1]
			i += 2
			continue
		}
		i++
	}
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if pattern == "" {
			fmt.Println(path)
			return nil
		}
		if ok, _ := filepath.Match(pattern, filepath.Base(path)); ok {
			fmt.Println(path)
		}
		return nil
	})
	return 0
}

func appletDu(args []string) int {
	human := false
	var paths []string
	for _, a := range args {
		switch a {
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "Usage: du [-h] [-s] [paths...]")
			return 0
		case "-s":
		default:
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	for _, p := range paths {
		size, err := dirSize(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "du: %s: %v\n", p, err)
			continue
		}
		if human {
			fmt.Printf("%s\t%s\n", humanSize(size), p)
		} else {
			fmt.Printf("%d\t%s\n", size/1024, p)
		}
	}
	return 0
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func appletDf(args []string) int {
	wd, _ := os.Getwd()
	fmt.Println("Filesystem     1K-blocks  Used  Available  Use%  Mounted on")
	fmt.Printf("(unknown)              0     0          0     -  %s\n", wd)
	return 0
}

func appletDd(args []string) int {
	inFile, outFile := "", ""
	bs := 512
	count := -1
	skip := 0
	seek := 0
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: dd if=... of=... [bs=N] [count=N] [skip=N] [seek=N]")
			return 0
		}
		kv := strings.SplitN(a, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k, v := kv[0], kv[1]
		switch k {
		case "if":
			inFile = v
		case "of":
			outFile = v
		case "bs":
			n, err := parseSize(v)
			if err != nil {
				return fail("dd: invalid bs: %s", v)
			}
			bs = n
		case "count":
			n, err := strconv.Atoi(v)
			if err != nil {
				return fail("dd: invalid count: %s", v)
			}
			count = n
		case "skip":
			n, _ := strconv.Atoi(v)
			skip = n
		case "seek":
			n, _ := strconv.Atoi(v)
			seek = n
		}
	}
	var in io.Reader = os.Stdin
	if inFile != "" {
		f, err := os.Open(inFile)
		if err != nil {
			return fail("dd: %v", err)
		}
		defer f.Close()
		in = f
	}
	var out io.Writer = os.Stdout
	if outFile != "" {
		f, err := os.Create(outFile)
		if err != nil {
			return fail("dd: %v", err)
		}
		defer f.Close()
		out = f
	}
	if skip > 0 {
		io.CopyN(io.Discard, in, int64(skip*bs))
	}
	if seek > 0 {
		if f, ok := out.(*os.File); ok {
			f.Seek(int64(seek*bs), io.SeekStart)
		}
	}
	buf := make([]byte, bs)
	var total int64
	blocks := 0
	for {
		if count >= 0 && blocks >= count {
			break
		}
		n, err := io.ReadFull(in, buf)
		if n > 0 {
			out.Write(buf[:n])
			total += int64(n)
			blocks++
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "dd: %v\n", err)
			break
		}
	}
	fmt.Fprintf(os.Stderr, "%d bytes copied\n", total)
	return 0
}

func parseSize(s string) (int, error) {
	mult := 1
	if len(s) > 0 {
		switch s[len(s)-1] {
		case 'k', 'K':
			mult, s = 1024, s[:len(s)-1]
		case 'm', 'M':
			mult, s = 1024*1024, s[:len(s)-1]
		case 'g', 'G':
			mult, s = 1024*1024*1024, s[:len(s)-1]
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

func appletTar(args []string) int {
	if len(args) == 0 {
		return fail("tar: usage: tar -c|-x|-t [-z] -f file [files...]")
	}
	create, extract, list, gz := false, false, false, false
	var archive string
	var files []string
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-c":
			create, i = true, i+1
		case a == "-x":
			extract, i = true, i+1
		case a == "-t":
			list, i = true, i+1
		case a == "-z":
			gz, i = true, i+1
		case a == "-f":
			if i+1 >= len(args) {
				return fail("tar: -f requires an argument")
			}
			archive, i = args[i+1], i+2
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stderr, "Usage: tar -c|-x|-t [-z] -f file [files...]")
			return 0
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for _, c := range a[1:] {
				switch c {
				case 'c':
					create = true
				case 'x':
					extract = true
				case 't':
					list = true
				case 'z':
					gz = true
				}
			}
			i++
		default:
			files, i = append(files, a), i+1
		}
	}
	if archive == "" {
		return fail("tar: -f file required")
	}
	switch {
	case create:
		return tarCreate(archive, files, gz)
	case extract:
		return tarExtract(archive, gz)
	case list:
		return tarList(archive, gz)
	}
	return fail("tar: specify -c, -x or -t")
}

func tarCreate(archive string, files []string, gz bool) int {
	f, err := os.Create(archive)
	if err != nil {
		return fail("tar: %v", err)
	}
	defer f.Close()
	var w io.Writer = f
	var gzw *gzip.Writer
	if gz {
		gzw = gzip.NewWriter(f)
		w = gzw
	}
	tw := tar.NewWriter(w)
	defer func() {
		tw.Close()
		if gzw != nil {
			gzw.Close()
		}
	}()
	for _, path := range files {
		err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = p
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			src, err := os.Open(p)
			if err != nil {
				return err
			}
			defer src.Close()
			_, err = io.Copy(tw, src)
			return err
		})
		if err != nil {
			return fail("tar: %v", err)
		}
	}
	return 0
}

func tarExtract(archive string, gz bool) int {
	f, err := os.Open(archive)
	if err != nil {
		return fail("tar: %v", err)
	}
	defer f.Close()
	var r io.Reader = f
	if gz {
		gzr, err := gzip.NewReader(f)
		if err != nil {
			return fail("tar: %v", err)
		}
		defer gzr.Close()
		r = gzr
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail("tar: %v", err)
		}
		target := hdr.Name
		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, os.FileMode(hdr.Mode))
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o755)
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				fmt.Fprintf(os.Stderr, "tar: %v\n", err)
				continue
			}
			io.Copy(out, tr)
			out.Close()
		}
	}
	return 0
}

func tarList(archive string, gz bool) int {
	f, err := os.Open(archive)
	if err != nil {
		return fail("tar: %v", err)
	}
	defer f.Close()
	var r io.Reader = f
	if gz {
		gzr, err := gzip.NewReader(f)
		if err != nil {
			return fail("tar: %v", err)
		}
		defer gzr.Close()
		r = gzr
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail("tar: %v", err)
		}
		fmt.Println(hdr.Name)
	}
	return 0
}

func appletGzip(args []string) int {
	decompress, keep, toStdout := false, false, false
	var files []string
	for _, a := range args {
		switch a {
		case "-d":
			decompress = true
		case "-k":
			keep = true
		case "-c":
			toStdout = true
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "Usage: gzip [-d] [-k] [-c] files...")
			return 0
		default:
			files = append(files, a)
		}
	}
	if len(files) == 0 {
		files = []string{"-"}
	}
	rc := 0
	for _, name := range files {
		if decompress {
			rc = gzipDecompress(name, toStdout, keep)
		} else {
			rc = gzipCompress(name, toStdout, keep)
		}
	}
	return rc
}

func gzipCompress(name string, toStdout, keep bool) int {
	f, err := openInput(name)
	if err != nil {
		return fail("gzip: %v", err)
	}
	defer f.Close()
	var out io.WriteCloser
	if toStdout || name == "-" {
		out = nopWriteCloser{os.Stdout}
	} else {
		out, err = os.Create(name + ".gz")
		if err != nil {
			return fail("gzip: %v", err)
		}
		defer out.Close()
	}
	gw := gzip.NewWriter(out)
	io.Copy(gw, f)
	gw.Close()
	if !keep && name != "-" && !toStdout {
		os.Remove(name)
	}
	return 0
}

func gzipDecompress(name string, toStdout, keep bool) int {
	f, err := openInput(name)
	if err != nil {
		return fail("gzip: %v", err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return fail("gzip: %v", err)
	}
	defer gr.Close()
	var out io.WriteCloser
	if toStdout || name == "-" {
		out = nopWriteCloser{os.Stdout}
	} else {
		target := strings.TrimSuffix(name, ".gz")
		out, err = os.Create(target)
		if err != nil {
			return fail("gzip: %v", err)
		}
		defer out.Close()
	}
	io.Copy(out, gr)
	if !keep && name != "-" && !toStdout {
		os.Remove(name)
	}
	return 0
}

// ===========================================================================
// Applet: zip / unzip
// ===========================================================================

func appletZip(args []string) int {
	recursive := false
	quiet := false
	level := uint16(zip.Deflate) // default: Deflate
	var archive string
	var files []string
	var deleteMode bool

	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stderr, `Usage: zip [-r] [-q] [-0|-9] archive.zip files...
       zip -d archive.zip files...
       zip -l archive.zip

Options:
  -r   Recurse into directories
  -q   Quiet (suppress per-file output)
  -0   Store only (no compression)
  -9   Maximum compression (Deflate)
  -d   Delete entries from the archive
  -l   List archive contents
  -h, --help  Show this help`)
			return 0
		case a == "-r":
			recursive, i = true, i+1
		case a == "-q":
			quiet, i = true, i+1
		case a == "-0":
			level, i = zip.Store, i+1
		case a == "-9":
			level, i = zip.Deflate, i+1
		case a == "-d":
			deleteMode, i = true, i+1
		case a == "-l":
			if i+1 >= len(args) {
				return fail("zip: -l requires an archive name")
			}
			return zipList(args[i+1], quiet)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			return fail("zip: invalid option: %s", a)
		default:
			if archive == "" {
				archive, i = a, i+1
			} else {
				files, i = append(files, a), i+1
			}
		}
	}
	if archive == "" {
		return fail("zip: usage: zip [-r] [-q] [-0|-9] archive.zip files...")
	}
	if deleteMode {
		if len(files) == 0 {
			return fail("zip: -d requires at least one entry name")
		}
		return zipDelete(archive, files, quiet)
	}
	if len(files) == 0 {
		return fail("zip: nothing to do (no input files)")
	}
	return zipCreate(archive, files, recursive, quiet, level)
}

func zipCreate(archive string, files []string, recursive, quiet bool, level uint16) int {
	// Rebuild archive from scratch, preserving entries not being overwritten.
	type existingEntry struct {
		hdr  *zip.FileHeader
		data []byte
	}
	var existing []existingEntry
	if f, err := zip.OpenReader(archive); err == nil {
		for _, zf := range f.File {
			rc, err := zf.Open()
			if err != nil {
				f.Close()
				return fail("zip: %s: %v", archive, err)
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			hdr := zf.FileHeader
			existing = append(existing, existingEntry{&hdr, data})
		}
		f.Close()
	}

	type newEntry struct {
		name string
		info os.FileInfo
	}
	var entries []newEntry
	seen := map[string]bool{}

	addFile := func(path string, info os.FileInfo) {
		if info.IsDir() {
			name := filepath.ToSlash(path) + "/"
			if !seen[name] {
				seen[name] = true
				entries = append(entries, newEntry{name, info})
			}
			return
		}
		name := filepath.ToSlash(path)
		if !seen[name] {
			seen[name] = true
			entries = append(entries, newEntry{name, info})
		}
	}

	for _, f := range files {
		info, err := os.Lstat(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "zip: %s: %v\n", f, err)
			continue
		}
		if info.IsDir() {
			if !recursive {
				fmt.Fprintf(os.Stderr, "zip: %s is a directory (use -r)\n", f)
				continue
			}
			filepath.Walk(f, func(p string, fi os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				if p == f {
					return nil
				}
				addFile(p, fi)
				return nil
			})
		} else {
			addFile(f, info)
		}
	}

	if len(entries) == 0 && len(existing) == 0 {
		return fail("zip: nothing to do")
	}

	out, err := os.Create(archive)
	if err != nil {
		return fail("zip: %v", err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)

	for _, e := range existing {
		if seen[e.hdr.Name] {
			continue
		}
		hdr := *e.hdr
		w, err := zw.CreateHeader(&hdr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "zip: %v\n", err)
			continue
		}
		w.Write(e.data)
	}

	// Use the caller-selected method (default Deflate).
	method := level
	for _, e := range entries {
		hdr, err := zip.FileInfoHeader(e.info)
		if err != nil {
			fmt.Fprintf(os.Stderr, "zip: %s: %v\n", e.name, err)
			continue
		}
		hdr.Name = e.name
		hdr.Method = method
		if e.info.IsDir() {
			hdr.Method = zip.Store
			if !strings.HasSuffix(hdr.Name, "/") {
				hdr.Name += "/"
			}
			if _, err := zw.CreateHeader(hdr); err != nil {
				fmt.Fprintf(os.Stderr, "zip: %s: %v\n", e.name, err)
			}
			continue
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "zip: %s: %v\n", e.name, err)
			continue
		}
		src, err := os.Open(filepath.FromSlash(e.name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "zip: %s: %v\n", e.name, err)
			continue
		}
		n, err := io.Copy(w, src)
		src.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "zip: %s: %v\n", e.name, err)
			continue
		}
		if !quiet {
			fmt.Printf("  adding: %s (%d bytes)\n", e.name, n)
		}
	}

	if err := zw.Close(); err != nil {
		return fail("zip: %v", err)
	}
	return 0
}

func zipDelete(archive string, names []string, quiet bool) int {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return fail("zip: %s: %v", archive, err)
	}
	type kept struct {
		hdr  *zip.FileHeader
		data []byte
	}
	var keep []kept
	toDelete := map[string]bool{}
	for _, n := range names {
		toDelete[filepath.ToSlash(n)] = true
	}
	for _, zf := range r.File {
		if toDelete[zf.Name] {
			if !quiet {
				fmt.Printf("deleting: %s\n", zf.Name)
			}
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			r.Close()
			return fail("zip: %s: %v", archive, err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		hdr := zf.FileHeader
		keep = append(keep, kept{&hdr, data})
	}
	r.Close()

	out, err := os.Create(archive)
	if err != nil {
		return fail("zip: %v", err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for _, k := range keep {
		w, err := zw.CreateHeader(k.hdr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "zip: %v\n", err)
			continue
		}
		w.Write(k.data)
	}
	if err := zw.Close(); err != nil {
		return fail("zip: %v", err)
	}
	return 0
}

func zipList(archive string, quiet bool) int {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return fail("zip: %s: %v", archive, err)
	}
	defer r.Close()
	if !quiet {
		fmt.Printf("Archive:  %s\n", archive)
		fmt.Printf("  Length      Date    Time    Name\n")
		fmt.Printf("---------  ---------- -----   ----\n")
	}
	var total uint64
	for _, zf := range r.File {
		total += zf.UncompressedSize64
		fmt.Printf("%9d  %10s %5s   %s\n",
			zf.UncompressedSize64,
			zf.Modified.Format("2006-01-02"),
			zf.Modified.Format("15:04"),
			zf.Name)
	}
	if !quiet {
		fmt.Printf("---------                     -------\n")
		fmt.Printf("%9d                     %d files\n", total, len(r.File))
	}
	return 0
}

func appletUnzip(args []string) int {
	listOnly := false
	var archive, destDir string
	var patterns []string
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stderr, `Usage: unzip [-l] [-d dir] archive.zip [patterns...]

Options:
  -l         List contents only
  -d DIR     Extract into DIR
  -h, --help Show this help`)
			return 0
		case a == "-l":
			listOnly, i = true, i+1
		case a == "-d":
			if i+1 >= len(args) {
				return fail("unzip: -d requires a directory")
			}
			destDir, i = args[i+1], i+2
		case strings.HasPrefix(a, "-") && len(a) > 1:
			return fail("unzip: invalid option: %s", a)
		default:
			if archive == "" {
				archive, i = a, i+1
			} else {
				patterns, i = append(patterns, a), i+1
			}
		}
	}
	if archive == "" {
		return fail("unzip: usage: unzip [-l] [-d dir] archive.zip [patterns...]")
	}
	r, err := zip.OpenReader(archive)
	if err != nil {
		return fail("unzip: %s: %v", archive, err)
	}
	defer r.Close()

	match := func(name string) bool {
		if len(patterns) == 0 {
			return true
		}
		for _, p := range patterns {
			if ok, _ := filepath.Match(p, name); ok {
				return true
			}
		}
		return false
	}

	if listOnly {
		fmt.Printf("Archive:  %s\n", archive)
		fmt.Printf("  Length      Date    Time    Name\n")
		fmt.Printf("---------  ---------- -----   ----\n")
		var total uint64
		for _, zf := range r.File {
			if !match(zf.Name) {
				continue
			}
			total += zf.UncompressedSize64
			fmt.Printf("%9d  %10s %5s   %s\n",
				zf.UncompressedSize64,
				zf.Modified.Format("2006-01-02"),
				zf.Modified.Format("15:04"),
				zf.Name)
		}
		fmt.Printf("---------                     -------\n")
		fmt.Printf("%9d                     %d files\n", total, len(r.File))
		return 0
	}

	if destDir != "" {
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return fail("unzip: %v", err)
		}
	}

	rc := 0
	for _, zf := range r.File {
		if !match(zf.Name) {
			continue
		}
		clean := filepath.Clean(filepath.FromSlash(zf.Name))
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			fmt.Fprintf(os.Stderr, "unzip: skipping unsafe path: %s\n", zf.Name)
			rc = 1
			continue
		}
		target := clean
		if destDir != "" {
			target = filepath.Join(destDir, clean)
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, zf.Mode()); err != nil {
				fmt.Fprintf(os.Stderr, "unzip: %s: %v\n", target, err)
				rc = 1
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "unzip: %s: %v\n", target, err)
			rc = 1
			continue
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, zf.Mode())
		if err != nil {
			fmt.Fprintf(os.Stderr, "unzip: %s: %v\n", target, err)
			rc = 1
			continue
		}
		src, err := zf.Open()
		if err != nil {
			out.Close()
			fmt.Fprintf(os.Stderr, "unzip: %s: %v\n", zf.Name, err)
			rc = 1
			continue
		}
		_, err = io.Copy(out, src)
		src.Close()
		out.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "unzip: %s: %v\n", target, err)
			rc = 1
			continue
		}
		fmt.Printf("  extracting: %s\n", target)
	}
	return rc
}

// ===========================================================================
// Applet: stat / chmod / chown / ln / readlink / split / hexedit
// ===========================================================================

func appletStat(args []string) int {
	if len(args) == 0 {
		return fail("stat: usage: stat files...")
	}
	rc := 0
	for _, p := range args {
		if p == "-h" || p == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: stat files...")
			return 0
		}
		info, err := os.Stat(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stat: %s: %v\n", p, err)
			rc = 1
			continue
		}
		fmt.Printf("  File: %s\n", p)
		fmt.Printf("  Size: %d\n", info.Size())
		fmt.Printf("  Mode: %s\n", info.Mode())
		fmt.Printf("  Mod:  %s\n", info.ModTime().Format(time.RFC3339))
		fmt.Printf("  Dir:  %v\n", info.IsDir())
	}
	return rc
}

func appletChmod(args []string) int {
	if len(args) < 2 {
		return fail("chmod: usage: chmod mode files...")
	}
	mode, err := strconv.ParseUint(args[0], 8, 32)
	if err != nil {
		return fail("chmod: invalid mode: %s", args[0])
	}
	rc := 0
	for _, p := range args[1:] {
		if p == "-h" || p == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: chmod mode files...")
			return 0
		}
		if err := os.Chmod(p, os.FileMode(mode)); err != nil {
			fmt.Fprintf(os.Stderr, "chmod: %s: %v\n", p, err)
			rc = 1
		}
	}
	return rc
}

func appletChown(args []string) int {
	if len(args) < 2 {
		return fail("chown: usage: chown user files...")
	}
	fmt.Fprintln(os.Stderr, "chown: operation not supported portably in pure Go")
	return 1
}

func appletLn(args []string) int {
	symlink := false
	var rest []string
	for _, a := range args {
		if a == "-s" {
			symlink = true
			continue
		}
		if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: ln [-s] target link")
			return 0
		}
		rest = append(rest, a)
	}
	if len(rest) < 2 {
		return fail("ln: usage: ln [-s] target link")
	}
	target, link := rest[0], rest[1]
	var err error
	if symlink {
		err = os.Symlink(target, link)
	} else {
		err = os.Link(target, link)
	}
	if err != nil {
		return fail("ln: %v", err)
	}
	return 0
}

func appletReadlink(args []string) int {
	if len(args) < 1 {
		return fail("readlink: usage: readlink link")
	}
	target, err := os.Readlink(args[0])
	if err != nil {
		return fail("readlink: %v", err)
	}
	fmt.Println(target)
	return 0
}

func appletSplit(args []string) int {
	linesPerFile := 1000
	prefix := "x"
	var input string
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-l":
			if i+1 >= len(args) {
				return fail("split: -l requires an argument")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fail("split: invalid value: %s", args[i+1])
			}
			linesPerFile, i = n, i+2
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "Usage: split [-l N] [input] [prefix]")
			return 0
		default:
			if input == "" {
				input, i = args[i], i+1
			} else {
				prefix, i = args[i], i+1
			}
		}
	}
	f, err := openInput(input)
	if err != nil {
		return fail("split: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	fileIdx := 0
	lineCount := 0
	var out *os.File
	openNext := func() error {
		name := fmt.Sprintf("%s%02d", prefix, fileIdx)
		fileIdx++
		var err error
		out, err = os.Create(name)
		return err
	}
	for sc.Scan() {
		if out == nil || lineCount >= linesPerFile {
			if out != nil {
				out.Close()
			}
			if err := openNext(); err != nil {
				return fail("split: %v", err)
			}
			lineCount = 0
		}
		fmt.Fprintln(out, strings.TrimSuffix(sc.Text(), "\r"))
		lineCount++
	}
	if out != nil {
		out.Close()
	}
	return 0
}

func appletHexedit(args []string) int {
	if len(args) < 1 {
		return fail("hexedit: usage: hexedit file")
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "Usage: hexedit file")
		return 0
	}
	f, err := os.OpenFile(args[0], os.O_RDWR, 0o644)
	if err != nil {
		return fail("hexedit: %v", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return fail("hexedit: %v", err)
	}
	for i := 0; i < len(data); i += 16 {
		end := i + 16
		if end > len(data) {
			end = len(data)
		}
		fmt.Printf("%08x  ", i)
		for j := i; j < i+16; j++ {
			if j < end {
				fmt.Printf("%02x ", data[j])
			} else {
				fmt.Print("   ")
			}
			if j == i+7 {
				fmt.Print(" ")
			}
		}
		fmt.Print(" |")
		for j := i; j < end; j++ {
			c := data[j]
			if c < 32 || c > 126 {
				c = '.'
			}
			fmt.Printf("%c", c)
		}
		fmt.Println("|")
	}
	return 0
}

// ===========================================================================
// Applets: text
// ===========================================================================

func appletCat(args []string) int {
	if len(args) == 0 {
		args = []string{"-"}
	}
	rc := 0
	for _, name := range args {
		if name == "-h" || name == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: cat [files...]")
			return 0
		}
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cat: %s: %v\n", name, err)
			rc = 1
			continue
		}
		io.Copy(os.Stdout, f)
		f.Close()
	}
	return rc
}

func appletHead(args []string) int {
	n := 10
	i := 0
	for i < len(args) {
		if args[i] == "-h" || args[i] == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: head [-n N] [files...]")
			return 0
		}
		if args[i] == "-n" && i+1 < len(args) {
			v, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fail("head: invalid value: %s", args[i+1])
			}
			n, i = v, i+2
			continue
		}
		break
	}
	files := args[i:]
	if len(files) == 0 {
		files = []string{"-"}
	}
	rc := 0
	for _, name := range files {
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "head: %s: %v\n", name, err)
			rc = 1
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		c := 0
		for sc.Scan() && c < n {
			fmt.Println(strings.TrimSuffix(sc.Text(), "\r"))
			c++
		}
		f.Close()
	}
	return rc
}

func appletTail(args []string) int {
	n := 10
	i := 0
	for i < len(args) {
		if args[i] == "-h" || args[i] == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: tail [-n N] [files...]")
			return 0
		}
		if args[i] == "-n" && i+1 < len(args) {
			v, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fail("tail: invalid value: %s", args[i+1])
			}
			n, i = v, i+2
			continue
		}
		break
	}
	files := args[i:]
	if len(files) == 0 {
		files = []string{"-"}
	}
	rc := 0
	for _, name := range files {
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tail: %s: %v\n", name, err)
			rc = 1
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		var lines []string
		for sc.Scan() {
			lines = append(lines, strings.TrimSuffix(sc.Text(), "\r"))
		}
		f.Close()
		start := len(lines) - n
		if start < 0 {
			start = 0
		}
		for _, l := range lines[start:] {
			fmt.Println(l)
		}
	}
	return rc
}

func appletWc(args []string) int {
	countLines, countWords, countBytes := false, false, false
	var files []string
	for _, a := range args {
		switch a {
		case "-l":
			countLines = true
		case "-w":
			countWords = true
		case "-c":
			countBytes = true
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "Usage: wc [-l] [-w] [-c] [files...]")
			return 0
		default:
			files = append(files, a)
		}
	}
	if !countLines && !countWords && !countBytes {
		countLines, countWords, countBytes = true, true, true
	}
	if len(files) == 0 {
		files = []string{"-"}
	}
	rc := 0
	for _, name := range files {
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wc: %s: %v\n", name, err)
			rc = 1
			continue
		}
		data, _ := io.ReadAll(f)
		f.Close()
		lines := strings.Count(string(data), "\n")
		words := len(strings.Fields(string(data)))
		var parts []string
		if countLines {
			parts = append(parts, strconv.Itoa(lines))
		}
		if countWords {
			parts = append(parts, strconv.Itoa(words))
		}
		if countBytes {
			parts = append(parts, strconv.Itoa(len(data)))
		}
		if name != "-" {
			parts = append(parts, name)
		}
		fmt.Println(strings.Join(parts, " "))
	}
	return rc
}

func appletGrep(args []string) int {
	ignoreCase, invert, showNum := false, false, false
	var rest []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			for _, c := range a[1:] {
				switch c {
				case 'i':
					ignoreCase = true
				case 'v':
					invert = true
				case 'n':
					showNum = true
				case 'h':
					fmt.Fprintln(os.Stderr, "Usage: grep [-i] [-v] [-n] pattern [files...]")
					return 0
				default:
					return fail("grep: invalid option: -%c", c)
				}
			}
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) == 0 {
		return fail("grep: usage: grep [-i] [-v] [-n] pattern [files...]")
	}
	pattern := rest[0]
	files := rest[1:]
	if len(files) == 0 {
		files = []string{"-"}
	}
	if ignoreCase {
		pattern = strings.ToLower(pattern)
	}
	rc := 1
	for _, name := range files {
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "grep: %s: %v\n", name, err)
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			line := strings.TrimSuffix(sc.Text(), "\r")
			hay := line
			if ignoreCase {
				hay = strings.ToLower(hay)
			}
			match := strings.Contains(hay, pattern)
			if invert {
				match = !match
			}
			if match {
				rc = 0
				if showNum {
					fmt.Printf("%d:%s\n", lineNo, line)
				} else {
					fmt.Println(line)
				}
			}
		}
		f.Close()
	}
	return rc
}

func appletSort(args []string) int {
	if len(args) == 0 {
		args = []string{"-"}
	}
	var lines []string
	for _, name := range args {
		if name == "-h" || name == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: sort [files...]")
			return 0
		}
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sort: %s: %v\n", name, err)
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			lines = append(lines, strings.TrimSuffix(sc.Text(), "\r"))
		}
		f.Close()
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
	return 0
}

func appletUniq(args []string) int {
	if len(args) == 0 {
		args = []string{"-"}
	}
	rc := 0
	for _, name := range args {
		if name == "-h" || name == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: uniq [files...]")
			return 0
		}
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "uniq: %s: %v\n", name, err)
			rc = 1
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		var prev string
		first := true
		for sc.Scan() {
			line := strings.TrimSuffix(sc.Text(), "\r")
			if first || line != prev {
				fmt.Println(line)
			}
			prev = line
			first = false
		}
		f.Close()
	}
	return rc
}

func appletTr(args []string) int {
	if len(args) < 2 {
		return fail("tr: usage: tr set1 set2")
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "Usage: tr set1 set2")
		return 0
	}
	set1 := expandTrSet(args[0])
	set2 := expandTrSet(args[1])
	if len(set1) != len(set2) {
		return fail("tr: sets must have the same length")
	}
	data, _ := io.ReadAll(os.Stdin)
	out := make([]byte, len(data))
	for i, b := range data {
		idx := strings.IndexByte(set1, b)
		if idx >= 0 {
			out[i] = set2[idx]
		} else {
			out[i] = b
		}
	}
	os.Stdout.Write(out)
	return 0
}

// expandTrSet expands ranges like "a-z", "A-Z", "0-9" into the full set.
func expandTrSet(s string) string {
	var out []byte
	i := 0
	for i < len(s) {
		if i+2 < len(s) && s[i+1] == '-' {
			start := s[i]
			end := s[i+2]
			if start <= end {
				for c := start; c <= end; c++ {
					out = append(out, c)
				}
			} else {
				for c := start; c >= end; c-- {
					out = append(out, c)
				}
			}
			i += 3
			continue
		}
		out = append(out, s[i])
		i++
	}
	return string(out)
}

func appletCut(args []string) int {
	delim := "\t"
	var fields []int
	var files []string
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-d":
			if i+1 >= len(args) {
				return fail("cut: -d requires an argument")
			}
			delim, i = args[i+1], i+2
		case "-f":
			if i+1 >= len(args) {
				return fail("cut: -f requires an argument")
			}
			for _, s := range strings.Split(args[i+1], ",") {
				n, err := strconv.Atoi(s)
				if err != nil || n < 1 {
					return fail("cut: invalid field: %s", s)
				}
				fields = append(fields, n)
			}
			i += 2
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "Usage: cut -d delim -f fields [files...]")
			return 0
		default:
			files, i = append(files, args[i]), i+1
		}
	}
	if len(fields) == 0 {
		return fail("cut: usage: cut -d delim -f fields [files...]")
	}
	if len(files) == 0 {
		files = []string{"-"}
	}
	rc := 0
	for _, name := range files {
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cut: %s: %v\n", name, err)
			rc = 1
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSuffix(sc.Text(), "\r")
			parts := strings.Split(line, delim)
			var out []string
			for _, n := range fields {
				if n-1 < len(parts) {
					out = append(out, parts[n-1])
				}
			}
			fmt.Println(strings.Join(out, delim))
		}
		f.Close()
	}
	return rc
}

func appletTee(args []string) int {
	appendMode := false
	var files []string
	for _, a := range args {
		if a == "-a" {
			appendMode = true
			continue
		}
		if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: tee [-a] files...")
			return 0
		}
		files = append(files, a)
	}
	var writers []io.Writer
	writers = append(writers, os.Stdout)
	var closers []io.Closer
	for _, name := range files {
		flags := os.O_CREATE | os.O_WRONLY
		if appendMode {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
		}
		f, err := os.OpenFile(name, flags, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tee: %s: %v\n", name, err)
			continue
		}
		writers = append(writers, f)
		closers = append(closers, f)
	}
	io.Copy(io.MultiWriter(writers...), os.Stdin)
	for _, c := range closers {
		c.Close()
	}
	return 0
}

func appletSed(args []string) int {
	if len(args) == 0 {
		return fail("sed: usage: sed [-n] [-e script]... [script] [files...]")
	}
	noPrint := false
	var scripts []string
	var files []string
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-n":
			noPrint, i = true, i+1
		case a == "-e":
			if i+1 >= len(args) {
				return fail("sed: -e requires an argument")
			}
			scripts, i = append(scripts, args[i+1]), i+2
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stderr, "Usage: sed [-n] [-e script]... [script] [files...]")
			return 0
		case strings.HasPrefix(a, "-"):
			return fail("sed: invalid option: %s", a)
		default:
			if len(scripts) == 0 {
				scripts, i = append(scripts, a), i+1
			} else {
				files, i = append(files, a), i+1
			}
		}
	}
	if len(scripts) == 0 {
		return fail("sed: missing script")
	}
	if len(files) == 0 {
		files = []string{"-"}
	}
	var prog []sedCmd
	for _, sc := range scripts {
		cmds, err := parseSedScript(sc)
		if err != nil {
			return fail("sed: %v", err)
		}
		prog = append(prog, cmds...)
	}
	rc := 0
	for _, name := range files {
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sed: %s: %v\n", name, err)
			rc = 1
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			line := strings.TrimSuffix(sc.Text(), "\r")
			del := false
			printed := false
			for _, c := range prog {
				if !c.matches(line, lineNo) {
					continue
				}
				switch c.op {
				case 'd':
					del = true
				case 'p':
					fmt.Println(line)
					printed = true
				case 's':
					line = c.re.ReplaceAllString(line, c.repl)
				}
			}
			if del {
				continue
			}
			if !noPrint && !printed {
				fmt.Println(line)
			}
		}
		f.Close()
	}
	return rc
}

type sedCmd struct {
	addrStart, addrEnd int
	addrRe             *regexp.Regexp
	op                 byte
	re                 *regexp.Regexp
	repl               string
	global             bool
}

func (c sedCmd) matches(line string, n int) bool {
	if c.addrRe != nil {
		return c.addrRe.MatchString(line)
	}
	if c.addrStart == 0 {
		return true
	}
	if c.addrEnd > 0 {
		return n >= c.addrStart && n <= c.addrEnd
	}
	return n == c.addrStart
}

func parseSedScript(s string) ([]sedCmd, error) {
	var out []sedCmd
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		cmd, err := parseSedCmd(part)
		if err != nil {
			return nil, err
		}
		out = append(out, cmd)
	}
	return out, nil
}

func parseSedCmd(s string) (sedCmd, error) {
	var c sedCmd
	if strings.HasPrefix(s, "/") {
		end := strings.Index(s[1:], "/")
		if end < 0 {
			return c, fmt.Errorf("unterminated regex address")
		}
		re, err := regexp.Compile(s[1 : 1+end])
		if err != nil {
			return c, err
		}
		c.addrRe = re
		s = s[2+end:]
	} else {
		fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' })
		if len(fields) > 0 {
			if n, err := strconv.Atoi(fields[0]); err == nil {
				c.addrStart = n
				if len(fields) > 1 {
					if n2, err := strconv.Atoi(fields[1]); err == nil {
						c.addrEnd = n2
					}
				}
				idx := strings.Index(s, fields[len(fields)-1])
				s = s[idx+len(fields[len(fields)-1]):]
			}
		}
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return c, fmt.Errorf("empty sed command")
	}
	c.op = s[0]
	switch c.op {
	case 'd', 'p':
	case 's':
		delim := s[1]
		parts := strings.SplitN(s[2:], string(delim), 3)
		if len(parts) < 2 {
			return c, fmt.Errorf("invalid s substitution")
		}
		re, err := regexp.Compile(parts[0])
		if err != nil {
			return c, err		}
		c.re = re
		c.repl = parts[1]
		if len(parts) >= 3 && strings.Contains(parts[2], "g") {
			c.global = true
		}
	default:
		return c, fmt.Errorf("unsupported sed command: %c", c.op)
	}
	return c, nil
}

func appletAwk(args []string) int {
	fs := " "
	var prog string
	var files []string
	i := 0
	for i < len(args) {
		switch {
		case args[i] == "-h" || args[i] == "--help":
			fmt.Fprintln(os.Stderr, "Usage: awk [-F sep] 'program' [files...]")
			return 0
		case args[i] == "-F":
			if i+1 >= len(args) {
				return fail("awk: -F requires an argument")
			}
			fs, i = args[i+1], i+2
		case strings.HasPrefix(args[i], "-F"):
			fs, i = args[i][2:], i+1
		case prog == "":
			prog, i = args[i], i+1
		default:
			files, i = append(files, args[i]), i+1
		}
	}
	if prog == "" {
		return fail("awk: usage: awk [-F sep] 'program' [files...]")
	}
	if len(files) == 0 {
		files = []string{"-"}
	}
	cond, action := parseAwk(prog)
	rc := 0
	lineNo := 0
	for _, name := range files {
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "awk: %s: %v\n", name, err)
			rc = 1
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			lineNo++
			line := strings.TrimSuffix(sc.Text(), "\r")
			var fields []string
			if fs == " " || fs == "" {
				fields = strings.Fields(line)
			} else {
				fields = strings.Split(line, fs)
			}
			ctx := map[string]string{
				"NR": strconv.Itoa(lineNo),
				"NF": strconv.Itoa(len(fields)),
				"FS": fs,
				"$0": line,
			}
			for idx, fv := range fields {
				ctx["$"+strconv.Itoa(idx+1)] = fv
			}
			if cond != "" && !evalAwkCond(cond, ctx) {
				continue
			}
			execAwkAction(action, ctx, fields, line)
		}
		f.Close()
	}
	return rc
}

func parseAwk(prog string) (cond, action string) {
	prog = strings.TrimSpace(prog)
	if strings.HasPrefix(prog, "{") && strings.HasSuffix(prog, "}") {
		return "", prog[1 : len(prog)-1]
	}
	if idx := strings.Index(prog, "{"); idx >= 0 {
		cond = strings.TrimSpace(prog[:idx])
		action = prog[idx+1:]
		if strings.HasSuffix(action, "}") {
			action = action[:len(action)-1]
		}
		return cond, strings.TrimSpace(action)
	}
	return prog, "print $0"
}

func evalAwkCond(cond string, ctx map[string]string) bool {
	re := regexp.MustCompile(`^\s*(\$\d+|NR|NF)\s*(==|!=|>=|<=|>|<)\s*(.+?)\s*$`)
	m := re.FindStringSubmatch(cond)
	if m == nil {
		return true
	}
	lhs := ctx[m[1]]
	rhs := strings.Trim(m[3], `"'`)
	switch m[2] {
	case "==":
		return lhs == rhs
	case "!=":
		return lhs != rhs
	case ">", "<", ">=", "<=":
		a, _ := strconv.ParseFloat(lhs, 64)
		b, _ := strconv.ParseFloat(rhs, 64)
		switch m[2] {
		case ">":
			return a > b
		case "<":
			return a < b
		case ">=":
			return a >= b
		case "<=":
			return a <= b
		}
	}
	return true
}

func execAwkAction(action string, ctx map[string]string, fields []string, line string) {
	action = strings.TrimSpace(action)
	for _, stmt := range strings.Split(action, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if strings.HasPrefix(stmt, "print") {
			expr := strings.TrimSpace(stmt[5:])
			if expr == "" {
				fmt.Println(line)
				continue
			}
			parts := strings.Split(expr, ",")
			var outs []string
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if v, ok := ctx[p]; ok {
					outs = append(outs, v)
				} else {
					outs = append(outs, strings.Trim(p, `"'`))
				}
			}
			fmt.Println(strings.Join(outs, " "))
		}
	}
}

func appletDiff(args []string) int {
	if len(args) < 2 {
		return fail("diff: usage: diff file1 file2")
	}
	a, err := readLines(args[0])
	if err != nil {
		return fail("diff: %v", err)
	}
	b, err := readLines(args[1])
	if err != nil {
		return fail("diff: %v", err)
	}
	diffs := diffLines(a, b)
	if len(diffs) == 0 {
		return 0
	}
	fmt.Printf("--- %s\n+++ %s\n", args[0], args[1])
	for _, d := range diffs {
		switch d.op {
		case '-':
			fmt.Printf("- %s\n", d.line)
		case '+':
			fmt.Printf("+ %s\n", d.line)
		}
	}
	return 1
}

type diffOp struct {
	op   byte
	line string
}

func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				lcs[i][j] = lcs[i-1][j-1] + 1
			} else if lcs[i-1][j] >= lcs[i][j-1] {
				lcs[i][j] = lcs[i-1][j]
			} else {
				lcs[i][j] = lcs[i][j-1]
			}
		}
	}
	var rev []diffOp
	i, j := n, m
	for i > 0 && j > 0 {
		if a[i-1] == b[j-1] {
			rev = append(rev, diffOp{' ', a[i-1]})
			i--
			j--
		} else if lcs[i-1][j] >= lcs[i][j-1] {
			rev = append(rev, diffOp{'-', a[i-1]})
			i--
		} else {
			rev = append(rev, diffOp{'+', b[j-1]})
			j--
		}
	}
	for i > 0 {
		rev = append(rev, diffOp{'-', a[i-1]})
		i--
	}
	for j > 0 {
		rev = append(rev, diffOp{'+', b[j-1]})
		j--
	}
	out := make([]diffOp, 0, len(rev))
	for k := len(rev) - 1; k >= 0; k-- {
		out = append(out, rev[k])
	}
	var filtered []diffOp
	for _, d := range out {
		if d.op != ' ' {
			filtered = append(filtered, d)
		}
	}
	return filtered
}

func appletPatch(args []string) int {
	var outFile string
	var rest []string
	i := 0
	for i < len(args) {
		if args[i] == "-o" && i+1 < len(args) {
			outFile, i = args[i+1], i+2
			continue
		}
		if args[i] == "-h" || args[i] == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: patch [-o output] file patchfile")
			return 0
		}
		rest, i = append(rest, args[i]), i+1
	}
	if len(rest) < 2 {
		return fail("patch: usage: patch [-o output] file patchfile")
	}
	target, patchFile := rest[0], rest[1]
	orig, err := readLines(target)
	if err != nil {
		return fail("patch: %v", err)
	}
	patchLines, err := readLines(patchFile)
	if err != nil {
		return fail("patch: %v", err)
	}
	var result []string
	origIdx := 0
	for _, pl := range patchLines {
		if strings.HasPrefix(pl, "---") || strings.HasPrefix(pl, "+++") {
			continue
		}
		if strings.HasPrefix(pl, "- ") {
			removed := pl[2:]
			for origIdx < len(orig) && orig[origIdx] != removed {
				result = append(result, orig[origIdx])
				origIdx++
			}
			if origIdx < len(orig) {
				origIdx++
			}
			continue
		}
		if strings.HasPrefix(pl, "+ ") {
			result = append(result, pl[2:])
			continue
		}
	}
	for origIdx < len(orig) {
		result = append(result, orig[origIdx])
		origIdx++
	}
	if outFile == "" {
		outFile = target
	}
	f, err := os.Create(outFile)
	if err != nil {
		return fail("patch: %v", err)
	}
	defer f.Close()
	for _, l := range result {
		fmt.Fprintln(f, l)
	}
	return 0
}

func appletStrings(args []string) int {
	minLen := 4
	var files []string
	i := 0
	for i < len(args) {
		if args[i] == "-h" || args[i] == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: strings [-n N] [files...]")
			return 0
		}
		if args[i] == "-n" && i+1 < len(args) {
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fail("strings: invalid value: %s", args[i+1])
			}
			minLen, i = n, i+2
			continue
		}
		files, i = append(files, args[i]), i+1
	}
	if len(files) == 0 {
		files = []string{"-"}
	}
	rc := 0
	for _, name := range files {
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "strings: %s: %v\n", name, err)
			rc = 1
			continue
		}
		data, _ := io.ReadAll(f)
		f.Close()
		var cur []byte
		flush := func() {
			if len(cur) >= minLen {
				fmt.Println(string(cur))
			}
			cur = cur[:0]
		}
		for _, b := range data {
			if b >= 32 && b <= 126 {
				cur = append(cur, b)
			} else {
				flush()
			}
		}
		flush()
	}
	return rc
}

// ===========================================================================
// Applet: nl
// ===========================================================================

func appletNl(args []string) int {
	// Defaults (POSIX nl).
	bodyStyle := "t" // -b: a=all, t=non-empty (default), n=none
	increment := 1   // -i
	separator := "\t" // -s
	startNum := 1    // -v
	width := 6       // -w
	format := "rn"   // -n: ln=left, rn=right, rz=right zero-padded
	noRenumber := false // -p (do not reset numbering on logical page breaks)

	var files []string
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stderr, `Usage: nl [-b STYLE] [-i N] [-s SEP] [-v N] [-w N] [-n FORMAT] [files...]

Number lines of files.

Options:
  -b STYLE   Which lines to number: a (all), t (non-empty, default), n (none)
  -i N       Line number increment (default: 1)
  -s SEP     Separator between number and line (default: TAB)
  -v N       Starting line number (default: 1)
  -w N       Width of the line number (default: 6)
  -n FORMAT  Number format: ln (left), rn (right, default), rz (right zero-padded)
  -h, --help Show this help`)
			return 0
		case a == "-b":
			if i+1 >= len(args) {
				return fail("nl: -b requires an argument")
			}
			bodyStyle, i = args[i+1], i+2
		case a == "-i":
			if i+1 >= len(args) {
				return fail("nl: -i requires an argument")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 {
				return fail("nl: invalid increment: %s", args[i+1])
			}
			increment, i = n, i+2
		case a == "-s":
			if i+1 >= len(args) {
				return fail("nl: -s requires an argument")
			}
			separator, i = args[i+1], i+2
		case a == "-v":
			if i+1 >= len(args) {
				return fail("nl: -v requires an argument")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fail("nl: invalid start: %s", args[i+1])
			}
			startNum, i = n, i+2
		case a == "-w":
			if i+1 >= len(args) {
				return fail("nl: -w requires an argument")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 {
				return fail("nl: invalid width: %s", args[i+1])
			}
			width, i = n, i+2
		case a == "-n":
			if i+1 >= len(args) {
				return fail("nl: -n requires an argument")
			}
			format, i = args[i+1], i+2
			switch format {
			case "ln", "rn", "rz":
			default:
				return fail("nl: invalid format: %s (use ln, rn or rz)", format)
			}
		case a == "-p":
			noRenumber, i = true, i+1
		case strings.HasPrefix(a, "-"):
			return fail("nl: invalid option: %s", a)
		default:
			files = append(files, a)
			i++
		}
	}
	_ = noRenumber // unused for now; logical page breaks not implemented

	switch bodyStyle {
	case "a", "t", "n":
	default:
		return fail("nl: invalid body style: %s (use a, t or n)", bodyStyle)
	}

	if len(files) == 0 {
		files = []string{"-"}
	}

	rc := 0
	num := startNum
	for _, name := range files {
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nl: %s: %v\n", name, err)
			rc = 1
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSuffix(sc.Text(), "\r")
			shouldNumber := false
			switch bodyStyle {
			case "a":
				shouldNumber = true
			case "t":
				shouldNumber = line != ""
			case "n":
				shouldNumber = false
			}
			if shouldNumber {
				var numStr string
				switch format {
				case "ln":
					numStr = fmt.Sprintf("%-*d", width, num)
				case "rn":
					numStr = fmt.Sprintf("%*d", width, num)
				case "rz":
					numStr = fmt.Sprintf("%0*d", width, num)
				}
				fmt.Println(numStr + separator + line)
				num += increment
			} else {
				fmt.Println(strings.Repeat(" ", width) + separator + line)
			}
		}
		f.Close()
	}
	return rc
}

// ===========================================================================
// Applets: system
// ===========================================================================

func appletPwd(args []string) int {
	wd, err := os.Getwd()
	if err != nil {
		return fail("pwd: %v", err)
	}
	fmt.Println(wd)
	return 0
}

func appletEnv(args []string) int {
	for _, e := range os.Environ() {
		fmt.Println(e)
	}
	return 0
}

func appletSleep(args []string) int {
	if len(args) < 1 {
		return fail("sleep: usage: sleep seconds")
	}
	sec, err := strconv.ParseFloat(args[0], 64)
	if err != nil {
		return fail("sleep: invalid value: %s", args[0])
	}
	time.Sleep(time.Duration(sec * float64(time.Second)))
	return 0
}

func appletUname(args []string) int {
	all := false
	for _, a := range args {
		if a == "-a" {
			all = true
		}
		if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: uname [-a]")
			return 0
		}
	}
	if all {
		fmt.Printf("%s %s %s %s %s\n", runtime.GOOS, hostnameSafe(),
			runtime.GOARCH, runtime.Version(), runtime.Compiler)
		return 0
	}
	fmt.Println(runtime.GOOS)
	return 0
}

func hostnameSafe() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func appletWhoami(args []string) int {
	if u := os.Getenv("USER"); u != "" {
		fmt.Println(u)
		return 0
	}
	if u := os.Getenv("USERNAME"); u != "" {
		fmt.Println(u)
		return 0
	}
	fmt.Println("unknown")
	return 0
}

func appletHostname(args []string) int {
	fmt.Println(hostnameSafe())
	return 0
}

func appletDate(args []string) int {
	fmt.Println(time.Now().Format("Mon Jan _2 15:04:05 MST 2006"))
	return 0
}

func appletId(args []string) int {
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	fmt.Printf("uid=%d gid=%d user=%s\n", os.Getuid(), os.Getgid(), user)
	return 0
}

func appletWhich(args []string) int {
	if len(args) == 0 {
		return fail("which: usage: which commands...")
	}
	rc := 1
	for _, c := range args {
		if c == "-h" || c == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: which commands...")
			return 0
		}
		if p, err := exec.LookPath(c); err == nil {
			fmt.Println(p)
			rc = 0
		}
	}
	return rc
}

var startTime = time.Now()

func appletTop(args []string) int {
	fmt.Printf("Uptime: %s\n", time.Since(startTime).Round(time.Second))
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("Goroutines: %d\n", runtime.NumGoroutine())
	fmt.Printf("Alloc: %s\n", humanSize(int64(m.Alloc)))
	fmt.Printf("Sys:   %s\n", humanSize(int64(m.Sys)))
	fmt.Printf("NumGC: %d\n", m.NumGC)
	return 0
}

// ===========================================================================
// Applet: cal
// ===========================================================================

var (
	calYear          int
	calOmitUnderline bool
)

func appletCal(args []string) int {
	calYear = time.Now().Year()
	calOmitUnderline = false
	var positional []string
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stderr, `Usage: cal [-y] [-o] [month] [year]
       cal .
       cal -y

Display a calendar.

Options:
  -y          Display the full year
  -o          Omit underline for current day
  -h, --help  Show this help

Examples:
  cal          Show current month with today underlined
  cal .        Show three months (prev, current, next) with today underlined
  cal -y       Show the full current year
  cal 2026     Show the full year 2026
  cal 10 2026  Show October 2026`)
			return 0
		case a == "-y":
			positional = append(positional, "+")
			i++
		case a == "-o":
			calOmitUnderline = true
			i++
		case a == "-j":
			i++
		case strings.HasPrefix(a, "-"):
			return fail("cal: invalid option: %s", a)
		default:
			positional = append(positional, a)
			i++
		}
	}

	if len(positional) == 0 {
		year, month, day := time.Now().Date()
		printCalWithUnderline(year, int(month), day)
		return 0
	}

	switch positional[0] {
	case ".":
		year, month, day := time.Now().Date()
		calendars := []string{
			getCalendar(year, int(month)-1),
			getCalendar(year, int(month)),
			getCalendar(year, int(month)+1),
		}
		lines := make([]string, 9)
		for i := range lines {
			if i == 0 {
				lines[i] = strings.Repeat(" ", 32) + fmt.Sprintf("%d", year)
				continue
			}
			lines[i] = strings.Join([]string{
				" " + getMonthRows(calendars[0], i-1),
				" " + getMonthRows(calendars[1], i-1),
				" " + getMonthRows(calendars[2], i-1),
			}, " ")
		}
		dayStr := fmt.Sprintf("%2d", day)
		dayUnderlined := dayStr
		if !calOmitUnderline {
			dayUnderlined = underline(dayStr)
		}
		lines[2] = strings.Replace(lines[2], dayStr, dayUnderlined, 1)
		for _, line := range lines {
			fmt.Println(line)
		}
	case "+":
		printFullCal(calYear)
	default:
		if y, err := strconv.Atoi(positional[0]); err == nil {
			calYear = y
			printFullCal(calYear)
			return 0
		}
		if len(positional) >= 2 {
			m, err1 := strconv.Atoi(positional[0])
			y, err2 := strconv.Atoi(positional[1])
			if err1 == nil && err2 == nil && m >= 1 && m <= 12 {
				printCalWithUnderline(y, m, -1)
				return 0
			}
		}
		return fail("cal: usage: cal [-y] [month] [year]")
	}
	return 0
}

func underline(text string) string {
	return "\033[4m" + text + "\033[0m"
}

func getCalendar(year, month int) string {
	t := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	monthName := t.Month().String()
	cal := fmt.Sprintf("%11s          \nSu Mo Tu We Th Fr Sa \n", monthName[:3])

	lastDay := getDaysInMonth(year, month)
	startWeekday := int(t.Weekday())

	weekday := startWeekday

	cal += strings.Repeat("   ", weekday)

	for day := 1; day <= lastDay; day++ {
		dayStr := fmt.Sprintf("%2d", day)
		if year == time.Now().Year() && month == int(time.Now().Month()) && day == time.Now().Day() && !calOmitUnderline {
			dayStr = "\033[4m" + dayStr + "\033[0m"
		}
		cal += dayStr + " "
		weekday = (weekday + 1) % 7
		if weekday == 0 && day < lastDay {
			cal += "\n"
		}
	}

	if weekday != 0 {
		cal += strings.Repeat("   ", 7-weekday)
		cal += "\n"
	}

	return cal
}

func getDaysInMonth(year, month int) int {
	return time.Date(year, time.Month(month+1), 0, 0, 0, 0, 0, time.UTC).Day()
}

func getMonthRows(calendar string, row int) string {
	rows := strings.Split(calendar, "\n")
	if len(rows) > row {
		rowStr := rows[row]
		if len(rowStr) < 21 {
			rowStr += strings.Repeat(" ", 21-len(rowStr))
		}
		return rowStr
	}
	return strings.Repeat(" ", 21)
}

func printFullCal(year int) {
	thisDate := time.Date(year, 1, 1, 1, 1, 1, 1, time.UTC)
	var (
		dayArr                  [12][7][6]int
		month, lastMonth        time.Month
		weekInMonth, dayInMonth int
	)
	for thisDate.Year() == year {
		if month = thisDate.Month(); month != lastMonth {
			weekInMonth = 0
			dayInMonth = 1
		}
		weekday := thisDate.Weekday()
		if weekday == 0 && dayInMonth > 1 {
			weekInMonth++
		}
		dayArr[int(month)-1][weekday][weekInMonth] = thisDate.Day()
		lastMonth = month
		dayInMonth++
		thisDate = thisDate.Add(time.Hour * 24)
	}
	fmt.Println(strings.Repeat(" ", 32) + fmt.Sprintf("%d", year))

	months := [12]string{
		"Jan", "Feb", "Mar", "Apr", "May", "Jun",
		"Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
	}

	for qtr := 0; qtr < 4; qtr++ {
		for monthInQtr := 0; monthInQtr < 3; monthInQtr++ {
			fmt.Printf("         %s           ", months[qtr*3+monthInQtr])
		}
		fmt.Println()
		for monthInQtr := 0; monthInQtr < 3; monthInQtr++ {
			days := [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
			for _, day := range days {
				fmt.Printf(" %s", day[:2])
			}
			fmt.Printf("  ")
		}
		fmt.Println()
		for weekInMonth = 0; weekInMonth < 6; weekInMonth++ {
			for monthInQtr := 0; monthInQtr < 3; monthInQtr++ {
				for day := 0; day < 7; day++ {
					if dayArr[qtr*3+monthInQtr][day][weekInMonth] == 0 {
						fmt.Printf("   ")
					} else {
						fmt.Printf("%3d", dayArr[qtr*3+monthInQtr][day][weekInMonth])
					}
				}
				fmt.Printf("  ")
			}
			fmt.Println()
		}
		fmt.Println()
	}
}

func printCalWithUnderline(year, month, currentDay int) {
	monthName := time.Month(month).String()[:3]
	fmt.Printf("       %s %d\n", monthName, year)
	fmt.Println(" Su Mo Tu We Th Fr Sa")

	startDate := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	startWeekday := int(startDate.Weekday())
	daysInMonth := time.Date(year, time.Month(month+1), 0, 0, 0, 0, 0, time.UTC).Day()

	day := 1
	lines := []string{}
	line := ""
	for week := 0; day <= daysInMonth; week++ {
		line = ""
		for weekday := 0; weekday < 7; weekday++ {
			if week == 0 && weekday < startWeekday {
				line += "   "
			} else if day == currentDay {
				dayStr := fmt.Sprintf("%2d", day)
				if !calOmitUnderline {
					dayStr = underline(dayStr)
				}
				line += " " + dayStr
				day++
			} else if day <= daysInMonth {
				line += fmt.Sprintf(" %2d", day)
				day++
			}
		}
		lines = append(lines, line)
	}

	for _, line := range lines {
		fmt.Println(line)
	}
	fmt.Println()
}

// ===========================================================================
// Applets: arithmetic
// ===========================================================================

func appletExpr(args []string) int {
	if len(args) == 0 {
		return fail("expr: usage: expr expression")
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "Usage: expr expression")
		return 0
	}
	expr := strings.Join(args, " ")
	v, err := evalExpr(expr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "expr: %v\n", err)
		return 2
	}
	if v == 0 {
		return 1
	}
	fmt.Println(v)
	return 0
}

func evalExpr(s string) (int64, error) {
	p := &exprParser{s: s}
	v, err := p.parseAdd()
	if err != nil {
		return 0, err
	}
	p.skipSpace()
	if p.pos < len(p.s) {
		return 0, fmt.Errorf("unexpected token: %q", p.s[p.pos:])
	}
	return v, nil
}

type exprParser struct {
	s   string
	pos int
}

func (p *exprParser) skipSpace() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t') {
		p.pos++
	}
}

func (p *exprParser) parseAdd() (int64, error) {
	left, err := p.parseMul()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.s) {
			return left, nil
		}
		op := p.s[p.pos]
		if op != '+' && op != '-' {
			return left, nil
		}
		p.pos++
		right, err := p.parseMul()
		if err != nil {
			return 0, err
		}
		if op == '+' {
			left += right
		} else {
			left -= right
		}
	}
}

func (p *exprParser) parseMul() (int64, error) {
	left, err := p.parseAtom()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.s) {
			return left, nil
		}
		op := p.s[p.pos]
		if op != '*' && op != '/' && op != '%' {
			return left, nil
		}
		p.pos++
		right, err := p.parseAtom()
		if err != nil {
			return 0, err
		}
		switch op {
		case '*':
			left *= right
		case '/':
			if right == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			left /= right
		case '%':
			if right == 0 {
				return 0, fmt.Errorf("modulo by zero")
			}
			left %= right
		}
	}
}

func (p *exprParser) parseAtom() (int64, error) {
	p.skipSpace()
	if p.pos >= len(p.s) {
		return 0, fmt.Errorf("unexpected end")
	}
	if p.s[p.pos] == '(' {
		p.pos++
		v, err := p.parseAdd()
		if err != nil {
			return 0, err
		}
		p.skipSpace()
		if p.pos >= len(p.s) || p.s[p.pos] != ')' {
			return 0, fmt.Errorf("unclosed parenthesis")
		}
		p.pos++
		return v, nil
	}
	start := p.pos
	for p.pos < len(p.s) && p.s[p.pos] >= '0' && p.s[p.pos] <= '9' {
		p.pos++
	}
	if start == p.pos {
		return 0, fmt.Errorf("number expected at %q", p.s[p.pos:])
	}
	return strconv.ParseInt(p.s[start:p.pos], 10, 64)
}

var bcScale int = 0

func appletBc(args []string) int {
	var files []string
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stderr, `Usage: bc [-s N] [files...]

Arbitrary precision calculator.

Options:
  -s N, --scale=N  Set the number of decimal places
  -h, --help       Show this help

Inside the script, the directive "scale=N" also works.

Examples:
  bc -s 2               Run bc with 2 decimal places
  echo "10/3" | bc -s 2 Print 3.33
  echo "scale=4; 1/7" | bc`)
			return 0
		case a == "-s" || a == "--scale":
			if i+1 >= len(args) {
				return fail("bc: -s requires an argument")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				return fail("bc: invalid scale: %s", args[i+1])
			}
			bcScale = n
			i += 2
		case strings.HasPrefix(a, "--scale="):
			n, err := strconv.Atoi(a[8:])
			if err != nil || n < 0 {
				return fail("bc: invalid scale: %s", a[8:])
			}
			bcScale = n
			i++
		case strings.HasPrefix(a, "-s"):
			n, err := strconv.Atoi(a[2:])
			if err != nil || n < 0 {
				return fail("bc: invalid scale: %s", a[2:])
			}
			bcScale = n
			i++
		case strings.HasPrefix(a, "-"):
			return fail("bc: invalid option: %s", a)
		default:
			files = append(files, a)
			i++
		}
	}
	if len(files) == 0 {
		files = []string{"-"}
	}
	rc := 0
	for _, name := range files {
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bc: %s: %v\n", name, err)
			rc = 1
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(strings.TrimSuffix(sc.Text(), "\r"))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			for _, part := range strings.Split(line, ";") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				if part == "quit" {
					f.Close()
					return rc
				}
				if strings.HasPrefix(part, "scale=") {
					n, err := strconv.Atoi(strings.TrimSpace(part[6:]))
					if err != nil || n < 0 {
						fmt.Fprintf(os.Stderr, "bc: invalid scale: %s\n", part)
						rc = 1
						continue
					}
					bcScale = n
					continue
				}
				v, err := evalBc(part)
				if err != nil {
					fmt.Fprintf(os.Stderr, "bc: %v\n", err)
					rc = 1
					continue
				}
				fmt.Println(formatBc(v))
			}
		}
		f.Close()
	}
	return rc
}

func evalBc(s string) (float64, error) {
	p := &bcParser{s: s}
	v, err := p.parseAdd()
	if err != nil {
		return 0, err
	}
	p.skipSpace()
	if p.pos < len(p.s) {
		return 0, fmt.Errorf("unexpected token: %q", p.s[p.pos:])
	}
	return v, nil
}

type bcParser struct {
	s   string
	pos int
}

func (p *bcParser) skipSpace() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t') {
		p.pos++
	}
}

func (p *bcParser) parseAdd() (float64, error) {
	left, err := p.parseMul()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.s) {
			return left, nil
		}
		op := p.s[p.pos]
		if op != '+' && op != '-' {
			return left, nil
		}
		p.pos++
		right, err := p.parseMul()
		if err != nil {
			return 0, err
		}
		if op == '+' {
			left += right
		} else {
			left -= right
		}
	}
}

func (p *bcParser) parseMul() (float64, error) {
	left, err := p.parsePow()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.s) {
			return left, nil
		}
		op := p.s[p.pos]
		if op != '*' && op != '/' && op != '%' {
			return left, nil
		}
		p.pos++
		right, err := p.parsePow()
		if err != nil {
			return 0, err
		}
		switch op {
		case '*':
			left *= right
		case '/':
			if right == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			left /= right
		case '%':
			left = float64(int64(left) % int64(right))
		}
	}
}

func (p *bcParser) parsePow() (float64, error) {
	base, err := p.parseAtom()
	if err != nil {
		return 0, err
	}
	p.skipSpace()
	if p.pos < len(p.s) && p.s[p.pos] == '^' {
		p.pos++
		exp, err := p.parsePow()
		if err != nil {
			return 0, err
		}
		return pow(base, exp), nil
	}
	return base, nil
}

func pow(base, exp float64) float64 {
	r := 1.0
	e := int(exp)
	if e < 0 {
		base = 1 / base
		e = -e
	}
	for i := 0; i < e; i++ {
		r *= base
	}
	return r
}

func (p *bcParser) parseAtom() (float64, error) {
	p.skipSpace()
	if p.pos >= len(p.s) {
		return 0, fmt.Errorf("unexpected end")
	}
	if p.s[p.pos] == '(' {
		p.pos++
		v, err := p.parseAdd()
		if err != nil {
			return 0, err
		}
		p.skipSpace()
		if p.pos >= len(p.s) || p.s[p.pos] != ')' {
			return 0, fmt.Errorf("unclosed parenthesis")
		}
		p.pos++
		return v, nil
	}
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if (c >= '0' && c <= '9') || c == '.' {
			p.pos++
			continue
		}
		break
	}
	if start == p.pos {
		return 0, fmt.Errorf("number expected at %q", p.s[p.pos:])
	}
	return strconv.ParseFloat(p.s[start:p.pos], 64)
}

func formatBc(f float64) string {
	if bcScale > 0 {
		return strconv.FormatFloat(f, 'f', bcScale, 64)
	}
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ===========================================================================
// Applets: hash / encoding
// ===========================================================================

func appletSha256sum(args []string) int {
	return hashSum(args, sha256.New, "sha256sum")
}
func appletMd5sum(args []string) int {
	return hashSum(args, md5.New, "md5sum")
}
func appletSha1sum(args []string) int {
	return hashSum(args, sha1.New, "sha1sum")
}

func hashSum(args []string, newHash func() hash.Hash, name string) int {
	if len(args) == 0 {
		args = []string{"-"}
	}
	rc := 0
	for _, fname := range args {
		if fname == "-h" || fname == "--help" {
			fmt.Fprintf(os.Stderr, "Usage: %s [files...]\n", name)
			return 0
		}
		f, err := openInput(fname)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %s: %v\n", name, fname, err)
			rc = 1
			continue
		}
		h := newHash()
		if _, err := io.Copy(h, f); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %s: %v\n", name, fname, err)
			rc = 1
			f.Close()
			continue
		}
		f.Close()
		sum := hex.EncodeToString(h.Sum(nil))
		if fname == "-" {
			fmt.Printf("%s  -\n", sum)
		} else {
			fmt.Printf("%s  %s\n", sum, fname)
		}
	}
	return rc
}

func appletCksum(args []string) int {
	if len(args) == 0 {
		args = []string{"-"}
	}
	rc := 0
	for _, name := range args {
		if name == "-h" || name == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: cksum [files...]")
			return 0
		}
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cksum: %s: %v\n", name, err)
			rc = 1
			continue
		}
		data, _ := io.ReadAll(f)
		f.Close()
		sum := crc32.ChecksumIEEE(data)
		if name == "-" {
			fmt.Printf("%d %d\n", sum, len(data))
		} else {
			fmt.Printf("%d %d %s\n", sum, len(data), name)
		}
	}
	return rc
}

func appletBase64(args []string) int {
	decode := false
	var files []string
	for _, a := range args {
		if a == "-d" || a == "--decode" {
			decode = true
			continue
		}
		if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: base64 [-d] [files...]")
			return 0
		}
		files = append(files, a)
	}
	if len(files) == 0 {
		files = []string{"-"}
	}
	rc := 0
	for _, name := range files {
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "base64: %s: %v\n", name, err)
			rc = 1
			continue
		}
		if decode {
			data, _ := io.ReadAll(f)
			f.Close()
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
			if err != nil {
				fmt.Fprintf(os.Stderr, "base64: %s: %v\n", name, err)
				rc = 1
				continue
			}
			os.Stdout.Write(decoded)
		} else {
			enc := base64.NewEncoder(base64.StdEncoding, os.Stdout)
			io.Copy(enc, f)
			enc.Close()
			fmt.Println()
			f.Close()
		}
	}
	return rc
}

func appletHexdump(args []string) int {
	if len(args) == 0 {
		args = []string{"-"}
	}
	rc := 0
	for _, name := range args {
		if name == "-h" || name == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: hexdump [files...]")
			return 0
		}
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hexdump: %s: %v\n", name, err)
			rc = 1
			continue
		}
		data, _ := io.ReadAll(f)
		f.Close()
		for i := 0; i < len(data); i += 16 {
			end := i + 16
			if end > len(data) {
				end = len(data)
			}
			fmt.Printf("%08x  ", i)
			for j := i; j < i+16; j++ {
				if j < end {
					fmt.Printf("%02x ", data[j])
				} else {
					fmt.Print("   ")
				}
				if j == i+7 {
					fmt.Print(" ")
				}
			}
			fmt.Print(" |")
			for j := i; j < end; j++ {
				c := data[j]
				if c < 32 || c > 126 {
					c = '.'
				}
				fmt.Printf("%c", c)
			}
			fmt.Println("|")
		}
	}
	return rc
}

// ===========================================================================
// Applets: network
// ===========================================================================

func appletWget(args []string) int {
	var outFile string
	var urls []string
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-O":
			if i+1 >= len(args) {
				return fail("wget: -O requires an argument")
			}
			outFile, i = args[i+1], i+2
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "Usage: wget [-O file] URL...")
			return 0
		default:
			if strings.HasPrefix(args[i], "-") {
				return fail("wget: invalid option: %s", args[i])
			}
			urls, i = append(urls, args[i]), i+1
		}
	}
	if len(urls) == 0 {
		return fail("wget: usage: wget [-O file] URL...")
	}
	rc := 0
	for _, u := range urls {
		if err := wgetOne(u, outFile); err != nil {
			fmt.Fprintf(os.Stderr, "wget: %s: %v\n", u, err)
			rc = 1
		}
	}
	return rc
}

func wgetOne(rawurl, outFile string) error {
	resp, err := http.Get(rawurl)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if outFile == "" {
		if u, err := url.Parse(rawurl); err == nil {
			outFile = filepath.Base(u.Path)
			if outFile == "" || outFile == "/" {
				outFile = "index.html"
			}
		}
	}
	f, err := os.Create(outFile)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s -> %s (%d bytes)\n", rawurl, outFile, n)
	return nil
}

func appletPing(args []string) int {
	count := 4
	timeout := 5 * time.Second
	var host string
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: ping [-c N] host")
			return 0
		}
		if strings.HasPrefix(a, "-c") {
			v := strings.TrimPrefix(a, "-c")
			v = strings.TrimPrefix(v, "=")
			if v == "" {
				continue
			}
			if n, err := strconv.Atoi(v); err == nil {
				count = n
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		host = a
	}
	if host == "" {
		return fail("ping: usage: ping [-c N] host")
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fail("ping: %v", err)
	}
	if len(addrs) == 0 {
		return fail("ping: host not resolved")
	}
	ip := addrs[0]
	fmt.Printf("PING %s (%s): %d packets\n", host, ip, count)
	conn, err := net.DialTimeout("ip4:icmp", ip, timeout)
	if err != nil {
		return fail("ping: %v (requires root/administrator privileges)", err)
	}
	defer conn.Close()
	for i := 0; i < count; i++ {
		start := time.Now()
		msg := []byte{8, 0, 0, 0, 0, 1, 0, 1}
		cs := icmpChecksum(msg)
		msg[2], msg[3] = byte(cs>>8), byte(cs)
		if _, err := conn.Write(msg); err != nil {
			fmt.Fprintf(os.Stderr, "ping: %v\n", err)
			continue
		}
		buf := make([]byte, 1500)
		conn.SetReadDeadline(time.Now().Add(timeout))
		n, err := conn.Read(buf)
		if err != nil {
			fmt.Printf("Request timeout for icmp_seq %d\n", i)
			continue
		}
		rtt := time.Since(start)
		fmt.Printf("%d bytes from %s: icmp_seq=%d ttl=%d time=%.2f ms\n",
			n, ip, i, buf[8], float64(rtt.Microseconds())/1000)
		time.Sleep(time.Second)
	}
	return 0
}

func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func appletNetstat(args []string) int {
	fmt.Println("Proto Local Address           Remote Address          State")
	if runtime.GOOS == "linux" {
		return netstatLinux()
	}
	fmt.Println("(full netstat requires /proc or native APIs)")
	return 0
}

func netstatLinux() int {
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6", "/proc/net/udp", "/proc/net/udp6"} {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for i, l := range lines {
			if i == 0 || strings.TrimSpace(l) == "" {
				continue
			}
			fields := strings.Fields(l)
			if len(fields) < 4 {
				continue
			}
			local := parseHexAddr(fields[1])
			remote := parseHexAddr(fields[2])
			fmt.Printf("%-5s %-23s %-23s %s\n", protoOf(file), local, remote, fields[3])
		}
	}
	return 0
}

func protoOf(file string) string {
	if strings.Contains(file, "udp") {
		return "udp"
	}
	return "tcp"
}

func parseHexAddr(s string) string {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return s
	}
	ipHex := parts[0]
	port, _ := strconv.ParseInt(parts[1], 16, 32)
	if len(ipHex) == 8 {
		var b [4]byte
		for i := 0; i < 4; i++ {
			v, _ := strconv.ParseInt(ipHex[i*2:i*2+2], 16, 32)
			b[3-i] = byte(v)
		}
		return fmt.Sprintf("%d.%d.%d.%d:%d", b[0], b[1], b[2], b[3], port)
	}
	return fmt.Sprintf("%s:%d", ipHex, port)
}

func appletNc(args []string) int {
	listen, udp := false, false
	var port, host string
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-l":
			listen, i = true, i+1
		case "-u":
			udp, i = true, i+1
		case "-p":
			if i+1 >= len(args) {
				return fail("nc: -p requires an argument")
			}
			port, i = args[i+1], i+2
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "Usage: nc [-u] host port   |   nc -l [-u] -p port")
			return 0
		default:
			if host == "" {
				host, i = args[i], i+1
			} else if port == "" {
				port, i = args[i], i+1
			} else {
				i++
			}
		}
	}
	if listen {
		if port == "" {
			return fail("nc: -l mode requires -p port")
		}
		return ncListen(port, udp)
	}
	if host == "" || port == "" {
		return fail("nc: usage: nc [-u] host port   |   nc -l [-u] -p port")
	}
	return ncConnect(host, port, udp)
}

func ncListen(port string, udp bool) int {
	proto := "tcp"
	if udp {
		proto = "udp"
	}
	addr := ":" + port
	if udp {
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return fail("nc: %v", err)
		}
		defer pc.Close()
		buf := make([]byte, 4096)
		for {
			n, raddr, err := pc.ReadFrom(buf)
			if err != nil {
				return fail("nc: %v", err)
			}
			os.Stdout.Write(buf[:n])
			pc.WriteTo(buf[:n], raddr)
		}
	}
	ln, err := net.Listen(proto, addr)
	if err != nil {
		return fail("nc: %v", err)
	}
	defer ln.Close()
	fmt.Fprintf(os.Stderr, "listening on %s (%s)\n", addr, proto)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return fail("nc: %v", err)
		}
		go ncHandle(conn)
	}
}

func ncHandle(conn net.Conn) {
	defer conn.Close()
	go io.Copy(conn, os.Stdin)
	io.Copy(os.Stdout, conn)
}

func ncConnect(host, port string, udp bool) int {
	proto := "tcp"
	if udp {
		proto = "udp"
	}
	conn, err := net.Dial(proto, net.JoinHostPort(host, port))
	if err != nil {
		return fail("nc: %v", err)
	}
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		io.Copy(conn, os.Stdin)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		close(done)
	}()
	io.Copy(os.Stdout, conn)
	<-done
	return 0
}

func appletIfconfig(args []string) int {
	ifaces, err := net.Interfaces()
	if err != nil {
		return fail("ifconfig: %v", err)
	}
	for _, iface := range ifaces {
		fmt.Printf("%s: flags=%s mtu %d\n", iface.Name, iface.Flags, iface.MTU)
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			fmt.Printf("    inet %s\n", a.String())
		}
	}
	return 0
}

func appletRoute(args []string) int {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return fail("route: %v", err)
	}
	defer conn.Close()
	local := conn.LocalAddr().(*net.UDPAddr)
	fmt.Println("Kernel IP routing table")
	fmt.Println("Destination     Gateway         Genmask         Iface")
	fmt.Printf("0.0.0.0         0.0.0.0         0.0.0.0         %s\n", local.IP.String())
	return 0
}

// ===========================================================================
// Applets: utilities
// ===========================================================================

func appletEcho(args []string) int {
	noNewline := false
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") && len(args[i]) > 1 {
		if args[i] == "-n" {
			noNewline, i = true, i+1
			continue
		}
		if args[i] == "-h" || args[i] == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: echo [-n] [args...]")
			return 0
		}
		break
	}
	out := strings.Join(args[i:], " ")
	if noNewline {
		fmt.Print(out)
	} else {
		fmt.Println(out)
	}
	return 0
}

func appletPrintf(args []string) int {
	if len(args) == 0 {
		return fail("printf: usage: printf format [args...]")
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "Usage: printf format [args...]")
		return 0
	}
	format := args[0]
	rest := args[1:]
	var out strings.Builder
	argIdx := 0
	nextArg := func() string {
		if argIdx < len(rest) {
			v := rest[argIdx]
			argIdx++
			return v
		}
		return ""
	}
	i := 0
	for i < len(format) {
		c := format[i]
		if c == '\\' && i+1 < len(format) {
			switch format[i+1] {
			case 'n':
				out.WriteByte('\n')
			case 't':
				out.WriteByte('\t')
			case 'r':
				out.WriteByte('\r')
			case '\\':
				out.WriteByte('\\')
			default:
				out.WriteByte('\\')
				out.WriteByte(format[i+1])
			}
			i += 2
			continue
		}
		if c != '%' {
			out.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(format) {
			out.WriteByte('%')
			i++
			continue
		}
		if format[i+1] == '%' {
			out.WriteByte('%')
			i += 2
			continue
		}
		j := i + 1
		spec := "%"
		for j < len(format) {
			ch := format[j]
			if ch == '-' || ch == '+' || ch == ' ' || ch == '0' ||
				(ch >= '0' && ch <= '9') || ch == '.' {
				spec += string(ch)
				j++
				continue
			}
			break
		}
		if j >= len(format) {
			out.WriteString(spec)
			break
		}
		verb := format[j]
		spec += string(verb)
		arg := nextArg()
		switch verb {
		case 's':
			fmt.Fprintf(&out, spec, arg)
		case 'd', 'i':
			n, _ := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
			fmt.Fprintf(&out, spec, n)
		case 'u':
			n, _ := strconv.ParseUint(strings.TrimSpace(arg), 10, 64)
			fmt.Fprintf(&out, spec, n)
		case 'x', 'X', 'o':
			n, _ := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
			fmt.Fprintf(&out, spec, n)
		case 'f', 'e', 'g', 'E', 'G':
			f, _ := strconv.ParseFloat(strings.TrimSpace(arg), 64)
			fmt.Fprintf(&out, spec, f)
		case 'c':
			if arg != "" {
				fmt.Fprintf(&out, spec, rune(arg[0]))
			} else {
				fmt.Fprintf(&out, spec, rune(0))
			}
		default:
			out.WriteString(spec)
		}
		i = j + 1
	}
	fmt.Print(out.String())
	return 0
}

func appletTrue(args []string) int  { return 0 }
func appletFalse(args []string) int { return 1 }

func appletYes(args []string) int {
	s := "y"
	if len(args) > 0 {
		s = strings.Join(args, " ")
	}
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for {
		fmt.Fprintln(w, s)
	}
}

func appletBasename(args []string) int {
	if len(args) < 1 {
		return fail("basename: usage: basename path")
	}
	fmt.Println(filepath.Base(args[0]))
	return 0
}

func appletDirname(args []string) int {
	if len(args) < 1 {
		return fail("dirname: usage: dirname path")
	}
	fmt.Println(filepath.Dir(args[0]))
	return 0
}

func appletSeq(args []string) int {
	var start, end, step float64
	step = 1
	switch len(args) {
	case 1:
		start = 1
		v, err := strconv.ParseFloat(args[0], 64)
		if err != nil {
			return fail("seq: invalid value: %s", args[0])
		}
		end = v
	case 2:
		v1, _ := strconv.ParseFloat(args[0], 64)
		v2, _ := strconv.ParseFloat(args[1], 64)
		start, end = v1, v2
	case 3:
		v1, _ := strconv.ParseFloat(args[0], 64)
		v2, _ := strconv.ParseFloat(args[1], 64)
		v3, _ := strconv.ParseFloat(args[2], 64)
		start, step, end = v1, v3, v2
	default:
		return fail("seq: usage: seq [start] end [step]")
	}
	if step == 0 {
		return fail("seq: step cannot be zero")
	}
	if step > 0 {
		for i := start; i <= end; i += step {
			printNum(i)
		}
	} else {
		for i := start; i >= end; i += step {
			printNum(i)
		}
	}
	return 0
}

func printNum(f float64) {
	if f == float64(int64(f)) {
		fmt.Println(int64(f))
	} else {
		fmt.Println(f)
	}
}

func appletXargs(args []string) int {
	if len(args) == 0 {
		return fail("xargs: usage: xargs command [args...]")
	}
	cmdName := args[0]
	baseArgs := args[1:]
	var input []string
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		for _, f := range strings.Fields(strings.TrimSuffix(sc.Text(), "\r")) {
			input = append(input, f)
		}
	}
	if len(input) == 0 {
		return 0
	}
	full := append(append([]string{}, baseArgs...), input...)
	cmd := exec.Command(cmdName, full...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "xargs: %v\n", err)
		return 1
	}
	return 0
}

// ===========================================================================
// sh — shell with pipes, redirections, variable expansion and logical ops
// ===========================================================================

// lastExit holds the exit status of the most recent command, for $?.
var lastExit int

func appletSh(args []string) int {
	// sh -c 'command'
	if len(args) >= 2 && args[0] == "-c" {
		return runScript(args[1])
	}

	// sh script.sh [args...]
	if len(args) > 0 {
		path := args[0]
		// Treat as a script file only if it exists and is a regular file.
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return fail("sh: %s: %v", path, err)
			}
			sc := bufio.NewScanner(strings.NewReader(string(data)))
			sc.Buffer(make([]byte, 1024*1024), 1024*1024)
			rc := 0
			for sc.Scan() {
				line := strings.TrimSpace(strings.TrimSuffix(sc.Text(), "\r"))
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if line == "exit" {
					return rc
				}
				rc = runScript(line)
				lastExit = rc
			}
			if err := sc.Err(); err != nil {
				return fail("sh: %v", err)
			}
			return rc
		}
		// Not a file: treat as inline command.
		return runScript(strings.Join(args, " "))
	}

	// stdin
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	rc := 0
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimSuffix(sc.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "exit" {
			return rc
		}
		rc = runScript(line)
		lastExit = rc
	}
	return rc
}

func runScript(script string) int {
	tokens := splitTopLevel(script, []string{"||", "&&", ";"})
	rc := 0
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" || tok == ";" {
			continue
		}
		if tok == "&&" {
			if rc != 0 {
				return rc
			}
			continue
		}
		if tok == "||" {
			if rc == 0 {
				return rc
			}
			continue
		}
		rc = runPipeline(tok)
		lastExit = rc
	}
	return rc
}

func splitTopLevel(s string, ops []string) []string {
	var out []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	i := 0
	for i < len(s) {
		c := s[i]
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
			cur.WriteByte(c)
			i++
			continue
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
			cur.WriteByte(c)
			i++
			continue
		}
		if !inSingle && !inDouble {
			matched := false
			for _, op := range ops {
				if strings.HasPrefix(s[i:], op) {
					out = append(out, cur.String(), op)
					cur.Reset()
					i += len(op)
					matched = true
					break
				}
			}
			if matched {
				continue
			}
		}
		cur.WriteByte(c)
		i++
	}
	out = append(out, cur.String())
	return out
}

func runPipeline(line string) int {
	parts := splitTopLevel(line, []string{"|"})
	var cmds []string
	for _, p := range parts {
		if p == "|" {
			continue
		}
		p = strings.TrimSpace(p)
		if p != "" {
			cmds = append(cmds, p)
		}
	}
	if len(cmds) == 0 {
		return 0
	}
	if len(cmds) == 1 {
		return runCommandWithRedir(cmds[0], nil, nil)
	}
	var prevOut io.Reader
	for i, c := range cmds {
		last := i == len(cmds)-1
		var nextIn io.Reader
		var outW io.Writer = os.Stdout
		if !last {
			pr, pw := io.Pipe()
			nextIn = pr
			outW = pw
		}
		rc := runCommandWithRedir(c, prevOut, outW)
		if !last {
			outW.(*io.PipeWriter).Close()
		}
		if rc != 0 && last {
			return rc
		}
		prevOut = nextIn
	}
	return 0
}

func runCommandWithRedir(cmdStr string, stdin io.Reader, stdout io.Writer) int {
	args, inFile, outFile, appendMode, err := parseRedirs(cmdStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sh: %v\n", err)
		return 2
	}
	if inFile != "" {
		f, err := os.Open(inFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sh: %v\n", err)
			return 1
		}
		defer f.Close()
		stdin = f
	}
	if outFile != "" {
		flags := os.O_CREATE | os.O_WRONLY
		if appendMode {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
		}
		f, err := os.OpenFile(outFile, flags, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sh: %v\n", err)
			return 1
		}
		defer f.Close()
		stdout = f
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stdin == nil {
		stdin = os.Stdin
	}
	if len(args) == 0 {
		return 0
	}

	// Extract leading VAR=value assignments.
	var env []string
	for len(args) > 0 && isAssignment(args[0]) {
		env = append(env, args[0])
		args = args[1:]
	}
	if len(args) == 0 {
		// Only assignments on the line: apply to the current process for
		// simplicity (real sh would also apply in the current shell).
		for _, e := range env {
			kv := strings.SplitN(e, "=", 2)
			os.Setenv(kv[0], kv[1])
		}
		return 0
	}

	name := args[0]
	rest := args[1:]

	// Handle exit status storage so $? works on the next line.
	var rc int
	defer func() { lastExit = rc }()

	if fn, ok := applets[name]; ok {
		if len(env) > 0 {
			rc = runAppletWithEnv(fn, rest, stdin, stdout, env)
		} else if stdout == os.Stdout && stdin == os.Stdin {
			rc = fn(rest)
		} else {
			rc = runAppletWithIO(fn, rest, stdin, stdout)
		}
		return rc
	}
	cmd := exec.Command(name, rest...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			rc = ee.ExitCode()
			return rc
		}
		fmt.Fprintf(os.Stderr, "sh: %s: %v\n", name, err)
		rc = 127
		return rc
	}
	rc = 0
	return 0
}

func isAssignment(s string) bool {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return false
	}
	name := s[:eq]
	for i, c := range name {
		if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func runAppletWithIO(fn appletFunc, args []string, stdin io.Reader, stdout io.Writer) int {
	oldIn, oldOut := os.Stdin, os.Stdout
	if f, ok := stdin.(*os.File); ok {
		os.Stdin = f
	} else {
		pr, pw, _ := os.Pipe()
		go func() { io.Copy(pw, stdin); pw.Close() }()
		os.Stdin = pr
		defer pr.Close()
	}
	if f, ok := stdout.(*os.File); ok {
		os.Stdout = f
	} else {
		pr, pw, _ := os.Pipe()
		os.Stdout = pw
		done := make(chan struct{})
		go func() { io.Copy(stdout, pr); pr.Close(); close(done) }()
		defer func() { pw.Close(); <-done }()
	}
	rc := fn(args)
	os.Stdin, os.Stdout = oldIn, oldOut
	return rc
}

func runAppletWithEnv(fn appletFunc, args []string, stdin io.Reader, stdout io.Writer, env []string) int {
	oldEnv := os.Environ()
	m := map[string]string{}
	for _, e := range oldEnv {
		if k := strings.SplitN(e, "=", 2)[0]; k != "" {
			m[k] = e
		}
	}
	for _, e := range env {
		kv := strings.SplitN(e, "=", 2)
		m[kv[0]] = e
	}
	os.Clearenv()
	for _, e := range m {
		kv := strings.SplitN(e, "=", 2)
		os.Setenv(kv[0], kv[1])
	}
	rc := runAppletWithIO(fn, args, stdin, stdout)
	os.Clearenv()
	for _, e := range oldEnv {
		kv := strings.SplitN(e, "=", 2)
		os.Setenv(kv[0], kv[1])
	}
	return rc
}

// parseRedirs tokenizes a command line, performs variable expansion on
// unquoted/double-quoted segments, and extracts redirections.
//
// Expansion rules (matching POSIX shell basics):
//   - Single quotes: no expansion.
//   - Double quotes: $-expansion happens; backslash escapes \" and \\ and \$.
//   - Unquoted: $-expansion happens; backslash escapes the next char.
//   - $NAME and ${NAME} expand from the environment.
//   - $? is the last exit code; $$ is the PID of this process.
//   - $((...)) is not supported; numeric arithmetic must use expr.
func parseRedirs(s string) (args []string, inFile, outFile string, appendMode bool, err error) {
	var cur strings.Builder
	inSingle, inDouble := false, false
	hasToken := false
	flush := func() {
		if hasToken {
			args = append(args, cur.String())
			cur.Reset()
			hasToken = false
		}
	}

	// expandAt expands a $... reference starting at s[i] (s[i] == '$').
	// Returns the replacement string and the new index after the reference.
	expandAt := func(i int) (string, int) {
		if i+1 >= len(s) {
			return "$", i + 1
		}
		c := s[i+1]
		switch {
		case c == '?':
			return strconv.Itoa(lastExit), i + 2
		case c == '$':
			return strconv.Itoa(os.Getpid()), i + 2
		case c == '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "$", i + 1
			}
			name := s[i+2 : i+2+end]
			return os.Getenv(name), i + 3 + end
		case c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'):
			j := i + 1
			for j < len(s) {
				d := s[j]
				if d == '_' || (d >= '0' && d <= '9') ||
					(d >= 'A' && d <= 'Z') || (d >= 'a' && d <= 'z') {
					j++
					continue
				}
				break
			}
			return os.Getenv(s[i+1 : j]), j
		}
		return "$", i + 1
	}

	i := 0
	for i < len(s) {
		c := s[i]

		// Backslash handling.
		if c == '\\' && !inSingle && i+1 < len(s) {
			next := s[i+1]
			if inDouble {
				// In double quotes, only \" \\ \$ \` \newline are escapes.
				switch next {
				case '"', '\\', '$', '`':
					cur.WriteByte(next)
					i += 2
					continue
				default:
					cur.WriteByte('\\')
					i++
					continue
				}
			}
			cur.WriteByte(next)
			hasToken = true
			i += 2
			continue
		}

		if c == '\'' {
			if inDouble {
				cur.WriteByte(c) // literal inside double quotes
			} else {
				inSingle = !inSingle
				hasToken = true
			}
			i++
			continue
		}
		if c == '"' {
			if inSingle {
				cur.WriteByte(c)
			} else {
				inDouble = !inDouble
				hasToken = true
			}
			i++
			continue
		}

		// Variable expansion (only when not in single quotes).
		if c == '$' && !inSingle {
			repl, ni := expandAt(i)
			cur.WriteString(repl)
			hasToken = true
			i = ni
			continue
		}

		if !inSingle && !inDouble {
			if c == ' ' || c == '\t' {
				flush()
				i++
				continue
			}
			if strings.HasPrefix(s[i:], ">>") {
				flush()
				i += 2
				for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
					i++
				}
				start := i
				for i < len(s) && s[i] != ' ' && s[i] != '\t' {
					i++
				}
				outFile, appendMode = s[start:i], true
				continue
			}
			if c == '>' {
				flush()
				i++
				for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
					i++
				}
				start := i
				for i < len(s) && s[i] != ' ' && s[i] != '\t' {
					i++
				}
				outFile = s[start:i]
				continue
			}
			if c == '<' {
				flush()
				i++
				for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
					i++
				}
				start := i
				for i < len(s) && s[i] != ' ' && s[i] != '\t' {
					i++
				}
				inFile = s[start:i]
				continue
			}
		}

		cur.WriteByte(c)
		hasToken = true
		i++
	}
	flush()
	return
}

// busybox.go — BusyBox completo em Go puro
//
// Compilar:
//   go build -ldflags="-s -w" -o busybox busybox.go
//   GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o busybox.exe busybox.go
//
// Uso:
//   busybox <applet> [args...]
//   busybox --list
//   ln -s busybox ls && ./ls -la

package main

import (
	"archive/tar"
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
// Registro de applets
// ===========================================================================

type appletFunc func(args []string) int

// Mapa declarado vazio; preenchido em init() para quebrar o ciclo
// applets -> appletSh -> runScript -> runPipeline -> runCommandWithRedir -> applets.
var applets = map[string]appletFunc{}

func init() {
	applets = map[string]appletFunc{
		// arquivos
		"ls": appletLs, "cp": appletCp, "mv": appletMv, "rm": appletRm,
		"mkdir": appletMkdir, "rmdir": appletRmdir, "touch": appletTouch,
		"find": appletFind, "du": appletDu, "df": appletDf, "dd": appletDd,
		"tar": appletTar, "gzip": appletGzip, "stat": appletStat,
		"chmod": appletChmod, "chown": appletChown, "ln": appletLn,
		"readlink": appletReadlink, "split": appletSplit, "hexedit": appletHexedit,
		// texto
		"cat": appletCat, "head": appletHead, "tail": appletTail, "wc": appletWc,
		"grep": appletGrep, "sort": appletSort, "uniq": appletUniq, "tr": appletTr,
		"cut": appletCut, "tee": appletTee, "sed": appletSed, "awk": appletAwk,
		"diff": appletDiff, "patch": appletPatch, "strings": appletStrings,
		// sistema
		"pwd": appletPwd, "env": appletEnv, "sleep": appletSleep, "uname": appletUname,
		"whoami": appletWhoami, "hostname": appletHostname, "date": appletDate,
		"id": appletId, "which": appletWhich, "top": appletTop,
		// cálculo
		"expr": appletExpr, "bc": appletBc,
		// hash / codificação
		"sha256sum": appletSha256sum, "md5sum": appletMd5sum, "sha1sum": appletSha1sum,
		"cksum": appletCksum, "base64": appletBase64, "hexdump": appletHexdump,
		// rede
		"wget": appletWget, "ping": appletPing, "netstat": appletNetstat,
		"nc": appletNc, "ifconfig": appletIfconfig, "route": appletRoute,
		// utilitários
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
			printUsage()
			os.Exit(0)
		case "--list", "-l":
			listApplets()
			os.Exit(0)
		}
		fn, ok := applets[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "busybox: applet desconhecido: %s\n", name)
			os.Exit(127)
		}
		os.Exit(fn(os.Args[2:]))
	}
	if fn, ok := applets[prog]; ok {
		os.Exit(fn(os.Args[1:]))
	}
	fmt.Fprintf(os.Stderr, "busybox: nome de invocação desconhecido: %s\n", prog)
	os.Exit(127)
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Uso: busybox <applet> [args...]")
	fmt.Fprintln(os.Stderr, "     busybox --list | --help")
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
		lines = append(lines, sc.Text())
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
// Applets: arquivos
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
		default:
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				return fail("ls: opção inválida: %s", a)
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
		return fail("cp: uso: cp origem destino")
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
		return fail("mv: uso: mv origem destino")
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
		default:
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				return fail("rm: opção inválida: %s", a)
			}
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		if force {
			return 0
		}
		return fail("rm: uso: rm [-r] [-f] arquivos...")
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
		dirs = append(dirs, a)
	}
	if len(dirs) == 0 {
		return fail("mkdir: uso: mkdir [-p] dirs...")
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
		return fail("rmdir: uso: rmdir dirs...")
	}
	rc := 0
	for _, d := range args {
		if err := os.Remove(d); err != nil {
			fmt.Fprintf(os.Stderr, "rmdir: %s: %v\n", d, err)
			rc = 1
		}
	}
	return rc
}

func appletTouch(args []string) int {
	if len(args) == 0 {
		return fail("touch: uso: touch arquivos...")
	}
	rc := 0
	now := time.Now()
	for _, p := range args {
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
		case "-h":
			human = true
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
	fmt.Printf("(desconhecido)         0     0          0     -  %s\n", wd)
	return 0
}

func appletDd(args []string) int {
	inFile, outFile := "", ""
	bs := 512
	count := -1
	skip := 0
	seek := 0
	for _, a := range args {
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
				return fail("dd: bs inválido: %s", v)
			}
			bs = n
		case "count":
			n, err := strconv.Atoi(v)
			if err != nil {
				return fail("dd: count inválido: %s", v)
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
	fmt.Fprintf(os.Stderr, "%d bytes copiados\n", total)
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
		return fail("tar: uso: tar -c|-x|-t [-z] -f arquivo [arquivos...]")
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
				return fail("tar: -f requer um argumento")
			}
			archive, i = args[i+1], i+2
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
		return fail("tar: -f arquivo obrigatório")
	}
	switch {
	case create:
		return tarCreate(archive, files, gz)
	case extract:
		return tarExtract(archive, gz)
	case list:
		return tarList(archive, gz)
	}
	return fail("tar: especifique -c, -x ou -t")
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

func appletStat(args []string) int {
	if len(args) == 0 {
		return fail("stat: uso: stat arquivos...")
	}
	rc := 0
	for _, p := range args {
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
		return fail("chmod: uso: chmod modo arquivos...")
	}
	mode, err := strconv.ParseUint(args[0], 8, 32)
	if err != nil {
		return fail("chmod: modo inválido: %s", args[0])
	}
	rc := 0
	for _, p := range args[1:] {
		if err := os.Chmod(p, os.FileMode(mode)); err != nil {
			fmt.Fprintf(os.Stderr, "chmod: %s: %v\n", p, err)
			rc = 1
		}
	}
	return rc
}

func appletChown(args []string) int {
	if len(args) < 2 {
		return fail("chown: uso: chown usuário arquivos...")
	}
	fmt.Fprintln(os.Stderr, "chown: operação não suportada de forma portátil em Go puro")
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
		rest = append(rest, a)
	}
	if len(rest) < 2 {
		return fail("ln: uso: ln [-s] alvo link")
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
		return fail("readlink: uso: readlink link")
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
				return fail("split: -l requer um argumento")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fail("split: valor inválido: %s", args[i+1])
			}
			linesPerFile, i = n, i+2
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
		fmt.Fprintln(out, sc.Text())
		lineCount++
	}
	if out != nil {
		out.Close()
	}
	return 0
}

func appletHexedit(args []string) int {
	if len(args) < 1 {
		return fail("hexedit: uso: hexedit arquivo")
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
// Applets: texto
// ===========================================================================

func appletCat(args []string) int {
	if len(args) == 0 {
		args = []string{"-"}
	}
	rc := 0
	for _, name := range args {
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
		if args[i] == "-n" && i+1 < len(args) {
			v, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fail("head: valor inválido: %s", args[i+1])
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
			fmt.Println(sc.Text())
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
		if args[i] == "-n" && i+1 < len(args) {
			v, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fail("tail: valor inválido: %s", args[i+1])
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
			lines = append(lines, sc.Text())
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
				default:
					return fail("grep: opção inválida: -%c", c)
				}
			}
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) == 0 {
		return fail("grep: uso: grep [-i] [-v] [-n] padrão [arquivos...]")
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
			line := sc.Text()
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
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sort: %s: %v\n", name, err)
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			lines = append(lines, sc.Text())
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
			line := sc.Text()
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
		return fail("tr: uso: tr conjunto1 conjunto2")
	}
	set1, set2 := args[0], args[1]
	if len(set1) != len(set2) {
		return fail("tr: conjuntos devem ter o mesmo tamanho")
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

func appletCut(args []string) int {
	delim := "\t"
	var fields []int
	var files []string
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-d":
			if i+1 >= len(args) {
				return fail("cut: -d requer um argumento")
			}
			delim, i = args[i+1], i+2
		case "-f":
			if i+1 >= len(args) {
				return fail("cut: -f requer um argumento")
			}
			for _, s := range strings.Split(args[i+1], ",") {
				n, err := strconv.Atoi(s)
				if err != nil || n < 1 {
					return fail("cut: campo inválido: %s", s)
				}
				fields = append(fields, n)
			}
			i += 2
		default:
			files, i = append(files, args[i]), i+1
		}
	}
	if len(fields) == 0 {
		return fail("cut: uso: cut -d delim -f campos [arquivos...]")
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
			parts := strings.Split(sc.Text(), delim)
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
		return fail("sed: uso: sed [-n] [-e script]... [script] [arquivos...]")
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
				return fail("sed: -e requer um argumento")
			}
			scripts, i = append(scripts, args[i+1]), i+2
		case strings.HasPrefix(a, "-"):
			return fail("sed: opção inválida: %s", a)
		default:
			if len(scripts) == 0 {
				scripts, i = append(scripts, a), i+1
			} else {
				files, i = append(files, a), i+1
			}
		}
	}
	if len(scripts) == 0 {
		return fail("sed: script ausente")
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
			line := sc.Text()
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
			return c, fmt.Errorf("endereço regex não fechado")
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
		return c, fmt.Errorf("comando sed vazio")
	}
	c.op = s[0]
	switch c.op {
	case 'd', 'p':
	case 's':
		delim := s[1]
		parts := strings.SplitN(s[2:], string(delim), 3)
		if len(parts) < 2 {
			return c, fmt.Errorf("substituição s inválida")
		}
		re, err := regexp.Compile(parts[0])
		if err != nil {
			return c, err
		}
		c.re = re
		c.repl = parts[1]
		if len(parts) >= 3 && strings.Contains(parts[2], "g") {
			c.global = true
		}
	default:
		return c, fmt.Errorf("comando sed não suportado: %c", c.op)
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
		case args[i] == "-F":
			if i+1 >= len(args) {
				return fail("awk: -F requer um argumento")
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
		return fail("awk: uso: awk [-F sep] 'programa' [arquivos...]")
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
			line := sc.Text()
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
		return fail("diff: uso: diff arquivo1 arquivo2")
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
		rest, i = append(rest, args[i]), i+1
	}
	if len(rest) < 2 {
		return fail("patch: uso: patch [-o saida] arquivo patchfile")
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
		if args[i] == "-n" && i+1 < len(args) {
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fail("strings: valor inválido: %s", args[i+1])
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
// Applets: sistema
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
		return fail("sleep: uso: sleep segundos")
	}
	sec, err := strconv.ParseFloat(args[0], 64)
	if err != nil {
		return fail("sleep: valor inválido: %s", args[0])
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
		return fail("which: uso: which comandos...")
	}
	rc := 1
	for _, c := range args {
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
// Applets: cálculo
// ===========================================================================

func appletExpr(args []string) int {
	if len(args) == 0 {
		return fail("expr: uso: expr expressão")
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
		return 0, fmt.Errorf("token inesperado: %q", p.s[p.pos:])
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
				return 0, fmt.Errorf("divisão por zero")
			}
			left /= right
		case '%':
			if right == 0 {
				return 0, fmt.Errorf("módulo por zero")
			}
			left %= right
		}
	}
}

func (p *exprParser) parseAtom() (int64, error) {
	p.skipSpace()
	if p.pos >= len(p.s) {
		return 0, fmt.Errorf("fim inesperado")
	}
	if p.s[p.pos] == '(' {
		p.pos++
		v, err := p.parseAdd()
		if err != nil {
			return 0, err
		}
		p.skipSpace()
		if p.pos >= len(p.s) || p.s[p.pos] != ')' {
			return 0, fmt.Errorf("parêntese não fechado")
		}
		p.pos++
		return v, nil
	}
	start := p.pos
	for p.pos < len(p.s) && p.s[p.pos] >= '0' && p.s[p.pos] <= '9' {
		p.pos++
	}
	if start == p.pos {
		return 0, fmt.Errorf("número esperado em %q", p.s[p.pos:])
	}
	return strconv.ParseInt(p.s[start:p.pos], 10, 64)
}

func appletBc(args []string) int {
	if len(args) == 0 {
		args = []string{"-"}
	}
	rc := 0
	for _, name := range args {
		f, err := openInput(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bc: %s: %v\n", name, err)
			rc = 1
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			for _, part := range strings.Split(line, ";") {
				part = strings.TrimSpace(part)
				if part == "" {
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
		return 0, fmt.Errorf("token inesperado: %q", p.s[p.pos:])
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
				return 0, fmt.Errorf("divisão por zero")
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
		return 0, fmt.Errorf("fim inesperado")
	}
	if p.s[p.pos] == '(' {
		p.pos++
		v, err := p.parseAdd()
		if err != nil {
			return 0, err
		}
		p.skipSpace()
		if p.pos >= len(p.s) || p.s[p.pos] != ')' {
			return 0, fmt.Errorf("parêntese não fechado")
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
		return 0, fmt.Errorf("número esperado em %q", p.s[p.pos:])
	}
	return strconv.ParseFloat(p.s[start:p.pos], 64)
}

func formatBc(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ===========================================================================
// Applets: hash / codificação
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
// Applets: rede
// ===========================================================================

func appletWget(args []string) int {
	var outFile string
	var urls []string
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-O":
			if i+1 >= len(args) {
				return fail("wget: -O requer um argumento")
			}
			outFile, i = args[i+1], i+2
		default:
			if strings.HasPrefix(args[i], "-") {
				return fail("wget: opção inválida: %s", args[i])
			}
			urls, i = append(urls, args[i]), i+1
		}
	}
	if len(urls) == 0 {
		return fail("wget: uso: wget [-O arquivo] URL...")
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
		return fail("ping: uso: ping [-c N] host")
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fail("ping: %v", err)
	}
	if len(addrs) == 0 {
		return fail("ping: host não resolvido")
	}
	ip := addrs[0]
	fmt.Printf("PING %s (%s): %d pacotes\n", host, ip, count)
	conn, err := net.DialTimeout("ip4:icmp", ip, timeout)
	if err != nil {
		return fail("ping: %v (requer privilégios de root/administrador)", err)
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
		fmt.Printf("%d bytes de %s: icmp_seq=%d ttl=%d time=%.2f ms\n",
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
	fmt.Println("(netstat completo requer /proc ou APIs nativas)")
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
				return fail("nc: -p requer um argumento")
			}
			port, i = args[i+1], i+2
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
			return fail("nc: modo -l requer -p porta")
		}
		return ncListen(port, udp)
	}
	if host == "" || port == "" {
		return fail("nc: uso: nc [-u] host porta   |   nc -l [-u] -p porta")
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
	fmt.Fprintf(os.Stderr, "escutando em %s (%s)\n", addr, proto)
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
// Applets: utilitários
// ===========================================================================

func appletEcho(args []string) int {
	noNewline := false
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") && len(args[i]) > 1 {
		if args[i] == "-n" {
			noNewline, i = true, i+1
			continue
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
		return fail("printf: uso: printf formato [args...]")
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
		return fail("basename: uso: basename caminho")
	}
	fmt.Println(filepath.Base(args[0]))
	return 0
}

func appletDirname(args []string) int {
	if len(args) < 1 {
		return fail("dirname: uso: dirname caminho")
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
			return fail("seq: valor inválido: %s", args[0])
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
		return fail("seq: uso: seq [início] fim [passo]")
	}
	if step == 0 {
		return fail("seq: passo não pode ser zero")
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
		return fail("xargs: uso: xargs comando [args...]")
	}
	cmdName := args[0]
	baseArgs := args[1:]
	var input []string
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		for _, f := range strings.Fields(sc.Text()) {
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
// sh — shell com pipes, redirecionamentos e operadores lógicos
// ===========================================================================

func appletSh(args []string) int {
	if len(args) > 0 {
		return runScript(strings.Join(args, " "))
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "exit" {
			return 0
		}
		runScript(line)
	}
	return 0
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
	name := args[0]
	rest := args[1:]
	if fn, ok := applets[name]; ok {
		if stdout == os.Stdout && stdin == os.Stdin {
			return fn(rest)
		}
		return runAppletWithIO(fn, rest, stdin, stdout)
	}
	cmd := exec.Command(name, rest...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "sh: %s: %v\n", name, err)
		return 127
	}
	return 0
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

func parseRedirs(s string) (args []string, inFile, outFile string, appendMode bool, err error) {
	var cur strings.Builder
	inSingle, inDouble := false, false
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
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
			if strings.HasPrefix(s[i:], ">>") {
				flush()
				i += 2
				for i < len(s) && s[i] == ' ' {
					i++
				}
				start := i
				for i < len(s) && s[i] != ' ' {
					i++
				}
				outFile, appendMode = s[start:i], true
				continue
			}
			if c == '>' {
				flush()
				i++
				for i < len(s) && s[i] == ' ' {
					i++
				}
				start := i
				for i < len(s) && s[i] != ' ' {
					i++
				}
				outFile = s[start:i]
				continue
			}
			if c == '<' {
				flush()
				i++
				for i < len(s) && s[i] == ' ' {
					i++
				}
				start := i
				for i < len(s) && s[i] != ' ' {
					i++
				}
				inFile = s[start:i]
				continue
			}
		}
		cur.WriteByte(c)
		i++
	}
	flush()
	return
}

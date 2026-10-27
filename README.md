# busybox
[![ISC License](http://img.shields.io/badge/license-ISC-blue.svg)](https://github.com/pedroalbanese/busybox/blob/master/LICENSE.md) 
[![GoDoc](https://godoc.org/github.com/pedroalbanese/busybox?status.png)](http://godoc.org/github.com/pedroalbanese/busybox)
[![GitHub downloads](https://img.shields.io/github/downloads/pedroalbanese/busybox/total.svg?logo=github&logoColor=white)](https://github.com/pedroalbanese/busybox/releases)
[![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/pedroalbanese/busybox)](https://golang.org)
[![GitHub release (latest by date)](https://img.shields.io/github/v/release/pedroalbanese/busybox)](https://github.com/pedroalbanese/busybox/releases)

Portable cross-platform BusyBox for small or embedded systems written in Pure Go

The BusyBox in pure Go is a multicall implementation of Unix-like utilities that bundles, into a single portable binary for Linux, macOS, and Windows, more than seventy essential applets — including file manipulation (`ls`, `cp`, `mv`, `rm`, `mkdir`, `rmdir`, `touch`, `find`, `du`, `df`, `dd`, `tar`, `gzip`, `zip`, `unzip`, `stat`, `chmod`, `chown`, `ln`, `readlink`, `split`, `hexedit`), text processing (`cat`, `head`, `tail`, `wc`, `grep`, `sort`, `uniq`, `tr`, `nl`, `cut`, `tee`, `sed`, `awk`, `diff`, `patch`, `strings`), system utilities (`cal`, `pwd`, `env`, `sleep`, `uname`, `whoami`, `hostname`, `date`, `id`, `which`, `top`), arithmetic (`expr`, `bc`), hashing and encoding (`sha256sum`, `md5sum`, `sha1sum`, `cksum`, `base64`, `hexdump`), networking (`wget`, `ping`, `netstat`, `nc`, `ifconfig`, `route`), and a minimal shell (`sh`) with pipes, redirections, and logical operators — all written without external dependencies, cgo, or third-party libraries, relying solely on Go's standard library to offer a lightweight, auditable, and cross-platform alternative to the traditional BusyBox in C.

## Contribute
**Use issues for everything**
- You can help and get help by:
  - Reporting doubts and questions
- You can contribute by:
  - Reporting issues
  - Suggesting new features or enhancements
  - Improve/fix documentation

## License

This project is licensed under the ISC License.

#### Copyright (c) 2020-2026 Pedro F. Albanese - ALBANESE Research Lab.  
Todos os direitos de propriedade intelectual sobre este software pertencem ao autor, Pedro F. Albanese. Vide Lei 9.610/98, Art. 7º, inciso XII.

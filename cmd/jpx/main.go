// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

// Command jpx is a development tool for the jpx library.  It can print
// image headers, decode images to PNG or PGX files and encode PNG or PGX
// files as JPEG 2000.
//
// Usage:
//
//	jpx info FILE
//	jpx decode [flags] FILE OUTBASE
//	jpx encode [flags] IN OUT
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "info":
		err = runInfo(os.Args[2:])
	case "decode":
		err = runDecode(os.Args[2:])
	case "encode":
		err = runEncode(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "jpx:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: jpx info|decode|encode [flags] args")
	os.Exit(1)
}

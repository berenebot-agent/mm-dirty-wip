// Command docsgen renders the generated API reference from the canonical
// route table in internal/apispec.
//
// It writes to stdout by default so the Makefile can redirect the output and
// keep the checked-in file owned by the host user. The -out flag writes to a
// path instead, which is only useful outside the container toolchain.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/dellarb/mailmoose/internal/apispec"
)

func main() {
	out := flag.String("out", "", "write to PATH instead of stdout")
	flag.Parse()

	doc := apispec.RenderMarkdown(apispec.Routes())

	if *out == "" {
		fmt.Print(doc)
		return
	}
	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "docsgen:", err)
		os.Exit(1)
	}
}

// Command docsgen renders the generated API endpoint section into docs/API.md
// from the canonical route table in internal/apispec.
//
// It writes to stdout by default so the Makefile can redirect the output and
// keep the checked-in file owned by the host user. The -out flag writes to a
// path instead, which is only useful outside the container toolchain.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/dellarb/mailmoose/internal/apispec"
)

func main() {
	out := flag.String("out", "", "write to PATH instead of stdout")
	flag.Parse()

	path := "docs/API.md"
	template, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docsgen:", err)
		os.Exit(1)
	}
	doc, err := replaceGeneratedSection(string(template), apispec.RenderMarkdown(apispec.Routes()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "docsgen:", err)
		os.Exit(1)
	}

	if *out == "" {
		fmt.Print(doc)
		return
	}
	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "docsgen:", err)
		os.Exit(1)
	}
}

const (
	generatedStart = "<!-- BEGIN GENERATED API REFERENCE -->"
	generatedEnd   = "<!-- END GENERATED API REFERENCE -->"
)

func replaceGeneratedSection(template, generated string) (string, error) {
	start := strings.Index(template, generatedStart)
	end := strings.Index(template, generatedEnd)
	if start < 0 || end < 0 || end < start || strings.Count(template, generatedStart) != 1 || strings.Count(template, generatedEnd) != 1 {
		return "", fmt.Errorf("%s must contain one ordered generated-section marker pair", "docs/API.md")
	}
	start += len(generatedStart)
	return template[:start] + "\n\n" + strings.TrimSpace(generated) + "\n\n" + template[end:], nil
}

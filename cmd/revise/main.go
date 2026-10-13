// Command revise applies a plain-language change to a saved edit.
//
//	revise -edl edit.json -do "make the first shot slow-mo and add the title Summer" -out edit2.json
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"filmeditor/internal/llm"
	"filmeditor/internal/project"
	"filmeditor/internal/revise"
)

func main() {
	in := flag.String("edl", "", "project JSON to revise")
	do := flag.String("do", "", "what to change, in plain language")
	out := flag.String("out", "", "where to write the revised project (default: overwrite -edl)")
	flag.Parse()
	if *in == "" || *do == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *out == "" {
		*out = *in
	}
	p, err := project.Load(*in)
	if err != nil {
		fatal(err)
	}
	next, err := revise.Apply(context.Background(), llm.CLI{}, p.EDL, *do)
	if err != nil {
		fatal(err)
	}
	if err := p.Replace(next); err != nil {
		fatal(err)
	}
	if err := p.Save(*out); err != nil {
		fatal(err)
	}
	fmt.Println("wrote", *out)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

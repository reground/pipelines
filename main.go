// Command pipelines runs the pipelines engine: plain pages for moving
// things through stages that are defined in text files, and a checker for
// those files.
//
//	pipelines serve            run the web server (DATABASE_URL, PIPELINES_DIR, LISTEN)
//	pipelines check [DIR]      check pipeline files; exits 1 if there are problems
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"reground.org/pipelines/internal/dbstore"
	"reground.org/pipelines/internal/pipeline"
	"reground.org/pipelines/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pipelines <serve|check [DIR]>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve()
	case "check":
		dir := envOrDefault("PIPELINES_DIR", ".")
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		os.Exit(check(dir))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func serve() {
	dir := mustEnv("PIPELINES_DIR")
	if code := check(dir); code != 0 {
		log.Printf("starting anyway; broken pipelines are hidden until fixed")
	}
	db, err := dbstore.Open(mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer db.Close()
	srv, err := server.New(db, dir)
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	addr := envOrDefault("LISTEN", "127.0.0.1:8080")
	log.Printf("listening on %s, pipelines in %s", addr, dir)
	log.Fatal(http.ListenAndServe(addr, srv.Handler()))
}

// check prints a plain-English report and returns the exit code.
func check(dir string) int {
	pls, err := pipeline.LoadAll(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not read the pipeline files: %v\n", err)
		return 1
	}
	if len(pls) == 0 {
		fmt.Fprintf(os.Stderr, "no pipelines found in %s (each pipeline is a folder containing pipeline.yml)\n", dir)
		return 1
	}
	code := 0
	for _, p := range pls {
		problems, warnings := p.Check()
		for _, w := range warnings {
			fmt.Printf("%s: note: %s\n", p.ID, w)
		}
		if len(problems) == 0 {
			fmt.Printf("%s: OK (%d stages, %d fields, %d views)\n", p.ID, len(p.Stages), len(p.Fields), len(p.Views))
			continue
		}
		code = 1
		fmt.Printf("%s: %d problem(s)\n", p.ID, len(problems))
		for _, pr := range problems {
			fmt.Printf("  - %s\n", pr)
		}
	}
	return code
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}

func envOrDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

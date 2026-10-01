// Command codeidx builds and queries a workspace source symbol index.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	index "github.com/cuixudong74/code-symbol-index-go"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "index":
		return cmdIndex(args[1:])
	case "query":
		return cmdQuery(args[1:])
	case "outline":
		return cmdOutline(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "-h", "--help", "help":
		return usage()
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() error {
	fmt.Fprint(os.Stderr, `usage: codeidx <command> [flags]

commands:
  index    build (or incrementally update) the index for a workspace
  query    search symbols by name, prefix, kind and/or file
  outline  list all symbols of a single file
  status   show index status

run "codeidx <command> -h" for command-specific flags.
`)
	return nil
}

func addCommon(fs *flag.FlagSet, root *string, indexPath *string) {
	fs.StringVar(root, "root", ".", "workspace root directory")
	fs.StringVar(indexPath, "index", "", "index file path (default: <root>/.codeidx/index.json)")
}

func defaultIndexPath(root string) string {
	return filepath.Join(root, index.DefaultIndexDir, index.DefaultIndexFile)
}

func cmdIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	var root, indexPath string
	var concurrency int
	var maxSize int64
	var ignores stringSlice
	addCommon(fs, &root, &indexPath)
	fs.IntVar(&concurrency, "concurrency", 0, "number of worker goroutines (default: NumCPU)")
	fs.Int64Var(&maxSize, "max-size", 0, "max file size in bytes, 0 for default (5MiB)")
	fs.Var(&ignores, "ignore", "glob pattern to ignore (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := index.DefaultConfig(root)
	if concurrency > 0 {
		cfg.Concurrency = concurrency
	}
	if maxSize > 0 {
		cfg.MaxFileSize = maxSize
	}
	if len(ignores) > 0 {
		cfg.IgnorePatterns = append(cfg.IgnorePatterns, ignores...)
	}
	if indexPath == "" {
		indexPath = defaultIndexPath(root)
	}

	idx, err := index.Open(cfg, indexPath)
	if err != nil {
		if errors.Is(err, index.ErrCorrupt) {
			fmt.Fprintf(os.Stderr, "warning: corrupt index at %s, rebuilding from scratch\n", indexPath)
			idx, err = index.New(cfg)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}
	report, err := idx.Update(context.Background())
	if err != nil {
		return err
	}
	if err := idx.Save(indexPath); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func loadIndex(root, indexPath string) (*index.Index, error) {
	if indexPath == "" {
		indexPath = defaultIndexPath(root)
	}
	cfg := index.DefaultConfig(root)
	idx, err := index.Open(cfg, indexPath)
	if err != nil {
		return nil, err
	}
	return idx, nil
}

func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	var root, indexPath, name, prefix, kind, file string
	var limit, offset int
	addCommon(fs, &root, &indexPath)
	fs.StringVar(&name, "name", "", "exact symbol name")
	fs.StringVar(&prefix, "prefix", "", "symbol name prefix")
	fs.StringVar(&kind, "kind", "", "symbol kind (package|function|method|type|variable|constant|field)")
	fs.StringVar(&file, "file", "", "restrict to file (relative path)")
	fs.IntVar(&limit, "limit", 0, "max results, 0 for no limit")
	fs.IntVar(&offset, "offset", 0, "skip first N results")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if kind != "" && !index.ValidKind(index.Kind(kind)) {
		return fmt.Errorf("invalid kind %q", kind)
	}
	idx, err := loadIndex(root, indexPath)
	if err != nil {
		return err
	}
	snap := idx.Snapshot()
	results := snap.Search(index.Query{
		Name:   name,
		Prefix: prefix,
		Kind:   index.Kind(kind),
		File:   file,
		Limit:  limit,
		Offset: offset,
	})
	return printJSON(results)
}

func cmdOutline(args []string) error {
	fs := flag.NewFlagSet("outline", flag.ContinueOnError)
	var root, indexPath, file string
	addCommon(fs, &root, &indexPath)
	fs.StringVar(&file, "file", "", "file to outline (relative path)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if file == "" && fs.NArg() > 0 {
		file = fs.Arg(0)
	}
	if file == "" {
		return fmt.Errorf("outline requires a file")
	}
	idx, err := loadIndex(root, indexPath)
	if err != nil {
		return err
	}
	snap := idx.Snapshot()
	if _, ok := snap.File(file); !ok {
		return fmt.Errorf("file %q is not in the index", file)
	}
	return printJSON(snap.Outline(file))
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var root, indexPath string
	addCommon(fs, &root, &indexPath)
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndex(root, indexPath)
	if err != nil {
		return err
	}
	snap := idx.Snapshot()
	ok, degraded, skipped := snap.StatusCounts()
	out := map[string]any{
		"version":        snap.Version,
		"files":          snap.FileCount(),
		"symbols":        snap.SymbolCount(),
		"files_ok":       ok,
		"files_degraded": degraded,
		"files_skipped":  skipped,
	}
	return printJSON(out)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// stringSlice is a repeatable string flag.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

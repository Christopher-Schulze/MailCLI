package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if _, writeErr := fmt.Fprintf(os.Stderr, "select gate packages: %v\n", err); writeErr != nil {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("expected ROOT BASELINE_JSON STAGED_JSON CHANGED_NUL_PATHS")
	}
	baseline, err := readPackages(args[1])
	if err != nil {
		return err
	}
	staged, err := readPackages(args[2])
	if err != nil {
		return err
	}
	paths, err := os.ReadFile(args[3])
	if err != nil {
		return fmt.Errorf("read changed paths: %w", err)
	}
	plan, err := selectChecks(args[0], baseline, staged, paths)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(plan); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	return nil
}

func readPackages(path string) ([]listedPackage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read package graph: %w", err)
	}
	return decodePackages(bytes.NewReader(data))
}

func decodePackages(input io.Reader) ([]listedPackage, error) {
	decoder := json.NewDecoder(input)
	var packages []listedPackage
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode package graph: %w", err)
		}
		if pkg.Incomplete || pkg.Error != nil || len(pkg.DepsErrors) != 0 || pkg.ImportPath == "" {
			detail := "missing package or dependency metadata"
			if pkg.Error != nil {
				detail = pkg.Error.Err
			} else if len(pkg.DepsErrors) != 0 && pkg.DepsErrors[0] != nil {
				detail = pkg.DepsErrors[0].Err
			}
			return nil, fmt.Errorf("incomplete package graph at %q: %s", pkg.ImportPath, detail)
		}
		packages = append(packages, pkg)
	}
	if len(packages) == 0 {
		return nil, fmt.Errorf("empty package graph")
	}
	return packages, nil
}
